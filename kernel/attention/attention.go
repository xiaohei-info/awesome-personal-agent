// Package attention 是触达（设计 §7.4、§6.14、§11.6）：各模块对外提醒的唯一出口。
// 它读账本事件与事项现状，按静默、合并、阈值（优先级路由）产出投递，自身不发送，由消费方向投递队列领取。
//
// 工作方式是电平触发：事项一变（Sync）就按事项现状重算它该有什么投递——proposed 与初筛未返回的不产出，
// 待决的决策产出一条（版本取事项版本），事项变化后旧版本投递作废（Supersede）；时间相关的（截止已过再提醒、
// 静默结束补发、合并窗口到期、无人领取退回 Web 未读）由一个周期 Timer 驱动的 Tick 处理。
// 配了 Drafter 且策略配了模型端点时，决策投递先经 decision-card 包起草卡片文案，见 draft.go。
// 本包只写自己的游标、暂存与起草表，投递表经 delivery.Queue 写入，事项与策略只读。
package attention

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// NearDeadline 是"临近截止"：决策的 ask.deadline 距现在不超过它，且本人开启了豁免，则不受静默限制（§7.4）。
const NearDeadline = 30 * time.Minute

// DigestItem 是合并摘要投递的事项占位：摘要不属于任何一个事项，不随单个事项变化而作废。
const DigestItem = "attention:digest"

const (
	tickID     = "attn_tick"
	tickTarget = "attention"
)

// Module 登记事件游标与暂存表。暂存表里是被静默或合并窗口拦住、尚未成为投递的提醒。
var Module = store.Module{Name: "attention", Migrations: []store.Migration{{
	Version: 1, Name: "create-attention",
	SQL: `CREATE TABLE attention_cursor (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
);
CREATE TABLE attention_held (
	id TEXT PRIMARY KEY,
	item TEXT NOT NULL,
	class TEXT NOT NULL,
	priority TEXT NOT NULL,
	content TEXT NOT NULL,
	version INTEGER NOT NULL,
	exempt INTEGER NOT NULL,
	mergeable INTEGER NOT NULL,
	release_at INTEGER NOT NULL
);
CREATE TABLE attention_done (
	id TEXT PRIMARY KEY
) WITHOUT ROWID;
CREATE INDEX attention_held_item ON attention_held(item);
CREATE INDEX attention_held_release ON attention_held(release_at);`,
}, {
	Version: 2, Name: "create-attention-draft",
	// 每个待决决策一行：哪个版本起草到了哪一步（pending 未返回时不投递）、卡片文案；verdict 是已取消的投递推迟判定留下的列，不再读写。
	SQL: `CREATE TABLE attention_draft (
	item TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	state TEXT NOT NULL,
	card TEXT NOT NULL DEFAULT '',
	verdict TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;`,
}, {
	// 账本结构版本 3：规则 alert 出口的提醒没有事项，被静默拦住时要把出处（origin，JSON）一并暂存。
	Version: 3, Ledger: 3, Name: "ledger-v3-held-origin",
	SQL: `ALTER TABLE attention_held ADD COLUMN origin TEXT NOT NULL DEFAULT '';`,
}}}

// Options 配置触达；Queue 必填。
type Options struct {
	Queue     *delivery.Queue
	Now       func() time.Time
	TickEvery time.Duration            // 周期 Timer 间隔，默认 30 秒
	WebURL    func(item string) string // 投递里的 Web 链接，默认 /items/<id>
	// Drafter 起草决策卡片文案；为空时投递内容只按规则。
	Drafter Drafter
}

// Attention 的状态都在账本里；draftDue 只是"有待起草"的唤醒信号。
type Attention struct {
	o        Options
	draftDue atomic.Bool
}

// New 返回触达模块。
func New(o Options) *Attention {
	if o.Now == nil {
		o.Now = clock.Now
	}
	if o.TickEvery <= 0 {
		o.TickEvery = 30 * time.Second
	}
	if o.WebURL == nil {
		o.WebURL = func(id string) string { return "/items/" + id }
	}
	return &Attention{o: o}
}

