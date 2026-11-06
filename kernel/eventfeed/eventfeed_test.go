package eventfeed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

var human = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}

func open(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module,
		clock.Module, items.Module, materials.Module} {
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

var (
	owner  = command.Trusted{Actor: human, Level: authz.LevelWeb}
	worker = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI, OnBehalfOf: "owner"},
		Level: authz.LevelWorker}
)

// cmds 返回在 s 上跑真实模块命令的入口：载荷由各模块自己产出，不手写。
func cmds(t *testing.T, s *store.Store) func(id command.Identity, name string, in any) json.RawMessage {
	t.Helper()
	x := command.New(s, func() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) })
	if err := projects.Register(x); err != nil {
		t.Fatal(err)
	}
	if err := items.Register(x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := decisions.Register(x); err != nil {
		t.Fatal(err)
	}
	if err := materials.Register(x); err != nil {
		t.Fatal(err)
	}
	return func(id command.Identity, name string, in any) json.RawMessage {
		t.Helper()
		b, _ := json.Marshal(in)
		r, err := x.Do(ctx, command.Request{Identity: id, Command: name, Input: b, Owner: "owner"})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return r.Value
	}
}

func idOf(t *testing.T, v json.RawMessage, key string) string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(v, &m); err != nil || m[key] == "" {
		t.Fatalf("no %q in %s", key, v)
	}
	return m[key]
}

func ev(typ, obj, payload string) domain.Event {
	return domain.Event{Type: typ, Object: obj, Actor: human, Reason: "SECRET-REASON",
		Evidence: "SECRET-EVIDENCE", Payload: json.RawMessage(payload)}
}

