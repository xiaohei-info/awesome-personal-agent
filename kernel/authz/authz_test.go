package authz

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

var (
	agent  = domain.Actor{Type: domain.ActorAgent, Principal: "cred", Via: domain.ViaCLI}
	web    = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	system = domain.Actor{Type: domain.ActorSystem, Principal: "system:rule", Via: domain.ViaRule}
	now    = time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	// open 是十组全部开放（物理删除开到明天）的卡。
	open = func() policy.Card {
		c := policy.DefaultCard().With(policy.Groups...)
		until := now.Add(24 * time.Hour)
		c.Groups[policy.GroupPurge] = policy.GroupGrant{Until: &until}
		return c
	}()
)

func req(l Level, a domain.Actor, act Action, g Group, c policy.Card) Request {
	return Request{Actor: a, Level: l, Action: act, Group: g, Card: c, Now: now, Agent: "agent_a", Page: "/settings/x", WebURL: "/items/i"}
}

func code(d Decision) domain.Code {
	if d.Err == nil {
		return ""
	}
	return d.Err.Code
}

// 只有本人的事不看权限卡：全部开放的 Agent、运行凭据、内核自己做都是 20（hard_line），链接落在设置页；
// 内核自己也做不了后果较大的组；Web 本人没再输口令是 reauth_required，输过才放行。
func TestOwnerOnlyIgnoresTheCard(t *testing.T) {
	p := policy.Default()
	for _, r := range []Request{
		req(LevelDelegate, agent, ActionChangeSettings, GroupOwner, open),
		req(LevelRun, agent, ActionChangeSettings, GroupOwner, open),
		req(LevelDelegate, system, ActionChangeSettings, GroupOwner, open),
		req(LevelDelegate, system, ActionChangeSettings, GroupPurge, open),
		req(LevelDelegate, agent, ActionChangeSettings, GroupInternal, open),
	} {
		d := Decide(p, r)
		if code(d) != domain.CodeWebOnly || d.Err.Reason != domain.ReasonHardLine || d.ExitCode() != domain.ExitWebOnly {
			t.Fatalf("%v %v %s: %+v", r.Level, r.Actor.Type, r.Group, d.Err)
		}
	}
	for _, g := range []Group{GroupOwner, GroupConnect, GroupPurge} {
		w := req(LevelWeb, web, ActionChangeSettings, g, policy.Card{})
		if d := Decide(p, w); code(d) != domain.CodeReauthRequired || d.ExitCode() != domain.ExitWebOnly {
			t.Fatalf("web without reauth on %s: %+v", g, d.Err)
		}
		w.Fresh = true
		if d := Decide(p, w); d.Err != nil {
			t.Fatalf("web with reauth on %s: %+v", g, d.Err)
		}
	}
	if d := Decide(p, req(LevelWeb, web, ActionChangeSettings, GroupRules, policy.Card{})); d.Err != nil {
		t.Fatalf("everyday settings need no reauth: %+v", d.Err)
	}
}

// 钥匙只决定范围：范围外是 4（out_of_scope），与档位无关。
func TestScopeIsTheKey(t *testing.T) {
	p := policy.Default()
	cases := []struct {
		r    Request
		want domain.Code
	}{
		{req(LevelObserver, agent, ActionUpdateOwn, GroupOrganize, open), domain.CodePermissionDenied},
		{req(LevelObserver, agent, ActionCreateProposed, GroupOrganize, open), ""},
		{req(LevelWorker, agent, ActionUpdateOwn, GroupOrganize, open), domain.CodePermissionDenied}, // 不是自己的事项
		{req(LevelRun, agent, ActionEditAnyItem, GroupOrganize, open), ""},                           // 运行凭据与 delegate 钥匙范围相同
		{req(LevelRun, agent, ActionChangeSettings, GroupRules, open), ""},
		{req(LevelDelegate, agent, ActionWebRead, GroupView, open), domain.CodePermissionDenied},
		{req(LevelDelegate, agent, ActionEditAnyItem, GroupOrganize, policy.DefaultCard()), ""},
		// 无效级别一律范围外：未知级别、空级别、非本人在 Web 之外自称 web。
		{req(Level("root"), agent, ActionRead, GroupView, open), domain.CodePermissionDenied},
		{req("", agent, ActionRead, GroupView, open), domain.CodePermissionDenied},
		{req(LevelWeb, agent, ActionRead, GroupView, open), domain.CodePermissionDenied},
	}
	for i, c := range cases {
		d := Decide(p, c.r)
		if code(d) != c.want || (c.want != "" && d.Err.Reason != domain.ReasonOutOfScope) {
			t.Fatalf("case %d: %+v", i, d.Err)
		}
	}
	own := req(LevelWorker, agent, ActionUpdateOwn, GroupOrganize, policy.DefaultCard())
	own.Object.Assignee = "cred"
	if d := Decide(p, own); d.Err != nil {
		t.Fatalf("in scope: %+v", d.Err)
	}
}

