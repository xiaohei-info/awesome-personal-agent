// Package followup 是承接跟进（设计 §7.3、§6.4、§8.3）：等人、等执行者、等外部的 waiting 事项到期后，
// 调判断型给出"催""再等"或"建议放弃"。
//
//   - 事项进入 waiting（等时间的除外，那由事项中心到期回 open）时排一个 follow_up Timer；离开 waiting 即取消。
//   - Timer 到期只在时钟事务里登记一条待跟进记录（HandleTimer）；判断是外呼，由 Drain 在事务外做。
//     待跟进记录在提交结果之前一直保留：模型未配置、持续不可用或输出不合 schema，都返回真实错误并记一条
//     health.alert，下次 Drain 原样重试，跟进不会丢。
//   - 催：经触达（唯一出口）产出一条提醒并再排一轮 Timer，不自动向对方发催问。
//     再等：延长 Timer，不产出提醒。
//     建议放弃：经 decision.request 把事项转 needs_input 交本人，选项由本人回答；不静默丢弃，也不自己 dropped。
//     置信度不足、需要升级到任务型等"判断未定"同样交本人，不替本人决定。
//   - 间隔、提示词与模式都取自能力包的 follow_up 时钟规则，每次使用时从加载器读（热加载后立即生效）：
//     schedule 是每一轮的跟进间隔，call_agent 的 prompt 是判断型模板；模式是该规则在规则状态里的当前模式，
//     与 kernel/rules 同一语义——auto 才判断与提醒，observe 只记 rule.observed（不外呼、不提醒），off 不运行。
//     不是 auto 的到期轮次照常再排下一轮，切到 auto 后在途的等待事项不会漏掉。
//   - 写入一律经命令模型（自己的 followup.settle 与决策模块的 decision.request）；本包不起子进程、不调网络，
//     只读事项，不直写其他模块的数据。
package followup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 判断型提示词必须是 choice 题，选项 id 取下面三个（可另带 explain 选项用于级联）。
const (
	ChoiceRemind  = "remind"
	ChoiceWait    = "wait"
	ChoiceAbandon = "abandon"
)

// 决策请求的选项 id。两个选项都不是"继续等"：waiting 只能由本人经命令重新进入。
const (
	OptDrop   = "drop"
	OptReopen = "reopen"
)

// 本模块自己追加的审计事件（不在对外事件类型表里，apa why 可见）。
const (
	EvReminded = "followup.reminded" // 催：已交触达产出提醒
	EvExtended = "followup.extended" // 再等：已延长
)

const (
	timerPrefix = "followup_"
	remindKind  = "followup"
	// ruleType 是能力包里跟进间隔时钟规则的类型（C.4）。
	ruleType = "follow_up"
)

// Module 登记事件游标与待跟进表。
var Module = store.Module{Name: "followup", Migrations: []store.Migration{{
	Version: 1, Name: "create-followup",
	SQL: `CREATE TABLE followup_cursor (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
);
CREATE TABLE followup_pending (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	item TEXT NOT NULL UNIQUE,
	due INTEGER NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0
);`,
}}}

// Judge 是专属 agent 的判断型入口（*direct.Service 满足它）。
type Judge interface {
	Do(ctx context.Context, c direct.Call) (direct.Outcome, error)
}

// Reminder 是触达的提醒入口（*attention.Attention 满足它）：提醒受静默与合并约束，同一 (kind, 事项, due) 只提醒一次。
type Reminder interface {
	Remind(tx *store.Tx, it domain.Item, kind string, due time.Time, title, summary string) error
}

// PackSource 取已加载的能力包（packs.Loader 满足它）。
type PackSource interface {
	Get(name string) (*packs.Pack, bool)
}

// Options 配置跟进。除 Now 与 Owner 外都必填。
type Options struct {
	Store    *store.Store
	Exec     *command.Executor
	Judge    Judge
	Reminder Reminder
	Packs    PackSource
	// Pack 是声明 follow_up 时钟规则的能力包名；间隔、判断型模板与模式都从该规则取。
	Pack  string
	Owner string // 规则式动作的 on_behalf_of，默认 "owner"
	Now   func() time.Time
}

