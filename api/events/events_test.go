package events

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var bg = context.Background()

type env struct {
	t    *testing.T
	st   *store.Store
	auth *auth.Auth
	srv  *core.Server
}

func setup(t *testing.T, poll time.Duration) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(st, poll)}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, st: st, auth: a, srv: srv}
}

func (e *env) do(path string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.RemoteAddr = "127.0.0.1:5000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) token(kind auth.Kind, scope string) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

var human = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}

func (e *env) emit(typ, obj, payload string) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: typ, Object: obj, Actor: human, Reason: "SECRET-REASON",
			Evidence: "SECRET-EVIDENCE", Payload: json.RawMessage(payload)})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

type page struct {
	Events []struct {
		Seq    int64  `json:"seq"`
		Type   string `json:"type"`
		Object string `json:"object"`
		Title  string `json:"title"`
	} `json:"events"`
	Next int64 `json:"next"`
}

func pageOf(t *testing.T, rec *httptest.ResponseRecorder) page {
	t.Helper()
	var p page
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &p) != nil {
		t.Fatalf("events = %d %s", rec.Code, rec.Body)
	}
	return p
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return de.Code
}

// ACT-EVT-03：observer 读到事件，信封不含正文/reason/证据；无凭据 401。
func TestObserverReadsEnvelopesAndAnonymousIsRefused(t *testing.T) {
	e := setup(t, 0)
	e.emit("item.created", "item_1", `{"item":{"title":"T1","summary":"SECRET-BODY"}}`)
	e.emit("item.phase_changed", "item_1", `{}`)
	obs := e.token(auth.Observer, "")

	rec := e.do("/v1/events?cursor=0", obs)
	p := pageOf(t, rec)
	if len(p.Events) != 2 || p.Events[0].Type != "item.created" || p.Events[0].Title != "T1" || p.Events[1].Seq <= p.Events[0].Seq || p.Next != p.Events[1].Seq {
		t.Fatalf("page = %+v", p)
	}
	for _, leak := range []string{"SECRET-BODY", "SECRET-REASON", "SECRET-EVIDENCE"} {
		if strings.Contains(rec.Body.String(), leak) {
			t.Fatalf("envelope leaks %s: %s", leak, rec.Body)
		}
	}

	anon := e.do("/v1/events?cursor=0", nil)
	if anon.Code != 401 || codeOf(t, anon) != domain.CodeUnauthenticated {
		t.Fatalf("anonymous = %d %s", anon.Code, anon.Body)
	}
}

// 游标自持：从上次 next 续读恰好补上新事件；省略 cursor 等于 0。
func TestResumeFromCursor(t *testing.T) {
	e := setup(t, 0)
	obs := e.token(auth.Observer, "")
	e.emit("item.created", "item_1", `{}`)
	first := pageOf(t, e.do("/v1/events", obs))
	for i := 0; i < 3; i++ {
		e.emit("item.updated", "item_1", `{}`)
	}
	rest := pageOf(t, e.do("/v1/events?cursor="+itoa(first.Next), obs))
	if len(rest.Events) != 3 || rest.Events[0].Seq != first.Next+1 {
		t.Fatalf("resume = %+v after %d", rest, first.Next)
	}
	if got := pageOf(t, e.do("/v1/events?cursor="+itoa(rest.Next), obs)); len(got.Events) != 0 || got.Next != rest.Next {
		t.Fatalf("drained = %+v", got)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ACT-EVT-04：未登记类型永不出现，且游标越过它不卡住；limit 生效。
func TestOnlyRegisteredTypesAndLimit(t *testing.T) {
	e := setup(t, 0)
	obs := e.token(auth.Observer, "")
	e.emit("item.created", "item_1", `{}`)
	e.emit("internal.secret_thing", "item_1", `{}`)
	e.emit("run.finished", "run_1", `{}`)
	p := pageOf(t, e.do("/v1/events?cursor=0", obs))
	if len(p.Events) != 2 || p.Events[1].Type != "run.finished" || p.Next != 3 {
		t.Fatalf("page = %+v", p)
	}
	for _, ev := range p.Events {
		if !domain.IsRegisteredEventType(ev.Type) {
			t.Fatalf("unregistered type leaked: %s", ev.Type)
		}
	}
	if one := pageOf(t, e.do("/v1/events?cursor=0&limit=1", obs)); len(one.Events) != 1 || one.Next != 1 {
		t.Fatalf("limit page = %+v", one)
	}
}

// 谁能读：observer 及以上，加导出与转发 ext；渠道 ext、webhook 与其他 scope 的 ext 一律 403。
func TestCredentialKinds(t *testing.T) {
	e := setup(t, 0)
	e.emit("item.created", "item_1", `{}`)
	for name, tc := range map[string]struct {
		kind  auth.Kind
		scope string
		want  int
	}{
		"observer":      {auth.Observer, "", 200},
		"export ext":    {auth.Ext, "export", 200},
		"named export":  {auth.Ext, "export.notes", 200},
		"forward ext":   {auth.Ext, "forward", 200},
		"channel ext":   {auth.Ext, "channel.telegram", 403},
		"webhook":       {auth.Webhook, "src", 403},
		"worker":        {auth.Worker, "", 200},
		"delegate":      {auth.Delegate, "", 200},
		"unknown scope": {auth.Ext, "exporter", 403},
	} {
		rec := e.do("/v1/events?cursor=0", e.token(tc.kind, tc.scope))
		if rec.Code != tc.want {
			t.Errorf("%s = %d %s, want %d", name, rec.Code, rec.Body, tc.want)
		}
	}
}

func TestBadParams(t *testing.T) {
	e := setup(t, 0)
	obs := e.token(auth.Observer, "")
	for _, q := range []string{"cursor=-1", "cursor=x", "limit=0", "limit=501", "wait=-1", "wait=x"} {
		rec := e.do("/v1/events?"+q, obs)
		if rec.Code != 400 || codeOf(t, rec) != domain.CodeInvalidRequest {
			t.Errorf("%s = %d %s", q, rec.Code, rec.Body)
		}
	}
}

// 长轮询：无新事件时挂起，事件到达立即返回；超时返回空页和原游标。
func TestLongPoll(t *testing.T) {
	e := setup(t, 10*time.Millisecond)
	obs := e.token(auth.Observer, "")
	e.emit("item.created", "item_1", `{}`)

	go func() { time.Sleep(100 * time.Millisecond); e.emit("run.finished", "run_1", `{}`) }()
	start := time.Now()
	p := pageOf(t, e.do("/v1/events?cursor=1&wait=5", obs))
	if len(p.Events) != 1 || p.Events[0].Type != "run.finished" || time.Since(start) > 3*time.Second {
		t.Fatalf("long poll = %+v after %v", p, time.Since(start))
	}

	start = time.Now()
	idle := pageOf(t, e.do("/v1/events?cursor=2&wait=1", obs))
	if len(idle.Events) != 0 || idle.Next != 2 || time.Since(start) < 900*time.Millisecond {
		t.Fatalf("idle = %+v after %v", idle, time.Since(start))
	}
}

// 长轮询只吃过滤掉的事件时，游标要前进，不能卡在不可见事件上。
func TestLongPollAdvancesPastInvisibleEvents(t *testing.T) {
	e := setup(t, 10*time.Millisecond)
	obs := e.token(auth.Observer, "")
	e.emit("internal.secret_thing", "x", `{}`)
	p := pageOf(t, e.do("/v1/events?cursor=0&wait=1", obs))
	if len(p.Events) != 0 || p.Next != 1 {
		t.Fatalf("page = %+v", p)
	}
}
