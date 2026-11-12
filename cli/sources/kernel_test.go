package sources_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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

func (noJudge) Do(context.Context, direct.Call) (direct.Outcome, error) {
	panic("judge must not be called")
}

// kernel 是真实账本、命令管线、授权与来源路由，经 httptest 暴露；CLI 经真实 HTTP 调它。
type kernel struct {
	t   *testing.T
	st  *store.Store
	au  *auth.Auth
	wh  *webhook.Service
	url string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, core.Module, registry.Module, webhook.Module,
		projects.Module, clock.Module, items.Module, runner.Module, rules.Module, proc.Module, runidentity.Module} {
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
	if err := registry.Register(x, registry.Options{Bundled: []string{"fetch-feed"}}); err != nil {
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
	grp, err := apisources.Mount(st, x, au, wh, in)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{grp}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &kernel{t, st, au, wh, ts.URL}
}

func (k *kernel) token(kind auth.Kind) string {
	k.t.Helper()
	var secret string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) (err error) {
		_, secret, err = k.au.Issue(tx, auth.Spec{Kind: kind})
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
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": token, "APA_SESSION": "s1", "APA_SESSION_KIND": "direct", "APA_HOME": k.t.TempDir()}
	var out, errb bytes.Buffer
	code := client.Main(append([]string{"--evidence", "owner: go ahead"}, args...), client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: k.t.TempDir(),
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

// ACT-CMD-01：delegate 带自定义命令登记来源得 20 并留痕，来源未登记；ACT-CMD-05：附带拉取程序和 webhook 可登记。
func TestAddRejectsCustomCommandAndAcceptsBundledProgram(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)

	for _, args := range [][]string{
		{"source", "add", "--id", "src_evil", "--command", "/tmp/acc/evil.sh", "--schedule", "15m", "--timeout", "30"},
		{"source", "add", "--id", "src_evil", "--type", "script", "--command", "/tmp/acc/evil.sh", "--schedule", "15m", "--timeout", "30"},
	} {
		if r := k.apa(dlg, args...); r.code != domain.ExitWebOnly || !strings.Contains(r.errOut, `"web_only"`) {
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

	var feed, hook domain.Source
	decode(t, k.apa(dlg, "source", "add", "--id", "src_feed", "--type", "pull", "--command", "fetch-feed", "--schedule", "15m", "--timeout", "60",
		"--config", `{"url":"http://x"}`), &feed)
	decode(t, k.apa(dlg, "source", "add", "--id", "src_hook", "--type", "webhook", "--source-type", "forge", "--config", `{"scheme":"shared"}`), &hook)
	if feed.Command != "fetch-feed" || !feed.Enabled || hook.Type != domain.SourceWebhook || hook.SourceKind != "forge" {
		t.Fatalf("registered: %+v %+v", feed, hook)
	}
	if r := k.apa(obs, "source", "add", "--id", "src_o", "--type", "webhook"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer add: %+v", r)
	}
	if r := k.apa(dlg, "source", "add", "--id", "src_feed", "--type", "pull", "--command", "fetch-feed", "--schedule", "15m", "--timeout", "60"); r.code != domain.ExitConflict {
		t.Fatalf("duplicate: %+v", r)
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

// list/show 对 observer 开放；ACT-SRC-10：disable/enable 翻转 enabled，其余字段不动。
func TestListShowEnableDisable(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.apa(dlg, "source", "add", "--id", "src_feed", "--type", "pull", "--command", "fetch-feed", "--schedule", "15m", "--timeout", "60")

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

// ACT-HOOK-01：create 返回 URL 与只显示一次的密钥并记 Event，observer 得 4；ACT-HOOK-11：rotate 换密钥，并存期内新旧都收，之后旧的失效。
// ingests 展示原始 payload 与状态；ACT-HOOK-10：replay 重放同一条入站记录，代数加一并记 Event。
func TestWebhookCreateRotateIngestsReplay(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.apa(dlg, "source", "add", "--id", "src_hook", "--type", "webhook", "--config", `{"scheme":"shared"}`)

	if r := k.apa(obs, "source", "webhook", "create", "src_hook"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer create: %+v", r)
	}
	var k1 struct {
		Secret, URL string
		Rotated     bool
	}
	decode(t, k.apa(dlg, "source", "webhook", "create", "src_hook"), &k1)
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
	decode(t, k.apa(dlg, "source", "webhook", "rotate", "src_hook"), &k2)
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
	for _, args := range [][]string{{"source", "replay", "ing_none"}, {"source", "ingests", "src_none"}, {"source", "webhook", "create", "src_none"}} {
		if r := k.apa(dlg, args...); r.code != domain.ExitNotFound {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	k.apa(dlg, "source", "add", "--id", "src_feed", "--type", "pull", "--command", "fetch-feed", "--schedule", "15m", "--timeout", "60")
	if r := k.apa(dlg, "source", "webhook", "create", "src_feed"); r.code != domain.ExitUsage {
		t.Fatalf("webhook key for pull source: %+v", r)
	}
	for _, args := range [][]string{{"source", "webhook"}, {"source", "webhook", "bogus", "src_hook"}, {"source", "webhook", "create"}, {"source", "ingests"}, {"source", "replay"}} {
		if r := k.apa(dlg, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}

// 附录 B.1 `apa source webhook revoke`：delegate 吊销该来源全部接收密钥（含宽限期内的旧密钥）并记 Event，之后接收被拒；observer 得 4。
func TestWebhookRevokeDropsEveryKey(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.apa(dlg, "source", "add", "--id", "src_hook", "--type", "webhook", "--config", `{"scheme":"shared"}`)
	var k1, k2 struct{ Secret string }
	decode(t, k.apa(dlg, "source", "webhook", "create", "src_hook"), &k1)
	decode(t, k.apa(dlg, "source", "webhook", "rotate", "src_hook"), &k2)
	recv := func(key, delivery string) int {
		return k.wh.Receive(context.Background(), webhook.Request{SourceID: "src_hook",
			Header: map[string]string{"X-Hook-Key": key, "X-Delivery": delivery}, Body: []byte(`{"n":1}`)}).Status
	}
	if recv(k1.Secret, "d1") != 200 || recv(k2.Secret, "d2") != 200 {
		t.Fatal("both keys must work before revoke")
	}
	k.wh.Wait()
	if r := k.apa(obs, "source", "webhook", "revoke", "src_hook"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer revoke: %+v", r)
	}
	var rv struct{ Revoked int }
	decode(t, k.apa(dlg, "source", "webhook", "revoke", "src_hook"), &rv)
	if rv.Revoked != 2 || k.events("source.webhook_key_revoked") == 0 {
		t.Fatalf("revoke: %+v", rv)
	}
	if recv(k1.Secret, "d3") == 200 || recv(k2.Secret, "d4") == 200 {
		t.Fatal("revoked keys must be rejected")
	}
	if r := k.apa(dlg, "source", "webhook", "revoke", "src_none"); r.code != domain.ExitNotFound {
		t.Fatalf("revoke unknown source: %+v", r)
	}
}

// `apa source rejects <id>`：observer 可读被拒收的输出行，--after/--limit 翻页；未登记来源得 3，缺 id 或多余参数得 2。
func TestRejectsListsAndPages(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	k.apa(dlg, "source", "add", "--id", "src_feed", "--type", "pull", "--command", "fetch-feed", "--schedule", "15m", "--timeout", "60")
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
// 默认项目不存在时返回 not_found，不静默丢弃。
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
	if r := k.apa(dlg, append(base, "--default-project", "nope")...); r.code != domain.ExitNotFound {
		t.Fatalf("unknown project: %+v", r)
	}
	if r := k.apa(dlg, append(base, "--default-project", "acc-home")...); r.code != 0 {
		t.Fatalf("add: %+v", r)
	}
	var s domain.Source
	decode(t, k.apa(k.token(auth.Observer), "source", "show", "src_rss"), &s)
	if s.DefaultProject != "acc-home" || s.Outbound.Local != "full" || s.Outbound.External != nil {
		t.Fatalf("show: %+v", s)
	}
}

// ACT-BKL-02、ACT-BKL-04：`apa source backlog list` 列出积压且旧原因原样；`rules` 缺本人原话得 23、observer 得 4，积压不变；
// 带原话时返回 attached/disposed/remaining（此内核没有来源规则，全部留在积压里）。
func TestBacklogListAndRulesCommands(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	var hook domain.Source
	decode(t, k.apa(dlg, "source", "add", "--id", "src_hook", "--type", "webhook", "--source-type", "forge", "--config", `{"scheme":"shared"}`), &hook)
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

	// 不带 --evidence：直接用 client 跑，k.apa 总是带原话。
	noWords := func(token string) int {
		vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": token, "APA_SESSION": "s1", "APA_SESSION_KIND": "direct", "APA_HOME": t.TempDir()}
		return client.Main([]string{"source", "backlog", "rules"}, client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: t.TempDir(),
			Stdin: strings.NewReader(""), Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, HTTP: http.DefaultClient})
	}
	if code := noWords(dlg); code != domain.ExitBasisRequired {
		t.Fatalf("rules without words: %d", code)
	}
	if r := k.apa(obs, "source", "backlog", "rules"); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer rules: %+v", r)
	}
	var res struct{ Attached, Disposed, Remaining int }
	decode(t, k.apa(dlg, "source", "backlog", "rules"), &res)
	if res.Attached != 0 || res.Disposed != 0 || res.Remaining != 3 {
		t.Fatalf("rules: %+v", res)
	}
	for _, args := range [][]string{{"source", "backlog"}, {"source", "backlog", "score"}, {"source", "backlog", "list", "extra"}} {
		if r := k.apa(obs, args...); r.code != domain.ExitUsage {
			t.Fatalf("%v: %+v", args, r)
		}
	}
}
