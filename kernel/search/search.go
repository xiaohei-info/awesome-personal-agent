// Package search 是材料检索（设计 §12.4、§18）：SQLite FTS5 加应用层中日韩二元切分的全文索引，加结构化筛选。
//
//   - 自有型材料索引标题、当前修订正文、标签；托管型只索引标题、路径、标签，正文永不进索引。
//   - 索引是 materials 表的派生数据，本包只读 materials，只写自己的表；每次检索前按签名增量同步，
//     所以修订、回退、确认都不需要通知本包，索引也可以随时丢弃重建。
//   - 事项关联由本包的 material.link 命令维护（走命令模型，授权同修订材料）。
//   - 结果只回 id、标题、片段（托管型回路径）。
package search

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "search", Migrations: []store.Migration{{
	Version: 1, Name: "create-material-search",
	SQL: `CREATE VIRTUAL TABLE material_search USING fts5(material UNINDEXED, title, body, tags, path, tokenize = 'unicode61');
CREATE TABLE material_search_state (material TEXT PRIMARY KEY, sig TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE material_links (material TEXT NOT NULL, item TEXT NOT NULL, PRIMARY KEY (material, item)) WITHOUT ROWID;
CREATE INDEX material_links_item ON material_links (item);`,
}}}

// 默认与上限结果数。
const (
	DefaultLimit = 20
	MaxLimit     = 200
)

// Query 是一次检索：Text 为空时只按筛选条件列出。筛选条件之间取交集，Tags 要求全部命中。
type Query struct {
	Text    string
	Project string
	Kind    string
	Tags    []string
	Item    string    // 关联事项
	After   time.Time // 当前修订时间下界（含）；托管型无修订，设置时间筛选后不会命中
	Before  time.Time // 当前修订时间上界（不含）
	Limit   int
}

// Hit 是一条结果。自有型回片段，托管型回路径。
type Hit struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Kind    string `json:"kind"`
	Project string `json:"project"`
	Hosted  bool   `json:"hosted"`
	Snippet string `json:"snippet,omitempty"`
	Path    string `json:"path,omitempty"`
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// tokens 做应用层切分：连续的中日韩字符切成相邻二元组（单字成一元组），其余按字母数字词切分并转小写。
// index 为真时中日韩字符另加一元组，使单字也能检索；查询只用二元组，避免单字噪声。
func tokens(s string, index bool) []string {
	var out []string
	var run []rune
	flushCJK := func() {
		if len(run) == 1 {
			out = append(out, string(run))
		}
		for i := 0; i+1 < len(run); i++ {
			out = append(out, string(run[i:i+2]))
		}
		if index && len(run) > 1 {
			for _, r := range run {
				out = append(out, string(r))
			}
		}
		run = run[:0]
	}
	var word []rune
	flushWord := func() {
		if len(word) > 0 {
			out = append(out, strings.ToLower(string(word)))
			word = word[:0]
		}
	}
	for _, r := range s {
		switch {
		case isCJK(r):
			flushWord()
			run = append(run, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			flushCJK()
			word = append(word, r)
		default:
			flushCJK()
			flushWord()
		}
	}
	flushCJK()
	flushWord()
	return out
}

func join(s string) string { return strings.Join(tokens(s, true), " ") }

type doc struct {
	id, title, body, tags, path, sig string
}

func loadDocs(tx *store.Tx) ([]doc, error) {
	r, err := tx.Query(`SELECT m.id, m.title, m.tags, m.location, m.current_rev, COALESCE(v.content, '')
		FROM materials m LEFT JOIN revisions v ON v.id = m.current_rev ORDER BY m.id`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []doc
	for r.Next() {
		var d doc
		var rev, content string
		if err := r.Scan(&d.id, &d.title, &d.tags, &d.path, &rev, &content); err != nil {
			return nil, err
		}
		var tags []string
		if err := json.Unmarshal([]byte(d.tags), &tags); err != nil {
			return nil, fmt.Errorf("search: corrupt tags of %s: %w", d.id, err)
		}
		d.tags = strings.Join(tags, " ")
		if d.path == "" { // 自有型：正文来自当前修订
			d.body = content
		}
		h := sha256.Sum256([]byte(strings.Join([]string{d.title, d.tags, d.path, rev}, "\x00")))
		d.sig = hex.EncodeToString(h[:])
		out = append(out, d)
	}
	return out, r.Err()
}

func states(tx *store.Tx) (map[string]string, error) {
	r, err := tx.Query("SELECT material, sig FROM material_search_state")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]string{}
	for r.Next() {
		var id, sig string
		if err := r.Scan(&id, &sig); err != nil {
			return nil, err
		}
		out[id] = sig
	}
	return out, r.Err()
}

// Sync 让索引与 materials 一致：签名变了的重建，已不存在的删除。需要写事务。
func Sync(tx *store.Tx) error {
	docs, err := loadDocs(tx)
	if err != nil {
		return err
	}
	have, err := states(tx)
	if err != nil {
		return err
	}
	for _, d := range docs {
		old, ok := have[d.id]
		delete(have, d.id)
		if ok && old == d.sig {
			continue
		}
		if _, err := tx.Exec("DELETE FROM material_search WHERE material = ?", d.id); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT INTO material_search (material, title, body, tags, path) VALUES (?, ?, ?, ?, ?)",
			d.id, join(d.title), join(d.body), join(d.tags), join(d.path)); err != nil {
			return err
		}
		if _, err := tx.Exec("INSERT OR REPLACE INTO material_search_state (material, sig) VALUES (?, ?)", d.id, d.sig); err != nil {
			return err
		}
	}
	for id := range have {
		if _, err := tx.Exec("DELETE FROM material_search WHERE material = ?", id); err != nil {
			return err
		}
		if _, err := tx.Exec("DELETE FROM material_search_state WHERE material = ?", id); err != nil {
			return err
		}
	}
	return nil
}

