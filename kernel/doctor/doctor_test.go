package doctor

import (
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
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
	if c.Data["tier"] != 1 || c.Status != OK || !strings.Contains(c.Next, "generate endpoint") {
		t.Fatalf("tier 1 expected: %+v", c)
	}
	setPolicy(t, st, func(p *policy.Policy) {
		p.GenerateEndpoint = policy.GenerateEndpoint{URL: "http://x/v1", Model: "m", Locality: "local"}
	})
	f, _ = ReadFacts(bg, st)
	if c := tier(f); c.Data["tier"] != 2 || !strings.Contains(c.Next, "host plugin") {
		t.Fatalf("tier 2 expected: %+v", c)
	}
	f.Declarations = []executors.Record{{Harness: "h", Declaration: executors.Declaration{Headless: &executors.Headless{}}}}
	if c := tier(f); c.Data["tier"] != 3 {
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
	if c.Status != OK || len(c.Details) != 2 || c.Details[0].Data["elapsed_ms"] == nil {
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
		if c.Status != Error || d.Status != Error {
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
	if c := modelEndpoint(bg, f, e); c.Status != OK {
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
	want := map[string]struct {
		st  Status
		sub string
	}{
		"src_ok": {OK, ""}, "src_missing": {Error, "command does not exist"},
		"src_unset": {Warning, "not chosen"}, "src_full": {Warning, "full text"},
	}
	for id, w := range want {
		d, ok := c.Find(id)
		if !ok || d.Status != w.st || !strings.Contains(d.Why, w.sub) || d.Data["last_run"] == nil {
			t.Errorf("%s: %+v", id, d)
		}
	}
	b, _ := json.Marshal(c)
	if strings.Contains(string(b), "PRIVATE-STDERR") {
		t.Fatal("source error text must not reach the report")
	}
	if d, _ := c.Find("src_full"); d.Data["last_run"] != "failed" {
		t.Fatalf("last run: %+v", d)
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
	if c.Status != Error || !strings.Contains(d.Why, "confidence_threshold") || !strings.Contains(d.Why, "builtin pack of the same name is in effect") {
		t.Fatalf("%+v", c)
	}
	if c := packCheck(Env{Packs: fakePacks{}}); c.Status != OK {
		t.Fatalf("%+v", c)
	}
	if c := packCheck(Env{}); c.Status != OK {
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
	d, _ := c.Find("channel.tg")
	if d.Status != Warning || d.Data["backlog"] != 3 || d.Data["last_seen_at"] != "2026-11-12T10:00:00Z" {
		t.Fatalf("stopped ext: %+v", d)
	}
	if e, _ := c.Find("export"); e.Status != OK || !strings.Contains(e.Why, "never") {
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
	d, _ = channels(f, Env{}).Find("channel.tg")
	if d.Status != OK || d.Data["backlog"] != 0 || d.Data["last_seen_at"] != "2026-11-12T10:01:00Z" {
		t.Fatalf("after claim: %+v", d)
	}
	// 没有 ext 密钥的路由上有积压：提醒。
	f.ExtKeys, f.Backlog = nil, map[string]int{"channel:lost": 2}
	if c := channels(f, Env{}); c.Status != Warning || len(c.Details) != 1 {
		t.Fatalf("orphan: %+v", c)
	}
	if c := channels(Facts{}, Env{}); c.Status != OK {
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
	if len(all) != 5 || byName(all, Tier).Name == "" {
		t.Fatalf("%+v", all)
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
		g := ProbeGenerate(bg, policy.GenerateEndpoint{URL: srv.URL + "/v1", Model: "m"}, e)
		srv.Close()
		if j.Status != Error || j.Code != c.want || g.Code != c.want {
			t.Fatalf("%d %s: judge=%+v generate=%+v", c.status, c.body, j, g)
		}
		if c.want == CodeRegionUnavailable && (!strings.Contains(j.Next, "gateway") || strings.Contains(j.Why, "protocol")) {
			t.Fatalf("region text: %+v", j)
		}
	}
}
