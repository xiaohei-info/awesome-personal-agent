package authz_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/guide"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const marker = "MARKER-TITLE"

// noLeak 断言错误体的每个字段（why、next、allowed、web_url）都不含账本里的用户内容。
func noLeak(t *testing.T, label string, de *domain.Error) {
	t.Helper()
	if de.Why == "" || de.Next == "" {
		t.Errorf("%s: error lacks why/next: %+v", label, de)
	}
	b, _ := json.Marshal(de)
	if strings.Contains(string(b), marker) {
		t.Errorf("%s: error leaks ledger content: %s", label, b)
	}
}

// ACT-INJ-04：事项标题与项目名都写成 MARKER-TITLE，引发退出码 4、6、20、21 各一次，并运行 apa guide，
// 所有错误的 why、next 与 guide 都不含 MARKER-TITLE。
func TestErrorsAndGuideCarryNoLedgerContent(t *testing.T) {
	e := setup(t)
	prj := e.cmd(owner, "project.create", map[string]any{"name": marker})
	e.cmd(owner, "project.allowed_executors.set", map[string]any{"id": prj, "executors": []string{workHost}})
	parent := e.cmd(owner, "item.create", map[string]any{"kind": "task", "title": marker, "summary": marker, "body": marker,
		"phase": "open", "project": prj})

	cases := []struct {
		label string
		exit  int
		rec   func() *httptest.ResponseRecorder
	}{
		// 退出码 4：worker 改不属于自己的事项。
		{"exit 4", domain.ExitPermissionDenied, func() *httptest.ResponseRecorder {
			return (e.api("WRK", "PATCH", "/v1/items/"+parent, patch("x"), ""))
		}},
		// 退出码 20：delegate 改项目允许的执行者（设置类）。
		{"exit 20", domain.ExitWebOnly, func() *httptest.ResponseRecorder {
			return (e.api("DEL", "PUT", "/v1/settings/projects/"+prj+"/allowed-executors", `{"executors":["evil"]}`, ""))
		}},
		// 退出码 21：delegate 把任务派给项目名单之外的执行者。
		{"exit 21", domain.ExitExecutorNotAllowed, func() *httptest.ResponseRecorder {
			return (e.api("DEL", "POST", "/v1/delegations", `{"parent":"`+parent+`","assignee":"`+otherHost+`","title":"`+marker+`"}`, "owner said so (chat#1)"))
		}},
	}
	for _, c := range cases {
		r := c.rec()
		de := wantReject(t, r, c.exit)
		noLeak(t, c.label, &de)
		if c.exit == domain.ExitExecutorNotAllowed && (len(de.Allowed) != 1 || de.Allowed[0] != workHost) {
			t.Errorf("exit 21 must list the project's allowed executor ids: %+v", de.Allowed)
		}
	}

	// 退出码 6：版本过期的写入。
	v := int64(0)
	_, err := e.do(owner, "item.update", map[string]any{"id": parent, "summary": "bump"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.do(owner, "item.update", map[string]any{"id": parent, "summary": "stale"}, func(r *command.Request) { r.ExpectVersion = &v })
	var de *domain.Error
	if !asErr(err, &de) || de.ExitCode() != domain.ExitConflict {
		t.Fatalf("exit 6: %v", err)
	}
	noLeak(t, "exit 6", de)

	// apa guide：无凭据运行，输出不含用户内容。
	var out, errb bytes.Buffer
	env := client.Env{Getenv: func(string) string { return "" }, UserHome: t.TempDir(), Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: failTransport{t}}}
	if code := client.Main([]string{"guide"}, env); code != 0 {
		t.Fatalf("guide exit %d: %s", code, errb.String())
	}
	if out.Len() == 0 || strings.Contains(out.String(), marker) {
		t.Fatalf("guide empty or leaks ledger content")
	}
}

type failTransport struct{ t *testing.T }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) {
	f.t.Fatal("guide must not call the kernel")
	return nil, nil
}
