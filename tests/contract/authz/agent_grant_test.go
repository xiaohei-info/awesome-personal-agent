package authz_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 长期授权对 Agent 后台运行生效（§14.5，决定 54）：签给 Agent 的授权单按内核记录的 harness 匹配；
// 运行凭据只在单上允许后台使用、对象同时在运行范围与授权范围内、触发可信时可用；代为接受建议只限可信发件人带来的。

const agent = "hermes"

// agentEnv 在 setup 之上登记决策与建议命令、一把归属 hermes 的 delegate（HDEL），并把 gh 来源的 alice 设为信任的发件人。
func agentEnv(t *testing.T) *env {
	t.Helper()
	e := setup(t)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := inbound.RegisterAttach(e.x); err != nil {
		t.Fatal(err)
	}
	e.issue("HDEL", auth.Spec{Kind: auth.Delegate, Harness: agent})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.ForwardAllowlist = map[string][]string{"gh": {"alice"}, "mail": {"alice@example.invalid"}}
			return nil
		})
		if err != nil {
			return err
		}
		var s string
		for _, ev := range []struct {
			src, id, sender string
			verified        bool
		}{{"gh", "ev-trusted", "alice", true}, {"mail", "ev-mail", "alice@example.invalid", false}} {
			if err := runner.AppendInternal(tx, domain.Source{ID: ev.src}, domain.SourceEvent{ID: ev.id, Type: "x", Subject: "s",
				OccurredAt: e.now(), Sender: ev.sender, SenderVerified: ev.verified}); err != nil {
				return err
			}
			rows, err := tx.Query("SELECT id FROM sources_runner_event WHERE event_id = ?", ev.id)
			if err != nil {
				return err
			}
			for rows.Next() {
				if err := rows.Scan(&s); err != nil {
					rows.Close()
					return err
				}
			}
			rows.Close()
			e.sev[ev.id] = s
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return e
}

// agentGrant 以 Web 本人签发一张签给 hermes 的长期授权单。
func (e *env) agentGrant(mod func(*policy.GrantInput)) policy.Grant {
	e.t.Helper()
	in := policy.GrantInput{Grantee: policy.Grantee{Agent: agent, Background: true}, Scope: policy.Scope{All: true},
		Actions: []string{"decision.answer", "proposal.accept", "proposal.dismiss", "proposal.attach", "item.update"}}
	if mod != nil {
		mod(&in)
	}
	var g policy.Grant
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { g, err = policy.IssueGrant(tx, e.au, owner.Actor, e.now(), in); return }); err != nil {
		e.t.Fatal(err)
	}
	return g
}

// runAs 为 target 起一个 harness 为 runHarness 的后台运行，返回运行凭据身份（自报的 harness 取 claimed）。
func (e *env) runAs(target, runHarness, claimed string) (command.Identity, string) {
	e.t.Helper()
	var secret, id string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: target, Harness: runHarness, TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{"true"}}}, e.now())
		if err != nil {
			return err
		}
		id = r.ID
		secret, err = runidentity.Issue(tx, r.ID, e.now())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return runidentity.Identity{Secret: secret, Origin: auth.Origin{Harness: claimed}, Now: e.now}, id
}

