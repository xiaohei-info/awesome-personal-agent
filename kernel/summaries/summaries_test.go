package summaries

import (
	"context"
	"encoding/json"
	"errors"
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
