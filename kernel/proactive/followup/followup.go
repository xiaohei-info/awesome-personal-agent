// Package followup 是承接跟进（设计 §7.3、§8.9）：等人、等执行者、等外部的 waiting 事项到期后按规则梯度处理，
// 不需要模型。
//
//   - 事项进入 waiting（等时间的除外，那由事项中心到期回 open）时排一个 follow_up Timer；离开 waiting 即取消。
//   - Timer 到期只在时钟事务里登记一条待跟进记录（HandleTimer）；Drain 在事务外处理它。
//   - 梯度：前 remind_rounds 轮（默认 2）每轮经触达（唯一出口）产出一条提醒，带"已催""再等""放弃"三个动作，
//     并按同一间隔再排下一轮；已催是本人写一条进展，放弃是本人把事项转 dropped，再等什么都不用做
//     ——下一轮在提醒产出时就已排好。轮次用完仍无进展，经 decision.request 把事项转 needs_input 问去留
//     （继续等、我来处理、放弃），不静默丢弃，也不自己 dropped。
//   - 新进展（item.progress_added）、新挂靠消息（followmsg.attached）、阶段变化都让轮次清零。
//   - 推迟判断：策略 disturb.followup_defer 开启（默认关闭）且规则声明 defer_with 时，每轮提醒前经级联问一道是非题
//     "这一轮可以晚点提吗"。只能跳过这一轮的提醒，至多连续跳过一轮，不计入轮次，不替本人选动作，也不改事项。
//     否、未定、端点不可用或判断失败都照常提醒；失败另记一条 health.alert（模型不可用）。
//   - 间隔、轮数与推迟模板取自能力包的 follow_up 时钟规则，每次使用时从加载器读（热加载后立即生效）；
//     模式是该规则在规则状态里的当前模式——auto 才提醒，observe 只记 rule.observed（不提醒），off 不运行。
//     不是 auto 的到期轮次照常再排下一轮，切到 auto 后在途的等待事项不会漏掉。
//   - 写入一律经命令模型（自己的 followup.settle 与决策模块的 decision.request、事项的 item.transition）；
//     本包不起子进程、不调网络，只读事项，不直写其他模块的数据。
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 提醒给本人的三个动作（展示文字）。
var ReminderOptions = []string{"已催", "再等", "放弃"}

// 轮次用完后决策请求的选项 id。继续等 = 回到 waiting（等待对象沿用），我来处理 = 回到 open。
const (
	OptContinue = "continue"
	OptMine     = "mine"
	OptDrop     = "drop"
)

// 本模块自己追加的审计事件（不在对外事件类型表里，apa why 可见）。
const (
	EvReminded = "followup.reminded" // 已按梯度产出提醒
	EvDeferred = "followup.deferred" // 推迟判断为"可以晚点"，跳过这一轮提醒
)

const (
	timerPrefix = "followup_"
	remindKind  = "followup"
	// ruleType 是能力包里跟进间隔时钟规则的类型（C.4）。
	ruleType = "follow_up"
	// defaultRounds 是 remind_rounds 缺省值（C.4）。
	defaultRounds = 2
)

// Module 登记事件游标、待跟进表与轮次状态。
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
}, {
	// 轮次状态：rounds 是自上次清零以来已产出的提醒数；deferred 为 1 表示上一轮被推迟判断跳过（这一轮不再问）；
	// waiting_on 是转 needs_input 问去留时存下的等待对象，供"继续等"恢复。
	Version: 2, Ledger: 3, Name: "ledger-v3-followup-rounds",
	SQL: `CREATE TABLE followup_state (
	item TEXT PRIMARY KEY,
	rounds INTEGER NOT NULL DEFAULT 0,
	deferred INTEGER NOT NULL DEFAULT 0,
	waiting_on TEXT NOT NULL DEFAULT ''
);
CREATE TABLE followup_resume (
	item TEXT PRIMARY KEY,
	seq INTEGER NOT NULL
);`,
}}}