// askedFrom 同 asked，但事项由来源事件 ev 建立（ext.source_event）。
func (e *env) askedFrom(ev string) string {
	e.t.Helper()
	id := e.cmd(owner, "item.create", map[string]any{"kind": "task", "title": "from " + ev, "phase": "open", "assignee": workHost,
		"ext": map[string]string{"source_event": e.sev[ev]}})
	e.cmd(e.as("WRK"), "decision.request", map[string]any{"id": id, "ask": map[string]any{"question": "ship?",
		"options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}})
	return id
}

// childAsked 在 parent 下委派一个子事项并在它上面提问。
func (e *env) childAsked(parent string) string {
	e.t.Helper()
	id := e.cmd(owner, "delegate.create", map[string]any{"parent": parent, "assignee": workHost, "title": "child"})
	e.cmd(e.as("WRK"), "decision.request", map[string]any{"id": id, "ask": map[string]any{"question": "ship?",
		"options": []map[string]any{{"id": "A", "label": "yes"}, {"id": "B", "label": "no"}}}})
	return id
}

// proposalFrom 建一条建议；ev 非空时它由该来源事件带来。
func (e *env) proposalFrom(ev string) string {
	e.t.Helper()
	in := map[string]any{"kind": "task", "title": "proposal " + ev, "phase": "proposed"}
	if ev != "" {
		in["ext"] = map[string]string{"source_event": e.sev[ev]}
	}
	return e.cmd(owner, "item.create", in)
}

func answer(id string) map[string]any { return map[string]any{"id": id, "choice": "A"} }

func wantWhy(t *testing.T, de *domain.Error, part string) {
	t.Helper()
	if !strings.Contains(de.Why, part) {
		t.Fatalf("why %q does not mention %q", de.Why, part)
	}
}

// T10 a：运行凭据凭签给本 Agent、允许后台使用的单，在运行范围内代答；Event 带 on_behalf_of、授权单与运行 id。
func TestRunAnswersWithAgentGrantInsideItsRun(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	id := e.asked()
	run, runID := e.runAs(id, agent, agent)
	e.cmd(run, "decision.answer", answer(id), withGrant(g.ID))
	var ev domain.Event
	for _, x := range e.events4(id) {
		if x.Type == domain.EvDecisionResolved {
			ev = x
		}
	}
	if ev.Grant != g.ID || ev.ResolvedVia != domain.ResolvedViaGrant || ev.Actor.OnBehalfOf != "owner" || ev.Actor.Run != runID {
		t.Fatalf("event must say the run decided for the owner under the grant: %+v", ev)
	}
}

// T10 b、c、h：允许后台使用、Agent 一致都要内核记录的值；自报的 harness 骗不过；签给 token 的旧式单对运行是 23（原为 4）。
func TestRunGrantMatchesOnlyKernelRecordedAgentAndBackground(t *testing.T) {
	e := agentEnv(t)
	id := e.asked()
	fg := e.agentGrant(func(in *policy.GrantInput) { in.Grantee.Background = false })
	run, _ := e.runAs(id, agent, agent)
	wantWhy(t, e.wantExit(23, run, "decision.answer", answer(id), withGrant(fg.ID)), "grant_not_for_background")

	g := e.agentGrant(nil)
	id2 := e.asked()
	other, _ := e.runAs(id2, "other-agent", agent) // 自报 hermes，运行记录是别的 Agent
	wantWhy(t, e.wantExit(23, other, "decision.answer", answer(id2), withGrant(g.ID)), "another Agent")

	old := e.grant("DEL", func(in *policy.GrantInput) { in.Scope = policy.Scope{Kinds: []string{"task"}} })
	wantWhy(t, e.wantExit(23, run, "decision.answer", answer(id), withGrant(old.ID)), "grant_not_for_background")
	if e.get(id).Ask.Resolution != nil {
		t.Fatal("no answer may take effect")
	}
	e.cmd(e.as("DEL"), "decision.answer", answer(id), withGrant(old.ID)) // 旧式单对 delegate 不变
}

// T10 d：对象在授权范围内但不在运行范围内：23，授权单不扩大运行的写入面。
func TestRunGrantDoesNotWidenRunScope(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	id := e.asked()
	run, _ := e.runAs(e.item(workHost, "other target"), agent, agent)
	wantWhy(t, e.wantExit(23, run, "decision.answer", answer(id), withGrant(g.ID)), "outside this run's scope")
	e.wantExit(4, run, "decision.answer", answer(id)) // 不带单：照旧没有代答能力
}

// T10 e：web_only 仍 20，初筛中仍 22，与授权单无关。
func TestRunGrantKeepsWebOnlyAndScreening(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	id := e.asked()
	run, _ := e.runAs(id, agent, agent)
	it := e.get(id)
	it.Ask.ScreeningPending = true
	revise(e, id, func(x *domain.Item) { x.Ask = it.Ask })
	e.wantExit(22, run, "decision.answer", answer(id), withGrant(g.ID))
	it.Ask.ScreeningPending = false
	revise(e, id, func(x *domain.Item) { x.Ask = it.Ask })
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error {
			p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Kind: "task", Category: policy.CategoryIrreversible})
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.wantExit(20, run, "decision.answer", answer(id), withGrant(g.ID))
}

// T10 f：R1。触发内容来自不在信任名单（或核实不了）的发件人时整次运行不能用授权；由本人、时钟建的，或来自信任发件人的可以。
func TestRunGrantNeedsTrustedTrigger(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)

	bad := e.askedFrom("ev-mail")
	run, _ := e.runAs(bad, agent, agent)
	wantWhy(t, e.wantExit(23, run, "decision.answer", answer(bad), withGrant(g.ID)), "untrusted_trigger")
	if e.rejected() == 0 {
		t.Fatal("the rejection must be recorded")
	}

	good := e.askedFrom("ev-trusted")
	run, _ = e.runAs(good, agent, agent)
	e.cmd(run, "decision.answer", answer(good), withGrant(g.ID))

	// 委派子事项沿父事项追到触发内容：父事项来自核实不了的发件人，子事项上的运行同样不能用授权。
	under := e.childAsked(e.cmd(owner, "item.create", map[string]any{"kind": "task", "title": "mail parent", "phase": "open", "assignee": workHost,
		"ext": map[string]string{"source_event": e.sev["ev-mail"]}}))
	run, _ = e.runAs(under, agent, agent)
	wantWhy(t, e.wantExit(23, run, "decision.answer", answer(under), withGrant(g.ID)), "untrusted_trigger")

	// 时钟与实例规则以 system 身份、凭规则依据建事项（同 kernel/rules 的 do）。
	clock := command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "kernel:clock", Via: domain.ViaClock, OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
	timed := e.cmd(clock, "item.create", map[string]any{"kind": "task", "title": "from a schedule", "phase": "open", "assignee": workHost},
		func(r *command.Request) { r.Evidence = "rule instance" })
	child := e.childAsked(timed)
	run, _ = e.runAs(child, agent, agent)
	e.cmd(run, "decision.answer", answer(child), withGrant(g.ID))

	byAgent := e.cmd(e.as("HDEL"), "item.create", map[string]any{"kind": "task", "title": "agent's own idea", "phase": "proposed"})
	run, _ = e.runAs(byAgent, agent, agent)
	wantWhy(t, e.wantExit(23, run, "proposal.dismiss", map[string]any{"id": byAgent}, withGrant(g.ID)), "untrusted_trigger")
}

// T10 g：R2。签给 Agent 的单代为接受、补到事项只限可信发件人带来的建议；忽略不受限。旧式单不受 R2 影响。
func TestAgentGrantAcceptsOnlyTrustedProposals(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	mail := e.proposalFrom("ev-mail")
	wantWhy(t, e.wantExit(23, e.as("HDEL"), "proposal.accept", map[string]any{"id": mail}, withGrant(g.ID)), "untrusted_origin")
	target := e.item(workHost, "attach target")
	wantWhy(t, e.wantExit(23, e.as("HDEL"), "proposal.attach", map[string]any{"id": mail, "to": target}, withGrant(g.ID)), "untrusted_origin")
	e.cmd(e.as("HDEL"), "proposal.dismiss", map[string]any{"id": mail}, withGrant(g.ID))

	wantWhy(t, e.wantExit(23, e.as("HDEL"), "proposal.accept", map[string]any{"id": e.proposalFrom("")}, withGrant(g.ID)), "untrusted_origin")
	e.cmd(e.as("HDEL"), "proposal.accept", map[string]any{"id": e.proposalFrom("ev-trusted")}, withGrant(g.ID))

	e.wantExit(23, e.as("DEL"), "proposal.accept", map[string]any{"id": e.proposalFrom("ev-trusted")}, withGrant(g.ID)) // 不属于 hermes 的 token
	old := e.grant("DEL", func(in *policy.GrantInput) { in.Actions = []string{"proposal.accept"} })
	e.cmd(e.as("DEL"), "proposal.accept", map[string]any{"id": e.proposalFrom("ev-mail")}, withGrant(old.ID))
}

// T10 i：唤醒租约期间被降级的 delegate（运行级、没有运行记录）带单仍是 4。真实租约路径见 e2e ACT-WAK-04。
func TestLeasedDelegateCannotUseAgentGrant(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	id := e.asked()
	leased := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "cred_leased", Via: domain.ViaCLI, Harness: agent},
		Level: authz.LevelRun, Kind: auth.Delegate, CredentialHarness: agent}
	e.wantExit(4, leased, "decision.answer", answer(id), withGrant(g.ID))
}

