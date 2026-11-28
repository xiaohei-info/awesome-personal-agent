package doctor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

var bg = context.Background()
var webOwner = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}

func newStore(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, executors.Module,
		delivery.Module, registry.Module, runner.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func upd(t *testing.T, st *store.Store, f func(*store.Tx) error) {
	t.Helper()
	if err := st.Update(bg, f); err != nil {
		t.Fatal(err)
	}
}

func setPolicy(t *testing.T, st *store.Store, f func(*policy.Policy)) {
	t.Helper()
	upd(t, st, func(tx *store.Tx) error {
		_, err := policy.Update(tx, webOwner, "test", func(p *policy.Policy) error { f(p); return nil })
		return err
	})
}

func addSource(t *testing.T, st *store.Store, id, typ, kind, cmd, external, stats string) {
	t.Helper()
	upd(t, st, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path,
			dedupe_domain, config, cursor, enabled, stats, default_project, outbound_local, outbound_external)
			VALUES (?, ?, ?, ?, '1m', 30, '', '', '', '', 1, ?, '', 'full', ?)`, id, typ, kind, cmd, stats, external)
		return err
	})
}

func byName(cs []Check, n string) Check {
	for _, c := range cs {
		if c.Name == n {
			return c
		}
	}
	return Check{}
}

type fakePacks struct {
	rej []packs.Rejection
	src packs.Source
}

func (f fakePacks) Health() []packs.Rejection          { return f.rej }
func (f fakePacks) Source(string) (packs.Source, bool) { return f.src, f.src != "" }
func (f fakePacks) List() []*packs.Pack                { return []*packs.Pack{{Name: "a"}} }

// ACT-DOC-05：形态按"有什么"判断，未配置不是错误，并指出到下一档缺什么。
func TestTierReportsLevelAndWhatIsMissing(t *testing.T) {
	st := newStore(t)
	f, err := ReadFacts(bg, st)
	if err != nil {
		t.Fatal(err)
	}
	c := tier(f)
	requireCodes(t, "codes.go", c)
	if c.Data["tier"] != 1 || c.Code != CodeTierKernelOnly || c.Status != OK || !strings.Contains(c.Next, "generate endpoint") {
		t.Fatalf("tier 1 expected: %+v", c)
	}
	setPolicy(t, st, func(p *policy.Policy) {
		p.GenerateEndpoint = policy.GenerateEndpoint{URL: "http://x/v1", Model: "m", Locality: "local"}
	})
	f, _ = ReadFacts(bg, st)
	if c := tier(f); c.Data["tier"] != 2 || c.Code != CodeTierNoHost || !strings.Contains(c.Next, "Agent plugin") {
		t.Fatalf("tier 2 expected: %+v", c)
	}
	f.Declarations = []executors.Record{{Harness: "h", Declaration: executors.Declaration{Headless: &executors.Headless{}}}}
	if c := tier(f); c.Data["tier"] != 3 || c.Code != CodeTierFull {
		t.Fatalf("tier 3 expected: %+v", c)
	}
}

// ACT-DOC-06：正常端点两项为正常且示例不含账本内容；self_reported 与协议不符为错误。
func TestModelEndpointProbesSampleAndFlagsBadProtocol(t *testing.T) {
	cls, gen := modelstub.NewCls(), modelstub.New()
	defer cls.Close()
	defer gen.Close()
	gen.SetDefault(modelstub.Reply{Content: `{"answer":"yes","sufficient":true}`})
	st := newStore(t)
	setPolicy(t, st, func(p *policy.Policy) {
		p.JudgeEndpoint = &policy.JudgeEndpoint{URL: cls.URL(), Protocol: policy.ProtocolProbability, Locality: "local"}
		p.GenerateEndpoint = policy.GenerateEndpoint{URL: gen.URL(), Model: "m", Locality: "local"}
	})
	f, _ := ReadFacts(bg, st)
	e := Env{Model: &model.Client{}}
	c := modelEndpoint(bg, f, e)
	requireCodes(t, "codes.go", c)
	if c.Status != OK || c.Code != CodeModelsOK || len(c.Details) != 2 || c.Details[0].Code != CodeOK || c.Details[0].Data["elapsed_ms"] == nil {
		t.Fatalf("healthy endpoints: %+v", c)
	}
	if len(cls.Batches()) != 1 || len(gen.Requests()) != 1 {
		t.Fatalf("one sample request each expected: %d %d", len(cls.Batches()), len(gen.Requests()))
	}
	if !strings.Contains(cls.Batches()[0].BodyText, "Is 7 greater than 3") {
		t.Fatalf("sample question missing: %s", cls.Batches()[0].BodyText)
	}
	for name, fault := range map[string]modelstub.ClsFault{
		"self_reported": {Source: "self_reported"},
		"protocol":      {Protocol: "other/9"},
	} {
		cls.SetFault(fault)
		c := modelEndpoint(bg, f, e)
		d, _ := c.Find("judge_endpoint")
		requireCodes(t, "codes.go", c)
		if c.Status != Error || c.Code != CodeModelsFailing || d.Status != Error {
			t.Fatalf("%s must be an error: %+v", name, c)
		}
		if name == "self_reported" && !strings.Contains(d.Why, "self_reported") {
			t.Fatalf("reason: %s", d.Why)
		}
	}
	cls.SetFault(modelstub.ClsFault{})
	gen.SetDefault(modelstub.Reply{Status: 500})
	if d, _ := modelEndpoint(bg, f, e).Find("generate_endpoint"); d.Status != Error || !strings.Contains(d.Why, "500") {
		t.Fatalf("generate 500: %+v", d)
	}
	gen.SetDefault(modelstub.Reply{Content: "not json"})
	if d, _ := modelEndpoint(bg, f, e).Find("generate_endpoint"); d.Status != Warning {
		t.Fatalf("schema violation is a warning: %+v", d)
	}
	// 未配置不是错误。
	f.Policy.JudgeEndpoint, f.Policy.GenerateEndpoint = nil, policy.GenerateEndpoint{}
	if c := modelEndpoint(bg, f, e); c.Status != OK || c.Code != CodeModelsNone || c.Details[0].Code != CodeNotConfigured || c.Details[1].Code != CodeNotConfigured {
		t.Fatalf("unconfigured: %+v", c)
	}
}

// ACT-DOC-07：命令缺失、外发范围未选、外部全文外发都被指出；每条带最近一次运行结果。
func TestSourcesFlagMissingCommandUnsetScopeAndFullTextOutbound(t *testing.T) {
	st := newStore(t)
	setPolicy(t, st, func(p *policy.Policy) {
		p.GenerateEndpoint = policy.GenerateEndpoint{URL: "http://x/v1", Model: "m", Locality: "external"}
	})
	okStats := `{"runs":2,"last_success_at":"2026-11-12T10:00:00Z"}`
	addSource(t, st, "src_ok", "script", "", "good", "excerpt", okStats)
	addSource(t, st, "src_missing", "script", "", "nope", "meta", "")
	addSource(t, st, "src_unset", "script", "", "good", "", "")
	addSource(t, st, "src_full", "script", "", "good", "full", `{"runs":3,"consecutive_failures":2,"last_failure_at":"2026-11-12T10:00:00Z","last_error":"PRIVATE-STDERR"}`)
	f, err := ReadFacts(bg, st)
	if err != nil {
		t.Fatal(err)
	}
	c := sources(f, Env{CommandExists: func(cmd string) bool { return cmd == "good" }})
	requireCodes(t, "codes.go", c)
	want := map[string]struct {
		st   Status
		sub  string
		code string
	}{
		"src_ok": {OK, "", CodeSourceOK}, "src_missing": {Error, "command does not exist", CodeSourceCommandMissing},
		"src_unset": {Warning, "not chosen", CodeSourceOutboundUnset}, "src_full": {Warning, "full text", CodeSourceOutboundFull},
	}
	if c.Code != CodeSourcesAttention || c.Data["count"] != 4 {
		t.Errorf("sources check: %+v", c)
	}
	for id, w := range want {
		d, ok := c.Find(id)
		if !ok || d.Status != w.st || d.Code != w.code || !strings.Contains(d.Why, w.sub) || d.Data["last_run"] == nil {
			t.Errorf("%s: %+v", id, d)
		}
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "PRIVATE-STDERR") {
		t.Fatal("source error text must not reach the report")
	}
	if d, _ := c.Find("src_full"); d.Data["last_run"] != "failed" || fmt.Sprint(d.Data["problems"]) != "[source_outbound_full source_last_failed]" || d.Data["consecutive_failures"] != 2 {
		t.Fatalf("last run: %+v", d)
	}
	if d, _ := c.Find("src_full"); d.Data["last_failure_kind"] != "" || !strings.Contains(d.Next, "temporary failure, retried automatically with backoff: read last_error on the source page") {
		t.Fatalf("next step: %+v", d)
	}
	// 没有外部端点时，外部范围不起作用。
	f.Policy.GenerateEndpoint.Locality = "local"
	if d, _ := sources(f, Env{CommandExists: func(string) bool { return true }}).Find("src_unset"); d.Status != OK {
		t.Fatalf("local endpoint: %+v", d)
	}
}

func TestCommandExistsResolvesPathsAndNames(t *testing.T) {
	if !CommandExists("sh -c x") || CommandExists("definitely-not-a-command-xyz") || CommandExists("") || CommandExists("/nonexistent/bin") {
		t.Fatal("CommandExists")
	}
	if !CommandExists("/bin/sh") {
		t.Fatal("absolute path")
	}
}

// 老结构账本（没有外发范围列）上，来源按迁移规则读出：外部范围未选择。
func TestLegacySourcesWithoutScopeColumns(t *testing.T) {
	st := newStore(t)
	upd(t, st, func(tx *store.Tx) error {
		_, err := tx.Exec(`DROP TABLE sources_registry_source`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`CREATE TABLE sources_registry_source (id TEXT PRIMARY KEY, type TEXT, source_type TEXT, command TEXT, enabled INTEGER, stats TEXT)`)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO sources_registry_source VALUES ('src_old','script','','cmd',1,'')`)
		return err
	})
	got, err := LegacySources(bg, st)
	if err != nil || len(got) != 1 || got[0].Outbound.External != nil || got[0].Outbound.Local != "full" {
		t.Fatalf("%+v %v", got, err)
	}
	// 结构 4/5/7：有显示名和外发范围，只缺保留天数列，升级预检仍能列出来源。
	st3 := newStore(t)
	addSource(t, st3, "src_v7", "script", "", "c", "meta", "")
	upd(t, st3, func(tx *store.Tx) error {
		_, err := tx.Exec(`ALTER TABLE sources_registry_source DROP COLUMN content_retention_days`)
		return err
	})
	if got, err := LegacySources(bg, st3); err != nil || len(got) != 1 || got[0].ID != "src_v7" || got[0].Outbound.External == nil {
		t.Fatalf("%+v %v", got, err)
	}
	// 新结构直接读。
	st2 := newStore(t)
	addSource(t, st2, "src_new", "script", "", "c", "meta", "")
	if got, err := LegacySources(bg, st2); err != nil || got[0].Outbound.External == nil {
		t.Fatalf("%+v %v", got, err)
	}
}

