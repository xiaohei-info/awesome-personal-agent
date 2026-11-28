package background

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/why"
)

type mentioned struct {
	ID      string   `json:"id"`
	Comment string   `json:"comment"`
	Items   []string `json:"items"`
}

func (e *env) mention(id command.Identity, in map[string]any) (mentioned, error) {
	var out mentioned
	r, err := e.do(id, "delegate.mention", in, command.Chain{})
	if err == nil {
		_ = json.Unmarshal(r.Value, &out)
	}
	return out, err
}

func (e *env) progress(item string) []domain.Progress {
	e.t.Helper()
	var ps []domain.Progress
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ps, err = items.Progress(tx, item); return }); err != nil {
		e.t.Fatal(err)
	}
	return ps
}

// mentionStates 读讨论流里发言 comment 的 @ 状态。
func (e *env) mentionStates(item, comment string) map[string]string {
	e.t.Helper()
	out := map[string]string{}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		th, err := why.ItemThread(tx, pol, why.Request{ID: item, Actor: owner.Actor, Level: authz.LevelWeb}, why.Page{})
		for _, en := range th.Entries {
			if en.Kind == why.KProgress && en.Args["id"] == comment {
				for _, m := range en.Args["mentions"].([]why.Mention) {
					out[m.Target] = m.State
				}
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// 讨论里的 @（§6.3，决定 100）：一条发言为每个被 @ 的执行者建委派子事项并由 APA 拉起；叫不动任何一个整条不生效；
// 同一执行者在这件事上没跑完时再 @ 被拒，运行凭据不能 @ 自己、可以 @ 别的执行者；回执作为对那条发言的回复写回讨论。
func TestMentionStartsExecutorsAndTheirReportsReplyToTheComment(t *testing.T) {
	e := setup(t, 0)
	e.svc.Attach(e.x)
	e.declareStartable()
	parent := e.newItem("dedupe design", nil, nil)
	e.open(parent)

	for name, in := range map[string]map[string]any{
		"more than three":                 {"mentions": []string{"a", "b", "c", "d"}},
		"duplicate":                       {"mentions": []string{"px.quick", "px.quick"}},
		"unknown executor":                {"mentions": []string{"px.quick", "nobody"}},
		"executor APA cannot start":       {"mentions": []string{"work-host.cli"}},
		"main session not set":            {"mentions": []string{domain.MentionMain}},
		"overrides with two executors":    {"mentions": []string{"px.quick", "px.far"}, "overrides": map[string]string{"model": "m1"}},
		"override outside the choices":    {"mentions": []string{"px.quick"}, "overrides": map[string]string{"model": "free"}},
		"reply to a comment that is gone": {"mentions": []string{"px.quick"}, "reply_to": parent + ".p000009"},
	} {
		in["id"], in["text"] = parent, "看一下"
		if _, err := e.mention(owner, in); err == nil {
			t.Errorf("%s: the whole comment must be refused", name)
		}
	}
	if p := e.item(parent); p.Phase != domain.PhaseOpen || len(e.progress(parent)) != 0 {
		t.Fatalf("a refused mention writes nothing: %s %+v", p.Phase, e.progress(parent))
	}

	e.gate.paused.Store(true) // 运行留在队列里：委派记录在途，执行者算"在跑"
	m, err := e.mention(owner, map[string]any{"id": parent, "text": "并发下会不会重复建事项？\n细节见上面", "mentions": []string{"px.quick"}})
	if err != nil {
		t.Fatal(err)
	}
	child := e.item(m.Items[0])
	ps := e.progress(parent)
	if len(m.Items) != 1 || child.Origin == nil || child.Origin.Source != delegation.OriginMention || child.Origin.Ref != m.Comment ||
		child.Title != "并发下会不会重复建事项？" || child.Assignee != "px.quick" || len(ps) != 1 || ps[0].ID != m.Comment || !slices.Equal(ps[0].Mentions, []string{"px.quick"}) {
		t.Fatalf("comment and child: %+v %+v %+v", m, child, ps)
	}
	if p := e.item(parent); p.Phase != domain.PhaseWaiting {
		t.Fatalf("the item waits on the mentioned executor: %s", p.Phase)
	}
	if s := e.mentionStates(parent, m.Comment); s["px.quick"] != why.MentionRunning {
		t.Fatalf("mention state while running: %v", s)
	}
	if _, err := e.mention(owner, map[string]any{"id": parent, "text": "再看一下", "mentions": []string{"px.quick"}}); err == nil || !strings.Contains(err.Error(), delegation.BlockBusy) {
		t.Fatalf("an executor still running here cannot be mentioned again: %v", err)
	}

	// 执行者的运行凭据：不能 @ 自己，可以在同一讨论里 @ 别的执行者接着做（子事项也挂在讨论所在的事项下）。
	var run1 string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		ds, err := delegation.Dispatches(tx, child.ID)
		if len(ds) == 1 {
			run1 = ds[0].Run
		}
		return err
	}); err != nil || run1 == "" {
		t.Fatalf("dispatch of the mention: %v", err)
	}
	agent := e.runIdentity(t, run1)
	if _, err := e.mention(agent, map[string]any{"id": parent, "text": "我自己", "mentions": []string{"px.quick"}}); err == nil || !strings.Contains(err.Error(), delegation.BlockSelf) {
		t.Fatalf("an executor cannot mention itself: %v", err)
	}
	relay, err := e.mention(agent, map[string]any{"id": parent, "text": "请按会话加幂等键实现", "reply_to": m.Comment, "mentions": []string{"px.far"}})
	if err != nil {
		t.Fatal(err)
	}
	if c := e.item(relay.Items[0]); c.ChainDepth <= child.ChainDepth || !c.Unattended {
		t.Fatalf("the relayed child is deeper in the chain and unattended: %d vs %d %v", c.ChainDepth, child.ChainDepth, c.Unattended)
	}

	e.gate.paused.Store(false)
	if err := e.gate.Dispatch(bg); err != nil {
		t.Fatal(err)
	}
	e.waitResult()
	e.waitResult()
	ps = e.progress(parent)
	var replies []domain.Progress
	for _, p := range ps {
		if p.ReplyTo == m.Comment && p.Text == "did it" {
			replies = append(replies, p)
		}
	}
	if len(replies) != 1 {
		t.Fatalf("the executor's report is one reply to the comment that called it: %+v", ps)
	}
	if s := e.mentionStates(parent, m.Comment); s["px.quick"] != why.MentionReplied {
		t.Fatalf("mention state after the report: %v", s)
	}
	if p := e.item(parent); p.Phase != domain.PhaseOpen {
		t.Fatalf("both children done: the item is back to open, got %s", p.Phase)
	}
	// 不带 @ 的回复不叫醒任何人。
	runs := len(e.runList())
	e.must(owner, "item.progress", map[string]any{"id": parent, "text": "收到", "reply_to": replies[0].ID})
	if len(e.runList()) != runs {
		t.Fatal("a reply without a mention starts nothing")
	}
}

// 链路深度沿用"连锁最多几层"（§14.9）：执行者在讨论里接力 @ 时每次加深，超过上限整条不生效。
func TestMentionRelayStopsAtTheChainLimit(t *testing.T) {
	e := setup(t, 2)
	e.svc.Attach(e.x)
	e.declareStartable()
	parent := e.newItem("loop", nil, nil)
	e.open(parent)
	e.gate.paused.Store(true)
	m, err := e.mention(owner, map[string]any{"id": parent, "text": "看看", "mentions": []string{"px.quick"}})
	if err != nil {
		t.Fatal(err)
	}
	var run string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		ds, err := delegation.Dispatches(tx, m.Items[0])
		run = ds[0].Run
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before := len(e.progress(parent))
	_, err = e.mention(e.runIdentity(t, run), map[string]any{"id": parent, "text": "交给你", "mentions": []string{"px.far"}})
	var de *domain.Error
	if err == nil || !errors.As(err, &de) || de.Reason != domain.ReasonChainDepthExceeded {
		t.Fatalf("a relay over the chain limit is refused: %v", err)
	}
	if len(e.progress(parent)) != before {
		t.Fatal("the refused relay writes no comment")
	}
}

// @ 主会话：不建子事项、不拉起运行，触达给主会话发一条唤醒，正文带怎么回复；事项之后的变化不作废它。
func TestMentionMainSessionWakesIt(t *testing.T) {
	e := setup(t, 0)
	e.svc.Attach(e.x)
	e.wakeHarness("hx")
	e.setPolicy(func(p *policy.Policy) { main := "legacy:hx"; p.MainAgentID = &main })
	parent := e.newItem("ask main", nil, nil)
	e.open(parent)
	m, err := e.mention(owner, map[string]any{"id": parent, "text": "你怎么看", "mentions": []string{domain.MentionMain}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Items) != 0 || len(e.runList()) != 0 || e.item(parent).Phase != domain.PhaseOpen {
		t.Fatalf("mentioning the main session creates no child and no run: %+v", m)
	}
	a := attention.New(attention.Options{Queue: delivery.New(delivery.Options{Now: e.clock}), Now: e.clock})
	for range 2 { // 重新追赶不会再入队
		if err := e.st.Update(bg, a.Sync); err != nil {
			t.Fatal(err)
		}
	}
	e.must(owner, "item.update", map[string]any{"id": parent, "summary": "changed", "reason": "edit"})
	if err := e.st.Update(bg, a.Sync); err != nil {
		t.Fatal(err)
	}
	var d domain.Delivery
	var n int
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		all, err := delivery.Since(tx, t0.Add(-time.Hour))
		for _, s := range all {
			if strings.HasPrefix(s.ID, delivery.MentionPrefix) {
				n++
			}
		}
		if err == nil {
			d, err = delivery.New(delivery.Options{}).Get(tx, attention.MentionWakeID(m.Comment))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 1 || d.Route != delivery.RouteWake || d.State == domain.DeliverySuperseded ||
		!strings.Contains(d.Content.Summary, "--reply-to "+m.Comment) || !strings.Contains(d.Content.Summary, "你怎么看") {
		t.Fatalf("one wake for the main session: n=%d %+v", n, d)
	}
	if s := e.mentionStates(parent, m.Comment); s[domain.MentionMain] != why.MentionSent {
		t.Fatalf("main session mention state: %v", s)
	}
}
