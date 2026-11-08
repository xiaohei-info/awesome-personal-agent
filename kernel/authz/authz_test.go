package authz

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

const web = "https://apa.local/web/items/i1"

func pol() policy.Policy {
	p := policy.Default()
	p.WebOnly.Rules = []policy.WebOnlyRule{{Kind: "payment", Category: policy.CategoryFunds}, {Tag: "contract", Project: "p1", Category: policy.CategoryExternalCommitment}}
	p.ChannelAnswerers = map[string][]string{"chat-a": {"alice"}}
	p.ForwardAllowlist = map[string][]string{"mail": {"boss"}}
	return p
}

func req(l Level, a Action, o Object) Request {
	actor := domain.Actor{Type: domain.ActorAgent, Principal: "me", Via: domain.ViaCLI}
	if l == LevelWeb {
		actor = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	}
	if l == LevelDelegate {
		actor.OnBehalfOf, actor.Session = "owner", "s1"
	}
	return Request{Actor: actor, Level: l, Action: a, Object: o, WebURL: web}
}

// §14.3 全表：行 × 级别 → 期望退出码（0 = 允许）。
func TestMatrix(t *testing.T) {
	own := Object{Assignee: "me"}
	other := Object{Assignee: "someone"}
	scoped := Object{Assignee: "someone", InRunScope: true, InRunProject: true}
	type row struct {
		name string
		a    Action
		o    Object
		want map[Level]int
	}
	all := func(obs, wk, del, run, w int) map[Level]int {
		return map[Level]int{LevelObserver: obs, LevelWorker: wk, LevelDelegate: del, LevelRun: run, LevelWeb: w}
	}
	rows := []row{
		{"read", ActionRead, other, all(0, 0, 0, 0, 0)},
		{"propose", ActionCreateProposed, other, all(0, 0, 0, 0, 0)},
		{"update own", ActionUpdateOwn, own, all(4, 0, 0, 4, 0)},
		{"update own in scope", ActionUpdateOwn, scoped, all(4, 4, 0, 0, 0)},
		{"revise material", ActionReviseMaterial, other, all(4, 0, 0, 0, 0)},
		{"delegate own", ActionDelegate, own, all(4, 0, 0, 4, 0)},
		{"delegate other", ActionDelegate, other, all(4, 4, 0, 4, 0)},
		{"delegate in scope", ActionDelegate, scoped, all(4, 4, 0, 0, 0)},
		{"repo checkout", ActionEditCheckout, other, all(4, 0, 0, 4, 0)},
		{"repo checkout project", ActionEditCheckout, scoped, all(4, 0, 0, 0, 0)},
		{"plugin decl", ActionSubmitPlugin, other, all(4, 0, 0, 4, 0)},
		{"edit any", ActionEditAnyItem, other, all(4, 4, 0, 4, 0)},
		{"answer decision", ActionAnswerDecision, other, all(4, 4, 0, 4, 0)},
		{"project mgmt", ActionManageProject, other, all(4, 4, 0, 4, 0)},
		{"sources mgmt", ActionManageSource, other, all(4, 4, 0, 4, 0)},
		{"settings", ActionChangeSettings, other, all(4, 4, 20, 4, 0)},
	}
	for _, r := range rows {
		for l, want := range r.want {
			rq := req(l, r.a, r.o)
			rq.Evidence = "owner: ok (session s1)"
			d := Decide(pol(), rq)
			if d.ExitCode() != want {
				t.Errorf("%s/%s: exit %d want %d (%+v)", r.name, l, d.ExitCode(), want, d.Err)
			}
			if (want == 0) != (d.Verdict == Allow) {
				t.Errorf("%s/%s: verdict %v for exit %d", r.name, l, d.Verdict, want)
			}
		}
	}
}

