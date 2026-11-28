package attention

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 等价性锁：没保存过矩阵时，按旧键换算后选出的路由与旧版本五处各算各的结果逐一相同 ----
// 下面 legacy* 是旧版本代码的原样摘录（attention.route、attention.taskRoute、health.pushRoute 与 runPush、health.route、
// schedule 的 MainRoute），只用来对照，改它们等于改等价性的定义。

func legacyRoute(pol policy.Policy, class domain.DeliveryClass, prio domain.Priority) string {
	r := pol.Disturb.Routes[string(class)+":"+string(prio)]
	if r == "" {
		r = pol.Disturb.Routes[string(class)]
	}
	switch {
	case r == "":
		return delivery.RouteWeb
	case r == delivery.RouteInject || r == delivery.RouteWake || r == delivery.RouteWeb || strings.HasPrefix(r, "channel:"):
		return r
	}
	return delivery.ChannelRoute(r)
}

func legacyPushRoute(pol policy.Policy, r policy.PushRule) string {
	if r.Route != "" {
		return r.Route
	}
	if d := pol.Disturb.Routes[string(domain.DeliveryDecision)]; d != "" {
		return d
	}
	return delivery.RouteWeb
}

func legacyHealthRoute(pol policy.Policy) string {
	r := pol.Disturb.Routes[string(domain.DeliveryHealth)]
	switch {
	case r == "":
		return delivery.RouteWeb
	case r == delivery.RouteInject || r == delivery.RouteWake || r == delivery.RouteWeb || strings.HasPrefix(r, "channel:"):
		return r
	}
	return delivery.ChannelRoute(r)
}

func legacyPushToMain(tx *store.Tx, pol policy.Policy, it domain.Item) (string, bool, error) {
	id, pack, ok, err := instances.TaskOf(tx, it)
	if err != nil || !ok {
		return pack, false, err
	}
	return pack, pol.Disturb.ProactiveSettings().PushTask(pack, id), nil
}

// legacyTask 是旧版本定时任务产出的最终路由；""表示不出投递。
func legacyTask(tx *store.Tx, pol policy.Policy, it domain.Item) (string, error) {
	_, on, err := legacyPushToMain(tx, pol, it)
	if err != nil {
		return "", err
	}
	if it.Kind != domain.KindBrief && !on {
		return "", nil // taskDone 不产出
	}
	route := legacyRoute(pol, domain.DeliveryBrief, it.Priority)
	if it.Kind != domain.KindBrief {
		route = legacyRoute(pol, domain.DeliveryReminder, it.Priority)
	}
	if !on {
		return route, nil
	}
	main, blocked, err := MainRoute(tx, pol)
	if err != nil || blocked == "" {
		return main, err
	}
	if it.Kind != domain.KindBrief {
		return delivery.RouteWeb, nil
	}
	return route, nil
}

// legacyFailed 是旧版本 runPush 里模板型"没出来"的路由。
func legacyFailed(tx *store.Tx, pol policy.Policy, it domain.Item) (string, error) {
	r := pol.Disturb.PushRuleFor(policy.PushTemplateRun)
	route := legacyPushRoute(pol, r)
	if r.Route == "" {
		if _, on, err := legacyPushToMain(tx, pol, it); err != nil {
			return "", err
		} else if on {
			if agent, blocked, err := executors.MainWake(tx, pol); err != nil {
				return "", err
			} else if blocked == "" {
				route = delivery.WakeRoute(agent)
			}
		}
	}
	return route, nil
}

// only 把新流水线解析出的两层收成一条路由：换算出来的矩阵不会两层都推。
func only(t *testing.T, l layers, r row, what string) string {
	t.Helper()
	if l.user != "" && l.agent != "" {
		t.Fatalf("%s: derived matrix must not push both layers: %+v", what, l)
	}
	if s := l.user + l.agent; s != "" {
		return s
	}
	if r.floor {
		return delivery.RouteWeb
	}
	return ""
}

