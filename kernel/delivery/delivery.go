// Package delivery 是投递队列（设计 §6.9、§10.2、§10.3）：触达产出投递，消费方来取。
// 状态机：pending → claimed（租约）→ delivered（ext 回报送达）→ acknowledged（本人确认）；
// 失败按退避重试（默认 1、3、7、15、30 分钟共 5 次），超限或无人领取转 fallback_web（Web 未读）；
// 事项变化后旧版本转 superseded。注入只记 injections，不改状态。
// 本包只管投递表，在调用方事务内读写；内核不主动推送，何时产出投递由触达决定。
package delivery

import (
	"encoding/json"
	"fmt"
	"math"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 路由约定：inject 由会话开始/消息前注入消费，wake 由网关插件领取，渠道路由用 ChannelRoute。
const (
	RouteInject = "inject"
	RouteWake   = "wake"
)

// ChannelRoute 是渠道 ext 的路由；渠道由 ext 密钥确定（§10.3）。
func ChannelRoute(name string) string { return "channel:" + name }

// AllVersions 传给 Supersede 表示作废该事项的所有未终结投递。
const AllVersions = math.MaxInt64

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "delivery", Migrations: []store.Migration{{
	Version: 1, Name: "create-delivery",
	SQL: `CREATE TABLE delivery_delivery (
	id TEXT PRIMARY KEY,
	item TEXT NOT NULL,
	class TEXT NOT NULL,
	route TEXT NOT NULL,
	content TEXT NOT NULL,
	version INTEGER NOT NULL,
	state TEXT NOT NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	claims INTEGER NOT NULL DEFAULT 0,
	next_attempt_at INTEGER NOT NULL DEFAULT 0,
	lease_until INTEGER NOT NULL DEFAULT 0,
	external_ref TEXT NOT NULL DEFAULT '',
	last_error TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	read_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX delivery_delivery_route ON delivery_delivery(route, state);
CREATE INDEX delivery_delivery_item ON delivery_delivery(item);
CREATE TABLE delivery_injection (
	delivery_id TEXT NOT NULL REFERENCES delivery_delivery(id),
	session TEXT NOT NULL,
	at INTEGER NOT NULL,
	PRIMARY KEY (delivery_id, session)
) WITHOUT ROWID;`,
}}}

// DefaultBackoff 是失败退避：第 n 次失败后 Backoff[n-1] 再可领；第 len+1 次失败转 fallback_web。
var DefaultBackoff = []time.Duration{time.Minute, 3 * time.Minute, 7 * time.Minute, 15 * time.Minute, 30 * time.Minute}

// Options 配置队列；零值字段取默认。
type Options struct {
	Backoff        []time.Duration // 默认 DefaultBackoff；可调小用于验收
	Lease          time.Duration   // 领取租约，默认 60 秒
	UnclaimedAfter time.Duration   // 无人领取多久转 fallback_web，默认 30 分钟
	Now            func() time.Time
}

// Queue 是投递队列；无状态，所有状态在账本里。
type Queue struct{ o Options }

// New 返回队列。
func New(o Options) *Queue {
	if o.Backoff == nil {
		o.Backoff = DefaultBackoff
	}
	if o.Lease <= 0 {
		o.Lease = time.Minute
	}
	if o.UnclaimedAfter <= 0 {
		o.UnclaimedAfter = 30 * time.Minute
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Queue{o}
}

// Result 区分一次回报的结果；Stale 表示版本已作废，调用方应回 409，ext 不应再发。
type Result int

const (
	Applied   Result = iota // 状态已推进
	Duplicate               // 幂等重复，返回首次结果，无变更
	Stale                   // 版本已作废或投递已 superseded
)

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request")
}
func conflict(why string) error {
	return domain.NewError(domain.CodeConflict, why, "re-read the delivery state")
}
func notFound(id string) error {
	return domain.NewError(domain.CodeNotFound, "delivery "+id+" not found", "check the delivery id")
}

const cols = "id, item, class, route, content, version, state, attempts, next_attempt_at, external_ref"

