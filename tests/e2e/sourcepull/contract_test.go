//go:build e2e

package sourcepull

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// accSource 是 ACC_SOURCE：读 stdin JSON 存盘，再按控制目录里的 out、exit、sleep 文件输出，
// 使每个验收动作能在运行之间改变它的行为。
type accSource struct {
	f   *fx
	id  string
	ctl string
}

const accScript = `#!/bin/sh
ctl="$1"
cat > "$ctl/stdin.tmp" && mv "$ctl/stdin.tmp" "$ctl/stdin.json"
[ -f "$ctl/sleep" ] && sleep "$(cat "$ctl/sleep")"
cat "$ctl/out"
exit "$(cat "$ctl/exit")"
`

func (f *fx) script(dir string) string {
	f.t.Helper()
	p := filepath.Join(dir, "acc_source.sh")
	if err := os.WriteFile(p, []byte(accScript), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func write(t *testing.T, path, content string) {
	t.Helper()
	tmp := path + ".w"
	if err := os.WriteFile(tmp, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// out 设置下一次运行的 stdout 与退出码。
func (a *accSource) out(stdout string, exit int) {
	write(a.f.t, filepath.Join(a.ctl, "out"), stdout)
	write(a.f.t, filepath.Join(a.ctl, "exit"), fmt.Sprint(exit))
}

func (a *accSource) stdin() map[string]json.RawMessage {
	b, err := os.ReadFile(filepath.Join(a.ctl, "stdin.json"))
	if err != nil {
		return nil
	}
	var m map[string]json.RawMessage
	_ = json.Unmarshal(b, &m)
	return m
}

// register 在 Web 登记一个 type=script 来源（可执行命令只 Web，退出码 20 由 authz 流程覆盖）。
func (f *fx) register(id, dir string, timeout int, dedupeDomain string) *accSource {
	f.t.Helper()
	ctl := filepath.Join(dir, id)
	if err := os.MkdirAll(ctl, 0o755); err != nil {
		f.t.Fatal(err)
	}
	a := &accSource{f: f, id: id, ctl: ctl}
	a.out("", 0)
	body := map[string]any{"id": id, "type": "script", "source_type": "acc",
		"command": "/bin/sh " + f.script(dir) + " " + ctl, "schedule": "1s", "timeout": timeout}
	if dedupeDomain != "" {
		body["dedupe_domain"] = dedupeDomain
	}
	b, _ := json.Marshal(body)
	if r := f.Web("POST", "/v1/sources", string(b)); r.Code != 201 {
		f.t.Fatalf("登记 %s: HTTP %d %s", id, r.Code, r.Body)
	}
	return a
}

func ev(id, subject string) string {
	return fmt.Sprintf(`{"id":%q,"type":"acc.event","subject":%q,"occurred_at":"2030-01-01T00:00:00Z"}`, id, subject)
}

func cursorLine(c string) string { return fmt.Sprintf(`{"cursor":%q}`, c) }

func lines(ls ...string) string { return strings.Join(ls, "\n") + "\n" }

func TestFlowSourceContract(t *testing.T) {
	f := setup(t)
	dir := t.TempDir()
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}
	var a, b, other *accSource

	step("ACT-SRC-01 登记验收脚本来源", func(t *testing.T) {
		a = f.register("src_acca", dir, 30, "acc")
		s := f.show("src_acca")
		if !s.Enabled {
			t.Fatalf("登记后来源应启用: %+v", s)
		}
		raw := f.apa(f.OBS, 0, "source", "show", "src_acca").Stdout
		for _, want := range []string{`"type":"script"`, `"source_type":"acc"`, `"schedule":"1s"`, `"timeout":30`} {
			if !strings.Contains(strings.ReplaceAll(raw, " ", ""), want) {
				t.Errorf("source show 缺少 %s: %s", want, raw)
			}
		}
	})

	step("ACT-SRC-02 正常拉取并推进游标", func(t *testing.T) {
		a.out(lines(ev("e1", "第一条"), ev("e2", "第二条"), cursorLine("c1")), 0)
		f.until("游标推进到 c1", func() bool { return f.show("src_acca").Cursor == "c1" })
		if got := ids(f.events("src_acca")); len(got) != 2 {
			t.Fatalf("应入账两条事件，实际 %v", got)
		}
		s := f.show("src_acca")
		if s.Stats.LastSuccessAt == "" {
			t.Errorf("来源页应有最近成功时间: %+v", s.Stats)
		}
		runs := s.Stats.Runs
		f.until("下一次运行的 stdin 到达", func() bool { return f.show("src_acca").Stats.Runs > runs })
		in := a.stdin()
		if string(in["cursor"]) != `"c1"` {
			t.Errorf("下次 stdin 的 cursor 应为 c1: %s", in["cursor"])
		}
		for _, k := range []string{"source_id", "config", "window", "run_id"} {
			if len(in[k]) == 0 {
				t.Errorf("stdin 缺少 %s: %v", k, in)
			}
		}
	})

	step("ACT-SRC-03 按去重键去重", func(t *testing.T) {
		b = f.register("src_accb", dir, 30, "acc")
		a.out(lines(ev("e1", "第一条"), cursorLine("c2")), 0)
		b.out(lines(ev("e1", "第一条"), cursorLine("b1")), 0)
		f.until("两个来源都跑到新游标", func() bool { return f.show("src_acca").Cursor == "c2" && f.show("src_accb").Cursor == "b1" })
		f.settle("src_acca", 1)
		total := len(f.events("src_acca")) + len(f.events("src_accb"))
		if total != 2 {
			t.Fatalf("同一去重域内 e1、e2 只应各有一条来源事件，实际 acc-a=%v acc-b=%v", ids(f.events("src_acca")), ids(f.events("src_accb")))
		}
	})

	step("ACT-SRC-04 缺必填字段的事件不入账", func(t *testing.T) {
		bad := `{"id":"bad1","type":"acc.event","occurred_at":"2030-01-01T00:00:00Z"}`
		a.out(lines(bad, ev("e3", "正常"), cursorLine("c3")), 0)
		f.until("游标推进到 c3", func() bool { return f.show("src_acca").Cursor == "c3" })
		got := ids(f.events("src_acca"))
		if !contains(got, "e3") || contains(got, "bad1") {
			t.Fatalf("正常事件应入账、坏事件不入账: %v", got)
		}
		rejects := f.rejects("src_acca")
		if len(rejects) != 1 || !strings.Contains(rejects[0].Raw, "bad1") || rejects[0].Reason == "" {
			t.Fatalf("坏事件应连同原因可查: %+v", rejects)
		}
	})

	step("ACT-SRC-05 末行不是合法游标时事件留下、游标不动", func(t *testing.T) {
		a.out(lines(ev("e4", "新事件"), "not-json"), 0)
		f.until("e4 落盘", func() bool { return contains(ids(f.events("src_acca")), "e4") })
		s := f.settle("src_acca", 2)
		if s.Cursor != "c3" {
			t.Errorf("游标应不动（c3）: %s", s.Cursor)
		}
		if s.Stats.ConsecutiveFailures == 0 {
			t.Errorf("应按临时失败计数: %+v", s.Stats)
		}
		if n := count(ids(f.events("src_acca")), "e4"); n != 1 {
			t.Errorf("重复输出同一事件不应重复入账，e4 有 %d 条", n)
		}
	})

	step("ACT-SRC-06 退出码 1 退避重试", func(t *testing.T) {
		a.out("", 1)
		f.until("连续失败 >= 2", func() bool { return f.show("src_acca").Stats.ConsecutiveFailures >= 2 })
		s := f.show("src_acca")
		if s.Cursor != "c3" || !s.Enabled {
			t.Errorf("游标不动且不得停用: %+v", s)
		}
		// 退避：失败 N 次后节拍被跳过，一段时间内运行次数少于节拍数。
		runs, t0 := s.Stats.Runs, time.Now()
		time.Sleep(6 * time.Second)
		if grown := f.show("src_acca").Stats.Runs - runs; grown >= 6 {
			t.Errorf("%v 内运行了 %d 次，没有退避", time.Since(t0), grown)
		}
		if n := f.needsInputItem("src_acca"); n != "" {
			t.Errorf("临时失败不应产生 needs_input 打扰: %s", n)
		}
	})

	step("ACT-SRC-07 退出码 2 生成 needs_input", func(t *testing.T) {
		other = f.register("src_accother", dir, 30, "")
		other.out(lines(ev("o1", "不受影响"), cursorLine("o1")), 0)
		a.out("", 2)
		f.until("a 生成 needs_input 事项", func() bool { return f.show("src_acca").Stats.NeedsInputItem != "" })
		item := f.show("src_acca").Stats.NeedsInputItem
		if !f.needsInputOf(item) {
			t.Errorf("事项 %s 应在 needs_input", item)
		}
		if f.show("src_acca").Cursor != "c3" {
			t.Errorf("游标不动")
		}
		f.until("其他来源照常推进", func() bool { return f.show("src_accother").Cursor == "o1" })
	})

	step("ACT-SRC-08 退出码 3 停用并通知", func(t *testing.T) {
		a.out("", 3)
		f.until("来源被停用", func() bool { return !f.show("src_acca").Enabled })
		runs := f.show("src_acca").Stats.Runs
		time.Sleep(3 * time.Second)
		if f.show("src_acca").Stats.Runs != runs {
			t.Errorf("停用后不应继续被调度")
		}
		if !f.hasFeedEvent("source.failed") {
			t.Errorf("事件流应有 source.failed")
		}
		if !f.hasHealthDelivery("src_acca") {
			t.Errorf("应产出健康类投递")
		}
	})

	step("ACT-SRC-09 超时按临时失败", func(t *testing.T) {
		slow := f.register("src_accslow", dir, 2, "")
		write(t, filepath.Join(slow.ctl, "sleep"), "60")
		f.until("计入超时", func() bool { return f.show("src_accslow").Stats.Timeouts >= 1 })
		s := f.show("src_accslow")
		if s.Cursor != "" || !s.Enabled || s.Stats.ConsecutiveFailures == 0 {
			t.Errorf("超时应计临时失败、游标不动、保持启用: %+v", s)
		}
		f.until("下次仍重试", func() bool { return f.show("src_accslow").Stats.Runs >= 2 })
	})

	step("ACT-SRC-10 启停来源", func(t *testing.T) {
		b.out(lines(cursorLine("b2")), 0)
		f.until("b 在运行", func() bool { return f.show("src_accb").Cursor == "b2" })
		f.apa(f.DEL, 0, "source", "disable", "src_accb")
		runs := f.settle0("src_accb")
		time.Sleep(4 * time.Second)
		if got := f.show("src_accb").Stats.Runs; got != runs {
			t.Fatalf("停用期间不应有 Run：%d → %d", runs, got)
		}
		b.out(lines(ev("e9", "停用期间产生"), cursorLine("b3")), 0)
		f.apa(f.DEL, 0, "source", "enable", "src_accb")
		f.until("启用后恢复且事件不漏", func() bool { return f.show("src_accb").Cursor == "b3" && contains(ids(f.events("src_accb")), "e9") })
	})
}

func contains(xs []string, x string) bool { return count(xs, x) > 0 }

func count(xs []string, x string) (n int) {
	for _, v := range xs {
		if v == x {
			n++
		}
	}
	return
}
