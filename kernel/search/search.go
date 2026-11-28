// Package search 是统一检索（设计 §7.1、§12.4、§18）：项目、事项（含进展）、材料与来源事件的标题摘要进同一个
// SQLite FTS5 索引，应用层做中日韩二元切分，FTS5 的 porter 词形还原叠在英文整词与前缀之上。
//
//   - 索引是账本的派生数据：只读各模块的投影，只写本包自己的表，随时可以丢弃重建；不参与任何写入判定。
//   - 增量同步按序号折叠：事件日志游标之后被动到的项目、事项、材料逐个从投影重读；来源事件按来源事件表的序号追加。
//     命令模块不知道索引的存在；只有物理删除经 Forget 当场清掉（被删对象不再有新事件）。
//   - 自有型材料索引正文；托管型与链接型只索引标题、位置、标签。来源事件只索引标题与摘要，对话捕获不进。
//   - 查询见 find.go：任一词命中、bm25 字段加权、阶段与时间与当前项目系数；不调模型，同一查询同一账本结果相同。
package search

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "search", Migrations: []store.Migration{{
	Version: 1, Name: "create-material-search",
	SQL: `CREATE VIRTUAL TABLE material_search USING fts5(material UNINDEXED, title, body, tags, path, tokenize = 'unicode61');
CREATE TABLE material_search_state (material TEXT PRIMARY KEY, sig TEXT NOT NULL) WITHOUT ROWID;
CREATE TABLE material_links (material TEXT NOT NULL, item TEXT NOT NULL, PRIMARY KEY (material, item)) WITHOUT ROWID;
CREATE INDEX material_links_item ON material_links (item);`,
}, {
	// 关联并入 item.relate（带类型、记 Event）；旧表没有事件来源，无法折叠重建，直接丢弃。
	Version: 2, Name: "drop-material-links",
	SQL: `DROP TABLE material_links;`,
}, {
	// 账本结构版本 15（§7.1）：统一索引取代材料索引。旧的两张表是派生数据，直接丢弃，首次检索时全量重建。
	// search_doc 是每个对象一行的结构化字段（筛选与系数用），rowid 与 search_index 对应；search_cursor 记折叠到的序号；
	// search_stat 按本机日期记检索次数、零命中与只有部分命中的次数，不存查询原文。
	Version: 3, Ledger: 15, Name: "ledger-v15-unified-search",
	SQL: `DROP TABLE material_search;
DROP TABLE material_search_state;
CREATE TABLE search_doc (rowid INTEGER PRIMARY KEY, object TEXT NOT NULL UNIQUE, type TEXT NOT NULL, project TEXT NOT NULL,
	phase TEXT NOT NULL, kind TEXT NOT NULL, tags TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE VIRTUAL TABLE search_index USING fts5(title, tags, summary, progress, body, tokenize = 'porter unicode61');
CREATE TABLE search_cursor (name TEXT PRIMARY KEY, seq INTEGER NOT NULL) WITHOUT ROWID;
CREATE TABLE search_stat (day TEXT PRIMARY KEY, queries INTEGER NOT NULL, zero INTEGER NOT NULL, partial INTEGER NOT NULL) WITHOUT ROWID;`,
}}}

// 对象类型。
const (
	TypeItem     = "item"
	TypeMaterial = "material"
	TypeProject  = "project"
	TypeSource   = "source" // 来源事件，只在显式指定时检索
)

// 索引列，顺序即 search_index 的列序；权重见 find.go。
const (
	colTitle = iota
	colTags
	colSummary
	colProgress
	colBody
	numCols
)

// colNames 是命中位置在结果里的叫法。
var colNames = [numCols]string{"title", "tags", "summary", "progress", "body"}

// Doc 是一个对象可检索的原文。Progress 按记录先后排列。读取闸（api 层）按钥匙档位裁剪的就是它。
type Doc struct {
	Type, ID                   string
	Title, Tags, Summary, Body string
	Progress                   []string
}

