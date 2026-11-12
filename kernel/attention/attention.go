// Package attention 是触达（设计 §7.4、§6.14、§11.6）：各模块对外提醒的唯一出口。
// 它读账本事件与事项现状，按静默、合并、阈值（优先级路由）产出投递，自身不发送，由消费方向投递队列领取。
//
// 工作方式是电平触发：事项一变（Sync）就按事项现状重算它该有什么投递——proposed 与初筛未返回的不产出，
// 待决的决策产出一条（版本取事项版本），事项变化后旧版本投递作废（Supersede）；时间相关的（截止已过再提醒、
// 静默结束补发、合并窗口到期、无人领取退回 Web 未读）由一个周期 Timer 驱动的 Tick 处理。
// 配了 Drafter 且策略配了模型端点时，决策投递先经 decision-card 包起草卡片文案（及可选的投递阈值判定），见 draft.go。
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

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
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
	// 每个待决决策一行：哪个版本起草到了哪一步（pending 未返回时不投递）、卡片文案与阈值判定。
	SQL: `CREATE TABLE attention_draft (
	item TEXT PRIMARY KEY,
	version INTEGER NOT NULL,
	state TEXT NOT NULL,
	card TEXT NOT NULL DEFAULT '',
	verdict TEXT NOT NULL DEFAULT ''
) WITHOUT ROWID;`,
}}}