// Service 无内存状态，所有状态在账本里。
type Service struct{ o Options }

// New 校验配置并返回服务。
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Exec == nil || o.Judge == nil || o.Reminder == nil || o.Packs == nil || o.Pack == "" {
		return nil, errors.New("followup: Store, Exec, Judge, Reminder, Packs and Pack are required")
	}
	if o.Owner == "" {
		o.Owner = "owner"
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	return &Service{o}, nil
}

// rule 是跟进间隔时钟规则的当前声明。
type rule struct {
	key, schedule, template string
}

// rule 从加载器取当前生效的 follow_up 规则；包未加载或没有这类规则时 ok 为假，跟进不运行。
func (s *Service) rule() (r rule, ok bool) {
	p, ok := s.o.Packs.Get(s.o.Pack)
	if !ok {
		return rule{}, false
	}
	for _, cr := range p.ClockRules {
		if cr.Type != ruleType {
			continue
		}
		for _, a := range cr.Do {
			if t, _ := a["prompt"].(string); a["action"] == "call_agent" && t != "" {
				return rule{key: rules.ClockKey(p.Name, cr.ID), schedule: cr.Schedule, template: t}, true
			}
		}
	}
	return rule{}, false
}

// next 是从 now 起的下一轮跟进时刻（schedule 是时长或 cron，加载器已校验）。
func (r rule) next(now time.Time) (time.Time, error) { return clock.Start(r.schedule, now) }

func (r rule) actor() domain.Actor {
	return domain.Actor{Type: domain.ActorSystem, Principal: "rule:" + r.key, Via: domain.ViaRule}
}

// mode 是规则状态里的当前模式；未登记按 off。
func mode(tx *store.Tx, key string) (string, error) {
	sts, err := rules.List(tx)
	for _, st := range sts {
		if st.Key == key {
			return st.Mode, err
		}
	}
	return rules.ModeOff, err
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the follow-up prompt template or the command input")
}

func timerID(item string) string { return timerPrefix + item }

