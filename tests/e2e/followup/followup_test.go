//go:build e2e

package followup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const ruleKey = "follow-up%2Fclock_rules%2Ffollow-up-interval"

type fx struct {
	*baseline.Env
	t *testing.T
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func id(t *testing.T, r baseline.Result) string {
	t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil || m.ID == "" {
		t.Fatalf("没有返回 id: %q %v", r.Stdout, err)
	}
	return m.ID
}

// setupShort 对应 ACT-FUP-01：把内置 follow-up 包覆盖到 $APA_HOME/packs/follow-up，间隔改为 1 分钟，
// 重启内核让覆盖生效，再在 Web 把规则切到 auto，并核对 GET /v1/rules 与覆盖来源。
func setupShort(t *testing.T) *fx {
	f := &fx{baseline.Setup(t), t}
	dst := filepath.Join(f.Home, "packs", "follow-up")
	if err := os.CopyFS(dst, os.DirFS("../../../packs/follow-up")); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dst, "pack.yaml")
	b, err := os.ReadFile(p)
	if err != nil || !strings.Contains(string(b), "schedule: 72h") {
		t.Fatalf("内置包没有 schedule: 72h: %v", err)
	}
	if err := os.WriteFile(p, []byte(strings.Replace(string(b), "schedule: 72h", "schedule: 1m", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	f.StopKernel()
	f.StartKernelAgain()
	if body := f.webOK("GET", "/packs", "", 200); !strings.Contains(body, `"name":"follow-up"`) {
		t.Fatalf("follow-up 包不可见: %s", body)
	}
	f.webOK("PATCH", "/rules/"+ruleKey+"/mode", `{"mode":"auto"}`, 200)
	rules := f.webOK("GET", "/rules", "", 200)
	var out struct {
		Rules []struct{ Key, Mode string }
	}
	if err := json.Unmarshal([]byte(rules), &out); err != nil {
		t.Fatal(err)
	}
	ok := false
	for _, r := range out.Rules {
		if r.Key == "follow-up/clock_rules/follow-up-interval" && r.Mode == "auto" {
			ok = true
		}
	}
	if !ok {
		t.Fatalf("规则不是 auto: %s", rules)
	}
	// 提醒走 Web 未读：验收文档允许"Hermes 注入或 Web 未读"，这里取 Web 未读。
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false}}`, 200)
	return f
}

// waiting 建一个事项并转 waiting（等外部）。
func (f *fx) waiting(title string) string {
	f.t.Helper()
	i := id(f.t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建个任务"))
	f.apa(f.DEL, 0, "item", "transition", i, "--to", "waiting", "--waiting-on", "external:装修公司报价", "--reason", "等报价", "--evidence", "等装修公司报价")
	return i
}

func (f *fx) judge(item, choice string) {
	b, _ := json.Marshal(map[string]any{"choice": choice, "confidence": 0.9})
	f.ModelStub.When(item, modelstub.Reply{Content: string(b)})
}

func (f *fx) show(item string) map[string]any {
	f.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", item).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Item
}

func (f *fx) phase(item string) string { s, _ := f.show(item)["phase"].(string); return s }

func (f *fx) unread() string { return f.webOK("GET", "/deliveries/unread", "", 200) }

func (f *fx) events(item, typ string) int {
	f.t.Helper()
	var out struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/audit/events?limit=500&object="+item+"&type="+typ, "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	return len(out.Events)
}

func (f *fx) until(d time.Duration, cond func() bool) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Second)
	}
	return cond()
}

// TestFlowFollowUp 对应 flow-follow-up 的 ACT-FUP-01..05：四个事项共用一个 2 分钟等待窗口。
func TestFlowFollowUp(t *testing.T) {
	t.Parallel()
	f := setupShort(t) // ACT-FUP-01
	f.StartModelStub()

	remind := f.waiting("fup-remind")
	wait := f.waiting("fup-wait")
	drop := f.waiting("fup-drop")
	parent := f.waiting("fup-parent")
	// ACT-FUP-05：委派给执行者，父事项 waiting，执行者不回执。
	child := id(t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", "fup-child", "--reason", "委派", "--evidence", "owner asked"))
	f.judge(remind, "remind")
	f.judge(wait, "wait")
	f.judge(drop, "abandon")
	f.judge(parent, "remind")

	hasUnread := func(item string) bool { return strings.Contains(f.unread(), item) }
	// 在 2 分钟内等所有判断落地：remind/parent 出提醒，wait 记延长，drop 转 needs_input。
	f.until(2*time.Minute+20*time.Second, func() bool {
		return hasUnread(remind) && hasUnread(parent) && f.events(wait, "followup.extended") > 0 && f.phase(drop) == "needs_input"
	})

	t.Run("ACT-FUP-02 判断催时产出提醒", func(t *testing.T) {
		if !hasUnread(remind) {
			t.Fatalf("Web 未读没有提醒: %s", f.unread())
		}
		if got := f.phase(remind); got != "waiting" {
			t.Errorf("事项阶段 = %s，期望 waiting", got)
		}
	})

	t.Run("ACT-FUP-03 判断再等时延长且无提醒", func(t *testing.T) {
		if f.events(wait, "followup.extended") == 0 {
			t.Error("没有延长记录")
		}
		if got := f.phase(wait); got != "waiting" {
			t.Errorf("事项阶段 = %s，期望 waiting", got)
		}
		if hasUnread(wait) || f.events(wait, "followup.reminded") != 0 {
			t.Error("再等不应产生提醒")
		}
	})

	t.Run("ACT-FUP-04 建议放弃时交本人", func(t *testing.T) {
		if got := f.phase(drop); got != "needs_input" {
			t.Fatalf("事项阶段 = %s，期望 needs_input（不得静默 dropped）", got)
		}
		if !strings.Contains(f.apa(f.OBS, 0, "inbox").Stdout, drop) {
			t.Error("决策待办里没有该事项")
		}
	})

	t.Run("ACT-FUP-05 委派不回执时跟进", func(t *testing.T) {
		if !hasUnread(parent) {
			t.Fatalf("没有催促提醒: %s", f.unread())
		}
		if got := f.phase(parent); got != "waiting" {
			t.Errorf("父事项阶段 = %s，期望 waiting", got)
		}
		if got := f.phase(child); got != "open" {
			t.Errorf("子事项被改动: %s", got)
		}
	})

	t.Run("规则非 auto 时不外呼不提醒", func(t *testing.T) {
		f.webOK("PATCH", "/rules/"+ruleKey+"/mode", `{"mode":"observe"}`, 200)
		before := len(f.ModelStub.Requests())
		quiet := f.waiting("fup-observe")
		f.judge(quiet, "remind")
		time.Sleep(100 * time.Second)
		if n := len(f.ModelStub.Requests()); n != before {
			t.Errorf("observe 下仍外呼模型: %d -> %d", before, n)
		}
		if hasUnread(quiet) {
			t.Error("observe 下不应产生提醒")
		}
	})
}

// TestFlowFollowUpModelDown 对应 ACT-FUP-06：模型 500 时跟进不丢，健康视图有信号，恢复后得到判断结果。
func TestFlowFollowUpModelDown(t *testing.T) {
	t.Parallel()
	f := setupShort(t)
	f.StartModelStub()
	f.ModelStub.SetDefault(modelstub.Reply{Status: 500})
	item := f.waiting("fup-down")

	if !f.until(2*time.Minute+20*time.Second, func() bool { return len(f.ModelStub.Requests()) > 0 }) {
		t.Fatal("跟进到期后没有调用模型")
	}
	if !f.until(90*time.Second, func() bool { return len(f.ModelStub.Requests()) > 1 }) {
		t.Error("模型不可用时没有退避重试")
	}
	if got := f.phase(item); got != "waiting" {
		t.Errorf("事项阶段 = %s，期望 waiting（不丢）", got)
	}
	if strings.Contains(f.unread(), item) {
		t.Error("模型不可用时不应有提醒")
	}
	// 健康信号：跟进失败记 health.alert（健康类投递由触达统一产出，走 Web 未读）。
	if !f.until(30*time.Second, func() bool { return f.events(item, "health.alert") > 0 }) {
		t.Error("模型不可用时没有 health.alert 信号")
	}

	f.ModelStub.SetDefault(modelstub.Reply{Content: `{"choice":"remind","confidence":0.9}`})
	if !f.until(3*time.Minute, func() bool { return strings.Contains(f.unread(), item) }) {
		t.Fatalf("恢复后没有得到判断结果（提醒）: %s", f.unread())
	}
}
