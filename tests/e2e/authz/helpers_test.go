//go:build e2e

// Package authz 是全流程验收 flow-authz-matrix、flow-allowed-executors、flow-command-registration 的端到端测试
// （验收文档 ACT-AUTH-*、ACT-ALW-*、ACT-CMD-*）。每个测试用真实 apa 二进制（apa init、apa serve）和
// tests/e2e/baseline 的共用环境脚本，在隔离的 APA_HOME 里逐步执行验收动作，并核对"通过"判据。
// Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
package authz

import (
	"encoding/json"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 退出码（附录 B.1）。
const (
	exitInvalid   = 2
	exitAuth      = 3
	exitDenied    = 4
	exitNotFound  = 5
	exitWebOnly   = 20
	exitNotAllow  = 21
	workHost      = "work-host.cli"
	otherHost     = "other-host.cli"
	ownerExecutor = "owner"
)

type kit struct {
	*baseline.Env
	t *testing.T
}

func newKit(t *testing.T) *kit { t.Helper(); return &kit{baseline.Setup(t), t} }

// step 运行一个验收动作；子测试期间 kit 与环境脚本的致命错误记在该子测试上。
func (k *kit) step(name string, f func(t *testing.T)) {
	k.t.Helper()
	parent := k.t
	ok := parent.Run(name, func(t *testing.T) {
		k.t, k.Env.T = t, t
		defer func() { k.t, k.Env.T = parent, parent }()
		f(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func (k *kit) expect(label string, r baseline.Result, code int) {
	k.t.Helper()
	if r.Code != code {
		k.t.Fatalf("%s: 退出码 %d，期望 %d\nstdout=%s\nstderr=%s", label, r.Code, code, r.Stdout, r.Stderr)
	}
}

// errBody 解析 CLI 失败时 stderr 里的结构化错误体（code、why、next）。
func (k *kit) errBody(r baseline.Result) map[string]any {
	k.t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(r.Stderr), &m); err != nil {
		k.t.Fatalf("stderr 不是结构化错误体: %q", r.Stderr)
	}
	return m
}

func (k *kit) idOf(label string, r baseline.Result) string {
	k.t.Helper()
	k.expect(label, r, 0)
	var o struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &o); err != nil || o.ID == "" {
		k.t.Fatalf("%s 没有返回 id: %q", label, r.Stdout)
	}
	return o.ID
}

// addItem 以 DEL 建一个 open 事项（delegate 写操作须附本人原话）。
func (k *kit) addItem(project, assignee string) string {
	k.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", "acc-" + project + "-" + assignee, "--phase", "open", "--evidence", "e2e setup"}
	if project != "" {
		args = append(args, "--project", k.Projects[project])
	}
	if assignee != "" {
		args = append(args, "--assignee", assignee)
	}
	return k.idOf("item add", k.Apa(k.DEL, "", args...))
}

// delegate 以 DEL 运行 `apa delegate`（delegate.create 属代理写命令，须附本人原话）。
func (k *kit) delegate(args ...string) baseline.Result {
	k.t.Helper()
	return k.Apa(k.DEL, "", append([]string{"delegate"}, append(args, "--evidence", "owner asked")...)...)
}

// item 以 OBS 读事项。
func (k *kit) item(id string) map[string]any {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "item", "show", id)
	k.expect("item show "+id, r, 0)
	var out struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil || out.Item == nil {
		k.t.Fatalf("item show 输出无 item: %q", r.Stdout)
	}
	return out.Item
}

func (k *kit) version(id string) float64 {
	k.t.Helper()
	v, _ := k.item(id)["version"].(float64)
	return v
}

// Event 是审计页的一条记录。
type Event struct {
	Seq      int64          `json:"seq"`
	Type     string         `json:"type"`
	Object   string         `json:"object"`
	Actor    map[string]any `json:"actor"`
	Reason   string         `json:"reason"`
	Evidence string         `json:"evidence"`
	Grant    string         `json:"grant"`
	Payload  map[string]any `json:"payload"`
}

// audit 以本人身份读审计页（/v1/audit/events）。
func (k *kit) audit(q url.Values) []Event {
	k.t.Helper()
	r := k.Web("GET", "/v1/audit/events?limit=500&"+q.Encode(), "")
	if r.Code != 200 {
		k.t.Fatalf("审计页: %d %s", r.Code, r.Body)
	}
	var out struct {
		Events []Event `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Events
}

func (k *kit) rejections() []Event { return k.audit(url.Values{"type": {"command.rejected"}}) }

// waitFor 轮询直到 cond 为真；用于等待内核后台循环（规则、运行）。
func (k *kit) waitFor(what string, d time.Duration, cond func() bool) {
	k.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			k.t.Fatalf("%s: %v 内未发生", what, d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (k *kit) webOK(method, path, body string, want int) string {
	k.t.Helper()
	r := k.Web(method, path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

// itemsOf 列出某项目下的全部事项 id → assignee。
func (k *kit) itemsOf(project string) map[string]string {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "item", "list", "--project", k.Projects[project])
	k.expect("item list", r, 0)
	var out struct {
		Items []struct {
			ID       string `json:"id"`
			Assignee string `json:"assignee"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	m := map[string]string{}
	for _, it := range out.Items {
		m[it.ID] = it.Assignee
	}
	return m
}

func (k *kit) allowed(project string) string {
	k.t.Helper()
	r := k.Apa(k.OBS, "", "project", "show", k.Projects[project])
	k.expect("project show", r, 0)
	var m map[string]json.RawMessage
	_ = json.Unmarshal([]byte(r.Stdout), &m)
	return string(m["allowed_executors"])
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }
