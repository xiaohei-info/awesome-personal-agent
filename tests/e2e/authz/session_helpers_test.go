//go:build e2e

package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const exitBasis = 23 // 替本人做决定的依据不足（§14.5）

// run 以完全受控的环境运行 apa：继承进程环境里除 APA_* 以外的部分，只带 APA_HOME、APA_URL、APA_TOKEN 和 extra。
// baseline.Apa 会在带 --evidence 的命令上自动补 direct 会话；验收要逐个会话类型（group、unknown、缺失）执行，所以不用它。
func (k *kit) run(token string, extra []string, args ...string) baseline.Result {
	k.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.Bin["apa"], args...)
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "APA_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "APA_HOME="+k.Home, "APA_URL="+k.URL)
	if token != "" {
		cmd.Env = append(cmd.Env, "APA_TOKEN="+token)
	}
	cmd.Env = append(cmd.Env, extra...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	r := baseline.Result{}
	err := cmd.Run()
	r.Stdout, r.Stderr = out.String(), errb.String()
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		k.t.Fatalf("运行 apa %v: %v", args, err)
	}
	return r
}

// at 是插件（STUB_HARNESS 扮演）带的会话身份：harness、会话引用、会话类型；kind 为空表示不上报会话类型。
func at(session, kind string) []string {
	e := []string{"APA_HARNESS=acc-stub"}
	if session != "" {
		e = append(e, "APA_SESSION="+session)
	}
	if kind != "" {
		e = append(e, "APA_SESSION_KIND="+kind)
	}
	return e
}

// decision 以 DEL 建一个 work-host.cli 的事项，再由 WRK 提出带 A、B 两个选项的普通决策请求，返回事项 id。
// question 同时写进标题，便于检查用户内容不会被带进错误信息。
func (k *kit) decision(project, question string, tags ...string) string {
	k.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", question, "--phase", "open", "--assignee", workHost,
		"--project", k.Projects[project], "--evidence", "e2e setup"}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	id := k.idOf("item add", k.Apa(k.DEL, "", args...))
	k.expect("ask", k.Apa(k.WRK, "", "ask", id, "--question", question, "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text"), 0)
	k.settle(id)
	return id
}

// settle 等 web_only 初筛返回：初筛未返回时 delegate 的代答按退出码 22 不接受（§14.4），验收动作都在初筛结束后执行。
func (k *kit) settle(id string) {
	k.t.Helper()
	k.waitFor("决策 "+id+" 的 web_only 初筛返回", 30*time.Second, func() bool { return k.ask(id)["screening_pending"] != true })
}

// ownItem 在 Web 以本人身份新建 open 事项（assignee 为空，即本人的事项）。
func (k *kit) ownItem(project, title string) string {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{"kind": "task", "title": title, "phase": "open", "project": k.Projects[project]})
	var o struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(k.webOK("POST", "/v1/items", string(body), 201)), &o); err != nil || o.ID == "" {
		k.t.Fatalf("Web 新建事项: %v", err)
	}
	return o.ID
}

func (k *kit) phase(id string) string { s, _ := k.item(id)["phase"].(string); return s }

func (k *kit) ask(id string) map[string]any { a, _ := k.item(id)["ask"].(map[string]any); return a }

// resolution 是决策的回答结果；未回答为 nil。
func (k *kit) resolution(id string) map[string]any {
	r, _ := k.ask(id)["resolution"].(map[string]any)
	return r
}

// lastRejection 是决策待办/建议流上标明的最近一次被拒绝的处理（GET /v1/items/{id} 的 last_rejection）。
func (k *kit) lastRejection(id string) map[string]any {
	k.t.Helper()
	var o struct {
		LastRejection map[string]any `json:"last_rejection"`
	}
	if err := json.Unmarshal([]byte(k.webOK("GET", "/v1/items/"+id, "", 200)), &o); err != nil {
		k.t.Fatal(err)
	}
	return o.LastRejection
}

// rejected 断言 r 是退出码 23 的 session_not_direct，并返回错误体。
func (k *kit) rejectedNotDirect(label string, r baseline.Result) map[string]any {
	k.t.Helper()
	k.expect(label, r, exitBasis)
	b := k.errBody(r)
	if b["code"] != "session_not_direct" {
		k.t.Fatalf("%s: code=%v，期望 session_not_direct: %s", label, b["code"], r.Stderr)
	}
	return b
}

func (k *kit) delID() string {
	k.t.Helper()
	var out struct {
		Credentials []struct{ ID, Label string } `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(k.webOK("GET", "/v1/settings/tokens", "", 200)), &out); err != nil {
		k.t.Fatal(err)
	}
	for _, c := range out.Credentials {
		if c.Label == "DEL" {
			return c.ID
		}
	}
	k.t.Fatal("找不到 DEL 凭据")
	return ""
}

// grant 由本人在 Web 为 DEL 签发授权单：actions、范围（项目名）、有效期 1 天。
func (k *kit) grant(actions []string, projects ...string) string {
	k.t.Helper()
	var ids []string
	for _, p := range projects {
		ids = append(ids, k.Projects[p])
	}
	body, _ := json.Marshal(map[string]any{"grantee": map[string]any{"credential": k.delID()}, "scope": map[string]any{"projects": ids},
		"actions": actions, "expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339), "reason": "e2e"})
	var g struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(k.webOK("POST", "/v1/settings/grants", string(body), 201)), &g); err != nil || g.ID == "" {
		k.t.Fatalf("签发授权单: %v", err)
	}
	return g.ID
}

// auditOf 是对象上某类型的审计 Event（Web 审计页与"agent 变更"视图的数据源）。
func (k *kit) auditOf(object, typ string) []Event {
	k.t.Helper()
	q := url.Values{"object": {object}}
	if typ != "" {
		q.Set("type", typ)
	}
	return k.audit(q)
}

// agentChanges 是"agent 变更"视图的口径：actor.type=agent 且不是被拒绝的命令。
func (k *kit) agentChanges(object string) []Event {
	var out []Event
	for _, e := range k.auditOf(object, "") {
		if e.Actor["type"] == "agent" && e.Type != "command.rejected" {
			out = append(out, e)
		}
	}
	return out
}

func (k *kit) lastEvent(object, typ string) Event {
	k.t.Helper()
	es := k.auditOf(object, typ)
	if len(es) == 0 {
		k.t.Fatalf("%s 上没有 %s 事件", object, typ)
	}
	return es[len(es)-1]
}

func notContains(t *testing.T, what, s, bad string) {
	t.Helper()
	if strings.Contains(s, bad) {
		t.Errorf("%s 不应含 %q: %s", what, bad, s)
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}