// §14.5：第二档要本人原话加会话引用，缺一不可，缺失为 23；第一档不要。
func TestDelegateTiers(t *testing.T) {
	decide := []Action{ActionEditAnyItem, ActionAnswerDecision}
	for _, a := range decide { // 动作本身属于第二档，命令登记成第一档也不能绕过
		for _, tier := range []Tier{TierNone, TierWork, TierDecide, TierWords} {
			r := req(LevelDelegate, a, Object{})
			r.Tier = tier
			if d := Decide(pol(), r); d.ExitCode() != domain.ExitBasisRequired {
				t.Errorf("%s tier %d without evidence: %+v", a, tier, d)
			}
		}
	}
	for _, a := range append(decide, ActionManageProject, ActionManageSource, ActionUpdateOwn) {
		r := req(LevelDelegate, a, Object{Assignee: "me"})
		r.Tier = TierDecide
		if a == ActionManageProject || a == ActionManageSource {
			r.Tier = TierWords
		}
		if d := Decide(pol(), r); d.ExitCode() != domain.ExitBasisRequired || d.Err.WebURL == nil {
			t.Errorf("%s without evidence: %+v", a, d)
		}
		r.Evidence = "  "
		if d := Decide(pol(), r); d.ExitCode() != domain.ExitBasisRequired {
			t.Errorf("%s with blank evidence: %+v", a, d)
		}
		r.Evidence = "quote"
		r.Actor.Session = ""
		if d := Decide(pol(), r); d.ExitCode() != domain.ExitBasisRequired {
			t.Errorf("%s evidence without session: %+v", a, d)
		}
		r.Actor.Session = "s1"
		if d := Decide(pol(), r); d.Verdict != Allow {
			t.Errorf("%s with evidence and session: %+v", a, d)
		}
		r.Actor.Type, r.Actor.Session = domain.ActorSystem, "" // 规则与内核服务没有 harness 会话，依据是自己带的证据
		if d := Decide(pol(), r); d.Verdict != Allow {
			t.Errorf("%s by a system actor with evidence: %+v", a, d)
		}
	}
	// 第一档：agent 自己的工作不要原话；不论带不带都放行。
	for _, tier := range []Tier{TierNone, TierWork} {
		r := req(LevelDelegate, ActionUpdateOwn, Object{Assignee: "me"})
		r.Tier = tier
		if d := Decide(pol(), r); d.Verdict != Allow {
			t.Errorf("tier %d own work: %+v", tier, d)
		}
	}
	// 其他凭据与 Web 不经三档；运行凭据带证据也没有代答能力。
	for _, l := range []Level{LevelWeb, LevelRun, LevelWorker} {
		r := req(l, ActionUpdateOwn, Object{Assignee: "me", InRunScope: true})
		r.Tier = TierDecide
		if d := Decide(pol(), r); d.Verdict != Allow {
			t.Errorf("%s ignores tiers: %+v", l, d)
		}
	}
}

func TestWebOnlyAnswer(t *testing.T) {
	cases := []struct {
		name string
		o    Object
		l    Level
		want int
	}{
		{"kind rule delegate", Object{Kind: "payment"}, LevelDelegate, 20},
		{"kind rule web", Object{Kind: "payment"}, LevelWeb, 0},
		{"kind rule run", Object{Kind: "payment"}, LevelRun, 4},
		{"kind rule worker", Object{Kind: "payment"}, LevelWorker, 4},
		{"tag+project both", Object{Tags: []string{"contract"}, Project: "p1"}, LevelDelegate, 20},
		{"tag only does not match rule needing project", Object{Tags: []string{"contract"}, Project: "p2"}, LevelDelegate, 0},
		{"default category", Object{Category: policy.CategoryIrreversible}, LevelDelegate, 20},
		{"unknown category", Object{Category: "weather"}, LevelDelegate, 0},
		{"screened up", Object{ScreenedWebOnly: true}, LevelDelegate, 20},
		{"screening pending delegate", Object{ScreeningPending: true}, LevelDelegate, 22},
		{"screening pending run", Object{ScreeningPending: true}, LevelRun, 4},
		{"screening pending web", Object{ScreeningPending: true}, LevelWeb, 0},
		{"rule wins over pending", Object{Kind: "payment", ScreeningPending: true}, LevelDelegate, 20},
		{"plain delegate", Object{Kind: "chore"}, LevelDelegate, 0},
	}
	for _, c := range cases {
		r := req(c.l, ActionAnswerDecision, c.o)
		r.Evidence = "owner said so"
		d := Decide(pol(), r)
		if d.ExitCode() != c.want {
			t.Errorf("%s: exit %d want %d", c.name, d.ExitCode(), c.want)
		}
		switch c.want {
		case 20:
			if d.Verdict != NeedWeb || d.Err.WebURL == nil || *d.Err.WebURL != web {
				t.Errorf("%s: want NeedWeb with link, got %+v", c.name, d)
			}
		case 22:
			if d.Verdict != Deny || d.Err.WebURL == nil {
				t.Errorf("%s: want Deny 22 with link, got %+v", c.name, d)
			}
		}
	}
}