// Search 先同步索引再查询，是 Web 搜索框与 `apa material search` 共用的入口。
func Search(ctx context.Context, st *store.Store, q Query) ([]Hit, error) {
	var hits []Hit
	err := st.Update(ctx, func(tx *store.Tx) error {
		if err := Sync(tx); err != nil {
			return err
		}
		var err error
		hits, err = Find(tx, q)
		return err
	})
	return hits, err
}

// Find 在已同步的索引上查询；索引过期时由 Search 负责先 Sync。
func Find(tx *store.Tx, q Query) ([]Hit, error) {
	sql := `SELECT m.id, m.title, m.kind, m.project, m.location, COALESCE(v.content, '') FROM materials m
		LEFT JOIN revisions v ON v.id = m.current_rev`
	var where []string
	var args []any
	order := "m.title, m.id"
	var terms []string
	if strings.TrimSpace(q.Text) != "" {
		terms = tokens(q.Text, false)
		if len(terms) == 0 {
			return nil, domain.NewError(domain.CodeInvalidRequest, "query has no searchable text", "use letters, digits or CJK characters")
		}
		quoted := make([]string, len(terms))
		for i, t := range terms {
			quoted[i] = `"` + t + `"`
		}
		sql += " JOIN material_search ON material_search.material = m.id"
		where = append(where, "material_search MATCH ?")
		args = append(args, strings.Join(quoted, " "))
		order = "bm25(material_search), m.id"
	}
	if q.Project != "" {
		where, args = append(where, "m.project = ?"), append(args, q.Project)
	}
	if q.Kind != "" {
		where, args = append(where, "m.kind = ?"), append(args, q.Kind)
	}
	for _, t := range q.Tags {
		where = append(where, "EXISTS (SELECT 1 FROM json_each(m.tags) WHERE value = ?)")
		args = append(args, t)
	}
	if q.Item != "" {
		where = append(where, "EXISTS (SELECT 1 FROM material_links l WHERE l.material = m.id AND l.item = ?)")
		args = append(args, q.Item)
	}
	if !q.After.IsZero() {
		where = append(where, "v.created_at >= ?")
		args = append(args, q.After.UnixNano())
	}
	if !q.Before.IsZero() {
		where = append(where, "v.created_at < ?")
		args = append(args, q.Before.UnixNano())
	}
	if len(where) > 0 {
		sql += " WHERE " + strings.Join(where, " AND ")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	sql += fmt.Sprintf(" ORDER BY %s LIMIT %d", order, min(limit, MaxLimit))
	r, err := tx.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Hit
	for r.Next() {
		var h Hit
		var loc, body string
		if err := r.Scan(&h.ID, &h.Title, &h.Kind, &h.Project, &loc, &body); err != nil {
			return nil, err
		}
		if loc != "" {
			h.Hosted, h.Path = true, loc
		} else {
			h.Snippet = snippet(body, q.Text)
		}
		out = append(out, h)
	}
	return out, r.Err()
}

const snippetRadius = 40

// snippet 取正文里第一处命中词附近的原文；没有命中（只命中标题或标签）时取开头。
func snippet(body, text string) string {
	body = strings.Join(strings.Fields(body), " ")
	rs, low := []rune(body), []rune(strings.ToLower(body))
	if len(low) != len(rs) { // 个别字符小写后变长，退回按开头截取
		low = rs
	}
	start := 0
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		if i := indexRunes(low, []rune(w)); i >= 0 {
			start = max(0, i-snippetRadius)
			break
		}
	}
	end := min(len(rs), start+2*snippetRadius)
	s := string(rs[start:end])
	if start > 0 {
		s = "…" + s
	}
	if end < len(rs) {
		s += "…"
	}
	return s
}

