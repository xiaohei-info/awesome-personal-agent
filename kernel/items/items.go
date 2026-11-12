// Package items 是事项中心的事项部分（设计 §6.3、§6.4、§7.1）：事项与关系的投影、
// item.create / update / transition / relate / revert 命令，以及 wait_until 与事项截止的到期处理。
// 所有写入经命令模型登记的命令：授权与事件由管线负责，本包只给出对象、校验与投影变更。
//
// 投影与事件的关系（§6.7）：每条命令在同一事务里更新投影并追加一条事件，事件 payload 带变更后的完整快照
// （关系变更带关系本身），因此 Fold 能只靠事件重建投影；Verify 用它做一致性校验，Rebuild 用它修复。
// 乐观版本只有一个来源（命令管线的 command_version），本包不另存。
// created_at、updated_at 是事项事件时间的投影，存于独立列，不进 data 与事件快照，折叠时取事件的 At。
package items

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。事项整体以 JSON 存于 data，
// 其余列只为按阶段、项目、执行者、kind 过滤；关系是带类型的边，不是独立实体。
var Module = store.Module{Name: "items", Migrations: []store.Migration{{
	Version: 1, Name: "create-items",
	SQL: `CREATE TABLE items (
	id TEXT PRIMARY KEY,
	phase TEXT NOT NULL,
	kind TEXT NOT NULL,
	project TEXT NOT NULL,
	assignee TEXT NOT NULL,
	data TEXT NOT NULL
) WITHOUT ROWID;
CREATE INDEX items_phase ON items(phase);
CREATE TABLE item_relations (
	type TEXT NOT NULL,
	from_id TEXT NOT NULL,
	to_id TEXT NOT NULL,
	PRIMARY KEY (type, from_id, to_id)
) WITHOUT ROWID;
CREATE INDEX item_relations_to ON item_relations(to_id);`,
}, {
	// 回填依赖 eventlog 的表：迁移按模块名排序，eventlog 先于 items。类型清单同 itemEvents。
	Version: 2, Name: "add-item-timestamps",
	SQL: `ALTER TABLE items ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE items ADD COLUMN updated_at INTEGER NOT NULL DEFAULT 0;
UPDATE items SET
	created_at = COALESCE((SELECT at FROM eventlog_event WHERE object = items.id AND type IN ('item.created', 'item.updated', 'item.phase_changed',
		'decision.requested', 'decision.resolved', 'delegation.created', 'delegation.reported') ORDER BY seq LIMIT 1), 0),
	updated_at = COALESCE((SELECT at FROM eventlog_event WHERE object = items.id AND type IN ('item.created', 'item.updated', 'item.phase_changed',
		'decision.requested', 'decision.resolved', 'delegation.created', 'delegation.reported') ORDER BY seq DESC LIMIT 1), 0);`,
}, {
	// 账本结构版本 3（v0.2.0）：计划日期（§6.3 planned_for，只到日）、建议信息与合并键（proposal、merge_key）、
	// 进展条目（item_progress）、事项登记的关联键域（item_correlation，§9.4）。只增不改，已有行取空值。
	Version: 3, Ledger: 3, Name: "ledger-v3-items",
	SQL: `ALTER TABLE items ADD COLUMN planned_for TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN proposal TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN proposal_attach_to TEXT NOT NULL DEFAULT '';
ALTER TABLE items ADD COLUMN merge_key TEXT NOT NULL DEFAULT '';
CREATE INDEX items_planned_for ON items(planned_for) WHERE planned_for <> '';
CREATE INDEX items_merge_key ON items(merge_key) WHERE merge_key <> '';
CREATE INDEX items_proposal_attach_to ON items(proposal_attach_to) WHERE proposal_attach_to <> '';
CREATE TABLE item_progress (
	id TEXT PRIMARY KEY,
	item TEXT NOT NULL,
	text TEXT NOT NULL,
	on_date TEXT NOT NULL DEFAULT '',
	actor TEXT NOT NULL,
	at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX item_progress_item ON item_progress(item, at);
CREATE TABLE item_correlation (
	item TEXT NOT NULL,
	domain TEXT NOT NULL,
	key TEXT NOT NULL,
	PRIMARY KEY (item, domain, key)
) WITHOUT ROWID;
CREATE INDEX item_correlation_key ON item_correlation(domain, key);`,
}}}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func notFound(id string) error {
	return domain.NewError(domain.CodeNotFound, "item "+id+" does not exist", "list items and retry")
}

func conflict(why, next string) error {
	return domain.NewError(domain.CodeConflict, why, next)
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "itm_" + hex.EncodeToString(b), nil
}

// ---- 读 ----

