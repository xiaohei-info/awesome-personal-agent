// Package clock 是内核时钟的 Timer 部分（设计 §6.8、§7.7）：
// Add/Cancel 在调用方事务内写 Timer；单循环 Tick/Run 扫描到期，向注册的处理器交回
// actor=system、via=clock 的系统命令。处理器与 Timer 状态推进在同一事务，失败则整体回滚、下次重试。
// 周期定时错过只补最近一次；一次性定时醒来后全部触发。本包不依赖业务模块，不发通知。
package clock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "clock", Migrations: []store.Migration{{
	Version: 1, Name: "create-timer",
	SQL: `CREATE TABLE clock_timer (
	id TEXT PRIMARY KEY,
	target TEXT NOT NULL,
	kind TEXT NOT NULL,
	at INTEGER NOT NULL,
	recurrence TEXT NOT NULL
);
CREATE INDEX clock_timer_at ON clock_timer(at, id);`,
}}}

// Command 是到期产生的系统命令。Due 是被满足的计划时刻（周期定时补发时为最近一个错过的节拍）。
type Command struct {
	TimerID string
	Target  string
	Kind    domain.TimerKind
	Due     time.Time
	// Late 是实际触发比计划时刻（Timer 登记的 at，周期定时是第一个错过的节拍）晚多久；准时为 0。
	Late  time.Duration
	Actor domain.Actor
}

// Handler 在 tx 内执行命令；返回错误则 Timer 不推进，整个事务回滚。
type Handler func(tx *store.Tx, c Command) error

var systemActor = domain.Actor{Type: domain.ActorSystem, Principal: "clock", Via: domain.ViaClock}

func invalid(format string, a ...any) error {
	return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(format, a...), "fix the timer fields")
}

// Add 在 tx 内登记 Timer 并返回其 id（t.ID 为空时生成）。
func Add(tx *store.Tx, t domain.Timer) (string, error) {
	switch t.Kind {
	case domain.TimerDue, domain.TimerRemind, domain.TimerFollowUp, domain.TimerWaitUntil, domain.TimerRecur:
	default:
		return "", invalid("timer kind %q", t.Kind)
	}
	switch {
	case t.Target == "":
		return "", invalid("timer target is empty")
	case t.At.IsZero():
		return "", invalid("timer at is zero")
	case t.Kind == domain.TimerRecur:
		if _, err := ParseSchedule(t.Recurrence); err != nil {
			return "", err
		}
	case t.Recurrence != "":
		return "", invalid("recurrence only applies to kind recur")
	}
	if t.ID == "" {
		b := make([]byte, 8)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		t.ID = "tm_" + hex.EncodeToString(b)
	}
	if _, err := tx.Exec(`INSERT INTO clock_timer (id, target, kind, at, recurrence) VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.Target, string(t.Kind), t.At.UnixNano(), t.Recurrence); err != nil {
		return "", err
	}
	return t.ID, nil
}

// Cancel 在 tx 内删除 Timer；不存在返回 not_found。
func Cancel(tx *store.Tx, id string) error {
	n, err := tx.Exec(`DELETE FROM clock_timer WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.NewError(domain.CodeNotFound, "timer "+id+" not found", "list timers")
	}
	return nil
}

// Get 在 tx 内读一个 Timer；不存在时 ok 为假。
func Get(tx *store.Tx, id string) (t domain.Timer, ok bool, err error) {
	var kind string
	var at int64
	r, err := tx.Query(`SELECT target, kind, at, recurrence FROM clock_timer WHERE id = ?`, id)
	if err != nil {
		return t, false, err
	}
	if ok = r.Next(); ok {
		err = r.Scan(&t.Target, &kind, &at, &t.Recurrence)
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	t.ID, t.Kind, t.At = id, domain.TimerKind(kind), time.Unix(0, at).UTC()
	return t, ok && err == nil, err
}

// Clock 是扫描循环。
type Clock struct {
	st      *store.Store
	handler Handler
	now     func() time.Time
}

// New 创建时钟；now 为 nil 取本包的 Now。handler 必填：到期处理经它交回，本包不认识业务。
func New(st *store.Store, h Handler, now func() time.Time) (*Clock, error) {
	if h == nil {
		return nil, errors.New("clock: handler is required")
	}
	if now == nil {
		now = Now
	}
	return &Clock{st: st, handler: h, now: now}, nil
}

// Tick 扫描一次，触发所有到期 Timer，返回触发数。某个 Timer 失败不影响其余，错误汇总返回，失败者保持到期待重试。
func (c *Clock) Tick(ctx context.Context) (int, error) {
	now := c.now()
	var ids []string
	err := c.st.View(ctx, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT id FROM clock_timer WHERE at <= ? ORDER BY at, id`, now.UnixNano())
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id string
			if err := r.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return r.Err()
	})
	if err != nil {
		return 0, err
	}
	fired := 0
	var errs []error
	for _, id := range ids {
		ok, err := c.fire(ctx, id, now)
		if err != nil {
			errs = append(errs, fmt.Errorf("clock: timer %s: %w", id, err))
		}
		if ok {
			fired++
		}
	}
	return fired, errors.Join(errs...)
}

// fire 在一个事务内重读 Timer（可能已被取消）、交回命令并推进或删除。
func (c *Clock) fire(ctx context.Context, id string, now time.Time) (bool, error) {
	fired := false
	err := c.st.Update(ctx, func(tx *store.Tx) error {
		var (
			t         domain.Timer
			kind, rec string
			at        int64
		)
		r, err := tx.Query(`SELECT target, kind, at, recurrence FROM clock_timer WHERE id = ?`, id)
		if err != nil {
			return err
		}
		found := r.Next()
		if found {
			err = r.Scan(&t.Target, &kind, &at, &rec)
		}
		if cerr := r.Close(); err == nil {
			err = cerr
		}
		if err != nil || !found {
			return err
		}
		t.Kind, t.At = domain.TimerKind(kind), time.Unix(0, at).UTC()
		due, next := t.At, time.Time{}
		if t.Kind == domain.TimerRecur {
			sc, err := ParseSchedule(rec)
			if err != nil {
				return err
			}
			if due, next = sc.latest(t.At, now); next.IsZero() {
				return invalid("recurrence %q never fires again", rec)
			}
			due, next = due.UTC(), next.UTC()
		}
		if err := c.handler(tx, Command{TimerID: id, Target: t.Target, Kind: t.Kind, Due: due, Late: max(0, now.Sub(t.At)), Actor: systemActor}); err != nil {
			return err
		}
		if next.IsZero() {
			_, err = tx.Exec(`DELETE FROM clock_timer WHERE id = ?`, id)
		} else {
			_, err = tx.Exec(`UPDATE clock_timer SET at = ? WHERE id = ?`, next.UnixNano(), id)
		}
		fired = err == nil
		return err
	})
	return fired, err
}

// Run 每隔 interval 扫描一次，直到 ctx 结束（返回 ctx.Err()）。单次扫描失败不终止循环，下个周期重试；
// 错误交给 onErr（可为 nil）。
func (c *Clock) Run(ctx context.Context, interval time.Duration, onErr ...func(error)) error {
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		if _, err := c.Tick(ctx); err != nil && ctx.Err() == nil {
			for _, f := range onErr {
				f(err)
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tk.C:
		case <-Changed(): // 测试构建：偏移前进后立即重算到期
		}
	}
}
