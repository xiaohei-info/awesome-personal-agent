// Package eventlog 是账本的追加日志（设计 §6.7、§6.15、§10.1）：
// Append 在调用方事务内写入并分配单调序号，Read 按序号游标读取，
// 读法只有两种：全量（Read、ReadObject、Query 默认），与跳过导入事件的"现场"读法（ReadLive、ReadObjectLive、Filter.Live）。
// 折叠、校验、审计、回放用全量；规则、触达、跟进、摘要、注入增量、统计、对外事件流、提交后回调等自动化一律用"现场"读法，新增的事件读取方也默认如此（§6.7、changes §10.9）。
// Tombstone 物理删除时把相关事件的内容字段替换为墓碑，序号与信封保留。
// 本包只管 Event 表；投影与命令由各模块经命令模型完成。
package eventlog

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock/wall"
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
}, {
	// 账本结构版本 8（§6.7，变更清单 WB-03）：导入批次与实际写入时刻。只增列：已有事件为空串与 0，即普通事件，载荷不改。
	Version: 3, Ledger: 8, Name: "ledger-v8-event-import",
	SQL: `ALTER TABLE eventlog_event ADD COLUMN import_batch TEXT NOT NULL DEFAULT '';
ALTER TABLE eventlog_event ADD COLUMN recorded_at INTEGER NOT NULL DEFAULT 0;`,
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
	case e.Imported == "" && !e.RecordedAt.IsZero():
		return invalid("recorded_at requires imported")
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
		at = wall.Now()
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
	// recorded_at 只记导入事件的实际写入时刻；普通事件为 0，读出时即 at。
	var recorded int64
	if e.Imported != "" {
		if e.RecordedAt.IsZero() {
			e.RecordedAt = wall.Now()
		}
		recorded = e.RecordedAt.UnixNano()
	}
	if _, err := tx.Exec(`INSERT INTO eventlog_event
		(type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, grant_id, at, import_batch, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.Type, e.Object, string(actor), e.Reason, string(e.Payload), int64(e.ChainDepth),
		string(basis), e.ResolvedVia, e.Evidence, e.Grant, at.UnixNano(), e.Imported, recorded); err != nil {
		return 0, err
	}
	var seq int64
	if err := queryRows(tx, func(r *store.Rows) error { return r.Scan(&seq) }, "SELECT last_insert_rowid()"); err != nil {
		return 0, err
	}
	return seq, nil
}

const cols = "seq, type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, grant_id, at, tombstone, import_batch, recorded_at"

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
			recorded           int64
		)
		if err := r.Scan(&e.Event.Seq, &e.Event.Type, &e.Event.Object, &actor, &e.Event.Reason, &payload,
			&depth, &bs, &e.Event.ResolvedVia, &e.Event.Evidence, &e.Event.Grant, &at, &tomb, &e.Event.Imported, &recorded); err != nil {
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
		if recorded != 0 {
			e.Event.RecordedAt = time.Unix(0, recorded).UTC()
		}
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

// live 是"现场"读法的过滤条件：跳过导入事件。
const live = " AND import_batch = ''"

// ReadLive 同 Read，但跳过导入事件（"现场"读法）。游标语义不变：订阅方把返回的最后一条序号当作新游标；
// 尾部被跳过的导入事件不会返回，下次读取时在服务端再次跳过。
func ReadLive(tx *store.Tx, after int64, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	return collect(tx, "SELECT "+cols+" FROM eventlog_event WHERE seq > ?"+live+" ORDER BY seq LIMIT ?", after, int64(limit))
}

// ReadObject 返回某对象的全部事件，按序号升序（事项时间线与回放用）。
func ReadObject(tx *store.Tx, object string) ([]Entry, error) {
	return collect(tx, "SELECT "+cols+" FROM eventlog_event WHERE object = ? ORDER BY seq", object)
}

// ReadObjectLive 同 ReadObject，但跳过导入事件（统计等自动化读取用）。
func ReadObjectLive(tx *store.Tx, object string) ([]Entry, error) {
	return collect(tx, "SELECT "+cols+" FROM eventlog_event WHERE object = ?"+live+" ORDER BY seq", object)
}

// ReadDisposedFor 返回把 item 建成（或并入）建议事项的入站处置事件（payload.item 或 payload.items 含它），按序号升序。
// 回放建议事项时用它带出来源事件上的判断（§15.1）；这些事件的 object 是来源事件 id。
func ReadDisposedFor(tx *store.Tx, item string) ([]Entry, error) {
	return collect(tx, "SELECT "+cols+` FROM eventlog_event WHERE type = 'inbound.disposed' AND tombstone = 0 AND (json_extract(payload, '$.item') = ?1
		OR EXISTS (SELECT 1 FROM json_each(payload, '$.items') WHERE value = ?1)) ORDER BY seq`, item)
}

// ReadMentioning 返回事项一侧点名某条来源消息的事件（跟进消息的挂上、交回、退回，建议补到已有事项时随之改挂），按序号升序。
// 这些事件的 object 是事项，消息 id 在载荷的 message 或 messages 里（处理经过用）。
// shortcut: 按类型与载荷扫表，没有索引；账本到几十万条事件、经过页明显变慢时再加载荷索引。
func ReadMentioning(tx *store.Tx, message string) ([]Entry, error) {
	return collect(tx, "SELECT "+cols+` FROM eventlog_event WHERE type IN ('followmsg.attached', 'followmsg.handed_back', 'followmsg.released', 'proposal.attached')
		AND tombstone = 0 AND (json_extract(payload, '$.message') = ?1 OR EXISTS (SELECT 1 FROM json_each(payload, '$.messages') WHERE value = ?1)) ORDER BY seq`, message)
}

// Filter 是审计查询条件；空字段不限。Actor 匹配操作者 principal，Grant 匹配依据的授权单 id（授权单的使用记录），
// Imported 只取某个导入批次的事件。Live 为真走"现场"读法，跳过导入事件；与 Imported 同时给出没有结果。
type Filter struct {
	Actor, Object, Type, Grant, Imported string
	Tier                                 string // 事件依据里的档位（basis.tier），如 rules：规矩改动清单按它取（§14.3）
	Live                                 bool
}

// Query 返回序号大于 after、满足 f 的至多 limit 条事件，按序号升序（审计页用）。
func Query(tx *store.Tx, f Filter, after int64, limit int) ([]Entry, error) {
	return query(tx, f, "seq > ?", after, "seq", limit)
}

// QueryBefore 返回序号小于 before（0 表示不限）、满足 f 的至多 limit 条事件，按序号降序，最新的在前（审计页用）。
func QueryBefore(tx *store.Tx, f Filter, before int64, limit int) ([]Entry, error) {
	if before == 0 {
		before = math.MaxInt64
	}
	return query(tx, f, "seq < ?", before, "seq DESC", limit)
}

func query(tx *store.Tx, f Filter, cursor string, bound int64, order string, limit int) ([]Entry, error) {
	if limit <= 0 {
		return nil, nil
	}
	sql, args := "SELECT "+cols+" FROM eventlog_event WHERE "+cursor, []any{bound}
	for _, c := range []struct{ col, val string }{{"json_extract(actor, '$.principal')", f.Actor}, {"object", f.Object}, {"type", f.Type}, {"grant_id", f.Grant}, {"import_batch", f.Imported}, {"json_extract(NULLIF(basis, ''), '$.tier')", f.Tier}} {
		if c.val != "" {
			sql, args = sql+" AND "+c.col+" = ?", append(args, c.val)
		}
	}
	if f.Live {
		sql += live
	}
	return collect(tx, sql+" ORDER BY "+order+" LIMIT ?", append(args, int64(limit))...)
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

// SeenHarness 返回每个操作者最近一次事件里自报的 harness（只作推荐，不参与判定，§14.2）；没有自报过的不出现在结果里。
// shortcut: 每次调用全表扫描一遍事件日志，事件到数十万条、设置页变慢时改为按操作者建索引或缓存。
func SeenHarness(tx *store.Tx) (map[string]string, error) {
	out := map[string]string{}
	err := queryRows(tx, func(r *store.Rows) error {
		var p, h string
		var seq int64
		if err := r.Scan(&p, &h, &seq); err != nil {
			return err
		}
		out[p] = h
		return nil
	}, `SELECT json_extract(actor, '$.principal'), json_extract(actor, '$.harness'), MAX(seq) FROM eventlog_event
		WHERE json_extract(actor, '$.harness') <> '' GROUP BY json_extract(actor, '$.principal')`)
	return out, err
}