func indexRunes(s, sub []rune) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if string(s[i:i+len(sub)]) == string(sub) {
			return i
		}
	}
	return -1
}

// ---- 事项关联 ----

type linkIn struct {
	Material string `json:"material"`
	Item     string `json:"item"`
	Remove   bool   `json:"remove,omitempty"`
}

// Register 登记 material.link：建立或移除材料与事项的关联，授权同修订材料；重复建立与移除不存在的关联都是空操作。
func Register(x *command.Executor) error {
	parse := func(in json.RawMessage) (linkIn, error) {
		var v linkIn
		if err := json.Unmarshal(in, &v); err != nil {
			return v, domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
		}
		return v, nil
	}
	return x.Register(command.Spec{
		Name: "material.link", Action: authz.ActionReviseMaterial,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			r, err := tx.Query("SELECT kind, project, tags FROM materials WHERE id = ?", v.Material)
			if err != nil {
				return authz.Object{}, err
			}
			defer r.Close()
			if !r.Next() {
				return authz.Object{}, domain.NewError(domain.CodeNotFound, "material "+v.Material+" does not exist", "list materials and retry")
			}
			o := authz.Object{ID: v.Material}
			var tags string
			if err := r.Scan(&o.Kind, &o.Project, &tags); err != nil {
				return o, err
			}
			return o, json.Unmarshal([]byte(tags), &o.Tags)
		},
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, err := parse(in)
			if err == nil && strings.TrimSpace(v.Item) == "" {
				err = domain.NewError(domain.CodeInvalidRequest, "item is required", "pass the item id")
			}
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, err := parse(c.Input)
			if err != nil {
				return nil, err
			}
			if v.Remove {
				_, err = c.Tx.Exec("DELETE FROM material_links WHERE material = ? AND item = ?", v.Material, v.Item)
			} else {
				_, err = c.Tx.Exec("INSERT OR IGNORE INTO material_links (material, item) VALUES (?, ?)", v.Material, v.Item)
			}
			return v, err
		},
	})
}