func legacyShapes() map[string]func(*policy.Policy) {
	return map[string]func(*policy.Policy){
		"default": func(*policy.Policy) {},
		"online": func(p *policy.Policy) {
			p.Disturb.Routes = map[string]string{"decision": "channel:ch-1", "reminder": "channel:ch-1", "brief": "inject", "health": "web", "wake": "web"}
		},
		"priorities": func(p *policy.Policy) {
			p.Disturb.Routes = map[string]string{"decision": "channel:a", "decision:urgent": "channel:b", "reminder": "inject", "reminder:low": "web",
				"brief": "channel:b", "brief:high": "web", "health": "channel:a", "wake": "web"}
		},
		"legacy names": func(p *policy.Policy) {
			p.Disturb.Routes = map[string]string{"decision": "web", "reminder": "tg", "brief": "tg", "health": "inject"}
		},
		"push routes": func(p *policy.Policy) {
			p.Disturb.Routes["decision"] = "channel:a"
			p.Disturb.Push = map[string]policy.PushRule{policy.PushSourceFailing: {On: true, Route: "channel:b"}, policy.PushTemplateRun: {On: true, Route: "channel:b"},
				policy.PushRunFailed: {On: true}, policy.PushNewProposals: {On: true, Min: 3, Route: "inject"}}
		},
		"task push flipped": func(p *policy.Policy) {
			p.Disturb.Routes["brief"] = "channel:a"
			pr := policy.DefaultProactive()
			pr.TaskPush = map[string]bool{"brief": false, "review": true, "routine": true}
			pr.InstancePush = map[string]bool{"ins_b2": true, "ins_x2": false, "ins_r": false}
			p.Disturb.Proactive = &pr
		},
	}
}

func TestDerivedMatrixRoutesExactlyLikeTheLegacyKeys(t *testing.T) {
	for _, wake := range []bool{true, false} {
		for name, shape := range legacyShapes() {
			e := setup(t)
			if wake {
				e.mainSession()
			}
			for id, pack := range map[string]string{"ins_b": "brief", "ins_b2": "brief", "ins_r": "review", "ins_x": "routine", "ins_x2": "routine", "ins_o": "custom"} {
				e.task(id, pack)
			}
			e.policy(shape)
			e.update(func(tx *store.Tx) error {
				_, pol, err := policy.Current(tx)
				if err != nil {
					return err
				}
				if pol.Disturb.Delivery != nil {
					t.Fatal("legacy shape must not carry a saved matrix")
				}
				check := func(what, want string, r row, prio domain.Priority) {
					t.Helper()
					l, err := resolve(tx, pol, r, prio)
					if err != nil {
						t.Fatal(err)
					}
					if got := only(t, l, r, what); got != want {
						t.Errorf("wake=%v %s / %s / %q: new %q, legacy %q", wake, name, what, prio, got, want)
					}
				}
				prios := []domain.Priority{"", domain.PriorityLow, domain.PriorityNormal, domain.PriorityHigh, domain.PriorityUrgent}
				for _, p := range prios {
					check("decision", legacyRoute(pol, domain.DeliveryDecision, p), row{keys: []string{policy.RowDecision}, floor: true}, p)
					for _, k := range []string{policy.RowAgentAnswered, policy.RowOverdue, policy.RowFollowup, policy.RowStale, policy.RowRuleReminder, policy.RowSourceAlert} {
						check(k, legacyRoute(pol, domain.DeliveryReminder, p), row{keys: []string{k}, floor: true}, p)
					}
					// 实例产出的事项类别由包声明（brief、review 只出 brief 类，routine 只出 routine 类）；自建类型、没有实例与实例已删的两类都试。
					for _, c := range []struct {
						src  string
						kind domain.Kind
					}{{"ins_b", domain.KindBrief}, {"ins_b2", domain.KindBrief}, {"ins_r", domain.KindBrief}, {"ins_x", "routine"}, {"ins_x2", "routine"},
						{"ins_o", domain.KindBrief}, {"ins_o", "routine"}, {"", domain.KindBrief}, {"", "routine"}, {"ins_gone", domain.KindBrief}, {"ins_gone", "routine"}} {
						{
							src, kind := c.src, c.kind
							it := domain.Item{ID: "it", Kind: kind, Priority: p}
							if src != "" {
								it.Origin = &domain.Origin{Source: domain.OriginInstancePrefix + src}
							}
							want, err := legacyTask(tx, pol, it)
							if err != nil {
								return err
							}
							r, err := taskRow(tx, it, false)
							if err != nil {
								return err
							}
							check(fmt.Sprintf("result %s/%s", src, kind), want, r, p)
							if want, err = legacyFailed(tx, pol, it); err != nil {
								return err
							}
							if r, err = taskRow(tx, it, true); err != nil {
								return err
							}
							check(fmt.Sprintf("failed %s/%s", src, kind), want, r, "") // 健康推送不带优先级
						}
					}
				}
				for row_, cat := range map[string]string{policy.RowSourceFailing: policy.PushSourceFailing, policy.RowRunFailed: policy.PushRunFailed, policy.RowNewProposals: policy.PushNewProposals} {
					check(row_, legacyPushRoute(pol, pol.Disturb.PushRuleFor(cat)), row{keys: []string{row_}, floor: true}, "")
				}
				check("health", legacyHealthRoute(pol), row{keys: []string{policy.RowHealth}, floor: true}, "")
				main, _, err := MainRoute(tx, pol)
				check("scheduled", main, row{keys: []string{policy.RowScheduled}, floor: true}, "")
				return err
			})
		}
	}
}