// ---- 接入 ----

// Start 在 tx 内登记周期 Timer（重复调用只重置下一次到期时间）。组装处启动时调用一次。
func (a *Attention) Start(tx *store.Tx) error {
	if err := clock.Cancel(tx, tickID); err != nil && !isNotFound(err) {
		return err
	}
	_, err := clock.Add(tx, domain.Timer{ID: tickID, Target: tickTarget, Kind: domain.TimerRecur, At: a.o.Now(), Recurrence: a.o.TickEvery.String()})
	return err
}

// Owns 判断时钟交来的命令是否归本模块；组装处据它在处理器间分派。
func (a *Attention) Owns(c clock.Command) bool { return c.TimerID == tickID }

// HandleTimer 处理周期 Timer：一次完整的 Tick。在时钟的事务里运行。
func (a *Attention) HandleTimer(tx *store.Tx, c clock.Command) error {
	if !a.Owns(c) {
		return fmt.Errorf("attention: timer %s is not mine", c.TimerID)
	}
	return a.Tick(tx)
}

// Attach 让命令提交后立即追赶事件（"提交后唤醒触达"，§7.0），不必等下一次 Tick。onErr 接收追赶失败；
// 失败不丢事件：游标没推进，下一次 Sync 或 Tick 会重做。
func (a *Attention) Attach(x *command.Executor, st *store.Store, onErr func(error)) {
	x.OnCommit(func(command.Commit) {
		if err := st.Update(context.Background(), a.Sync); err != nil && onErr != nil {
			onErr(err)
		}
	})
}

