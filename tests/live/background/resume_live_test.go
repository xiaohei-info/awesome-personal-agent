//go:build live

package background

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const askBody = "这个事项需要本人在方案 A 与方案 B 之间决定，你不能替本人决定。请先用 apa context 读取事项，然后用 apa ask <本事项 id> --question \"采用方案 A 还是方案 B？\" --option A=方案甲 --option B=方案乙 --resume-note \"本人选定后据此继续，并用 apa report 记录所选方案\" 提交决策请求，提交后立即结束，不要等待回答。"

// resumeEvent 取事项上第 n 个 resume.launched 事件的 payload。
func (l *live) resumeEvents(item string) []event {
	var out []event
	for _, e := range l.events(item) {
		if e.Type == "resume.launched" {
			out = append(out, e)
		}
	}
	return out
}

// askAndStop 建 tag 事项，等运行者用 apa ask 提交决策请求并结束；返回事项、第一次运行和它的会话 id。
func (l *live) askAndStop(title string, tags ...string) (string, run, string, askView) {
	l.t.Helper()
	id := l.openItem(title, askBody, tags...)
	r := l.settled(id, 1)[0]
	eq(l.t, "Run state", r.State, "succeeded")
	eq(l.t, "事项阶段", l.phase(id), "needs_input")
	a := l.askOf(id)
	if a.ResumeNote == "" || a.Question == "" || len(a.Options) < 2 { // 模型会改写措辞，所以只要求存在，后面按账本里存的原文核对
		l.t.Fatalf("决策请求缺 resume_note 或选项: %+v", a)
	}
	return id, r, l.sessionOf(id, r.ID), a
}

// delivered 以渠道 ext（CHAN）领取该事项的决策投递并回报送达；两分钟内没有领到返回 false。
func (l *live) delivered(item string) bool {
	l.t.Helper()
	for end := time.Now().Add(2 * time.Minute); time.Now().Before(end); {
		r := l.APIRaw("GET", "/v1/deliveries/next?wait=5", "", l.CHAN)
		if r.Code != 200 {
			continue
		}
		var d struct {
			ID      string `json:"id"`
			Item    string `json:"item"`
			Class   string `json:"class"`
			Version int64  `json:"version"`
		}
		must(l.t, json.Unmarshal([]byte(r.Body), &d))
		if d.Item != item || d.Class != "decision" {
			continue
		}
		ack := l.APIRaw("POST", "/v1/deliveries/"+d.ID+"/ack", fmt.Sprintf(`{"version":%d,"external_ref":"live-%s"}`, d.Version, item), l.CHAN)
		return ack.Code == 200
	}
	return false
}

