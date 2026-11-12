package delegationverify

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/verify"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const ruleKey = "delegation-verify/event_rules/verify-report"

func loadPack(t *testing.T) (*packs.Loader, *packs.Pack) {
	t.Helper()
	ld := packs.New(packs.Options{Builtin: rootpacks.FS, Kinds: items.NewKinds()})
	rej, err := ld.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "delegation-verify" {
			t.Fatalf("delegation-verify rejected by loader: %s", r.Reason)
		}
	}
	p, ok := ld.Get("delegation-verify")
	if !ok {
		t.Fatal("delegation-verify is not an active builtin pack")
	}
	return ld, p
}

// 真实内置包 + 真实核验模块与规则引擎 + 模型替身（唯一的外部边界）。
type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	au   *auth.Auth
	stub *modelstub.Stub
	wrk  string
}

func newEnv(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, executors.Module, delegation.Module, rules.Module, direct.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, au: auth.New(nil), stub: modelstub.New()}
	t.Cleanup(e.stub.Close)
	now := func() time.Time { return t0 }
	e.x = command.New(st, now)
	kinds := items.NewKinds()
	for _, f := range []func(*command.Executor) error{projects.Register, rules.Register, decisions.Register,
		func(x *command.Executor) error { return items.Register(x, kinds) },
		func(x *command.Executor) error { return executors.Register(x, executors.Options{Owner: "owner"}) },
		func(x *command.Executor) error {
			return delegation.Register(x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Errorf("delegation: %v", err) }})
		}} {
		if err := f(e.x); err != nil {
			t.Fatal(err)
		}
	}
	err = st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error {
			p.GenerateEndpoint, p.ScreeningTimeoutMs = policy.GenerateEndpoint{URL: e.stub.URL(), Model: "m1", APIKey: "k"}, 1000
			return nil
		})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	ld := packs.New(packs.Options{Builtin: rootpacks.FS, Kinds: kinds})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %+v", err, rej)
	}
	svc, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}, Now: now,
		Sleep: func(context.Context, time.Duration) error { return nil }, Attempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	h, err := verify.Handler(verify.Options{Store: st, Judge: svc})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rules.New(rules.Options{Store: st, Exec: e.x, Packs: ld, Handlers: map[string]rules.Handler{"call_agent": h},
		Owner: "owner", Now: now, OnError: func(err error) { t.Errorf("consume: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	e.must(owner, "executor.put", map[string]any{"id": "wk.cli", "kind": "external-cli"})
	err = st.Update(bg, func(tx *store.Tx) error {
		_, e.wrk, err = e.au.Issue(tx, auth.Spec{Kind: auth.Worker, Executor: "wk.cli"})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) must(id command.Identity, cmd string, in any) json.RawMessage {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func (e *env) item(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) delegated() (parent, child string) {
	e.t.Helper()
	var o, c struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", map[string]any{"title": "发布 v2", "kind": "task"}), &o)
	e.must(owner, "item.transition", map[string]any{"id": o.ID, "to": "open", "reason": "go"})
	_ = json.Unmarshal(e.must(owner, "delegate.create", map[string]any{"parent": o.ID, "assignee": "wk.cli", "title": "做发布"}), &c)
	return o.ID, c.ID
}

func (e *env) report(child string) {
	e.t.Helper()
	w := command.Bearer{Auth: e.au, Secret: e.wrk, Origin: auth.Origin{Harness: "h"}}
	e.must(w, "delegate.report", map[string]any{"id": child, "outcome": "done", "summary": "全部完成，测试通过",
		"evidence": map[string]string{"pr": "https://x/pr/" + child}})
}

func (e *env) judge(prob, conf float64) {
	b, _ := json.Marshal(map[string]float64{"probability": prob, "confidence": conf})
	e.stub.SetDefault(modelstub.Reply{Content: string(b)})
}

func (e *env) auto() {
	e.t.Helper()
	e.must(owner, "rule.set_mode", map[string]string{"rule": ruleKey, "mode": rules.ModeAuto})
}

// C.14：包只声明响应 delegation.reported 的事件规则（初始 observe）、判断型初筛与任务型核验提示词；
// 无 kind、时钟规则，也无 harness 名。
func TestPackDeclaresWhatC14Lists(t *testing.T) {
	_, p := loadPack(t)
	if len(p.Kinds) != 0 || len(p.ClockRules) != 0 || len(p.EventRules) != 1 {
		t.Fatalf("kinds=%d clock=%d event=%d", len(p.Kinds), len(p.ClockRules), len(p.EventRules))
	}
	r := p.EventRules[0]
	if r.When["event"] != "delegation.reported" || r.Mode != "observe" || len(r.Do) != 1 ||
		r.Do[0]["action"] != "call_agent" || r.Do[0]["prompt"] != "delegation-screen" {
		t.Fatalf("rule: %+v", r)
	}
	s := p.Prompts["delegation-screen"]
	if s.Meta.Agent != "judge" || s.Meta.QuestionType != "yes_no" || s.Meta.OnUndecided != "yes" || s.Meta.Band == nil || *s.Meta.Band.Low != 0.20 {
		t.Fatalf("screen meta: %+v", s.Meta)
	}
	v := p.Prompts["delegation-verify-task"]
	if v.Meta.Agent != "task" || v.Meta.Access != "read" || !strings.Contains(v.Body, "{item_id}") {
		t.Fatalf("verify meta: %+v", v.Meta)
	}
	for name, pr := range p.Prompts {
		for _, h := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), h) {
				t.Errorf("prompt %s mentions %q", name, h)
			}
		}
	}
}

// 默认 observe：回执不外呼模型、不写业务状态。
func TestObserveByDefaultDoesNothing(t *testing.T) {
	e := newEnv(t)
	e.judge(0.1, 0.9)
	parent, child := e.delegated()
	e.report(child)
	if n := len(e.stub.Requests()); n != 0 || e.item(parent).Ask != nil {
		t.Fatalf("observe acted: calls=%d ask=%+v", n, e.item(parent).Ask)
	}
}

// flow-delegate-recover / ACT-DLG-11：本人把规则切到 auto 后，回执可信则放行；
// 存疑则在父事项生成决策请求，子事项不动、不自动返工，本人回答后父事项回 open。
func TestDoubtfulReportAsksOwnerOnParentWithoutRework(t *testing.T) {
	e := newEnv(t)
	e.auto()

	e.judge(0.95, 0.9)
	p1, c1 := e.delegated()
	e.report(c1)
	if it := e.item(p1); it.Ask != nil || it.Phase != domain.PhaseOpen {
		t.Fatalf("credible report changed parent: %s %+v", it.Phase, it.Ask)
	}

	e.stub.Reset()
	e.judge(0.2, 0.9)
	parent, child := e.delegated()
	e.report(child)
	p := e.item(parent)
	if p.Phase != domain.PhaseNeedsInput || p.Ask == nil || len(p.Ask.Options) != 2 || p.Ask.Recommend != verify.OptRework ||
		!strings.Contains(p.Ask.Question, "做发布") {
		t.Fatalf("parent phase=%s ask=%+v", p.Phase, p.Ask)
	}
	if !strings.Contains(e.stub.Requests()[0].BodyText, "全部完成，测试通过") {
		t.Fatal("report summary not given to the judge")
	}
	if c := e.item(child); c.Phase != domain.PhaseDone {
		t.Fatalf("child was touched: %s", c.Phase)
	}
	var kids []domain.Item
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { kids, err = items.Children(tx, parent); return })
	if len(kids) != 1 {
		t.Fatalf("auto re-delegation: %d children", len(kids))
	}
	e.must(owner, "decision.answer", map[string]any{"id": parent, "choice": verify.OptAccept})
	if got := e.item(parent).Phase; got != domain.PhaseOpen {
		t.Fatalf("after answer: %s", got)
	}
}

// 初筛把握不足时按包声明的 escalate_to 升级到任务型核验，同样走存疑决策，不当作可信。
func TestLowConfidenceEscalatesToTaskAndAsksOwner(t *testing.T) {
	t.Skip("v0.1 置信度升级到任务型核验已移除（C.7、C.14）；判断引擎由后续节点按 answer_by/band 重写，届时替换本测试")
	e := newEnv(t)
	e.auto()
	e.judge(0.99, 0.1)
	parent, child := e.delegated()
	e.report(child)
	p := e.item(parent)
	if p.Ask == nil || !strings.Contains(p.Ask.Question, "delegation-verify-task") {
		t.Fatalf("ask %+v", p.Ask)
	}
}