// Tick 做所有与时间有关的事：追赶事件、截止已过再提醒、到点提醒（schedule.go）、放行到期的暂存提醒、清扫无人领取的投递。
func (a *Attention) Tick(tx *store.Tx) error {
	if err := a.Sync(tx); err != nil {
		return err
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	if err := a.remindOverdue(tx, pol); err != nil {
		return err
	}
	if err := a.remindScheduled(tx, pol); err != nil {
		return err
	}
	if err := a.flush(tx, pol); err != nil {
		return err
	}
	_, err = a.o.Queue.Sweep(tx)
	return err
}

// ---- 事件追赶与按事项重算 ----

// 会改变事项该有什么投递的事件；决策已回答、阶段变化、字段修改都在其中。
var itemEvents = map[string]bool{domain.EvItemCreated: true, domain.EvItemUpdated: true, domain.EvItemPhaseChanged: true,
	domain.EvDecisionRequested: true, domain.EvDecisionResolved: true}

// Sync 读游标之后的事件，对涉及的事项按现状重算投递。幂等：重复处理同一事件不会重复产出。
func (a *Attention) Sync(tx *store.Tx) error {
	after, err := a.cursor(tx)
	if err != nil {
		return err
	}
	var order []string
	seen := map[string]bool{}
	for {
		es, err := eventlog.ReadLive(tx, after, 500)
		if err != nil {
			return err
		}
		if len(es) == 0 {
			break
		}
		for _, en := range es {
			after = en.Event.Seq
			if id := en.Event.Object; itemEvents[en.Event.Type] && !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
		}
	}
	if len(order) > 0 {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		for _, id := range order {
			if err := a.reconcile(tx, pol, id); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`INSERT INTO attention_cursor (id, seq) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET seq = excluded.seq`, after)
	return err
}

func (a *Attention) cursor(tx *store.Tx) (int64, error) {
	var seq int64
	if err := scan(tx, func(r *store.Rows) error { return r.Scan(&seq) }, `SELECT seq FROM attention_cursor WHERE id = 1`); err != nil {
		return 0, err
	}
	head, err := eventlog.Head(tx)
	if err != nil {
		return 0, err
	}
	if seq > head { // 账本被恢复到较早状态：从头来，宁可重算也不漏
		return 0, nil
	}
	return seq, nil
}

// reconcile 按事项现状重算它的投递：该有的补上，旧版本的作废，不该有的清掉。
// shortcut: 待决决策在事项被无关字段修改后会以新版本重发（旧版本作废）；要避免需记录"已确认"，真有骚扰再做。
func (a *Attention) reconcile(tx *store.Tx, pol policy.Policy, id string) error {
	if err := a.voidScheduled(tx, id); err != nil {
		return err
	}
	it, err := items.Get(tx, id)
	if isNotFound(err) {
		return a.drop(tx, id)
	}
	if err != nil {
		return err
	}
	ask := it.Ask
	if ask != nil && ask.Resolution != nil && answeredByPerson(ask.Resolution) {
		// 本人直接回答的那条投递记 acknowledged，而不是被随后的作废盖成 superseded
		if _, err := a.o.Queue.ConfirmAnswered(tx, id); err != nil {
			return err
		}
	}
	if it.Kind == domain.KindBrief && it.Phase == domain.PhaseDone {
		return a.briefDone(tx, pol, it)
	}
	if it.Phase == domain.PhaseDone && it.Kind != domain.KindBrief {
		if handled, err := a.taskDone(tx, pol, it); err != nil || handled {
			return err
		}
	}
	if ask != nil && ask.Resolution != nil && ask.Resolution.ResolvedVia == decisions.ViaAgent {
		return a.agentAnswered(tx, pol, it)
	}
	if it.Phase != domain.PhaseNeedsInput || ask == nil || ask.Resolution != nil {
		return a.drop(tx, id) // proposed 不产生投递（§6.14.2）；阶段离开则作废
	}
	ver, err := command.Version(tx, id)
	if err != nil {
		return err
	}
	if _, err := a.o.Queue.Supersede(tx, id, ver); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM attention_held WHERE item = ? AND version < ?`, id, ver); err != nil {
		return err
	}
	dr, ready, err := a.draft(tx, pol, it, ver)
	if err != nil || !ready {
		return err // 起草未返回前不投递；返回后 Drafts 会再重算这个事项
	}
	content := dr.content(it)
	content.WebURL = a.o.WebURL(id)
	h := held{
		id: fmt.Sprintf("decision:%s:%d", id, ver), item: id, class: domain.DeliveryDecision, priority: it.Priority, version: ver,
		content: content,
		exempt:  nearDeadline(pol, it, a.o.Now()),
	}
	return a.offer(tx, pol, h)
}

// briefDone 在简报写回完成（转 done）时为它产出一条 class=brief 的投递，路由与静默、日上限、静音都走 offer（§3.6、§7.4）。
// 一份简报只入队一次：已有该 id 的暂存或投递就什么都不做，所以简报之后的事项变化既不重发也不作废它。
func (a *Attention) briefDone(tx *store.Tx, pol policy.Policy, it domain.Item) error {
	id := delivery.BriefID(it.ID)
	if known, err := a.known(tx, id); err != nil || known {
		return err
	}
	if err := a.drop(tx, it.ID); err != nil { // 简报曾转 needs_input 问过本人：那条决策投递随完成作废
		return err
	}
	ver, err := command.Version(tx, it.ID)
	if err != nil {
		return err
	}
	return a.offer(tx, pol, held{id: id, item: it.ID, class: domain.DeliveryBrief, priority: it.Priority, version: ver,
		content: delivery.BriefContent(it, a.o.WebURL(it.ID))})
}

// agentAnswered：档位放开二档的 Agent 替本人答了问题（§14.3、裁决 2），推一条提醒说它替你答了什么，链接到可按次撤回的「Agent 改动」。
// 一次回答只推一次（id 带回答时刻）：之后的事项变化既不重推也不作废它；撤回后再答是新的一次。
func (a *Attention) agentAnswered(tx *store.Tx, pol policy.Policy, it domain.Item) error {
	r := it.Ask.Resolution
	id := fmt.Sprintf("agent-answer:%s:%d", it.ID, r.ResolvedAt.UnixNano())
	if known, err := a.known(tx, id); err != nil || known {
		return err
	}
	if err := a.drop(tx, it.ID); err != nil { // 原来那张等本人回答的卡随回答作废
		return err
	}
	ver, err := command.Version(tx, it.ID)
	if err != nil {
		return err
	}
	answer := r.Text
	for _, o := range it.Ask.Options {
		if o.ID == r.Choice {
			answer = o.Label
		}
	}
	return a.offer(tx, pol, held{id: id, item: it.ID, class: domain.DeliveryReminder, priority: it.Priority, version: ver,
		content: domain.DeliveryContent{Title: "Agent 替你答了：" + it.Title, Summary: it.Ask.Question + " → " + answer, WebURL: "/agent-changes"}})
}

// answeredByPerson：只有本人在 Web 给出了真实回答才算（渠道回答自己已经 Confirm 过）。
// 用白名单：Agent 替本人答（agent）、旧模型的原话代答（chat）、超时默认、授权单都不是本人确认。
func answeredByPerson(r *domain.Resolution) bool {
	return r.ResolvedVia == decisions.ViaWeb && (r.Choice != "" || r.Text != "")
}

// nearDeadline：本人开启了豁免，且事项的决策截止距 now 不超过 NearDeadline（已过也算）。
func nearDeadline(pol policy.Policy, it domain.Item, now time.Time) bool {
	return pol.Disturb.QuietExempt && it.Ask != nil && it.Ask.Deadline != nil && it.Ask.Deadline.Sub(now) <= NearDeadline
}

// drop 作废事项的全部未终结投递并清掉它被拦住的提醒。
func (a *Attention) drop(tx *store.Tx, id string) error {
	if _, err := a.o.Queue.Supersede(tx, id, delivery.AllVersions); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM attention_draft WHERE item = ?`, id); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM attention_held WHERE item = ?`, id)
	return err
}

// ---- 截止已过再提醒 ----

// remindOverdue：事项截止或决策 ask.deadline 已过仍未处理，且策略开启再提醒，则产出一条提醒。
// 内核不会替本人决定：决策保持 needs_input，问题永不超时代答。
// 提醒 id 含截止时间，每个截止只提醒一次；截止被改后是新的截止。
func (a *Attention) remindOverdue(tx *store.Tx, pol policy.Policy) error {
	if !pol.Disturb.RemindOnDeadline {
		return nil
	}
	now := a.o.Now()
	for _, ph := range []domain.Phase{domain.PhaseOpen, domain.PhaseWaiting, domain.PhaseNeedsInput} {
		list, err := items.List(tx, items.Filter{Phase: ph})
		if err != nil {
			return err
		}
		for _, it := range list {
			if d := it.Deadline; d != nil && !d.After(now) {
				if err := a.remind(tx, pol, it, "due", *d, "截止已过："+it.Title, "事项截止时间 "+d.UTC().Format(time.RFC3339)+" 已过"); err != nil {
					return err
				}
			}
			ask := it.Ask
			if ph != domain.PhaseNeedsInput || ask == nil || ask.Resolution != nil ||
				ask.Deadline == nil || ask.Deadline.After(now) {
				continue
			}
			if err := a.remind(tx, pol, it, "ask", *ask.Deadline, "决策仍待回答："+it.Title, ask.Question); err != nil {
				return err
			}
		}
	}
	return nil
}

// Remind 让其他模块的提醒（如跟进判断的"催"）走触达这个唯一出口：同样受静默与合并约束，
// 同一 (kind, 事项, due) 只提醒一次，所以重复调用不会重复提醒。在调用方的事务里运行。
func (a *Attention) Remind(tx *store.Tx, it domain.Item, kind string, due time.Time, title, summary string) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	return a.remind(tx, pol, it, kind, due, title, summary)
}

// RemindWith 同 Remind，并在提醒内容里给出本人可选的动作（如跟进的"已催""再等""放弃"）。
func (a *Attention) RemindWith(tx *store.Tx, it domain.Item, kind string, due time.Time, title, summary string, options []string) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	return a.remind(tx, pol, it, kind, due, title, summary, options...)
}

// RemindGroup 把一批事项合成一条提醒（如久未推进），挂在 anchor 事项上；同一 key 只提醒一次。
// 内容里的 webURL 指向批的入口，不是 anchor。在调用方的事务里运行。
func (a *Attention) RemindGroup(tx *store.Tx, anchor domain.Item, key, title, summary, webURL string) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	ver, err := command.Version(tx, anchor.ID)
	if err != nil {
		return err
	}
	return a.offer(tx, pol, held{
		id: "remind:" + key, item: anchor.ID, class: domain.DeliveryReminder, priority: anchor.Priority, version: ver,
		content: domain.DeliveryContent{Title: title, Summary: summary, WebURL: webURL},
	})
}

// AlertID 是规则 alert 出口为来源事件产出的提醒的投递 id：同一来源事件只提醒一次。
func AlertID(sourceEvent string) string { return "alert:" + sourceEvent }

// Alert 让规则 alert 出口的提醒走触达：没有事项，只记出处（origin）；受静默、日上限和静音约束（§7.4）。
// 同一来源事件重复调用不会重复提醒。在调用方的事务里运行。
func (a *Attention) Alert(tx *store.Tx, o domain.DeliveryOrigin, content domain.DeliveryContent) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	return a.offer(tx, pol, held{id: AlertID(o.SourceEvent), class: domain.DeliveryReminder, priority: domain.PriorityNormal,
		version: 1, content: content, origin: &o})
}

// Urgent 让 notify_urgent 规则产生的新建议即时提醒（§9.3）：投递 id 取建议事项 id，同一条建议只提醒一次；
// 与 Alert 一样不带事项（建议仍是 proposed，不计承诺），origin 记来源事件、规则与这条建议；受静默、日上限和静音约束。
// 内容的 Web 链接指向这条建议。在调用方的事务里运行。
func (a *Attention) Urgent(tx *store.Tx, o domain.DeliveryOrigin, content domain.DeliveryContent) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	content.WebURL = a.o.WebURL(o.Item)
	return a.offer(tx, pol, held{id: o.Item, class: domain.DeliveryReminder, priority: domain.PriorityNormal,
		version: 1, content: content, origin: &o})
}

func (a *Attention) remind(tx *store.Tx, pol policy.Policy, it domain.Item, kind string, due time.Time, title, summary string, options ...string) error {
	ver, err := command.Version(tx, it.ID)
	if err != nil {
		return err
	}
	return a.offer(tx, pol, held{
		id: fmt.Sprintf("remind:%s:%s:%d", kind, it.ID, due.UnixNano()), item: it.ID, class: domain.DeliveryReminder, priority: it.Priority, version: ver,
		content: domain.DeliveryContent{Title: title, Summary: summary, Options: options, WebURL: a.o.WebURL(it.ID)},
	})
}

// ---- 静默、合并、路由 ----

// held 是一条待投的提醒：已决定要投，只差放行时间。
type held struct {
	id, item  string
	class     domain.DeliveryClass
	priority  domain.Priority
	content   domain.DeliveryContent
	version   int64
	exempt    bool // 豁免静默
	mergeable bool
	release   time.Time
	origin    *domain.DeliveryOrigin // 没有事项的提醒（规则 alert 出口）的出处；有事项时为空
}

// offer 决定一条提醒现在投还是先拦住：按它所在行的节流属性，受静默的在静默中拦到静默结束；
// 低优先级提醒进合并窗口；到点提醒一律先暂存，由 flush 按批放行。
// 已经有同 id 的投递或暂存（包括已作废的）就什么都不做，所以重复调用不会重复提醒。
func (a *Attention) offer(tx *store.Tx, pol policy.Policy, h held) error {
	if known, err := a.known(tx, h.id); err != nil || known {
		return err
	}
	th := throttleOf(h.id)
	now := a.o.Now()
	h.release = now
	if end, ok := quietEnd(pol, now); ok && th.quiet && !h.exempt {
		h.release = end
	}
	if th.merge == mergeLow && h.class == domain.DeliveryReminder && h.priority == domain.PriorityLow && pol.Disturb.MergeWindowSec > 0 {
		h.mergeable = true
		window := now.Add(time.Duration(pol.Disturb.MergeWindowSec) * time.Second)
		var open int64 // 窗口内的后来者并入同一个窗口
		if err := scan(tx, func(r *store.Rows) error { return r.Scan(&open) },
			`SELECT COALESCE(MIN(release_at), 0) FROM attention_held WHERE mergeable = 1 AND release_at > ?`, now.UnixNano()); err != nil {
			return err
		}
		if open > 0 {
			window = time.Unix(0, open)
		}
		if window.After(h.release) {
			h.release = window
		}
	}
	if !h.release.After(now) && th.merge != mergeBatch {
		return a.deliver(tx, pol, []held{h})
	}
	content, err := json.Marshal(h.content)
	if err != nil {
		return err
	}
	origin := ""
	if h.origin != nil {
		b, err := json.Marshal(h.origin)
		if err != nil {
			return err
		}
		origin = string(b)
	}
	_, err = tx.Exec(`INSERT INTO attention_held (id, item, class, priority, content, version, exempt, mergeable, release_at, origin)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, h.id, h.item, string(h.class), string(h.priority), string(content), h.version,
		b2i(h.exempt), b2i(h.mergeable), h.release.UnixNano(), origin)
	return err
}

