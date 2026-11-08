package authz_test

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/why"
)

// 授权单作为第二档依据（§14.5）：走真实凭据、真实授权记录、真实命令管线。

func (e *env) grantAt(at time.Time, cred string, mod func(*policy.GrantInput)) policy.Grant {
	e.t.Helper()
	in := policy.GrantInput{Grantee: policy.Grantee{Credential: cred}, Scope: policy.Scope{Kinds: []string{"task"}},
		Actions: []string{"decision.answer", "item.update", "item.transition"}, ExpiresAt: at.Add(24 * time.Hour)}
	if mod != nil {
		mod(&in)
	}
	var g policy.Grant
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { g, err = policy.IssueGrant(tx, e.au, owner.Actor, at, in); return }); err != nil {
		e.t.Fatal(err)
	}
	return g
}

func (e *env) grant(name string, mod func(*policy.GrantInput)) policy.Grant {
	return e.grantAt(e.now(), e.principal(name), mod)
}

func withGrant(id string) func(*command.Request) { return func(r *command.Request) { r.Grant = id } }

// asked 建一个等待决定的事项（执行者是 WRK 之外的人，DEL 没有第一档绑定）。
func (e *env) asked() string {
	e.t.Helper()
	id := e.item(workHost, "ask")
	e.cmd(e.as("WRK"), "decision.request", map[string]any{"id": id, "ask": map[string]any{"question": "ship?",
		"options": []map[string]any{{"id": "A", "label": "yes"}}}})
	return id
}

func (e *env) events4(obj string) []domain.Event {
	e.t.Helper()
	var out []domain.Event
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, obj)
		for _, en := range es {
			out = append(out, en.Event)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// ACT-AUTH-03、ACT-CHAT-03、ACT-REV-01/02：授权单代答 → resolved_via=grant、Event 记 grant、apa why 可见；
// 本人在 Web 撤销后 ask 回到未回答、事项回 needs_input，可再次代答。
func TestGrantAnswerIsAuditedAndRevertible(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	g := e.grant("DEL", nil)
	id := e.asked()

	e.cmd(e.as("DEL"), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(g.ID))
	res := e.get(id).Ask.Resolution
	if res == nil || res.ResolvedVia != domain.ResolvedViaGrant || res.Grant != g.ID || res.Evidence != "" {
		t.Fatalf("resolution: %+v", res)
	}
	var ev domain.Event
	for _, x := range e.events4(id) {
		if x.Type == domain.EvDecisionResolved {
			ev = x
		}
	}
	if ev.Grant != g.ID || ev.ResolvedVia != domain.ResolvedViaGrant || ev.Actor.OnBehalfOf == "" && ev.Actor.Type != domain.ActorAgent {
		t.Fatalf("event must record the grant: %+v", ev)
	}
	var tl why.Timeline
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		tl, err = why.Replay(tx, policy.Policy{}, why.Request{Actor: owner.Actor, Level: owner.Level, ID: id})
		return
	}); err != nil {
		t.Fatal(err)
	}
	seen := false
	for _, s := range tl.Steps {
		seen = seen || (s.Type == domain.EvDecisionResolved && s.Grant == g.ID)
	}
	if !seen {
		t.Fatalf("why does not show the grant: %+v", tl.Steps)
	}

	e.wantExit(20, e.as("DEL"), "item.revert", map[string]any{"id": id, "event": ev.Seq})
	e.cmd(owner, "item.revert", map[string]any{"id": id, "event": ev.Seq})
	it := e.get(id)
	if it.Phase != domain.PhaseNeedsInput || it.Ask == nil || it.Ask.Resolution != nil {
		t.Fatalf("revert must restore the unanswered ask: %+v", it)
	}
	e.cmd(e.as("DEL"), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(g.ID)) // 授权单未被撤销销毁
	if err := e.st.View(bg, items.Verify); err != nil {
		t.Fatal(err)
	}
}

// §14.5 校验顺序：每一项不符都是退出码 23、记 command.rejected、事项不变、why 说明原因。
func TestGrantMismatchIsExit23InOrder(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.issue("DEL2", auth.Spec{Kind: auth.Delegate})
	id := e.asked()
	past := e.now().Add(-72 * time.Hour)
	revoked := e.grant("DEL", nil)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.RevokeGrant(tx, owner.Actor, e.now(), revoked.ID, "back")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, why string
		id        string
	}{
		{"other credential", "another credential", e.grant("DEL2", nil).ID},
		{"other harness", "another harness", e.grant("DEL", func(in *policy.GrantInput) { in.Grantee.Harness = "elsewhere" }).ID},
		{"revoked", "revoked", revoked.ID},
		{"expired", "expired", e.grantAt(past, e.principal("DEL"), nil).ID},
		{"action", "does not cover", e.grant("DEL", func(in *policy.GrantInput) { in.Actions = []string{"item.update"} }).ID},
		{"scope kind", "scope", e.grant("DEL", func(in *policy.GrantInput) { in.Scope = policy.Scope{Kinds: []string{"note"}} }).ID},
		{"scope item", "scope", e.grant("DEL", func(in *policy.GrantInput) { in.Scope = policy.Scope{Items: []string{"itm_other"}} }).ID},
		{"unknown", "does not exist", "grt_nope"},
	}
	for _, c := range cases {
		rej := e.rejected()
		de := e.wantExit(23, e.as("DEL"), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(c.id))
		if !strings.Contains(de.Why, c.why) || de.Next == "" {
			t.Errorf("%s: why %q should mention %q", c.name, de.Why, c.why)
		}
		if e.rejected() != rej+1 || e.get(id).Ask.Resolution != nil {
			t.Errorf("%s: rejection must be recorded and leave the ask unanswered", c.name)
		}
	}
}

