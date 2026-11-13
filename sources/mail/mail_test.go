package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

var fixedNow = func() time.Time { return time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC) }

// replay 按版本样本目录回放录制输出（测试边界替身，不连真实账号），并断言只读、argv 形态正确。
func replay(t *testing.T, dir string, calls *[][]string) Runner {
	t.Helper()
	return func(_ context.Context, args []string) ([]byte, []byte, error) {
		*calls = append(*calls, args)
		joined := " " + strings.Join(args, " ") + " "
		for _, bad := range []string{" send ", " flag ", " move ", " delete ", " copy ", " add ", " remove ", " save ", " write ", " reply ", " forward "} {
			if strings.Contains(joined, bad) {
				t.Errorf("出现写操作参数: %v", args)
			}
		}
		read := func(name string) ([]byte, []byte, error) {
			b, err := os.ReadFile(filepath.Join("testdata", dir, name))
			if err != nil {
				t.Fatalf("缺样本 %s: %v", name, err)
			}
			return b, nil, nil
		}
		switch {
		case len(args) == 1 && args[0] == "--version":
			return read("version.txt")
		case args[0] == "envelope" && args[1] == "list":
			return read("list.json")
		case args[0] == "message" && args[1] == "read":
			if !strings.Contains(joined, " --preview ") {
				t.Errorf("message read 缺 --preview，会改已读标记: %v", args)
			}
			return read("read-" + args[len(args)-len(jsonFlagFor(dir))-1] + ".json")
		}
		t.Fatalf("意外调用 %v", args)
		return nil, nil, nil
	}
}

func jsonFlagFor(dir string) []string {
	dir, _, _ = strings.Cut(dir, "/") // "1.1.0/thread" 与 1.1.0 同属一个版本范围
	for _, vr := range Supported {
		if vr.Sample == dir {
			return vr.JSONFlag
		}
	}
	return nil
}

func exec1(t *testing.T, r Runner, input string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := run(context.Background(), strings.NewReader(input), &o, &e, r, fixedNow)
	return code, o.String(), e.String()
}

const in0 = `{"source_id":"src_m","run_id":"r","config":{"account":"work"},"cursor":"%s"}`

func input(cursor string) string { return strings.Replace(in0, "%s", cursor, 1) }

// ACT-MAIL-02、ACT-MAIL-03、ACT-MAIL-08：每个声明版本的录制样本 → 标准事件；id 取 Message-ID；
// 不设 sender_verified；只读；JSON 标志随版本变化。
func TestEveryDeclaredVersionConvertsRecordedSample(t *testing.T) {
	for _, vr := range Supported {
		t.Run(vr.Sample, func(t *testing.T) {
			var calls [][]string
			code, out, errs := exec1(t, replay(t, vr.Sample, &calls), input(""))
			if code != ExitOK {
				t.Fatalf("exit=%d %s", code, errs)
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if n := len(lines); n < 3 {
				t.Fatalf("lines=%v", lines)
			}
			want := map[string]struct{ id0, id1, sender, detail string }{
				"1.1.0": {"<mid-4@example.com>", "<mid-5@example.com>", "agent@example.com", "BODY-PLACEHOLDER\n\n\n--\nSent via AgentMail"},
				"2.0.0": {"<old-1@example.com>", "<inv-42@example.com>", "bob@example.com", "周报正文"},
			}[vr.Sample]
			var first map[string]any
			_ = json.Unmarshal([]byte(lines[0]), &first)
			if first["id"] != want.id0 || first["type"] != "mail.received" || first["sender"] != want.sender || first["detail"] != want.detail {
				t.Fatalf("first=%v", first)
			}
			if _, ok := first["sender_verified"]; ok {
				t.Fatal("不应设置 sender_verified")
			}
			var second map[string]any
			_ = json.Unmarshal([]byte(lines[1]), &second)
			if second["id"] != want.id1 {
				t.Fatalf("second=%v", second)
			}
			var cur map[string]string
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &cur); err != nil || cur["cursor"] == "" {
				t.Fatalf("末行不是游标: %q", lines[len(lines)-1])
			}
			flag := strings.Join(vr.JSONFlag, " ")
			for _, c := range calls[1:] {
				if !strings.HasSuffix(strings.Join(c, " "), flag) {
					t.Errorf("调用缺少该版本的 JSON 标志 %q: %v", flag, c)
				}
			}
			// Message-ID 的取法随版本：IDFromRead 的范围借 message read 的头部，且仍带 --preview（只读）。
			for _, c := range calls[2:] {
				if j := strings.Join(c, " "); strings.Contains(j, "-H Message-ID") != vr.IDFromRead || !strings.Contains(j, "--preview") {
					t.Errorf("message read argv=%v", c)
				}
			}
			if strings.Join(calls[1], " ") != "envelope list -a work -f INBOX --page-size 50 --page 1 "+flag {
				t.Errorf("list argv=%v", calls[1])
			}
		})
	}
}

