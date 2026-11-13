package packs

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const ownerPW = "correct horse battery"

const briefPack = `name: brief
version: 1.0.0
description: 周期简报
instance_params:
  type: object
  required: [schedule]
  additionalProperties: false
  properties:
    schedule: {type: string}
event_rules:
  - id: tag-hit
    mode: observe
    when:
      event: item.phase_changed
      to_phase: open
    do:
      - action: add_tag
        tag: hit
`

type env struct {
	t      *testing.T
	st     *store.Store
	au     *auth.Auth
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, instances.Module, rules.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ld := kp.New(kp.Options{Kinds: items.NewKinds(), Builtin: fstest.MapFS{
		"brief/pack.yaml":          {Data: []byte(briefPack)},
		"brief/views/brief.yaml":   {Data: []byte("kind: brief\nlist:\n  columns: [title, project, created_at]\n  filters: [project, tag]\ncard:\n  fields: [summary, spec.period]\n")},
		"brief/views/compact.yaml": {Data: []byte("kind: note\n")},
		"brief/prompts/ask.md":     {Data: []byte("---\nagent: judge\nquestion_type: yes_no\non_undecided: yes\n---\nPROMPT-BODY\n")},
		"brief/templates/note.md":  {Data: []byte("TEMPLATE-BODY\n")},
	}})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load packs: %v %v", err, rej)
	}
	x := command.New(st, nil)
	kinds := items.NewKinds()
	for _, f := range []func(*command.Executor) error{
		projects.Register, rules.Register,
		func(x *command.Executor) error { return instances.Register(x, ld) },
		func(x *command.Executor) error { return items.Register(x, kinds) },
	} {
		if err := f(x); err != nil {
			t.Fatal(err)
		}
	}
	eng, err := rules.New(rules.Options{Store: st, Exec: x, Packs: ld, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	au := auth.New(nil)
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{Routes(st, x, ld)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, au: au, srv: srv}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	e.csrf = out.CSRF
	return e
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set(core.EvidenceHeader, "owner: go ahead") // delegate 代理写需要证据；其他凭据不读
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

func (e *env) bearer(kind auth.Kind) func(*http.Request) {
	e.t.Helper()
	spec := auth.Spec{Kind: kind}
	if kind == auth.Ext || kind == auth.Webhook {
		spec.Scope = "x"
	}
	var secret string
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		_, s, err := e.au.Issue(tx, spec)
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) {
		r.Header.Set("Authorization", "Bearer "+secret)
		r.Header.Set("X-APA-Session", "s1")
		r.Header.Set("X-APA-Session-Kind", "direct")
	}
}

func decode[T any](t *testing.T, rec *httptest.ResponseRecorder) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("not json: %s", rec.Body)
	}
	return v
}

func (e *env) events(typ string) (out []domain.Event) {
	_ = e.st.View(context.Background(), func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			if x.Event.Type == typ {
				out = append(out, x.Event)
			}
		}
		return err
	})
	return
}

const ruleKey = "brief/event_rules/tag-hit"

var rulePath = "/v1/rules/" + url.PathEscape(ruleKey) + "/mode"

func TestPacksListsLoadedPack(t *testing.T) {
	e := setup(t)
	rec := e.do("GET", "/v1/packs", "", e.bearer(auth.Observer))
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	out := decode[struct{ Packs []packView }](t, rec)
	if len(out.Packs) != 1 || out.Packs[0].Name != "brief" || out.Packs[0].Source != kp.Builtin ||
		len(out.Packs[0].EventRules) != 1 || out.Packs[0].EventRules[0].ID != "tag-hit" || out.Packs[0].InstanceParams == nil {
		t.Fatalf("packs = %+v", out.Packs)
	}
	// views 的对外形状（附录 B.2）：未声明的列表是 []；提示词正文与模板不外露。
	body := rec.Body.String()
	want := `"views":{"brief":{"kind":"brief","list":{"columns":["title","project","created_at"],"filters":["project","tag"]},"card":{"fields":["summary","spec.period"]}},` +
		`"compact":{"kind":"note","list":{"columns":[],"filters":[]},"card":{"fields":[]}}}`
	if !strings.Contains(body, want) || strings.Contains(body, "PROMPT-BODY") || strings.Contains(body, "TEMPLATE-BODY") {
		t.Fatalf("views: %s", body)
	}
}

