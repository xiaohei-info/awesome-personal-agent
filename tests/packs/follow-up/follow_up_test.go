package followup

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	fu "github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/followup"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

func loadPack(t *testing.T) (*packs.Loader, *packs.Pack) {
	t.Helper()
	ld := packs.New(packs.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	rej, err := ld.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "follow-up" {
			t.Fatalf("follow-up rejected by loader: %s", r.Reason)
		}
	}
	p, ok := ld.Get("follow-up")
	if !ok {
		t.Fatal("follow-up is not an active builtin pack")
	}
	return ld, p
}

// onlyPack 让规则引擎只登记 follow-up 包的规则：其他内置包的周期 Timer 不归这里的时钟处理器。
type onlyPack struct{ p *packs.Pack }

func (o onlyPack) List() []*packs.Pack { return []*packs.Pack{o.p} }

// 真实内置包 + 真实内核跟进模块 + 模型替身（唯一的外部边界）。
type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	stub *modelstub.Stub
	svc  *fu.Service
	clk  *clock.Clock
	q    *delivery.Queue
	now  time.Time
	// every 是包里 follow_up 规则声明的跟进间隔，每一轮都按它。
	every time.Duration
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, delivery.Module, attention.Module, direct.Module, rules.Module, fu.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: t0, stub: modelstub.New()}
	t.Cleanup(e.stub.Close)
	now := func() time.Time { return e.now }
	e.x = command.New(st, now)
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := errors.Join(decisions.Register(e.x), rules.Register(e.x)); err != nil {
		t.Fatal(err)
	}
	e.configure()
	ld, p := loadPack(t)
	var key string
	for _, r := range p.ClockRules {
		if r.Type == "follow_up" {
			key = rules.ClockKey(p.Name, r.ID)
			if e.every, err = time.ParseDuration(r.Schedule); err != nil {
				t.Fatalf("follow_up schedule %q: %v", r.Schedule, err)
			}
		}
	}
	if key == "" {
		t.Fatal("follow-up pack declares no follow_up clock rule")
	}
	judge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}, Now: now,
		Sleep: func(context.Context, time.Duration) error { return nil }, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	e.q = delivery.New(delivery.Options{Now: now})
	a := attention.New(attention.Options{Queue: e.q, Now: now})
	e.svc, err = fu.New(fu.Options{Store: st, Exec: e.x, Judge: judge, Reminder: a, Packs: ld, Pack: "follow-up", Owner: "owner", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	// 提醒类规则省略 mode，初始就是 auto（C.4），不需要本人切换。
	eng, err := rules.New(rules.Options{Store: st, Exec: e.x, Packs: onlyPack{p}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Register(); err != nil {
		t.Fatal(err)
	}
	e.svc.Attach(func(err error) { t.Errorf("sync: %v", err) })
	e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error {
		switch {
		case e.svc.Owns(c):
			return e.svc.HandleTimer(tx, c)
		case decisions.Owns(c):
			return decisions.HandleTimer(tx, c)
		}
		return items.HandleTimer(tx, c)
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) configure() {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error {
			p.GenerateEndpoint, p.ScreeningTimeoutMs = policy.GenerateEndpoint{URL: e.stub.URL(), Model: "m1", APIKey: "k"}, 1000
			return nil
		})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) do(cmd string, in any) json.RawMessage {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func (e *env) create(title string, extra map[string]any) string {
	e.t.Helper()
	in := map[string]any{"title": title, "kind": "task", "phase": "open", "body": "周五前要定", "assignee": "owner"}
	for k, v := range extra {
		in[k] = v
	}
	var o struct{ ID string }
	_ = json.Unmarshal(e.do("item.create", in), &o)
	return o.ID
}

func (e *env) wait(id string, kind domain.WaitingKind, ref string) {
	e.do("item.transition", map[string]any{"id": id, "to": "waiting", "reason": "sent", "waiting_on": map[string]string{"kind": string(kind), "ref": ref}})
}

func (e *env) get(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) reply(content string) {
	e.stub.SetDefault(modelstub.Reply{Content: content})
}

func (e *env) judged(choice string) {
	b, _ := json.Marshal(map[string]any{"choice": choice, "confidence": 0.9})
	e.reply(string(b))
}

func (e *env) due() error {
	e.t.Helper()
	e.now = e.now.Add(e.every)
	if _, err := e.clk.Tick(bg); err != nil {
		e.t.Fatal(err)
	}
	return e.svc.Drain(bg)
}

func (e *env) reminders(id string) (n int) {
	e.t.Helper()
	err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT COUNT(*) FROM delivery_delivery WHERE item = ? AND class = 'reminder'`, id)
		if err != nil {
			return err
		}
		defer r.Close()
		var c int64
		r.Next()
		err = r.Scan(&c)
		n = int(c)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) events(id, typ string) (n int) {
	e.t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, _ := eventlog.ReadObject(tx, id)
		for _, x := range es {
			if x.Event.Type == typ {
				n++
			}
		}
		return nil
	})
	return n
}

func (e *env) reminderOptions(id string) (out [][]string) {
	e.t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT id FROM delivery_delivery WHERE item = ? AND class = 'reminder' ORDER BY rowid`, id)
		if err != nil {
			return err
		}
		var ids []string
		for r.Next() {
			var d string
			_ = r.Scan(&d)
			ids = append(ids, d)
		}
		_ = r.Close()
		for _, d := range ids {
			dv, err := e.q.Get(tx, d)
			if err != nil {
				return err
			}
			out = append(out, dv.Content.Options)
		}
		return nil
	})
	return out
}

// ACT-FUP-02：真实包的第一轮到期——一条带"已催""再等""放弃"的提醒，事项仍是 waiting，没有任何模型请求。
func TestFirstRoundRemindsWithThreeActionsWithoutModel(t *testing.T) {
	e := setup(t)
	id := e.create("等装修公司报价", nil)
	e.wait(id, domain.WaitingExternal, "装修公司报价")
	if err := e.due(); err != nil {
		t.Fatal(err)
	}
	if it := e.get(id); it.Phase != domain.PhaseWaiting {
		t.Fatalf("phase = %s", it.Phase)
	}
	opts := e.reminderOptions(id)
	if len(opts) != 1 || strings.Join(opts[0], "/") != "已催/再等/放弃" {
		t.Fatalf("reminders = %v", opts)
	}
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("model requests = %d", n)
	}
}