// 游标之前的邮件被跳过；等于游标的保留；游标推进到最新邮件时间。
func TestCursorFiltersAndAdvances(t *testing.T) {
	var calls [][]string
	code, out, _ := exec1(t, replay(t, "2.0.0", &calls), input("2026-01-02T01:30:00Z"))
	if code != ExitOK {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "inv-42") || lines[1] != `{"cursor":"2026-01-02T01:30:00Z"}` {
		t.Fatalf("out=%s", out)
	}
}

// 无新邮件时游标保持不变。
func TestNoNewMailKeepsCursor(t *testing.T) {
	var calls [][]string
	code, out, _ := exec1(t, replay(t, "2.0.0", &calls), input("2026-06-01T00:00:00Z"))
	if code != ExitOK || strings.TrimSpace(out) != `{"cursor":"2026-06-01T00:00:00Z"}` {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func failing(version string, stderr string, runErr error, failOn string) Runner {
	return func(_ context.Context, args []string) ([]byte, []byte, error) {
		if args[0] == "--version" {
			if failOn == "version" {
				return nil, []byte(stderr), runErr
			}
			return []byte(version), nil, nil
		}
		return nil, []byte(stderr), runErr
	}
}

// ACT-MAIL-04、05、06：未安装、版本越界、认证失败 → 退出码 2，stdout 为空（无新游标）。
func TestToolFailuresExit2WithoutCursor(t *testing.T) {
	notFound := &exec.Error{Name: "himalaya", Err: exec.ErrNotFound}
	cases := map[string]Runner{
		"未安装":    failing("", "", notFound, "version"),
		"版本过低":   failing("himalaya 0.9.0", "", nil, ""),
		"版本过高":   failing("himalaya 3.0.0", "", nil, ""),
		"版本无法识别": failing("garbage", "", nil, ""),
		"认证失败":   failing("himalaya 2.0.0", "Error: authentication failed: invalid credentials", errors.New("exit status 1"), ""),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errs := exec1(t, r, input("2026-01-01T00:00:00Z"))
			if code != ExitConfig || out != "" || errs == "" {
				t.Fatalf("code=%d out=%q err=%q", code, out, errs)
			}
		})
	}
	_, _, errs := exec1(t, failing("himalaya 0.9.0", "", nil, ""), input(""))
	if !strings.Contains(errs, "版本") {
		t.Fatalf("应说明版本不符: %s", errs)
	}
}

