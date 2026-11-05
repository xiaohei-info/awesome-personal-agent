package sources

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

type fakePacks struct{}

func (fakePacks) List() []*packs.Pack {
	return []*packs.Pack{{Name: "hooks", WebhookSchemes: []packs.WebhookScheme{
		{ID: "shared", SignatureHeader: "X-Hook-Key", Algorithm: "token", DeliveryIDHeader: "X-Delivery"},
	}}}
}

type noSecrets struct{}

func (noSecrets) Secrets(string) ([][]byte, error) { return nil, nil }

type env struct {
	t   *testing.T
	st  *store.Store
	au  *auth.Auth
	srv *core.Server
	wh  *webhook.Service
	now time.Time
}

func (e *env) clock() time.Time { return e.now }

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, core.Module, registry.Module, webhook.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)}
	x := command.New(st, e.clock)
	e.au = auth.New(e.clock)
	if err := registry.Register(x, registry.Options{Bundled: []string{"fetch-feed"}}); err != nil {
		t.Fatal(err)
	}
	e.wh, err = webhook.New(webhook.Options{Store: st, Exec: x, Packs: fakePacks{}, Auth: e.au, Secrets: noSecrets{}, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.wh.Wait)
	grp, err := Mount(st, x, e.au, e.wh)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	e.srv, err = core.New(core.Deps{Store: st, Auth: e.au, Now: e.clock, Groups: []core.RouteGroup{grp}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) token(kind auth.Kind) string {
	e.t.Helper()
	spec := auth.Spec{Kind: kind}
	if kind == auth.Ext || kind == auth.Webhook {
		spec.Scope = "src_other"
	}
	var secret string
	if err := e.st.Update(context.Background(), func(tx *store.Tx) (err error) { _, secret, err = e.au.Issue(tx, spec); return }); err != nil {
		e.t.Fatal(err)
	}
	return secret
}

type resp struct{ *httptest.ResponseRecorder }

func (r resp) into(v any) {
	if err := json.Unmarshal(r.Body.Bytes(), v); err != nil {
		panic("not json: " + r.Body.String())
	}
}

func (r resp) code() string {
	var m struct{ Code string }
	r.into(&m)
	return m.Code
}

func (e *env) do(secret, method, path, body string, hdr ...string) resp {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return resp{rec}
}

// web 以本人 Web 会话发请求。
func (e *env) web(method, path, body string) resp {
	e.t.Helper()
	login := httptest.NewRequest("POST", "/v1/auth/login", strings.NewReader(`{"password":"`+ownerPW+`"}`))
	login.RemoteAddr = "[::1]:50000"
	lrec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(lrec, login)
	if lrec.Code != 200 {
		e.t.Fatalf("login: %d %s", lrec.Code, lrec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	resp{lrec}.into(&out)
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.AddCookie(lrec.Result().Cookies()[0])
	req.Header.Set(core.CSRFHeader, out.CSRF)
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return resp{rec}
}

func (e *env) events(typ string) int {
	e.t.Helper()
	var n int64
	if err := e.st.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT COUNT(*) FROM eventlog_event WHERE type = ?`, typ)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			return r.Scan(&n)
		}
		return r.Err()
	}); err != nil {
		e.t.Fatal(err)
	}
	return int(n)
}

const (
	hook = `{"id":"src_hook","type":"webhook","source_type":"forge","config":{"scheme":"shared"}}`
	pull = `{"id":"src_feed","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`
)

// flow-source-contract：登记、读取、改、启停的契约，以及各凭据级别的权限。
func TestSourceRegistrationContract(t *testing.T) {
	e := setup(t)
	obs, wrk, dlg := e.token(auth.Observer), e.token(auth.Worker), e.token(auth.Delegate)

	if r := e.do("", "GET", "/v1/sources", ""); r.Code != 401 {
		t.Fatalf("anonymous: %d", r.Code)
	}
	if r := e.do(wrk, "POST", "/v1/sources", hook); r.Code != 403 {
		t.Fatalf("worker register: %d %s", r.Code, r.Body)
	}
	if r := e.do(obs, "POST", "/v1/sources", hook); r.Code != 403 {
		t.Fatalf("observer register: %d", r.Code)
	}
	if r := e.do(dlg, "POST", "/v1/sources", hook); r.Code != 201 {
		t.Fatalf("delegate webhook register: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", "/v1/sources", pull); r.Code != 201 {
		t.Fatalf("delegate bundled pull register: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", "/v1/sources", hook); r.Code != 409 {
		t.Fatalf("duplicate: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", "/v1/sources", `[1]`); r.Code != 400 {
		t.Fatalf("non-object: %d", r.Code)
	}
	if r := e.do(dlg, "POST", "/v1/sources", `null`); r.Code != 400 {
		t.Fatalf("null body: %d", r.Code)
	}

	var list struct{ Sources []struct{ ID, Type string } }
	lr := e.do(obs, "GET", "/v1/sources", "")
	lr.into(&list)
	if lr.Code != 200 || len(list.Sources) != 2 || list.Sources[0].ID != "src_feed" {
		t.Fatalf("list: %d %s", lr.Code, lr.Body)
	}
	if r := e.do(obs, "GET", "/v1/sources/src_hook", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"source_type":"forge"`) {
		t.Fatalf("get: %d %s", r.Code, r.Body)
	}
	if r := e.do(obs, "GET", "/v1/sources/src_nope", ""); r.Code != 404 {
		t.Fatalf("missing: %d", r.Code)
	}
	for _, k := range []auth.Kind{auth.Ext, auth.Webhook} {
		if r := e.do(e.token(k), "GET", "/v1/sources", ""); r.Code != 403 {
			t.Fatalf("%s credential must not read sources: %d", k, r.Code)
		}
		if r := e.do(e.token(k), "POST", "/v1/sources", pull); r.Code != 403 {
			t.Fatalf("%s credential must not register: %d", k, r.Code)
		}
	}

	// PATCH：改字段、启停、空体、未知字段、类型不可改
	if r := e.do(dlg, "PATCH", "/v1/sources/src_feed", `{"schedule":"1h","enabled":false}`); r.Code != 200 || !strings.Contains(r.Body.String(), `"schedule":"1h"`) || !strings.Contains(r.Body.String(), `"enabled":false`) {
		t.Fatalf("patch: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "PATCH", "/v1/sources/src_feed", `{"enabled":true}`); r.Code != 200 || !strings.Contains(r.Body.String(), `"enabled":true`) {
		t.Fatalf("enable only: %d %s", r.Code, r.Body)
	}
	for body, want := range map[string]int{`{}`: 400, `{"type":"script"}`: 400, `{"cursor":"x"}`: 400, `{"enabled":"yes"}`: 400} {
		if r := e.do(dlg, "PATCH", "/v1/sources/src_feed", body); r.Code != want {
			t.Fatalf("patch %s: %d %s", body, r.Code, r.Body)
		}
	}
	if r := e.do(obs, "PATCH", "/v1/sources/src_feed", `{"schedule":"2h"}`); r.Code != 403 {
		t.Fatalf("observer patch: %d", r.Code)
	}
	if r := e.do(dlg, "PATCH", "/v1/sources/src_nope", `{"schedule":"2h"}`); r.Code != 404 {
		t.Fatalf("patch missing: %d", r.Code)
	}
}

// flow-command-registration：自定义命令字段经 token 写入被拒绝，只有 Web 能登记。
func TestCustomCommandsRejectedForTokensAcceptedInWeb(t *testing.T) {
	e := setup(t)
	dlg, wrk := e.token(auth.Delegate), e.token(auth.Worker)
	script := `{"id":"src_mail","type":"script","command":"/opt/tool --json","schedule":"10m","timeout":30}`

	r := e.do(dlg, "POST", "/v1/sources", script)
	if r.Code != 403 || r.code() != "web_only" {
		t.Fatalf("delegate script register must be web_only: %d %s", r.Code, r.Body)
	}
	if r := e.do(wrk, "POST", "/v1/sources", script); r.Code != 403 || r.code() != "permission_denied" {
		t.Fatalf("worker script register: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", "/v1/sources", `{"id":"src_x","type":"pull","command":"/bin/sh -c evil","schedule":"1h","timeout":5}`); r.Code != 400 {
		t.Fatalf("non-bundled pull command: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", "/v1/sources", `{"id":"src_y","type":"webhook","command":"/bin/evil"}`); r.Code != 400 {
		t.Fatalf("webhook with command: %d %s", r.Code, r.Body)
	}
	if got := e.do(dlg, "GET", "/v1/sources", ""); strings.Contains(got.Body.String(), "src_mail") || strings.Contains(got.Body.String(), "src_x") {
		t.Fatalf("rejected registrations must not be stored: %s", got.Body)
	}
	if n := e.events("command.rejected"); n < 1 {
		t.Fatalf("web_only attempt must leave a trace, got %d command.rejected events", n)
	}

	if r := e.web("POST", "/v1/sources", script); r.Code != 201 {
		t.Fatalf("web script register: %d %s", r.Code, r.Body)
	}
	// 已有 script 来源：delegate 不能改命令，Web 可以；delegate 也不能把命令字段塞进 PATCH
	if r := e.do(dlg, "PATCH", "/v1/sources/src_mail", `{"command":"/opt/other"}`); r.Code != 403 || r.code() != "web_only" {
		t.Fatalf("delegate script patch: %d %s", r.Code, r.Body)
	}
	if r := e.web("PATCH", "/v1/sources/src_mail", `{"command":"/opt/other"}`); r.Code != 200 || !strings.Contains(r.Body.String(), "/opt/other") {
		t.Fatalf("web script patch: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "PATCH", "/v1/sources/src_mail", `{"enabled":false}`); r.Code != 200 {
		t.Fatalf("delegate may still disable a script source: %d %s", r.Code, r.Body)
	}
}

func receive(e *env, id, key, body, delivery string) webhook.Response {
	return e.wh.Receive(context.Background(), webhook.Request{SourceID: id, Header: map[string]string{"X-Hook-Key": key, "X-Delivery": delivery}, Body: []byte(body)})
}

// flow-webhook：创建密钥 → 接收 → 排查 → 重放 → 轮换。
func TestWebhookKeyIngestsAndReplay(t *testing.T) {
	e := setup(t)
	obs, wrk, dlg := e.token(auth.Observer), e.token(auth.Worker), e.token(auth.Delegate)
	e.do(dlg, "POST", "/v1/sources", hook)
	e.do(dlg, "POST", "/v1/sources", pull)

	for name, c := range map[string]struct {
		tok  string
		path string
		want int
	}{
		"observer": {obs, "/v1/sources/src_hook/webhook", 403},
		"worker":   {wrk, "/v1/sources/src_hook/webhook", 403},
		"missing":  {dlg, "/v1/sources/src_nope/webhook", 404},
		"pull":     {dlg, "/v1/sources/src_feed/webhook", 400},
	} {
		if r := e.do(c.tok, "POST", c.path, ""); r.Code != c.want {
			t.Fatalf("%s: %d %s", name, r.Code, r.Body)
		}
	}
	if n := len(receiveKeys(e)); n != 0 {
		t.Fatalf("failed attempts must not issue keys, got %d", n)
	}

	var k1 struct {
		Secret, URL string
		Rotated     bool
	}
	r := e.do(dlg, "POST", "/v1/sources/src_hook/webhook", "", "Idempotency-Key", "ignored")
	r.into(&k1)
	if r.Code != 201 || k1.Rotated || !strings.HasPrefix(k1.Secret, "apa_whk_") || k1.URL != "/ingest/src_hook" {
		t.Fatalf("create key: %d %s", r.Code, r.Body)
	}
	if e.events(EvWebhookCreated) != 1 || strings.Contains(eventPayloads(e), k1.Secret) {
		t.Fatalf("create must be recorded without the secret")
	}

	if got := receive(e, "src_hook", "wrong", `{}`, "d0"); got.Status != 401 {
		t.Fatalf("wrong key: %d", got.Status)
	}
	if got := receive(e, "src_hook", k1.Secret, `{"n":1}`, "d1"); got.Status != 200 {
		t.Fatalf("receive: %d %s", got.Status, got.Body)
	}
	e.wh.Wait()

	var ing struct {
		Ingests []struct {
			ID, State, Reason, Body string
			Gen                     int
			BodySize                int `json:"body_size"`
		}
	}
	lr := e.do(obs, "GET", "/v1/sources/src_hook/ingests", "")
	lr.into(&ing)
	// 第一条是错误密钥被拒绝的元数据记录（无 payload），第二条是正常接收。
	if lr.Code != 200 || len(ing.Ingests) != 2 || ing.Ingests[0].State != webhook.StateRejected || ing.Ingests[0].Reason != "token rejected" || ing.Ingests[0].Body != "" {
		t.Fatalf("ingests: %d %s", lr.Code, lr.Body)
	}
	ing.Ingests = ing.Ingests[1:]
	if ing.Ingests[0].Body != `{"n":1}` || ing.Ingests[0].BodySize != 7 || ing.Ingests[0].State != webhook.StateFailed || ing.Ingests[0].Gen != 1 {
		t.Fatalf("ingests: %d %s", lr.Code, lr.Body)
	}
	if strings.Contains(lr.Body.String(), k1.Secret) {
		t.Fatal("ingest view must not leak the receive key")
	}
	if r := e.do(obs, "GET", "/v1/sources/src_nope/ingests", ""); r.Code != 404 {
		t.Fatalf("ingests of missing source: %d", r.Code)
	}
	if r := e.do(e.token(auth.Webhook), "GET", "/v1/sources/src_hook/ingests", ""); r.Code != 403 {
		t.Fatalf("webhook credential reading ingests: %d", r.Code)
	}

	// 重放：delegate 可以，observer 不行；代数加一
	id := ing.Ingests[0].ID
	if r := e.do(obs, "POST", "/v1/ingests/"+id+"/replay", ""); r.Code != 403 {
		t.Fatalf("observer replay: %d", r.Code)
	}
	if r := e.do(dlg, "POST", "/v1/ingests/ing_nope/replay", ""); r.Code != 404 {
		t.Fatalf("replay missing: %d %s", r.Code, r.Body)
	}
	if e.events(EvReplayed) != 0 {
		t.Fatal("rejected or missing replays must leave no replay event")
	}
	var rp struct {
		ID    string
		Gen   int
		State string
	}
	rr := e.do(dlg, "POST", "/v1/ingests/"+id+"/replay", "")
	rr.into(&rp)
	if rr.Code != 200 || rp.ID != id || rp.Gen != 2 || e.events(EvReplayed) != 1 {
		t.Fatalf("replay: %d %s", rr.Code, rr.Body)
	}

	// 轮换：旧密钥宽限期内仍有效，期满失效；新密钥可用
	var k2 struct {
		Secret  string
		Rotated bool
	}
	r = e.do(dlg, "POST", "/v1/sources/src_hook/webhook", "")
	r.into(&k2)
	if r.Code != 200 || !k2.Rotated || k2.Secret == k1.Secret || e.events(EvWebhookRotated) != 1 {
		t.Fatalf("rotate: %d %s", r.Code, r.Body)
	}
	if got := receive(e, "src_hook", k1.Secret, `{"n":2}`, "d2"); got.Status != 200 {
		t.Fatalf("old key within grace: %d", got.Status)
	}
	if got := receive(e, "src_hook", k2.Secret, `{"n":3}`, "d3"); got.Status != 200 {
		t.Fatalf("new key: %d", got.Status)
	}
	e.now = e.now.Add(RotateGrace + time.Minute)
	if got := receive(e, "src_hook", k1.Secret, `{"n":4}`, "d4"); got.Status != 401 {
		t.Fatalf("old key after grace: %d", got.Status)
	}
	// 再轮换一次：轮换的是新密钥，不会连带复活旧的
	var k3 struct{ Secret string }
	r = e.do(dlg, "POST", "/v1/sources/src_hook/webhook", "")
	r.into(&k3)
	if r.Code != 200 || receive(e, "src_hook", k2.Secret, `{"n":5}`, "d5").Status != 200 || receive(e, "src_hook", k3.Secret, `{"n":6}`, "d6").Status != 200 {
		t.Fatalf("second rotate: %d %s", r.Code, r.Body)
	}
	e.wh.Wait()
}

func TestIngestViewEncodesBinaryAndPurged(t *testing.T) {
	v := viewOf(webhook.Ingest{ID: "i", Body: []byte{0xff, 0xfe}, BodySize: 2})
	if v.Body != nil || v.BodyBase64 != "//4=" || v.Headers == nil {
		t.Fatalf("binary payload: %+v", v)
	}
	v = viewOf(webhook.Ingest{ID: "i", Body: nil, BodySize: 9, Purged: true})
	if v.Body != nil || v.BodyBase64 != "" || !v.Purged {
		t.Fatalf("purged payload: %+v", v)
	}
}

func receiveKeys(e *env) []auth.Credential {
	var out []auth.Credential
	_ = e.st.View(context.Background(), func(tx *store.Tx) error {
		all, err := e.au.List(tx)
		for _, c := range all {
			if c.Kind == auth.Webhook && c.Scope == "src_hook" {
				out = append(out, c)
			}
		}
		return err
	})
	return out
}

func eventPayloads(e *env) string {
	var sb strings.Builder
	_ = e.st.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT payload FROM eventlog_event`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var p string
			_ = r.Scan(&p)
			sb.WriteString(p)
		}
		return r.Err()
	})
	return sb.String()
}
