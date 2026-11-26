package authz

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

var (
	agent  = domain.Actor{Type: domain.ActorAgent, Principal: "cred", Via: domain.ViaCLI}
	web    = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	system = domain.Actor{Type: domain.ActorSystem, Principal: "system:rule", Via: domain.ViaRule}
	open   = policy.AgentAllowance{Tier: policy.AllowRules}
)

func req(l Level, a domain.Actor, act Action, t Tier, al policy.AgentAllowance) Request {
	return Request{Actor: a, Level: l, Action: act, Tier: t, Allowance: al, Page: "/settings/x", WebURL: "/items/i"}
}

func code(d Decision) domain.Code {
	if d.Err == nil {
		return ""
	}
	return d.Err.Code
}

// 硬线先于档位且不读档位：全部放开的 Agent、内核自己做 3b 都是 20（hard_line），链接落在设置页；
// Web 本人没再输口令是 reauth_required，输过才放行。
func TestHardLineIgnoresAllowance(t *testing.T) {
	p := policy.Default()
	for _, r := range []Request{
		req(LevelDelegate, agent, ActionChangeSettings, TierPower, open),
		req(LevelRun, agent, ActionChangeSettings, TierPower, open),
		req(LevelDelegate, system, ActionChangeSettings, TierPower, open),
	} {
		d := Decide(p, r)
		if code(d) != domain.CodeWebOnly || d.Err.Reason != domain.ReasonHardLine || *d.Err.WebURL != "/settings/x" || d.ExitCode() != domain.ExitWebOnly {
			t.Fatalf("%v %v: %+v", r.Level, r.Actor.Type, d.Err)
		}
	}
	w := req(LevelWeb, web, ActionChangeSettings, TierPower, policy.AgentAllowance{})
	if d := Decide(p, w); code(d) != domain.CodeReauthRequired || d.ExitCode() != domain.ExitWebOnly {
		t.Fatalf("web without reauth: %+v", d.Err)
	}
	w.Fresh = true
	if d := Decide(p, w); d.Err != nil {
		t.Fatalf("web with reauth: %+v", d.Err)
	}
}

// 钥匙只决定范围：范围外是 4（out_of_scope），与档位无关。
func TestScopeIsTheKey(t *testing.T) {
	p := policy.Default()
	cases := []struct {
		r    Request
		want domain.Code
	}{
		{req(LevelObserver, agent, ActionUpdateOwn, TierOrganize, open), domain.CodePermissionDenied},
		{req(LevelObserver, agent, ActionCreateProposed, TierOrganize, open), ""},
		{req(LevelWorker, agent, ActionUpdateOwn, TierOrganize, open), domain.CodePermissionDenied}, // 不是自己的事项
		{req(LevelRun, agent, ActionEditAnyItem, TierOrganize, open), ""},                           // 运行凭据与 delegate 钥匙范围相同
		{req(LevelRun, agent, ActionChangeSettings, TierRules, open), ""},
		{req(LevelDelegate, agent, ActionWebRead, TierRead, open), domain.CodePermissionDenied},
		{req(LevelDelegate, agent, ActionEditAnyItem, TierOrganize, policy.AgentAllowance{}), ""},
		// 无效级别一律范围外：未知级别、空级别、非本人在 Web 之外自称 web。
		{req(Level("root"), agent, ActionRead, TierRead, open), domain.CodePermissionDenied},
		{req("", agent, ActionRead, TierRead, open), domain.CodePermissionDenied},
		{req(LevelWeb, agent, ActionRead, TierRead, open), domain.CodePermissionDenied},
	}
	for i, c := range cases {
		d := Decide(p, c.r)
		if code(d) != c.want || (c.want != "" && d.Err.Reason != domain.ReasonOutOfScope) {
			t.Fatalf("case %d: %+v", i, d.Err)
		}
	}
	own := req(LevelWorker, agent, ActionUpdateOwn, TierOrganize, policy.AgentAllowance{})
	own.Object.Assignee = "cred"
	if d := Decide(p, own); d.Err != nil {
		t.Fatalf("in scope: %+v", d.Err)
	}
}