// 网络等临时错误 → 退出码 1。
func TestTemporaryFailureExit1(t *testing.T) {
	r := failing("himalaya 2.0.0", "connection reset by peer", errors.New("exit status 1"), "")
	if code, out, _ := exec1(t, r, input("")); code != ExitTemporary || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// ACT-MAIL-07：缺 Message-ID、非 JSON、正文非字符串 → 退出码 2，不输出任何东西（含游标）。
func TestUnparsableOutputExit2WithoutCursor(t *testing.T) {
	outputs := map[string]string{
		"缺Message-ID": `[{"id":"1","subject":"x","from":{"addr":"a@b"},"date":"2026-01-02T00:00:00Z"}]`,
		"非JSON":       `not json`,
		"日期坏":         `[{"id":"1","message_id":"<a>","date":"yesterday"}]`,
		"缺编号":         `[{"message_id":"<a>","date":"2026-01-02T00:00:00Z"}]`,
	}
	for name, list := range outputs {
		t.Run(name, func(t *testing.T) {
			r := func(_ context.Context, args []string) ([]byte, []byte, error) {
				if args[0] == "--version" {
					return []byte("himalaya 2.0.0"), nil, nil
				}
				return []byte(list), nil, nil
			}
			if code, out, _ := exec1(t, r, input("")); code != ExitConfig || out != "" {
				t.Fatalf("code=%d out=%q", code, out)
			}
		})
	}
	r := func(_ context.Context, args []string) ([]byte, []byte, error) {
		switch args[0] {
		case "--version":
			return []byte("himalaya 2.0.0"), nil, nil
		case "envelope":
			b, err := os.ReadFile("testdata/2.0.0/list.json")
			if err != nil {
				t.Fatal(err)
			}
			return b, nil, nil
		}
		return []byte(`{"not":"string"}`), nil, nil
	}
	if code, out, _ := exec1(t, r, input("")); code != ExitConfig || out != "" {
		t.Fatalf("正文坏: code=%d out=%q", code, out)
	}
}

// 1.x 范围的 Message-ID 来自 message read 头部；头部缺失 → 退出码 2，不输出任何东西。
func TestReadWithoutMessageIDHeaderExit2(t *testing.T) {
	var calls [][]string
	rec := replay(t, "1.1.0", &calls)
	r := func(ctx context.Context, args []string) ([]byte, []byte, error) {
		if args[0] == "message" {
			return []byte(`"BODY-PLACEHOLDER"`), nil, nil
		}
		return rec(ctx, args)
	}
	if code, out, _ := exec1(t, r, input("")); code != ExitConfig || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func TestBadInputExit2(t *testing.T) {
	for _, in := range []string{`nope`, input("not-a-time")} {
		if code, out, _ := exec1(t, nil, in); code != ExitConfig || out != "" {
			t.Fatalf("%q: code=%d out=%q", in, code, out)
		}
	}
}

// 真实 exec 路径（ACT-MAIL-04）：PATH 里没有 himalaya → 退出码 2；不连任何账号。
func TestRealExecWithoutToolExit2(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var o, e bytes.Buffer
	if code := Run(context.Background(), strings.NewReader(input("2026-01-01T00:00:00Z")), &o, &e); code != ExitConfig || o.Len() != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, o.String(), e.String())
	}
}

// 真实 exec 路径：同名替身脚本只收到白名单环境变量，输出被正确解析。
func TestRealExecEnvWhitelist(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
--version) echo "himalaya 2.0.0";;
envelope) if env | grep -q '^SECRET='; then echo leaked >&2; exit 9; fi; echo '[]';;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "himalaya"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("SECRET", "x")
	var o, e bytes.Buffer
	code := Run(context.Background(), strings.NewReader(input("2026-01-01T00:00:00Z")), &o, &e)
	if code != ExitOK || strings.TrimSpace(o.String()) != `{"cursor":"2026-01-01T00:00:00Z"}` {
		t.Fatalf("code=%d out=%q err=%q", code, o.String(), e.String())
	}
}

// 空游标且无邮件：用 window.to 起步。
func TestFirstRunEmptyMailboxUsesWindowTo(t *testing.T) {
	r := func(_ context.Context, args []string) ([]byte, []byte, error) {
		if args[0] == "--version" {
			return []byte("himalaya 2.0.0"), nil, nil
		}
		return []byte(`[]`), nil, nil
	}
	code, out, _ := exec1(t, r, `{"config":{},"window":{"to":"2026-03-01T00:00:00Z"}}`)
	if code != ExitOK || strings.TrimSpace(out) != `{"cursor":"2026-03-01T00:00:00Z"}` {
		t.Fatalf("code=%d out=%q", code, out)
	}
	_, out, _ = exec1(t, r, `{"config":{}}`)
	if !strings.Contains(out, "2026-01-04T00:00:00Z") {
		t.Fatalf("out=%q", out)
	}
}

func TestDetailTruncated(t *testing.T) {
	if got := truncate(strings.Repeat("好", maxDetail)); len(got) > maxDetail {
		t.Fatal(len(got))
	}
}

// pagedRunner 提供 total 封新到旧的邮件（均晚于游标），按 --page/--page-size 切页。
func pagedRunner(total int, listCalls *int) Runner {
	return func(_ context.Context, args []string) ([]byte, []byte, error) {
		switch args[0] {
		case "--version":
			return []byte("himalaya 2.0.0"), nil, nil
		case "message":
			return []byte(`"body"`), nil, nil
		}
		*listCalls++
		var size, page int
		for i, a := range args {
			if a == "--page-size" {
				size, _ = strconv.Atoi(args[i+1])
			}
			if a == "--page" {
				page, _ = strconv.Atoi(args[i+1])
			}
		}
		var envs []map[string]any
		for n := (page - 1) * size; n < page*size && n < total; n++ {
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(total-n) * time.Minute) // n=0 最新
			envs = append(envs, map[string]any{"id": n + 1, "message_id": fmt.Sprintf("<m%d@x>", n), "subject": "s", "date": at.Format(time.RFC3339)})
		}
		b, _ := json.Marshal(envs)
		return b, nil, nil
	}
}

