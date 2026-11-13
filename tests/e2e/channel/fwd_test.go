//go:build e2e

package channel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 四条消息的标记：subject 里的标记会出现在所有读取入口的输出里，用来判断泄露。
const (
	mAlice   = "FWD-ALICE-OK"         // alice，已核实：唯一进白名单的
	mBob     = "FWD-BOB"              // bob，已核实
	mAliceUv = "FWD-ALICE-UNVERIFIED" // alice，未核实
	mNoSend  = "FWD-NOSENDER"         // 无发送者
)

// observeFor 是 ACT-FWD-06 要求的观察时长（验收文档：2 分钟）；APA_E2E_OBSERVE 只用于本地调试时缩短。
var observeFor = func() time.Duration {
	if d, err := time.ParseDuration(os.Getenv("APA_E2E_OBSERVE")); err == nil {
		return d
	}
	return 2 * time.Minute
}()

// launched 是后台运行与任务型运行的总数（来源拉取等短任务不算）。
func (f *bg) launched() int {
	return len(f.runs("--category", "background")) + len(f.runs("--category", "task"))
}

type msgView struct {
	ID             string `json:"id"`
	Subject        string `json:"subject"`
	AttachState    string `json:"attach_state"`
	ForwardAllowed bool   `json:"forward_allowed"`
}

// webMessages 是 Web 事项页看到的全部挂靠消息。
func (f *bg) webMessages(item string) []msgView {
	f.t.Helper()
	var out struct {
		Messages []msgView `json:"messages"`
	}
	must(f.t, json.Unmarshal([]byte(f.webOK("GET", "/items/"+item+"/messages", "", 200)), &out))
	return out.Messages
}

func subjects(ms []msgView) string {
	var s []string
	for _, m := range ms {
		s = append(s, m.Subject)
	}
	return strings.Join(s, "|")
}

// showMessages 是 `apa item show` 输出里合并的挂靠消息。
func (f *bg) showMessages(token, item string) string {
	f.t.Helper()
	return f.apa(token, 0, "item", "show", item).Stdout
}

// contextOf 取 `apa context --scope item --item <id>` 的原始输出与该事项的确认游标。
func (f *bg) contextOf(token, item string) (raw, cursor string) {
	f.t.Helper()
	raw = f.apa(token, 0, "context", "--scope", "item", "--item", item).Stdout
	var out struct {
		Messages []struct {
			Item   string `json:"item"`
			Cursor string `json:"cursor"`
		} `json:"messages"`
	}
	must(f.t, json.Unmarshal([]byte(raw), &out))
	for _, g := range out.Messages {
		if g.Item == item {
			cursor = g.Cursor
		}
	}
	return
}

func only(t *testing.T, where, got string, want string, notWant ...string) {
	t.Helper()
	if !strings.Contains(got, want) {
		t.Errorf("%s 应读到 %s: %s", where, want, got)
	}
	for _, n := range notWant {
		if strings.Contains(got, n) {
			t.Errorf("%s 泄露了 %s: %s", where, n, got)
		}
	}
}

// runnerScript 以运行者身份读 child 的全部入口，并在运行目录里找未进白名单消息的残留。
func runnerScript(child string) string {
	return preamble + `
rm -f "$OUT"/rd_*
$APA context --scope item --item ` + child + ` > "$OUT/rd_ctx.out" 2>&1; rc rd_ctx
$APA item show ` + child + ` > "$OUT/rd_show.out" 2>&1; rc rd_show
ls -la . > "$OUT/rd_ls.out" 2>&1
grep -rl "` + mBob + `\|` + mAliceUv + `\|` + mNoSend + `" . > "$OUT/rd_leak.out" 2>&1
`
}

