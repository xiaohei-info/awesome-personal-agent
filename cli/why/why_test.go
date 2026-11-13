package why

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apiwhy "github.com/xiaohei-info/awesome-personal-agent/api/why"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	kwhy "github.com/xiaohei-info/awesome-personal-agent/kernel/why"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

var bg = context.Background()

// rig 装配真实账本与真实 /v1/why 路由；CLI 经 http.Client 直接打进该 handler。
type rig struct {
	t    *testing.T
	st   *store.Store
	auth *auth.Auth
	srv  *core.Server
}

type handlerTransport struct{ h http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.RemoteAddr = "127.0.0.1:5000"
	h.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

func newRig(t *testing.T) *rig {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, kitems.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	if err := kitems.Register(x, kitems.NewKinds()); err != nil {
		t.Fatal(err)
	}
	a := auth.New(nil)
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apiitems.Routes(x, st), apiwhy.Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, st: st, auth: a, srv: srv}
}

func (r *rig) token(kind auth.Kind) string {
	var secret string
	if err := r.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := r.auth.Issue(tx, auth.Spec{Kind: kind})
		secret = s
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return secret
}

// createItem 经 Web 会话创建一个带原因的事项（owner 会话只存在于 Web，故用 HTTP 直发）。
func (r *rig) createItem() string {
	r.t.Helper()
	call := func(path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", path, strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:5000"
		if mod != nil {
			mod(req)
		}
		rec := httptest.NewRecorder()
		r.srv.Handler().ServeHTTP(rec, req)
		return rec
	}
	rec := call("/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	var l struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &l)
	web := func(q *http.Request) {
		for _, c := range rec.Result().Cookies() {
			q.AddCookie(c)
		}
		q.Header.Set(core.CSRFHeader, l.CSRF)
	}
	rec2 := call("/v1/items", `{"kind":"task","title":"fix login","reason":"SECRET-REASON"}`, web)
	if rec2.Code != 201 {
		r.t.Fatalf("create: %d %s", rec2.Code, rec2.Body)
	}
	var w struct{ ID string }
	_ = json.Unmarshal(rec2.Body.Bytes(), &w)
	return w.ID
}

type result struct {
	code        int
	out, errOut string
}

func (r *rig) apa(token string, args ...string) result {
	r.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": token, "APA_HOME": r.t.TempDir()}
	env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: r.t.TempDir(), Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: handlerTransport{r.srv.Handler()}}}
	code := client.Main(args, env)
	return result{code, out.String(), errb.String()}
}

// ACT-WHY-01：委派级凭据回放事项，JSON 与 --pretty 都按序列出每一步的操作者和原因。
func TestWhyReplaysItemInOrderWithActorAndReason(t *testing.T) {
	r := newRig(t)
	id := r.createItem()
	del := r.token(auth.Delegate)

	res := r.apa(del, "why", id)
	if res.code != 0 {
		t.Fatalf("why: %+v", res)
	}
	var tl struct {
		ID    string
		Steps []struct {
			Seq    int64
			Type   string
			Reason string
			Actor  struct{ Principal string }
		}
	}
	if err := json.Unmarshal([]byte(res.out), &tl); err != nil || tl.ID != id || len(tl.Steps) == 0 {
		t.Fatalf("bad timeline %q: %v", res.out, err)
	}
	if s := tl.Steps[0]; s.Type != "item.created" || s.Reason != "SECRET-REASON" || s.Actor.Principal == "" {
		t.Fatalf("first step = %+v", s)
	}
	if strings.Contains(strings.TrimSpace(res.out), "\n") {
		t.Fatalf("default output must be compact JSON: %q", res.out)
	}

	pretty := r.apa(del, "why", id, "--pretty")
	if pretty.code != 0 || !strings.Contains(pretty.out, id) || !strings.Contains(pretty.out, "item.created") ||
		!strings.Contains(pretty.out, "SECRET-REASON") || !strings.Contains(pretty.out, tl.Steps[0].Actor.Principal) {
		t.Fatalf("pretty: %+v", pretty)
	}
	if strings.Index(pretty.out, "item.created") < strings.Index(pretty.out, id) {
		t.Fatalf("header must precede steps: %q", pretty.out)
	}
}

// ACT-WHY-02：observer 回放同一事项，正文被裁剪（--pretty 同样不泄露），结构仍完整并标注已裁剪。
func TestWhyObserverIsTrimmedButStructured(t *testing.T) {
	r := newRig(t)
	id := r.createItem()
	full := r.apa(r.token(auth.Delegate), "why", id)
	obs := r.token(auth.Observer)
	for _, args := range [][]string{{"why", id}, {"why", id, "--pretty"}} {
		res := r.apa(obs, args...)
		if res.code != 0 {
			t.Fatalf("%v: %+v", args, res)
		}
		if strings.Contains(res.out, "SECRET-REASON") || strings.Contains(res.out, "fix login") {
			t.Fatalf("%v leaks content: %s", args, res.out)
		}
		if !strings.Contains(res.out, "item.created") || !strings.Contains(res.out, "redacted") {
			t.Fatalf("%v lost structure: %s", args, res.out)
		}
	}
	var a, b struct{ Steps []struct{ Seq int64 } }
	_ = json.Unmarshal([]byte(full.out), &a)
	_ = json.Unmarshal([]byte(r.apa(obs, "why", id).out), &b)
	if len(a.Steps) == 0 || len(a.Steps) != len(b.Steps) {
		t.Fatalf("observer must see the same steps: %d vs %d", len(a.Steps), len(b.Steps))
	}
}