// ACT-FUP-04 / 06：选"再等"什么都不做；连续 remind_rounds（包里是 2）轮后转 needs_input，三个选项，不自动 dropped。
func TestLadderEndsWithQuestionNotDrop(t *testing.T) {
	e := setup(t)
	id := e.create("等装修公司报价", nil)
	e.wait(id, domain.WaitingExternal, "装修公司报价")
	for range 2 {
		if err := e.due(); err != nil {
			t.Fatal(err)
		}
	}
	if e.reminders(id) != 2 || e.get(id).Phase != domain.PhaseWaiting {
		t.Fatalf("reminders=%d phase=%s", e.reminders(id), e.get(id).Phase)
	}
	if err := e.due(); err != nil {
		t.Fatal(err)
	}
	it := e.get(id)
	if e.reminders(id) != 2 || it.Phase != domain.PhaseNeedsInput || it.Ask == nil || len(it.Ask.Options) != 3 {
		t.Fatalf("reminders=%d item %s %+v", e.reminders(id), it.Phase, it.Ask)
	}
	e.do("decision.answer", map[string]any{"id": id, "choice": fu.OptDrop})
	if got := e.get(id).Phase; got != domain.PhaseDropped {
		t.Fatalf("after owner answer: %s", got)
	}
}

// ACT-FUP-05 / 08：委派子事项迟迟不回执，父事项等执行者——同样被跟进，产生提醒，父事项阶段不变。
func TestSilentDelegationIsFollowedUp(t *testing.T) {
	e := setup(t)
	parent := e.create("装修方案", nil)
	child := e.create("向装修公司索取报价", nil)
	e.do("item.relate", map[string]any{"from": child, "to": parent, "type": "parent", "reason": "delegated"})
	e.wait(parent, domain.WaitingExecutor, child)
	if err := e.due(); err != nil {
		t.Fatal(err)
	}
	if e.get(parent).Phase != domain.PhaseWaiting || e.reminders(parent) != 1 {
		t.Fatalf("phase=%s reminders=%d", e.get(parent).Phase, e.reminders(parent))
	}
}

// 包的声明：跟进间隔时钟规则初始 observe，提示词类别与级联链，包内无 harness 名。
func TestPackDeclarations(t *testing.T) {
	_, p := loadPack(t)
	if len(p.ClockRules) != 2 {
		t.Fatalf("clock rules: %+v", p.ClockRules)
	}
	if r := p.ClockRules[0]; r.ID != "follow-up-interval" || r.Type != "follow_up" || r.Schedule != "72h" || len(r.Do) != 0 ||
		r.RemindRounds == nil || *r.RemindRounds != 2 || r.DeferWith != "follow-up-defer" {
		t.Fatalf("follow-up-interval: %+v", r)
	}
	if r := p.ClockRules[1]; r.ID != "stale-check" || r.Type != "stale" || r.StaleAfter != "14d" || len(r.Do) != 0 {
		t.Fatalf("stale-check: %+v", r)
	}
	d := p.Prompts["follow-up-defer"].Meta
	if d.Agent != "judge" || d.QuestionType != "yes_no" || d.OnUndecided != "no" || d.Band == nil || *d.Band.Low != 0.85 {
		t.Fatalf("follow-up-defer: %+v", d)
	}
	if len(p.Prompts) != 1 {
		t.Errorf("v0.1 judge, generate and task templates must be removed: %v", p.Prompts)
	}
	if len(p.EventRules) != 0 || len(p.SourceRules) != 0 {
		t.Error("follow-up declares a clock rule only")
	}
	for name, pr := range p.Prompts {
		for _, h := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), h) {
				t.Errorf("prompt %s mentions harness/tool name %q", name, h)
			}
		}
	}
}