// T10 j：不设有效期的单一直有效；撤销后立即失效。
func TestLongLivedAgentGrantLastsUntilRevoked(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	if !g.ExpiresAt.IsZero() || !g.Active(e.now().AddDate(5, 0, 0)) {
		t.Fatalf("grant without expiry must stay active: %+v", g)
	}
	id := e.asked()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.RevokeGrant(tx, owner.Actor, e.now(), g.ID, "done")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run, _ := e.runAs(id, agent, agent)
	wantWhy(t, e.wantExit(23, run, "decision.answer", answer(id), withGrant(g.ID)), "revoked")
}

// T11：GET /grants 对运行凭据只列签给该运行所属 Agent、允许后台使用的有效单。
func TestRunListsOnlyItsAgentsBackgroundGrants(t *testing.T) {
	e := agentEnv(t)
	bgGrant := e.agentGrant(nil)
	e.agentGrant(func(in *policy.GrantInput) { in.Grantee.Background = false })
	e.agentGrant(func(in *policy.GrantInput) { in.Grantee.Agent = "other-agent" })
	e.grant("DEL", nil)
	_, runID := e.runAs(e.item(workHost, "t"), agent, agent)
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { secret, err = runidentity.Issue(tx, runID, e.now()); return }); err != nil {
		t.Fatal(err)
	}
	rec := e.http("GET", "/v1/grants", "", bearer(secret))
	var out struct {
		Grants []policy.Grant `json:"grants"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil || len(out.Grants) != 1 || out.Grants[0].ID != bgGrant.ID {
		t.Fatalf("run sees only its Agent's background grants: %d %s", rec.Code, rec.Body)
	}
}

// T12：后台运行凭单忽略的建议，本人在 Web 撤销后回到 proposed。
func TestOwnerRevertsAGrantedBackgroundDismissal(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	p := e.proposalFrom("ev-trusted")
	run, _ := e.runAs(p, agent, agent)
	e.cmd(run, "proposal.dismiss", map[string]any{"id": p}, withGrant(g.ID))
	var seq int64
	for _, x := range e.events4(p) {
		if x.Grant == g.ID {
			seq = x.Seq
		}
	}
	if e.get(p).Phase != domain.PhaseDropped || seq == 0 {
		t.Fatalf("dismissal must be recorded under the grant: %+v", e.get(p))
	}
	e.cmd(owner, "item.revert", map[string]any{"id": p, "event": seq})
	if ph := e.get(p).Phase; ph != domain.PhaseProposed {
		t.Fatalf("revert must restore the proposal, got %s", ph)
	}
}

func revise(e *env, id string, f func(*domain.Item)) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, _, _, err := items.Revise(tx, id, "test", e.now(), func(x *domain.Item) error { f(x); return nil })
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// 授权单按被授权方生效（§14.5，本人 2026-11-20 裁决）：签给 hermes 的单，hermes 的 delegate 在任何调用通道都能用——
// 不带 --grant、不报会话类型（命令行直连），或在多人会话里带原话，内核都会找到这张单放行并在 Event 记 grant id。
// 被授权方仍按内核记录的 harness 匹配：另一把不属于 hermes 的 delegate 照旧 23；撤销后 hermes 也是 23。
func TestGrantFollowsGranteeAcrossChannels(t *testing.T) {
	e := agentEnv(t)
	g := e.agentGrant(nil)
	gotGrant := func(id string) string {
		evs := e.events4(id)
		for i := len(evs) - 1; i >= 0; i-- {
			if evs[i].Type == domain.EvItemUpdated {
				return evs[i].Grant
			}
		}
		return ""
	}
	cli := e.item(otherHost, "cli")
	e.cmd(e.asKind("HDEL", "", ""), "item.update", map[string]any{"id": cli, "summary": "by grant"})
	if gotGrant(cli) != g.ID {
		t.Fatalf("cli write must record grant %s, got %q", g.ID, gotGrant(cli))
	}
	group := e.item(otherHost, "group")
	e.cmd(e.asKind("HDEL", "s", "group"), "item.update", map[string]any{"id": group, "summary": "by grant"}, func(r *command.Request) { r.Evidence = "owner: yes" })
	if it := e.get(group); it.Summary != "by grant" || gotGrant(group) != g.ID {
		t.Fatalf("group session with a grant: %+v grant=%q", it, gotGrant(group))
	}
	// 另一个 Agent 的 delegate：A 的授权不给 B 用。
	e.issue("ODEL", auth.Spec{Kind: auth.Delegate, Harness: "other-agent"})
	other := e.item(otherHost, "other")
	e.wantExit(domain.ExitBasisRequired, e.asKind("ODEL", "", ""), "item.update", map[string]any{"id": other, "summary": "x"})
	// 单上没有的动作照旧拒绝。
	e.wantExit(domain.ExitBasisRequired, e.asKind("HDEL", "", ""), "item.transition", map[string]any{"id": other, "to": "done", "reason": "x"})
	// 撤销后同一场景被拒。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.RevokeGrant(tx, owner.Actor, e.now(), g.ID, "test")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	after := e.item(otherHost, "after")
	e.wantExit(domain.ExitBasisRequired, e.asKind("HDEL", "", ""), "item.update", map[string]any{"id": after, "summary": "x"})
	if e.get(after).Summary != "" {
		t.Fatal("a rejected write changed the item")
	}
}
