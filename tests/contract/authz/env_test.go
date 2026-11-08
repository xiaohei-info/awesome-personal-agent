// Package authz_test 是 §14.3 授权矩阵的契约测试：全部走真实的授权判定、命令管线与 HTTP 骨干，
// 不 mock 授权判定。
package authz_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apidelegation "github.com/xiaohei-info/awesome-personal-agent/api/delegation"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apiprojects "github.com/xiaohei-info/awesome-personal-agent/api/projects"
	"github.com/xiaohei-info/awesome-personal-agent/api/settings/credentials"
	settingsexec "github.com/xiaohei-info/awesome-personal-agent/api/settings/exec"
	settingspolicy "github.com/xiaohei-info/awesome-personal-agent/api/settings/policy"
	settingspurge "github.com/xiaohei-info/awesome-personal-agent/api/settings/purge"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	kpurge "github.com/xiaohei-info/awesome-personal-agent/kernel/purge"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const (
	ownerPW   = "correct horse battery"
	workHost  = "work-host.cli"
	otherHost = "other-host.cli"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

// env 是真实账本 + 真实命令管线 + 真实 HTTP 骨干（含四组设置路由）。
type env struct {
	t      *testing.T
	st     *store.Store
	au     *auth.Auth
	x      *command.Executor
	srv    *core.Server
	secret map[string]string // OBS、WRK、DEL → 凭据明文
	cookie *http.Cookie
	csrf   string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, materials.Module, executors.Module, delegation.Module, proc.Module, runidentity.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	l := config.LayoutOf(home)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, au: auth.New(nil), secret: map[string]string{}}
	e.x = command.New(st, func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(projects.Register(e.x))
	must(items.Register(e.x, items.NewKinds()))
	must(materials.Register(e.x))
	must(blobs.Register(e.x, l.Blobs))
	must(kpurge.Register(e.x, l.Blobs))
	must(executors.Register(e.x, executors.Options{Owner: "owner", Project: projects.Get}))
	must(delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Error(err) }}))
	for _, id := range []string{workHost, otherHost} {
		e.cmd(owner, "executor.put", map[string]any{"id": id, "kind": "external-cli"})
	}
	e.issue("OBS", auth.Spec{Kind: auth.Observer})
	e.issue("WRK", auth.Spec{Kind: auth.Worker, Executor: workHost})
	e.issue("DEL", auth.Spec{Kind: auth.Delegate})
	must(st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }))
	cred, err := credentials.Mount(e.x, e.au)
	must(err)
	e.srv, err = core.New(core.Deps{Store: st, Auth: e.au, Groups: []core.RouteGroup{
		cred, apiitems.Routes(e.x, st), apidelegation.Routes(e.x), apiprojects.Routes(st, e.x), settingspolicy.Routes(st), settingsexec.Routes(st, e.x), settingspurge.Routes(e.x)}})
	must(err)
	rec := e.http("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	e.csrf = out.CSRF
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	return e
}

func (e *env) issue(name string, s auth.Spec) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, secret, err := e.au.Issue(tx, s)
		e.secret[name] = secret
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// as 以真实签发的凭据发起请求：操作者与信任级由认证步骤据账本得出。
func (e *env) as(name string) command.Identity {
	return command.Bearer{Auth: e.au, Secret: e.secret[name], Origin: auth.Origin{Harness: "h"}}
}

func (e *env) do(id command.Identity, cmd string, in any, mod ...func(*command.Request)) (command.Result, error) {
	b, _ := json.Marshal(in)
	req := command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner"}
	for _, m := range mod {
		m(&req)
	}
	return e.x.Do(bg, req)
}

func (e *env) cmd(id command.Identity, cmd string, in any, mod ...func(*command.Request)) string {
	e.t.Helper()
	r, err := e.do(id, cmd, in, mod...)
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	var out struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(r.Value, &out)
	return out.ID
}

// wantExit 断言命令被拒绝且退出码为 want，返回错误体。
func (e *env) wantExit(want int, id command.Identity, cmd string, in any, mod ...func(*command.Request)) *domain.Error {
	e.t.Helper()
	_, err := e.do(id, cmd, in, mod...)
	var de *domain.Error
	if !errors.As(err, &de) || de.ExitCode() != want {
		e.t.Fatalf("%s: want exit %d, got %v", cmd, want, err)
	}
	return de
}

func (e *env) item(assignee, title string) string {
	e.t.Helper()
	return e.cmd(owner, "item.create", map[string]any{"kind": "task", "title": title, "phase": "open", "assignee": assignee})
}

func (e *env) http(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(method, path, body string) *httptest.ResponseRecorder {
	return e.http(method, path, body, func(r *http.Request) {
		r.AddCookie(e.cookie)
		r.Header.Set(core.CSRFHeader, e.csrf)
	})
}

func bearer(secret string) func(*http.Request) {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

// events 读出账本里某类型的全部事件。
func (e *env) events(typ string) []eventlog.Entry {
	e.t.Helper()
	var out []eventlog.Entry
	err := e.st.View(bg, func(tx *store.Tx) error {
		all, err := eventlog.Read(tx, 0, 100000)
		for _, en := range all {
			if string(en.Event.Type) == typ {
				out = append(out, en)
			}
		}
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) head() int64 {
	e.t.Helper()
	var h int64
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { h, err = eventlog.Head(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return h
}

func (e *env) now() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func asErr(err error, de **domain.Error) bool { return errors.As(err, de) }

// api 以名为 name 的 Bearer 凭据请求 HTTP；evidence 非空时带 X-APA-Evidence。不带会话引用。
func (e *env) api(name, method, path, body, evidence string) *httptest.ResponseRecorder {
	return e.apiIn(name, "", method, path, body, evidence)
}

// apiIn 同 api，另带会话引用 X-APA-Session（第二档的本人原话要与它同时携带）。
func (e *env) apiIn(name, session, method, path, body, evidence string) *httptest.ResponseRecorder {
	return e.http(method, path, body, func(r *http.Request) {
		bearer(e.secret[name])(r)
		if evidence != "" {
			r.Header.Set(core.EvidenceHeader, evidence)
		}
		if session != "" {
			r.Header.Set("X-APA-Session", session)
		}
	})
}

// principal 是凭据在账本里的操作者标识：事项的 assignee 等于它，就是该凭据"绑定"的事项。
func (e *env) principal(name string) string {
	e.t.Helper()
	var p string
	err := e.st.View(bg, func(tx *store.Tx) error {
		c, err := e.as(name).Authenticate(tx)
		p = c.Actor.Principal
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

// rejected 是账本里的 command.rejected 事件数。
func (e *env) rejected() int { return len(e.events(domain.EvCommandRejected)) }

// wantReject 断言响应是 403/400 等拒绝，错误体含 code、why、next，且退出码为 want。
func wantReject(t *testing.T, rec *httptest.ResponseRecorder, want int) domain.Error {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" || de.Why == "" || de.Next == "" {
		t.Fatalf("not a B.3 error body (%d): %s", rec.Code, rec.Body)
	}
	if got := de.ExitCode(); got != want {
		t.Fatalf("exit %d, want %d: %s", got, want, rec.Body)
	}
	return de
}