// Options 配置触达；Queue 必填。
type Options struct {
	Queue     *delivery.Queue
	Now       func() time.Time
	TickEvery time.Duration             // 周期 Timer 间隔，默认 30 秒
	WebURL    func(item string) string  // 投递里的 Web 链接，默认 /items/<id>
	FormURL   func(token string) string // 投递里的一次性表单链接，默认 /forms/<令牌>
	// Drafter 起草决策卡片文案与做投递阈值判定；为空时投递内容与路由只按规则。
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
		o.Now = time.Now
	}
	if o.TickEvery <= 0 {
		o.TickEvery = 30 * time.Second
	}
	if o.WebURL == nil {
		o.WebURL = func(id string) string { return "/items/" + id }
	}
	if o.FormURL == nil {
		o.FormURL = func(token string) string { return "/forms/" + token }
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

// Tick 做所有与时间有关的事：追赶事件、截止已过再提醒、放行到期的暂存提醒、清扫无人领取的投递。
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
		es, err := eventlog.Read(tx, after, 500)
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
	if it.Phase != domain.PhaseNeedsInput || ask == nil || ask.Resolution != nil || ask.ScreeningPending {
		return a.drop(tx, id) // proposed 不产生投递（§6.14.2）；初筛返回前决策不投递（§14.4）；阶段离开则作废
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
	webOnly := authz.WebOnly(pol, authz.Object{Kind: string(it.Kind), Tags: it.Tags, Project: it.Project, Category: ask.RiskClass})
	content := dr.content(it, webOnly)
	content.WebURL = a.o.WebURL(id)
	h := held{
		id: fmt.Sprintf("decision:%s:%d", id, ver), item: id, class: domain.DeliveryDecision, priority: it.Priority, version: ver,
		content: content,
		exempt:  nearDeadline(pol, it, a.o.Now()),
	}
	if dr.Verdict == VerdictDigest && pol.Disturb.MergeWindowSec > 0 {
		// 带选项的决策不并入摘要（§7.4）：推迟到合并窗口结束后单独投递。
		h.notBefore = a.o.Now().Add(time.Duration(pol.Disturb.MergeWindowSec) * time.Second)
	}
	return a.offer(tx, pol, h)
}

// answeredByPerson：只有本人在 Web 或聊天里给出了真实回答才算（渠道回答自己已经 Confirm 过）。
// 用白名单：超时默认值、授权单代答、relocate 等系统收口都不是本人确认。
func answeredByPerson(r *domain.Resolution) bool {
	return (r.ResolvedVia == decisions.ViaWeb || r.ResolvedVia == decisions.ViaChat) && (r.Choice != "" || r.Text != "")
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
// 内核不会替本人决定：没有默认值的决策保持 needs_input（有默认值的由决策模块到期决定，这里不抢）。
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
			if ph != domain.PhaseNeedsInput || ask == nil || ask.Resolution != nil || ask.ScreeningPending || ask.DefaultOnTimeout != "" ||
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

func (a *Attention) remind(tx *store.Tx, pol policy.Policy, it domain.Item, kind string, due time.Time, title, summary string) error {
	ver, err := command.Version(tx, it.ID)
	if err != nil {
		return err
	}
	return a.offer(tx, pol, held{
		id: fmt.Sprintf("remind:%s:%s:%d", kind, it.ID, due.UnixNano()), item: it.ID, class: domain.DeliveryReminder, priority: it.Priority, version: ver,
		content: domain.DeliveryContent{Title: title, Summary: summary, WebURL: a.o.WebURL(it.ID)},
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
	notBefore time.Time // 阈值判定为并入摘要时的最早放行时刻
}

// offer 决定一条提醒现在投还是先拦住：静默中拦到静默结束；低优先级提醒进合并窗口。
// 已经有同 id 的投递或暂存（包括已作废的）就什么都不做，所以重复调用不会重复提醒。
func (a *Attention) offer(tx *store.Tx, pol policy.Policy, h held) error {
	if known, err := a.known(tx, h.id); err != nil || known {
		return err
	}
	now := a.o.Now()
	h.release = now
	if end, ok := quietEnd(pol, now); ok && !h.exempt {
		h.release = end
	}
	if h.class == domain.DeliveryReminder && h.priority == domain.PriorityLow && pol.Disturb.MergeWindowSec > 0 {
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
	if h.notBefore.After(h.release) {
		h.release = h.notBefore
	}
	if !h.release.After(now) {
		return a.deliver(tx, pol, []held{h})
	}
	content, err := json.Marshal(h.content)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO attention_held (id, item, class, priority, content, version, exempt, mergeable, release_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, h.id, h.item, string(h.class), string(h.priority), string(content), h.version,
		b2i(h.exempt), b2i(h.mergeable), h.release.UnixNano())
	return err
}

func (a *Attention) known(tx *store.Tx, id string) (bool, error) {
	var n int64
	if err := scan(tx, func(r *store.Rows) error { return r.Scan(&n) }, `SELECT COUNT(*) FROM attention_held WHERE id = ?`, id); err != nil || n > 0 {
		return n > 0, err
	}
	if err := scan(tx, func(r *store.Rows) error { return r.Scan(&n) }, `SELECT COUNT(*) FROM attention_done WHERE id = ?`, id); err != nil || n > 0 {
		return n > 0, err
	}
	if _, err := a.o.Queue.Get(tx, id); err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// flush 放行到期的暂存提醒。放行时按当时的策略重新判静默（策略可能在等待期间变了）；
// 同批到期的可合并提醒两条以上合成一条摘要，一条则原样投。
func (a *Attention) flush(tx *store.Tx, pol policy.Policy) error {
	now := a.o.Now()
	var due []held
	err := scan(tx, func(r *store.Rows) error {
		var (
			h                  held
			class, prio, body  string
			exempt, merge, rel int64
		)
		if err := r.Scan(&h.id, &h.item, &class, &prio, &body, &h.version, &exempt, &merge, &rel); err != nil {
			return err
		}
		h.class, h.priority, h.exempt, h.mergeable, h.release = domain.DeliveryClass(class), domain.Priority(prio), exempt == 1, merge == 1, time.Unix(0, rel)
		due = append(due, h)
		return json.Unmarshal([]byte(body), &due[len(due)-1].content)
	}, `SELECT id, item, class, priority, content, version, exempt, mergeable, release_at FROM attention_held WHERE release_at <= ? OR class = 'decision' ORDER BY release_at, rowid`, now.UnixNano())
	if err != nil {
		return err
	}
	var go_, merged []held
	for _, h := range due {
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
		if end, ok := quietEnd(pol, now); ok && !h.exempt {
			if _, err := tx.Exec(`UPDATE attention_held SET release_at = ? WHERE id = ?`, end.UnixNano(), h.id); err != nil {
				return err
			}
			continue
		}
		if h.mergeable {
			merged = append(merged, h)
		} else {
			go_ = append(go_, h)
		}
		if _, err := tx.Exec(`DELETE FROM attention_held WHERE id = ?`, h.id); err != nil {
			return err
		}
	}
	for _, h := range go_ {
		if err := a.deliver(tx, pol, []held{h}); err != nil {
			return err
		}
	}
	if len(merged) > 0 {
		return a.deliver(tx, pol, merged)
	}
	return nil
}

// deliver 把一条或一批（合并）提醒写成投递。一批多条时合成摘要；摘要不属于任何单个事项。
func (a *Attention) deliver(tx *store.Tx, pol policy.Policy, hs []held) error {
	if len(hs) > 1 { // 被静音的成员不并入摘要：各自走 Web 未读
		var rest []held
		for _, h := range hs {
			t, err := a.target(tx, h)
			if err != nil {
				return err
			}
			if a.o.Queue.Muted(pol, h.item, t) {
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
	d := domain.Delivery{ID: hs[0].id, Item: hs[0].item, Class: hs[0].class, Version: hs[0].version, Content: hs[0].content}
	if len(hs) > 1 {
		titles := make([]string, len(hs))
		for i, h := range hs {
			titles[i] = h.content.Title
		}
		ids := make([]string, len(hs))
		for i, h := range hs {
			ids[i] = h.id
		}
		sort.Strings(ids)
		sum := sha256.Sum256([]byte(strings.Join(ids, "\n")))
		d.ID, d.Item, d.Version = "digest:"+hex.EncodeToString(sum[:8]), DigestItem, 1
		d.Content = domain.DeliveryContent{Title: fmt.Sprintf("%d 条低优先级提醒", len(hs)), Summary: strings.Join(titles, "；")}
	}
	d.Route = a.route(pol, hs[0].class, hs[0].priority)
	if d.Class == domain.DeliveryDecision {
		v, err := a.verdict(tx, d.Item, d.Version)
		if err != nil {
			return err
		}
		if v == VerdictWebUnread { // 阈值判定只能降低打扰：留在 Web 未读
			d.Route = delivery.RouteWeb
		}
	}
	if err := a.attachForm(tx, pol, &d); err != nil {
		return err
	}
	t, err := a.target(tx, hs[0])
	if err != nil {
		return err
	}
	if len(hs) > 1 {
		t = delivery.Target{} // 摘要不属于任何事项，成员已逐个对照过静音
	}
	if _, _, err := a.o.Queue.CreateGated(tx, pol, d, t); err != nil {
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
	it, err := items.Get(tx, h.item)
	if isNotFound(err) {
		return t, nil
	}
	if err != nil {
		return t, err
	}
	t.Project = it.Project
	if it.Origin != nil {
		t.Source = it.Origin.Source
	}
	return t, nil
}

// attachForm 在写入决策投递时发放一次性表单链接（§10.3）。放在这里而不是暂存时，令牌只为真正写入的投递发放。
func (a *Attention) attachForm(tx *store.Tx, pol policy.Policy, d *domain.Delivery) error {
	if d.Class != domain.DeliveryDecision {
		return nil
	}
	it, err := items.Get(tx, d.Item)
	if isNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	token, err := channel.IssueForm(tx, pol, it, d.Route, d.Version, a.o.Now())
	if err != nil || token == "" {
		return err
	}
	d.Content.FormURL = a.o.FormURL(token)
	return nil
}

// route 是阈值：「类别:优先级」优先于「类别」；策略没给就退回 Web 未读。
func (a *Attention) route(pol policy.Policy, class domain.DeliveryClass, prio domain.Priority) string {
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