// following 判断事项是否该被跟进：等人、等执行者、等外部。
func following(it domain.Item) bool {
	return it.Phase == domain.PhaseWaiting && it.WaitingOn != nil && it.WaitingOn.Kind != domain.WaitingTime
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

// ---- 排 Timer ----

// Attach 让命令提交后立即追赶事件（排或取消 Timer）。onErr 接收失败；游标没推进，下次 Sync 会重做。
func (s *Service) Attach(onErr func(error)) {
	s.o.Exec.OnCommit(func(command.Commit) {
		if err := s.o.Store.Update(context.Background(), s.Sync); err != nil && onErr != nil {
			onErr(err)
		}
	})
}

// Sync 读游标之后的事件：事项现在该被跟进就（重）排 follow_up Timer，否则取消。幂等。
func (s *Service) Sync(tx *store.Tx) error {
	after, err := s.cursor(tx)
	if err != nil {
		return err
	}
	var order []string
	seen := map[string]bool{}
	for {
		es, err := eventlog.Read(tx, after, 500)
		if err != nil {
			return err
		}
		if len(es) == 0 {
			break
		}
		for _, en := range es {
			after = en.Event.Seq
			t := en.Event.Type
			if id := en.Event.Object; (t == domain.EvItemCreated || t == domain.EvItemPhaseChanged) && !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}
	r, ok := s.rule()
	for _, id := range order {
		it, err := items.Get(tx, id)
		if err != nil && !isNotFound(err) {
			return err
		}
		if err := cancel(tx, id); err != nil {
			return err
		}
		if err == nil && ok && following(it) {
			if err := arm(tx, r, id, s.o.Now()); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`INSERT INTO followup_cursor (id, seq) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET seq = excluded.seq`, after)
	return err
}

// arm 按规则当前间隔排（或重排）下一轮跟进。
// shortcut: 间隔改了只影响之后排的轮次，已排的 Timer 按原到期；需要即时生效时在热加载里重排在途 Timer。
func arm(tx *store.Tx, r rule, item string, now time.Time) error {
	at, err := r.next(now)
	if err != nil {
		return err
	}
	if err := cancel(tx, item); err != nil {
		return err
	}
	_, err = clock.Add(tx, domain.Timer{ID: timerID(item), Target: item, Kind: domain.TimerFollowUp, At: at})
	return err
}

func cancel(tx *store.Tx, item string) error {
	if err := clock.Cancel(tx, timerID(item)); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

func (s *Service) cursor(tx *store.Tx) (int64, error) {
	var seq int64
	r, err := tx.Query(`SELECT seq FROM followup_cursor WHERE id = 1`)
	if err != nil {
		return 0, err
	}
	if r.Next() {
		err = r.Scan(&seq)
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return 0, err
	}
	head, err := eventlog.Head(tx)
	if err != nil {
		return 0, err
	}
	if seq > head { // 账本被恢复到较早状态：从头来，宁可重排也不漏
		return 0, nil
	}
	return seq, nil
}

// ---- Timer 到期 ----

// Owns 判断时钟交来的命令是否归本模块；组装处据它在处理器间分派。
func (s *Service) Owns(c clock.Command) bool {
	return c.Kind == domain.TimerFollowUp && strings.HasPrefix(c.TimerID, timerPrefix)
}

// HandleTimer 在时钟的事务里只登记一条待跟进记录；判断是外呼，不能嵌在时钟事务里，由 Drain 执行。
// 已有未处理的待跟进（上一轮还在重试）时保留较早的那条。
func (s *Service) HandleTimer(tx *store.Tx, c clock.Command) error {
	if !s.Owns(c) {
		return invalid("timer " + c.TimerID + " is not handled by the follow-up module")
	}
	_, err := tx.Exec(`INSERT OR IGNORE INTO followup_pending (item, due) VALUES (?, ?)`, c.Target, c.Due.UnixNano())
	return err
}

// ---- 处理待跟进 ----

type pending struct {
	id       int64
	item     string
	due      time.Time
	attempts int64
}

func (s *Service) load(ctx context.Context, key string) (ps []pending, pv int, m string, err error) {
	err = s.o.Store.View(ctx, func(tx *store.Tx) error {
		if pv, _, err = policy.Current(tx); err != nil {
			return err
		}
		if m, err = mode(tx, key); err != nil {
			return err
		}
		r, err := tx.Query(`SELECT id, item, due, attempts FROM followup_pending ORDER BY id`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var p pending
			var due int64
			if err := r.Scan(&p.id, &p.item, &due, &p.attempts); err != nil {
				return err
			}
			p.due = time.Unix(0, due).UTC()
			ps = append(ps, p)
		}
		return r.Err()
	})
	return
}

// Drain 处理所有待跟进。某条失败不影响其余：失败的保留待重试，错误汇总返回。
// 组装处在每次时钟 Tick 之后调用，并按固定间隔再调（重试）。
// shortcut: 失败的待跟进在每次 Drain 都重试，没有退避；模型持续不可用时调用间隔即重试间隔。
// 规则不是 auto 时不判断：observe 记一条 rule.observed，off 什么都不记，两者都清掉这条待跟进并再排下一轮。
func (s *Service) Drain(ctx context.Context) error {
	r, ok := s.rule()
	ps, pv, m, err := s.load(ctx, r.key)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range ps {
		if !ok || m != rules.ModeAuto {
			err = s.skip(ctx, p, r, ok, m, pv)
		} else {
			err = s.handle(ctx, p, r, pv)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("followup: %s: %w", p.item, err))
		}
	}
	return errors.Join(errs...)
}

// skip 处理不是 auto 的一轮：同一事务里清掉待跟进、observe 时记录本来会做什么、事项仍在等待就再排下一轮。
func (s *Service) skip(ctx context.Context, p pending, r rule, ok bool, m string, pv int) error {
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM followup_pending WHERE id = ?`, p.id); err != nil || !ok {
			return err
		}
		it, err := items.Get(tx, p.item)
		if isNotFound(err) || (err == nil && !following(it)) {
			return nil
		}
		if err != nil {
			return err
		}
		if m == rules.ModeObserve {
			body, _ := json.Marshal(map[string]any{"rule": r.key, "due": p.due, "would_do": "follow-up judgement " + s.o.Pack + "/" + r.template})
			if _, err := eventlog.Append(tx, domain.Event{Type: rules.EvObserved, Object: it.ID, Actor: r.actor(),
				Basis: &domain.Basis{PolicyVersion: pv, RuleID: r.key}, Reason: "observe: would run " + r.key,
				Payload: body, ChainDepth: it.ChainDepth, At: s.o.Now()}); err != nil {
				return err
			}
		}
		return arm(tx, r, it.ID, s.o.Now())
	})
}

func (s *Service) handle(ctx context.Context, p pending, r rule, pv int) error {
	var it domain.Item
	var depth int
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if it, err = items.Get(tx, p.item); err == nil {
			depth, err = items.ChainDepth(tx, p.item)
		}
		return
	})
	if isNotFound(err) || (err == nil && !following(it)) {
		return s.clear(ctx, p.id) // 事项已不在等待：跟进作废
	}
	if err != nil {
		return err
	}
	in, _ := json.Marshal(map[string]any{"item": it.ID, "waiting_on": it.WaitingOn, "due": p.due})
	material := it.Body
	if material == "" {
		material = it.Summary
	}
	out, err := s.o.Judge.Do(ctx, direct.Call{Pack: s.o.Pack, Template: r.template, Input: in, Material: material,
		Target: it.ID, Project: it.Project, ChainDepth: depth, // 定时触发：沿用事项深度，不能让判断记录把链路清零
		Actor: domain.Actor{Type: domain.ActorAgent, Principal: "rule:" + r.key, Via: domain.ViaRule},
		Basis: &domain.Basis{PolicyVersion: pv, RuleID: r.key}})
	if err != nil {
		return errors.Join(err, s.failed(ctx, p, r, err))
	}
	choice, err := decide(out)
	if err != nil {
		return errors.Join(err, s.failed(ctx, p, r, err))
	}
	key := fmt.Sprintf("followup:%s:%d", it.ID, p.due.UnixNano())
	switch choice {
	case ChoiceRemind, ChoiceWait:
		_, err = s.run(ctx, r, it.ID, key, "followup.settle", map[string]any{"id": it.ID, "pending": p.id, "remind": choice == ChoiceRemind, "due": p.due})
		return err
	case ChoiceAbandon:
		err = s.ask(ctx, r, it, key, "跟进判断建议放弃", fmt.Sprintf("「%s」等待已久仍无进展，建议放弃。是否放弃这件事？", it.Title), OptDrop)
	default: // 判断未定
		err = s.ask(ctx, r, it, key, "跟进判断未定", fmt.Sprintf("「%s」到了跟进时间，但无法自动判断该催、再等还是放弃，请你决定。", it.Title), OptReopen)
	}
	if err != nil {
		return err
	}
	return s.clear(ctx, p.id)
}

const choiceUndecided = ""

// decide 取判断型的答案；判断未定（置信度不足、需要解释、升级到任务型）返回空串，由本人决定。
func decide(out direct.Outcome) (string, error) {
	if out.HandoffTask != "" || !out.Settled {
		return choiceUndecided, nil
	}
	var j direct.Judgement
	if err := json.Unmarshal(out.Final().Result, &j); err != nil || j.Choice == "" {
		return "", invalid("follow-up prompt " + out.Final().Template + " must be a choice judge")
	}
	switch j.Choice {
	case ChoiceRemind, ChoiceWait, ChoiceAbandon:
		return j.Choice, nil
	}
	return "", invalid("follow-up judge answered " + j.Choice + "; choices must be remind, wait, abandon")
}

func (s *Service) ask(ctx context.Context, r rule, it domain.Item, key, reason, question, recommend string) error {
	_, err := s.run(ctx, r, it.ID, key+":ask", "decision.request", map[string]any{"id": it.ID, "reason": reason, "ask": domain.AskInput{
		Question: question,
		Options: []domain.AskOption{
			{ID: OptDrop, Label: "放弃", Next: domain.PhaseDropped},
			{ID: OptReopen, Label: "回到待办，由我处理"},
		},
		Recommend: recommend,
	}})
	return err
}

// run 以跟进自己的规则式身份走命令模型；授权依据（策略版本与规则 id）记入 Event。
func (s *Service) run(ctx context.Context, r rule, item, idem, cmd string, input any) (command.Result, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return command.Result{}, err
	}
	a := r.actor()
	a.OnBehalfOf = s.o.Owner
	return s.o.Exec.Do(ctx, command.Request{Identity: command.Trusted{Actor: a, Level: authz.LevelDelegate}, Command: cmd, Input: b,
		IdemKey: idem, Chain: command.Chain{Mode: command.ChainInherit, From: item}, RuleID: r.key, Evidence: "rule " + r.key, Owner: s.o.Owner})
}

func (s *Service) clear(ctx context.Context, id int64) error {
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM followup_pending WHERE id = ?`, id)
		return err
	})
}

