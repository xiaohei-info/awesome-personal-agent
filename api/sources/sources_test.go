package sources

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

type fakePacks struct{}

func (fakePacks) List() []*packs.Pack {
	return []*packs.Pack{{Name: "hooks", WebhookSchemes: []packs.WebhookScheme{
		{ID: "shared", SignatureHeader: "X-Hook-Key", Algorithm: "token", DeliveryIDHeader: "X-Delivery"},
		{ID: "signed", SignatureHeader: "X-Sig", SignaturePrefix: "sha256=", Algorithm: "hmac-sha256", DeliveryIDHeader: "X-Delivery"},
	}}}
}

// noJudge：转建议不调判断型。
type noJudge struct{}

func (noJudge) Do(context.Context, direct.Call) (direct.Outcome, error) {
	panic("judge must not be called")
}

type env struct {
	t    *testing.T
	st   *store.Store
	au   *auth.Auth
	srv  *core.Server
	wh   *webhook.Service
	x    *command.Executor
	now  time.Time
	dir  string // 账本目录
	keys string // HMAC 接收密钥文件目录
}

func (e *env) clock() time.Time { return e.now }

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, core.Module, registry.Module, webhook.Module,
		projects.Module, clock.Module, items.Module, runner.Module, rules.Module, proc.Module, runidentity.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	dir := t.TempDir()
	st, err := store.Open(dir, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC), dir: dir, keys: filepath.Join(t.TempDir(), "webhook-keys")}
	x := command.New(st, e.clock)
	e.x = x
	e.au = auth.New(e.clock)
	if err := registry.Register(x, registry.Options{Bundled: []string{"fetch-feed"}}); err != nil {
		t.Fatal(err)
	}
	e.wh, err = webhook.New(webhook.Options{Store: st, Exec: x, Packs: fakePacks{}, Auth: e.au, Secrets: webhook.KeyFiles{Dir: e.keys, Auth: e.au}, Now: e.clock})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.wh.Wait)
	if err := items.Register(x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	in, err := inbound.New(inbound.Options{Store: st, Exec: x, Packs: fakePacks{}, Judge: noJudge{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Register(x); err != nil {
		t.Fatal(err)
	}
	grp, err := Mount(st, x, e.au, e.wh, in)
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
	req.Header.Set(core.EvidenceHeader, "owner: go ahead") // delegate 代理写需要证据；其他凭据不读
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
		req.Header.Set("X-APA-Session", "s1")
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

func signedReceive(e *env, key, body, delivery string) webhook.Response {
	m := hmac.New(sha256.New, []byte(key))
	m.Write([]byte(body))
	return e.wh.Receive(context.Background(), webhook.Request{SourceID: "src_signed",
		Header: map[string]string{"X-Sig": "sha256=" + hex.EncodeToString(m.Sum(nil)), "X-Delivery": delivery}, Body: []byte(body)})
}

// flow-webhook-hmac（§14.2）：HMAC 方案的接收密钥明文只在 0600 密钥文件里，签名按它校验；
// 账本、事件和任何 API 都读不回明文；轮换后新旧并存，宽限期满旧的失效并从文件里删掉。共享 token 方案不留明文。
func TestHMACWebhookKeyIsKeptOnlyInKeyFile(t *testing.T) {
	e := setup(t)
	obs, dlg := e.token(auth.Observer), e.token(auth.Delegate)
	e.do(dlg, "POST", "/v1/sources", hook)
	if r := e.do(dlg, "POST", "/v1/sources", `{"id":"src_signed","type":"webhook","source_type":"forge","config":{"scheme":"signed"}}`); r.Code != 201 {
		t.Fatalf("register: %d %s", r.Code, r.Body)
	}
	if r := e.do(dlg, "POST", "/v1/sources/src_hook/webhook", ""); r.Code != 201 {
		t.Fatalf("token key: %d %s", r.Code, r.Body)
	}
	if _, err := os.Stat(filepath.Join(e.keys, "src_hook")); !os.IsNotExist(err) {
		t.Fatalf("shared-token sources keep only the hash, key file: %v", err)
	}

	var k1, k2 struct{ Secret string }
	e.do(dlg, "POST", "/v1/sources/src_signed/webhook", "").into(&k1)
	file := filepath.Join(e.keys, "src_signed")
	fi, err := os.Stat(file)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file must exist with mode 0600: %v %v", fi, err)
	}
	if di, _ := os.Stat(e.keys); di.Mode().Perm() != 0o700 {
		t.Fatalf("key dir mode: %v", di.Mode())
	}

	if got := signedReceive(e, k1.Secret, `{"n":1}`, "s1"); got.Status != 200 {
		t.Fatalf("valid signature: %d %s", got.Status, got.Body)
	}
	if got := signedReceive(e, "apa_whk_wrong", `{"n":2}`, "s2"); got.Status != 401 {
		t.Fatalf("wrong key: %d", got.Status)
	}
	tampered := e.wh.Receive(context.Background(), webhook.Request{SourceID: "src_signed",
		Header: map[string]string{"X-Sig": "sha256=00", "X-Delivery": "s3"}, Body: []byte(`{}`)})
	if tampered.Status != 401 {
		t.Fatalf("bad signature: %d", tampered.Status)
	}
	// 共享 token 方案的凭据不能拿来当 HMAC 密钥，反之亦然
	if got := receive(e, "src_signed", k1.Secret, `{}`, "s4"); got.Status != 401 {
		t.Fatalf("hmac key sent as a token: %d", got.Status)
	}

	e.do(dlg, "POST", "/v1/sources/src_signed/webhook", "").into(&k2)
	if signedReceive(e, k1.Secret, `{"n":5}`, "s5").Status != 200 || signedReceive(e, k2.Secret, `{"n":6}`, "s6").Status != 200 {
		t.Fatal("old and new keys must both verify within the grace period")
	}
	e.now = e.now.Add(RotateGrace + time.Minute)
	if got := signedReceive(e, k1.Secret, `{"n":7}`, "s7"); got.Status != 401 {
		t.Fatalf("old key after grace: %d", got.Status)
	}
	e.do(dlg, "POST", "/v1/sources/src_signed/webhook", "")
	if b, _ := os.ReadFile(file); strings.Contains(string(b), k1.Secret) || !strings.Contains(string(b), k2.Secret) {
		t.Fatalf("rotation must drop expired keys and keep the valid one: %q", b)
	}
	e.wh.Wait()

	// 读不回：来源、入站记录、事件、账本文件里都没有明文
	for _, p := range []string{"/v1/sources", "/v1/sources/src_signed", "/v1/sources/src_signed/ingests"} {
		if r := e.do(obs, "GET", p, ""); r.Code != 200 || strings.Contains(r.Body.String(), k1.Secret) || strings.Contains(r.Body.String(), k2.Secret) {
			t.Fatalf("%s leaks the key: %d", p, r.Code)
		}
	}
	if ev := eventPayloads(e); strings.Contains(ev, k1.Secret) || strings.Contains(ev, k2.Secret) {
		t.Fatal("events must not carry the key")
	}
	if err := filepath.WalkDir(e.dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err == nil && (bytes.Contains(b, []byte(k1.Secret)) || bytes.Contains(b, []byte(k2.Secret))) {
			t.Errorf("ledger file %s contains the plaintext key", filepath.Base(p))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 吊销接收密钥：账本里的凭据全部吊销，同一事务里删掉 HMAC 明文文件；删不掉则整体回滚，账本不变。
func TestRevokeWebhookKeyDeletesKeyFile(t *testing.T) {
	e := setup(t)
	obs, dlg := e.token(auth.Observer), e.token(auth.Delegate)
	e.do(dlg, "POST", "/v1/sources", `{"id":"src_signed","type":"webhook","source_type":"forge","config":{"scheme":"signed"}}`)
	var k1, k2 struct{ Secret string }
	e.do(dlg, "POST", "/v1/sources/src_signed/webhook", "").into(&k1)
	e.do(dlg, "POST", "/v1/sources/src_signed/webhook", "").into(&k2) // 轮换：宽限期内两把都有效
	file := filepath.Join(e.keys, "src_signed")

	if r := e.do(obs, "DELETE", "/v1/sources/src_signed/webhook", ""); r.Code == 200 {
		t.Fatal("observer must not revoke receive keys")
	}
	if os.Geteuid() != 0 { // root 不受目录权限限制，无法制造删除失败
		if err := os.Chmod(e.keys, 0o500); err != nil {
			t.Fatal(err)
		}
		r := e.do(dlg, "DELETE", "/v1/sources/src_signed/webhook", "")
		_ = os.Chmod(e.keys, 0o700)
		if r.Code == 200 {
			t.Fatal("revoke must fail when the key file cannot be deleted")
		}
		if signedReceive(e, k2.Secret, `{}`, "r0").Status != 200 {
			t.Fatal("a failed revoke must leave the ledger untouched")
		}
	}

	var out struct{ Revoked int }
	if r := e.do(dlg, "DELETE", "/v1/sources/src_signed/webhook", ""); r.Code != 200 {
		t.Fatalf("revoke: %d %s", r.Code, r.Body)
	} else {
		r.into(&out)
	}
	if out.Revoked != 2 {
		t.Fatalf("revoked %d credentials, want both", out.Revoked)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatalf("key file must be deleted: %v", err)
	}
	if signedReceive(e, k1.Secret, `{}`, "r1").Status != 401 || signedReceive(e, k2.Secret, `{}`, "r2").Status != 401 {
		t.Fatal("revoked keys must stop verifying")
	}
	if r := e.do(dlg, "DELETE", "/v1/sources/src_signed/webhook", ""); r.Code != 200 {
		t.Fatalf("revoking again must be a no-op: %d", r.Code)
	}
	if strings.Contains(eventPayloads(e), k1.Secret) {
		t.Fatal("events must not carry the key")
	}
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

// seedEvents 给来源落盘若干事件并写入处置；每项是 {事件 id（也作主题）, 处置}。
func (e *env) seedEvents(source string, evs ...[2]string) {
	e.t.Helper()
	src := domain.Source{ID: source}
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		for _, x := range evs {
			if err := runner.AppendInternal(tx, src, domain.SourceEvent{ID: x[0], Type: "forge.note", Subject: x[0], Summary: "s", Link: "https://x.example/" + x[0], OccurredAt: e.now}); err != nil {
				return err
			}
		}
		all, err := runner.Events(tx, source)
		if err != nil {
			return err
		}
		for i, x := range evs {
			if domain.Disposition(x[1]) != domain.DispositionPending {
				if err := runner.SetDisposition(tx, all[i].ID, domain.Disposition(x[1]), "", ""); err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

type eventPage struct {
	Events    []domain.SourceEvent `json:"events"`
	NextAfter int64                `json:"next_after"`
}

func TestSourceEventsListFilterAndPage(t *testing.T) {
	e := setup(t)
	dlg, obs := e.token(auth.Delegate), e.token(auth.Observer)
	if r := e.do(dlg, "POST", "/v1/sources", hook); r.Code != 201 {
		t.Fatalf("register: %d %s", r.Code, r.Body)
	}
	if r := e.do(obs, "GET", "/v1/sources/src_hook/events", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"events":[]`) {
		t.Fatalf("empty list: %d %s", r.Code, r.Body)
	}
	e.seedEvents("src_hook", [2]string{"a", "unjudged"}, [2]string{"b", "ignored"}, [2]string{"c", "unjudged"})

	var all, un eventPage
	e.do(obs, "GET", "/v1/sources/src_hook/events", "").into(&all)
	if len(all.Events) != 3 || all.NextAfter != 0 {
		t.Fatalf("all: %+v", all)
	}
	if a := all.Events[0]; a.Type != "forge.note" || a.Subject != "a" || a.Summary != "s" || a.Link != "https://x.example/a" || !a.OccurredAt.Equal(e.now) || a.Disposition != domain.DispositionUnjudged {
		t.Fatalf("fields: %+v", a)
	}
	e.do(obs, "GET", "/v1/sources/src_hook/events?disposition=unjudged", "").into(&un)
	if len(un.Events) != 2 || un.Events[0].Subject != "a" || un.Events[1].Subject != "c" {
		t.Fatalf("unjudged: %+v", un)
	}

	var p1, p2 eventPage
	e.do(obs, "GET", "/v1/sources/src_hook/events?disposition=unjudged&limit=1", "").into(&p1)
	if len(p1.Events) != 1 || p1.Events[0].Subject != "a" || p1.NextAfter == 0 {
		t.Fatalf("page 1: %+v", p1)
	}
	e.do(obs, "GET", "/v1/sources/src_hook/events?disposition=unjudged&limit=1&after="+strconv.FormatInt(p1.NextAfter, 10), "").into(&p2)
	if len(p2.Events) != 1 || p2.Events[0].Subject != "c" || p2.NextAfter != 0 {
		t.Fatalf("page 2: %+v", p2)
	}

	for _, q := range []string{"disposition=bogus", "limit=0", "limit=501", "after=-1"} {
		if r := e.do(obs, "GET", "/v1/sources/src_hook/events?"+q, ""); r.Code != 400 {
			t.Errorf("%s: %d", q, r.Code)
		}
	}
	if r := e.do(obs, "GET", "/v1/sources/src_nope/events", ""); r.Code != 404 {
		t.Errorf("unknown source: %d", r.Code)
	}
	if r := e.do(e.token(auth.Ext), "GET", "/v1/sources/src_hook/events", ""); r.Code != 403 {
		t.Errorf("ext: %d", r.Code)
	}
	// 列表是未过滤的外部内容：运行凭据不能绕过转发白名单。
	if r := e.do(e.runCredential(), "GET", "/v1/sources/src_hook/events", ""); r.Code != 403 {
		t.Errorf("run credential: %d %s", r.Code, r.Body)
	}
}

// seedRejects 直接落盘拒收记录（表由 runner 模块拥有，这里只造数据）；每项是 {原始行, 原因}。
func (e *env) seedRejects(source string, rs ...[2]string) {
	e.t.Helper()
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		for i, x := range rs {
			if _, err := tx.Exec(`INSERT INTO sources_runner_reject (source_id, run_id, line, reason, raw, at) VALUES (?,?,?,?,?,?)`,
				source, "run_1", i+1, x[1], x[0], e.now.UnixNano()); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		e.t.Fatal(err)
	}
}

type rejectPage struct {
	Rejects   []runner.Reject `json:"rejects"`
	NextAfter int64           `json:"next_after"`
}

// ACT-SRC-04：坏事件不入账，原因在来源页可查：分页、字段、接收密钥遮蔽，运行凭据与 ext 不可读。
func TestSourceRejectsListPageAndAccess(t *testing.T) {
	e := setup(t)
	dlg, obs := e.token(auth.Delegate), e.token(auth.Observer)
	if r := e.do(dlg, "POST", "/v1/sources", hook); r.Code != 201 {
		t.Fatalf("register: %d %s", r.Code, r.Body)
	}
	if r := e.do(obs, "GET", "/v1/sources/src_hook/rejects", ""); r.Code != 200 || !strings.Contains(r.Body.String(), `"rejects":[]`) {
		t.Fatalf("empty list: %d %s", r.Code, r.Body)
	}
	e.seedRejects("src_hook", [2]string{"{bad", "invalid JSON"}, [2]string{"token apa_whk_SECRET", "missing subject"}, [2]string{"{}", "missing type"})
	e.seedRejects("src_other", [2]string{"x", "other source"})

	var p1, p2 rejectPage
	e.do(obs, "GET", "/v1/sources/src_hook/rejects?limit=2", "").into(&p1)
	if len(p1.Rejects) != 2 || p1.NextAfter == 0 {
		t.Fatalf("page 1: %+v", p1)
	}
	if a := p1.Rejects[0]; a.SourceID != "src_hook" || a.RunID != "run_1" || a.Line != 1 || a.Reason != "invalid JSON" || a.Raw != "{bad" || !a.At.Equal(e.now) {
		t.Fatalf("fields: %+v", a)
	}
	if p1.Rejects[1].Raw != "[redacted]" || p1.Rejects[1].Reason != "missing subject" {
		t.Fatalf("receive key must not be shown: %+v", p1.Rejects[1])
	}
	e.do(obs, "GET", "/v1/sources/src_hook/rejects?limit=2&after="+strconv.FormatInt(p1.NextAfter, 10), "").into(&p2)
	if len(p2.Rejects) != 1 || p2.Rejects[0].Reason != "missing type" || p2.NextAfter != 0 {
		t.Fatalf("page 2: %+v", p2)
	}

	for _, q := range []string{"limit=0", "limit=501", "after=-1", "after=x"} {
		if r := e.do(obs, "GET", "/v1/sources/src_hook/rejects?"+q, ""); r.Code != 400 {
			t.Errorf("%s: %d", q, r.Code)
		}
	}
	if r := e.do(obs, "GET", "/v1/sources/src_nope/rejects", ""); r.Code != 404 {
		t.Errorf("unknown source: %d", r.Code)
	}
	if r := e.do(e.token(auth.Ext), "GET", "/v1/sources/src_hook/rejects", ""); r.Code != 403 {
		t.Errorf("ext: %d", r.Code)
	}
	// 原始行是未过滤的外部输出：运行凭据不能绕过转发白名单。
	if r := e.do(e.runCredential(), "GET", "/v1/sources/src_hook/rejects", ""); r.Code != 403 {
		t.Errorf("run credential: %d %s", r.Code, r.Body)
	}
}

// runCredential 建一个事项和它的后台运行，返回运行凭据明文。
func (e *env) runCredential() string {
	e.t.Helper()
	res, err := e.x.Do(context.Background(), command.Request{Identity: command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb},
		Command: "item.create", Input: json.RawMessage(`{"kind":"task","title":"t"}`)})
	if err != nil {
		e.t.Fatal(err)
	}
	var it struct{ ID string }
	if err := json.Unmarshal(res.Value, &it); err != nil {
		e.t.Fatal(err)
	}
	var run string
	if err := e.st.Update(context.Background(), func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: it.ID, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, e.now)
		if err != nil {
			return err
		}
		run, err = runidentity.Issue(tx, r.ID, e.now)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return run
}

// ACT-TRI-02：Web 把未判定事件转为建议；没有来源管理权的凭据被拒绝，非 unjudged 返回冲突。
func TestProposeSourceEventRoute(t *testing.T) {
	e := setup(t)
	if r := e.do(e.token(auth.Delegate), "POST", "/v1/sources", hook); r.Code != 201 {
		t.Fatalf("register: %d %s", r.Code, r.Body)
	}
	e.seedEvents("src_hook", [2]string{"a", "unjudged"})
	var page eventPage
	e.do(e.token(auth.Observer), "GET", "/v1/sources/src_hook/events", "").into(&page)
	path := "/v1/source-events/" + page.Events[0].ID + "/propose"

	if r := e.do(e.token(auth.Worker), "POST", path, ""); r.Code != 403 {
		t.Fatalf("worker: %d %s", r.Code, r.Body)
	}
	r := e.web("POST", path, "")
	if r.Code != 200 {
		t.Fatalf("web: %d %s", r.Code, r.Body)
	}
	var got domain.SourceEvent
	r.into(&got)
	if got.Disposition != domain.DispositionProposed || got.Item == "" {
		t.Fatalf("event: %+v", got)
	}
	if err := e.st.View(context.Background(), func(tx *store.Tx) error {
		it, err := items.Get(tx, got.Item)
		if err == nil && (it.Phase != domain.PhaseProposed || it.Origin == nil || it.Origin.Source != "src_hook") {
			t.Fatalf("item: %+v", it)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := e.web("POST", path, ""); r.Code != 409 {
		t.Fatalf("second: %d %s", r.Code, r.Body)
	}
	if r := e.web("POST", "/v1/source-events/sev_nope/propose", ""); r.Code != 404 {
		t.Fatalf("missing: %d %s", r.Code, r.Body)
	}
}

// 内部来源 capture:<项目> 的事件只对 Web 开放（§9.3）；运行凭据 403，其他凭据与登记外的来源一样 404。
func TestCaptureSourceEventsAreWebOnly(t *testing.T) {
	e := setup(t)
	e.seedEvents("capture:prj_1", [2]string{"c1", "unjudged"})
	path := "/v1/sources/capture:prj_1/events?disposition=unjudged"
	var page eventPage
	r := e.web("GET", path, "")
	r.into(&page)
	if r.Code != 200 || len(page.Events) != 1 || page.Events[0].Subject != "c1" {
		t.Fatalf("web: %d %s", r.Code, r.Body)
	}
	for _, k := range []auth.Kind{auth.Observer, auth.Worker, auth.Delegate} {
		if r := e.do(e.token(k), "GET", path, ""); r.Code != 404 {
			t.Errorf("%s: %d", k, r.Code)
		}
	}
	if r := e.do(e.runCredential(), "GET", path, ""); r.Code != 403 {
		t.Errorf("run credential: %d", r.Code)
	}
	if r := e.web("GET", "/v1/sources/capture:/events", ""); r.Code != 404 {
		t.Errorf("empty project: %d", r.Code)
	}
}

// 入站记录的原始请求体未经转发白名单，运行凭据不能读（§14.3、§14.6）。
func TestRunCredentialCannotReadIngests(t *testing.T) {
	e := setup(t)
	if r := e.do(e.token(auth.Delegate), "POST", "/v1/sources", hook); r.Code != 201 {
		t.Fatalf("register: %d %s", r.Code, r.Body)
	}
	if r := e.do(e.runCredential(), "GET", "/v1/sources/src_hook/ingests", ""); r.Code != 403 {
		t.Fatalf("run credential: %d %s", r.Code, r.Body)
	}
	if r := e.do(e.token(auth.Observer), "GET", "/v1/sources/src_hook/ingests", ""); r.Code != 200 {
		t.Fatalf("observer: %d %s", r.Code, r.Body)
	}
}