func (d Doc) col(c int) []string {
	switch c {
	case colTitle:
		return []string{d.Title}
	case colTags:
		return []string{d.Tags}
	case colSummary:
		return []string{d.Summary}
	case colProgress:
		return d.Progress
	}
	return []string{d.Body}
}

func (d Doc) equal(o Doc) bool {
	return d.Title == o.Title && d.Tags == o.Tags && d.Summary == o.Summary && d.Body == o.Body &&
		strings.Join(d.Progress, "\x00") == strings.Join(o.Progress, "\x00") && len(d.Progress) == len(o.Progress)
}

// meta 是 search_doc 里的结构化字段。
type meta struct {
	typ, project, phase, kind string
	tags                      []string
	updated                   int64  // UnixNano；0 表示没有时间（项目、托管型材料）
	path                      string // 托管型材料的位置：结果回路径，不回片段
}

func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

// tokens 做应用层切分：连续的中日韩字符切成相邻二元组（单字成一元组），其余按字母数字词切分并转小写。
// index 为真时中日韩字符另加一元组，使单字也能检索；一元组排在该段二元组之后，带引号的短语（相邻二元组）照样连续。
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

func cursor(tx *store.Tx, name string) (int64, error) {
	var seq int64
	r, err := tx.Query("SELECT seq FROM search_cursor WHERE name = ?", name)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	if r.Next() {
		err = r.Scan(&seq)
	}
	return seq, err
}

func setCursor(tx *store.Tx, name string, seq int64) error {
	_, err := tx.Exec("INSERT INTO search_cursor (name, seq) VALUES (?, ?) ON CONFLICT (name) DO UPDATE SET seq = excluded.seq", name, seq)
	return err
}

func maxSeq(tx *store.Tx, table string) (int64, error) {
	var seq int64
	r, err := tx.Query("SELECT COALESCE(MAX(seq), 0) FROM " + table) // 表名是本包的常量
	if err != nil {
		return 0, err
	}
	defer r.Close()
	if r.Next() {
		err = r.Scan(&seq)
	}
	return seq, err
}

// Sync 把索引折叠到账本当前的样子。需要写事务；没有新事件时只读两行游标。
// 游标为 0 或超过日志头（账本被回滚过）时全量重建。
func Sync(tx *store.Tx) error {
	head, err := maxSeq(tx, "eventlog_event")
	if err != nil {
		return err
	}
	cur, err := cursor(tx, "events")
	if err != nil {
		return err
	}
	switch {
	case cur == 0 || cur > head:
		if err := rebuild(tx); err != nil {
			return err
		}
	case cur < head:
		r, err := tx.Query(`SELECT DISTINCT object FROM eventlog_event WHERE seq > ? AND seq <= ?
			AND (object LIKE 'itm\_%' ESCAPE '\' OR object LIKE 'prj\_%' ESCAPE '\' OR object LIKE 'mat\_%' ESCAPE '\')`, cur, head)
		if err != nil {
			return err
		}
		var ids []string
		for r.Next() {
			var id string
			if err := r.Scan(&id); err != nil {
				r.Close()
				return err
			}
			ids = append(ids, id)
		}
		if err := r.Close(); err != nil {
			return err
		}
		for _, id := range ids {
			if err := reindex(tx, id); err != nil {
				return err
			}
		}
	}
	if err := setCursor(tx, "events", head); err != nil {
		return err
	}
	return syncSources(tx)
}