// 评审阻塞项 1：积压超过一页时翻页取全，不因游标越过而丢信。
func TestBacklogBeyondOnePageIsFullyFetched(t *testing.T) {
	var lists int
	in := `{"config":{"page_size":10,"max_messages":1000},"cursor":"2025-12-31T00:00:00Z"}`
	code, out, _ := exec1(t, pagedRunner(25, &lists), in)
	if code != ExitOK || lists != 3 {
		t.Fatalf("code=%d lists=%d", code, lists)
	}
	if n := strings.Count(out, `"type":"mail.received"`); n != 25 {
		t.Fatalf("events=%d", n)
	}
}

// 翻到早于游标的信件即停，不多翻页。
func TestPagingStopsAtCursor(t *testing.T) {
	var lists int
	in := `{"config":{"page_size":10},"cursor":"2026-01-01T01:27:00Z"}` // 最新 14 封不早于游标
	code, out, _ := exec1(t, pagedRunner(100, &lists), in)
	if code != ExitOK || lists != 2 || strings.Count(out, "mail.received") != 14 {
		t.Fatalf("code=%d lists=%d out=%s", code, lists, out)
	}
}

// 带游标时积压超过页数上限：退出 2（needs_input，提示调大 page_size），不输出游标。
func TestBacklogOverCapExit1WithoutCursor(t *testing.T) {
	var lists int
	in := `{"config":{"page_size":1,"max_messages":1000},"cursor":"2025-12-31T00:00:00Z"}`
	code, out, errs := exec1(t, pagedRunner(maxPages+5, &lists), in)
	if code != ExitConfig || out != "" || !strings.Contains(errs, "page_size") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errs)
	}
}