func TestWhyErrorSemantics(t *testing.T) {
	r := newRig(t)
	del := r.token(auth.Delegate)
	if res := r.apa(del, "why", "item_nope"); res.code != 5 || !strings.Contains(res.errOut, "not_found") {
		t.Fatalf("unknown id want exit 5: %+v", res)
	}
	if res := r.apa("bad-token", "why", "x"); res.code != 3 {
		t.Fatalf("bad token want exit 3: %+v", res)
	}
	for _, args := range [][]string{{"why"}, {"why", "a", "b"}, {"why", "--bogus"}} {
		if res := r.apa(del, args...); res.code != 2 || !strings.Contains(res.errOut, "usage: apa why") {
			t.Fatalf("%v want usage exit 2: %+v", args, res)
		}
	}
}

func TestWhyEscapesIDInPath(t *testing.T) {
	r := newRig(t)
	if res := r.apa(r.token(auth.Delegate), "why", "../health"); res.code != 5 {
		t.Fatalf("path-like id must stay one segment and 404: %+v", res)
	}
}

// --pretty 对自动动作、代答、抹除与初筛中的回放逐项标注，不丢授权依据。
func TestRenderShowsBasisChainErasureAndScreening(t *testing.T) {
	tl := kwhy.Timeline{ID: "item_1", ScreeningPending: true, Steps: []kwhy.Step{
		{Seq: 7, At: "2026-01-01T00:00:00Z", Type: "item.stage_changed", ChainDepth: 2, ResolvedVia: "chat",
			Actor: domain.Actor{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI, OnBehalfOf: "owner"},
			Basis: &domain.Basis{PolicyVersion: 12, RuleID: "r1"}, Evidence: "ok go", Payload: json.RawMessage(`{"to":"doing"}`)},
		{Seq: 8, Type: "item.created", Tombstone: true, Redacted: true},
	}}
	out := render(tl)
	for _, want := range []string{"screening pending", "policy v12 rule r1", "depth 2", "resolved via chat", "on behalf of owner",
		"evidence: ok go", `payload: {"to":"doing"}`, "[erased]", "[redacted]"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

// ACT-JRC-04/06：--pretty 在命令 Event 下写"判断"行，记录在就逐级列出，过了保留期只写 Event 里的结论。
func TestRenderShowsJudgmentLevelsThenOnlyConclusionAfterRetention(t *testing.T) {
	p := 0.30
	yes := true
	rec := &record.Record{ID: "jd_mid", Template: "triage/triage-screen@abc", Answer: "yes", DecidedBy: "chat", Band: record.Band{Low: 0.15, High: 0.60},
		Levels: []record.Level{
			{Backend: "classifier", PYes: &p, Outcome: "uncertain", DurationMs: 3},
			{Backend: "chat", Answer: "yes", Sufficient: &yes, Outcome: "decided", DurationMs: 90}}}
	out := render(kwhy.Timeline{ID: "item_1", Steps: []kwhy.Step{{Seq: 1, Type: "inbound.disposed", Judgment: &kwhy.Judgment{ID: "jd_mid", Answer: "yes", DecidedBy: "chat", Record: rec}}}})
	for _, want := range []string{"judgment jd_mid: triage/triage-screen@abc → yes, decided by chat", "1. classifier p=0.30 in the uncertain band, uncertain", "2. chat answer=yes, decided — decided"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	gone := render(kwhy.Timeline{ID: "item_1", Steps: []kwhy.Step{{Seq: 1, Type: "inbound.disposed", Judgment: &kwhy.Judgment{ID: "jd_mid", Answer: "yes", DecidedBy: "chat", Expired: true}}}})
	if !strings.Contains(gone, "judgment jd_mid: yes, decided by chat (record past retention") || strings.Contains(gone, "classifier") {
		t.Errorf("expired judgment must show only the conclusion:\n%s", gone)
	}
}

// ACT-DGA-10：--pretty 回放写出规则、条件、上一次与本次取值，以及产出的投递和它的状态；序列第一次出现明说没有上一次取值。
func TestRenderAlertReplay(t *testing.T) {
	out := render(kwhy.Timeline{ID: "sev_1", Alerts: []kwhy.Alert{
		{Rule: "p/source_rules/over", Disposition: "alerted", Conditions: json.RawMessage(`[{"condition":"metrics.value crosses_above 90","previous":70,"current":96}]`),
			Delivery: &kwhy.DeliveryRef{ID: "alert:sev_1", Class: "reminder", Route: "channel:acc", State: "delivered"}},
		{Rule: "p/source_rules/over", Disposition: "ignored", Conditions: json.RawMessage(`[{"condition":"metrics.value crosses_above 90","current":95}]`)},
	}})
	for _, want := range []string{"alert rule p/source_rules/over → alerted", "metrics.value crosses_above 90: previous 70, current 96",
		"delivery alert:sev_1 (reminder via channel:acc): delivered", "previous none (first appearance of the series), current 95"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}