// Judge 是级联判断入口（*direct.Service 满足它）。
type Judge interface {
	Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error)
}

// Reminder 是触达的提醒入口（*attention.Attention 满足它）：提醒受静默与合并约束，同一 (kind, 事项, due) 只提醒一次。
type Reminder interface {
	RemindWith(tx *store.Tx, it domain.Item, kind string, due time.Time, title, summary string, options []string) error
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
	// Pack 是声明 follow_up 时钟规则的能力包名；间隔、轮数与推迟模板都从该规则取。
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
	key, schedule, deferWith string
	rounds                   int
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
		rounds := defaultRounds
		if cr.RemindRounds != nil {
			rounds = *cr.RemindRounds
		}
		return rule{key: rules.ClockKey(p.Name, cr.ID), schedule: cr.Schedule, deferWith: cr.DeferWith, rounds: rounds}, true
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
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the follow-up rule or the command input")
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

// Sync 读游标之后的事件：
//   - 事项创建或阶段变化：轮次清零；现在该被跟进就（重）排 follow_up Timer，否则取消。
//   - 新进展、新挂靠消息：轮次清零（Timer 不动，下一轮已经排好）。
//   - 决策被回答：本人选了"继续等"就登记一条恢复，由 Drain 把事项放回 waiting；其他选择丢掉存下的等待对象。
//
// 幂等。
func (s *Service) Sync(tx *store.Tx) error {
	after, err := s.cursor(tx)
	if err != nil {
		return err
	}
	const (
		rearm = 1 << iota
		reset
		resolved
	)
	var order []string
	flags := map[string]int{}
	resolvedAt := map[string]int64{}
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
			var f int
			switch en.Event.Type {
			case domain.EvItemCreated, domain.EvItemPhaseChanged:
				f = rearm
			case domain.EvItemProgressAdded, followmsg.EvAttached:
				f = reset
			case domain.EvDecisionResolved:
				f, resolvedAt[en.Event.Object] = resolved, en.Event.Seq
			}
			if id := en.Event.Object; f != 0 {
				if _, seen := flags[id]; !seen {
					order = append(order, id)
				}
				flags[id] |= f
			}
		}
	}
	r, ok := s.rule()
	for _, id := range order {
		f := flags[id]
		it, err := items.Get(tx, id)
		if err != nil && !isNotFound(err) {
			return err
		}
		gone := err != nil
		switch {
		case f&rearm != 0:
			if err := cancel(tx, id); err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM followup_state WHERE item = ?`, id); err != nil {
				return err
			}
			if !gone && ok && following(it) {
				if err := arm(tx, r, id, s.o.Now()); err != nil {
					return err
				}
			}
		case f&reset != 0:
			if _, err := tx.Exec(`UPDATE followup_state SET rounds = 0, deferred = 0 WHERE item = ?`, id); err != nil {
				return err
			}
		}
		if f&resolved != 0 && f&rearm == 0 {
			if err := resolveAsk(tx, it, gone, resolvedAt[id]); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`INSERT INTO followup_cursor (id, seq) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET seq = excluded.seq`, after)
	return err
}

// resolveAsk 处理一条决策被回答：只关心本模块存过等待对象的事项（即本模块发起的去留询问）。
func resolveAsk(tx *store.Tx, it domain.Item, gone bool, seq int64) error {
	if gone || it.Ask == nil || it.Ask.Resolution == nil {
		return nil
	}
	if it.Ask.Resolution.Choice == OptContinue && it.Phase == domain.PhaseOpen {
		_, err := tx.Exec(`INSERT INTO followup_resume (item, seq) SELECT item, ? FROM followup_state WHERE item = ? AND waiting_on != ''
			ON CONFLICT(item) DO UPDATE SET seq = excluded.seq`, seq, it.ID)
		return err
	}
	_, err := tx.Exec(`DELETE FROM followup_state WHERE item = ?`, it.ID)
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
	id   int64
	item string
	due  time.Time
}

// state 是一个事项当前的轮次状态；没有记录等同于全零。
type state struct {
	rounds   int64
	deferred bool
	waitedOn string
}

func loadState(tx *store.Tx, item string) (st state, err error) {
	r, err := tx.Query(`SELECT rounds, deferred, waiting_on FROM followup_state WHERE item = ?`, item)
	if err != nil {
		return st, err
	}
	if r.Next() {
		var d int64
		err = r.Scan(&st.rounds, &d, &st.waitedOn)
		st.deferred = d != 0
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	return st, err
}

func (s *Service) load(ctx context.Context, key string) (ps []pending, pol policy.Policy, pv int, m string, err error) {
	err = s.o.Store.View(ctx, func(tx *store.Tx) error {
		if pv, pol, err = policy.Current(tx); err != nil {
			return err
		}
		if m, err = mode(tx, key); err != nil {
			return err
		}
		r, err := tx.Query(`SELECT id, item, due FROM followup_pending ORDER BY id`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var p pending
			var due int64
			if err := r.Scan(&p.id, &p.item, &due); err != nil {
				return err
			}
			p.due = time.Unix(0, due).UTC()
			ps = append(ps, p)
		}
		return r.Err()
	})
	return
}

// Drain 处理所有待跟进，再处理"继续等"的恢复。某条失败不影响其余：失败的保留待重试，错误汇总返回。
// 组装处在每次时钟 Tick 之后调用，并按固定间隔再调（重试）。
// 规则不是 auto 时不提醒：observe 记一条 rule.observed，off 什么都不记，两者都清掉这条待跟进并再排下一轮。
func (s *Service) Drain(ctx context.Context) error {
	r, ok := s.rule()
	ps, pol, pv, m, err := s.load(ctx, r.key)
	if err != nil {
		return err
	}
	var errs []error
	for _, p := range ps {
		if !ok || m != rules.ModeAuto {
			err = s.skip(ctx, p, r, ok, m, pv)
		} else {
			err = s.handle(ctx, p, r, pol, pv)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("followup: %s: %w", p.item, err))
		}
	}
	if err := s.resume(ctx, r); err != nil {
		errs = append(errs, err)
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
			body, _ := json.Marshal(map[string]any{"rule": r.key, "due": p.due, "would_do": "follow-up reminder with chase/wait/abandon"})
			if _, err := eventlog.Append(tx, domain.Event{Type: rules.EvObserved, Object: it.ID, Actor: r.actor(),
				Basis: &domain.Basis{PolicyVersion: pv, RuleID: r.key}, Reason: "observe: would run " + r.key,
				Payload: body, ChainDepth: it.ChainDepth, At: s.o.Now()}); err != nil {
				return err
			}
		}
		return arm(tx, r, it.ID, s.o.Now())
	})
}

func (s *Service) handle(ctx context.Context, p pending, r rule, pol policy.Policy, pv int) error {
	var it domain.Item
	var st state
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if it, err = items.Get(tx, p.item); err == nil {
			st, err = loadState(tx, p.item)
		}
		return
	})
	if isNotFound(err) || (err == nil && !following(it)) {
		return s.clear(ctx, p.id) // 事项已不在等待：跟进作废
	}
	if err != nil {
		return err
	}
	key := fmt.Sprintf("followup:%s:%d", it.ID, p.due.UnixNano())
	if st.rounds >= int64(r.rounds) { // 轮次用完仍无进展：问去留
		if err := s.askKeep(ctx, r, it, key); err != nil {
			return err
		}
		return s.clear(ctx, p.id)
	}
	action := "remind"
	if pol.Disturb.FollowupDefer && r.deferWith != "" && !st.deferred && s.canDefer(ctx, p, r, it, st, pv) {
		action = "skip"
	}
	_, err = s.run(ctx, r, it.ID, key, "followup.settle", map[string]any{"id": it.ID, "pending": p.id, "action": action, "due": p.due})
	return err
}

// canDefer 问推迟判断："这一轮可以晚点提吗"。只有明确答"是"才为真；否、未定、端点不可用与判断失败都返回假（照常提醒），
// 后两种另记 health.alert，不让跟进因模型而丢失或延后。
func (s *Service) canDefer(ctx context.Context, p pending, r rule, it domain.Item, st state, pv int) bool {
	in, _ := json.Marshal(map[string]any{"item": it.ID, "waiting_on": it.WaitingOn, "round": st.rounds + 1, "remind_rounds": r.rounds, "due": p.due})
	material := it.Body
	if material == "" {
		material = it.Summary
	}
	v, err := s.o.Judge.Judge(ctx, direct.JudgeCall{Pack: s.o.Pack, Template: r.deferWith, Input: in, MaterialText: material,
		Subject: it.ID, Project: it.Project, NoStats: true})
	if err == nil && v.Unavailable {
		err = errors.New("no judge endpoint or chat endpoint answered")
	}
	if err != nil {
		_ = s.failed(ctx, p, r, err)
		return false
	}
	return v.Answer == "yes"
}

// askKeep 把轮次用完的事项转 needs_input 问去留，并存下等待对象供"继续等"恢复。
func (s *Service) askKeep(ctx context.Context, r rule, it domain.Item, key string) error {
	wo, _ := json.Marshal(it.WaitingOn)
	err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO followup_state (item, waiting_on) VALUES (?, ?) ON CONFLICT(item) DO UPDATE SET waiting_on = excluded.waiting_on`, it.ID, string(wo))
		return err
	})
	if err != nil {
		return err
	}
	_, err = s.run(ctx, r, it.ID, key+":ask", "decision.request", map[string]any{"id": it.ID, "reason": "follow-up rounds used up", "ask": domain.AskInput{
		Question: fmt.Sprintf("「%s」已经提醒过 %d 轮，等待仍没有进展。继续等、你来处理，还是放弃？", it.Title, r.rounds),
		Options: []domain.AskOption{
			{ID: OptContinue, Label: "继续等"},
			{ID: OptMine, Label: "我来处理"},
			{ID: OptDrop, Label: "放弃", Next: domain.PhaseDropped},
		},
	}})
	return err
}