// rebuild 重建项目、事项、材料三类（来源事件另有游标）。
func rebuild(tx *store.Tx) error {
	if err := removeWhere(tx, "type <> ?", TypeSource); err != nil {
		return err
	}
	ps, err := projects.List(tx)
	if err != nil {
		return err
	}
	for _, p := range ps {
		d, mt := projectDoc(p)
		if err := put(tx, d, mt); err != nil {
			return err
		}
	}
	its, err := items.List(tx, items.Filter{})
	if err != nil {
		return err
	}
	prog := map[string][]string{}
	r, err := tx.Query("SELECT item, text FROM item_progress ORDER BY at, id")
	if err != nil {
		return err
	}
	for r.Next() {
		var id, text string
		if err := r.Scan(&id, &text); err != nil {
			r.Close()
			return err
		}
		prog[id] = append(prog[id], text)
	}
	if err := r.Close(); err != nil {
		return err
	}
	for _, it := range its {
		d, mt := itemDoc(it, prog[it.ID])
		if err := put(tx, d, mt); err != nil {
			return err
		}
	}
	ms, err := materials.List(tx, "")
	if err != nil {
		return err
	}
	for _, m := range ms {
		d, mt, err := materialDoc(tx, m)
		if err != nil {
			return err
		}
		if err := put(tx, d, mt); err != nil {
			return err
		}
	}
	return nil
}

// reindex 从投影重读一个对象并替换它在索引里的那一行；对象已不存在时只删除。
func reindex(tx *store.Tx, id string) error {
	if err := Forget(tx, id); err != nil {
		return err
	}
	d, mt, ok, err := load(tx, id)
	if err != nil || !ok {
		return err
	}
	return put(tx, d, mt)
}