// 权限卡：十组各自开与关；没开为 23（tier_closed），带组名、权限卡页面与本人直接做的链接；
// 开放到期即关；"替我拍板"的项目限定含要移入的项目；Web 本人与内核自己不看权限卡。
func TestCardOpensEachGroup(t *testing.T) {
	p := policy.Default()
	for _, g := range policy.Groups {
		closed := req(LevelDelegate, agent, ActionChangeSettings, Group(g), policy.Card{})
		d := Decide(p, closed)
		if code(d) != domain.CodeTierClosed || d.Err.Reason != domain.ReasonTierClosed || d.ExitCode() != domain.ExitTierClosed ||
			d.Err.Group != g || d.Err.SettingsURL == nil || *d.Err.SettingsURL != "/settings/agents/agent_a#permissions" || *d.Err.WebURL != map[bool]string{true: "/items/i", false: "/settings/x"}[g == policy.GroupDecide] {
			t.Fatalf("%s closed: %+v", g, d.Err)
		}
		if d := Decide(p, req(LevelDelegate, agent, ActionChangeSettings, Group(g), open)); d.Err != nil {
			t.Fatalf("%s open: %+v", g, d.Err)
		}
		if d := Decide(p, req(LevelRun, agent, ActionChangeSettings, Group(g), open)); d.Err != nil {
			t.Fatalf("%s open for a run of the same Agent: %+v", g, d.Err)
		}
	}
	late := req(LevelDelegate, agent, ActionChangeSettings, GroupPurge, open)
	late.Now = now.Add(25 * time.Hour)
	if code(Decide(p, late)) != domain.CodeTierClosed {
		t.Fatal("an expired grant must be closed")
	}
	decide := open
	decide.DecideProjects = []string{"p1"}
	in := func(r Request, project string) Request { r.Object.Project = project; return r }
	if d := Decide(p, in(req(LevelDelegate, agent, ActionAnswerDecision, GroupDecide, decide), "p1")); d.Err != nil {
		t.Fatalf("decide in a listed project: %+v", d.Err)
	}
	for _, r := range []Request{
		in(req(LevelDelegate, agent, ActionAnswerDecision, GroupDecide, decide), "p2"),
		req(LevelDelegate, agent, ActionAnswerDecision, GroupDecide, decide), // 限了项目，没有项目的对象不在范围
	} {
		if code(Decide(p, r)) != domain.CodeTierClosed {
			t.Fatalf("outside the project limit: %+v", r.Object)
		}
	}
	move := in(req(LevelDelegate, agent, ActionAnswerDecision, GroupDecide, decide), "p1")
	to := "p2"
	move.Object.ToProject = &to
	if code(Decide(p, move)) != domain.CodeTierClosed {
		t.Fatal("moving into a project outside the limit must be refused")
	}
	if d := Decide(p, req(LevelWeb, web, ActionAnswerDecision, GroupDecide, policy.Card{})); d.Err != nil {
		t.Fatalf("web: %+v", d.Err)
	}
	if d := Decide(p, req(LevelDelegate, system, ActionChangeSettings, GroupRules, policy.Card{})); d.Err != nil {
		t.Fatalf("system: %+v", d.Err)
	}
	if d := Decide(p, Request{Actor: agent, Level: LevelDelegate, Action: ActionRead}); code(d) != domain.CodeInvalidRequest {
		t.Fatalf("a request without a group is a kernel bug: %+v", d.Err)
	}
}

// 渠道回答：名单内即本人，不看档位；名单为空等于这个渠道不能回答。
func TestChannelAnswerers(t *testing.T) {
	p := policy.Default()
	r := Request{Actor: system, Level: LevelDelegate, Action: ActionChannelAnswer, Group: GroupDecide, Channel: "tg", Sender: "u1"}
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
	r := req(LevelWeb, web, ActionReassign, GroupOrganize, policy.Card{})
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
		req(LevelDelegate, agent, ActionChangeSettings, GroupOwner, open),
		req(LevelDelegate, agent, ActionAnswerDecision, GroupDecide, policy.Card{}),
		req(LevelObserver, agent, ActionUpdateOwn, GroupOrganize, open),
	} {
		if _, ok := RejectionEvent(r, Decide(p, r)); !ok {
			t.Fatalf("%+v must be recorded", r)
		}
	}
	w := req(LevelWeb, web, ActionChangeSettings, GroupOwner, policy.Card{})
	if _, ok := RejectionEvent(w, Decide(p, w)); ok {
		t.Fatal("reauth_required is not a rejection")
	}
}
