package screening_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	apicontext "github.com/xiaohei-info/awesome-personal-agent/api/context"
	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apidecisions "github.com/xiaohei-info/awesome-personal-agent/api/decisions"
	apideliveries "github.com/xiaohei-info/awesome-personal-agent/api/deliveries"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	"github.com/xiaohei-info/awesome-personal-agent/api/screening"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	kdec "github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	kscreening "github.com/xiaohei-info/awesome-personal-agent/kernel/screening"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const (
	ownerPW = "correct horse battery"
	webBase = "http://apa.example"
	title   = "wire the vendor payment"
	askBody = `{"ask":{"question":"approve-the-wire?","options":[{"id":"A","label":"yes-wire"},{"id":"B","label":"no-wire"}],` +
		`"deadline":"2099-01-01T00:00:00Z","default_on_timeout":"B"}}`
)

var bg = context.Background()

// gateJudge 是模型边界的替身：在 release 之前一直阻塞，之后返回失败（按规则继续，初筛标记清除）。
type gateJudge struct{ release chan struct{} }

func (j gateJudge) Judge(ctx context.Context, _ direct.JudgeCall) (direct.Verdict, error) {
	select {
	case <-j.release:
	case <-ctx.Done():
	}
	return direct.Verdict{}, errors.New("screening stub: released")
}

type env struct {
	t       *testing.T
	st      *store.Store
	auth    *auth.Auth
	srv     *core.Server
	scr     *kscreening.Service
	q       *delivery.Queue
	clk     *clock.Clock
	judge   gateJudge
	cookie  *http.Cookie
	csrf    string
	now     time.Time
	tickNow func() time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		kitems.Module, delivery.Module, kcontext.Module, attention.Module, proc.Module, runidentity.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, auth: auth.New(nil), judge: gateJudge{release: make(chan struct{})}, now: time.Now()}
	e.tickNow = func() time.Time { return e.now }
	x := command.New(st, e.tickNow)
	if err := kitems.Register(x, kitems.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if e.scr, err = kscreening.New(kscreening.Options{Store: st, Judge: e.judge, Now: e.tickNow}); err != nil {
		t.Fatal(err)
	}
	if err := kdec.RegisterWith(x, e.scr); err != nil {
		t.Fatal(err)
	}
	x.OnCommit(e.scr.OnCommit)
	e.q = delivery.New(delivery.Options{Now: e.tickNow, UnclaimedAfter: 5 * time.Minute, Lease: time.Hour})
	att := attention.New(attention.Options{Queue: e.q, Now: e.tickNow})
	if e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error {
		switch {
		case att.Owns(c):
			return att.HandleTimer(tx, c)
		case kdec.Owns(c):
			return kdec.HandleTimer(tx, c)
		}
		return kitems.HandleTimer(tx, c)
	}, e.tickNow); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, att.Start); err != nil {
		t.Fatal(err)
	}
	owner := domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	if err := st.Update(bg, func(tx *store.Tx) error {
		if err := core.SetOwnerPassword(tx, ownerPW); err != nil {
			return err
		}
		// 配置了模型端点，决策请求才会进入初筛；端点本身由 gateJudge 替身承接。
		_, err := policy.Update(tx, owner, "model", func(p *policy.Policy) error {
			p.GenerateEndpoint, p.ScreeningTimeoutMs = policy.GenerateEndpoint{URL: "http://model.invalid", Model: "m1", APIKey: "k"}, 120000
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.srv, err = core.New(core.Deps{Store: st, Auth: e.auth, Groups: []core.RouteGroup{
		apiitems.Routes(x, st), apidecisions.Routes(x, webBase), apicontext.Routes(st), apiwhy.Routes(st),
		apideliveries.Routes(st, e.q, time.Millisecond)}})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	if rec.Code != 200 || e.cookie == nil {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	e.csrf = out.CSRF
	return e
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(r *http.Request) {
	r.AddCookie(e.cookie)
	r.Header.Set(core.CSRFHeader, e.csrf)
}

func bearer(secret string) func(*http.Request) {
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("X-APA-Session", "s1")
		r.Header.Set("X-APA-Session-Kind", "direct")
	}
}

func (e *env) issue(spec auth.Spec) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, spec)
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return bearer(secret)
}

// runCredential 为目标事项入队一次后台运行并签发运行凭据（与内核拉起运行同一条路径）。
func (e *env) runCredential(target string) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: target, TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{"true"}}}, e.now)
		if err != nil {
			return err
		}
		secret, err = runidentity.Issue(tx, r.ID, e.now)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return bearer(secret)
}

