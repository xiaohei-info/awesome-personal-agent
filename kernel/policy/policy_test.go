package policy

import (
	"context"
	"encoding/json"
	"errors"
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

// flow-web-only：默认类别、权限上限 read、白名单与人格为空；规则扩展落入新版本。
func TestDefaultsAndWebOnlyRules(t *testing.T) {
	s := open(t)
	v, p := current(t, s)
	if v != 0 || p.PermissionCap.Default != PermRead || p.Persona != "" ||
		len(p.ForwardAllowlist) != 0 || len(p.ChannelAnswerers) != 0 {
		t.Fatalf("defaults: v=%d %+v", v, p)
	}
	want := []string{CategoryExternalCommitment, CategoryFunds, CategoryIrreversible, CategoryPermissionChange, CategorySpecActivation}
	if strings.Join(p.WebOnly.Categories, ",") != strings.Join(want, ",") {
		t.Fatalf("categories = %v", p.WebOnly.Categories)
	}

	v = must(t, s, func(p *Policy) error {
		p.WebOnly.Rules = append(p.WebOnly.Rules, WebOnlyRule{Kind: "decision", Tag: "tax", Category: CategoryFunds})
		return nil
	})
	if v != 1 {
		t.Fatalf("version = %d", v)
	}
	_, p = current(t, s)
	if len(p.WebOnly.Rules) != 1 || p.WebOnly.Rules[0].Tag != "tax" {
		t.Fatalf("rules = %+v", p.WebOnly.Rules)
	}

	for name, bad := range map[string]func(*Policy) error{
		"rule matches nothing": func(p *Policy) error { p.WebOnly.Rules = []WebOnlyRule{{Category: CategoryFunds}}; return nil },
		"unknown category":     func(p *Policy) error { p.WebOnly.Rules = []WebOnlyRule{{Kind: "x", Category: "nope"}}; return nil },
		"no categories":        func(p *Policy) error { p.WebOnly.Categories = nil; return nil },
	} {
		if _, err := update(t, s, owner, bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if v, _ := current(t, s); v != 1 {
		t.Fatalf("rejected change moved version to %d", v)
	}
}

// web_only 分类只能在 Web 由本人改：agent、能力包、CLI 一律退出码 20，且不留版本与事件。
func TestOnlyOwnerOnWebCanWrite(t *testing.T) {
	s := open(t)
	for _, a := range []domain.Actor{agent, pack, cli} {
		_, err := update(t, s, a, func(p *Policy) error { p.Persona = "pirate"; return nil })
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeWebOnly || de.ExitCode() != domain.ExitWebOnly {
			t.Fatalf("%v: err = %v", a.Via, err)
		}
	}
	if v, _ := current(t, s); v != 0 || len(events(t, s)) != 0 {
		t.Fatal("rejected write left a version or event")
	}
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
		func(p *Policy) error { p.MaterialWrite.Kinds["minutes"] = WriteProposed; return nil },
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
	if p.Disturb.Routes["decision:urgent"] != "wake" || p.MaterialWrite.Kinds["minutes"] != WriteProposed ||
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
		"material": func(p *Policy) error { p.MaterialWrite.Default = "maybe"; return nil },
		"url":      func(p *Policy) error { p.GenerateEndpoint.URL = "ftp://x"; return nil },
		"timeout":  func(p *Policy) error { p.ScreeningTimeoutMs = 0; return nil },
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
		p.JudgeEndpoint != nil || p.ScreeningTimeoutMs != 45000 {
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
	if _, ok := m["model_endpoint"]; ok {
		t.Fatalf("new version still has model_endpoint: %s", d[1])
	}
	for _, k := range []string{"judge_endpoint", "generate_endpoint", "screening_timeout_ms"} {
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
	if p.JudgeEndpoint != nil || p.GenerateEndpoint.URL != "" || p.ScreeningTimeoutMs != 30000 {
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
		j.Locality != LocalityExternal || p.GenerateEndpoint.Locality != LocalityExternal {
		t.Fatalf("judge=%+v generate=%+v", j, p.GenerateEndpoint)
	}
	for _, e := range events(t, s) {
		if strings.Contains(string(e.Event.Payload), "sk-j") {
			t.Fatalf("api key leaked: %s", e.Event.Payload)
		}
	}
	for name, bad := range map[string]func(*Policy){
		"judge url":      func(p *Policy) { p.JudgeEndpoint.URL = "ftp://x" },
		"judge protocol": func(p *Policy) { p.JudgeEndpoint.Protocol = "magic" },
		"logprobs model": func(p *Policy) { p.JudgeEndpoint.Protocol = ProtocolChatLogprobs; p.JudgeEndpoint.Model = "" },
		"judge locality": func(p *Policy) { p.JudgeEndpoint.Locality = "nearby" },
		"judge batch":    func(p *Policy) { p.JudgeEndpoint.MaxBatch = -1 },
		"gen url":        func(p *Policy) { p.GenerateEndpoint.URL = "ftp://x" },
		"gen locality":   func(p *Policy) { p.GenerateEndpoint.Locality = "nearby" },
		"timeout":        func(p *Policy) { p.ScreeningTimeoutMs = 0 },
	} {
		if _, err := update(t, s, owner, func(p *Policy) error { bad(p); return nil }); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}