// known：这条提醒已暂存、已处理，或已有投递（含两层都推时 Agent 层那一条）。
func (a *Attention) known(tx *store.Tx, id string) (bool, error) {
	var n int64
	if err := scan(tx, func(r *store.Rows) error { return r.Scan(&n) }, `SELECT COUNT(*) FROM attention_held WHERE id = ?`, id); err != nil || n > 0 {
		return n > 0, err
	}
	if err := scan(tx, func(r *store.Rows) error { return r.Scan(&n) }, `SELECT COUNT(*) FROM attention_done WHERE id = ?`, id); err != nil || n > 0 {
		return n > 0, err
	}
	err := scan(tx, func(r *store.Rows) error { return r.Scan(&n) }, `SELECT COUNT(*) FROM delivery_delivery WHERE id IN (?, ?)`, id, id+AgentSuffix)
	return n > 0, err
}

// flush 放行到期的暂存提醒。放行时按当时的策略重新判静默（策略可能在等待期间变了）；
// 按各行的合并方式：到点提醒成批放行；同批到期的低优先级提醒、静默结束时一起放行的推送，去处相同的两条以上合成一条摘要，一条则原样投。
func (a *Attention) flush(tx *store.Tx, pol policy.Policy) error {
	now := a.o.Now()
	var due []held
	err := scan(tx, func(r *store.Rows) error {
		var (
			h                  held
			class, prio, body  string
			origin             string
			exempt, merge, rel int64
		)
		if err := r.Scan(&h.id, &h.item, &class, &prio, &body, &h.version, &exempt, &merge, &rel, &origin); err != nil {
			return err
		}
		if origin != "" {
			if err := json.Unmarshal([]byte(origin), &h.origin); err != nil {
				return fmt.Errorf("held %s origin: %w", h.id, err)
			}
		}
		h.class, h.priority, h.exempt, h.mergeable, h.release = domain.DeliveryClass(class), domain.Priority(prio), exempt == 1, merge == 1, time.Unix(0, rel)
		due = append(due, h)
		return json.Unmarshal([]byte(body), &due[len(due)-1].content)
	}, `SELECT id, item, class, priority, content, version, exempt, mergeable, release_at, origin FROM attention_held WHERE release_at <= ? OR class = 'decision' ORDER BY release_at, rowid`, now.UnixNano())
	if err != nil {
		return err
	}
	var go_, merged, quieted, batch []held
	for _, h := range due {
		th := throttleOf(h.id)
		if h.class == domain.DeliveryDecision { // 豁免按当前时间与策略重算：静默期内决策可能刚变得临近截止
			it, err := items.Get(tx, h.item)
			if err != nil && !isNotFound(err) {
				return err
			}
			h.exempt = err == nil && nearDeadline(pol, it, now)
			if !h.exempt && h.release.After(now) {
				continue
			}
		}
		if end, ok := quietEnd(pol, now); ok && th.quiet && !h.exempt {
			if _, err := tx.Exec(`UPDATE attention_held SET release_at = ? WHERE id = ?`, end.UnixNano(), h.id); err != nil {
				return err
			}
			continue
		}
		switch {
		case th.merge == mergeBatch:
			batch = append(batch, h) // 成批放行时再删：到了每日上限的要留着
			continue
		case h.mergeable:
			merged = append(merged, h)
		case th.merge == mergeQuiet:
			quieted = append(quieted, h)
		default:
			go_ = append(go_, h)
		}
		if _, err := tx.Exec(`DELETE FROM attention_held WHERE id = ?`, h.id); err != nil {
			return err
		}
	}
	if err := a.releaseScheduled(tx, pol, batch, now); err != nil {
		return err
	}
	for _, h := range go_ {
		if err := a.deliver(tx, pol, []held{h}); err != nil {
			return err
		}
	}
	for _, g := range [][]held{merged, quieted} {
		groups, err := a.byTargets(tx, pol, g)
		if err != nil {
			return err
		}
		for _, hs := range groups {
			if err := a.deliver(tx, pol, hs); err != nil {
				return err
			}
		}
	}
	return nil
}

