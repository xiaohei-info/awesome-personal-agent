package channel_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg = context.Background()
	t0 = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	owner  = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	worker = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: authz.LevelWorker}
	// 渠道 ext 密钥：scope channel.<名> 解析成 Principal ext:channel.<名>。
	chanKey  = ext("chan")
	otherKey = ext("other")
	// 表单令牌自己就是凭据，调用方不需要别的身份。
	form = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "form", Via: domain.ViaExt}, Level: authz.LevelWorker}
)

func ext(name string) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "ext:channel." + name, Via: domain.ViaExt}, Level: authz.LevelWorker, Kind: auth.Ext}
}

type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	q   *delivery.Queue
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, delivery.Module, kcontext.Module, channel.Module, attention.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: t0}
	now := func() time.Time { return e.now }
	e.x = command.New(st, now)
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.q = delivery.New(delivery.Options{Now: now, UnclaimedAfter: 5 * time.Minute, Lease: 24 * time.Hour})
	if err := channel.Register(e.x, channel.Options{Queue: e.q, Now: now}); err != nil {
		t.Fatal(err)
	}
	a := attention.New(attention.Options{Queue: e.q, Now: now})
	a.Attach(e.x, st, func(err error) { t.Errorf("attention: %v", err) })
	clk, err := clock.New(st, func(tx *store.Tx, c clock.Command) error {
		if a.Owns(c) {
			return a.HandleTimer(tx, c)
		}
		return items.HandleTimer(tx, c)
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	_ = clk
	e.update(a.Start)
	// 决策投递走渠道 chan；chan 只能发文字，回答人白名单里只有 alice；money 标签属 web_only。
	e.policy(func(p *policy.Policy) {
		p.Disturb.Routes["decision"] = "chan"
		p.Channels = map[string]policy.Channel{"chan": {TextOnly: true}}
		p.ChannelAnswerers = map[string][]string{"chan": {"alice"}}
		p.WebOnly.Rules = []policy.WebOnlyRule{{Tag: "money", Category: policy.CategoryFunds}}
	})
	return e
}

func (e *env) update(fn func(*store.Tx) error) {
	e.t.Helper()
	if err := e.st.Update(bg, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) policy(f func(*policy.Policy)) {
	e.t.Helper()
	e.update(func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error { f(p); return nil })
		return err
	})
}

func (e *env) try(id command.Identity, cmd string, in any) (json.RawMessage, error) {
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner"})
	return r.Value, err
}

func (e *env) do(id command.Identity, cmd string, in any) json.RawMessage {
	e.t.Helper()
	v, err := e.try(id, cmd, in)
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return v
}

