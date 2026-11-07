//go:build live

package background

import (
	"context"
	"encoding/json"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// configure 对应 ACT-BGM-01 的 Web 设置：Hermes 权限上限 edit，默认 harness 为 Hermes。
func (l *live) configure() {
	l.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"read","by_harness":{"hermes":"edit","hermes-noresume":"edit"}}}`, 200)
	// 决策走渠道 acc-chan（CHAN 领取并回报送达），其余只进 Web 未读。
	l.webOK("PUT", "/settings/policy", `{"default_harness":"hermes","disturb":{"routes":{"brief":"web","decision":"acc-chan","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false}}`, 200)
}

// sessionOf 是内核记在事项上的该运行的 Hermes 会话 id：运行结束时按 stdout 末段摘要写进 run.finished 事件
// （apa why 以 DEL 读取）。
func (l *live) sessionOf(item, runID string) string {
	l.t.Helper()
	var out struct {
		Steps []struct {
			Type  string `json:"type"`
			Actor struct {
				Run string `json:"run"`
			} `json:"actor"`
			Payload struct {
				Session string `json:"session_id"`
			} `json:"payload"`
		} `json:"steps"`
	}
	must(l.t, json.Unmarshal([]byte(l.apa(l.DEL, 0, "why", item).Stdout), &out))
	for _, s := range out.Steps {
		if s.Type == "run.finished" && s.Actor.Run == runID {
			return s.Payload.Session
		}
	}
	return ""
}

// transcript 导出 Hermes 会话记录（隔离家目录里的 state.db）。
func (l *live) transcript(session string) string {
	l.t.Helper()
	dst := filepath.Join(l.Root, "transcript-"+session)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.hbin, "sessions", "export", "--format", "jsonl", "--session-id", session, "--yes", dst)
	cmd.Env = append(os.Environ(), "HERMES_HOME="+l.hhome)
	if out, err := cmd.CombinedOutput(); err != nil {
		l.t.Fatalf("hermes sessions export: %v\n%s", err, out)
	}
	// 导出结果是目录（每个会话一个文件）。
	var all strings.Builder
	must(l.t, filepath.WalkDir(dst, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		all.Write(b)
		return err
	}))
	return all.String()
}

type askView struct {
	Question   string `json:"question"`
	ResumeNote string `json:"resume_note"`
	Options    []struct {
		ID    string `json:"id"`
		Label string `json:"label"`
	} `json:"options"`
}

func (l *live) askOf(id string) askView {
	l.t.Helper()
	var a askView
	if raw, ok := l.show(id)["ask"]; ok {
		b, _ := json.Marshal(raw)
		must(l.t, json.Unmarshal(b, &a))
	}
	return a
}

// childOf 找父事项上委派给 work-host.cli 的子事项。
func (l *live) childOf(parent string) string {
	l.t.Helper()
	for _, c := range l.children(parent) {
		var out struct {
			Relations []struct{ From, To, Type string } `json:"relations"`
		}
		must(l.t, json.Unmarshal([]byte(l.apa(l.OBS, 0, "item", "show", c).Stdout), &out))
		for _, r := range out.Relations {
			if r.Type == "parent" && r.To == parent && r.From == c {
				return c
			}
		}
	}
	return ""
}

// runsByCategory 列出全部后台运行。
func (l *live) runsByCategory() []run {
	l.t.Helper()
	var out struct {
		Runs []run `json:"runs"`
	}
	must(l.t, json.Unmarshal([]byte(l.apa(l.OBS, 0, "run", "list", "--category", "background").Stdout), &out))
	return out.Runs
}

func hasType(evs []event, typ string) bool {
	for _, e := range evs {
		if e.Type == typ {
			return true
		}
	}
	return false
}

const (
	trivialBody = "这是一个只需确认的事项。不要调用任何 apa 命令，不要委派，不要提交决策请求，直接回复一句“已看到”后结束。"
	delegBody   = "线上有个 bug，修复需要改代码。请把修复交给执行者 work-host.cli：先用 apa executors 确认，再 apa delegate，并在 harness 内调用该执行者。委派并调用执行者后，不要评估它的输出（执行者之后会自己回执，这里只是登记调用），不要对子事项 apa report，不要提交决策请求，不要自己改代码，直接结束。"
)