// ---- 两层都推 ----

func (e *env) matrix(rows map[string]policy.Target) {
	e.t.Helper()
	e.policy(func(p *policy.Policy) {
		m := p.Disturb.Matrix(nil)
		m.DefaultChannel = "channel:chan"
		for k, v := range rows {
			m.Rows[k] = v
		}
		p.Disturb.Delivery = &m
	})
}

// 两层都推出两条：推给本人的用基础 id，推给 Agent 的加 @agent；本人在 Web 回答后两条都确认。
func TestBothLayersYieldTwoDeliveriesAndAnswerAcknowledgesBoth(t *testing.T) {
	e := setup(t)
	e.matrix(map[string]policy.Target{policy.RowDecision: {Agent: policy.AgentNextChat, User: policy.UserDefault}})
	id := e.asked(nil)
	base := fmt.Sprintf("decision:%s:%d", id, e.version(id))
	u, a := e.get(base), e.get(base+AgentSuffix)
	if u.Route != "channel:chan" || a.Route != delivery.RouteInject || u.Version != a.Version {
		t.Fatalf("layers = %+v / %+v", u, a)
	}
	e.do(owner, "decision.answer", map[string]any{"id": id, "choice": "A"})
	if u, a := e.get(base), e.get(base+AgentSuffix); u.State != domain.DeliveryAcknowledged || a.State != domain.DeliveryAcknowledged {
		t.Fatalf("after answer: %s / %s, want both acknowledged", u.State, a.State)
	}
}