// 评审阻塞项 2：发件人 Date 在未来时，游标不超过当前时间。
func TestFutureDatedMailCannotPushCursorPastNow(t *testing.T) {
	r := func(_ context.Context, args []string) ([]byte, []byte, error) {
		switch args[0] {
		case "--version":
			return []byte("himalaya 2.0.0"), nil, nil
		case "message":
			return []byte(`"b"`), nil, nil
		}
		return []byte(`[{"id":1,"message_id":"<f@x>","subject":"s","date":"2099-01-01T00:00:00Z"}]`), nil, nil
	}
	code, out, _ := exec1(t, r, input("2026-01-01T00:00:00Z"))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != ExitOK || len(lines) != 2 || lines[1] != `{"cursor":"2026-01-04T00:00:00Z"}` { // fixedNow
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// 评审阻塞项 3：空游标 + 超大邮箱必须 exit 0、只取最新一批、游标推进到最新邮件，且不翻遍全部页。
func TestFirstRunHugeMailboxIsBoundedAndAdvances(t *testing.T) {
	var lists int
	code, out, errs := exec1(t, pagedRunner(6000, &lists), `{"config":{"max_messages":100}}`)
	if code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 101 || lists != 2 { // 100 封 + 游标；每页 50 → 只翻 2 页
		t.Fatalf("lines=%d lists=%d", len(lines), lists)
	}
	if lines[100] != `{"cursor":"2026-01-04T00:00:00Z"}` { // 最新邮件在 now 之后被截到 now
		t.Fatalf("cursor=%s", lines[100])
	}
}

// 首次拉取不回溯 lookback 之外的邮件；window.from 优先。
func TestFirstRunLookbackAndWindowFrom(t *testing.T) {
	var calls [][]string
	_, out, _ := exec1(t, replay(t, "2.0.0", &calls), `{"config":{"lookback_days":1}}`) // now=1/4 → 1/3 之后：两封都更早
	if strings.Contains(out, "mail.received") {
		t.Fatalf("不应有事件: %s", out)
	}
	_, out, _ = exec1(t, replay(t, "2.0.0", &calls), `{"config":{"lookback_days":1},"window":{"from":"2026-01-02T00:00:00Z"}}`)
	if strings.Count(out, "mail.received") != 1 || !strings.Contains(out, "inv-42") {
		t.Fatalf("window.from 未生效: %s", out)
	}
}

// 带游标的积压超过 max_messages：从最旧的接着读，游标只推进到已读部分，多次运行收敛。
func TestBudgetConvergesAcrossRuns(t *testing.T) {
	cursor, seen := "2025-12-31T00:00:00Z", map[string]bool{}
	for run := 0; run < 10 && len(seen) < 25; run++ {
		var lists int
		in := `{"config":{"page_size":10,"max_messages":10},"cursor":"` + cursor + `"}`
		code, out, errs := exec1(t, pagedRunner(25, &lists), in)
		if code != ExitOK {
			t.Fatalf("run %d code=%d %s", run, code, errs)
		}
		lines := strings.Split(strings.TrimSpace(out), "\n")
		for _, l := range lines[:len(lines)-1] {
			var ev event
			_ = json.Unmarshal([]byte(l), &ev)
			seen[ev.ID] = true
		}
		var c map[string]string
		_ = json.Unmarshal([]byte(lines[len(lines)-1]), &c)
		if c["cursor"] <= cursor && len(seen) < 25 {
			t.Fatalf("游标未前进: %s -> %s", cursor, c["cursor"])
		}
		cursor = c["cursor"]
	}
	if len(seen) != 25 {
		t.Fatalf("seen=%d", len(seen))
	}
}

// 单次运行超时：已读部分输出，游标只到已读部分，退出 0。
func TestTimeoutMidRunKeepsReadPart(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	reads := 0
	var lists int
	pr := pagedRunner(5, &lists)
	r := func(c context.Context, args []string) ([]byte, []byte, error) {
		if args[0] == "message" {
			if reads++; reads == 2 {
				cancel()
				return nil, nil, c.Err()
			}
		}
		return pr(c, args)
	}
	var o, e bytes.Buffer
	code := run(ctx, strings.NewReader(`{"config":{},"cursor":"2025-12-31T00:00:00Z"}`), &o, &e, r, fixedNow)
	lines := strings.Split(strings.TrimSpace(o.String()), "\n")
	if code != ExitOK || len(lines) != 2 { // 1 封 + 游标
		t.Fatalf("code=%d out=%q err=%q", code, o.String(), e.String())
	}
}

// ACT-MAIL-10、ACT-MAIL-02、ACT-MAIL-08：线程样本（1.1.0/thread）。读正文的同一次调用取
// Message-ID、References、In-Reply-To；关联键是线程根 Message-ID（References 首项 > In-Reply-To > 自己）。
func TestThreadCorrelationKeys(t *testing.T) {
	var calls [][]string
	code, out, errs := exec1(t, replay(t, "1.1.0/thread", &calls), input(""))
	if code != ExitOK {
		t.Fatalf("exit=%d %s", code, errs)
	}
	keys := map[string][]string{}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		var ev struct {
			ID   string   `json:"id"`
			Keys []string `json:"correlation_keys"`
		}
		_ = json.Unmarshal([]byte(l), &ev)
		if ev.ID != "" {
			keys[ev.ID] = ev.Keys
		}
	}
	root := "thread:<mid-t1@example.com>"
	want := map[string]string{
		"<mid-t1@example.com>": root, // 原信：自己的 Message-ID
		"<mid-t2@example.com>": root, // References + In-Reply-To
		"<mid-t3@example.com>": root, // 只有 In-Reply-To
		"<mid-t4@example.com>": root, // 多级回复：取 References（折行）首项，不是直接父信
	}
	for id, k := range want {
		if len(keys[id]) != 1 || keys[id][0] != k {
			t.Errorf("%s correlation_keys=%v, want [%s]", id, keys[id], k)
		}
	}
	// 同一次 message read 取三个头，且仍只读。
	for _, c := range calls[2:] {
		if j := strings.Join(c, " "); !strings.Contains(j, "-H Message-ID -H References -H In-Reply-To") || !strings.Contains(j, "--preview") {
			t.Errorf("message read argv=%v", c)
		}
	}
}

// 没有线程头时关联键只含自己的 Message-ID（回信按普通事件处理）；2.0.0 范围同样给出。
func TestKeysWithoutThreadHeaders(t *testing.T) {
	for _, dir := range []string{"1.1.0", "2.0.0"} {
		var calls [][]string
		code, out, _ := exec1(t, replay(t, dir, &calls), input(""))
		if code != ExitOK {
			t.Fatal(dir, code)
		}
		first, _, _ := strings.Cut(out, "\n")
		var ev struct {
			ID   string   `json:"id"`
			Keys []string `json:"correlation_keys"`
		}
		_ = json.Unmarshal([]byte(first), &ev)
		if len(ev.Keys) != 1 || ev.Keys[0] != "thread:"+ev.ID {
			t.Errorf("%s keys=%v id=%s", dir, ev.Keys, ev.ID)
		}
	}
}
