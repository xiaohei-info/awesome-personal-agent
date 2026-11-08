// Package eventlog 是账本的追加日志（设计 §6.7、§6.15、§10.1）：
// Append 在调用方事务内写入并分配单调序号，Read 按序号游标读取，
// Tombstone 物理删除时把相关事件的内容字段替换为墓碑，序号与信封保留。
// 本包只管 Event 表；投影与命令由各模块经命令模型完成。
package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ErrInvalidEvent 表示事件不满足追加约束，事务内未写入任何东西。
var ErrInvalidEvent = errors.New("eventlog: invalid event")

// Module 是本模块在存储层登记的迁移。AUTOINCREMENT 保证序号永不复用，墓碑化与回滚都不会造成重号。
var Module = store.Module{Name: "eventlog", Migrations: []store.Migration{{
	Version: 1, Name: "create-event",
	SQL: `CREATE TABLE eventlog_event (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	type TEXT NOT NULL,
	object TEXT NOT NULL,
	actor TEXT NOT NULL,
	reason TEXT NOT NULL,
	payload TEXT NOT NULL,
	chain_depth INTEGER NOT NULL,
	basis TEXT NOT NULL,
	resolved_via TEXT NOT NULL,
	evidence TEXT NOT NULL,
	at INTEGER NOT NULL,
	tombstone INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX eventlog_event_object ON eventlog_event(object, seq);`,
}, {
	Version: 2, Name: "add-event-grant",
	SQL: `ALTER TABLE eventlog_event ADD COLUMN grant_id TEXT NOT NULL DEFAULT '';`,
}}}

// Entry 是读出的事件；Tombstone 为真表示内容字段已被物理删除抹除。
type Entry struct {
	Event     domain.Event
	Tombstone bool
}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidEvent, fmt.Sprintf(format, a...))
}

func validate(e domain.Event) error {
	switch {
	case e.Type == "":
		return invalid("type is empty")
	case e.Object == "":
		return invalid("object is empty")
	case e.Actor.Principal == "":
		return invalid("actor principal is empty")
	case e.ChainDepth < 0:
		return invalid("chain_depth %d < 0", e.ChainDepth)
	case !json.Valid(e.Payload):
		return invalid("payload is not valid JSON")
	}
	switch e.Actor.Type {
	case domain.ActorHuman, domain.ActorAgent, domain.ActorSystem:
	default:
		return invalid("actor type %q", e.Actor.Type)
	}
	switch e.Actor.Via {
	case domain.ViaWeb, domain.ViaCLI, domain.ViaClock, domain.ViaRule, domain.ViaSource, domain.ViaExt:
	default:
		return invalid("actor via %q", e.Actor.Via)
	}
	// 自动动作必须能追溯到授权它的策略版本与规则。
	if e.Actor.Via == domain.ViaRule && (e.Basis == nil || e.Basis.RuleID == "") {
		return invalid("rule action requires basis with rule_id")
	}
	return nil
}

