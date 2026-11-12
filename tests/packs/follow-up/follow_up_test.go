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
	// 包声明 observe（只记录）；这里验的是判断与提醒，按本人在 Web 的方式切到 auto。
	eng, err := rules.New(rules.Options{Store: st, Exec: e.x, Packs: onlyPack{p}, Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	e.do("rule.set_mode", map[string]string{"rule": key, "mode": rules.ModeAuto})
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
			p.ModelEndpoint = policy.ModelEndpoint{URL: e.stub.URL(), Model: "m1", APIKey: "k", ScreeningTimeoutMs: 1000}
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

// ACT-FUP-02 / ACT-DAY-06：真实包的判断型提示词被内核按结构化输出解析为"催"——一条提醒，事项仍是 waiting。
func TestRemindProducesReminderAndKeepsWaiting(t *testing.T) {
	t.Skip("v0.1 三选一跟进判断与逐级升级已从内置包移除（C.14）；跟进梯度服务由后续节点重写，届时替换本测试")
	e := setup(t)
	e.judged("remind")
	id := e.create("等装修公司报价", nil)
	e.wait(id, domain.WaitingExternal, "装修公司报价")
	if err := e.due(); err != nil {
		t.Fatal(err)
	}
	if it := e.get(id); it.Phase != domain.PhaseWaiting {
		t.Fatalf("phase = %s", it.Phase)
	}
	if e.reminders(id) != 1 {
		t.Fatalf("reminders = %d", e.reminders(id))
	}
	reqs := e.stub.Requests()
	if len(reqs) != 1 || !strings.Contains(reqs[0].BodyText, "周五前要定") || !strings.Contains(reqs[0].BodyText, id) {
		t.Fatalf("judge request lacks item id or material: %+v", reqs)
	}
}

// ACT-FUP-03：再等——延长 Timer，没有提醒。
func TestWaitMoreExtendsWithoutReminder(t *testing.T) {
	t.Skip("v0.1 三选一跟进判断与逐级升级已从内置包移除（C.14）；跟进梯度服务由后续节点重写，届时替换本测试")
	e := setup(t)
	e.judged("wait")
	id := e.create("等装修公司报价", nil)
	e.wait(id, domain.WaitingExternal, "装修公司报价")
	if err := e.due(); err != nil {
		t.Fatal(err)
	}
	if e.get(id).Phase != domain.PhaseWaiting || e.reminders(id) != 0 || e.events(id, fu.EvExtended) != 1 {
		t.Fatalf("phase=%s reminders=%d extended=%d", e.get(id).Phase, e.reminders(id), e.events(id, fu.EvExtended))
	}
}

// ACT-FUP-04：建议放弃——转 needs_input 交本人，不自动 dropped；本人回答后才落地。
func TestAbandonGoesToOwnerNotSilentlyDropped(t *testing.T) {
	t.Skip("v0.1 三选一跟进判断与逐级升级已从内置包移除（C.14）；跟进梯度服务由后续节点重写，届时替换本测试")
	e := setup(t)
	e.judged("abandon")
	id := e.create("等装修公司报价", nil)
	e.wait(id, domain.WaitingExternal, "装修公司报价")
	if err := e.due(); err != nil {
		t.Fatal(err)
	}
	if it := e.get(id); it.Phase != domain.PhaseNeedsInput || it.Ask == nil {
		t.Fatalf("item %s %+v", it.Phase, it.Ask)
	}
	e.do("decision.answer", map[string]any{"id": id, "choice": fu.OptDrop})
	if got := e.get(id).Phase; got != domain.PhaseDropped {
		t.Fatalf("after owner answer: %s", got)
	}
}

// ACT-FUP-05：委派子事项迟迟不回执，父事项等执行者——同样被跟进，产生提醒，父事项阶段不变。
func TestSilentDelegationIsFollowedUp(t *testing.T) {
	t.Skip("v0.1 三选一跟进判断与逐级升级已从内置包移除（C.14）；跟进梯度服务由后续节点重写，届时替换本测试")
	e := setup(t)
	e.judged("remind")
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
	if !strings.Contains(e.stub.Requests()[0].BodyText, string(domain.WaitingExecutor)) {
		t.Error("judge was not told the item waits on an executor")
	}
}

// ACT-FUP-06：模型 500——跟进不丢，暴露真实错误并记 health.alert，恢复后拿到判断结果。
func TestModelFailureKeepsFollowUpAndRecovers(t *testing.T) {
	t.Skip("v0.1 三选一跟进判断与逐级升级已从内置包移除（C.14）；跟进梯度服务由后续节点重写，届时替换本测试")
	e := setup(t)
	e.stub.SetDefault(modelstub.Reply{Status: 500})
	id := e.create("等装修公司报价", nil)
	e.wait(id, domain.WaitingExternal, "装修公司报价")
	err := e.due()
	if err == nil || !errors.Is(err, direct.ErrUnavailable) {
		t.Fatalf("err = %v", err)
	}
	if e.get(id).Phase != domain.PhaseWaiting || e.reminders(id) != 0 || e.events(id, domain.EvHealthAlert) != 1 {
		t.Fatalf("phase=%s reminders=%d alerts=%d", e.get(id).Phase, e.reminders(id), e.events(id, domain.EvHealthAlert))
	}
	e.judged("remind")
	if err := e.svc.Drain(bg); err != nil {
		t.Fatal(err)
	}
	if e.reminders(id) != 1 {
		t.Fatalf("recovery produced %d reminders", e.reminders(id))
	}
}

// 逐级升级：判断型答"需要解释"，交生成型给出 choice；生成型信息不足则升级到任务型，交本人，不替本人决定。
func TestNeedExplanationEscalatesThroughGenerate(t *testing.T) {
	t.Skip("v0.1 三选一跟进判断与逐级升级已从内置包移除（C.14）；跟进梯度服务由后续节点重写，届时替换本测试")
	e := setup(t)
	e.stub.Enqueue(
		modelstub.Reply{Content: `{"choice":"need_explanation","confidence":0.9}`},
		modelstub.Reply{Content: `{"insufficient_info":false,"choice":"remind","explanation":"对方三天未回"}`},
	)
	id := e.create("等装修公司报价", nil)
	e.wait(id, domain.WaitingExternal, "装修公司报价")
	if err := e.due(); err != nil {
		t.Fatal(err)
	}
	if len(e.stub.Requests()) != 2 || e.reminders(id) != 1 || e.get(id).Phase != domain.PhaseWaiting {
		t.Fatalf("calls=%d reminders=%d phase=%s", len(e.stub.Requests()), e.reminders(id), e.get(id).Phase)
	}

	e2 := setup(t)
	e2.stub.Enqueue(
		modelstub.Reply{Content: `{"choice":"need_explanation","confidence":0.9}`},
		modelstub.Reply{Content: `{"insufficient_info":true}`},
	)
	id2 := e2.create("等装修公司报价", nil)
	e2.wait(id2, domain.WaitingExternal, "装修公司报价")
	if err := e2.due(); err != nil {
		t.Fatal(err)
	}
	if it := e2.get(id2); it.Phase != domain.PhaseNeedsInput || e2.reminders(id2) != 0 {
		t.Fatalf("phase=%s reminders=%d", it.Phase, e2.reminders(id2))
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