// ACT-DOC-08：被拒的用户包逐字段给出原因，并说明同名内置包生效。
func TestPackCheckRelaysFieldReasonsAndFallbackSource(t *testing.T) {
	rej := packs.Rejection{Pack: "inbound-triage", Source: packs.Data, Reason: "template judge: field confidence_threshold removed; escalate_to removed; on_undecided missing"}
	c := packCheck(Env{Packs: fakePacks{rej: []packs.Rejection{rej}, src: packs.Builtin}})
	d, _ := c.Find("inbound-triage")
	requireCodes(t, "codes.go", c)
	if c.Status != Error || c.Code != CodePacksRejected || d.Code != CodePackRejected || d.Data["in_effect"] != "builtin" || !strings.Contains(d.Why, "confidence_threshold") || !strings.Contains(d.Why, "builtin pack of the same name is in effect") {
		t.Fatalf("%+v", c)
	}
	if c := packCheck(Env{Packs: fakePacks{}}); c.Status != OK || c.Code != CodePacksOK {
		t.Fatalf("%+v", c)
	}
	if c := packCheck(Env{}); c.Status != OK || c.Code != CodePacksNoLoader {
		t.Fatalf("%+v", c)
	}
}

// ACT-DOC-11：渠道 ext 最近领取时间与投递积压；导出 ext 最近读取时间。
func TestChannelsReportLastClaimAndBacklog(t *testing.T) {
	st := newStore(t)
	var chanKey, exportKey auth.Credential
	upd(t, st, func(tx *store.Tx) (err error) {
		a := auth.New(nil)
		if chanKey, _, err = a.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "channel.tg"}); err != nil {
			return err
		}
		exportKey, _, err = a.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "export"})
		return err
	})
	_ = chanKey
	_ = exportKey
	q := delivery.New(delivery.Options{})
	upd(t, st, func(tx *store.Tx) error {
		for _, id := range []string{"d1", "d2", "d3"} {
			if _, _, err := q.Create(tx, domain.Delivery{ID: id, Item: "it", Route: delivery.ChannelRoute("tg"), Version: 1,
				Class: domain.DeliveryReminder, Content: domain.DeliveryContent{Title: "t"}}); err != nil {
				return err
			}
		}
		return nil
	})
	seen := NewSeen(time.Second)
	t0 := time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
	if err := seen.Note(bg, st, SubjectExt+"channel.tg", t0); err != nil {
		t.Fatal(err)
	}
	f, err := ReadFacts(bg, st)
	if err != nil {
		t.Fatal(err)
	}
	c := channels(f, Env{})
	requireCodes(t, "codes.go", c)
	d, _ := c.Find("channel.tg")
	if d.Status != Warning || d.Code != CodeChannelBacklog || c.Code != CodeChannelsBacklog || d.Data["backlog"] != 3 || d.Data["last_seen_at"] != "2026-11-12T10:00:00Z" {
		t.Fatalf("stopped ext: %+v", d)
	}
	if e, _ := c.Find("export"); e.Status != OK || e.Code != CodeExportOK || !strings.Contains(e.Why, "never") {
		t.Fatalf("export: %+v", e)
	}
	// ext 启动并领完：积压为 0，领取时间更新。
	upd(t, st, func(tx *store.Tx) error {
		for i := 0; i < 3; i++ {
			d, err := q.Claim(tx, delivery.ChannelRoute("tg"))
			if err != nil {
				return err
			}
			if _, _, err := q.Ack(tx, d.ID, d.Version, "ref"); err != nil {
				return err
			}
		}
		return nil
	})
	// 窗口内重复记录被合并，窗口外更新。
	if err := seen.Note(bg, st, SubjectExt+"channel.tg", t0.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if err := seen.Note(bg, st, SubjectExt+"channel.tg", t0.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	f, _ = ReadFacts(bg, st)
	c = channels(f, Env{})
	d, _ = c.Find("channel.tg")
	if d.Status != OK || d.Code != CodeChannelOK || c.Code != CodeChannelsOK || d.Data["backlog"] != 0 || d.Data["last_seen_at"] != "2026-11-12T10:01:00Z" {
		t.Fatalf("after claim: %+v", d)
	}
	// 没有 ext 密钥的路由上有积压：提醒。
	f.ExtKeys, f.Backlog = nil, map[string]int{"channel:lost": 2}
	if c := channels(f, Env{}); c.Status != Warning || len(c.Details) != 1 || c.Details[0].Code != CodeChannelOrphan {
		t.Fatalf("orphan: %+v", c)
	}
	if c := channels(Facts{}, Env{}); c.Status != OK || c.Code != CodeChannelsNone {
		t.Fatalf("empty: %+v", c)
	}
}

func TestAssembleOrdersChecksAndComputesOK(t *testing.T) {
	r := Assemble(Check{Name: Channels, Status: OK}, Check{Name: "zzz", Status: OK}, Check{Name: DataDir, Status: Warning})
	if !r.OK || r.Checks[0].Name != DataDir || r.Checks[1].Name != Channels || r.Checks[2].Name != "zzz" {
		t.Fatalf("%+v", r)
	}
	if Assemble(Check{Name: Config, Status: Error}).OK {
		t.Fatal("error must clear OK")
	}
	all := Kernel(bg, Facts{Policy: policy.Default()}, Env{})
	if len(all) != 6 || byName(all, Tier).Name == "" || byName(all, Authz).Status != OK {
		t.Fatalf("%+v", all)
	}
	requireCodes(t, "codes.go", all...)
	if c := byName(all, Sources); c.Code != CodeSourcesNone {
		t.Fatalf("no sources: %+v", c)
	}
	// 账本里还留着旧权限模型的东西（策略旧段或有效授权单）而没切换：错误。
	if c := authzCheck(Facts{ActiveGrants: 1}); c.Status != Error || c.Code != CodeAuthzNotSwitched {
		t.Fatalf("unswitched: %+v", c)
	}
	if c := authzCheck(Facts{AuthzSwitched: true, LegacyPolicy: []string{"web_only"}}); c.Status != Error {
		t.Fatalf("legacy sections left: %+v", c)
	}
}

// 地区不可用（451，或说明地区受限的 403）有独立的结论码，不显示成协议错误或鉴权失败；普通 403 仍是鉴权失败。
func TestProbeReportsRegionUnavailable(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{451, `{"title":"Not available in your region.","status":451}`, CodeRegionUnavailable},
		{403, `{"error":{"message":"unsupported_country_region_territory"}}`, CodeRegionUnavailable},
		{403, `{"error":{"message":"forbidden"}}`, CodeUnauthorized},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		}))
		e := Env{Model: &model.Client{}}
		j := ProbeJudge(bg, &policy.JudgeEndpoint{URL: srv.URL + "/v1", Protocol: policy.ProtocolSystemOne, Model: "m", TimeoutMs: 5000}, e)
		g := ProbeGenerate(bg, policy.GenerateEndpoint{URL: srv.URL + "/v1", Model: "m"}, policy.Thinking{}, e)
		srv.Close()
		if j.Status != Error || j.Code != c.want || g.Code != c.want {
			t.Fatalf("%d %s: judge=%+v generate=%+v", c.status, c.body, j, g)
		}
		if c.want == CodeRegionUnavailable && (!strings.Contains(j.Next, "gateway") || strings.Contains(j.Why, "protocol")) {
			t.Fatalf("region text: %+v", j)
		}
	}
}

