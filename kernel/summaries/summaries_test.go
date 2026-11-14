package summaries

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	builtin "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

// fakeGen 按模板回固定结果；err 非空时每次都失败。
type fakeGen struct {
	err   error
	calls []direct.Call
}

func (f *fakeGen) Do(_ context.Context, c direct.Call) (direct.Outcome, error) {
	f.calls = append(f.calls, c)
	if f.err != nil {
		return direct.Outcome{}, f.err
	}
	res := `{"insufficient_info":false,"summary":"一句话：等供应商报价"}`
	if c.Template == TemplateRevision {
		res = `{"insufficient_info":false,"summary":"结论由 A 改为 B","effect_on_rules":"按 B 执行"}`
	}
	return direct.Outcome{Settled: true, Steps: []direct.Step{{Result: json.RawMessage(res)}}}, nil
}

// 一句话摘要与材料修订摘要：成功时以 system 身份、带 Basis 经命令写回；模型未配置时不写、不告警；
// 失败时不写、记 kind=decision-card 的 health.alert（§5.6）。已有摘要、proposed 事项不起草，重跑不重复调用。
func TestDraftsItemAndRevisionSummaries(t *testing.T) {
	for name, tc := range map[string]struct {
		err           error
		item, rev     string
		alerts, calls int
	}{
		"written back":   {item: "一句话：等供应商报价", rev: "结论由 A 改为 B（对 agent 行为的影响：按 B 执行）", calls: 2},
		"not configured": {err: model.ErrNotConfigured, calls: 2},
		"failed":         {err: direct.ErrUnavailable, alerts: 2, calls: 2},
	} {
		t.Run(name, func(t *testing.T) {
			reg := store.NewRegistry()
			for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
				items.Module, materials.Module, Module} {
				if err := reg.Register(m); err != nil {
					t.Fatal(err)
				}
			}
			st, err := store.Open(t.TempDir(), reg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { st.Close() })
			x := command.New(st, time.Now)
			if err := errors.Join(projects.Register(x), items.Register(x, items.NewKinds()), materials.Register(x)); err != nil {
				t.Fatal(err)
			}
			gen := &fakeGen{err: tc.err}
			s, err := New(Options{Store: st, Exec: x, Gen: gen})
			if err != nil {
				t.Fatal(err)
			}
			do := func(cmd string, in any) json.RawMessage {
				b, _ := json.Marshal(in)
				r, err := x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner"})
				if err != nil {
					t.Fatalf("%s: %v", cmd, err)
				}
				return r.Value
			}
			var ref struct{ ID string }
			_ = json.Unmarshal(do("item.create", map[string]any{"title": "采购服务器", "kind": "task", "phase": "open", "body": "等报价"}), &ref)
			do("item.create", map[string]any{"title": "已有摘要", "kind": "task", "phase": "open", "summary": "s"})
			do("item.create", map[string]any{"title": "建议", "kind": "task", "phase": "proposed"})
			var pid struct{ ID string }
			_ = json.Unmarshal(do("project.create", map[string]any{"name": "P"}), &pid)
			var mr materials.Result
			_ = json.Unmarshal(do("material.commit", map[string]any{"project": pid.ID, "kind": "minutes", "title": "周会", "content": "结论 A", "reason": "r"}), &mr)

			for i := 0; i < 2; i++ {
				if err := s.Drain(bg); err != nil {
					t.Fatal(err)
				}
			}
			if len(gen.calls) != tc.calls {
				t.Fatalf("calls = %d, want %d: %+v", len(gen.calls), tc.calls, gen.calls)
			}
			for _, c := range gen.calls {
				if c.Actor.Type != domain.ActorSystem || c.Basis == nil || !strings.HasPrefix(c.Basis.RuleID, Pack+"/") {
					t.Fatalf("call must be recorded as system with basis: %+v", c)
				}
			}
			var alerts int
			if err := st.View(bg, func(tx *store.Tx) error {
				it, err := items.Get(tx, ref.ID)
				if err != nil {
					return err
				}
				if it.Summary != tc.item {
					t.Errorf("item summary = %q, want %q", it.Summary, tc.item)
				}
				v, err := materials.GetRevision(tx, mr.Revision)
				if err != nil {
					return err
				}
				if v.Summary != tc.rev {
					t.Errorf("revision summary = %q, want %q", v.Summary, tc.rev)
				}
				es, err := eventlog.ReadObject(tx, ref.ID)
				if err != nil {
					return err
				}
				if last := es[len(es)-1].Event; tc.item != "" && (last.Type != domain.EvItemUpdated || last.Actor.Type != domain.ActorSystem || last.Basis == nil) {
					t.Errorf("write-back event %+v", last)
				}
				as, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, 0, 10)
				for _, a := range as {
					// 带 template：健康视图凭它在同一模板之后成功调用时清除告警。
					if pl := string(a.Event.Payload); strings.Contains(pl, `"kind":"decision-card"`) && strings.Contains(pl, `-summary"`) {
						alerts++
					}
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if alerts != tc.alerts {
				t.Fatalf("alerts = %d, want %d", alerts, tc.alerts)
			}
		})
	}
}

// 只改关联键或关系（item.relate，item.updated 不带 before）不触发起草：标题与正文没变，
// 积压"只跑规则"挂靠消息时不能因此多出模型调用（§9.3 积压放开第 1 步，需要模型：否）。
func TestRelateDoesNotDraftSummary(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, materials.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	if err := errors.Join(projects.Register(x), items.Register(x, items.NewKinds()), materials.Register(x)); err != nil {
		t.Fatal(err)
	}
	gen := &fakeGen{err: model.ErrNotConfigured} // 不写回：事项的摘要一直为空
	s, err := New(Options{Store: st, Exec: x, Gen: gen})
	if err != nil {
		t.Fatal(err)
	}
	do := func(cmd string, in any) json.RawMessage {
		b, _ := json.Marshal(in)
		r, err := x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner"})
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return r.Value
	}
	var ref struct{ ID string }
	_ = json.Unmarshal(do("item.create", map[string]any{"title": "采购服务器", "kind": "task", "phase": "open", "body": "等报价"}), &ref)
	if err := s.Drain(bg); err != nil || len(gen.calls) != 1 {
		t.Fatalf("created item should be drafted once: calls=%d err=%v", len(gen.calls), err)
	}
	do("item.relate", map[string]any{"from": ref.ID, "key": "followmsg:m1", "domain": "followmsg", "reason": "attach"})
	if err := s.Drain(bg); err != nil || len(gen.calls) != 1 {
		t.Fatalf("relate must not draft again: calls=%d err=%v", len(gen.calls), err)
	}
	do("item.update", map[string]any{"id": ref.ID, "title": "采购服务器（改）", "reason": "retitle"})
	if err := s.Drain(bg); err != nil || len(gen.calls) != 2 {
		t.Fatalf("title change must draft: calls=%d err=%v", len(gen.calls), err)
	}
}

// 挂靠与来源刷新是 followmsg 写的：标题与正文变了也不起草（积压只跑规则零模型调用，§9.3、§9.4）。
func TestCandidatesSkipFollowmsgWrites(t *testing.T) {
	upd := func(actor, title string) eventlog.Entry {
		p, _ := json.Marshal(map[string]any{"before": map[string]string{"Title": "a"}, "item": map[string]string{"Title": title}})
		return eventlog.Entry{Event: domain.Event{Type: domain.EvItemUpdated, Object: actor + title, Payload: p,
			Actor: domain.Actor{Type: domain.ActorSystem, Principal: actor}}}
	}
	got, _ := candidates([]eventlog.Entry{upd(followmsg.Principal, "b"), upd("owner", "b"), upd("owner", "a")})
	if len(got) != 1 || got[0] != "ownerb" {
		t.Fatalf("candidates = %v, want only the owner's title change", got)
	}
}

// 外发范围拦下起草（direct.ErrOutboundBlocked）：摘要留空、不写回、不记 health.alert（未判定积压已在健康视图提示本人）。
func TestOutboundBlockedLeavesSummaryEmptyWithoutAlert(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, materials.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	if err := errors.Join(projects.Register(x), items.Register(x, items.NewKinds()), materials.Register(x)); err != nil {
		t.Fatal(err)
	}
	gen := &fakeGen{err: direct.ErrOutboundBlocked}
	s, err := New(Options{Store: st, Exec: x, Gen: gen})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{"title": "t", "kind": "task", "phase": "open", "body": "synthetic body"})
	r, err := x.Do(bg, command.Request{Identity: owner, Command: "item.create", Input: b, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	var ref struct{ ID string }
	_ = json.Unmarshal(r.Value, &ref)
	if err := s.Drain(bg); err != nil {
		t.Fatal(err)
	}
	if len(gen.calls) != 1 {
		t.Fatalf("calls = %d, want 1", len(gen.calls))
	}
	if err := st.View(bg, func(tx *store.Tx) error {
		it, err := items.Get(tx, ref.ID)
		if err != nil || it.Summary != "" {
			t.Errorf("summary %q err %v, want empty", it.Summary, err)
		}
		as, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, 0, 10)
		if len(as) != 0 {
			t.Errorf("blocked draft recorded %d health alerts", len(as))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// 真实统一出口 + 后台游标异步时序：事项出自来源、来源 external 范围未选、生成端点在外部时，
// 无论 Drain 在事件落库之前还是之后到达，外部生成端点都收不到任何请求；本人选定范围后的下一次变更才发出请求。
func TestDrainSendsNothingToExternalEndpointWhileScopeNotChosen(t *testing.T) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, materials.Module, proc.Module, registry.Module, direct.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	if err := errors.Join(projects.Register(x), items.Register(x, items.NewKinds()), materials.Register(x)); err != nil {
		t.Fatal(err)
	}
	stub := modelstub.New()
	t.Cleanup(stub.Close)
	stub.SetDefault(modelstub.Reply{Content: `{"insufficient_info":false,"summary":"一句话"}`})
	ld := packs.New(packs.Options{Builtin: builtin.FS, Kinds: items.NewKinds()})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %+v", err, rej)
	}
	gen, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}})
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Options{Store: st, Exec: x, Gen: gen})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, func(tx *store.Tx) error {
		if _, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error {
			p.GenerateEndpoint = policy.GenerateEndpoint{URL: stub.URL(), Model: "m", APIKey: "k", Locality: policy.LocalityExternal}
			return nil
		}); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sources_registry_source
			(id, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, default_project, outbound_local, outbound_external, config, cursor, enabled, stats)
			VALUES ('src_a', 'pull', 'k', '', '', 0, '', '', '', 'full', '', '', '', 1, '')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	do := func(cmd string, in any) json.RawMessage {
		b, _ := json.Marshal(in)
		r, err := x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner"})
		if err != nil {
			t.Fatalf("%s: %v", cmd, err)
		}
		return r.Value
	}
	drain := func() {
		t.Helper()
		if err := s.Drain(bg); err != nil {
			t.Fatal(err)
		}
	}
	var ref struct{ ID string }
	_ = json.Unmarshal(do("item.create", map[string]any{"title": "Fixture proposal 11", "kind": "task", "phase": "open",
		"origin": map[string]string{"source": "src_a"}}), &ref)
	drain() // 事件在 Drain 时已落库
	do("item.update", map[string]any{"id": ref.ID, "title": "Calendar event #11", "body": "synthetic body", "reason": "refresh from source"})
	drain() // 刷新之后才到达的游标
	if n := len(stub.Requests()); n != 0 {
		t.Fatalf("external endpoint got %d requests while the scope is not chosen", n)
	}
	// 对照：范围选定为 full 后，同一事项的下一次标题变更才发出请求。
	if err := st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE sources_registry_source SET outbound_external = 'full' WHERE id = 'src_a'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	do("item.update", map[string]any{"id": ref.ID, "title": "Calendar event #11b", "reason": "refresh"})
	drain()
	if n := len(stub.Requests()); n != 1 {
		t.Fatalf("after choosing full: requests = %d, want 1", n)
	}
}
