//go:build e2e

// Package materials 是全流程验收 flow-material-owned、flow-material-hosted 的唯一 owner（验收文档 ACT-MAT-01..13、
// ACT-HST-01..08）：真实 apa 二进制 + 真实内核进程 + 真实 hosted-reader，只替身模型端点与无头运行。
// Web 步骤以本人会话请求 Web 页面调用的同一组 /v1 接口（与 tests/e2e/decisions 一致）。
package materials

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

type fx struct {
	*baseline.Env
	t *testing.T
}

// apa 运行命令并断言退出码。
func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// webOK 以本人 Web 会话请求并断言状态码，返回响应体。
func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) file(name, content string) string {
	f.t.Helper()
	p := filepath.Join(f.t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
	return v
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

type committed struct {
	Material string `json:"material"`
	Revision string `json:"revision"`
	State    string `json:"state"`
}

// commit 以 token 提交一条修订（新建材料带 kind/title/project，已有材料带 id）并断言成功。
func (f *fx) commit(token, content string, args ...string) committed {
	f.t.Helper()
	a := append([]string{"material", "commit"}, args...)
	a = append(a, "--from", f.file("m.md", content))
	return decode[committed](f.t, f.apa(token, 0, a...).Stdout)
}

// revise 以 token 对已有材料提交新修订。
func (f *fx) revise(token, id, content string, args ...string) committed {
	f.t.Helper()
	a := append([]string{"material", "commit", id, "--from", f.file("m.md", content)}, args...)
	return decode[committed](f.t, f.apa(token, 0, a...).Stdout)
}

type revision struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	BasedOn string `json:"based_on"`
	Reason  string `json:"reason"`
	Actor   struct {
		Type string `json:"type"`
	} `json:"actor"`
	Content string `json:"content"`
	Seq     int    `json:"seq"`
}

type material struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Title      string `json:"title"`
	Project    string `json:"project"`
	CurrentRev string `json:"current_rev"`
	Location   string `json:"location"`
}

type shown struct {
	Material material       `json:"material"`
	Revision *revision      `json:"revision"`
	Hosted   map[string]any `json:"hosted"`
}

func (f *fx) show(id string, extra ...string) shown {
	f.t.Helper()
	return decode[shown](f.t, f.apa(f.OBS, 0, append([]string{"material", "show", id}, extra...)...).Stdout)
}

func (f *fx) revisions(id string) []revision {
	f.t.Helper()
	return decode[struct {
		Revisions []revision `json:"revisions"`
	}](f.t, f.webOK("GET", "/materials/"+id+"/revisions", "", http.StatusOK)).Revisions
}

type event struct {
	Type   string `json:"type"`
	Object string `json:"object"`
	Actor  struct {
		Type string `json:"type"`
		Via  string `json:"via"`
	} `json:"actor"`
	Payload map[string]any `json:"payload"`
}

// events 取某对象（空表示全部）某类型的审计事件，即 Web 审计页与"agent 变更"视图的数据源。
func (f *fx) events(object, typ string) []event {
	f.t.Helper()
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + object
	}
	if typ != "" {
		path += "&type=" + typ
	}
	return decode[struct {
		Events []event `json:"events"`
	}](f.t, f.webOK("GET", path, "", http.StatusOK)).Events
}

// agentChanges 是"agent 变更"视图的过滤口径：actor.type=agent 且不是被拒绝的命令。
func (f *fx) agentChanges(object string) []event {
	var out []event
	for _, e := range f.events(object, "") {
		if e.Actor.Type == "agent" && e.Type != "command.rejected" {
			out = append(out, e)
		}
	}
	return out
}