// 档位：一档永远开；二档要 decide 以上且项目在范围（含要移入的项目）；3a 要 rules；不放开是 23（tier_closed）。
// Web 本人与内核自己不看档位。
func TestAllowanceOpensTiers(t *testing.T) {
	p := policy.Default()
	none := policy.AgentAllowance{Tier: policy.AllowOrganize}
	decide := policy.AgentAllowance{Tier: policy.AllowDecide, Projects: []string{"p1"}}
	in := func(r Request, project string) Request { r.Object.Project = project; return r }
	cases := []struct {
		r    Request
		want domain.Code
	}{
		{req(LevelDelegate, agent, ActionEditAnyItem, TierOrganize, none), ""},
		{req(LevelDelegate, agent, ActionAnswerDecision, TierDecide, none), domain.CodeTierClosed},
		{in(req(LevelDelegate, agent, ActionAnswerDecision, TierDecide, decide), "p1"), ""},
		{in(req(LevelDelegate, agent, ActionAnswerDecision, TierDecide, decide), "p2"), domain.CodeTierClosed},
		{req(LevelDelegate, agent, ActionAnswerDecision, TierDecide, decide), domain.CodeTierClosed}, // 限了项目，没有项目的对象不在范围
		{in(req(LevelDelegate, agent, ActionChangeSettings, TierRules, decide), "p1"), domain.CodeTierClosed},
		{req(LevelDelegate, agent, ActionChangeSettings, TierRules, open), ""},
		{req(LevelRun, agent, ActionAnswerDecision, TierDecide, none), domain.CodeTierClosed}, // 运行凭据随所属 Agent
		{req(LevelWeb, web, ActionAnswerDecision, TierDecide, none), ""},
		{req(LevelDelegate, system, ActionChangeSettings, TierRules, none), ""},
	}
	for i, c := range cases {
		d := Decide(p, c.r)
		if code(d) != c.want {
			t.Fatalf("case %d: got %+v want %s", i, d.Err, c.want)
		}
		if c.want == domain.CodeTierClosed && (d.Err.Reason != domain.ReasonTierClosed || d.ExitCode() != domain.ExitTierClosed) {
			t.Fatalf("case %d: %+v", i, d.Err)
		}
	}
	move := in(req(LevelDelegate, agent, ActionAnswerDecision, TierDecide, decide), "p1")
	to := "p2"
	move.Object.ToProject = &to
	if code(Decide(p, move)) != domain.CodeTierClosed {
		t.Fatal("moving into a project outside the limit must be refused")
	}
	if d := Decide(p, Request{Actor: agent, Level: LevelDelegate, Action: ActionRead}); code(d) != domain.CodeInvalidRequest {
		t.Fatalf("a request without a tier is a kernel bug: %+v", d.Err)
	}
}

// 渠道回答：名单内即本人，不看档位；名单为空等于这个渠道不能回答。
func TestChannelAnswerers(t *testing.T) {
	p := policy.Default()
	r := Request{Actor: system, Level: LevelDelegate, Action: ActionChannelAnswer, Tier: TierDecide, Channel: "tg", Sender: "u1"}
	if code(Decide(p, r)) != domain.CodePermissionDenied {
		t.Fatal("empty answerer list must refuse")
	}
	p.ChannelAnswerers = map[string][]string{"tg": {"u1"}}
	if d := Decide(p, r); d.Err != nil {
		t.Fatalf("listed answerer: %+v", d.Err)
	}
	r.Sender = "u2"
	if code(Decide(p, r)) != domain.CodePermissionDenied {
		t.Fatal("unlisted answerer must refuse")
	}
}

// 项目允许的执行者只按 id：旧的按机器、harness 匹配规则不再放行。
func TestExecutorListByIDOnly(t *testing.T) {
	pr := &domain.Project{AllowedExecutors: []string{"a"}, AllowedExecutorMatch: []domain.ExecutorMatch{{Harness: "h"}}}
	r := req(LevelWeb, web, ActionReassign, TierOrganize, policy.AgentAllowance{})
	r.Object.ProjectInfo, r.Object.Executor = pr, "b"
	if d := Decide(policy.Default(), r); code(d) != domain.CodeExecutorNotAllowed {
		t.Fatalf("match rules must not allow: %+v", d.Err)
	}
	r.Object.Executor = "a"
	if d := Decide(policy.Default(), r); d.Err != nil {
		t.Fatalf("listed id: %+v", d.Err)
	}
}

// 三种口径与执行者拒绝都留痕；再输口令是本人还没完成的操作，不留痕。
func TestRejectionEventCoversTheThreeReasons(t *testing.T) {
	p := policy.Default()
	for _, r := range []Request{
		req(LevelDelegate, agent, ActionChangeSettings, TierPower, open),
		req(LevelDelegate, agent, ActionAnswerDecision, TierDecide, policy.AgentAllowance{}),
		req(LevelObserver, agent, ActionUpdateOwn, TierOrganize, open),
	} {
		if _, ok := RejectionEvent(r, Decide(p, r)); !ok {
			t.Fatalf("%+v must be recorded", r)
		}
	}
	w := req(LevelWeb, web, ActionChangeSettings, TierPower, policy.AgentAllowance{})
	if _, ok := RejectionEvent(w, Decide(p, w)); ok {
		t.Fatal("reauth_required is not a rejection")
	}
}