// load 读一个项目、事项或材料的原文与结构化字段；不存在时 ok 为假。
func load(tx *store.Tx, id string) (Doc, meta, bool, error) {
	var (
		d   Doc
		mt  meta
		err error
	)
	switch {
	case strings.HasPrefix(id, "itm_"):
		var it domain.Item
		if it, err = items.Get(tx, id); err == nil {
			var ps []domain.Progress
			if ps, err = items.Progress(tx, id); err == nil {
				texts := make([]string, len(ps))
				for i, p := range ps {
					texts[i] = p.Text
				}
				d, mt = itemDoc(it, texts)
			}
		}
	case strings.HasPrefix(id, "prj_"):
		var p domain.Project
		if p, err = projects.Get(tx, id); err == nil {
			d, mt = projectDoc(p)
		}
	case strings.HasPrefix(id, "mat_"):
		var m materials.Material
		if m, err = materials.Get(tx, id); err == nil {
			d, mt, err = materialDoc(tx, m)
		}
	case strings.HasPrefix(id, "sev_"):
		var ok bool
		d, mt, ok, err = sourceDoc(tx, id)
		if err == nil && !ok {
			return d, mt, false, nil
		}
	default:
		return d, mt, false, nil
	}
	if isNotFound(err) {
		return d, mt, false, nil
	}
	return d, mt, err == nil, err
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

func itemDoc(it domain.Item, progress []string) (Doc, meta) {
	return Doc{Type: TypeItem, ID: it.ID, Title: it.Title, Tags: strings.Join(it.Tags, " "), Summary: it.Summary, Body: it.Body, Progress: progress},
		meta{typ: TypeItem, project: it.Project, phase: string(it.Phase), kind: string(it.Kind), tags: it.Tags, updated: it.UpdatedAt.UnixNano()}
}

func projectDoc(p domain.Project) (Doc, meta) {
	return Doc{Type: TypeProject, ID: p.ID, Title: p.Name, Tags: strings.Join(p.Tags, " "), Summary: p.Intro},
		meta{typ: TypeProject, project: p.ID, phase: string(p.Status), tags: p.Tags}
}

func materialDoc(tx *store.Tx, m materials.Material) (Doc, meta, error) {
	d := Doc{Type: TypeMaterial, ID: m.ID, Title: m.Title, Tags: strings.Join(m.Tags, " ")}
	mt := meta{typ: TypeMaterial, project: m.Project, kind: m.Kind, tags: m.Tags}
	switch {
	case m.Location != "": // 托管型：正文永不进索引，只有位置
		d.Summary, mt.path = m.Location, m.Location
	case m.Link != "":
		d.Summary = m.Link
	}
	if m.CurrentRev != "" {
		v, err := materials.GetRevision(tx, m.CurrentRev)
		if err != nil {
			return d, mt, err
		}
		if m.Location == "" && m.Link == "" {
			d.Body = v.Content
		}
		mt.updated = v.At.UnixNano()
	}
	return d, mt, nil
}

func sourceDoc(tx *store.Tx, id string) (Doc, meta, bool, error) {
	r, err := tx.Query("SELECT source_id, type, subject, summary, occurred_at FROM sources_runner_event WHERE id = ?", id)
	if err != nil {
		return Doc{}, meta{}, false, err
	}
	defer r.Close()
	if !r.Next() {
		return Doc{}, meta{}, false, r.Err()
	}
	var src, typ, subject, summary string
	var at int64
	if err := r.Scan(&src, &typ, &subject, &summary, &at); err != nil {
		return Doc{}, meta{}, false, err
	}
	return Doc{Type: TypeSource, ID: id, Title: subject, Summary: summary}, meta{typ: TypeSource, kind: typ, updated: at}, true, nil
}

// syncSources 追加来源事件表里游标之后的事件（标题与摘要不会再变）；游标超过表头时重建。对话捕获不进索引。
func syncSources(tx *store.Tx) error {
	head, err := maxSeq(tx, "sources_runner_event")
	if err != nil {
		return err
	}
	cur, err := cursor(tx, "sources")
	if err != nil {
		return err
	}
	if cur == head {
		return nil
	}
	if cur > head {
		if err := removeWhere(tx, "type = ?", TypeSource); err != nil {
			return err
		}
		cur = 0
	}
	r, err := tx.Query(`SELECT id, type, subject, summary, occurred_at FROM sources_runner_event
		WHERE seq > ? AND seq <= ? AND source_id NOT LIKE 'capture:%' ORDER BY seq`, cur, head)
	if err != nil {
		return err
	}
	type row struct {
		d  Doc
		mt meta
	}
	var rows []row
	for r.Next() {
		var x row
		if err := r.Scan(&x.d.ID, &x.mt.kind, &x.d.Title, &x.d.Summary, &x.mt.updated); err != nil {
			r.Close()
			return err
		}
		x.d.Type, x.mt.typ = TypeSource, TypeSource
		rows = append(rows, x)
	}
	if err := r.Close(); err != nil {
		return err
	}
	for _, x := range rows {
		if err := Forget(tx, x.d.ID); err != nil {
			return err
		}
		if err := put(tx, x.d, x.mt); err != nil {
			return err
		}
	}
	return setCursor(tx, "sources", head)
}

func put(tx *store.Tx, d Doc, mt meta) error {
	tags, err := json.Marshal(append([]string{}, mt.tags...))
	if err != nil {
		return err
	}
	r, err := tx.Query(`INSERT INTO search_doc (object, type, project, phase, kind, tags, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?) RETURNING rowid`,
		d.ID, mt.typ, mt.project, mt.phase, mt.kind, string(tags), mt.updated)
	if err != nil {
		return err
	}
	var rowid int64
	if r.Next() {
		err = r.Scan(&rowid)
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO search_index (rowid, title, tags, summary, progress, body) VALUES (?, ?, ?, ?, ?, ?)`,
		rowid, join(d.Title), join(d.Tags), join(d.Summary), join(strings.Join(d.Progress, "\n")), join(d.Body))
	return err
}

func removeWhere(tx *store.Tx, where string, args ...any) error {
	if _, err := tx.Exec("DELETE FROM search_index WHERE rowid IN (SELECT rowid FROM search_doc WHERE "+where+")", args...); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM search_doc WHERE "+where, args...)
	return err
}

// Forget 把对象从索引里删掉。物理删除在同一事务里调用它（§6.15），删完立即检索不到。
func Forget(tx *store.Tx, ids ...string) error {
	for _, id := range ids {
		if err := removeWhere(tx, "object = ?", id); err != nil {
			return err
		}
	}
	return nil
}