// resume 把本人选了"继续等"的事项放回 waiting（沿用原等待对象）；事项已被别处改动、没有存下的等待对象则丢掉这条恢复。
func (s *Service) resume(ctx context.Context, r rule) error {
	type job struct {
		item string
		seq  int64
		on   json.RawMessage
		live bool
	}
	var jobs []job
	err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		rs, err := tx.Query(`SELECT r.item, r.seq, COALESCE(s.waiting_on, '') FROM followup_resume r LEFT JOIN followup_state s ON s.item = r.item ORDER BY r.rowid`)
		if err != nil {
			return err
		}
		var all []job
		for rs.Next() {
			var j job
			var on string
			if err := rs.Scan(&j.item, &j.seq, &on); err != nil {
				_ = rs.Close()
				return err
			}
			j.on = json.RawMessage(on)
			all = append(all, j)
		}
		if err := rs.Close(); err != nil {
			return err
		}
		for _, j := range all {
			it, err := items.Get(tx, j.item)
			if err != nil && !isNotFound(err) {
				return err
			}
			j.live = err == nil && it.Phase == domain.PhaseOpen && len(j.on) > 0
			jobs = append(jobs, j)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if r.key == "" { // 包里没有 follow_up 规则时仍要兑现本人的选择，授权依据记包名
		r.key = s.o.Pack + "/clock_rules/follow-up"
	}
	var errs []error
	for _, j := range jobs {
		if j.live {
			var wo domain.WaitingOn
			if err := json.Unmarshal(j.on, &wo); err != nil {
				errs = append(errs, fmt.Errorf("followup: %s: %w", j.item, err))
				continue
			}
			if _, err := s.run(ctx, r, j.item, fmt.Sprintf("followup:%s:resume:%d", j.item, j.seq), "item.transition",
				map[string]any{"id": j.item, "to": domain.PhaseWaiting, "reason": "owner chose to keep waiting", "waiting_on": wo}); err != nil {
				errs = append(errs, fmt.Errorf("followup: %s: %w", j.item, err))
				continue
			}
		}
		if err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec(`DELETE FROM followup_resume WHERE item = ?`, j.item)
			return err
		}); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
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

// failed 记一条推迟判断失败的 health.alert；跟进本身不受影响（照常提醒）。
func (s *Service) failed(ctx context.Context, p pending, r rule, cause error) error {
	return s.o.Store.Update(ctx, func(tx *store.Tx) error {
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		// kind 与 template 供健康视图归类（模型不可用）并在同一模板之后成功调用时清除，字面值见 kernel/health.FollowupKind。
		body, _ := json.Marshal(map[string]string{"kind": "followup", "template": r.deferWith, "rule": r.key, "item": p.item, "error": cause.Error()})
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: p.item, Actor: r.actor(),
			Basis: &domain.Basis{PolicyVersion: pv, RuleID: r.key}, Reason: "follow-up defer judgement failed, reminding as scheduled: " + cause.Error(),
			Payload: body, At: s.o.Now()})
		return err
	})
}

