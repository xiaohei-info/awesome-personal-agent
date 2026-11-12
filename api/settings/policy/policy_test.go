package policy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

var bg = context.Background()

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	srv    *core.Server
	cookie *http.Cookie
	csrf   string
}

// setup 装配真实账本与 HTTP 服务，并以本人口令登录一个 Web 会话。
func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, kpolicy.Module, command.Module, core.Module} {
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
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(st)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, auth: a, srv: srv}
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
	var secret string
	scope := ""
	if kind == auth.Ext || kind == auth.Webhook {
		scope = "x"
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

func errCode(t *testing.T, rec *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var e domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return e.Code
}

func (e *env) current() (int, kpolicy.Policy) {
	e.t.Helper()
	var (
		v int
		p kpolicy.Policy
	)
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { v, p, err = kpolicy.Current(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return v, p
}

func (e *env) eventTypes() []string {
	e.t.Helper()
	var out []string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 100)
		for _, x := range es {
			if strings.HasPrefix(x.Event.Type, "auth.") { // 登录本身的审计事件，与被测的设置变更无关
				continue
			}
			out = append(out, string(x.Event.Type))
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

var routes = []string{"/v1/settings/policy", "/v1/settings/allowlists", "/v1/settings/model-endpoints", "/v1/settings/access-limits"}

// flow-web-only：所有设置路由，非 Web 凭据（含 observer/worker/delegate/ext/webhook、无凭据）读写都被拒，且不写任何版本。
func TestSettingsRejectNonWebCallers(t *testing.T) {
	e := setup(t)
	for _, kind := range []auth.Kind{auth.Observer, auth.Worker, auth.Delegate, auth.Ext, auth.Webhook} {
		mod := e.bearer(kind)
		for _, p := range routes {
			for _, m := range []string{"GET", "PUT"} {
				rec := e.do(m, p, `{"persona":"x"}`, mod)
				if rec.Code != 403 || errCode(t, rec) != domain.CodeWebOnly {
					t.Fatalf("%s %s as %s: %d %s", m, p, kind, rec.Code, rec.Body)
				}
			}
		}
	}
	if rec := e.do("PUT", routes[0], `{"persona":"x"}`, nil); rec.Code != 401 {
		t.Fatalf("anonymous: %d", rec.Code)
	}
	// Cookie 写请求缺 CSRF 令牌同样被骨干拒绝。
	if rec := e.do("PUT", routes[0], `{"persona":"x"}`, func(r *http.Request) { r.AddCookie(e.cookie) }); rec.Code != 403 {
		t.Fatalf("no csrf: %d", rec.Code)
	}
	if v, _ := e.current(); v != 0 {
		t.Fatalf("rejected calls wrote version %d", v)
	}
}

// ACT-WEB-01：Web 上配置 web_only 规则生成新版本，事件流有 policy.changed，默认类别仍在。
func TestWebWritesNewPolicyVersionAndEvent(t *testing.T) {
	e := setup(t)
	rec := e.do("GET", routes[0], "", e.web)
	var got struct {
		Version int `json:"version"`
		WebOnly struct {
			Categories []string `json:"categories"`
		} `json:"web_only"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got.Version != 0 || len(got.WebOnly.Categories) != 5 {
		t.Fatalf("initial GET: %d %s", rec.Code, rec.Body)
	}

	body := `{"web_only":{"categories":["funds","external_commitment","irreversible","permission_change","spec_activation"],"rules":[{"tag":"acc-finance","category":"funds"}]}}`
	rec = e.do("PUT", routes[0], body, e.web)
	if rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	v, p := e.current()
	if v != 1 || len(p.WebOnly.Categories) != 5 || len(p.WebOnly.Rules) != 1 || p.WebOnly.Rules[0].Tag != "acc-finance" {
		t.Fatalf("stored v%d %+v", v, p.WebOnly)
	}
	if ev := e.eventTypes(); len(ev) != 1 || ev[0] != string(domain.EvPolicyChanged) {
		t.Fatalf("events %v", ev)
	}
	// 新版本被授权内核实际采用：该 tag 的决策属于 web_only。
	if !authz.WebOnly(p, authz.Object{Tags: []string{"acc-finance"}}) {
		t.Fatal("authz does not see the new rule")
	}
	// 同内容再 PUT 不产生新版本。
	e.do("PUT", routes[0], body, e.web)
	if v2, _ := e.current(); v2 != 1 {
		t.Fatalf("no-op PUT made version %d", v2)
	}
}

// 未出现的段不动；persona 写入后可读回。
func TestPartialPutKeepsOtherSections(t *testing.T) {
	e := setup(t)
	e.do("PUT", routes[0], `{"material_write":{"default":"direct","kinds":{}}}`, e.web)
	rec := e.do("PUT", routes[0], `{"persona":"简洁、直接"}`, e.web)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	v, p := e.current()
	if v != 2 || p.Persona != "简洁、直接" || p.MaterialWrite.Default != "direct" {
		t.Fatalf("v%d persona=%q mw=%+v", v, p.Persona, p.MaterialWrite)
	}
	rec = e.do("GET", routes[0], "", e.web)
	if !strings.Contains(rec.Body.String(), "简洁、直接") {
		t.Fatalf("GET lacks persona: %s", rec.Body)
	}
}

// 人格超长被拒（§11.2），错误为 invalid_request，且不写版本。
func TestPersonaTooLongRejected(t *testing.T) {
	e := setup(t)
	long, _ := json.Marshal(map[string]string{"persona": strings.Repeat("好", kpolicy.MaxPersonaTokens+1)})
	rec := e.do("PUT", routes[0], string(long), e.web)
	if rec.Code != 400 || errCode(t, rec) != domain.CodeInvalidRequest || strings.Contains(rec.Body.String(), "好好") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	ok, _ := json.Marshal(map[string]string{"persona": strings.Repeat("好", kpolicy.MaxPersonaTokens)})
	if rec := e.do("PUT", routes[0], string(ok), e.web); rec.Code != 200 {
		t.Fatalf("limit-sized persona: %d %s", rec.Code, rec.Body)
	}
	if v, _ := e.current(); v != 1 {
		t.Fatalf("version %d", v)
	}
}

// 请求体形态错误：未知段、别的路由的段、段内未知字段、空体、尾随内容、非法取值。
func TestMalformedBodies(t *testing.T) {
	e := setup(t)
	for _, c := range []struct{ path, body string }{
		{routes[0], `{"nope":1}`},
		{routes[0], `{"forward_allowlist":{}}`}, // 属于 allowlists
		{routes[0], `{"web_only":{"categories":["funds"],"bogus":1}}`},
		{routes[0], `{}`},
		{routes[0], `null`},
		{routes[0], `{"persona":"a"} {}`},
		{routes[0], `{"persona":5}`},
		{routes[0], `{"web_only":{"categories":[]}}`},
		{routes[2], `{"model_endpoint":{"url":"ftp://x","screening_timeout_ms":10}}`},
		{routes[3], `{"permission_cap":{"default":"root","by_harness":{}}}`},
	} {
		rec := e.do("PUT", c.path, c.body, e.web)
		if rec.Code != 400 || errCode(t, rec) != domain.CodeInvalidRequest {
			t.Errorf("%s %s: %d %s", c.path, c.body, rec.Code, rec.Body)
		}
	}
	if v, _ := e.current(); v != 0 {
		t.Fatalf("bad bodies wrote version %d", v)
	}
}

// flow-forward-allowlist：Web 把转发白名单设为 alice，内核转发判定据此放行/拒绝；渠道回答人白名单独立；记 allowlist.changed。
func TestAllowlistsDriveKernelForwardDecision(t *testing.T) {
	e := setup(t)
	rec := e.do("PUT", routes[1], `{"forward_allowlist":{"telegram":[" alice ","alice"]},"channel_answerers":{"telegram":["bob"]}}`, e.web)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	_, p := e.current()
	if !authz.ForwardAllowed(p, "telegram", "alice", true) || authz.ForwardAllowed(p, "telegram", "bob", true) ||
		authz.ForwardAllowed(p, "telegram", "alice", false) {
		t.Fatal("kernel forward decision does not follow the saved allowlist")
	}
	if got := p.ForwardAllowlist["telegram"]; len(got) != 1 || got[0] != "alice" {
		t.Fatalf("not normalised: %v", got)
	}
	if got := p.ChannelAnswerers["telegram"]; len(got) != 1 || got[0] != "bob" {
		t.Fatalf("answerers: %v", got)
	}
	ev := e.eventTypes()
	if len(ev) != 2 || ev[0] != string(domain.EvPolicyChanged) || ev[1] != string(domain.EvAllowlistChanged) {
		t.Fatalf("events %v", ev)
	}
	// 只改回答人时不影响转发白名单。
	e.do("PUT", routes[1], `{"channel_answerers":{}}`, e.web)
	if _, p = e.current(); len(p.ForwardAllowlist["telegram"]) != 1 || len(p.ChannelAnswerers) != 0 {
		t.Fatalf("partial put: %+v", p)
	}
}

// 运行权限上限：Web 设置后读回；model-endpoints：API 密钥写入账本但读取和事件里都不出现。
func TestAccessLimitsAndModelEndpointSecret(t *testing.T) {
	e := setup(t)
	rec := e.do("PUT", routes[3], `{"permission_cap":{"default":"read","by_harness":{"hermes":"edit"}}}`, e.web)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"hermes":"edit"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}

	rec = e.do("PUT", routes[2], `{"generate_endpoint":{"url":"https://m.example/v1","model":"m","api_key":"sk-secret"},"screening_timeout_ms":5000}`, e.web)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "sk-secret") {
		t.Fatalf("PUT leaked key: %d %s", rec.Code, rec.Body)
	}
	rec = e.do("GET", routes[2], "", e.web)
	if strings.Contains(rec.Body.String(), "sk-secret") || !strings.Contains(rec.Body.String(), `"api_key_set":true`) {
		t.Fatalf("GET: %s", rec.Body)
	}
	// 不带 api_key 或回传占位保持原密钥，只改模型名。
	e.do("PUT", routes[2], `{"generate_endpoint":{"url":"https://m.example/v1","model":"m2"}}`, e.web)
	e.do("PUT", routes[2], `{"generate_endpoint":{"url":"https://m.example/v1","model":"m2","api_key":"***"}}`, e.web)
	if _, p := e.current(); p.GenerateEndpoint.APIKey != "sk-secret" || p.GenerateEndpoint.Model != "m2" {
		t.Fatalf("endpoint %+v", p.GenerateEndpoint)
	}
	// 事件载荷里没有明文密钥。
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 100)
		for _, x := range es {
			if strings.Contains(string(x.Event.Payload), "sk-secret") {
				t.Errorf("event leaks key: %s", x.Event.Payload)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 路由登记不冲突、响应带 no-store（骨干提供）；超时配置字段保持默认。
func TestDefaultsReadable(t *testing.T) {
	e := setup(t)
	for _, p := range routes {
		rec := e.do("GET", p, "", e.web)
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("%s: %d %v", p, rec.Code, rec.Header())
		}
	}
}

// 渠道属性（§10.3）属于 /settings/policy，只有 Web 能改，未知字段被拒绝。
func TestChannelsSection(t *testing.T) {
	e := setup(t)
	if rec := e.do("PUT", routes[0], `{"channels":{"chat":{"text_only":true}}}`, e.web); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); !p.Channels["chat"].TextOnly {
		t.Fatalf("not saved: %+v", p.Channels)
	}
	if rec := e.do("PUT", routes[0], `{"channels":{"chat":{"bogus":true}}}`, e.web); rec.Code != 400 {
		t.Fatalf("unknown field: %d", rec.Code)
	}
	if rec := e.do("GET", routes[0], "", e.web); !strings.Contains(rec.Body.String(), `"channels":{"chat":{"text_only":true}}`) {
		t.Fatalf("read back: %s", rec.Body)
	}
}

// ACT-DLV-15：缺省日上限 20、静音为空、followup_defer 为 false。
func TestDisturbDefaultsCapAndMutes(t *testing.T) {
	e := setup(t)
	rec := e.do("GET", routes[0], "", e.web)
	var got struct {
		Disturb map[string]json.RawMessage `json:"disturb"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if string(got.Disturb["daily_cap"]) != "20" || string(got.Disturb["mutes"]) != "[]" || string(got.Disturb["followup_defer"]) != "false" {
		t.Fatalf("disturb defaults: %s", rec.Body)
	}
}

// ACT-DLV-16：日上限与静音只能 Web 改（委派级请求被拒、策略不变）；Web 改产生新版本与 policy.changed。
func TestDailyCapAndMutesAreWebOnly(t *testing.T) {
	e := setup(t)
	body := `{"disturb":{"routes":{"decision":"web","brief":"web","reminder":"web","health":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true,"daily_cap":0,"mutes":[{"scope":"project","id":"p"}],"followup_defer":false}}`
	rec := e.do("PUT", routes[0], body, e.bearer(auth.Delegate))
	if rec.Code != 403 || errCode(t, rec) != domain.CodeWebOnly {
		t.Fatalf("delegate PUT: %d %s", rec.Code, rec.Body)
	}
	if v, p := e.current(); v != 0 || p.Disturb.DailyCap != 20 || len(p.Disturb.Mutes) != 0 {
		t.Fatalf("policy changed by a rejected call: v%d %+v", v, p.Disturb)
	}
	until := "2030-01-01T00:00:00Z"
	rec = e.do("PUT", routes[0], `{"disturb":{"routes":{"decision":"inject","brief":"inject","reminder":"inject","health":"web","wake":"web"},"merge_window_sec":1800,"remind_on_deadline":true,"daily_cap":3,"mutes":[{"scope":"item","id":"x","until":"`+until+`"}]}}`, e.web)
	if rec.Code != 200 {
		t.Fatalf("web PUT: %d %s", rec.Code, rec.Body)
	}
	v, p := e.current()
	if v != 1 || p.Disturb.DailyCap != 3 || len(p.Disturb.Mutes) != 1 || p.Disturb.Mutes[0].Until == nil {
		t.Fatalf("stored v%d %+v", v, p.Disturb)
	}
	if ev := e.eventTypes(); len(ev) != 2 || ev[0] != string(domain.EvCommandRejected) || ev[1] != string(domain.EvPolicyChanged) {
		t.Fatalf("events %v", ev)
	}
}

// 请求体缺 daily_cap 取缺省 20 而不是"不限"；显式 0 才是不限；非法静音与负数被拒。
func TestDisturbPutDefaultsAndValidation(t *testing.T) {
	e := setup(t)
	routes := `"routes":{"decision":"inject","brief":"inject","reminder":"inject","health":"web","wake":"web"}`
	if rec := e.do("PUT", "/v1/settings/policy", `{"disturb":{`+routes+`,"merge_window_sec":60}}`, e.web); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.Disturb.DailyCap != 20 {
		t.Fatalf("missing daily_cap must default to 20, got %d", p.Disturb.DailyCap)
	}
	e.do("PUT", "/v1/settings/policy", `{"disturb":{`+routes+`,"merge_window_sec":60,"daily_cap":0}}`, e.web)
	if _, p := e.current(); p.Disturb.DailyCap != 0 {
		t.Fatalf("explicit 0 must mean unlimited, got %d", p.Disturb.DailyCap)
	}
	for _, bad := range []string{`"daily_cap":-1`, `"mutes":[{"scope":"team","id":"x"}]`, `"mutes":[{"scope":"item","id":""}]`} {
		rec := e.do("PUT", "/v1/settings/policy", `{"disturb":{`+routes+`,"merge_window_sec":60,`+bad+`}}`, e.web)
		if rec.Code != 400 || errCode(t, rec) != domain.CodeInvalidRequest {
			t.Fatalf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
}

func (e *env) endpoints(body string) map[string]json.RawMessage {
	e.t.Helper()
	rec := e.do("PUT", routes[2], body, e.web)
	if rec.Code != 200 {
		e.t.Fatalf("PUT %s: %d %s", body, rec.Code, rec.Body)
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// flow-endpoint-config / flow-outbound-scope：两个端点分别配置，未声明的位置返回 external，缺省字段取默认值，两者都可清空为 null。
func TestSplitEndpointsDefaultsAndNull(t *testing.T) {
	e := setup(t)
	out := e.endpoints(`{"judge_endpoint":{"url":"http://cls.example/j","protocol":"probability-judgment/1"},"generate_endpoint":{"url":"http://gen.example/v1","model":"m"}}`)
	var j, g map[string]any
	_ = json.Unmarshal(out["judge_endpoint"], &j)
	_ = json.Unmarshal(out["generate_endpoint"], &g)
	if j["locality"] != "external" || g["locality"] != "external" || j["timeout_ms"] != 10000.0 || j["max_batch"] != 32.0 ||
		j["max_material_chars"] != 8000.0 || string(out["screening_timeout_ms"]) != "30000" {
		t.Fatalf("judge=%v generate=%v out=%s", j, g, out["screening_timeout_ms"])
	}
	out = e.endpoints(`{"judge_endpoint":null,"generate_endpoint":null}`)
	if string(out["judge_endpoint"]) != "null" || string(out["generate_endpoint"]) != "null" {
		t.Fatalf("not cleared: %s %s", out["judge_endpoint"], out["generate_endpoint"])
	}
	for _, bad := range []string{`{"judge_endpoint":{"url":""}}`, `{"generate_endpoint":{"url":"ftp://x"}}`, `{"judge_endpoint":{"url":"http://x","locality":"near"}}`,
		`{"generate_endpoint":{"url":"http://x","bogus":1}}`} {
		if rec := e.do("PUT", routes[2], bad, e.web); rec.Code != 400 {
			t.Errorf("%s accepted: %d", bad, rec.Code)
		}
	}
}

// flow-endpoint-config / flow-upgrade-rollback：旧账本里的 model_endpoint 读作生成端点；旧形状 PUT 仍接受且不动判断端点；
// 本人保存后写成新字段，旧版本逐字节不变。
func TestLegacyShapeReadAndPut(t *testing.T) {
	e := setup(t)
	const legacy = `{"web_only":{"categories":["funds"],"rules":[]},"disturb":{"routes":{"decision":"inject"},"merge_window_sec":0,"remind_on_deadline":true},` +
		`"material_write":{"default":"proposed","kinds":{}},"permission_cap":{"default":"read","by_harness":{}},` +
		`"model_endpoint":{"url":"http://old.example/v1","model":"old-m","api_key":"sk-old","screening_timeout_ms":45000},"persona":""}`
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO policy_version (version, doc, at) VALUES (1, ?, 1)", legacy)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rec := e.do("GET", routes[2], "", e.web)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"url":"http://old.example/v1"`) || !strings.Contains(rec.Body.String(), `"locality":"external"`) ||
		!strings.Contains(rec.Body.String(), `"judge_endpoint":null`) || !strings.Contains(rec.Body.String(), `"screening_timeout_ms":45000`) ||
		strings.Contains(rec.Body.String(), "model_endpoint") || strings.Contains(rec.Body.String(), "sk-old") {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body)
	}
	if v, _ := e.current(); v != 1 {
		t.Fatalf("read created a version: %d", v)
	}

	// 先设判断端点 J，再用旧形状 PUT：生成端点按所给值（external），超时提到顶层，J 不变，旧密钥可经占位保留。
	e.endpoints(`{"judge_endpoint":{"url":"http://cls.example/j","api_key":"sk-j"}}`)
	out := e.endpoints(`{"model_endpoint":{"url":"http://stub.example/v1","model":"acc-model","api_key":"k1","screening_timeout_ms":45000}}`)
	var g map[string]any
	_ = json.Unmarshal(out["generate_endpoint"], &g)
	if g["url"] != "http://stub.example/v1" || g["model"] != "acc-model" || g["locality"] != "external" || string(out["screening_timeout_ms"]) != "45000" ||
		!strings.Contains(string(out["judge_endpoint"]), "http://cls.example/j") {
		t.Fatalf("generate=%v out=%v", g, out)
	}
	if _, p := e.current(); p.GenerateEndpoint.APIKey != "k1" || p.JudgeEndpoint.APIKey != "sk-j" {
		t.Fatalf("policy %+v", p)
	}
	// 旧形状与新段混用有歧义，拒绝。
	if rec := e.do("PUT", routes[2], `{"model_endpoint":{"url":"","model":"","api_key":"","screening_timeout_ms":1},"judge_endpoint":null}`, e.web); rec.Code != 400 {
		t.Fatalf("mixed shapes accepted: %d", rec.Code)
	}
	// 旧版本文档逐字节不变，新版本里没有 model_endpoint。
	var docs []string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query("SELECT doc FROM policy_version ORDER BY version")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var d string
			if err := r.Scan(&d); err != nil {
				return err
			}
			docs = append(docs, d)
		}
		return r.Err()
	}); err != nil {
		t.Fatal(err)
	}
	if len(docs) != 3 || docs[0] != legacy || strings.Contains(docs[1], "model_endpoint") || strings.Contains(docs[2], "model_endpoint") {
		t.Fatalf("docs = %v", docs)
	}
}

// flow-endpoint-config：本人在设置页不改任何值直接保存旧账本，仍追加一个新版本（新字段、密钥保留）；再保存一次无变化不加版本。
func TestSaveUnchangedOverLegacyWritesNewVersion(t *testing.T) {
	e := setup(t)
	const legacy = `{"web_only":{"categories":["funds"],"rules":[]},"disturb":{"routes":{"decision":"inject"},"merge_window_sec":0,"remind_on_deadline":true},` +
		`"material_write":{"default":"proposed","kinds":{}},"permission_cap":{"default":"read","by_harness":{}},` +
		`"model_endpoint":{"url":"http://old.example/v1","model":"old-m","api_key":"sk-old","screening_timeout_ms":45000},"persona":""}`
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO policy_version (version, doc, at) VALUES (1, ?, 1)", legacy)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	same := `{"judge_endpoint":null,"generate_endpoint":{"url":"http://old.example/v1","model":"old-m","locality":"external"},"screening_timeout_ms":45000}`
	var out map[string]json.RawMessage
	for i, want := range []string{"2", "2"} {
		rec := e.do("PUT", routes[2], same, e.web)
		if rec.Code != 200 {
			t.Fatalf("save %d: %d %s", i, rec.Code, rec.Body)
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		if string(out["version"]) != want {
			t.Fatalf("save %d: version %s, want %s", i, out["version"], want)
		}
	}
	if _, p := e.current(); p.GenerateEndpoint.APIKey != "sk-old" {
		t.Fatalf("api key lost: %+v", p.GenerateEndpoint)
	}
}