func ns(t time.Time) int64 { return t.UnixNano() }

func query(tx *store.Tx, each func(*store.Rows) error, sql string, args ...any) error {
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

func (q *Queue) list(tx *store.Tx, where string, args ...any) ([]domain.Delivery, error) {
	var out []domain.Delivery
	err := query(tx, func(r *store.Rows) error {
		var (
			d                  domain.Delivery
			class, content, st string
			attempts, next     int64
		)
		if err := r.Scan(&d.ID, &d.Item, &class, &d.Route, &content, &d.Version, &st, &attempts, &next, &d.ExternalRef); err != nil {
			return err
		}
		d.Class, d.State, d.Attempts = domain.DeliveryClass(class), domain.DeliveryState(st), int(attempts)
		if err := json.Unmarshal([]byte(content), &d.Content); err != nil {
			return fmt.Errorf("delivery %s content: %w", d.ID, err)
		}
		if next > 0 && d.State == domain.DeliveryPending {
			t := time.Unix(0, next).UTC()
			d.NextAttemptAt = &t
		}
		out = append(out, d)
		return nil
	}, "SELECT "+cols+" FROM delivery_delivery "+where, args...)
	if err != nil {
		return nil, err
	}
	for i := range out {
		err := query(tx, func(r *store.Rows) error {
			var in domain.Injection
			var at int64
			if err := r.Scan(&in.Session, &at); err != nil {
				return err
			}
			in.At = time.Unix(0, at).UTC()
			out[i].Injections = append(out[i].Injections, in)
			return nil
		}, "SELECT session, at FROM delivery_injection WHERE delivery_id = ? ORDER BY at, session", out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// Get 按 id 读取投递；不存在返回 not_found。
func (q *Queue) Get(tx *store.Tx, id string) (domain.Delivery, error) {
	ds, err := q.list(tx, "WHERE id = ?", id)
	if err != nil {
		return domain.Delivery{}, err
	}
	if len(ds) == 0 {
		return domain.Delivery{}, notFound(id)
	}
	return ds[0], nil
}

// Create 以 d.ID 为幂等键入队：已存在则原样返回首次结果（created=false），不覆盖。
// 新投递立即可领；State、Attempts 等运行态字段由队列设置。
func (q *Queue) Create(tx *store.Tx, d domain.Delivery) (domain.Delivery, bool, error) {
	switch {
	case d.ID == "" || d.Item == "" || d.Route == "":
		return d, false, invalid("delivery id, item and route are required")
	case d.Version < 1:
		return d, false, invalid("delivery version must be >= 1")
	case d.Content.Title == "":
		return d, false, invalid("delivery content title is required")
	}
	switch d.Class {
	case domain.DeliveryDecision, domain.DeliveryBrief, domain.DeliveryReminder, domain.DeliveryHealth, domain.DeliveryWake:
	default:
		return d, false, invalid("delivery class " + string(d.Class))
	}
	if old, err := q.Get(tx, d.ID); err == nil {
		return old, false, nil
	} else if e, ok := err.(*domain.Error); !ok || e.Code != domain.CodeNotFound {
		return d, false, err
	}
	content, err := json.Marshal(d.Content)
	if err != nil {
		return d, false, err
	}
	now := ns(q.o.Now())
	if _, err := tx.Exec(`INSERT INTO delivery_delivery
		(id, item, class, route, content, version, state, next_attempt_at, created_at)
		VALUES (?, ?, ?, ?, ?, ?, 'pending', ?, ?)`,
		d.ID, d.Item, string(d.Class), d.Route, string(content), d.Version, now, now); err != nil {
		return d, false, err
	}
	got, err := q.Get(tx, d.ID)
	return got, true, err
}

// Claim 领取 route 上下一条可领投递并加租约：pending 且到了重试时间，或租约已到期未回报的 claimed。
// 同一租约内其他消费者领不到；没有可领返回 nil。注入路由不经领取，见 Injectable。
func (q *Queue) Claim(tx *store.Tx, route string) (*domain.Delivery, error) {
	if route == "" || route == RouteInject {
		return nil, invalid("route " + route + " cannot be claimed")
	}
	now := q.o.Now()
	var id string
	err := query(tx, func(r *store.Rows) error { return r.Scan(&id) }, `SELECT id FROM delivery_delivery
		WHERE route = ? AND ((state = 'pending' AND next_attempt_at <= ?) OR (state = 'claimed' AND lease_until <= ?))
		ORDER BY created_at, rowid LIMIT 1`, route, ns(now), ns(now))
	if err != nil || id == "" {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE delivery_delivery SET state = 'claimed', lease_until = ?, claims = claims + 1 WHERE id = ?`,
		ns(now.Add(q.o.Lease)), id); err != nil {
		return nil, err
	}
	d, err := q.Get(tx, id)
	return &d, err
}

// check 读取投递并判断回报是否针对当前版本；Stale 时不改任何东西。
func (q *Queue) check(tx *store.Tx, id string, version int64) (domain.Delivery, bool, error) {
	d, err := q.Get(tx, id)
	if err != nil {
		return d, false, err
	}
	return d, d.State == domain.DeliverySuperseded || d.Version != version, nil
}

func (q *Queue) setState(tx *store.Tx, d *domain.Delivery, to domain.DeliveryState, set string, args ...any) error {
	if !d.State.CanTransition(to) {
		return conflict(fmt.Sprintf("delivery %s cannot go %s → %s", d.ID, d.State, to))
	}
	args = append(args, string(to), d.ID)
	if _, err := tx.Exec("UPDATE delivery_delivery SET "+set+"state = ? WHERE id = ?", args...); err != nil {
		return err
	}
	d.State = to
	return nil
}

// Ack 是 ext 的送达回报（claimed → delivered，记 external_ref）。同版本同 ref 重复回报幂等；
// 版本已作废返回 Stale 且不改状态，ext 不应再发。
func (q *Queue) Ack(tx *store.Tx, id string, version int64, externalRef string) (domain.Delivery, Result, error) {
	d, stale, err := q.check(tx, id, version)
	if err != nil || stale {
		return d, Stale, err
	}
	if externalRef == "" {
		return d, Applied, invalid("external_ref is required")
	}
	switch d.State {
	case domain.DeliveryDelivered, domain.DeliveryAcknowledged:
		if d.ExternalRef != externalRef {
			return d, Applied, conflict("delivery already delivered with a different external_ref")
		}
		return d, Duplicate, nil
	}
	if err := q.setState(tx, &d, domain.DeliveryDelivered, "external_ref = ?, lease_until = 0, ", externalRef); err != nil {
		return d, Applied, err
	}
	return q.reload(tx, id)
}

func (q *Queue) reload(tx *store.Tx, id string) (domain.Delivery, Result, error) {
	d, err := q.Get(tx, id)
	return d, Applied, err
}

// Fail 是 ext 的失败回报：第 n 次失败按 Backoff[n-1] 退避后重新可领；超过退避表长度转 fallback_web。
// 版本已作废返回 Stale；已在退避中的重复回报返回 Duplicate。
func (q *Queue) Fail(tx *store.Tx, id string, version int64, reason string) (domain.Delivery, Result, error) {
	d, stale, err := q.check(tx, id, version)
	if err != nil || stale {
		return d, Stale, err
	}
	switch d.State {
	case domain.DeliveryPending, domain.DeliveryFallbackWeb:
		return d, Duplicate, nil
	case domain.DeliveryClaimed:
	default:
		return d, Applied, conflict(fmt.Sprintf("delivery %s is %s, cannot fail", id, d.State))
	}
	attempts := d.Attempts + 1
	if attempts > len(q.o.Backoff) {
		err = q.setState(tx, &d, domain.DeliveryFallbackWeb, "attempts = ?, last_error = ?, lease_until = 0, next_attempt_at = 0, ", attempts, reason)
	} else {
		next := ns(q.o.Now().Add(q.o.Backoff[attempts-1]))
		err = q.setState(tx, &d, domain.DeliveryPending, "attempts = ?, last_error = ?, lease_until = 0, next_attempt_at = ?, ", attempts, reason, next)
	}
	if err != nil {
		return d, Applied, err
	}
	return q.reload(tx, id)
}

// Confirm 是本人确认（Web 点击、聊天代答、渠道回答）：未送达的投递按 pending→claimed→delivered 补齐后转 acknowledged。
// 重复确认幂等；版本已作废返回 Stale；fallback_web 是终态，其已读用 MarkRead。
func (q *Queue) Confirm(tx *store.Tx, id string, version int64) (domain.Delivery, Result, error) {
	d, stale, err := q.check(tx, id, version)
	if err != nil || stale {
		return d, Stale, err
	}
	switch d.State {
	case domain.DeliveryAcknowledged:
		return d, Duplicate, nil
	case domain.DeliveryFallbackWeb:
		return d, Applied, conflict("delivery fell back to web; use MarkRead")
	}
	if _, err := tx.Exec("UPDATE delivery_delivery SET state = 'acknowledged', lease_until = 0 WHERE id = ?", id); err != nil {
		return d, Applied, err
	}
	return q.reload(tx, id)
}

// Supersede 作废该事项中版本小于 below 的未终结投递（事项变化的命令在同一事务调用）。
func (q *Queue) Supersede(tx *store.Tx, item string, below int64) (int64, error) {
	return tx.Exec(`UPDATE delivery_delivery SET state = 'superseded', lease_until = 0
		WHERE item = ? AND version < ? AND state IN ('pending', 'claimed', 'delivered')`, item, below)
}

// Sweep 把无人领取的投递（创建超过 UnclaimedAfter 仍从未被领取）转 fallback_web，返回条数。
func (q *Queue) Sweep(tx *store.Tx) (int64, error) {
	return tx.Exec(`UPDATE delivery_delivery SET state = 'fallback_web', next_attempt_at = 0
		WHERE state = 'pending' AND claims = 0 AND created_at <= ?`, ns(q.o.Now().Add(-q.o.UnclaimedAfter)))
}

// Inject 记录注入到某会话，只写 injections、不改状态；同一会话重复注入幂等。已作废或已确认的投递不可再注入。
func (q *Queue) Inject(tx *store.Tx, id, session string) (domain.Delivery, error) {
	if session == "" {
		return domain.Delivery{}, invalid("session is required")
	}
	d, err := q.Get(tx, id)
	if err != nil {
		return d, err
	}
	if d.State == domain.DeliverySuperseded || d.State == domain.DeliveryAcknowledged {
		return d, conflict(fmt.Sprintf("delivery %s is %s, cannot inject", id, d.State))
	}
	if _, err := tx.Exec("INSERT OR IGNORE INTO delivery_injection (delivery_id, session, at) VALUES (?, ?, ?)",
		id, session, ns(q.o.Now())); err != nil {
		return d, err
	}
	return q.Get(tx, id)
}

// Injectable 返回注入路由上尚未确认或作废的投递，供 apa context 带上。
func (q *Queue) Injectable(tx *store.Tx) ([]domain.Delivery, error) {
	return q.list(tx, "WHERE route = ? AND state IN ('pending', 'claimed', 'delivered') ORDER BY created_at, rowid", RouteInject)
}

// MarkRead 记录 fallback_web 投递已在 Web 读过；幂等。
func (q *Queue) MarkRead(tx *store.Tx, id string) error {
	d, err := q.Get(tx, id)
	if err != nil {
		return err
	}
	if d.State != domain.DeliveryFallbackWeb {
		return conflict(fmt.Sprintf("delivery %s is %s, not an unread web item", id, d.State))
	}
	_, err = tx.Exec("UPDATE delivery_delivery SET read_at = ? WHERE id = ? AND read_at = 0", ns(q.o.Now()), id)
	return err
}

// Unread 返回 Web 未读：退回 Web 且未读过的投递，按创建顺序。
func (q *Queue) Unread(tx *store.Tx) ([]domain.Delivery, error) {
	return q.list(tx, "WHERE state = 'fallback_web' AND read_at = 0 ORDER BY created_at, rowid")
}