// failed 记下一次失败；只在第一次失败时追加 health.alert，重试不刷屏。待跟进保留。
func (s *Service) failed(ctx context.Context, p pending, r rule, cause error) error {
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE followup_pending SET attempts = attempts + 1 WHERE id = ?`, p.id); err != nil || p.attempts > 0 {
			return err
		}
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		// kind 与 template 供健康视图归类（模型不可用）并在同一模板之后成功调用时清除，字面值见 kernel/health.FollowupKind。
		body, _ := json.Marshal(map[string]string{"kind": "followup", "template": r.template, "rule": r.key, "item": p.item, "error": cause.Error()})
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: p.item, Actor: r.actor(),
			Basis: &domain.Basis{PolicyVersion: pv, RuleID: r.key}, Reason: "follow-up judgement failed, will retry: " + cause.Error(),
			Payload: body, At: s.o.Now()})
		return err
	})
}

// ---- followup.settle ----

type settleIn struct {
	ID      string    `json:"id"`
	Pending int64     `json:"pending"`
	Remind  bool      `json:"remind"`
	Due     time.Time `json:"due"`
}

// Register 登记 followup.settle：催（产出提醒）或再等，都再排下一轮 Timer，并清掉这条待跟进。
// 三件事在同一事务里完成，崩溃不会留下"提醒已出、待跟进还在"的半截状态。
func (s *Service) Register() error {
	parse := func(in json.RawMessage) (v settleIn, err error) {
		dec := json.NewDecoder(strings.NewReader(string(in)))
		dec.DisallowUnknownFields()
		if err = dec.Decode(&v); err != nil {
			err = invalid("bad input: " + err.Error())
		}
		return
	}
	return s.o.Exec.Register(command.Spec{Name: "followup.settle", Action: authz.ActionUpdateOwn,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			it, err := items.Get(tx, v.ID)
			return authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Tags: it.Tags, Project: it.Project}, err
		},
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, err := parse(in)
			if err == nil && (v.ID == "" || v.Due.IsZero()) {
				err = invalid("followup.settle needs an item id and the round's due time")
			}
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			it, err := items.Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			if _, err := c.Tx.Exec(`DELETE FROM followup_pending WHERE id = ?`, v.Pending); err != nil {
				return nil, err
			}
			if !following(it) { // 并发下事项已离开 waiting：这一轮作废
				return map[string]string{"outcome": "stale"}, nil
			}
			typ, outcome := EvExtended, "wait"
			if v.Remind {
				typ, outcome = EvReminded, "remind"
				if err := s.o.Reminder.Remind(c.Tx, it, remindKind, v.Due, "该跟进了："+it.Title,
					fmt.Sprintf("事项已等待 %s「%s」到期仍无进展，可以主动问一句。", it.WaitingOn.Kind, it.WaitingOn.Ref)); err != nil {
					return nil, err
				}
			}
			r, ok := s.rule()
			if !ok {
				return nil, invalid("pack " + s.o.Pack + " has no follow_up clock rule")
			}
			next, err := r.next(c.Now())
			if err != nil {
				return nil, err
			}
			if err := arm(c.Tx, r, it.ID, c.Now()); err != nil {
				return nil, err
			}
			p, _ := json.Marshal(map[string]any{"outcome": outcome, "next": next})
			_, err = c.Emit(domain.Event{Type: typ, Object: it.ID, Reason: "follow-up " + outcome, Payload: p})
			return map[string]string{"outcome": outcome}, err
		}})
}