// TestFlowBgMainSession 对应验收文档 flow-bg-main-session（ACT-BGM-01..09，真实 Hermes）。
func TestFlowBgMainSession(t *testing.T) {
	l := setup(t)
	var observed, parent, child string
	var first run

	l.step("ACT-BGM-01 准备规则和权限上限", func(t *testing.T) {
		l.configure()
		var out struct {
			Rules []struct{ Key, Mode string } `json:"rules"`
		}
		must(t, json.Unmarshal([]byte(l.webOK("GET", "/rules", "", 200)), &out))
		got := map[string]string{}
		for _, r := range out.Rules {
			got[r.Key] = r.Mode
		}
		eq(t, "handle-labelled 初始模式", got[packRule+"handle-labelled"], "observe")
		eq(t, "live-bg 规则初始模式", got[liveRule+"noresume"], "observe")
	})

	l.step("ACT-BGM-02 observe 只记录不拉起", func(t *testing.T) {
		observed = l.openItem("bgm-02 observe", trivialBody, "agent-ok")
		l.waitFor("记下 rule.observed", 30*time.Second, func() bool { return hasType(l.events(observed), "rule.observed") })
		if n := len(l.runs(observed)); n != 0 {
			t.Errorf("observe 不应有 Run，实际 %d", n)
		}
		why := l.apa(l.DEL, 0, "why", observed).Stdout
		if !strings.Contains(why, "rule.observed") {
			t.Errorf("apa why 应包含“本来会做什么”的记录: %s", why)
		}
	})

	l.step("ACT-BGM-03 切到 auto 后拉起运行", func(t *testing.T) {
		l.setMode(packRule+"handle-labelled", "auto")
		parent = l.openItem("bgm-03 修复交给执行者", delegBody, "agent-ok")
		l.waitFor("Run 建立", 30*time.Second, func() bool { return len(l.runs(parent)) == 1 })
		first = l.runs(parent)[0]
		eq(t, "category", first.Category, "background")
		eq(t, "runner", first.Runner, "main_session")
		eq(t, "launch", first.Launch, "headless")
		eq(t, "harness", first.Harness, "hermes")
		eq(t, "access", first.Access, "edit")
		eq(t, "规则 id", first.Basis.RuleID, packRule+"handle-labelled")
		if first.Basis.PolicyVersion == 0 {
			t.Errorf("Run 应记策略版本: %+v", first.Basis)
		}
		if a, _ := l.show(parent)["assignee"].(string); a != "" {
			t.Errorf("运行者不占事项的 assignee，实际 %q", a)
		}
	})

	l.step("ACT-BGM-04 运行者读上下文并委派", func(t *testing.T) {
		first = l.settled(parent, 1)[0]
		child = l.childOf(parent)
		if child == "" {
			t.Fatalf("没有委派给 work-host.cli 的子事项；运行 %+v", first)
		}
		c := l.show(child)
		eq(t, "子事项 assignee", c["assignee"], "work-host.cli")
		eq(t, "子事项 unattended", c["unattended"], true)
		sess := l.sessionOf(parent, first.ID)
		if sess == "" {
			t.Fatalf("账本里没有该运行的 Hermes 会话 id")
		}
		tr := l.transcript(sess)
		for _, want := range []string{"apa context", "apa delegate"} {
			if !strings.Contains(tr, want) && !strings.Contains(tr, strings.ReplaceAll(want, "apa ", "apa_")) {
				t.Errorf("Hermes 会话记录里没有 %q", want)
			}
		}
		if l.runFile(first.ID, "args.txt") == "" {
			t.Errorf("无头进程应由 APA 所在主机的外呼调用器拉起（包装脚本留下的 args.txt 缺失）")
		}
	})

	l.step("ACT-BGM-05 运行结束，父事项等待", func(t *testing.T) {
		eq(t, "Run state", first.State, "succeeded")
		eq(t, "父事项阶段", l.phase(parent), "waiting")
		if l.sessionOf(parent, first.ID) == "" {
			t.Errorf("事项应记录会话 id")
		}
		tok := l.runFile(first.ID, "token")
		if tok == "" {
			t.Fatal("包装脚本没有留下运行凭据")
		}
		eq(t, "用已失效的运行凭据调 apa inbox 的退出码", l.Apa(tok, "", "inbox").Code, 3)
	})

	l.step("ACT-BGM-06 回执后下一次运行接续", func(t *testing.T) {
		l.apa(l.WRK, 0, "report", child, "--outcome", "done", "--summary", "修复完成", "--pr", "https://example.invalid/acc/test-repo/pull/7")
		eq(t, "子事项阶段", l.phase(child), "done")
		l.waitFor("规则再次命中，建新 Run", 60*time.Second, func() bool { return len(l.runs(parent)) >= 2 })
		var next run
		for _, r := range l.runs(parent) {
			if r.ID != first.ID {
				next = r
			}
		}
		if next.ChainDepth <= first.ChainDepth {
			t.Errorf("新 Run 的 chain_depth = %d，应大于 %d", next.ChainDepth, first.ChainDepth)
		}
		l.settled(parent, 2)
	})

	l.step("ACT-BGM-07 全程回放", func(t *testing.T) {
		why := l.apa(l.DEL, 0, "why", parent).Stdout
		for _, want := range []string{"item.phase_changed", "background.launched", packRule + "handle-labelled", "run.finished", "waiting on work-host.cli", "all delegated items finished"} {
			if !strings.Contains(why, want) {
				t.Errorf("apa why 缺少 %q", want)
			}
		}
	})

	l.step("ACT-BGM-08 运行结束时仍是 open 交本人", func(t *testing.T) {
		id := l.openItem("bgm-08 不回执", trivialBody, "agent-ok")
		rs := l.settled(id, 1)
		eq(t, "Run 数", len(rs), 1)
		eq(t, "事项阶段", l.phase(id), "needs_input")
		a := l.askOf(id)
		if len(a.Options) < 3 {
			t.Errorf("应给出重新运行、改派、放弃三个选项: %+v", a)
		}
		time.Sleep(20 * time.Second)
		eq(t, "不自动再次拉起，Run 数", len(l.runs(id)), 1)
		eq(t, "默认 harness", rs[0].Harness, "hermes")
	})

	l.step("ACT-BGM-09 指定 harness", func(t *testing.T) {
		l.setMode(liveRule+"noresume", "auto")
		id := l.openItem("bgm-09 指定 harness", trivialBody, "live-noresume")
		rs := l.settled(id, 1)
		eq(t, "规则声明的 harness", rs[0].Harness, "hermes-noresume")
	})
}
