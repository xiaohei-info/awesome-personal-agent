package sources_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apisources "github.com/xiaohei-info/awesome-personal-agent/api/sources"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
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

type schemes struct{}

func (schemes) List() []*packs.Pack {
	return []*packs.Pack{{Name: "hooks", WebhookSchemes: []packs.WebhookScheme{
		{ID: "shared", SignatureHeader: "X-Hook-Key", Algorithm: "token", DeliveryIDHeader: "X-Delivery"}}}}
}

type noJudge struct{}

func (noJudge) Judge(context.Context, direct.JudgeCall) (direct.Verdict, error) {
	panic("judge must not be called")
}

func (noJudge) JudgeBatch(context.Context, []direct.JudgeCall) ([]direct.Verdict, []error) {
	panic("judge must not be called")
}

// kernel 是真实账本、命令管线、授权与来源路由，经 httptest 暴露；CLI 经真实 HTTP 调它。
type kernel struct {
	t    *testing.T
	st   *store.Store
	au   *auth.Auth
	wh   *webhook.Service
	url  string
	web  *http.Cookie // 本人的 Web 会话：来源登记、接收密钥是 3b，只有它能做
	csrf string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, core.Module, registry.Module, webhook.Module,
		projects.Module, clock.Module, items.Module, runner.Module, rules.Module, proc.Module, runidentity.Module, direct.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, nil)
	au := auth.New(nil)
	if err := registry.Register(x, registry.Options{Bundled: []string{"fetch-feed"}, Declared: declared}); err != nil {
		t.Fatal(err)
	}
	wh, err := webhook.New(webhook.Options{Store: st, Exec: x, Packs: schemes{}, Auth: au, Secrets: webhook.KeyFiles{Dir: filepath.Join(t.TempDir(), "keys"), Auth: au}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(wh.Wait)
	if err := items.Register(x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	in, err := inbound.New(inbound.Options{Store: st, Exec: x, Packs: schemes{}, Judge: noJudge{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := in.Register(x); err != nil {
		t.Fatal(err)
	}
	grp, err := apisources.Mount(st, x, au, wh, in, apisources.Declared{})
	if err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{grp}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	k := &kernel{t: t, st: st, au: au, wh: wh, url: ts.URL}
	const pw = "correct horse battery"
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return core.SetOwnerPassword(tx, pw) }); err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]string{"pass" + "word": pw})
	resp, err := http.Post(ts.URL+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 || len(resp.Cookies()) == 0 {
		t.Fatalf("login: %d %v", resp.StatusCode, err)
	}
	k.web, k.csrf = resp.Cookies()[0], out.CSRF
	return k
}

// owner 以本人 Web 会话（登录即刚验过口令）发请求，返回状态与响应体。
func (k *kernel) owner(method, path, body string) (int, []byte) {
	k.t.Helper()
	req, _ := http.NewRequest(method, k.url+path, strings.NewReader(body))
	req.AddCookie(k.web)
	req.Header.Set(core.CSRFHeader, k.csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		k.t.Fatal(err)
	}
	defer resp.Body.Close()
	var b bytes.Buffer
	_, _ = b.ReadFrom(resp.Body)
	return resp.StatusCode, b.Bytes()
}

// ownerOK 同 owner，状态须是 2xx，响应体解进 v（v 可为 nil）。
func (k *kernel) ownerOK(method, path, body string, v any) {
	k.t.Helper()
	code, b := k.owner(method, path, body)
	if code/100 != 2 || (v != nil && json.Unmarshal(b, v) != nil) {
		k.t.Fatalf("%s %s: %d %s", method, path, code, b)
	}
}

// rulesAgent 签一把归属 Agent 的 delegate 钥匙，本人把它放开到「连日常规矩也替我改」：来源启停与改名是 3a。
func (k *kernel) rulesAgent() string {
	k.t.Helper()
	var secret string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		_, s, err := k.au.Issue(tx, auth.Spec{Kind: auth.Delegate, AgentID: "agent_r", Harness: "h", Audience: auth.AudienceInternal})
		if err != nil {
			return err
		}
		secret = s
		_, err = policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "test tier", func(p *policy.Policy) error {
			p.AgentCards["agent_r"] = policy.DefaultCard().With(policy.GroupDecide, policy.GroupRules)
			return nil
		})
		return err
	}); err != nil {
		k.t.Fatal(err)
	}
	return secret
}

