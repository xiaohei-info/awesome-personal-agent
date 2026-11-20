package policy

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

var (
	owner = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}
	agent = domain.Actor{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI}
	pack  = domain.Actor{Type: domain.ActorSystem, Principal: "pack", Via: domain.ViaRule}
	cli   = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaCLI}
)

func open(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func update(t *testing.T, s *store.Store, a domain.Actor, fn func(*Policy) error) (int, error) {
	t.Helper()
	var v int
	err := s.Update(ctx, func(tx *store.Tx) (err error) {
		v, err = Update(tx, a, "test", fn)
		return
	})
	return v, err
}

func must(t *testing.T, s *store.Store, fn func(*Policy) error) int {
	t.Helper()
	v, err := update(t, s, owner, fn)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func current(t *testing.T, s *store.Store) (int, Policy) {
	t.Helper()
	var (
		v int
		p Policy
	)
	if err := s.View(ctx, func(tx *store.Tx) (err error) { v, p, err = Current(tx); return }); err != nil {
		t.Fatal(err)
	}
	return v, p
}

func events(t *testing.T, s *store.Store) []eventlog.Entry {
	t.Helper()
	var out []eventlog.Entry
	if err := s.View(ctx, func(tx *store.Tx) (err error) { out, err = eventlog.Read(tx, 0, 100); return }); err != nil {
		t.Fatal(err)
	}
	return out
}

func types(es []eventlog.Entry) (out []string) {
	for _, e := range es {
		out = append(out, e.Event.Type)
	}
	return
}

// flow-forward-allowlist：两份白名单默认空、相互独立，变更立即生效并记两类事件。
func TestAllowlistsIndependent(t *testing.T) {
	s := open(t)
	must(t, s, func(p *Policy) error {
		p.ForwardAllowlist = map[string][]string{"mail": {"alice", "alice", " bob ", ""}}
		return nil
	})
	_, p := current(t, s)
	if got := strings.Join(p.ForwardAllowlist["mail"], ","); got != "alice,bob" || len(p.ChannelAnswerers) != 0 {
		t.Fatalf("forward=%v answerers=%v", p.ForwardAllowlist, p.ChannelAnswerers)
	}
	must(t, s, func(p *Policy) error { p.ChannelAnswerers = map[string][]string{"chat": {"me"}}; return nil })
	_, p = current(t, s)
	if len(p.ForwardAllowlist["mail"]) != 2 || p.ChannelAnswerers["chat"][0] != "me" {
		t.Fatalf("lists not independent: %+v %+v", p.ForwardAllowlist, p.ChannelAnswerers)
	}
	// 清空发送者即移除该来源，立即生效。
	must(t, s, func(p *Policy) error { p.ForwardAllowlist["mail"] = nil; return nil })
	if _, p = current(t, s); len(p.ForwardAllowlist) != 0 {
		t.Fatalf("not removed: %v", p.ForwardAllowlist)
	}
	if got := strings.Join(types(events(t, s)), ","); got != "policy.changed,allowlist.changed,policy.changed,allowlist.changed,policy.changed,allowlist.changed" {
		t.Fatalf("events = %s", got)
	}
}

// flow-delivery-control：打扰策略、材料写入策略、权限上限、模型端点每次变更都是新版本并记 policy.changed。
func TestEachChangeIsNewVersionWithEvent(t *testing.T) {
	s := open(t)
	steps := []func(*Policy) error{
		func(p *Policy) error { p.Disturb.Routes["decision:urgent"] = "wake"; return nil },
		func(p *Policy) error { p.AgentTier.ByAgent["agent_a"] = AgentAllowance{Tier: AllowDecide}; return nil },
		func(p *Policy) error { p.PermissionCap.ByHarness["h1"] = PermEdit; return nil },
		func(p *Policy) error {
			p.GenerateEndpoint = GenerateEndpoint{URL: "http://127.0.0.1:9/v1", Model: "m", APIKey: "sk-secret"}
			return nil
		},
	}
	for i, fn := range steps {
		if v := must(t, s, fn); v != i+1 {
			t.Fatalf("step %d version = %d", i, v)
		}
	}
	_, p := current(t, s)
	if p.Disturb.Routes["decision:urgent"] != "wake" || p.Allowance("agent_a").Tier != AllowDecide ||
		p.PermissionCap.ByHarness["h1"] != PermEdit || p.GenerateEndpoint.APIKey != "sk-secret" {
		t.Fatalf("policy = %+v", p)
	}
	es := events(t, s)
	if len(es) != 4 {
		t.Fatalf("events = %v", types(es))
	}
	for i, e := range es {
		var pl struct {
			Version, Previous int
			Sections          []string
		}
		if e.Event.Type != domain.EvPolicyChanged || e.Event.Object != Object || json.Unmarshal(e.Event.Payload, &pl) != nil ||
			pl.Version != i+1 || pl.Previous != i || len(pl.Sections) != 1 {
			t.Fatalf("event %d = %+v payload=%s", i, e.Event, e.Event.Payload)
		}
	}
	if strings.Contains(string(es[3].Event.Payload), "sk-secret") {
		t.Fatal("api key leaked into event")
	}

	// 无变化不产生版本与事件。
	if v := must(t, s, func(p *Policy) error { return nil }); v != 4 || len(events(t, s)) != 4 {
		t.Fatalf("no-op bumped version=%d events=%d", v, len(events(t, s)))
	}
	for name, bad := range map[string]func(*Policy) error{
		"cap":      func(p *Policy) error { p.PermissionCap.Default = "root"; return nil },
		"route":    func(p *Policy) error { p.Disturb.Routes["mystery"] = "x"; return nil },
		"priority": func(p *Policy) error { p.Disturb.Routes["decision:meh"] = "x"; return nil },
		"tier":     func(p *Policy) error { p.AgentTier.ByAgent["a"] = AgentAllowance{Tier: "boss"}; return nil },
		"url":      func(p *Policy) error { p.GenerateEndpoint.URL = "ftp://x"; return nil },
		"mutate":   func(p *Policy) error { return errors.New("boom") },
	} {
		if _, err := update(t, s, owner, bad); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if v, _ := current(t, s); v != 4 {
		t.Fatalf("version = %d", v)
	}
}

// 人格超过约 400 token 拒绝保存；恰好在限内可存，且只写入 policy.changed。
func TestPersonaLimit(t *testing.T) {
	s := open(t)
	ok := strings.Repeat("好", MaxPersonaTokens)
	if v := must(t, s, func(p *Policy) error { p.Persona = ok; return nil }); v != 1 {
		t.Fatalf("version = %d", v)
	}
	for _, long := range []string{ok + "好", strings.Repeat("word ", 500)} {
		if _, err := update(t, s, owner, func(p *Policy) error { p.Persona = long; return nil }); !errors.Is(err, ErrInvalid) {
			t.Fatalf("err = %v", err)
		}
	}
	if _, p := current(t, s); p.Persona != ok {
		t.Fatal("persona changed by rejected write")
	}
	if got := strings.Join(types(events(t, s)), ","); got != "policy.changed" {
		t.Fatalf("events = %s", got)
	}
}

// 内置默认语气：空白人格用默认（中英各一份，取不到按英文）；自定义优先；恢复默认只是把人格清空；默认文本不超上限且不含权限类内容；
// 默认不写进账本，所以空白人格的策略文档里仍是空串。
func TestDefaultPersona(t *testing.T) {
	zh, en := DefaultPersona("zh_CN.UTF-8"), DefaultPersona("en")
	if zh == en || DefaultPersona("") != en || DefaultPersona("fr") != en || DefaultPersona("ZH") != zh {
		t.Fatalf("lang pick: zh=%q en=%q", zh, en)
	}
	for _, d := range []string{zh, en} {
		if n := EstimateTokens(d); n == 0 || n > MaxPersonaTokens {
			t.Fatalf("default persona is %d tokens, limit %d: %q", n, MaxPersonaTokens, d)
		}
	}
	s := open(t)
	_, p := current(t, s)
	if p.Persona != "" || PersonaText(p, "zh") != PersonaDeclaration+"\n"+zh {
		t.Fatalf("fresh ledger: persona %q, text %q", p.Persona, PersonaText(p, "zh"))
	}
	must(t, s, func(p *Policy) error { p.Persona = "mine"; return nil })
	_, p = current(t, s)
	if PersonaText(p, "zh") != PersonaDeclaration+"\nmine" {
		t.Fatalf("custom must win: %q", PersonaText(p, "zh"))
	}
	must(t, s, func(p *Policy) error { p.Persona = ""; return nil })
	_, p = current(t, s)
	if PersonaText(p, "en") != PersonaDeclaration+"\n"+en {
		t.Fatalf("restore default: %q", PersonaText(p, "en"))
	}
}

// 事务回滚时版本与事件一并回滚（同一事务提交）。
func TestRollbackDropsVersionAndEvent(t *testing.T) {
	s := open(t)
	boom := errors.New("boom")
	err := s.Update(ctx, func(tx *store.Tx) error {
		if _, err := Update(tx, owner, "x", func(p *Policy) error { p.Persona = "hi"; return nil }); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if v, _ := current(t, s); v != 0 || len(events(t, s)) != 0 {
		t.Fatal("rollback leaked state")
	}
}

// Update 的调用方持有的快照不会被变更函数污染，历史版本留在表里不被改写。
func TestHistoryAppendOnly(t *testing.T) {
	s := open(t)
	must(t, s, func(p *Policy) error { p.Persona = "one"; return nil })
	must(t, s, func(p *Policy) error { p.Persona = "two"; return nil })
	var docs []string
	err := s.View(ctx, func(tx *store.Tx) error {
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
	})
	if err != nil || len(docs) != 2 || !strings.Contains(docs[0], `"persona":"one"`) || !strings.Contains(docs[1], `"persona":"two"`) {
		t.Fatalf("docs=%v err=%v", docs, err)
	}
}

func TestEstimateTokens(t *testing.T) {
	if EstimateTokens("") != 0 || EstimateTokens("abcd") != 1 || EstimateTokens("你好") != 2 || EstimateTokens("a b") != 1 {
		t.Fatal("estimate off")
	}
}

func TestDefaultIsNormalized(t *testing.T) {
	n, err := normalize(Default())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(n)
	b, _ := json.Marshal(Default())
	if string(a) != string(b) {
		t.Fatalf("default not a fixed point:\n%s\n%s", a, b)
	}
}

func TestDisturbQuietAndMergeWindowAreValidated(t *testing.T) {
	if d := Default().Disturb; d.MergeWindowSec != 1800 || !d.RemindOnDeadline || d.Quiet != nil {
		t.Fatalf("defaults: %+v", d)
	}
	for name, f := range map[string]func(*Policy){
		"negative window": func(p *Policy) { p.Disturb.MergeWindowSec = -1 },
		"empty quiet":     func(p *Policy) { p.Disturb.Quiet = &Quiet{Start: "08:00", End: "08:00"} },
		"bad zone":        func(p *Policy) { p.Disturb.Quiet = &Quiet{Start: "22:00", End: "07:00", Zone: "Nowhere/X"} },
	} {
		p := Default()
		f(&p)
		if _, err := normalize(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s accepted: %v", name, err)
		}
	}
	p := Default()
	p.Disturb.Quiet = &Quiet{Start: "22:00", End: "07:00", Zone: "Asia/Shanghai"}
	if _, err := normalize(p); err != nil {
		t.Fatal(err)
	}
}

// 升级前写下的策略文档没有 daily_cap：读出来取缺省 20（已有部署升级后日上限即生效），静音为空列表。
func TestStoredDocWithoutDailyCapDefaultsTo20(t *testing.T) {
	doc := func(cap string) string {
		b, _ := json.Marshal(Default())
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		d := m["disturb"].(map[string]any)
		delete(d, "daily_cap")
		delete(d, "mutes")
		if cap != "" {
			d["daily_cap"] = json.RawMessage(cap)
		}
		out, _ := json.Marshal(m)
		return string(out)
	}
	p, _, err := decode(doc(""))
	if err != nil {
		t.Fatal(err)
	}
	if p.Disturb.DailyCap != DefaultDailyCap || p.Disturb.Mutes == nil || len(p.Disturb.Mutes) != 0 {
		t.Fatalf("%+v", p.Disturb)
	}
	q, _, err := decode(doc("0"))
	if err != nil || q.Disturb.DailyCap != 0 {
		t.Fatalf("explicit 0 must stay unlimited: %v %+v", err, q.Disturb)
	}
}

// 升级前的策略文档没有 main_session_launch：读作两份空表（都没选，按声明与无头运行），不改写存储；取值只能是两种拉起方式。
func TestMainSessionLaunchOldDocAndValues(t *testing.T) {
	p, _, err := decode(legacyDoc)
	if err != nil || p.MainSessionLaunch.ByHarness == nil || p.MainSessionLaunch.ByRule == nil || len(p.MainSessionLaunch.ByHarness)+len(p.MainSessionLaunch.ByRule) != 0 {
		t.Fatalf("old doc: %v %+v", err, p.MainSessionLaunch)
	}
	ok := Default()
	ok.MainSessionLaunch.ByHarness["hx"], ok.MainSessionLaunch.ByRule["p/event_rules/r"] = LaunchWake, LaunchHeadless
	if _, err := normalize(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []map[string]string{{"hx": "chat"}, {"": "wake"}} {
		q := Default()
		q.MainSessionLaunch.ByRule = bad
		if _, err := normalize(q); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}

// 旧账本里存的是 v0.1 的单一 model_endpoint 文档（flow-upgrade-rollback / flow-endpoint-config）。
const legacyDoc = `{"web_only":{"categories":["funds"],"rules":[]},"disturb":{"routes":{"decision":"inject"},"merge_window_sec":0,"remind_on_deadline":true},
"material_write":{"default":"proposed","kinds":{}},"permission_cap":{"default":"read","by_harness":{}},
"model_endpoint":{"url":"http://127.0.0.1:9/v1","model":"old-m","api_key":"sk-old","screening_timeout_ms":45000},
"persona":"","forward_allowlist":{},"channel_answerers":{},"channels":{},"default_harness":""}`

func seedLegacy(t *testing.T, s *store.Store) {
	t.Helper()
	if err := s.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO policy_version (version, doc, at) VALUES (1, ?, 1)", legacyDoc)
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

func storedDocs(t *testing.T, s *store.Store) (docs []string) {
	t.Helper()
	if err := s.View(ctx, func(tx *store.Tx) error {
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
	return
}

// 旧 model_endpoint 读作生成端点（位置 external），超时提到顶层，判断端点为空；读取不产生新版本、不改写旧文档。
func TestLegacyModelEndpointReadsAsGenerateEndpoint(t *testing.T) {
	s := open(t)
	seedLegacy(t, s)
	v, p := current(t, s)
	g := p.GenerateEndpoint
	if v != 1 || g.URL != "http://127.0.0.1:9/v1" || g.Model != "old-m" || g.APIKey != "sk-old" || g.Locality != LocalityExternal ||
		p.JudgeEndpoint != nil {
		t.Fatalf("v=%d policy=%+v", v, p)
	}
	if d := storedDocs(t, s); len(d) != 1 || d[0] != legacyDoc {
		t.Fatalf("stored docs rewritten: %v", d)
	}
}

// 本人在 Web 不改任何值直接保存：仍追加一个新版本，写成新字段，旧版本逐字节不变。
func TestSaveOverLegacyWritesNewFieldsAndKeepsOldVersion(t *testing.T) {
	s := open(t)
	seedLegacy(t, s)
	if v := must(t, s, func(*Policy) error { return nil }); v != 2 {
		t.Fatalf("version = %d, want 2", v)
	}
	d := storedDocs(t, s)
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(d[1]), &m); err != nil {
		t.Fatal(err)
	}
	// 旧形状与已取消的段（初筛超时等）不再写进新版本。
	for _, k := range []string{"model_endpoint", "screening_timeout_ms", "web_only", "material_write"} {
		if _, ok := m[k]; ok {
			t.Fatalf("new version still has %s: %s", k, d[1])
		}
	}
	for _, k := range []string{"judge_endpoint", "generate_endpoint", "agent_tier"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("new version lacks %s: %s", k, d[1])
		}
	}
	if d[0] != legacyDoc {
		t.Fatal("version 1 rewritten")
	}
	// 再保存一次没有变化，不再产生版本。
	if v := must(t, s, func(*Policy) error { return nil }); v != 2 {
		t.Fatalf("second no-op save bumped version to %d", v)
	}
}

// 端点校验与默认值：判断端点缺省字段取附录 B 的默认值；未声明位置按 external；密钥不进事件。
func TestEndpointDefaultsAndValidation(t *testing.T) {
	s := open(t)
	_, p := current(t, s)
	if p.JudgeEndpoint != nil || p.GenerateEndpoint.URL != "" {
		t.Fatalf("defaults = %+v", p)
	}
	must(t, s, func(p *Policy) error {
		p.JudgeEndpoint = &JudgeEndpoint{URL: "http://127.0.0.1:9/judge", APIKey: "sk-j"}
		p.GenerateEndpoint = GenerateEndpoint{URL: "http://127.0.0.1:9/gen", Model: "m"}
		return nil
	})
	_, p = current(t, s)
	j := p.JudgeEndpoint
	if j.Protocol != ProtocolProbability || j.TimeoutMs != 10000 || j.MaxBatch != 32 || j.MaxMaterialChars != 8000 ||
		j.Locality != LocalityExternal || p.GenerateEndpoint.Locality != LocalityExternal || p.GenerateEndpoint.MaterialBudget() != 24000 {
		t.Fatalf("judge=%+v generate=%+v", j, p.GenerateEndpoint)
	}
	for _, e := range events(t, s) {
		if strings.Contains(string(e.Event.Payload), "sk-j") {
			t.Fatalf("api key leaked: %s", e.Event.Payload)
		}
	}
	for name, bad := range map[string]func(*Policy){
		"judge url":        func(p *Policy) { p.JudgeEndpoint.URL = "ftp://x" },
		"judge protocol":   func(p *Policy) { p.JudgeEndpoint.Protocol = "magic" },
		"logprobs model":   func(p *Policy) { p.JudgeEndpoint.Protocol = ProtocolChatLogprobs; p.JudgeEndpoint.Model = "" },
		"system-one model": func(p *Policy) { p.JudgeEndpoint.Protocol = ProtocolSystemOne; p.JudgeEndpoint.Model = "" },
		"judge locality":   func(p *Policy) { p.JudgeEndpoint.Locality = "nearby" },
		"judge batch":      func(p *Policy) { p.JudgeEndpoint.MaxBatch = -1 },
		"gen url":          func(p *Policy) { p.GenerateEndpoint.URL = "ftp://x" },
		"gen locality":     func(p *Policy) { p.GenerateEndpoint.Locality = "nearby" },
	} {
		if _, err := update(t, s, owner, func(p *Policy) error { bad(p); return nil }); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// 暂停模型（§8.7）：旧文档没有 models_paused 即不暂停；暂停是一次策略变更；Current 里两个端点视为未配置，
// Stored 原样保留端点与密钥；恢复后 Current 立即给回原配置；没有端点时规整为不暂停。
func TestModelsPausedKeepsEndpointsButCurrentSeesNone(t *testing.T) {
	s := open(t)
	seedLegacy(t, s)
	if _, p := current(t, s); p.ModelsPaused {
		t.Fatal("a stored document without models_paused must read as not paused")
	}
	must(t, s, func(p *Policy) error {
		p.JudgeEndpoint = &JudgeEndpoint{URL: "http://127.0.0.1:9/judge", APIKey: "cls-key"}
		return nil
	})
	v := must(t, s, func(p *Policy) error { p.ModelsPaused = true; return nil })
	if es := events(t, s); es[len(es)-1].Event.Type != domain.EvPolicyChanged || !strings.Contains(string(es[len(es)-1].Event.Payload), `"models_paused"`) {
		t.Fatalf("pausing must be a policy.changed naming models_paused: %+v", es[len(es)-1].Event)
	}
	if _, p := current(t, s); !p.ModelsPaused || p.GenerateEndpoint.URL != "" || p.JudgeEndpoint != nil {
		t.Fatalf("paused Current must see no endpoint: %+v", p)
	}
	var stored Policy
	if err := s.View(ctx, func(tx *store.Tx) (err error) { _, stored, err = Stored(tx); return }); err != nil {
		t.Fatal(err)
	}
	if !stored.ModelsPaused || stored.GenerateEndpoint.APIKey != "sk-old" || stored.JudgeEndpoint == nil || stored.JudgeEndpoint.APIKey != "cls-key" {
		t.Fatalf("Stored must keep endpoints and keys while paused: %+v", stored)
	}
	// 暂停期间改端点：写入口基于存储原文，密钥不丢，仍是暂停。
	must(t, s, func(p *Policy) error { p.GenerateEndpoint.Model = "new-m"; return nil })
	if err := s.View(ctx, func(tx *store.Tx) (err error) { _, stored, err = Stored(tx); return }); err != nil {
		t.Fatal(err)
	}
	if !stored.ModelsPaused || stored.GenerateEndpoint.APIKey != "sk-old" || stored.GenerateEndpoint.Model != "new-m" {
		t.Fatalf("editing while paused: %+v", stored.GenerateEndpoint)
	}
	if got := must(t, s, func(p *Policy) error { p.ModelsPaused = false; return nil }); got != v+2 {
		t.Fatalf("resume version = %d, want %d", got, v+2)
	}
	if _, p := current(t, s); p.ModelsPaused || p.GenerateEndpoint.APIKey != "sk-old" || p.JudgeEndpoint == nil {
		t.Fatalf("resumed Current must give the configuration back: %+v", p)
	}
	// 两个端点都撤掉：暂停没有意义，规整为不暂停。
	must(t, s, func(p *Policy) error {
		p.ModelsPaused, p.GenerateEndpoint, p.JudgeEndpoint = true, GenerateEndpoint{}, nil
		return nil
	})
	if _, p := current(t, s); p.ModelsPaused {
		t.Fatal("models_paused must normalize to false without any endpoint")
	}
}

func TestNoteVaultsValidationAndRender(t *testing.T) {
	s := open(t)
	set := func(vs ...NoteVault) error {
		_, err := update(t, s, owner, func(p *Policy) error { p.NoteVaults = vs; return nil })
		return err
	}
	for name, vs := range map[string][]NoteVault{
		"duplicate name":    {{Name: "a", OpenURL: "x://{vault}"}, {Name: "a"}},
		"no protocol":       {{Name: "a", OpenURL: "{vault}/{path}"}},
		"leading space":     {{Name: "a", OpenURL: " obsidian://open"}},
		"javascript":        {{Name: "a", OpenURL: "JavaScript:alert({path})"}},
		"data":              {{Name: "a", OpenURL: "data:text/html,{path}"}},
		"vbscript":          {{Name: "a", OpenURL: "vbscript:x"}},
		"file":              {{Name: "a", OpenURL: "file:///{path}"}},
		"slash in name":     {{Name: "a/b"}},
		"empty name":        {{Name: " "}},
		"newline in scheme": {{Name: "a", OpenURL: "java\nscript:x"}},
	} {
		if err := set(vs...); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if err := set(NoteVault{Name: "notes", OpenURL: "obsidian://open?vault={vault}&file={path}"}, NoteVault{Name: "bare"}); err != nil {
		t.Fatal(err)
	}
	var p Policy
	_ = s.View(ctx, func(tx *store.Tx) (err error) { _, p, err = Current(tx); return })
	if got := p.OpenURL("notes", "项目/a b.md"); got != "obsidian://open?vault=notes&file=%E9%A1%B9%E7%9B%AE%2Fa%20b.md" {
		t.Fatalf("open url = %q", got)
	}
	if got := p.OpenURL("notes", "a&b=c+d.md"); got != "obsidian://open?vault=notes&file=a%26b%3Dc%2Bd.md" {
		t.Fatalf("query delimiters in the path must be encoded: %q", got)
	}
	if p.OpenURL("bare", "x.md") != "" || p.OpenURL("gone", "x.md") != "" {
		t.Fatal("a vault without open_url or an unregistered vault has no open url")
	}
	var evs int
	_ = s.View(ctx, func(tx *store.Tx) error {
		es, _ := eventlog.Query(tx, eventlog.Filter{Type: domain.EvPolicyChanged}, 0, 100)
		evs = len(es)
		return nil
	})
	if evs != 1 {
		t.Fatalf("policy.changed events = %d, want 1", evs)
	}
	// 旧版本策略文档没有 note_vaults：读作没有登记。
	if err := s.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO policy_version (version, doc, at) VALUES (99, ?, 0)", `{"web_only":{"categories":["funds"],"rules":[]},"disturb":{"routes":{"decision":"inject"},"merge_window_sec":0},"material_write":{"default":"proposed","kinds":{}},"permission_cap":{"default":"read","by_harness":{}},"screening_timeout_ms":1000}`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	_ = s.View(ctx, func(tx *store.Tx) (err error) { _, p, err = Current(tx); return })
	if p.NoteVaults == nil || len(p.NoteVaults) != 0 {
		t.Fatalf("old document should read as no vaults, got %#v", p.NoteVaults)
	}
}

// TestOpenURLMatchesWebPreview：内核渲染与 Web 预览（settings-vaults 的 render）跑同一份用例，逐字一致。
func TestOpenURLMatchesWebPreview(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "web", "src", "features", "settings-vaults", "open-url-cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct{ Template, Vault, Path, Want string }
	if err := json.Unmarshal(raw, &cases); err != nil || len(cases) == 0 {
		t.Fatalf("cases: %v (%d)", err, len(cases))
	}
	for _, c := range cases {
		p := Policy{NoteVaults: []NoteVault{{Name: c.Vault, OpenURL: c.Template}}}
		if got := p.OpenURL(c.Vault, c.Path); got != c.Want {
			t.Errorf("%q in %q: got %q, want %q", c.Path, c.Vault, got, c.Want)
		}
	}
}