// 判定只看账本里的对象属性与策略，与事项创建者无关：无字段可让创建者降低门槛。
func TestWebOnlyIndependentOfCreator(t *testing.T) {
	o := Object{Kind: "payment"}
	for _, creator := range []domain.Actor{
		{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb},
		{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI},
	} {
		o.Creator = creator
		r := req(LevelDelegate, ActionAnswerDecision, o)
		r.Evidence = "e"
		if d := Decide(pol(), r); d.Verdict != NeedWeb {
			t.Errorf("creator %+v changed verdict: %+v", creator, d)
		}
	}
}

func TestExecutorAllowed(t *testing.T) {
	proj := &domain.Project{ID: "p1", AllowedExecutors: []string{"host-a"}}
	empty := &domain.Project{ID: "p2", AllowedExecutors: []string{}}
	open := &domain.Project{ID: "p3"}
	cases := []struct {
		name string
		l    Level
		p    *domain.Project
		exec string
		want int
	}{
		{"in list", LevelDelegate, proj, "host-a", 0},
		{"not in list", LevelDelegate, proj, "host-b", 21},
		{"not in list web", LevelWeb, proj, "host-b", 21},
		{"owner always", LevelDelegate, proj, "owner", 0},
		{"empty list owner only", LevelDelegate, empty, "host-a", 21},
		{"empty list owner", LevelWeb, empty, "owner", 0},
		{"unrestricted", LevelDelegate, open, "anyone", 0},
		{"no project", LevelDelegate, nil, "anyone", 0},
	}
	for _, c := range cases {
		r := req(c.l, ActionDelegate, Object{Assignee: "me", Project: "x", ProjectInfo: c.p, Executor: c.exec})
		r.Owner = "owner"
		d := Decide(pol(), r)
		if d.ExitCode() != c.want {
			t.Errorf("%s: exit %d want %d", c.name, d.ExitCode(), c.want)
		}
		if c.want == 21 && (len(d.Err.Allowed) != len(c.p.AllowedExecutors) || d.Err.Code != domain.CodeExecutorNotAllowed) {
			t.Errorf("%s: allowed list not reported: %+v", c.name, d.Err)
		}
	}
	// 权限不足优先于执行者校验：observer 不能 delegate，退出码仍为 4。
	r := req(LevelObserver, ActionDelegate, Object{ProjectInfo: proj, Executor: "host-b"})
	if d := Decide(pol(), r); d.ExitCode() != 4 {
		t.Errorf("observer: %+v", d)
	}
}

func TestExecutorCheckPoints(t *testing.T) {
	proj := &domain.Project{ID: "p1", AllowedExecutors: []string{"host-a"}}
	for _, a := range []Action{ActionReassign, ActionMoveIntoProject, ActionRuleCreateChild, ActionRunnerTask} {
		r := req(LevelWeb, a, Object{ProjectInfo: proj, Executor: "host-b"})
		r.Owner = "owner"
		if d := Decide(pol(), r); d.ExitCode() != 21 {
			t.Errorf("%s: %+v", a, d)
		}
		r.Object.Executor = "host-a"
		if d := Decide(pol(), r); d.Verdict != Allow {
			t.Errorf("%s allowed: %+v", a, d)
		}
	}
}