// sent 建一个带决策请求的事项，把触达写入的投递按渠道 ext 的流程领取并回报送达，返回事项、投递与表单令牌。
func (e *env) sent(tags ...string) (item string, d domain.Delivery, token string) {
	e.t.Helper()
	var out struct{ ID string }
	_ = json.Unmarshal(e.do(owner, "item.create", map[string]any{"title": "t", "kind": "task", "phase": "open", "assignee": "wrk", "tags": tags}), &out)
	e.do(worker, "decision.request", map[string]any{"id": out.ID, "ask": map[string]any{"question": "ship it?",
		"options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}})
	e.update(func(tx *store.Tx) error {
		got, err := e.q.Claim(tx, delivery.ChannelRoute("chan"))
		if err != nil || got == nil || got.Item != out.ID {
			e.t.Fatalf("claim: %+v %v", got, err)
		}
		d = *got
		_, _, err = e.q.Ack(tx, d.ID, d.Version, "msg-"+out.ID)
		return err
	})
	return out.ID, d, strings.TrimPrefix(d.Content.FormURL, "/forms/")
}

func (e *env) answer(id command.Identity, d domain.Delivery, sender, ref, choice string) error {
	_, err := e.try(id, channel.CmdAnswer, map[string]any{"delivery": d.ID, "sender": sender, "ref": ref, "choice": choice})
	return err
}

func (e *env) item(id string) (it domain.Item) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) events(id string) (out []domain.Event) {
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, id)
		for _, x := range es {
			out = append(out, x.Event)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) delivery(id string) (d domain.Delivery) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { d, err = e.q.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func code(err error) domain.Code {
	if de, ok := err.(*domain.Error); ok {
		return de.Code
	}
	return ""
}

func (e *env) stillAsking(id string) {
	e.t.Helper()
	if it := e.item(id); it.Phase != domain.PhaseNeedsInput || it.Ask.Resolution != nil {
		e.t.Fatalf("answer took effect: %+v", it)
	}
}

func (e *env) rejections(id string) (n int) {
	for _, ev := range e.events(id) {
		if ev.Type == domain.EvCommandRejected {
			n++
		}
	}
	return
}

// ACT-CHP-05/06 渠道回答：白名单内的回答人、与送达一致的 ref，回答生效并记渠道名。
func TestChannelAnswerResolvesViaChannel(t *testing.T) {
	e := setup(t)
	id, d, _ := e.sent()
	if err := e.answer(chanKey, d, "alice", "msg-"+id, "A"); err != nil {
		t.Fatal(err)
	}
	it := e.item(id)
	r := it.Ask.Resolution
	if r == nil || r.Choice != "A" || r.ResolvedVia != "chan" || r.AnsweredBy != "alice" || it.Phase != domain.PhaseOpen {
		t.Fatalf("resolution %+v phase %s", r, it.Phase)
	}
	var ev *domain.Event
	for _, x := range e.events(id) {
		if x.Type == domain.EvDecisionResolved {
			x := x
			ev = &x
		}
	}
	if ev == nil || ev.ResolvedVia != "chan" {
		t.Fatalf("decision.resolved event %+v", ev)
	}
	if got := e.delivery(d.ID).State; got != domain.DeliveryAcknowledged {
		t.Fatalf("delivery state %s", got)
	}
}

func TestChannelAnswerRejections(t *testing.T) {
	e := setup(t)
	id, d, _ := e.sent()
	ref := "msg-" + id
	for name, c := range map[string]struct {
		key            command.Identity
		sender, ref    string
		want           domain.Code
		wantRejectedEv bool
	}{
		"sender outside the allowlist": {chanKey, "mallory", ref, domain.CodePermissionDenied, true},
		"sender missing":               {chanKey, "", ref, domain.CodePermissionDenied, true},
		"ref differs from delivery":    {chanKey, "alice", "msg-other", domain.CodePermissionDenied, false},
		"ref missing":                  {chanKey, "alice", "", domain.CodePermissionDenied, false},
		"another channel's key":        {otherKey, "alice", ref, domain.CodePermissionDenied, false},
	} {
		before := e.rejections(id)
		err := e.answer(c.key, d, c.sender, c.ref, "A")
		if code(err) != c.want {
			t.Fatalf("%s: %v", name, err)
		}
		if got := e.rejections(id) - before; (got == 1) != c.wantRejectedEv {
			t.Fatalf("%s: %d rejection events", name, got)
		}
		e.stillAsking(id)
	}
	// 没有回报送达的投递没有 ref，任何回答都拒绝。
	e2 := setup(t)
	var out struct{ ID string }
	_ = json.Unmarshal(e2.do(owner, "item.create", map[string]any{"title": "t", "kind": "task", "phase": "open", "assignee": "wrk"}), &out)
	e2.do(worker, "decision.request", map[string]any{"id": out.ID, "ask": map[string]any{"question": "q", "options": []map[string]any{{"id": "A", "label": "a"}}}})
	var fresh domain.Delivery
	e2.update(func(tx *store.Tx) error {
		g, err := e2.q.Claim(tx, delivery.ChannelRoute("chan"))
		fresh = *g
		return err
	})
	if err := e2.answer(chanKey, fresh, "alice", "", "A"); code(err) != domain.CodePermissionDenied {
		t.Fatalf("unacked delivery: %v", err)
	}
}

// ACT-CHP-07 只认第一次回答：第二次返回冲突，已有结果不变。
func TestChannelAnswerFirstOneWins(t *testing.T) {
	e := setup(t)
	id, d, _ := e.sent()
	if err := e.answer(chanKey, d, "alice", "msg-"+id, "A"); err != nil {
		t.Fatal(err)
	}
	if err := e.answer(chanKey, d, "alice", "msg-"+id, "B"); code(err) != domain.CodeConflict {
		t.Fatalf("second answer: %v", err)
	}
	if r := e.item(id).Ask.Resolution; r.Choice != "A" {
		t.Fatalf("result overwritten: %+v", r)
	}
}

// web_only 类同样拒绝并回 Web 链接，且留拒绝记录。
func TestChannelAnswerWebOnlyRefusedWithWebLink(t *testing.T) {
	e := setup(t)
	id, d, token := e.sent("money")
	if token != "" {
		t.Fatalf("web_only decision got a form link: %q", d.Content.FormURL)
	}
	err := e.answer(chanKey, d, "alice", "msg-"+id, "A")
	de, ok := err.(*domain.Error)
	if !ok || de.Code != domain.CodeWebOnly || de.WebURL == nil || *de.WebURL != "/items/"+id {
		t.Fatalf("got %#v", err)
	}
	e.stillAsking(id)
	if e.rejections(id) != 1 {
		t.Fatal("web_only rejection not recorded")
	}
}

// 事项变化后旧版本投递作废，旧消息里的回答被拒绝。
func TestChannelAnswerOnOutdatedDeliveryConflicts(t *testing.T) {
	e := setup(t)
	id, d, _ := e.sent()
	e.do(owner, "item.update", map[string]any{"id": id, "title": "changed"})
	if err := e.answer(chanKey, d, "alice", "msg-"+id, "A"); code(err) != domain.CodeConflict {
		t.Fatalf("outdated answer: %v", err)
	}
	e.stillAsking(id)
}

func (e *env) formAnswer(token, choice string) error {
	_, err := e.try(form, channel.CmdFormAnswer, map[string]any{"token": token, "choice": choice})
	return err
}

// ACT-CHP-10 表单：第一次可答并记渠道名，用过即失效。
func TestFormAnswersOnceThenExpires(t *testing.T) {
	e := setup(t)
	id, _, token := e.sent()
	if token == "" {
		t.Fatal("no form link")
	}
	if err := e.formAnswer(token, "B"); err != nil {
		t.Fatal(err)
	}
	if r := e.item(id).Ask.Resolution; r == nil || r.Choice != "B" || r.ResolvedVia != "chan" {
		t.Fatalf("resolution %+v", r)
	}
	if err := e.formAnswer(token, "A"); code(err) != domain.CodeConflict {
		t.Fatalf("reused link: %v", err)
	}
	if err := e.formAnswer("unknown", "A"); code(err) != domain.CodeNotFound {
		t.Fatalf("unknown link: %v", err)
	}
}

func TestFormExpiresAfterTTL(t *testing.T) {
	e := setup(t)
	id, _, token := e.sent()
	e.now = e.now.Add(channel.FormTTL)
	if err := e.formAnswer(token, "A"); code(err) != domain.CodeConflict {
		t.Fatalf("expired link: %v", err)
	}
	e.stillAsking(id)
}

// 链接只对发放时的那个版本有效：事项变化后旧链接作答被拒绝，令牌不算用过。
func TestFormOutdatedVersionRejected(t *testing.T) {
	e := setup(t)
	id, _, token := e.sent()
	e.do(owner, "item.update", map[string]any{"id": id, "title": "changed"})
	if err := e.formAnswer(token, "A"); code(err) != domain.CodeConflict {
		t.Fatalf("outdated link: %v", err)
	}
	e.stillAsking(id)
}

// 策略在发放后把决策划为 web_only：链接作答仍被拒绝并回 Web 链接。
func TestFormRefusedWhenDecisionBecameWebOnly(t *testing.T) {
	e := setup(t)
	id, _, token := e.sent()
	e.policy(func(p *policy.Policy) {
		p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Kind: "task", Category: policy.CategoryFunds})
	})
	err := e.formAnswer(token, "A")
	de, ok := err.(*domain.Error)
	if !ok || de.Code != domain.CodeWebOnly || de.WebURL == nil || *de.WebURL != "/items/"+id {
		t.Fatalf("got %#v", err)
	}
	e.stillAsking(id)
}

// 表单令牌不能当渠道凭据，渠道 ext 密钥也不能冒用表单命令之外的命令：非渠道 ext 密钥被拒绝。
func TestNonChannelExtKeyCannotAnswer(t *testing.T) {
	e := setup(t)
	id, d, _ := e.sent()
	if err := e.answer(ext("export"), d, "alice", "msg-"+id, "A"); code(err) != domain.CodePermissionDenied {
		t.Fatalf("export key: %v", err)
	}
	if err := e.answer(owner, d, "alice", "msg-"+id, "A"); err == nil {
		t.Fatal("web login without ext key must not go through the channel path")
	}
	e.stillAsking(id)
}
