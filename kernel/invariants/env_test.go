package invariants_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/screening"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

var (
	bg       = context.Background()
	t0       = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	worker   = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelWorker}
	delegate = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
)

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

// ledger 是完整的"账本 → 决策 → 初筛 → 触达"链路：真存储、真命令管线、真时钟、decision-card 内置包、MODEL_STUB。
type ledger struct {
	st   *store.Store
	x    *command.Executor
	clk  *clock.Clock
	stub *modelstub.Stub
	cls  *modelstub.ClsStub // 判断端点：web_only 初筛先问它
	scr  *screening.Service
	q    *delivery.Queue
	att  *attention.Attention
	now  time.Time
}

func newLedger(t *testing.T) *ledger {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, delivery.Module, kcontext.Module, attention.Module, direct.Module, proc.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &ledger{st: st, now: t0, stub: modelstub.New(), cls: modelstub.NewCls()}
	t.Cleanup(e.stub.Close)
	t.Cleanup(e.cls.Close)
	e.cls.SetDefaultPYes(0.05)
	now := func() time.Time { return e.now }
	e.x = command.New(st, now)
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	ld := packs.New(packs.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load packs: %v %+v", err, rej)
	}
	judge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}, Now: now, Attempts: 1,
		Sleep: func(context.Context, time.Duration) error { t.Error("screening must not retry"); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if e.scr, err = screening.New(screening.Options{Store: st, Judge: judge, Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := decisions.RegisterWith(e.x, e.scr); err != nil {
		t.Fatal(err)
	}
	e.x.OnCommit(e.scr.OnCommit)
	e.q = delivery.New(delivery.Options{Now: now, UnclaimedAfter: 5 * time.Minute, Lease: 24 * time.Hour})
	e.att = attention.New(attention.Options{Queue: e.q, Now: now})
	if e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error {
		switch {
		case e.att.Owns(c):
			return e.att.HandleTimer(tx, c)
		case decisions.Owns(c):
			return decisions.HandleTimer(tx, c)
		}
		return items.HandleTimer(tx, c)
	}, now); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, e.att.Start); err != nil {
		t.Fatal(err)
	}
	ep := policy.GenerateEndpoint{URL: e.stub.URL(), Model: "m1", APIKey: "k"}
	if err := st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error {
			p.GenerateEndpoint, p.ScreeningTimeoutMs = ep, 5000
			p.JudgeEndpoint = &policy.JudgeEndpoint{URL: e.cls.URL(), Protocol: policy.ProtocolProbability, Locality: policy.LocalityLocal, TimeoutMs: 5000}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *ledger) do(id command.Identity, cmd, evidence string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: evidence, WebURL: "http://web/items"})
}

func (e *ledger) must(t *testing.T, id command.Identity, cmd string, in any) command.Result {
	t.Helper()
	r, err := e.do(id, cmd, "owner said so", in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func (e *ledger) newItem(t *testing.T, extra map[string]any) string {
	t.Helper()
	in := map[string]any{"title": "pay the invoice", "kind": "task", "phase": "open", "assignee": "wrk", "body": "invoice text"}
	for k, v := range extra {
		in[k] = v
	}
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "item.create", in).Value, &out)
	return out.ID
}

func ask(extra map[string]any) map[string]any {
	a := map[string]any{"question": "ship it?", "options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no", "next": "dropped"}}}
	for k, v := range extra {
		a[k] = v
	}
	return a
}

func (e *ledger) get(t *testing.T, id string) domain.Item {
	t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return it
}

// events 返回对象的全部 Event；object 为空读全账本。
func (e *ledger) events(t *testing.T, object string) []domain.Event {
	t.Helper()
	var out []domain.Event
	if err := e.st.View(bg, func(tx *store.Tx) error {
		var es []eventlog.Entry
		var err error
		if object == "" {
			es, err = eventlog.Read(tx, 0, 100000)
		} else {
			es, err = eventlog.ReadObject(tx, object)
		}
		for _, x := range es {
			out = append(out, x.Event)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *ledger) version(t *testing.T, id string) (v int64) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { v, err = command.Version(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return
}

func (e *ledger) verify(t *testing.T) {
	t.Helper()
	if err := e.st.View(bg, items.Verify); err != nil {
		t.Fatalf("projection diverges from events: %v", err)
	}
}

// tick 把时间拨过一分钟并扫描一次时钟，触达据此追赶事件。
func (e *ledger) tick(t *testing.T) {
	t.Helper()
	e.now = e.now.Add(time.Minute)
	if _, err := e.clk.Tick(bg); err != nil {
		t.Fatal(err)
	}
}

// deliveriesFor 返回对某事项产出的、可见（注入或 Web 未读）的投递。
func (e *ledger) deliveriesFor(t *testing.T, item string) (out []domain.Delivery) {
	t.Helper()
	e.tick(t)
	if err := e.st.View(bg, func(tx *store.Tx) error {
		inj, err := e.q.Injectable(tx)
		if err != nil {
			return err
		}
		unread, err := e.q.Unread(tx)
		for _, d := range append(inj, unread...) {
			if d.Item == item {
				out = append(out, d)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return
}