// TestFlowForwardAllowlist 对应验收文档 flow-forward-allowlist（ACT-FWD-01..08；ACT-FWD-09 见 TestForwardAllowlistDefaults）。
func TestFlowForwardAllowlist(t *testing.T) {
	f := setupBG(t)
	f.WireEndpoints() // CLS_STUB 与 MODEL_STUB 接到内核：ACT-FWD-07 的入站流水线用内置 inbound-triage 模板
	src := f.newSource()
	f.auto("main-read")
	const pr = "https://example.invalid/acc/test-repo/pull/101"
	var parent, child string
	emitted := map[string]string{}

	f.step("ACT-FWD-01 配置白名单和消息", func(t *testing.T) {
		f.webOK("PUT", "/settings/allowlists", `{"forward_allowlist":{"`+srcID+`":["alice"]}}`, 200)
		f.mode("summary-only", preamble+`
$APA delegate --parent "$ITEM" --assignee work-host.cli --title fwd-child > "$OUT/deleg.out" 2>&1; rc deleg
`)
		parent = f.task("fwd-parent", "acc-work", "acc-main", "acc-reread")
		f.settled(parent, 1)
		eq(t, "运行者委派退出码 "+f.outFile("deleg.out"), f.outFile("deleg.rc"), "0")
		child = idFrom(f.outFile("deleg.out"))
		f.apa(f.WRK, 0, "report", child, "--outcome", "progress", "--summary", "已开 PR", "--pr", pr)
		for name, m := range map[string]struct {
			sender   string
			verified bool
		}{mAlice: {"alice", true}, mBob: {"bob", true}, mAliceUv: {"alice", false}, mNoSend: {"", false}} {
			emitted[name] = src.emit(name, m.sender, m.verified, pr)
		}
		for name, id := range emitted {
			if ev := src.settled(id); ev.Item != child || ev.Disposition != "attached" {
				t.Fatalf("%s 应挂靠 %s: %+v", name, child, ev)
			}
		}
		ms := f.webMessages(child)
		eq(t, "Web 事项页消息数", len(ms), 4)
		for _, m := range ms {
			eq(t, m.Subject+" 的 forward_allowed", m.ForwardAllowed, m.Subject == mAlice)
		}
	})

	f.step("ACT-FWD-02 运行者只看到白名单内的消息", func(t *testing.T) {
		f.mode("summary-only", runnerScript(child))
		f.auto("reread")
		f.apa(f.DEL, 0, "item", "update", parent, "--title", "fwd-parent 再读", "--reason", "再读", "--evidence", "e2e 再触发一次运行")
		f.waitFor("第二次运行拉起", 20*time.Second, func() bool { return len(f.runsOf(parent)) == 2 })
		f.setMode("reread", "observe") // 运行结束时内核改事项，不应再触发
		f.settled(parent, 2)
		eq(t, "读 context 退出码", f.outFile("rd_ctx.rc"), "0")
		eq(t, "读 item show 退出码", f.outFile("rd_show.rc"), "0")
		only(t, "运行内 apa context", f.outFile("rd_ctx.out"), mAlice, mBob, mAliceUv, mNoSend)
		only(t, "运行内 apa item show", f.outFile("rd_show.out"), mAlice, mBob, mAliceUv, mNoSend)
		if leak := f.outFile("rd_leak.out"); leak != "" {
			t.Errorf("运行目录里不应有未进白名单的消息: %s（目录: %s）", leak, f.outFile("rd_ls.out"))
		}
	})

	var cursor string
	f.step("ACT-FWD-03 子事项的执行者同样被过滤", func(t *testing.T) {
		var raw string
		raw, cursor = f.contextOf(f.WRK, child)
		only(t, "WRK 的 apa context", raw, mAlice, mBob, mAliceUv, mNoSend)
		only(t, "WRK 的 apa item show", f.showMessages(f.WRK, child), mAlice, mBob, mAliceUv, mNoSend)
		if cursor == "" {
			t.Error("负责者应拿到确认游标")
		}
	})

	f.step("ACT-FWD-04 未进白名单的消息本人可见，不算已处理", func(t *testing.T) {
		// 本人在场的交互式主会话持 delegate token：能看到全部，且只看不推进。
		mainSession, _ := f.contextOf(f.DEL, child)
		for _, m := range []string{mAlice, mBob, mAliceUv, mNoSend} {
			if !strings.Contains(mainSession, m) {
				t.Errorf("交互式主会话应看到 %s: %s", m, mainSession)
			}
		}
		eq(t, "Web 事项页消息数", len(f.webMessages(child)), 4)
		f.apa(f.WRK, 0, "report", child, "--outcome", "progress", "--summary", "已处理", "--ack-cursor", cursor)
		for _, m := range f.webMessages(child) {
			want := "pending"
			if m.Subject == mAlice {
				want = "processed"
			}
			eq(t, m.Subject+" 的 attach_state", m.AttachState, want)
		}
	})

	f.step("ACT-FWD-05 白名单变更立即生效", func(t *testing.T) {
		before := len(f.events("", "allowlist.changed"))
		f.webOK("PUT", "/settings/allowlists", `{"forward_allowlist":{"`+srcID+`":["alice","bob"]}}`, 200)
		only(t, "加入 bob 后 WRK 的 item show", f.showMessages(f.WRK, child), mBob, mAliceUv, mNoSend)
		raw, _ := f.contextOf(f.WRK, child)
		only(t, "加入 bob 后 WRK 的 apa context", raw, mBob, mAliceUv, mNoSend)
		f.webOK("PUT", "/settings/allowlists", `{"forward_allowlist":{"`+srcID+`":["alice"]}}`, 200)
		if got := f.showMessages(f.WRK, child); strings.Contains(got, mBob) {
			t.Errorf("移除 bob 后不应再读到: %s", got)
		}
		if raw, _ := f.contextOf(f.WRK, child); strings.Contains(raw, mBob) {
			t.Errorf("移除 bob 后 context 不应再给出: %s", raw)
		}
		eq(t, "新增 allowlist.changed 事件数", len(f.events("", "allowlist.changed"))-before, 2)
	})

	f.step("ACT-FWD-06 挂靠消息不拉起运行", func(t *testing.T) {
		runs := f.launched()
		late := src.emit("FWD-ALICE-LATE", "alice", true, pr)
		if ev := src.settled(late); ev.Item != child || ev.AttachState != "pending" {
			t.Fatalf("新消息应挂靠: %+v", ev)
		}
		time.Sleep(observeFor)
		eq(t, "观察期内新增 Run 数", f.launched()-runs, 0)
		// 下一次运行才带上。
		f.mode("summary-only", runnerScript(child))
		f.setMode("reread", "auto")
		f.apa(f.DEL, 0, "item", "update", parent, "--title", "fwd-parent 下一次", "--reason", "下一次", "--evidence", "e2e 再触发一次运行")
		f.waitFor("下一次运行拉起", 20*time.Second, func() bool { return len(f.runsOf(parent)) == 3 })
		f.setMode("reread", "observe")
		f.settled(parent, 3)
		only(t, "下一次运行的 apa context", f.outFile("rd_ctx.out"), "FWD-ALICE-LATE", mBob)
	})

	f.step("ACT-FWD-07 退回判断流后不进入无人值守", func(t *testing.T) {
		const mark = "FWD-BOB-RELEASED"
		f.Cls.PYes(modelstub.Key{Contains: mark}, 0.90)
		f.ModelStub.When(mark, modelstub.Reply{Content: `{"title":"fwd-07 起草标题","kind":"task","recommend":"accept","reason":"bob 的跟进"}`})
		id := src.emit(mark, "bob", true, pr)
		if ev := src.settled(id); ev.Item != child || ev.AttachState != "pending" {
			t.Fatalf("应挂靠且未处理: %+v", ev)
		}
		runs := f.launched()
		f.apa(f.DEL, 0, "item", "transition", child, "--to", "done", "--reason", "完成", "--evidence", "e2e 关闭子事项")
		var ev srcEvent
		f.waitFor("退回的消息重新进入入站流水线并得到处置", 40*time.Second, func() bool {
			ev, _ = src.event(id)
			return ev.AttachState == "released" && ev.Disposition == "proposed"
		})
		if ev.Item == "" || ev.Item == child {
			t.Fatalf("退回的消息应成为新的建议事项: %+v", ev)
		}
		eq(t, "建议事项的 phase", f.phase(ev.Item), "proposed")
		eq(t, "起草请求数", len(f.draftRequests(mark)), 1)
		for _, r := range f.ModelStub.Requests() {
			if strings.Contains(r.BodyText, "ACCMAIN") && strings.Contains(r.BodyText, mark) {
				t.Errorf("内容不应进入任何运行提示词: %s", r.BodyText)
			}
		}
		time.Sleep(3 * time.Second)
		eq(t, "后台/任务型 Run 数不变", f.launched(), runs)
		// 运行目录里（包括此前每次无人值守运行）都不应出现这条消息的内容。
		if out, _ := exec.Command("grep", "-rl", mark, filepath.Join(f.Home, "runs")).Output(); len(out) > 0 {
			t.Errorf("运行目录里出现了退回消息的内容: %s", out)
		}
	})

	f.step("ACT-FWD-08 本人重新委派开始新链路", func(t *testing.T) {
		const pr2 = "https://example.invalid/acc/test-repo/pull/102"
		body, _ := json.Marshal(map[string]string{"parent": parent, "assignee": "work-host.cli", "title": "fwd-redelegated"})
		child2 := idFrom(f.webOK("POST", "/delegations", string(body), 201))
		if child2 == "" || child2 == child {
			t.Fatalf("应建出新子事项")
		}
		f.apa(f.WRK, 0, "report", child2, "--outcome", "progress", "--summary", "新 PR", "--pr", pr2)
		for name, m := range map[string]struct {
			sender   string
			verified bool
		}{mAlice: {"alice", true}, mBob: {"bob", true}, mAliceUv: {"alice", false}, mNoSend: {"", false}} {
			src.settled(src.emit(name+"-2", m.sender, m.verified, pr2))
		}
		raw, _ := f.contextOf(f.WRK, child2)
		for _, m := range []string{mAlice, mBob, mAliceUv, mNoSend} {
			if !strings.Contains(raw, m+"-2") {
				t.Errorf("新子事项不带 unattended，WRK 应读到 %s: %s", m, raw)
			}
		}
	})
}