func scanItem(r *store.Rows) (domain.Item, error) {
	var (
		data             string
		created, updated int64
	)
	if err := r.Scan(&data, &created, &updated); err != nil {
		return domain.Item{}, err
	}
	var it domain.Item
	if err := json.Unmarshal([]byte(data), &it); err != nil {
		return it, fmt.Errorf("items: corrupt row: %w", err)
	}
	it.CreatedAt, it.UpdatedAt = time.Unix(0, created).UTC(), time.Unix(0, updated).UTC()
	return it, nil
}

func query(tx *store.Tx, where string, args ...any) ([]domain.Item, error) {
	r, err := tx.Query("SELECT data, created_at, updated_at FROM items "+where, args...)
	if err != nil {
		return nil, err
	}
	var out []domain.Item
	for r.Next() {
		it, err := scanItem(r)
		if err != nil {
			r.Close()
			return nil, err
		}
		out = append(out, it)
	}
	if err := r.Err(); err != nil {
		r.Close()
		return nil, err
	}
	if err := r.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		v, err := command.Version(tx, out[i].ID)
		if err != nil {
			return nil, err
		}
		out[i].Version = v
	}
	return out, nil
}

// Get 读取一个事项；不存在返回 not_found。
func Get(tx *store.Tx, id string) (domain.Item, error) {
	its, err := query(tx, "WHERE id = ?", id)
	if err != nil {
		return domain.Item{}, err
	}
	if len(its) == 0 {
		return domain.Item{}, notFound(id)
	}
	return its[0], nil
}

// Filter 的非空字段全部满足才命中。
type Filter struct {
	Phase    domain.Phase
	Kind     domain.Kind
	Project  string
	Assignee string
}

// List 按 id 排序返回命中的事项。
func List(tx *store.Tx, f Filter) ([]domain.Item, error) {
	var conds []string
	var args []any
	for _, c := range []struct{ col, val string }{
		{"phase", string(f.Phase)}, {"kind", string(f.Kind)}, {"project", f.Project}, {"assignee", f.Assignee},
	} {
		if c.val != "" {
			conds = append(conds, c.col+" = ?")
			args = append(args, c.val)
		}
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ") + " "
	}
	return query(tx, where+"ORDER BY id", args...)
}

func allRelations(tx *store.Tx, where string, args ...any) ([]domain.Relation, error) {
	r, err := tx.Query("SELECT type, from_id, to_id FROM item_relations "+where+"ORDER BY type, from_id, to_id", args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []domain.Relation
	for r.Next() {
		var x domain.Relation
		if err := r.Scan(&x.Type, &x.From, &x.To); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, r.Err()
}

// Relations 返回与事项相连的全部边（它是起点或终点）。
func Relations(tx *store.Tx, id string) ([]domain.Relation, error) {
	return allRelations(tx, "WHERE from_id = ? OR to_id = ? ", id, id)
}

func hasRelation(tx *store.Tx, x domain.Relation) (bool, error) {
	rs, err := allRelations(tx, "WHERE type = ? AND from_id = ? AND to_id = ? ", x.Type, x.From, x.To)
	return len(rs) > 0, err
}

// ---- 写（投影） ----

// stamped 记下一条事项事件的时刻：首条是 created_at，每条都是 updated_at。写投影与折叠共用。
// 写投影时 at 必须等于同一事务里那条事件的 At，否则 Verify 报不一致。
func stamped(it domain.Item, at time.Time) domain.Item {
	if it.CreatedAt.IsZero() {
		it.CreatedAt = at
	}
	it.UpdatedAt = at
	return it
}

// bare 去掉不进 data 与事件快照的字段：版本归命令管线，时间在独立列。
func bare(it domain.Item) domain.Item {
	it.Version, it.CreatedAt, it.UpdatedAt = 0, time.Time{}, time.Time{}
	return it
}

func save(tx *store.Tx, it domain.Item) error {
	b, err := json.Marshal(bare(it))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO items (id, phase, kind, project, assignee, data, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		it.ID, string(it.Phase), string(it.Kind), it.Project, it.Assignee, string(b), it.CreatedAt.UnixNano(), it.UpdatedAt.UnixNano())
	return err
}

func setRelation(tx *store.Tx, x domain.Relation, remove bool) error {
	var err error
	if remove {
		_, err = tx.Exec(`DELETE FROM item_relations WHERE type = ? AND from_id = ? AND to_id = ?`, x.Type, x.From, x.To)
	} else {
		_, err = tx.Exec(`INSERT OR IGNORE INTO item_relations (type, from_id, to_id) VALUES (?, ?, ?)`, x.Type, x.From, x.To)
	}
	return err
}

// Purge 物理删除事项投影及其关系边（设计 §6.15）；事项不存在返回 not_found。
// 事件内容的墓碑与删除事件由 kernel/purge 在同一事务内完成。
func Purge(tx *store.Tx, id string) error {
	n, err := tx.Exec(`DELETE FROM items WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n == 0 {
		return notFound(id)
	}
	_, err = tx.Exec(`DELETE FROM item_relations WHERE from_id = ? OR to_id = ?`, id, id)
	return err
}