// 生成端点探测固定低档（写作项调高也不跟，免得超时）；写作项不指定时不发参数。
func TestProbeGenerateAsksForLowThinking(t *testing.T) {
	for write, want := range map[string]bool{"": true, "high": true, policy.ThinkingOff: false} {
		var body string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			body = string(b)
			w.WriteHeader(http.StatusBadRequest)
		}))
		ProbeGenerate(bg, policy.GenerateEndpoint{URL: srv.URL + "/v1", Model: "m", Protocol: policy.ProtocolResponses}, policy.Thinking{Write: write}, Env{Model: &model.Client{}})
		srv.Close()
		if body == "" || strings.Contains(body, `"reasoning":{"effort":"low"}`) != want || strings.Contains(body, `"reasoning"`) != want {
			t.Fatalf("write %q: %s", write, body)
		}
	}
}

// 暂停模型（§8.7、§15.5）：运行形态与模型端点两项都是正常，结论写"已暂停"，不向端点发探测。
func TestPausedModelsAreReportedAsPausedWithoutProbing(t *testing.T) {
	st := newStore(t)
	gen := modelstub.New()
	t.Cleanup(gen.Close)
	setPolicy(t, st, func(p *policy.Policy) {
		p.GenerateEndpoint = policy.GenerateEndpoint{URL: gen.URL(), Model: "m", Locality: "local"}
		p.ModelsPaused = true
	})
	f, err := ReadFacts(bg, st)
	if err != nil {
		t.Fatal(err)
	}
	if c := tier(f); c.Status != OK || c.Code != CodeTierPaused || c.Data["tier"] != 1 || c.Data["models_paused"] != true || !strings.Contains(c.Why, "paused") {
		t.Fatalf("tier while paused: %+v", c)
	}
	if c := modelEndpoint(bg, f, Env{Model: &model.Client{}}); c.Status != OK || c.Code != CodeModelsPaused || len(c.Details) != 0 || !strings.Contains(c.Why, "paused") {
		t.Fatalf("model endpoint while paused: %+v", c)
	}
	if n := len(gen.Requests()); n != 0 {
		t.Fatalf("doctor probed a paused endpoint %d times", n)
	}
}