// 升级时在途的旧投递（基础 id）还在：改成两层都推后重算，不为同一提醒补出 Agent 层那一条。
func TestExistingBaseDeliveryIsNotDuplicatedByASecondLayer(t *testing.T) {
	e := setup(t)
	e.toChannel()
	id := e.asked(nil)
	base := fmt.Sprintf("decision:%s:%d", id, e.version(id))
	if e.get(base).Route != "channel:chan" {
		t.Fatal("setup")
	}
	e.matrix(map[string]policy.Target{policy.RowDecision: {Agent: policy.AgentNextChat, User: policy.UserDefault}})
	e.advance(time.Minute)
	e.update(func(tx *store.Tx) error { return e.a.reconcile(tx, mustPolicy(t, tx), id) })
	if err := e.st.View(bg, func(tx *store.Tx) error {
		_, err := e.q.Get(tx, base+AgentSuffix)
		if !isNotFound(err) {
			t.Fatalf("agent layer created for an in-flight reminder: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 合并只合去处相同的成员：两类低优先级提醒去处不同，各自成摘要。
func TestDigestOnlyMergesMembersWithTheSameTargets(t *testing.T) {
	e := setup(t)
	e.matrix(map[string]policy.Target{policy.RowOverdue: {Agent: policy.AgentOff, User: policy.UserDefault},
		policy.RowRuleReminder: {Agent: policy.AgentNextChat, User: policy.UserOff}})
	var its []domain.Item
	for i := 0; i < 4; i++ {
		its = append(its, e.getItem(e.item(map[string]any{"priority": "low", "title": fmt.Sprint("t", i)})))
	}
	e.update(func(tx *store.Tx) error {
		for i, it := range its {
			kind := "due"
			if i%2 == 1 {
				kind = "rule"
			}
			if err := e.a.Remind(tx, it, kind, t0, it.Title, ""); err != nil {
				return err
			}
		}
		return nil
	})
	e.advance(31 * time.Minute)
	ch, inj := e.drain("channel:chan"), e.injectable()
	if len(ch) != 1 || !strings.HasPrefix(ch[0].ID, "digest:") || ch[0].Content.Summary != "- t0\n- t2" ||
		len(inj) != 1 || !strings.HasPrefix(inj[0].ID, "digest:") || inj[0].Content.Summary != "- t1\n- t3" {
		t.Fatalf("channel %+v\ninject %+v", ch, inj)
	}
}

// 低优先级的例行任务产出不并进低优先级摘要：按自己的行进主会话（§1.4 的缺陷：旧版本并进摘要后走 routes.reminder）。
func TestLowPriorityRoutineOutputIsNotMergedAway(t *testing.T) {
	e := setup(t)
	e.mainSession()
	e.toChannel()
	src := e.task("ins_x", "routine")
	e.proactive(func(s *policy.Proactive) { s.TaskPush = map[string]bool{"routine": true} })
	other := e.getItem(e.item(map[string]any{"priority": "low"}))
	e.update(func(tx *store.Tx) error { return e.a.Remind(tx, other, "rule", t0, "另一条", "") })
	a := e.item(map[string]any{"kind": "task", "title": "例行", "priority": "low", "origin": src, "spec": map[string]any{"result": "已备份"}})
	e.do(owner, "item.transition", map[string]any{"id": a, "to": "done", "reason": "ran"})
	if d := e.wakes(); len(d) != 1 || d[0].ID != TaskID(a) || d[0].Content.Title != "【例行任务】例行" {
		t.Fatalf("routine output = %+v", d)
	}
}

// ---- 健康推送受静默（本人拍板）：安静时段攒着，结束后合成一条 ----

func TestHealthPushesWaitForQuietEndAndMergeIntoOne(t *testing.T) {
	e := setup(t)
	e.toChannel()
	e.policy(quiet(e.now.Add(-time.Hour), e.now.Add(time.Hour)))
	e.update(func(tx *store.Tx) error {
		for _, id := range []string{"push:source:s1:0:fail", "push:source:s2:0:fail"} {
			if err := e.a.Notify(tx, Notice{ID: id, Item: id[12:14], Class: domain.DeliveryHealth, Content: domain.DeliveryContent{Title: id[12:14] + " 失败"}}); err != nil {
				return err
			}
		}
		return e.a.Notify(tx, Notice{ID: "health:7", Item: "x", Class: domain.DeliveryHealth, Content: domain.DeliveryContent{Title: "告警"}})
	})
	if d := e.drain("channel:chan"); len(d) != 0 {
		t.Fatalf("pushes went out during quiet hours: %+v", d)
	}
	if u := e.unread(); len(u) != 1 || u[0].ID != "health:7" { // 系统告警不判静默（routes.health 是 web）
		t.Fatalf("system alert = %+v", u)
	}
	e.advance(61 * time.Minute)
	d := e.drain("channel:chan")
	if len(d) != 1 || !strings.HasPrefix(d[0].ID, "digest:") || d[0].Class != domain.DeliveryHealth || d[0].Content.Summary != "- s1 失败\n- s2 失败" {
		t.Fatalf("after quiet: %+v", d)
	}
}

// ---- 到点提醒受全局日上限（旧版本自己入队不过 CreateGated，送到渠道也不计数） ----

func TestScheduledReminderToChannelObeysDailyCap(t *testing.T) {
	e := setup(t)
	e.matrix(map[string]policy.Target{policy.RowScheduled: {Agent: policy.AgentOff, User: policy.UserDefault},
		policy.RowDecision: {Agent: policy.AgentOff, User: policy.UserDefault}})
	e.policy(func(p *policy.Policy) { p.Disturb.DailyCap = 1 })
	e.asked(nil) // 今天第一条渠道推送
	e.enable()
	e.item(map[string]any{"title": "喝水", "daily_at": e.now.Add(2 * time.Minute).In(time.Local).Format("15:04")})
	e.advance(3 * time.Minute)
	var sched []domain.Delivery
	for _, d := range e.unread() {
		if strings.HasPrefix(d.ID, schedBatchPrefix) {
			sched = append(sched, d)
		}
	}
	if len(sched) != 1 || sched[0].Suppressed != delivery.SuppressedDailyCap {
		t.Fatalf("scheduled batch over the cap = %+v", sched)
	}
}

// ---- "下次聊天时告诉我"不因 30 分钟没聊天而丢 ----

func TestNextChatSurvivesTheUnclaimedSweep(t *testing.T) {
	e := setup(t) // 出厂：决策下次对话时带上；无人领取 5 分钟转 Web 未读
	id := e.asked(nil)
	e.advance(10 * time.Minute)
	ds := e.injectable()
	if len(ds) != 1 || ds[0].Item != id || ds[0].State != domain.DeliveryFallbackWeb {
		t.Fatalf("swept next-chat delivery must still be injectable: %+v", ds)
	}
	e.update(func(tx *store.Tx) error { _, err := e.q.Inject(tx, ds[0].ID, "s1"); return err })
	if ds := e.injectable(); len(ds) != 0 {
		t.Fatalf("after one injection it is done: %+v", ds)
	}
}

func mustPolicy(t *testing.T, tx *store.Tx) policy.Policy {
	t.Helper()
	_, p, err := policy.Current(tx)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) version(id string) (v int64) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { v, err = command.Version(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) getItem(id string) (it domain.Item) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

// 委派结果（§13.6）：只推发起方那一层。主 Agent 发起的唤醒主会话；别的 Agent 发起的不唤醒主会话，按该行的 Else 下次对话带上；
// 本人发起的推本人层（没有默认渠道时 Web 未读），不再推给 Agent。
func TestDelegationResultGoesToWhoStartedIt(t *testing.T) {
	e := setup(t)
	e.mainSession()
	r := func(from, agent string) layers {
		var l layers
		e.update(func(tx *store.Tx) error {
			_, pol, err := policy.Current(tx)
			if err == nil {
				l, err = resolve(tx, pol, row{keys: []string{policy.RowDelegationResult}, floor: true, from: from, fromAgent: agent}, domain.PriorityNormal)
			}
			return err
		})
		return l
	}
	if l := r(fromAgent, "hx"); l.agent != delivery.RouteWake || l.user != "" {
		t.Errorf("main agent started it: %+v", l)
	}
	if l := r(fromAgent, "agt_other"); l.agent != delivery.RouteInject || l.user != "" {
		t.Errorf("another agent started it: %+v", l)
	}
	if l := r(fromUser, ""); l.user != delivery.RouteWeb || l.agent != "" {
		t.Errorf("the owner started it: %+v", l)
	}
}