func errBody(t *testing.T, rec *httptest.ResponseRecorder) domain.Error {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return de
}

func noLeak(t *testing.T, where, body string) {
	t.Helper()
	for _, secret := range []string{title, "approve-the-wire", "yes-wire", "no-wire"} {
		if strings.Contains(body, secret) {
			t.Fatalf("%s leaks %q while screening is pending: %s", where, secret, body)
		}
	}
}

// 初筛返回前：Web 可见并标"初筛中"；其他凭据的事项读取、why、context、投递都得不到问题与选项；
// 代答 22（附 web_url，事项仍待决，留拒绝记录）；运行凭据 4；初筛结束后一切恢复。
// 对应 ACT-SCR-02、ACT-SCR-03、ACT-SCR-04、ACT-SCR-05。
func TestScreeningPendingGates(t *testing.T) {
	e := setup(t)
	wrk := e.issue(auth.Spec{Kind: auth.Worker, Executor: "wrk1"})
	dlg := e.issue(auth.Spec{Kind: auth.Delegate})
	ch := e.issue(auth.Spec{Kind: auth.Ext, Scope: apideliveries.ChannelScopePrefix + "acc-chan"})

	rec := e.do("POST", "/v1/items", `{"kind":"task","title":"`+title+`","phase":"open","assignee":"wrk1","reason":"r"}`, e.web)
	var created struct{ ID string }
	_ = json.Unmarshal(rec.Body.Bytes(), &created)
	id := created.ID
	if rec.Code != 201 || id == "" {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/items/"+id+"/ask", askBody, wrk); rec.Code != 200 {
		t.Fatalf("ask: %d %s", rec.Code, rec.Body)
	}

	// ACT-SCR-02：Web 看得到完整卡片并标"初筛中"。
	rec = e.do("GET", "/v1/items/"+id, "", e.web)
	var web struct{ Item domain.Item }
	_ = json.Unmarshal(rec.Body.Bytes(), &web)
	if web.Item.Ask == nil || !web.Item.Ask.ScreeningPending || web.Item.Ask.Question != "approve-the-wire?" || web.Item.Title != title {
		t.Fatalf("Web must see the full card marked screening: %s", rec.Body)
	}

	// ACT-SCR-02 / 03：事项读取（inbox 与 item show 的数据源）只给标记。
	for _, path := range []string{"/v1/items/" + id, "/v1/items?phase=needs_input"} {
		rec = e.do("GET", path, "", dlg)
		if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"screening_pending":true`) {
			t.Fatalf("GET %s must carry the marker: %d %s", path, rec.Code, rec.Body)
		}
		noLeak(t, "GET "+path, rec.Body.String())
	}
	rec = e.do("GET", "/v1/why/"+id, "", dlg)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"screening_pending":true`) {
		t.Fatalf("why must carry the marker: %d %s", rec.Code, rec.Body)
	}
	noLeak(t, "why", rec.Body.String())

	// ACT-SCR-02：context 与投递不暴露。
	e.now = e.now.Add(time.Minute)
	if _, err := e.clk.Tick(bg); err != nil {
		t.Fatal(err)
	}
	rec = e.do("GET", "/v1/context?scope=all&format=inject", "", dlg)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), id) {
		t.Fatalf("context must not carry the pending decision: %d %s", rec.Code, rec.Body)
	}
	noLeak(t, "context", rec.Body.String())
	if rec = e.do("GET", "/v1/deliveries/next?wait=1", "", ch); strings.Contains(rec.Body.String(), id) || rec.Code == 200 {
		t.Fatalf("no delivery may be claimable while screening: %d %s", rec.Code, rec.Body)
	}

	// ACT-SCR-04：代答 22。
	rec = e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A","evidence":"选 A"}`, dlg)
	de := errBody(t, rec)
	if de.ExitCode() != 22 || de.Code != domain.CodeRiskScreeningPending || de.WebURL == nil || *de.WebURL != webBase+"/items/"+id || de.Why == "" {
		t.Fatalf("delegate answer = %d %s", rec.Code, rec.Body)
	}
	rec = e.do("GET", "/v1/items/"+id, "", e.web)
	_ = json.Unmarshal(rec.Body.Bytes(), &web)
	if web.Item.Phase != domain.PhaseNeedsInput || web.Item.Ask.Resolution != nil {
		t.Fatalf("a refused answer must leave the item in needs_input: %s", rec.Body)
	}
	rec = e.do("GET", "/v1/audit/events?object="+id, "", e.web)
	if !strings.Contains(rec.Body.String(), string(domain.CodeRiskScreeningPending)) {
		t.Fatalf("the refusal must be on record: %s", rec.Body)
	}

	// ACT-SCR-05：运行凭据仍是 4，不是 22。
	run := e.runCredential(id)
	rec = e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A","evidence":"x"}`, run)
	if de = errBody(t, rec); de.ExitCode() != 4 || de.Code != domain.CodePermissionDenied {
		t.Fatalf("run credential answer = %d %s", rec.Code, rec.Body)
	}
	// 运行凭据同样读不到问题与选项。
	if rec = e.do("GET", "/v1/items/"+id, "", run); rec.Code != 200 {
		t.Fatalf("run credential read: %d %s", rec.Code, rec.Body)
	} else {
		noLeak(t, "run credential read", rec.Body.String())
	}

	// 初筛结束（失败按规则继续）：标记清除，其他凭据读得到内容，代答恢复。
	close(e.judge.release)
	e.scr.Wait()
	rec = e.do("GET", "/v1/items/"+id, "", dlg)
	if !strings.Contains(rec.Body.String(), "approve-the-wire?") || strings.Contains(rec.Body.String(), `"screening_pending":true`) {
		t.Fatalf("after screening the item is readable: %s", rec.Body)
	}
	if rec = e.do("POST", "/v1/items/"+id+"/decide", `{"choice":"A","evidence":"选 A"}`, dlg); rec.Code != 200 {
		t.Fatalf("delegate answer after screening: %d %s", rec.Code, rec.Body)
	}
}

// Redact 的契约：Web 与初筛已返回的事项原样；其余只留标记。
func TestRedact(t *testing.T) {
	pending := domain.Item{ID: "i1", Phase: domain.PhaseNeedsInput, Version: 3, Kind: "task", Title: "secret", Body: "secret",
		Ask: &domain.Ask{AskInput: domain.AskInput{Question: "secret"}, AskKernelState: domain.AskKernelState{ScreeningPending: true}}}
	if got := screening.Redact(pending, authz.LevelWeb); got.Title != "secret" || got.Ask.Question != "secret" {
		t.Fatalf("Web sees everything: %+v", got)
	}
	got := screening.Redact(pending, authz.LevelDelegate)
	if got.ID != "i1" || got.Version != 3 || got.Title != "" || got.Body != "" || got.Ask.Question != "" || !got.Ask.ScreeningPending {
		t.Fatalf("others see the marker only: %+v", got)
	}
	done := pending
	done.Ask = &domain.Ask{AskInput: domain.AskInput{Question: "secret"}}
	if got := screening.Redact(done, authz.LevelObserver); got.Title != "secret" {
		t.Fatalf("a finished screening hides nothing: %+v", got)
	}
}