func (k *kernel) token(kind auth.Kind) string {
	k.t.Helper()
	var secret string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) (err error) {
		_, secret, err = k.au.Issue(tx, auth.Spec{Kind: kind, Audience: agentAudience(kind)})
		return
	}); err != nil {
		k.t.Fatal(err)
	}
	return secret
}

type result struct {
	code        int
	out, errOut string
}

func (k *kernel) apa(token string, args ...string) result {
	k.t.Helper()
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": token, "APA_SESSION": "s1", "APA_HOME": k.t.TempDir()}
	var out, errb bytes.Buffer
	code := client.Main(args, client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: k.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return result{code, out.String(), errb.String()}
}

func (k *kernel) events(typ string) (n int) {
	k.t.Helper()
	if err := k.st.View(context.Background(), func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, en := range es {
			if en.Event.Type == typ {
				n++
			}
		}
		return err
	}); err != nil {
		k.t.Fatal(err)
	}
	return
}

func decode(t *testing.T, r result, v any) {
	t.Helper()
	if r.code != 0 || json.Unmarshal([]byte(r.out), v) != nil {
		t.Fatalf("exit %d out=%q err=%q", r.code, r.out, r.errOut)
	}
}

// ACT-CMD-01：登记来源是 3b：Agent 钥匙带不带自定义命令都得 20 并留痕，来源未登记；ACT-CMD-05：本人可登记附带拉取程序和 webhook。
func TestAddRejectsCustomCommandAndAcceptsBundledProgram(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)

	for _, args := range [][]string{
		{"source", "add", "--id", "src_evil", "--command", "/tmp/acc/evil.sh", "--schedule", "15m", "--timeout", "30"},
		{"source", "add", "--id", "src_evil", "--type", "script", "--command", "/tmp/acc/evil.sh", "--schedule", "15m", "--timeout", "30"},
	} {
		if r := k.apa(dlg, args...); r.code != domain.ExitTierClosed || !strings.Contains(r.errOut, `"tier_closed"`) {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if k.events(domain.EvCommandRejected) < 2 {
		t.Fatal("web_only attempts must leave command.rejected events")
	}
	var list struct{ Sources []domain.Source }
	decode(t, k.apa(obs, "source", "list"), &list)
	if len(list.Sources) != 0 {
		t.Fatalf("rejected source was stored: %+v", list.Sources)
	}

	for tok, want := range map[string]int{dlg: domain.ExitTierClosed, obs: domain.ExitPermissionDenied} {
		if r := k.apa(tok, "source", "add", "--id", "src_feed", "--type", "pull", "--command", "fetch-feed", "--schedule", "15m", "--timeout", "60"); r.code != want {
			t.Fatalf("Agent key registering a bundled source: %+v", r)
		}
	}
	var feed, hook domain.Source
	k.ownerOK("POST", "/v1/sources", `{"id":"src_feed","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60,"config":{"url":"http://x"}}`, &feed)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_hook","type":"webhook","source_type":"forge","config":{"scheme":"shared"}}`, &hook)
	if feed.Command != "fetch-feed" || !feed.Enabled || hook.Type != domain.SourceWebhook || hook.SourceKind != "forge" {
		t.Fatalf("registered: %+v %+v", feed, hook)
	}
	if code, _ := k.owner("POST", "/v1/sources", `{"id":"src_feed","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`); code != http.StatusConflict {
		t.Fatalf("duplicate: %d", code)
	}
	for _, args := range [][]string{
		{"source", "add"}, {"source", "add", "--id", "src_a", "--timeout", "x"}, {"source", "add", "--id", "src_a", "--config", "{"},
		{"source", "add", "--id", "src_a", "extra"},
	} {
		if r := k.apa(dlg, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// list/show 对 observer 开放；ACT-SRC-10：disable/enable 翻转 enabled，其余字段不动。启停是 3a：只整理的 delegate 得 23，
// 放开到日常规矩的 Agent 可以做。
func TestListShowEnableDisable(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.rulesAgent(), k.token(auth.Observer)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_feed","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`, nil)
	if r := k.apa(k.token(auth.Delegate), "source", "disable", "src_feed"); r.code != domain.ExitTierClosed {
		t.Fatalf("organize-only disable: %+v", r)
	}

	var s domain.Source
	decode(t, k.apa(dlg, "source", "disable", "src_feed"), &s)
	if s.Enabled || s.Schedule != "15m" {
		t.Fatalf("disable: %+v", s)
	}
	decode(t, k.apa(obs, "source", "show", "src_feed"), &s)
	if s.Enabled {
		t.Fatalf("show after disable: %+v", s)
	}
	decode(t, k.apa(dlg, "source", "enable", "src_feed"), &s)
	if !s.Enabled {
		t.Fatalf("enable: %+v", s)
	}
	var list struct{ Sources []domain.Source }
	decode(t, k.apa(obs, "source", "list"), &list)
	if len(list.Sources) != 1 || list.Sources[0].ID != "src_feed" {
		t.Fatalf("list: %+v", list)
	}
	if r := k.apa(obs, "source", "disable", "src_feed"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer disable: %+v", r)
	}
	for _, args := range [][]string{{"source", "show", "src_none"}, {"source", "enable", "src_none"}} {
		if r := k.apa(dlg, args...); r.code != domain.ExitNotFound {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	for _, args := range [][]string{{"source", "show"}, {"source", "list", "x"}, {"source", "enable", "a", "b"}, {"source", "disable"}} {
		if r := k.apa(dlg, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// ACT-HOOK-01：接收密钥是 3b：Agent 钥匙经 CLI create/rotate 都得 20；本人 create 返回 URL 与只显示一次的密钥并记 Event；
// ACT-HOOK-11：rotate 换密钥，并存期内新旧都收，之后旧的失效。ingests 展示原始 payload 与状态；
// ACT-HOOK-10：replay 是一档，delegate 重放同一条入站记录，代数加一并记 Event。
func TestWebhookCreateRotateIngestsReplay(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_hook","type":"webhook","config":{"scheme":"shared"}}`, nil)

	for _, args := range [][]string{{"source", "webhook", "create", "src_hook"}, {"source", "webhook", "rotate", "src_hook"}} {
		for tok, want := range map[string]int{obs: domain.ExitPermissionDenied, dlg: domain.ExitTierClosed} {
			if r := k.apa(tok, args...); r.code != want {
				t.Fatalf("%v by an Agent key: %+v", args, r)
			}
		}
	}
	var k1 struct {
		Secret, URL string
		Rotated     bool
	}
	k.ownerOK("POST", "/v1/sources/src_hook/webhook", "", &k1)
	if !strings.HasPrefix(k1.Secret, "apa_whk_") || k1.URL != "/ingest/src_hook" || k1.Rotated || k.events("source.webhook_key_created") != 1 {
		t.Fatalf("create: %+v", k1)
	}
	recv := func(key, delivery string) int {
		return k.wh.Receive(context.Background(), webhook.Request{SourceID: "src_hook",
			Header: map[string]string{"X-Hook-Key": key, "X-Delivery": delivery}, Body: []byte(`{"n":1}`)}).Status
	}
	if recv(k1.Secret, "d1") != 200 {
		t.Fatal("receive with created key")
	}
	k.wh.Wait()

	var k2 struct {
		Secret  string
		Rotated bool
	}
	k.ownerOK("POST", "/v1/sources/src_hook/webhook", "", &k2)
	if !k2.Rotated || k2.Secret == k1.Secret || k.events("source.webhook_key_rotated") != 1 {
		t.Fatalf("rotate: %+v", k2)
	}
	if recv(k1.Secret, "d2") != 200 || recv(k2.Secret, "d3") != 200 {
		t.Fatal("old and new key must both be accepted within the grace period")
	}
	k.wh.Wait()

	var ing struct {
		Ingests []struct {
			ID, Body string
			Gen      int
		}
	}
	decode(t, k.apa(obs, "source", "ingests", "src_hook"), &ing)
	if len(ing.Ingests) != 3 || ing.Ingests[0].Body != `{"n":1}` {
		t.Fatalf("ingests: %+v", ing)
	}
	if r := k.apa(obs, "source", "replay", ing.Ingests[0].ID); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer replay: %+v", r)
	}
	var rp struct {
		ID  string
		Gen int
	}
	decode(t, k.apa(dlg, "source", "replay", ing.Ingests[0].ID), &rp)
	if rp.ID != ing.Ingests[0].ID || rp.Gen != ing.Ingests[0].Gen+1 || k.events("source.replay_requested") != 1 {
		t.Fatalf("replay: %+v", rp)
	}
	k.wh.Wait()
	for _, args := range [][]string{{"source", "replay", "ing_none"}, {"source", "ingests", "src_none"}} {
		if r := k.apa(dlg, args...); r.code != domain.ExitNotFound {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	k.ownerOK("POST", "/v1/sources", `{"id":"src_feed","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`, nil)
	if code, b := k.owner("POST", "/v1/sources/src_feed/webhook", ""); code != http.StatusBadRequest {
		t.Fatalf("webhook key for pull source: %d %s", code, b)
	}
	for _, args := range [][]string{{"source", "webhook"}, {"source", "webhook", "bogus", "src_hook"}, {"source", "webhook", "create"}, {"source", "ingests"}, {"source", "replay"}} {
		if r := k.apa(dlg, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// 附录 B.1 `apa source webhook revoke`：吊销是 3b，Agent 钥匙都得 20；本人吊销该来源全部接收密钥（含宽限期内的旧密钥）并记 Event，之后接收被拒。
func TestWebhookRevokeDropsEveryKey(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_hook","type":"webhook","config":{"scheme":"shared"}}`, nil)
	var k1, k2 struct{ Secret string }
	k.ownerOK("POST", "/v1/sources/src_hook/webhook", "", &k1)
	k.ownerOK("POST", "/v1/sources/src_hook/webhook", "", &k2)
	recv := func(key, delivery string) int {
		return k.wh.Receive(context.Background(), webhook.Request{SourceID: "src_hook",
			Header: map[string]string{"X-Hook-Key": key, "X-Delivery": delivery}, Body: []byte(`{"n":1}`)}).Status
	}
	if recv(k1.Secret, "d1") != 200 || recv(k2.Secret, "d2") != 200 {
		t.Fatal("both keys must work before revoke")
	}
	k.wh.Wait()
	for tok, want := range map[string]int{obs: domain.ExitPermissionDenied, dlg: domain.ExitTierClosed} {
		if r := k.apa(tok, "source", "webhook", "revoke", "src_hook"); r.code != want {
			t.Fatalf("Agent key revoke: %+v", r)
		}
	}
	var rv struct{ Revoked int }
	k.ownerOK("DELETE", "/v1/sources/src_hook/webhook", "", &rv)
	if rv.Revoked != 2 || k.events("source.webhook_key_revoked") == 0 {
		t.Fatalf("revoke: %+v", rv)
	}
	if recv(k1.Secret, "d3") == 200 || recv(k2.Secret, "d4") == 200 {
		t.Fatal("revoked keys must be rejected")
	}
	if code, _ := k.owner("DELETE", "/v1/sources/src_none/webhook", ""); code != http.StatusNotFound {
		t.Fatalf("revoke unknown source: %d", code)
	}
}

// `apa source rejects <id>`：observer 可读被拒收的输出行，--after/--limit 翻页；未登记来源得 3，缺 id 或多余参数得 2。
func TestRejectsListsAndPages(t *testing.T) {
	k := newKernel(t)
	obs := k.token(auth.Observer)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_feed","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`, nil)
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		for i, why := range []string{"invalid JSON", "missing type"} {
			if _, err := tx.Exec(`INSERT INTO sources_runner_reject (source_id, run_id, line, reason, raw, at) VALUES (?,?,?,?,?,?)`, "src_feed", "run_1", i+1, why, "raw"+why, 1); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var p1, p2 struct {
		Rejects []struct {
			RunID  string `json:"run_id"`
			Reason string
			Raw    string
		}
		NextAfter int64 `json:"next_after"`
	}
	decode(t, k.apa(obs, "source", "rejects", "src_feed", "--limit", "1"), &p1)
	if len(p1.Rejects) != 1 || p1.Rejects[0].Reason != "invalid JSON" || p1.Rejects[0].RunID != "run_1" || p1.Rejects[0].Raw != "rawinvalid JSON" || p1.NextAfter == 0 {
		t.Fatalf("page 1: %+v", p1)
	}
	decode(t, k.apa(obs, "source", "rejects", "src_feed", "--limit", "1", "--after", strconv.FormatInt(p1.NextAfter, 10)), &p2)
	if len(p2.Rejects) != 1 || p2.Rejects[0].Reason != "missing type" || p2.NextAfter != 0 {
		t.Fatalf("page 2: %+v", p2)
	}
	if r := k.apa(obs, "source", "rejects", "src_none"); r.code != domain.ExitNotFound {
		t.Fatalf("unknown source: %+v", r)
	}
	if r := k.apa(obs, "source", "rejects", "src_feed", "--limit", "0"); r.code != domain.ExitUsage {
		t.Fatalf("bad limit: %+v", r)
	}
	for _, args := range [][]string{{"source", "rejects"}, {"source", "rejects", "--limit", "1"}, {"source", "rejects", "src_feed", "extra"}, {"source", "rejects", "src_feed", "--bogus", "1"}} {
		if r := k.apa(obs, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// 验收 ACT-OBD-11：`apa source add --default-project` 登记成功，show 显示默认项目、本机范围 full、外部范围未选择；
// 默认项目不存在时返回 not_found，不静默丢弃。登记是 3b：CLI 的 Agent 钥匙得 20，本人经 Web 登记。
func TestSourceAddWithDefaultProject(t *testing.T) {
	k := newKernel(t)
	dlg := k.token(auth.Delegate)
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO projects (id, name, intro, tags, status, repos, allowed_executors, capture) VALUES ('acc-home', 'home', '', '[]', 'active', '[]', 'null', 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	base := []string{"source", "add", "--id", "src_rss", "--type", "pull", "--command", "fetch-feed", "--schedule", "15m", "--timeout", "60"}
	if r := k.apa(dlg, append(base, "--default-project", "acc-home")...); r.code != domain.ExitTierClosed {
		t.Fatalf("Agent key add: %+v", r)
	}
	reg := func(prj string) string {
		return `{"id":"src_rss","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60,"default_project":"` + prj + `"}`
	}
	if code, _ := k.owner("POST", "/v1/sources", reg("nope")); code != http.StatusNotFound {
		t.Fatalf("unknown project: %d", code)
	}
	k.ownerOK("POST", "/v1/sources", reg("acc-home"), nil)
	var s domain.Source
	decode(t, k.apa(k.token(auth.Observer), "source", "show", "src_rss"), &s)
	if s.DefaultProject != "acc-home" || s.Outbound.Local != "full" || s.Outbound.External != nil {
		t.Fatalf("show: %+v", s)
	}
}

// ACT-BKL-02、ACT-BKL-04：`apa source backlog list` 列出积压且旧原因原样；`rules` 是一档：observer 得 4、积压不变，
// delegate 直接跑，返回 attached/disposed/remaining（此内核没有来源规则，全部留在积压里）。
func TestBacklogListAndRulesCommands(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_hook","type":"webhook","source_type":"forge","config":{"scheme":"shared"}}`, nil)
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		for i, row := range []struct{ disp, reason string }{{"unjudged", "undecided"}, {"unjudged", ""}, {"expired", ""}, {"proposed", ""}} {
			if _, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at,
				correlation_keys, sender, sender_verified, disposition, item, attach_state, unjudged_reason, rejudges)
				VALUES ('dd', ?, ?, 'src_hook', 'r', 't', 's', '', '', '', 1, '[]', '', 0, ?, '', '', ?, 0)`,
				"e"+strconv.Itoa(i), "e"+strconv.Itoa(i), row.disp, row.reason); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	var list struct {
		Total  int
		Events []struct {
			Disposition    string
			UnjudgedReason string `json:"unjudged_reason"`
		}
	}
	decode(t, k.apa(obs, "source", "backlog", "list"), &list)
	if list.Total != 3 || len(list.Events) != 3 || list.Events[0].UnjudgedReason != "undecided" || list.Events[1].UnjudgedReason != "" {
		t.Fatalf("list: %+v", list)
	}
	decode(t, k.apa(obs, "source", "backlog", "list", "--source", "src_hook"), &list)
	if list.Total != 3 {
		t.Fatalf("list --source: %+v", list)
	}

	if r := k.apa(obs, "source", "backlog", "rules"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer rules: %+v", r)
	}
	var res struct{ Attached, Disposed, Remaining int }
	decode(t, k.apa(dlg, "source", "backlog", "rules"), &res)
	if res.Attached != 0 || res.Disposed != 0 || res.Remaining != 3 {
		t.Fatalf("rules: %+v", res)
	}
	// ACT-BKL-16：影子打分同为一档；没有判断端点时被拒，退出码 6，不外呼（noJudge 一调就 panic）。
	if r := k.apa(obs, "source", "backlog", "score"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer score: %+v", r)
	}
	if r := k.apa(dlg, "source", "backlog", "score"); r.code != domain.ExitConflict || !strings.Contains(r.out+r.errOut, "judge endpoint") {
		t.Fatalf("score without a judge endpoint: %+v", r)
	}
	// 放开重判（第 4 步）：observer 被拒，且在碰账本之前（noJudge 一调就 panic）。
	if r := k.apa(obs, "source", "backlog", "rejudge", "--limit", "3"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer rejudge: %+v", r)
	}
	for _, args := range [][]string{{"source", "backlog"}, {"source", "backlog", "rejudge", "--limit", "-1"}, {"source", "backlog", "rules", "--limit", "3"},
		{"source", "backlog", "list", "extra"}, {"source", "backlog", "score", "--sort", "score"}, {"source", "backlog", "list", "--sort", "name"}} {
		if r := k.apa(obs, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// ACT-BKL-06：`backlog list --sort score` 把各来源的积压合起来按分数从高到低排，每条带分数，没有分数的在最后。
func TestBacklogListSortsByScoreAcrossSources(t *testing.T) {
	k := newKernel(t)
	obs := k.token(auth.Observer)
	for _, id := range []string{"src_a", "src_b"} {
		k.ownerOK("POST", "/v1/sources", `{"id":"`+id+`","type":"webhook","source_type":"forge","config":{"scheme":"shared"}}`, nil)
	}
	scores := map[string]*float64{"a1": ptr(0.40), "a2": nil, "b1": ptr(0.90), "b2": ptr(0.05)}
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		for _, e := range []struct{ src, id string }{{"src_a", "a1"}, {"src_a", "a2"}, {"src_b", "b1"}, {"src_b", "b2"}} {
			if _, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at,
				correlation_keys, sender, sender_verified, disposition, item, attach_state, unjudged_reason, rejudges)
				VALUES ('dd', ?, ?, ?, 'r', 't', 's', '', '', '', 1, '[]', '', 0, 'unjudged', '', '', '', 0)`, e.id, e.id, e.src); err != nil {
				return err
			}
			if p := scores[e.id]; p != nil {
				err := record.Put(tx, record.Record{ID: "jdg_" + e.id, Use: "inbound_screen", Template: "p/triage-screen@h", Subject: e.id, Source: e.src, Fingerprint: "f" + e.id,
					Band: record.Band{Low: 0.15, High: 0.6}, DecidedBy: record.DecidedDefault, Answer: "yes", LowConfidence: true, Shadow: true, CreatedAt: time.Unix(1, 0).UTC(),
					Levels: []record.Level{{Backend: record.BackendClassifier, PYes: p, Outcome: record.OutcomeSkipped, ErrorCode: "shadow"}}})
				if err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var list struct {
		Events []struct {
			ID    string
			Score *float64
		}
	}
	decode(t, k.apa(obs, "source", "backlog", "list", "--sort", "score"), &list)
	var got []string
	for _, e := range list.Events {
		got = append(got, e.ID)
		if (e.Score == nil) != (e.ID == "a2") {
			t.Fatalf("%s: score %v", e.ID, e.Score)
		}
	}
	if fmt.Sprint(got) != "[b1 a1 b2 a2]" {
		t.Fatalf("order %v", got)
	}
}

func ptr(f float64) *float64 { return &f }

// ACT-RST-03、05、06、09：`apa source backlog restore --dry-run` 是 observer 可用的只读预览；执行是一档：observer 得 4，
// delegate 放回 1 条旧级联忽略的事件，再放回得 0 条。
func TestBacklogRestoreCommand(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_hook","type":"webhook","source_type":"forge","config":{"scheme":"shared"}}`, nil)
	at := time.Date(2026, 2, 15, 0, 0, 0, 0, time.UTC)
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at,
			correlation_keys, sender, sender_verified, disposition, item, attach_state, unjudged_reason, rejudges)
			VALUES ('dd', 'old', 'old', 'src_hook', 'r', 't', 's', '', '', '', ?, '[]', '', 0, 'ignored', '', '', '', 0)`, at.UnixNano()); err != nil {
			return err
		}
		_, err := eventlog.Append(tx, domain.Event{Type: inbound.EvDisposed, Object: "old", At: at, Payload: json.RawMessage(`{"event":"old","disposition":"ignored","rule":""}`),
			Actor: domain.Actor{Type: domain.ActorSystem, Principal: "kernel:inbound", Via: domain.ViaSource}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	win := []string{"--from", "2026-02-10T00:00:00Z", "--to", "2026-02-20T00:00:00Z"}
	restore := func(token string, extra ...string) result {
		return k.apa(token, append(append([]string{"source", "backlog", "restore"}, win...), extra...)...)
	}

	var pv inbound.RestorePreview
	decode(t, restore(obs, "--dry-run"), &pv)
	if pv.Matched != 1 || len(pv.Sources) != 1 || pv.Sources[0].Source != "src_hook" {
		t.Fatalf("dry run: %+v", pv)
	}
	decode(t, restore(obs, "--dry-run", "--source", "src_hook"), &pv)
	if pv.Matched != 1 {
		t.Fatalf("dry run, one source: %+v", pv)
	}
	if r := restore(obs, "--dry-run", "--source", "nope"); r.code != domain.ExitNotFound {
		t.Fatalf("unknown source: %+v", r)
	}
	if r := restore(obs); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer: %+v", r)
	}
	if n := k.events(inbound.EvDisposed); n != 1 {
		t.Fatalf("a refused restore wrote events: %d", n)
	}
	for _, args := range [][]string{{"source", "backlog", "restore"}, {"source", "backlog", "restore", "--from", win[1]}, {"source", "backlog", "rules", "--from", win[1]},
		{"source", "backlog", "rules", "--dry-run"}, {"source", "backlog", "restore", "--limit", "3", "--from", win[1], "--to", win[3]}} {
		if r := k.apa(obs, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	var res struct{ Restored int }
	decode(t, restore(dlg, "--source", "src_hook"), &res)
	if res.Restored != 1 || k.events(inbound.EvDisposed) != 2 {
		t.Fatalf("restore: %+v", res)
	}
	decode(t, restore(dlg), &res)
	if res.Restored != 0 || k.events(inbound.EvDisposed) != 2 {
		t.Fatalf("second restore: %+v", res)
	}
}

// 显示名（§9.1）：`apa source add --name` 存下名字；不带时输出里有内核起的名字；重名退出码 6；`apa source rename` 改名。
// declared 是测试装配的接入层声明（§9.1）：附带的 fetch-feed 与 webhook 来源按实例声明名字，其他程序（如自定义脚本）没有声明。
func declared(s domain.Source) string {
	switch {
	case s.Command == "fetch-feed":
		return "订阅 " + s.ID
	case s.Type == domain.SourceWebhook:
		return "推送 " + s.ID
	}
	return ""
}

// 登记（3b）由本人经 Web 做；改名是 3a：只整理的 delegate 得 23，放开到日常规矩的 Agent 经 CLI 改名。
func TestAddNameAndRename(t *testing.T) {
	k := newKernel(t)
	dlg := k.rulesAgent()
	var s domain.Source
	k.ownerOK("POST", "/v1/sources", `{"id":"src_a","name":"  订阅  ","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`, &s)
	if s.Name != "订阅" {
		t.Fatalf("--name: %+v", s)
	}
	k.ownerOK("POST", "/v1/sources", `{"id":"src_b","type":"webhook","config":{"scheme":"shared"}}`, &s)
	if s.Name != "推送 src_b" {
		t.Fatalf("no --name: the output shows the declared name: %+v", s)
	}
	if code, _ := k.owner("POST", "/v1/sources", `{"id":"src_c","name":"订阅","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`); code != http.StatusConflict {
		t.Fatalf("duplicate name: %d", code)
	}
	if r := k.apa(k.token(auth.Delegate), "source", "rename", "src_b", "x"); r.code != domain.ExitTierClosed {
		t.Fatalf("organize-only rename: %+v", r)
	}
	decode(t, k.apa(dlg, "source", "rename", "src_b", "仓库推送"), &s)
	if s.Name != "仓库推送" {
		t.Fatalf("rename: %+v", s)
	}
	if r := k.apa(dlg, "source", "rename", "src_b"); r.code != domain.ExitUsage {
		t.Fatalf("rename without a name: %+v", r)
	}
}

// ACT-RAW-05、ACT-RSC-05：`apa source event <id>` 输出事件、原文清单与正文并写明所用范围与裁剪量；--part 只输出那一部分；
// token 读到的内容按来源的外发范围裁剪；运行凭据得退出码 4，未知事件得 5，未知部分得 2。
func TestSourceEventReadsContentScopedAndPartwise(t *testing.T) {
	k := newKernel(t)
	obs := k.token(auth.Observer)
	k.ownerOK("POST", "/v1/sources", `{"id":"src_mail","type":"pull","command":"fetch-feed","schedule":"15m","timeout":60}`, nil)
	body := strings.Repeat("正文", 400) // 800 个字符
	var id string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, domain.Source{ID: "src_mail"}, domain.SourceEvent{ID: "m1", Type: "mail.received", Subject: "s", Detail: body, OccurredAt: time.Now()}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, "src_mail")
		if err != nil {
			return err
		}
		id = evs[0].ID
		_, err = tx.Exec(`INSERT INTO sources_runner_event_part (event_id, part, media_type, bytes, sha256, content) VALUES (?, 'headers', 'application/json', 12, 'h', ?)`, id, []byte(`{"From":"a"}`))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	// 内部钥匙全文；外部钥匙按它自己的逐来源档位（§14.14），与来源送外部端点的范围无关。
	keyWith := func(scope string) string {
		t.Helper()
		var s string
		if err := k.st.Update(context.Background(), func(tx *store.Tx) (err error) {
			_, s, err = k.au.Issue(tx, auth.Spec{Kind: auth.Observer, SourceScopes: map[string]string{"src_mail": scope}})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	type out struct {
		Scope   string          `json:"scope"`
		Body    *string         `json:"body"`
		Headers json.RawMessage `json:"headers"`
		Trimmed *struct {
			TotalChars    int `json:"total_chars"`
			ReturnedChars int `json:"returned_chars"`
		} `json:"trimmed"`
		Content struct {
			Parts []struct{ Part string } `json:"parts"`
		} `json:"content"`
	}

	var full out
	decode(t, k.apa(obs, "source", "event", id), &full)
	if full.Scope != "full" || full.Body == nil || *full.Body != body || full.Trimmed != nil || len(full.Content.Parts) != 2 {
		t.Fatalf("full: %+v", full)
	}
	var hdr out
	decode(t, k.apa(obs, "source", "event", id, "--part", "headers"), &hdr)
	if string(hdr.Headers) != `{"From":"a"}` || hdr.Body != nil {
		t.Fatalf("--part headers must print only headers: %+v", hdr)
	}

	var ex out
	decode(t, k.apa(keyWith("excerpt"), "source", "event", id), &ex)
	if ex.Scope != "excerpt" || ex.Body == nil || len([]rune(*ex.Body)) != 500 || ex.Trimmed == nil || ex.Trimmed.TotalChars != 800 || ex.Trimmed.ReturnedChars != 500 || ex.Headers != nil {
		t.Fatalf("excerpt: %+v", ex)
	}

	if r := k.apa(obs, "source", "event", "sev_nope"); r.code != domain.ExitNotFound {
		t.Fatalf("unknown event: %+v", r)
	}
	if r := k.apa(obs, "source", "event", id, "--part", "bogus"); r.code != domain.ExitUsage {
		t.Fatalf("unknown part: %+v", r)
	}
	var ext string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) (err error) {
		_, ext, err = k.au.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "chan"})
		return
	}); err != nil {
		t.Fatal(err)
	}
	if r := k.apa(ext, "source", "event", id); r.code != domain.ExitPermissionDenied {
		t.Fatalf("ext credential: %+v", r)
	}
	for _, args := range [][]string{{"source", "event"}, {"source", "event", "--part", "body"}, {"source", "event", id, "extra"}} {
		if r := k.apa(obs, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// agentAudience：测试里的三级 token 代表 APA 内部的 Agent，读来源不受档位限制（§14.2）；ext、webhook 是外部钥匙。
func agentAudience(k auth.Kind) string {
	if k == auth.Observer || k == auth.Worker || k == auth.Delegate {
		return auth.AudienceInternal
	}
	return ""
}

// `apa source remind` 记为已口头提醒；只口头提醒来源的事件，开放了全部日常权限的 Agent 也建不了建议（退出码 6）、切不了处理方式（20）。
func TestRemindCommandOnRemindOnlySource(t *testing.T) {
	k := newKernel(t)
	agent := k.rulesAgent()
	k.ownerOK("POST", "/v1/sources", `{"id":"src_hook","type":"webhook","source_type":"forge","config":{"scheme":"shared"}}`, nil)
	var s domain.Source
	k.ownerOK("PATCH", "/v1/sources/src_hook", `{"handling_mode":"remind_only"}`, &s)
	if s.HandlingMode != domain.HandlingRemindOnly {
		t.Fatalf("patched: %+v", s)
	}
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at,
			correlation_keys, sender, sender_verified, disposition, item, attach_state, unjudged_reason, rejudges)
			VALUES ('dd', 'e1', 'e1', 'src_hook', 'r', 't', 's', '', '', '', 1, '[]', '', 0, 'unjudged', '', '', '', 0)`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if r := k.apa(agent, "source", "propose", "e1"); r.code != domain.ExitConflict || !strings.Contains(r.errOut, "remind") {
		t.Fatalf("propose: %+v", r)
	}
	var ev domain.SourceEvent
	decode(t, k.apa(agent, "source", "remind", "e1"), &ev)
	if ev.Disposition != domain.DispositionReminded {
		t.Fatalf("remind: %+v", ev)
	}
	if code, b := k.owner("GET", "/v1/sources/src_hook/events?disposition=reminded", ""); code != 200 || !strings.Contains(string(b), `"e1"`) {
		t.Fatalf("list: %d %s", code, b)
	}
}
