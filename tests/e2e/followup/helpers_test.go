//go:build e2e

package followup

import (
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// fx 是一套验收环境加上本包常用的断言助手。Web 步骤以本人会话请求 Web 页面调用的同一组 /v1 接口，页面渲染由 web/ 组件测试覆盖。
type fx struct {
	*baseline.Env
	t *testing.T
}

func newFx(t *testing.T) *fx { t.Helper(); return &fx{baseline.Setup(t), t} }

// apa 运行 CLI 并断言退出码。
func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// webOK 以本人 Web 会话请求并断言状态码，path 不含 /v1。
func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
	return v
}

func step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	if !t.Run(name, fn) {
		t.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

// waitFor 轮询直到 cond 为真，超时即失败。
func waitFor(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
		if time.Now().After(end) {
			t.Fatalf("%s（等待 %s 仍未出现）", what, d)
		}
	}
}

// quiet 在 d 内反复检查 cond 一直为真（用来证明"没有发生某事"）。
func quiet(t *testing.T, what string, d time.Duration, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		if !cond() {
			t.Fatalf("%s", what)
		}
		if time.Now().After(end) {
			return
		}
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// ---- 事项 ----

type item struct {
	ID       string           `json:"id"`
	Phase    string           `json:"phase"`
	Tags     []string         `json:"tags"`
	Progress []map[string]any `json:"progress"`
	Planned  string           `json:"planned_for"`
}

func (i item) has(tag string) bool {
	for _, t := range i.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

func (f *fx) show(id string) item {
	f.t.Helper()
	out := decode[struct {
		Item     item             `json:"item"`
		Progress []map[string]any `json:"progress"`
	}](f.t, f.apa(f.OBS, 0, "item", "show", id).Stdout)
	out.Item.Progress = out.Progress
	return out.Item
}

// newItem 用 DEL 建事项并返回 id。
func (f *fx) newItem(title string, extra ...string) string {
	f.t.Helper()
	args := append([]string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "验收建事项"}, extra...)
	return decode[struct {
		ID string `json:"id"`
	}](f.t, f.apa(f.DEL, 0, args...).Stdout).ID
}

// wait 把事项转 waiting（等外部），token 为空取 WRK。
func (f *fx) wait(id string) {
	f.t.Helper()
	f.apa(f.WRK, 0, "item", "transition", id, "--to", "waiting", "--waiting-on", "external:装修公司报价", "--reason", "等报价", "--evidence", "等装修公司报价")
}

// ---- 规则 ----

type ruleState struct {
	Key  string `json:"key"`
	ID   string `json:"id"`
	Mode string `json:"mode"`
}

func rulesOn(t *testing.T, e *baseline.Env, token string) map[string]ruleState {
	t.Helper()
	r := e.APIRaw("GET", "/v1/rules", "", token)
	if r.Code != 200 {
		t.Fatalf("GET /v1/rules: %d %s", r.Code, r.Body)
	}
	m := map[string]ruleState{}
	for _, s := range decode[struct {
		Rules []ruleState `json:"rules"`
	}](t, r.Body).Rules {
		m[s.ID] = s
	}
	return m
}

func (f *fx) rules() map[string]ruleState { f.t.Helper(); return rulesOn(f.t, f.Env, f.OBS) }

// modePath 规则键含 "/"，放进路径要转义。
func modePath(key string) string { return "/rules/" + url.PathEscape(key) + "/mode" }

// ---- 审计与投递 ----

type auditEvent struct {
	Seq    int64                                 `json:"seq"`
	Type   string                                `json:"type"`
	Object string                                `json:"object"`
	Actor  struct{ Type, Principal, Via string } `json:"actor"`
}

// events 读 Web 审计页的数据源；object、typ 为空则不筛。
func (f *fx) events(object, typ string) []auditEvent {
	f.t.Helper()
	return eventsOn(f.t, f.Env, object, typ)
}

func eventsOn(t *testing.T, e *baseline.Env, object, typ string) []auditEvent {
	t.Helper()
	q := "?limit=500"
	if object != "" {
		q += "&object=" + url.QueryEscape(object)
	}
	if typ != "" {
		q += "&type=" + url.QueryEscape(typ)
	}
	r := e.Web("GET", "/v1/audit/events"+q, "")
	if r.Code != 200 {
		t.Fatalf("审计页: %d %s", r.Code, r.Body)
	}
	return decode[struct {
		Events []auditEvent `json:"events"`
	}](t, r.Body).Events
}

type delivery struct {
	ID      string `json:"id"`
	Item    string `json:"item"`
	Class   string `json:"class"`
	Content struct {
		Title   string   `json:"title"`
		Summary string   `json:"summary"`
		Options []string `json:"options"`
	} `json:"content"`
}

func unreadOn(t *testing.T, e *baseline.Env) []delivery {
	t.Helper()
	r := e.Web("GET", "/v1/deliveries/unread", "")
	if r.Code != 200 {
		t.Fatalf("GET /v1/deliveries/unread: %d %s", r.Code, r.Body)
	}
	return decode[struct {
		Deliveries []delivery `json:"deliveries"`
	}](t, r.Body).Deliveries
}

func (f *fx) unread() []delivery { f.t.Helper(); return unreadOn(f.t, f.Env) }

// reminded 是事项已产出的跟进提醒轮数（审计里的 followup.reminded，每轮恰一条）。
func (f *fx) reminded(id string) int { f.t.Helper(); return len(f.events(id, "followup.reminded")) }

// followupDeliveries 是 Web 未读里指向该事项、带三个动作的跟进提醒。
func (f *fx) followupDeliveries(id string) (out []delivery) {
	for _, d := range f.unread() {
		if d.Item == id && d.Class == "reminder" && strings.HasPrefix(d.Content.Title, "该跟进了") {
			out = append(out, d)
		}
	}
	return out
}
