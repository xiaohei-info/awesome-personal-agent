//go:build e2e

// Package ledger 是验收文档四个全流程的端到端测试：flow-item-lifecycle、flow-clock-due、flow-why-audit、
// flow-agent-revert。全部走真实 apa 二进制与真实内核进程（环境来自 tests/e2e/baseline），
// CLI 步骤直接运行 apa；Web 步骤以登录后的本人会话调用 Web 页面使用的同一组 HTTP API。
package ledger

import (
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

type step struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Actor  struct {
		Type       string `json:"type"`
		Principal  string `json:"principal"`
		Via        string `json:"via"`
		OnBehalfOf string `json:"on_behalf_of"`
	} `json:"actor"`
	Reason   string         `json:"reason"`
	Evidence string         `json:"evidence"`
	Payload  map[string]any `json:"payload"`
	Redacted bool           `json:"redacted"`
}

func parse(t *testing.T, s string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		t.Fatalf("不是 JSON 对象: %v\n%s", err, s)
	}
	return m
}

// ok 断言命令退出码 0，返回标准输出里的 JSON。
func ok(t *testing.T, r baseline.Result, what string) map[string]any {
	t.Helper()
	if r.Code != 0 {
		t.Fatalf("%s: 退出码 %d，stderr=%s", what, r.Code, r.Stderr)
	}
	return parse(t, r.Stdout)
}

// fail 断言命令以 wantCode 失败，且错误体带 code、why、next（附录 B.3）。
func fail(t *testing.T, r baseline.Result, wantCode int, what string) map[string]any {
	t.Helper()
	if r.Code != wantCode {
		t.Fatalf("%s: 退出码 %d（期望 %d），stdout=%s stderr=%s", what, r.Code, wantCode, r.Stdout, r.Stderr)
	}
	body := parse(t, r.Stderr)
	for _, k := range []string{"code", "why", "next"} {
		if s, _ := body[k].(string); s == "" {
			t.Errorf("%s: 错误体缺少 %s: %s", what, k, r.Stderr)
		}
	}
	return body
}

func idOf(t *testing.T, r baseline.Result, what string) string {
	t.Helper()
	id, _ := ok(t, r, what)["id"].(string)
	if id == "" {
		t.Fatalf("%s: 没有返回 id: %s", what, r.Stdout)
	}
	return id
}

// addTask 以 DEL 新建 open 的 task（附本人原话作证据）。
func addTask(t *testing.T, e *baseline.Env, title string, extra ...string) string {
	t.Helper()
	args := append([]string{"item", "add", "--kind", "task", "--phase", "open", "--title", title, "--evidence", "owner: add " + title}, extra...)
	return idOf(t, e.Apa(e.DEL, "", args...), "item add "+title)
}

// show 用 OBS 读事项，返回 `apa item show` 里的 item 对象。
func show(t *testing.T, e *baseline.Env, id string) map[string]any {
	t.Helper()
	it, _ := ok(t, e.Apa(e.OBS, "", "item", "show", id), "item show "+id)["item"].(map[string]any)
	if it == nil {
		t.Fatalf("item show %s 没有 item", id)
	}
	return it
}

func version(it map[string]any) int { v, _ := it["version"].(float64); return int(v) }

// why 以 token 运行 apa why，返回原始输出与解析后的步骤。
func why(t *testing.T, e *baseline.Env, token, id string, extra ...string) (string, []step) {
	t.Helper()
	r := e.Apa(token, "", append([]string{"why", id}, extra...)...)
	if r.Code != 0 {
		t.Fatalf("apa why %s: 退出码 %d %s", id, r.Code, r.Stderr)
	}
	var tl struct {
		Steps []step `json:"steps"`
	}
	if len(extra) == 0 {
		if err := json.Unmarshal([]byte(r.Stdout), &tl); err != nil {
			t.Fatalf("apa why 输出不是时间线: %v\n%s", err, r.Stdout)
		}
	}
	return r.Stdout, tl.Steps
}

// webSteps 是 Web 事项详情时间线读的接口（GET /v1/why/{id}）。
func webSteps(t *testing.T, e *baseline.Env, id string) []step {
	t.Helper()
	r := e.Web("GET", "/v1/why/"+id, "")
	if r.Code != 200 {
		t.Fatalf("Web 时间线 %s: %d %s", id, r.Code, r.Body)
	}
	var tl struct {
		Steps []step `json:"steps"`
	}
	if err := json.Unmarshal([]byte(r.Body), &tl); err != nil {
		t.Fatal(err)
	}
	return tl.Steps
}

// audit 以 Web 会话读审计事件（Web 审计页与 agent 变更视图用的接口）。
func audit(t *testing.T, e *baseline.Env, query string) []step {
	t.Helper()
	r := e.Web("GET", "/v1/audit/events?"+query, "")
	if r.Code != 200 {
		t.Fatalf("审计查询 %s: %d %s", query, r.Code, r.Body)
	}
	var out struct {
		Events []step `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		t.Fatal(err)
	}
	return out.Events
}

func ofType(ss []step, typ string) (out []step) {
	for _, s := range ss {
		if s.Type == typ {
			out = append(out, s)
		}
	}
	return
}

// req 以 Bearer 凭据发一次带任意头的请求（baseline.APIRaw 只带 Authorization）。
func req(t *testing.T, e *baseline.Env, method, path, body, token string, hdr map[string]string) baseline.Resp {
	t.Helper()
	rq, err := http.NewRequest(method, e.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		rq.Header.Set("Content-Type", "application/json")
	}
	rq.Header.Set("Authorization", "Bearer "+token)
	rq.Header.Set("X-APA-Session", "e2e-session") // 带本人原话的第二档写入还要会话引用
	rq.Header.Set("X-APA-Session-Kind", "direct") // 且会话是与本人的私聊（§14.5）
	for k, v := range hdr {
		rq.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(rq)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return baseline.Resp{Code: resp.StatusCode, Body: string(b)}
}

func sameJSON(a, b any) bool { return reflect.DeepEqual(a, b) }

// project 返回验收项目 id。
func project(e *baseline.Env, name string) string { return e.Projects[name] }

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

// toAny 把任意值经 JSON 转成通用结构，便于整体比较。
func toAny(v any) any {
	b, _ := json.Marshal(v)
	var out any
	_ = json.Unmarshal(b, &out)
	return out
}

// setup 复用 baseline 的环境步骤，但不声明 stub-harness：本组流程不拉起任何无头运行，
// 且 baseline 的 `apa declare` 以 DEL 提交、不带本人原话，被内核的 delegate 证据规则拒绝（属于 baseline 节点）。
func setup(t testing.TB) *baseline.Env {
	t.Helper()
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	return e
}