// Append 在 tx 内追加一条事件并返回其序号；Seq 字段被忽略，At 为零时取当前时间。
// 事务回滚则事件与序号一并回滚。
func Append(tx *store.Tx, e domain.Event) (int64, error) {
	if err := validate(e); err != nil {
		return 0, err
	}
	at := e.At
	if at.IsZero() {
		at = time.Now()
	}
	actor, err := json.Marshal(e.Actor)
	if err != nil {
		return 0, err
	}
	basis := []byte{}
	if e.Basis != nil {
		if basis, err = json.Marshal(e.Basis); err != nil {
			return 0, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO eventlog_event
		(type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, grant_id, at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Type, e.Object, string(actor), e.Reason, string(e.Payload), int64(e.ChainDepth),
		string(basis), e.ResolvedVia, e.Evidence, e.Grant, at.UnixNano()); err != nil {
		return 0, err
	}
	var seq int64
	if err := queryRows(tx, func(r *store.Rows) error { return r.Scan(&seq) }, "SELECT last_insert_rowid()"); err != nil {
		return 0, err
	}
	return seq, nil
}

const cols = "seq, type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, grant_id, at, tombstone"

func queryRows(tx *store.Tx, each func(*store.Rows) error, sql string, args ...any) error {
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

func collect(tx *store.Tx, sql string, args ...any) ([]Entry, error) {
	var out []Entry
	err := queryRows(tx, func(r *store.Rows) error {
		var (
			e                  Entry
			actor, payload, bs string
			depth, at, tomb    int64
		)
		if err := r.Scan(&e.Event.Seq, &e.Event.Type, &e.Event.Object, &actor, &e.Event.Reason, &payload,
			&depth, &bs, &e.Event.ResolvedVia, &e.Event.Evidence, &e.Event.Grant, &at, &tomb); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(actor), &e.Event.Actor); err != nil {
			return fmt.Errorf("eventlog: seq %d actor: %w", e.Event.Seq, err)
		}
		if bs != "" {
			e.Event.Basis = new(domain.Basis)
			if err := json.Unmarshal([]byte(bs), e.Event.Basis); err != nil {
				return fmt.Errorf("eventlog: seq %d basis: %w", e.Event.Seq, err)
			}
		}
		e.Event.Payload = json.RawMessage(payload)
		e.Event.ChainDepth = int(depth)
		e.Event.At = time.Unix(0, at).UTC()
		e.Tombstone = tomb != 0
		out = append(out, e)
		return nil
	}, sql, args...)
	return out, err
}

// Read 返回序号大于 after 的至多 limit 条事件，按序号升序；after 即订阅方自持的游标。
func Read(tx *store.Tx, after int64, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	return collect(tx, "SELECT "+cols+" FROM eventlog_event WHERE seq > ? ORDER BY seq LIMIT ?", after, int64(limit))
}

// ReadObject 返回某对象的全部事件，按序号升序（事项时间线与回放用）。
func ReadObject(tx *store.Tx, object string) ([]Entry, error) {
	return collect(tx, "SELECT "+cols+" FROM eventlog_event WHERE object = ? ORDER BY seq", object)
}

// Filter 是审计查询条件；空字段不限。Actor 匹配操作者 principal，Grant 匹配依据的授权单 id（授权单的使用记录）。
type Filter struct {
	Actor, Object, Type, Grant string
}

// Query 返回序号大于 after、满足 f 的至多 limit 条事件，按序号升序（审计页用）。
func Query(tx *store.Tx, f Filter, after int64, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	sql, args := "SELECT "+cols+" FROM eventlog_event WHERE seq > ?", []any{after}
	for _, c := range []struct{ col, val string }{{"json_extract(actor, '$.principal')", f.Actor}, {"object", f.Object}, {"type", f.Type}, {"grant_id", f.Grant}} {
		if c.val != "" {
			sql, args = sql+" AND "+c.col+" = ?", append(args, c.val)
		}
	}
	return collect(tx, sql+" ORDER BY seq LIMIT ?", append(args, int64(limit))...)
}

// Head 返回当前最大序号，空日志为 0。
func Head(tx *store.Tx) (int64, error) {
	var head int64
	err := queryRows(tx, func(r *store.Rows) error { return r.Scan(&head) }, "SELECT COALESCE(MAX(seq), 0) FROM eventlog_event")
	return head, err
}

// Tombstone 把指定对象的所有事件内容字段（reason、payload、evidence）替换为墓碑并返回新墓碑化的条数。
// 序号、类型、对象 id、时间、actor 等信封字段保留，因此序号与订阅游标不受影响；重复调用幂等。
// 「另记一条删除事件」由调用方在同一事务内 Append。
func Tombstone(tx *store.Tx, objects ...string) (int64, error) {
	var total int64
	for _, o := range objects {
		if o == "" {
			return 0, invalid("tombstone object is empty")
		}
		n, err := tx.Exec(`UPDATE eventlog_event SET reason = '', payload = 'null', evidence = '', tombstone = 1
			WHERE object = ? AND tombstone = 0`, o)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}