// byTargets 把待合并的提醒按两层去处分组（只合并去处相同的成员），组的顺序按首个成员。
func (a *Attention) byTargets(tx *store.Tx, pol policy.Policy, hs []held) ([][]held, error) {
	var order []string
	groups := map[string][]held{}
	for _, h := range hs {
		r, err := rowOf(tx, h)
		if err != nil {
			return nil, err
		}
		l, err := resolve(tx, pol, r, h.priority)
		if err != nil {
			return nil, err
		}
		k := l.user + "|" + l.agent
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], h)
	}
	out := make([][]held, len(order))
	for i, k := range order {
		out[i] = groups[k]
	}
	return out, nil
}

// deliver 把一条或一批（合并，去处相同）提醒写成投递。一批多条时合成摘要；摘要不属于任何单个事项。
func (a *Attention) deliver(tx *store.Tx, pol policy.Policy, hs []held) error {
	if len(hs) > 1 { // 被静音的成员不并入摘要：各自走 Web 未读
		var rest []held
		for _, h := range hs {
			t, err := a.target(tx, h)
			if err != nil {
				return err
			}
			if a.o.Queue.Muted(pol, orElse(h.item, originItem(h.origin)), t) {
				if err := a.deliver(tx, pol, []held{h}); err != nil {
					return err
				}
				continue
			}
			rest = append(rest, h)
		}
		if len(rest) != len(hs) {
			if len(rest) == 0 {
				return nil
			}
			return a.deliver(tx, pol, rest)
		}
	}
	d := domain.Delivery{ID: hs[0].id, Item: hs[0].item, Class: hs[0].class, Version: hs[0].version, Content: hs[0].content, Origin: hs[0].origin}
	if len(hs) > 1 {
		titles := make([]string, len(hs))
		ids := make([]string, len(hs))
		class := domain.DeliveryHealth
		for i, h := range hs {
			titles[i], ids[i] = h.content.Title, h.id
			if h.class != domain.DeliveryHealth { // 混有提醒类时按提醒类计日上限
				class = domain.DeliveryReminder
			}
		}
		sort.Strings(ids)
		sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
		title := fmt.Sprintf("%d 条低优先级提醒", len(hs))
		if throttleOf(hs[0].id).merge == mergeQuiet {
			title = fmt.Sprintf("安静时段攒下的 %d 条推送", len(hs))
		} else {
			class = hs[0].class
		}
		d = domain.Delivery{ID: "digest:" + hex.EncodeToString(sum[:8]), Item: DigestItem, Class: class, Version: 1,
			Content: domain.DeliveryContent{Title: title, Summary: strings.Join(titles, "；")}}
	}
	r, err := rowOf(tx, hs[0])
	if err != nil {
		return err
	}
	t, err := a.target(tx, hs[0])
	if err != nil {
		return err
	}
	if len(hs) > 1 {
		t = delivery.Target{} // 摘要不属于任何事项，成员已逐个对照过静音
		r.task, r.floor = nil, true
	}
	if _, err := a.emit(tx, pol, r, hs[0].priority, d, t); err != nil {
		return err
	}
	for _, h := range hs { // 合并成员也算已提醒：不再被重新暂存，摘要 id 与成员集绑定而非与某一条绑定
		if _, err := tx.Exec(`INSERT OR IGNORE INTO attention_done (id) VALUES (?)`, h.id); err != nil {
			return err
		}
	}
	return nil
}

