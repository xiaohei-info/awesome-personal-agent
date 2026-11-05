// Package items 是事项中心的事项部分（设计 §6.3、§6.4、§7.1）：事项与关系的投影、
// item.create / update / transition / relate / revert 命令，以及 wait_until 与事项截止的到期处理。
// 所有写入经命令模型登记的命令：授权与事件由管线负责，本包只给出对象、校验与投影变更。
//
// 投影与事件的关系（§6.7）：每条命令在同一事务里更新投影并追加一条事件，事件 payload 带变更后的完整快照
// （关系变更带关系本身），因此 Fold 能只靠事件重建投影；Verify 用它做一致性校验，Rebuild 用它修复。
// 乐观版本只有一个来源（命令管线的 command_version），本包不另存。
package items

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	var data string
	if err := r.Scan(&data); err != nil {
		return domain.Item{}, err
	}
	var it domain.Item
	if err := json.Unmarshal([]byte(data), &it); err != nil {
		return it, fmt.Errorf("items: corrupt row: %w", err)
	}
	return it, nil
}

func query(tx *store.Tx, where string, args ...any) ([]domain.Item, error) {
	r, err := tx.Query("SELECT data FROM items "+where, args...)
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

func save(tx *store.Tx, it domain.Item) error {
	it.Version = 0 // 版本归命令管线，不进投影
	b, err := json.Marshal(it)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO items (id, phase, kind, project, assignee, data) VALUES (?, ?, ?, ?, ?, ?)`,
		it.ID, string(it.Phase), string(it.Kind), it.Project, it.Assignee, string(b))
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