// ---- followup.settle ----

type settleIn struct {
	ID      string    `json:"id"`
	Pending int64     `json:"pending"`
	Action  string    `json:"action"` // remind：产出提醒；skip：推迟判断跳过这一轮
	Due     time.Time `json:"due"`
}

// Register 登记 followup.settle：提醒（或推迟跳过）、记轮次、再排下一轮 Timer、清掉这条待跟进。
// 都在同一事务里完成，崩溃不会留下"提醒已出、待跟进还在"的半截状态。
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
			if err == nil && (v.ID == "" || v.Due.IsZero() || (v.Action != "remind" && v.Action != "skip")) {
				err = invalid("followup.settle needs an item id, the round's due time and action remind or skip")
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
			r, ok := s.rule()
			if !ok {
				return nil, invalid("pack " + s.o.Pack + " has no follow_up clock rule")
			}
			typ := EvDeferred
			if v.Action == "remind" {
				typ = EvReminded
				if err := s.o.Reminder.RemindWith(c.Tx, it, remindKind, v.Due, "该跟进了："+it.Title,
					fmt.Sprintf("事项已等待 %s「%s」到期仍无进展，可以主动问一句。选「已催」记一条进展，「再等」等下一轮，「放弃」结束这件事。", it.WaitingOn.Kind, it.WaitingOn.Ref),
					ReminderOptions); err != nil {
					return nil, err
				}
				_, err = c.Tx.Exec(`INSERT INTO followup_state (item, rounds) VALUES (?, 1) ON CONFLICT(item) DO UPDATE SET rounds = rounds + 1, deferred = 0`, it.ID)
			} else {
				_, err = c.Tx.Exec(`INSERT INTO followup_state (item, deferred) VALUES (?, 1) ON CONFLICT(item) DO UPDATE SET deferred = 1`, it.ID)
			}
			if err != nil {
				return nil, err
			}
			next, err := r.next(c.Now())
			if err != nil {
				return nil, err
			}
			if err := arm(c.Tx, r, it.ID, c.Now()); err != nil {
				return nil, err
			}
			p, _ := json.Marshal(map[string]any{"outcome": v.Action, "next": next})
			_, err = c.Emit(domain.Event{Type: typ, Object: it.ID, Reason: "follow-up " + v.Action, Payload: p})
			return map[string]string{"outcome": v.Action}, err
		}})
}