func TestReadsAreClosedToExtAndWebhookAndAnonymous(t *testing.T) {
	e := setup(t)
	for _, p := range []string{"/v1/packs", "/v1/instances", "/v1/rules"} {
		for name, mod := range map[string]func(*http.Request){"ext": e.bearer(auth.Ext), "webhook": e.bearer(auth.Webhook)} {
			if rec := e.do("GET", p, "", mod); rec.Code != 403 {
				t.Errorf("%s %s: %d", name, p, rec.Code)
			}
		}
		if rec := e.do("GET", p, "", nil); rec.Code != 401 {
			t.Errorf("anonymous %s: %d", p, rec.Code)
		}
	}
}

// flow-pack-loader：实例经 API 建立、读取、改参数、停用，参数按包声明校验。
func TestInstanceLifecycleThroughAPI(t *testing.T) {
	e := setup(t)
	dlg, obs := e.bearer(auth.Delegate), e.bearer(auth.Observer)

	if rec := e.do("GET", "/v1/instances", "", obs); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"instances":[]`) {
		t.Fatalf("empty list: %d %s", rec.Code, rec.Body)
	}
	rec := e.do("POST", "/v1/instances", `{"pack":"brief","params":{"schedule":"24h"}}`, dlg)
	if rec.Code != 201 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	in := decode[instances.Instance](t, rec)
	if !in.Enabled || in.Pack != "brief" || in.ID == "" {
		t.Fatalf("created = %+v", in)
	}

	// 参数不符包声明：400，且没有留下实例。
	if rec := e.do("POST", "/v1/instances", `{"pack":"brief","params":{"nope":1}}`, dlg); rec.Code != 400 {
		t.Fatalf("bad params: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/instances", `{"pack":"ghost","params":{"schedule":"1h"}}`, dlg); rec.Code != 400 {
		t.Fatalf("unknown pack: %d %s", rec.Code, rec.Body)
	}

	// 同时改参数并停用。
	rec = e.do("PATCH", "/v1/instances/"+in.ID, `{"params":{"schedule":"48h"},"enabled":false}`, dlg)
	if rec.Code != 200 {
		t.Fatalf("patch: %d %s", rec.Code, rec.Body)
	}
	got := decode[instances.Instance](t, rec)
	if got.Enabled || !strings.Contains(string(got.Params), "48h") {
		t.Fatalf("patched = %+v", got)
	}
	list := decode[struct{ Instances []instances.Instance }](t, e.do("GET", "/v1/instances", "", obs))
	if len(list.Instances) != 1 || list.Instances[0].Enabled {
		t.Fatalf("list = %+v", list.Instances)
	}

	if rec := e.do("PATCH", "/v1/instances/"+in.ID, `{}`, dlg); rec.Code != 400 {
		t.Fatalf("empty patch: %d", rec.Code)
	}
	if rec := e.do("PATCH", "/v1/instances/ins_missing", `{"enabled":true}`, dlg); rec.Code != 404 {
		t.Fatalf("missing: %d %s", rec.Code, rec.Body)
	}
}

func TestInstanceWritesNeedDelegate(t *testing.T) {
	e := setup(t)
	for name, mod := range map[string]func(*http.Request){"observer": e.bearer(auth.Observer), "worker": e.bearer(auth.Worker)} {
		if rec := e.do("POST", "/v1/instances", `{"pack":"brief","params":{"schedule":"24h"}}`, mod); rec.Code != 403 {
			t.Errorf("%s create: %d %s", name, rec.Code, rec.Body)
		}
	}
	// Web 本人可写，且写 Cookie 会话必须带 CSRF 令牌（骨干负责）。
	if rec := e.do("POST", "/v1/instances", `{"pack":"brief","params":{"schedule":"24h"}}`, e.web); rec.Code != 201 {
		t.Fatalf("web create: %d %s", rec.Code, rec.Body)
	}
}

// flow-rule-modes：规则状态可读；Web 会话切模式并记 rule.mode_changed。
func TestWebSessionSwitchesRuleModeAndRecordsEvent(t *testing.T) {
	e := setup(t)
	rec := e.do("GET", "/v1/rules", "", e.bearer(auth.Observer))
	list := decode[struct{ Rules []rules.State }](t, rec)
	if len(list.Rules) != 1 || list.Rules[0].Key != ruleKey || list.Rules[0].Mode != rules.ModeObserve {
		t.Fatalf("rules = %+v (%s)", list.Rules, rec.Body)
	}

	rec = e.do("PATCH", rulePath, `{"mode":"auto"}`, e.web)
	if rec.Code != 200 {
		t.Fatalf("switch: %d %s", rec.Code, rec.Body)
	}
	if st := decode[rules.State](t, rec); st.Mode != rules.ModeAuto || st.Key != ruleKey {
		t.Fatalf("state = %+v", st)
	}
	again := decode[struct{ Rules []rules.State }](t, e.do("GET", "/v1/rules", "", e.bearer(auth.Observer)))
	if again.Rules[0].Mode != rules.ModeAuto {
		t.Fatalf("mode not persisted: %+v", again.Rules)
	}
	evs := e.events(domain.EvRuleModeChanged)
	if len(evs) != 1 || evs[0].Actor.Via != domain.ViaWeb || !strings.Contains(string(evs[0].Payload), `"to":"auto"`) {
		t.Fatalf("rule.mode_changed = %+v", evs)
	}
}

// 非目标：没有把规则切 auto 的 token 路径。任何级别的 token 都是 web_only（403），模式不变、不留 mode_changed。
func TestNoTokenCanSwitchRuleMode(t *testing.T) {
	e := setup(t)
	for name, kind := range map[string]auth.Kind{"delegate": auth.Delegate, "worker": auth.Worker, "observer": auth.Observer} {
		for _, mode := range []string{"auto", "off"} {
			rec := e.do("PATCH", rulePath, `{"mode":"`+mode+`"}`, e.bearer(kind))
			if rec.Code != 403 {
				t.Fatalf("%s -> %s: %d %s", name, mode, rec.Code, rec.Body)
			}
		}
	}
	if rec := e.do("PATCH", rulePath, `{"mode":"auto"}`, nil); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	cur := decode[struct{ Rules []rules.State }](t, e.do("GET", "/v1/rules", "", e.bearer(auth.Observer)))
	if cur.Rules[0].Mode != rules.ModeObserve {
		t.Fatalf("mode changed by token: %+v", cur.Rules)
	}
	if n := len(e.events(domain.EvRuleModeChanged)); n != 0 {
		t.Fatalf("%d mode_changed events from rejected calls", n)
	}
}

func TestRuleModeFailureSemantics(t *testing.T) {
	e := setup(t)
	for name, c := range map[string]struct {
		path, body string
		want       int
	}{
		"bad mode":  {rulePath, `{"mode":"sometimes"}`, 400},
		"same mode": {rulePath, `{"mode":"observe"}`, 400},
		"unknown":   {"/v1/rules/" + url.PathEscape("brief/event_rules/nope") + "/mode", `{"mode":"off"}`, 404},
		"bad body":  {rulePath, `{"mode":"off"} x`, 400},
	} {
		if rec := e.do("PATCH", c.path, c.body, e.web); rec.Code != c.want {
			t.Errorf("%s: %d %s", name, rec.Code, rec.Body)
		}
	}
}

// 评审阻塞项：非法 enabled 不能让 params 先落地再报错。
func TestPatchInstanceBadEnabledChangesNothing(t *testing.T) {
	e := setup(t)
	dlg := e.bearer(auth.Delegate)
	in := decode[instances.Instance](t, e.do("POST", "/v1/instances", `{"pack":"brief","params":{"schedule":"24h"}}`, dlg))
	for _, bad := range []string{`"yes"`, `null`, `1`} {
		rec := e.do("PATCH", "/v1/instances/"+in.ID, `{"params":{"schedule":"48h"},"enabled":`+bad+`}`, dlg)
		if rec.Code != 400 {
			t.Fatalf("enabled=%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	list := decode[struct{ Instances []instances.Instance }](t, e.do("GET", "/v1/instances", "", dlg))
	if len(list.Instances) != 1 || !strings.Contains(string(list.Instances[0].Params), "24h") || !list.Instances[0].Enabled {
		t.Fatalf("instance changed despite 400: %+v", list.Instances)
	}
}
