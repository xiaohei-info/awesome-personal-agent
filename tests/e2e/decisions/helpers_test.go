//go:build e2e

package decisions

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

type fx struct {
	*baseline.Env
	t *testing.T
}

func setup(t *testing.T) *fx { t.Helper(); return &fx{baseline.Setup(t), t} }

// apa 运行命令并断言退出码。
func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) idOf(r baseline.Result) string {
	f.t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %q %v", r.Stdout, err)
	}
	return m.ID
}

// task 用 DEL 代本人建一个 open 的任务事项（assignee 为 work-host.cli）。
func (f *fx) task(title, project string, tags ...string) string {
	f.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects[project], "--evidence", "建个任务"}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	return f.idOf(f.apa(f.DEL, 0, args...))
}

// proposal 用 OBS 新建 proposed 建议。
func (f *fx) proposal(title string, tags ...string) string {
	f.t.Helper()
	args := []string{"propose", "--kind", "task", "--title", title, "--project", f.Projects["acc-open"]}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	return f.idOf(f.apa(f.OBS, 0, args...))
}

// ask 用 WRK 为 work-host.cli 的事项写入决策请求。
func (f *fx) ask(id string, extra ...string) {
	f.t.Helper()
	args := append([]string{"ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text"}, extra...)
	f.apa(f.WRK, 0, args...)
	f.settle(id)
}

// settle 等 web_only 初筛返回（ask.screening_pending 清除）：初筛是提交后异步进行，未返回时 delegate 代答按退出码 22 不接受
// （设计 §9、§14.4），慢机或并发负载下不能指望它先于 decide 完成。
func (f *fx) settle(id string) {
	f.t.Helper()
	f.waitFor(30*time.Second, "决策 "+id+" 的初筛返回", func() bool {
		a, _ := f.item(id)["ask"].(map[string]any)
		return a["screening_pending"] != true
	})
}

// item 读事项（item 对象）。
func (f *fx) item(id string) map[string]any {
	f.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Item
}

func (f *fx) phase(id string) string { s, _ := f.item(id)["phase"].(string); return s }

func (f *fx) resolution(id string) map[string]any {
	a, _ := f.item(id)["ask"].(map[string]any)
	r, _ := a["resolution"].(map[string]any)
	return r
}

// webOK 以本人 Web 会话请求并断言状态码。
func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

type step struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Actor   actor          `json:"actor"`
	Reason  string         `json:"reason"`
	Ev      string         `json:"evidence"`
	Payload map[string]any `json:"payload"`
}

type actor struct {
	Type       string `json:"type"`
	Principal  string `json:"principal"`
	Via        string `json:"via"`
	OnBehalfOf string `json:"on_behalf_of"`
}

// events 取某对象（空表示全部）某类型（空表示全部）的审计事件，即 Web 审计页与"agent 变更"视图的数据源。
func (f *fx) events(object, typ string) []step {
	f.t.Helper()
	var out struct {
		Events []step `json:"events"`
	}
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + object
	}
	if typ != "" {
		path += "&type=" + typ
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", path, "", http.StatusOK)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

// last 返回对象上最后一条指定类型事件。
func (f *fx) last(object, typ string) step {
	f.t.Helper()
	es := f.events(object, typ)
	if len(es) == 0 {
		f.t.Fatalf("%s 上没有 %s 事件", object, typ)
	}
	return es[len(es)-1]
}

// agentChanges 是"agent 变更"视图的过滤口径（与 AgentChangesPage 一致）：actor.type=agent 且不是被拒绝的命令。
func (f *fx) agentChanges(object string) []step {
	var out []step
	for _, s := range f.events(object, "") {
		if s.Actor.Type == "agent" && s.Type != "command.rejected" {
			out = append(out, s)
		}
	}
	return out
}

func (f *fx) unread() int {
	f.t.Helper()
	var out struct {
		Deliveries []json.RawMessage `json:"deliveries"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/deliveries/unread", "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	return len(out.Deliveries)
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// riskClass 是内核对决策判定的风险类别（ask.risk_class，调用方不可写）。
func (f *fx) riskClass(id string) any {
	a, _ := f.item(id)["ask"].(map[string]any)
	return a["risk_class"]
}