// TestFlowBgResume 对应验收文档 flow-bg-resume（ACT-RES-01..05，真实 Hermes）。
func TestFlowBgResume(t *testing.T) {
	l := setup(t)
	l.configure()
	l.setMode(packRule+"handle-labelled", "auto")
	l.setMode(liveRule+"noresume", "auto")
	var item string
	var first run
	var sess, resumeNote string

	l.step("ACT-RES-01 决定前结束运行并通知", func(t *testing.T) {
		var ask askView
		item, first, sess, ask = l.askAndStop("res-01 需要本人决定", "agent-ok")
		resumeNote = ask.ResumeNote
		if sess == "" {
			t.Errorf("事项应记录运行者的 Hermes 会话 id")
		}
		if !l.delivered(item) {
			t.Errorf("决策请求应已触达（渠道没有领到 %s 的决策投递）", item)
		}
		out, _ := exec.Command("pgrep", "-f", first.ID).Output()
		if strings.TrimSpace(string(out)) != "" {
			t.Errorf("运行结束后不应有进程在等待: %s", out)
		}
	})

	l.step("ACT-RES-02 回答后用同一会话续接", func(t *testing.T) {
		l.webOK("POST", "/items/"+item+"/decide", `{"choice":"A"}`, 200)
		rs := l.resumeEvents(item)
		if len(rs) != 1 {
			t.Fatalf("回答的同一事务应写 1 个续接 Run，实际 %d 个 resume.launched", len(rs))
		}
		eq(t, "续接方式", rs[0].Payload["mode"], "resumed")
		eq(t, "续接用的会话 id", rs[0].Payload["session_id"], sess)
		eq(t, "续接 harness", rs[0].Payload["harness"], "hermes")
		runs := l.settled(item, 2)
		var next run
		for _, r := range runs {
			if r.ID != first.ID {
				next = r
			}
		}
		eq(t, "续接 Run 的 session_id", next.Session, sess)
		if tok := l.runFile(next.ID, "token"); tok == "" || tok == l.runFile(first.ID, "token") {
			t.Errorf("续接应使用新的运行凭据")
		}
		args := l.runFile(next.ID, "args.txt")
		for _, want := range []string{resumeNote, "A（方案甲）"} {
			if !strings.Contains(args, want) {
				t.Errorf("续接提示词/参数缺少 %q:\n%s", want, args)
			}
		}
		time.Sleep(20 * time.Second) // 事件规则随后命中时应看到在途/已有续接而跳过
		eq(t, "没有重复拉起，Run 数", len(l.runs(item)), 2)
	})

	l.step("ACT-RES-03 不支持续接时用新会话", func(t *testing.T) {
		id, first, _, ask := l.askAndStop("res-03 不支持续接", "live-noresume")
		l.webOK("POST", "/items/"+id+"/decide", `{"choice":"B"}`, 200)
		rs := l.resumeEvents(id)
		if len(rs) != 1 {
			t.Fatalf("应写 1 个续接 Run，实际 %d", len(rs))
		}
		eq(t, "续接方式", rs[0].Payload["mode"], "fresh")
		var next run
		for _, r := range l.settled(id, 2) {
			if r.ID != first.ID {
				next = r
			}
		}
		args := l.runFile(next.ID, "args.txt")
		for _, want := range []string{ask.Question, "B（方案乙）", ask.ResumeNote} {
			if !strings.Contains(args, want) {
				t.Errorf("新会话提示词缺少 %q:\n%s", want, args)
			}
		}
		if strings.Contains(args, "--resume") {
			t.Errorf("不支持续接的 harness 不应带 --resume: %s", args)
		}
		ctxFile, _ := os.ReadFile(filepath.Join(l.Home, "runs", next.ID, "context.md"))
		for _, want := range []string{ask.Question, "B（方案乙）", ask.ResumeNote, "挂靠消息"} {
			if !strings.Contains(string(ctxFile), want) {
				t.Errorf("资料文件缺少 %q:\n%s", want, ctxFile)
			}
		}
	})

	l.step("ACT-RES-05 恢复前按当前名单复核", func(t *testing.T) {
		l.setMode(liveRule+"task-ask", "auto")
		parent := l.openItem("res-05 需要本人决定（用 apa ask 提交，不要自己决定）", askBody, "live-task")
		var child string
		l.waitFor("任务型运行建立并结束", waitRun, func() bool {
			for _, r := range l.runsByCategory() {
				if r.Basis.RuleID == liveRule+"task-ask" && r.Target != parent {
					child = r.Target
				}
			}
			if child == "" {
				return false
			}
			for _, r := range l.runs(child) {
				if r.State == "queued" || r.State == "running" {
					return false
				}
			}
			return len(l.runs(child)) == 1
		})
		eq(t, "子事项阶段", l.phase(child), "needs_input")
		// 等待续接时把声明执行者从项目名单移除，再回答。
		l.webOK("PUT", "/settings/projects/"+l.Projects["acc-work"]+"/allowed-executors", `{"executors":[]}`, 200)
		l.webOK("POST", "/items/"+child+"/decide", `{"choice":"A"}`, 200)
		time.Sleep(15 * time.Second)
		evs := l.events(child)
		if hasType(evs, "resume.launched") || len(l.runs(child)) != 1 {
			t.Errorf("名单已不含执行者，不应续接: 运行 %d 个", len(l.runs(child)))
		}
		if !hasType(evs, "resume.refused") {
			t.Errorf("应记 resume.refused")
		}
		eq(t, "子事项阶段", l.phase(child), "needs_input")
	})
}

// TestBgResumeSessionLost 对应 ACT-RES-04：会话失效时改用新会话。独立成测试，便于单独复现。
func TestBgResumeSessionLost(t *testing.T) {
	l := setup(t)
	l.configure()
	l.setMode(packRule+"handle-labelled", "auto")
	l.step("ACT-RES-04 会话失效时用新会话", func(t *testing.T) {
		id, _, sid, _ := l.askAndStop("res-04 会话失效", "agent-ok")
		if sid == "" {
			t.Fatal("没有会话 id")
		}
		cmd := exec.Command(l.hbin, "sessions", "delete", sid, "--yes")
		cmd.Env = append(os.Environ(), "HERMES_HOME="+l.hhome)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("hermes sessions delete: %v\n%s", err, out)
		}
		l.webOK("POST", "/items/"+id+"/decide", `{"choice":"A"}`, 200)
		l.settled(id, 2)
		if !hasType(l.events(id), "resume.session_lost") {
			t.Errorf("会话失效应记 resume.session_lost 并改用新会话")
		}
		rs := l.resumeEvents(id)
		if len(rs) < 2 || rs[len(rs)-1].Payload["mode"] != "fresh" {
			t.Errorf("应改用新会话续接: %+v", rs)
		}
		if p := l.phase(id); p == "needs_input" {
			t.Errorf("事项不应卡在 needs_input")
		}
	})
}