// target 取一条提醒对应事项的项目与来源，供日上限与静音对照；事项已不存在时只剩事项 id。
func (a *Attention) target(tx *store.Tx, h held) (delivery.Target, error) {
	t := delivery.Target{Exempt: h.exempt}
	id := h.item
	if h.origin != nil { // 没有事项：来源取出处；紧急建议提醒另按它指向的建议对照项目与事项静音
		t.Source = h.origin.Source
		if h.origin.Item == "" {
			return t, nil
		}
		id = h.origin.Item
	}
	it, err := items.Get(tx, id)
	if isNotFound(err) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	t.Project = it.Project
	if it.Origin != nil && h.origin == nil {
		t.Source = it.Origin.Source
	}
	return t, nil
}

func originItem(o *domain.DeliveryOrigin) string {
	if o == nil {
		return ""
	}
	return o.Item
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// quietEnd 返回 now 所处静默时段的结束时刻；不在静默中（或未设静默）返回 false。
func quietEnd(pol policy.Policy, now time.Time) (time.Time, bool) {
	q := pol.Disturb.Quiet
	if q == nil {
		return time.Time{}, false
	}
	loc, err := time.LoadLocation(q.Zone) // 策略写入时已校验
	if err != nil {
		return time.Time{}, false
	}
	start, _ := policy.ParseClock(q.Start)
	end, _ := policy.ParseClock(q.End)
	local := now.In(loc)
	m := local.Hour()*60 + local.Minute()
	in := start < end && m >= start && m < end || start > end && (m >= start || m < end)
	if !in {
		return time.Time{}, false
	}
	at := time.Date(local.Year(), local.Month(), local.Day(), end/60, end%60, 0, 0, loc)
	if !at.After(local) {
		at = at.AddDate(0, 0, 1)
	}
	return at, true
}

func scan(tx *store.Tx, each func(*store.Rows) error, sql string, args ...any) error {
	r, err := tx.Query(sql, args...)
	if err != nil {
		return err
	}
	defer r.Close()
	for r.Next() {
		if err := each(r); err != nil {
			return err
		}
	}
	return r.Err()
}

func isNotFound(err error) bool {
	de, ok := err.(*domain.Error)
	return ok && de.Code == domain.CodeNotFound
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