// TestForwardAllowlistDefaults 对应 ACT-FWD-09：全新环境白名单为空，能力包与 delegate 都改不了。
func TestForwardAllowlistDefaults(t *testing.T) {
	e := baseline.New(t)
	e.Init()
	e.Serve()
	e.IssueCredentials()
	f := &bg{Env: e, t: t}
	got := f.webOK("GET", "/settings/allowlists", "", 200)
	if !strings.Contains(got, `"forward_allowlist":{}`) {
		t.Errorf("全新环境转发白名单应为空: %s", got)
	}
	dir := filepath.Join(t.TempDir(), "acc-allow")
	must(t, os.MkdirAll(dir, 0o755))
	must(t, os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte("name: acc-allow\nversion: 1.0.0\ndescription: 声明白名单\nforward_allowlist:\n  src_acc: [mallory]\n"), 0o644))
	if r := e.Apa(e.OBS, "", "pack", "validate", dir); r.Code == 0 {
		t.Errorf("声明白名单字段的能力包应被拒绝: %s", r.Stdout)
	}
	for _, method := range []string{"PATCH", "PUT"} {
		r := e.APIRaw(method, "/v1/settings/allowlists", `{"forward_allowlist":{"src_acc":["mallory"]}}`, e.DEL)
		if r.Code == 200 {
			t.Errorf("DEL %s 不应成功: %d %s", method, r.Code, r.Body)
		}
	}
	if got := f.webOK("GET", "/settings/allowlists", "", 200); strings.Contains(got, "mallory") {
		t.Errorf("白名单不应被改动: %s", got)
	}
}