func builtinPacks(t *testing.T) *packs.Loader {
	t.Helper()
	ld := packs.New(packs.Options{Builtin: rootpacks.FS, DataDir: t.TempDir(), Kinds: items.NewKinds()})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load builtin packs: %v %v", rej, err)
	}
	return ld
}

// 自检用决策卡片文案的真实模板探测生成端点：请求带的是模板自己的结构约束（不是内置小样例）。
// 端点答不了真实约束时，自检不能再报"正常"。
func TestProbeGenerateUsesTheDraftTemplate(t *testing.T) {
	ld := builtinPacks(t)
	for _, c := range []struct {
		name, reply string
		status      Status
		code, shape string
	}{
		{"valid", `{"insufficient_info":false,"title":"选择发布时间","question":"何时正式发布？","options":[{"id":"A","label":"今晚发布","consequence":"今晚值班观察"},{"id":"B","label":"明天上午发布","consequence":"延迟一夜"}]}`, OK, CodeOK, ""},
		{"fenced", "```json\n{\"insufficient_info\":false}\n```", Error, CodeDraftSchema, model.ShapeFence},
		{"prose", "I could not draft this one.", Error, CodeDraftSchema, model.ShapePlainText},
		{"empty", "", Error, CodeDraftSchema, model.ShapeEmpty},
		{"wrong type", `{"insufficient_info":"no"}`, Error, CodeDraftSchema, ""},
		{"required field missing", `{"title":"a"}`, Error, CodeDraftSchema, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			gen := modelstub.New()
			defer gen.Close()
			gen.SetDefault(modelstub.Reply{Content: c.reply})
			d := ProbeGenerate(bg, policy.GenerateEndpoint{URL: gen.URL(), Model: "m", Locality: "local"}, policy.Thinking{}, Env{Model: &model.Client{}, Packs: ld})
			if d.Status != c.status || d.Code != c.code || d.Data["shape"] != nilIfEmpty(c.shape) || d.Data["sample"] != "draft" {
				t.Fatalf("%+v", d)
			}
			sent := gen.Requests()[0].BodyText
			if !strings.Contains(sent, "insufficient_info") || !strings.Contains(sent, "consequence") || !strings.Contains(sent, "itm_doctor_sample") {
				t.Fatalf("probe did not carry the template's constraints: %s", sent)
			}
		})
	}
}

func nilIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// 没有起草模板可用（包没加载）时退回内置小样例，不因此报错。
func TestProbeGenerateFallsBackToTheBuiltinSample(t *testing.T) {
	gen := modelstub.New()
	defer gen.Close()
	gen.SetDefault(modelstub.Reply{Content: `{"answer":"yes","sufficient":true}`})
	d := ProbeGenerate(bg, policy.GenerateEndpoint{URL: gen.URL(), Model: "m", Locality: "local"}, policy.Thinking{}, Env{Model: &model.Client{}, Packs: fakePacks{}})
	if d.Status != OK || d.Data["sample"] != "builtin" {
		t.Fatalf("%+v", d)
	}
}

// 外部端点下 Inbox 对话捕获未选外发范围时，sources 自检给出 warning（subject=capture:<inboxID>）；本机端点不告警。
func TestDoctorWarnsExternalEndpointWithUnsetInboxCaptureOutbound(t *testing.T) {
	inbox := &domain.Project{ID: "prj_inbox", Name: "Inbox", Status: domain.ProjectActive, Capture: true, SystemRole: domain.ProjectRoleInbox}
	extPol := policy.Policy{GenerateEndpoint: policy.GenerateEndpoint{URL: "https://api.example/v1", Model: "m", Locality: policy.LocalityExternal}}
	c := sources(Facts{Policy: extPol, Inbox: inbox}, Env{})
	if c.Status != Warning || c.Code != CodeSourcesAttention || len(c.Details) != 1 || c.Details[0].Subject != "capture:prj_inbox" || c.Details[0].Code != CodeSourceOutboundUnset {
		t.Fatalf("external unset inbox capture should warn: %+v", c)
	}
	localPol := policy.Policy{GenerateEndpoint: policy.GenerateEndpoint{URL: "http://127.0.0.1:8000/v1", Model: "m", Locality: policy.LocalityLocal}}
	c2 := sources(Facts{Policy: localPol, Inbox: inbox}, Env{})
	if c2.Status != OK || c2.Code != CodeSourcesNone {
		t.Fatalf("local endpoint should not warn on unset capture_outbound: %+v", c2)
	}
}