func appendAll(t *testing.T, s *store.Store, evs ...domain.Event) {
	t.Helper()
	err := s.Update(ctx, func(tx *store.Tx) error {
		for _, e := range evs {
			if _, err := eventlog.Append(tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, s *store.Store, after int64, limit int) Page {
	t.Helper()
	var p Page
	err := s.View(ctx, func(tx *store.Tx) error {
		var err error
		p, err = Read(tx, after, limit)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// flow-event-subscribe：订阅方按游标读取登记类型的信封，处理成功后才推进游标；
// 未推进则重读得到同样的信封（至少一次）；信封不泄露正文、reason、证据。
func TestEventSubscribeFlow(t *testing.T) {
	s := open(t)
	do := cmds(t, s)
	id := idOf(t, do(owner, "item.create", map[string]any{"kind": "task", "title": "写周报", "summary": "SECRET-BODY", "reason": "SECRET-REASON"}), "id")
	appendAll(t, s, ev("internal.unregistered", "x", `{"title":"hidden"}`))
	do(owner, "item.transition", map[string]any{"id": id, "to": "done", "reason": "shipped"})

	p := read(t, s, 0, 10)
	if len(p.Envelopes) != 2 {
		t.Fatalf("want 2 registered envelopes, got %+v", p.Envelopes)
	}
	first := p.Envelopes[0]
	if first.Seq != 1 || first.Type != domain.EvItemCreated || first.Object != id ||
		first.Title != "写周报" || first.Actor != human || first.At.IsZero() {
		t.Fatalf("bad envelope %+v", first)
	}
	if p.Envelopes[1].Seq != 3 || p.Envelopes[1].Type != domain.EvItemPhaseChanged || p.Envelopes[1].Title != "写周报" || p.Next != 3 {
		t.Fatalf("bad second envelope/next: %+v next=%d", p.Envelopes[1], p.Next)
	}
	raw, _ := json.Marshal(p)
	for _, leak := range []string{"SECRET-BODY", "SECRET-REASON", "payload", "reason", "evidence"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("envelope leaks %q: %s", leak, raw)
		}
	}

	// 订阅方没推进游标（处理失败）：重读得到相同结果。
	again := read(t, s, 0, 10)
	if len(again.Envelopes) != 2 || again.Envelopes[0] != first {
		t.Fatalf("re-read differs: %+v", again)
	}
	// 推进游标后，只看到新事件。
	appendAll(t, s, ev(domain.EvRunFinished, "run-1", `{}`))
	next := read(t, s, p.Next, 10)
	if len(next.Envelopes) != 1 || next.Envelopes[0].Seq != 4 || next.Next != 4 {
		t.Fatalf("after cursor: %+v", next)
	}
	// 已追平：无新事件，游标不动。
	if idle := read(t, s, 4, 10); len(idle.Envelopes) != 0 || idle.Next != 4 {
		t.Fatalf("idle: %+v", idle)
	}
}

// 整段都是未登记类型时，游标仍要越过它们，否则订阅方会卡死。
func TestCursorSkipsUnregisteredRuns(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		appendAll(t, s, ev("internal.noise", "n", `{}`))
	}
	appendAll(t, s, ev(domain.EvSourceFailed, "src", `{}`))
	p := read(t, s, 0, 1)
	if len(p.Envelopes) != 1 || p.Envelopes[0].Seq != 6 || p.Next != 6 {
		t.Fatalf("%+v", p)
	}
}

func TestLimitStopsAtLastReturned(t *testing.T) {
	s := open(t)
	appendAll(t, s, ev(domain.EvItemCreated, "a", `{}`), ev(domain.EvItemCreated, "b", `{}`),
		ev(domain.EvItemCreated, "c", `{}`))
	p := read(t, s, 0, 2)
	if len(p.Envelopes) != 2 || p.Next != 2 {
		t.Fatalf("%+v", p)
	}
	if p := read(t, s, 0, 0); len(p.Envelopes) != 0 || p.Next != 0 {
		t.Fatalf("limit 0: %+v", p)
	}
}

// 物理删除墓碑化内容后信封仍在，序号不变，标题随内容抹除。
func TestTombstonedEventKeepsEnvelope(t *testing.T) {
	s := open(t)
	id := idOf(t, cmds(t, s)(owner, "item.create", map[string]any{"kind": "task", "title": "私密"}), "id")
	if p := read(t, s, 0, 10); len(p.Envelopes) != 1 || p.Envelopes[0].Title != "私密" {
		t.Fatalf("before tombstone: %+v", p)
	}
	err := s.Update(ctx, func(tx *store.Tx) error {
		_, err := eventlog.Tombstone(tx, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p := read(t, s, 0, 10)
	if len(p.Envelopes) != 1 || p.Envelopes[0].Seq != 1 || p.Envelopes[0].Title != "" {
		t.Fatalf("%+v", p)
	}
}

// 每个登记类型的标题取法：载荷由真实模块命令产出，覆盖 item.* / decision.* / material.* / guideline.* / project.updated；
// 没有标题概念的类型（载荷里没有对象标题）保持空。
func TestTitleFromRealModulePayloads(t *testing.T) {
	s := open(t)
	do := cmds(t, s)

	prj := idOf(t, do(owner, "project.create", map[string]any{"name": "项目甲"}), "id")
	do(owner, "project.update", map[string]any{"id": prj, "name": "项目乙"})
	id := idOf(t, do(owner, "item.create", map[string]any{"kind": "task", "title": "事项甲", "assignee": "wrk", "phase": "open"}), "id")
	do(owner, "item.update", map[string]any{"id": id, "title": "事项乙"})
	do(worker, "decision.request", map[string]any{"id": id, "ask": map[string]any{"question": "ship it?",
		"options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}})
	do(owner, "decision.answer", map[string]any{"id": id, "choice": "A"})
	do(owner, "item.transition", map[string]any{"id": id, "to": "done", "reason": "shipped"})
	m := do(worker, "material.commit", map[string]any{"project": prj, "kind": "guideline", "title": "规范甲", "content": "v1", "reason": "初版"})
	do(owner, "revision.confirm", map[string]any{"id": idOf(t, m, "revision")})

	got := map[string]string{} // 同类型多次出现时留最后一条
	for _, e := range read(t, s, 0, 100).Envelopes {
		got[e.Type] = e.Title
	}
	for typ, want := range map[string]string{
		domain.EvProjectUpdated:           "项目乙",
		domain.EvItemCreated:              "事项甲",
		domain.EvItemUpdated:              "事项乙",
		domain.EvDecisionRequested:        "事项乙",
		domain.EvDecisionResolved:         "事项乙",
		domain.EvItemPhaseChanged:         "事项乙",
		domain.EvMaterialRevisionProposed: "规范甲",
		domain.EvMaterialRevised:          "规范甲",
		domain.EvGuidelineActivated:       "规范甲",
	} {
		if g, ok := got[typ]; !ok || g != want {
			t.Errorf("%s: title %q (seen=%v), want %q", typ, g, ok, want)
		}
	}
}

// delegation.* 的载荷就是 items.Create 的事项快照（delegation 模块原样使用），标题在 item.title。
func TestTitleOfDelegationPayload(t *testing.T) {
	s := open(t)
	err := s.Update(ctx, func(tx *store.Tx) error {
		_, snap, err := items.Create(tx, domain.Item{Kind: "task", Phase: domain.PhaseOpen, Title: "子事项"}, "", time.Now())
		if err != nil {
			return err
		}
		for _, typ := range []string{domain.EvDelegationCreated, domain.EvDelegationReported} {
			if _, err := eventlog.Append(tx, domain.Event{Type: typ, Object: "x", Actor: human, Payload: snap}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range read(t, s, 0, 10).Envelopes {
		if e.Title != "子事项" {
			t.Fatalf("%s: %q", e.Type, e.Title)
		}
	}
}

// 没有标题概念的类型保持空：载荷由真实模块产出，里面没有对象标题。
func TestTitlelessTypesStayEmpty(t *testing.T) {
	s := open(t)
	do := cmds(t, s)
	prj := idOf(t, do(owner, "project.create", map[string]any{"name": "项目甲"}), "id")
	do(owner, "project.allowed_executors.set", map[string]any{"id": prj, "executors": []string{"x"}})
	err := s.Update(ctx, func(tx *store.Tx) error {
		_, err := policy.Update(tx, human, "test", func(p *policy.Policy) error {
			p.ForwardAllowlist["unattended"] = []string{"x"}
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range read(t, s, 0, 100).Envelopes {
		seen[e.Type] = true
		if e.Type != domain.EvProjectUpdated && e.Title != "" {
			t.Errorf("%s should have no title, got %q", e.Type, e.Title)
		}
	}
	if !seen[domain.EvProjectAllowedExecutorsChanged] || !seen[domain.EvPolicyChanged] {
		t.Fatalf("expected events missing: %v", seen)
	}
}