func TestChannelAnswer(t *testing.T) {
	mk := func(sender, ch string, o Object) Request {
		r := req(LevelObserver, ActionChannelAnswer, o)
		r.Channel, r.Sender = ch, sender
		return r
	}
	cases := []struct {
		name string
		r    Request
		want int
	}{
		{"listed", mk("alice", "chat-a", Object{}), 0},
		{"other sender", mk("mallory", "chat-a", Object{}), 4},
		{"missing sender", mk("", "chat-a", Object{}), 4},
		{"unlisted channel", mk("alice", "chat-b", Object{}), 4},
		{"web_only", mk("alice", "chat-a", Object{Kind: "payment"}), 20},
		{"screening", mk("alice", "chat-a", Object{ScreeningPending: true}), 22},
	}
	for _, c := range cases {
		if d := Decide(pol(), c.r); d.ExitCode() != c.want {
			t.Errorf("%s: %+v", c.name, d)
		}
	}
}

func TestForwardAllowed(t *testing.T) {
	p := pol()
	if !ForwardAllowed(p, "mail", "boss", true) {
		t.Error("listed verified sender must pass")
	}
	for _, c := range []struct {
		src, sender string
		verified    bool
	}{{"mail", "boss", false}, {"mail", "", true}, {"mail", "x", true}, {"other", "boss", true}} {
		if ForwardAllowed(p, c.src, c.sender, c.verified) {
			t.Errorf("%+v must not pass", c)
		}
	}
}

func TestIdentityGuards(t *testing.T) {
	// Level=web 必须真是 Web 上的本人。
	r := req(LevelWeb, ActionChangeSettings, Object{})
	r.Actor = domain.Actor{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI}
	if d := Decide(pol(), r); d.ExitCode() != 4 {
		t.Errorf("spoofed web: %+v", d)
	}
	if d := Decide(pol(), req(Level("root"), ActionRead, Object{})); d.ExitCode() != 4 {
		t.Errorf("unknown level: %+v", d)
	}
	if d := Decide(pol(), req(LevelWeb, Action("nope"), Object{})); d.Err.Code != domain.CodeInvalidRequest {
		t.Errorf("unknown action: %+v", d)
	}
}

func TestRejectionEvent(t *testing.T) {
	r := req(LevelDelegate, ActionAnswerDecision, Object{ID: "d1", Kind: "payment"})
	r.Evidence = "e"
	d := Decide(pol(), r)
	ev, ok := RejectionEvent(r, d)
	if !ok || ev.Type != domain.EvCommandRejected || ev.Object != "d1" || ev.Actor.Principal != "me" || len(ev.Payload) == 0 {
		t.Fatalf("bad event: %+v ok=%v", ev, ok)
	}
	// 第二档依据不足（退出码 23）要留痕。
	r = req(LevelDelegate, ActionAnswerDecision, Object{ID: "d2"})
	if ev, ok := RejectionEvent(r, Decide(pol(), r)); !ok || ev.Object != "d2" {
		t.Errorf("basis rejection must be recorded: %+v ok=%v", ev, ok)
	}
	// 运行身份被拒绝的决策命令也要记。
	r = req(LevelRun, ActionAnswerDecision, Object{ID: "d1"})
	if _, ok := RejectionEvent(r, Decide(pol(), r)); !ok {
		t.Error("run-denied decision command must be recorded")
	}
	// 普通 4 与允许不记。
	r = req(LevelObserver, ActionEditAnyItem, Object{ID: "i"})
	if _, ok := RejectionEvent(r, Decide(pol(), r)); ok {
		t.Error("plain 4 must not be recorded")
	}
	r = req(LevelWeb, ActionRead, Object{})
	if _, ok := RejectionEvent(r, Decide(pol(), r)); ok {
		t.Error("allow must not be recorded")
	}
}