// 原话与授权单同时携带 → 退出码 2；管理命令不接受授权单 → 23；其余硬约束不因授权单放宽。
func TestGrantBoundaries(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	g := e.grant("DEL", nil)
	id := e.asked()
	e.wantExit(2, e.as("DEL"), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(g.ID), func(r *command.Request) { r.Evidence = "owner said" })
	if e.get(id).Ask.Resolution != nil {
		t.Fatal("answer must not take effect")
	}
	e.wantExit(23, e.as("DEL"), "project.create", map[string]any{"name": "P"}, withGrant(g.ID))
	e.wantExit(4, e.as("WRK"), "item.update", map[string]any{"id": id, "summary": "x"}, withGrant(g.ID)) // 授权单只对 delegate 生效

	// web_only 仍 20，与授权单无关。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Kind: "task", Category: policy.CategoryIrreversible})
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.wantExit(20, e.as("DEL"), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(g.ID))
	// 非决策命令的风险超过上限：23。
	e.wantExit(23, e.as("DEL"), "item.update", map[string]any{"id": id, "summary": "x"}, withGrant(g.ID))
}

// 初筛未返回仍 22。
func TestGrantDoesNotBypassScreening(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	g := e.grant("DEL", nil)
	id := e.asked()
	it := e.get(id)
	it.Ask.ScreeningPending = true
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := items.Revise(tx, id, "test", e.now(), func(x *domain.Item) error { x.Ask = it.Ask; return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.wantExit(22, e.as("DEL"), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(g.ID))
}

// 授权单不扩散：不继承到子事项；移动事项时来源和目标都要在范围内；第一档的工作不使用授权单。
func TestGrantDoesNotSpread(t *testing.T) {
	e := setup(t)
	p1 := e.cmd(owner, "project.create", map[string]any{"name": "P1"})
	p2 := e.cmd(owner, "project.create", map[string]any{"name": "P2"})
	p3 := e.cmd(owner, "project.create", map[string]any{"name": "P3"})
	mk := func(prj string) string {
		return e.cmd(owner, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "open", "project": prj})
	}
	g := e.grant("DEL", func(in *policy.GrantInput) { in.Scope = policy.Scope{Projects: []string{p1, p2}} })
	in, out, child := mk(p1), mk(p3), mk(p3)
	e.cmd(e.as("DEL"), "item.update", map[string]any{"id": in, "summary": "ok"}, withGrant(g.ID))
	for _, ev := range e.events4(in) {
		if ev.Type == domain.EvItemUpdated && ev.Actor.Type == domain.ActorAgent && ev.Grant != g.ID {
			t.Fatalf("grant use must be recorded: %+v", ev)
		}
	}
	e.wantExit(23, e.as("DEL"), "item.update", map[string]any{"id": out, "summary": "no"}, withGrant(g.ID))
	e.wantExit(23, e.as("DEL"), "item.update", map[string]any{"id": child, "summary": "no"}, withGrant(g.ID)) // 范围外对象的子事项同理
	e.wantExit(23, e.as("DEL"), "item.relate", map[string]any{"from": in, "to": out, "type": "blocks"}, withGrant(g.ID))
	e.cmd(e.as("DEL"), "item.update", map[string]any{"id": in, "project": p2}, withGrant(g.ID)) // 来源、目标都在范围内
	e.wantExit(23, e.as("DEL"), "item.update", map[string]any{"id": in, "project": p3}, withGrant(g.ID))
	if got := e.get(in).Project; got != p2 {
		t.Fatalf("item in %s, move to out-of-scope project must not happen", got)
	}
	e.wantExit(23, e.as("DEL"), "item.update", map[string]any{"id": out, "project": p1}, withGrant(g.ID))

	// 第一档（绑定的事项）不使用授权单，Event 不记 grant。
	own := e.item(e.principal("DEL"), "mine")
	e.cmd(e.as("DEL"), "item.update", map[string]any{"id": own, "summary": "mine"}, withGrant(g.ID))
	for _, ev := range e.events4(own) {
		if ev.Grant != "" {
			t.Fatalf("tier-one work must not record a grant: %+v", ev)
		}
	}
}

// 吊销 token 后，授权单同时失效。
func TestGrantDiesWithItsToken(t *testing.T) {
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	g := e.grant("DEL", nil)
	id := e.asked()
	if err := e.st.Update(bg, func(tx *store.Tx) error { return e.au.Revoke(tx, g.Grantee.Credential) }); err != nil {
		t.Fatal(err)
	}
	e.wantExit(domain.ExitUnauthenticated, e.as("DEL"), "decision.answer", map[string]any{"id": id, "choice": "A"}, withGrant(g.ID))
	var live []policy.Grant
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		live, err = policy.ActiveGrants(tx, e.au, g.Grantee.Credential, e.now())
		return
	}); err != nil || len(live) != 0 {
		t.Fatalf("grant must die with its token: %v %v", live, err)
	}
}
