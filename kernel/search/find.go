package search

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 每页条数与片段长度（内核常量，不是设置）。
const (
	DefaultLimit = 10
	MaxLimit     = 50
	SnippetChars = 120
	maxTerms     = 32 // 一次查询最多用多少个词；整段长文只取前面的
)

// weights 是 bm25 的字段权重，顺序同索引列：标题 10、标签 6、摘要 4、进展 2、正文 1。
const weights = "10.0, 6.0, 4.0, 2.0, 1.0"

// 结构系数：阶段（事项）、简报降权、时间（按最近变更，半衰期 90 天，下限 0.5）、当前项目。
const (
	halfLife    = 90 * 24 * time.Hour
	minRecent   = 0.5
	hereBoost   = 1.5
	briefFactor = 0.7 // 简报是派生内容，与它总结的事项重复（§7.1）
)

func phaseFactor(p string) float64 {
	switch domain.Phase(p) {
	case domain.PhaseDone:
		return 0.8
	case domain.PhaseProposed, domain.PhaseDropped:
		return 0.5
	}
	return 1
}

// Query 是一次检索。Text 为空时只按筛选列出，最近变更的在前。筛选之间取交集，Tags 要求全部命中。
type Query struct {
	Text  string
	Types []string // 空：事项、材料、项目；来源事件要显式给 TypeSource
	// Project 是过滤；Here 是调用方当前所在的项目，不过滤，只给该项目的命中加权（带 Project 时不起作用）。
	Project, Here  string
	Phase, Kind    string
	Tags           []string
	Item           string    // 与该事项有关系边的材料（§6.3）
	After, Before  time.Time // 最近变更时间的下界（含）与上界（不含）；没有时间的对象（项目、托管型材料）设了就不命中
	IncludeDropped bool      // 已放弃的事项默认不出，计入 Withheld
	Limit          int
	Cursor         string // 上一页给的 NextCursor
	// Record 为真时把这次检索计入零命中统计（只有 GET /search 的第一页计）。
	Record bool
}

// Hit 是一条结果。MatchedIn 是命中的字段（title、tags、summary、progress、body），Coverage 是查询词命中了几个。
type Hit struct {
	Type        string   `json:"type"`
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Project     string   `json:"project,omitempty"`
	ProjectName string   `json:"project_name,omitempty"`
	Phase       string   `json:"phase,omitempty"`
	Kind        string   `json:"kind,omitempty"`
	Updated     string   `json:"updated,omitempty"` // 本机日期 YYYY-MM-DD
	MatchedIn   []string `json:"matched_in"`
	Coverage    string   `json:"coverage,omitempty"`
	Snippet     string   `json:"snippet,omitempty"`
	Path        string   `json:"path,omitempty"` // 托管型材料的位置（不回片段）
}

// Result 是一页结果。Total 是这把钥匙看得到、且没被默认隐去的命中总数；Withheld 是默认没给的（dropped）；
// NextCursor 非空表示还有下一页。SingleChars 表示二元组一个都没中，按单字检索。
type Result struct {
	Hits        []Hit          `json:"hits"`
	Total       int            `json:"total"`
	Withheld    map[string]int `json:"withheld,omitempty"`
	NextCursor  string         `json:"next_cursor,omitempty"`
	SingleChars bool           `json:"single_chars,omitempty"`
}

// Gate 是读取闸：按调用者的钥匙裁剪一个对象的原文。返回 false 表示整条不给；裁过的原文用于复核命中与取片段，
// 计数也只算复核后仍命中的，所以命中与否、片段、条数都不泄露被裁掉的内容。nil 表示不受限。
type Gate func(Doc) (Doc, bool, error)

type term struct {
	expr string   // FTS5 表达式
	toks []string // 切分后的词；带引号的短语有多个
}

func quoted(t string) string {
	isWord := utf8.RuneCountInString(t) >= 3
	for _, r := range t {
		if isCJK(r) || r >= '0' && r <= '9' {
			isWord = false
		}
	}
	if isWord { // 三个字母以上的英文词加前缀匹配；porter 词形还原在 FTS5 里叠加
		return `"` + t + `"*`
	}
	return `"` + t + `"`
}

// parse 把查询切成词：引号里的是短语，其余逐词。去重，最多 maxTerms 个。
func parse(text string) []term {
	var out []term
	seen := map[string]bool{}
	add := func(t term) {
		if !seen[t.expr] && len(out) < maxTerms {
			seen[t.expr] = true
			out = append(out, t)
		}
	}
	for i, part := range strings.Split(text, `"`) {
		ts := tokens(part, false)
		if i%2 == 1 && len(ts) > 1 {
			add(term{expr: `"` + strings.Join(ts, " ") + `"`, toks: ts})
			continue
		}
		for _, t := range ts {
			add(term{expr: quoted(t), toks: []string{t}})
		}
	}
	return out
}

// singleChars 是二元组全落空时的退路：查询里的中日韩单字。
func singleChars(text string) []term {
	var out []term
	seen := map[string]bool{}
	for _, r := range text {
		if isCJK(r) && !seen[string(r)] && len(out) < maxTerms {
			seen[string(r)] = true
			out = append(out, term{expr: `"` + string(r) + `"`, toks: []string{string(r)}})
		}
	}
	return out
}

func orExpr(ts []term) string {
	es := make([]string, len(ts))
	for i, t := range ts {
		es[i] = t.expr
	}
	return strings.Join(es, " OR ")
}

type cand struct {
	rowid                         int64
	id, typ, project, phase, kind string
	updated                       int64
	path                          string
	score                         float64
	doc                           *Doc                // 已读出的原文（裁剪后）；nil 表示还没读
	trimmed                       bool                // 读取闸裁过，命中要在裁后的原文上复核
	match                         [][numCols][]string // 每个词在各列命中的原文片段（highlight），nil 表示还没分析
}

func (c *cand) withheld(q Query) bool {
	return c.typ == TypeItem && c.phase == string(domain.PhaseDropped) && q.Phase == "" && !q.IncludeDropped
}

// Search 先把索引折叠到最新，再查询；Record 为真时计数。gate 在同一事务里构造读取闸，nil 表示不受限。
func Search(ctx context.Context, st *store.Store, q Query, gate func(*store.Tx) Gate) (Result, error) {
	var res Result
	err := st.Update(ctx, func(tx *store.Tx) error {
		if err := Sync(tx); err != nil {
			return err
		}
		var g Gate
		if gate != nil {
			g = gate(tx)
		}
		var err error
		if res, err = Find(tx, q, g); err != nil {
			return err
		}
		if q.Record && q.Cursor == "" && strings.TrimSpace(q.Text) != "" {
			return record(tx, res)
		}
		return nil
	})
	return res, err
}

// Rank 返回查询命中的全部事项 id（含已放弃），相关的在前；事项列表的全文筛选用它（GET /items?q=）。
func Rank(ctx context.Context, st *store.Store, text string, gate func(*store.Tx) Gate) ([]string, error) {
	var ids []string
	err := st.Update(ctx, func(tx *store.Tx) error {
		if err := Sync(tx); err != nil {
			return err
		}
		var g Gate
		if gate != nil {
			g = gate(tx)
		}
		cs, _, _, err := candidates(tx, Query{Text: text, Types: []string{TypeItem}, IncludeDropped: true}, g)
		for _, c := range cs {
			ids = append(ids, c.id)
		}
		return err
	})
	return ids, err
}

// Find 在已同步的索引上查询一页。
func Find(tx *store.Tx, q Query, gate Gate) (Result, error) {
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultLimit
	}
	limit = min(limit, MaxLimit)
	offset := 0
	if q.Cursor != "" {
		n, err := strconv.Atoi(q.Cursor)
		if err != nil || n < 0 {
			return Result{}, domain.NewError(domain.CodeInvalidRequest, "cursor is not one this search returned", "drop cursor to start from the first page")
		}
		offset = n
	}
	cs, terms, single, err := candidates(tx, q, gate)
	if err != nil {
		return Result{}, err
	}
	res := Result{Hits: []Hit{}, SingleChars: single}
	var shown []*cand
	dropped := 0
	for _, c := range cs {
		if c.withheld(q) {
			dropped++
			continue
		}
		shown = append(shown, c)
	}
	if dropped > 0 {
		res.Withheld = map[string]int{"dropped": dropped}
	}
	res.Total = len(shown)
	if offset >= len(shown) {
		return res, nil
	}
	page := shown[offset:min(len(shown), offset+limit)]
	if offset+len(page) < len(shown) {
		res.NextCursor = strconv.Itoa(offset + len(page))
	}
	if err := ensureDocs(tx, page); err != nil {
		return res, err
	}
	if len(terms) > 0 {
		if err := analyze(tx, page, terms); err != nil {
			return res, err
		}
	}
	names, err := projectNames(tx)
	if err != nil {
		return res, err
	}
	for _, c := range page {
		h := Hit{Type: c.typ, ID: c.id, Title: c.doc.Title, Project: c.project, ProjectName: names[c.project], Kind: c.kind, MatchedIn: []string{}}
		if c.typ == TypeItem {
			h.Phase = c.phase
		}
		if c.typ == TypeProject {
			h.Project, h.ProjectName = "", ""
		}
		if c.updated > 0 {
			h.Updated = time.Unix(0, c.updated).In(time.Local).Format("2006-01-02")
		}
		var spans [numCols][]string
		if len(terms) > 0 {
			hit := 0
			cols := [numCols]bool{}
			for _, m := range c.match {
				matched := false
				for col, ss := range m {
					if len(ss) > 0 {
						cols[col], matched = true, true
						spans[col] = append(spans[col], ss...)
					}
				}
				if matched {
					hit++
				}
			}
			for col, on := range cols {
				if on {
					h.MatchedIn = append(h.MatchedIn, colNames[col])
				}
			}
			h.Coverage = fmt.Sprintf("%d/%d", hit, len(terms))
		}
		if h.Path = c.path; h.Path == "" {
			h.Snippet = snippet(*c.doc, spans)
		}
		res.Hits = append(res.Hits, h)
	}
	return res, nil
}

// candidates 返回全部命中（含默认隐去的已放弃事项），已过读取闸并按分数排好；terms 是实际用上的词，
// single 为真表示二元组一个都没中、退到了单字。
func candidates(tx *store.Tx, q Query, gate Gate) (cs []*cand, terms []term, single bool, err error) {
	types := q.Types
	if len(types) == 0 {
		types = []string{TypeItem, TypeMaterial, TypeProject}
	}
	for _, t := range types {
		if !slices.Contains([]string{TypeItem, TypeMaterial, TypeProject, TypeSource}, t) {
			return nil, nil, false, domain.NewError(domain.CodeInvalidRequest, "unknown type "+t, "type is one of item, material, project, source")
		}
	}
	if strings.TrimSpace(q.Text) != "" {
		if terms = parse(q.Text); len(terms) == 0 {
			return nil, nil, false, domain.NewError(domain.CodeInvalidRequest, "query has no searchable text", "use letters, digits or CJK characters")
		}
	}
	if cs, err = query(tx, q, types, terms); err != nil {
		return nil, nil, false, err
	}
	if len(cs) == 0 && len(terms) > 0 {
		// 二元组全落空时退到单字；查询本来就只有单字时不重复查。
		if chars := singleChars(q.Text); len(chars) > 0 && orExpr(chars) != orExpr(terms) {
			terms, single = chars, true
			if cs, err = query(tx, q, types, terms); err != nil {
				return nil, nil, false, err
			}
		}
	}
	if gate != nil {
		kept := cs[:0]
		var recheck []*cand
		for _, c := range cs {
			d, mt, ok, err := load(tx, c.id)
			if err != nil {
				return nil, nil, false, err
			}
			if !ok {
				continue
			}
			c.path = mt.path
			g, visible, err := gate(d)
			if err != nil {
				return nil, nil, false, err
			}
			if !visible {
				continue
			}
			c.doc, c.trimmed = &g, !g.equal(d)
			if c.trimmed && len(terms) > 0 {
				recheck = append(recheck, c)
			}
			kept = append(kept, c)
		}
		cs = kept
		if len(recheck) > 0 {
			if err := analyze(tx, recheck, terms); err != nil {
				return nil, nil, false, err
			}
			cs = slices.DeleteFunc(cs, func(c *cand) bool {
				if !c.trimmed || len(terms) == 0 {
					return false
				}
				for _, m := range c.match {
					for _, ss := range m {
						if len(ss) > 0 {
							return false
						}
					}
				}
				return true // 只在裁掉的部分命中：这把钥匙搜不到它
			})
		}
	}
	return cs, terms, single, nil
}

func query(tx *store.Tx, q Query, types []string, terms []term) ([]*cand, error) {
	var (
		sql   string
		where []string
		args  []any
	)
	if len(terms) > 0 {
		sql = `SELECT d.rowid, d.object, d.type, d.project, d.phase, d.kind, d.updated_at, bm25(search_index, ` + weights + `)
			FROM search_index JOIN search_doc d ON d.rowid = search_index.rowid`
		where, args = append(where, "search_index MATCH ?"), append(args, orExpr(terms))
	} else {
		sql = `SELECT d.rowid, d.object, d.type, d.project, d.phase, d.kind, d.updated_at, 0.0 FROM search_doc d`
	}
	where = append(where, "d.type IN (?"+strings.Repeat(", ?", len(types)-1)+")")
	for _, t := range types {
		args = append(args, t)
	}
	for col, v := range map[string]string{"d.project": q.Project, "d.phase": q.Phase, "d.kind": q.Kind} {
		if v != "" {
			where, args = append(where, col+" = ?"), append(args, v)
		}
	}
	for _, t := range q.Tags {
		where, args = append(where, "EXISTS (SELECT 1 FROM json_each(d.tags) WHERE value = ?)"), append(args, t)
	}
	if q.Item != "" {
		where, args = append(where, "EXISTS (SELECT 1 FROM item_relations l WHERE l.to_id = d.object AND l.from_id = ?)"), append(args, q.Item)
	}
	if !q.After.IsZero() {
		where, args = append(where, "d.updated_at >= ?"), append(args, q.After.UnixNano())
	}
	if !q.Before.IsZero() {
		where, args = append(where, "d.updated_at > 0 AND d.updated_at < ?"), append(args, q.Before.UnixNano())
	}
	r, err := tx.Query(sql+" WHERE "+strings.Join(where, " AND "), args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	now := clock.Now()
	var out []*cand
	for r.Next() {
		c := &cand{}
		var bm float64
		if err := r.Scan(&c.rowid, &c.id, &c.typ, &c.project, &c.phase, &c.kind, &c.updated, &bm); err != nil {
			return nil, err
		}
		f := 1.0
		if c.typ == TypeItem {
			f = phaseFactor(c.phase)
			if c.kind == string(domain.KindBrief) {
				f *= briefFactor
			}
		}
		if c.updated > 0 {
			f *= max(minRecent, math.Pow(0.5, float64(now.Sub(time.Unix(0, c.updated)))/float64(halfLife)))
		}
		if q.Here != "" && q.Project == "" && c.project == q.Here {
			f *= hereBoost
		}
		if len(terms) > 0 {
			c.score = -bm * f // bm25 越小越相关
		} else {
			c.score = float64(c.updated) // 没有查询词：最近变更的在前
		}
		out = append(out, c)
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(out, func(a, b *cand) int {
		if a.score != b.score {
			if a.score > b.score {
				return -1
			}
			return 1
		}
		return strings.Compare(a.id, b.id)
	})
	return out, nil
}

// ensureDocs 读出这一页的原文。过了读取闸的候选在闸里已读过（裁剪后的），不受限的调用者只在这里读。
func ensureDocs(tx *store.Tx, cs []*cand) error {
	for _, c := range cs {
		if c.doc != nil {
			continue
		}
		d, mt, _, err := load(tx, c.id)
		if err != nil {
			return err
		}
		c.doc, c.path = &d, mt.path
	}
	return nil
}

const markOpen, markClose = "\x02", "\x03"

// analyze 逐词查出每个候选在各列命中的原文片段（FTS5 highlight，已含 porter 与前缀的匹配）。
// 读取闸裁过的候选只保留在裁后原文里仍出现的片段。
func analyze(tx *store.Tx, cs []*cand, terms []term) error {
	byRow := map[int64]*cand{}
	ids := make([]string, 0, len(cs))
	for _, c := range cs {
		if c.match != nil {
			continue
		}
		c.match = make([][numCols][]string, len(terms))
		byRow[c.rowid] = c
		ids = append(ids, strconv.FormatInt(c.rowid, 10))
	}
	if len(ids) == 0 {
		return nil
	}
	cols := make([]string, numCols)
	for i := range cols {
		cols[i] = fmt.Sprintf("highlight(search_index, %d, char(2), char(3))", i)
	}
	sql := "SELECT rowid, " + strings.Join(cols, ", ") + " FROM search_index WHERE search_index MATCH ? AND rowid IN (" + strings.Join(ids, ", ") + ")"
	for i, t := range terms {
		r, err := tx.Query(sql, t.expr)
		if err != nil {
			return err
		}
		for r.Next() {
			var rowid int64
			var hs [numCols]string
			if err := r.Scan(&rowid, &hs[0], &hs[1], &hs[2], &hs[3], &hs[4]); err != nil {
				r.Close()
				return err
			}
			c := byRow[rowid]
			for col, h := range hs {
				for _, span := range marked(h) {
					if !c.trimmed || visible(c.doc.col(col), span) {
						c.match[i][col] = append(c.match[i][col], span)
					}
				}
			}
		}
		if err := r.Close(); err != nil {
			return err
		}
	}
	return nil
}

// marked 取出 highlight 标出的片段（索引里存的是切分后的词，片段是一个或几个相邻的词）。
func marked(h string) []string {
	var out []string
	for {
		i := strings.Index(h, markOpen)
		if i < 0 {
			return out
		}
		h = h[i+len(markOpen):]
		j := strings.Index(h, markClose)
		if j < 0 {
			return out
		}
		out = append(out, h[:j])
		h = h[j+len(markClose):]
	}
}

// visible 报告片段里的每个词都出现在裁剪后的原文里。裁剪只留前缀或整段去掉，所以这足以说明命中没有用到被裁的内容。
func visible(texts []string, span string) bool {
	have := map[string]bool{}
	for _, t := range tokens(strings.Join(texts, "\n"), true) {
		have[t] = true
	}
	for _, w := range strings.Fields(span) {
		if !have[w] {
			return false
		}
	}
	return true
}

// snippet 从正文类字段里取命中最集中的一段（命中在进展时取那一条进展）；正文类字段都没命中时取摘要或正文开头。
func snippet(d Doc, spans [numCols][]string) string {
	best, bestN, bestAt := "", 0, 0
	for _, col := range []int{colSummary, colProgress, colBody} {
		var words []string
		for _, s := range spans[col] {
			words = append(words, strings.Fields(s)...)
		}
		if len(words) == 0 {
			continue
		}
		texts := d.col(col)
		for i := len(texts) - 1; i >= 0; i-- { // 进展从最近的找起
			if n, at := densest(texts[i], words); n > bestN {
				best, bestN, bestAt = texts[i], n, at
			}
		}
	}
	if bestN == 0 {
		for _, s := range []string{d.Summary, d.Body} {
			if strings.TrimSpace(s) != "" {
				return excerpt(s, 0)
			}
		}
		if len(d.Progress) > 0 {
			return excerpt(d.Progress[len(d.Progress)-1], 0)
		}
		return ""
	}
	return excerpt(best, bestAt)
}

// densest 找出 text 里命中词最集中的窗口：返回窗口里的命中数与窗口起点（按字符）。
func densest(text string, words []string) (int, int) {
	rs := []rune(text)
	low := []rune(strings.ToLower(text))
	if len(low) != len(rs) { // 个别字符小写后变长：按原文找
		low = rs
	}
	var pos []int
	for _, w := range words {
		wr := []rune(w)
		for i, n := 0, 0; i+len(wr) <= len(low) && n < 50; i++ {
			if string(low[i:i+len(wr)]) == w {
				pos = append(pos, i)
				n++
			}
		}
	}
	if len(pos) == 0 {
		return 0, 0
	}
	slices.Sort(pos)
	bestN, bestAt := 0, 0
	for i, p := range pos {
		n := 0
		for _, q := range pos[i:] {
			if q >= p+SnippetChars*3/4 {
				break
			}
			n++
		}
		if n > bestN {
			bestN, bestAt = n, p
		}
	}
	return bestN, bestAt
}

// excerpt 从 at 前面一点起取 SnippetChars 个字符，空白压成一个空格。
func excerpt(text string, at int) string {
	rs := []rune(text)
	start := max(0, at-SnippetChars/6)
	end := min(len(rs), start+SnippetChars)
	s := strings.Join(strings.Fields(string(rs[start:end])), " ")
	if start > 0 {
		s = "…" + s
	}
	if end < len(rs) {
		s += "…"
	}
	return s
}

func projectNames(tx *store.Tx) (map[string]string, error) {
	ps, err := projects.List(tx)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, p := range ps {
		out[p.ID] = p.Name
	}
	return out, nil
}

// Stats 是零命中统计：检索总次数、零命中次数、有结果但排第一的也没覆盖全部查询词的次数；Since 是第一次计数的本机日期。
type Stats struct {
	Queries int64  `json:"queries"`
	Zero    int64  `json:"zero"`
	Partial int64  `json:"partial"`
	Since   string `json:"since,omitempty"`
}

func record(tx *store.Tx, res Result) error {
	zero, partial := 0, 0
	if res.Total == 0 {
		zero = 1
	} else if n, d, ok := strings.Cut(res.Hits[0].Coverage, "/"); ok && n != d {
		partial = 1
	}
	_, err := tx.Exec(`INSERT INTO search_stat (day, queries, zero, partial) VALUES (?, 1, ?, ?)
		ON CONFLICT (day) DO UPDATE SET queries = queries + 1, zero = zero + excluded.zero, partial = partial + excluded.partial`,
		clock.Now().In(time.Local).Format("2006-01-02"), zero, partial)
	return err
}

// CurrentStats 读零命中统计（健康视图用）。
func CurrentStats(tx *store.Tx) (Stats, error) {
	var s Stats
	r, err := tx.Query("SELECT COALESCE(SUM(queries), 0), COALESCE(SUM(zero), 0), COALESCE(SUM(partial), 0), COALESCE(MIN(day), '') FROM search_stat")
	if err != nil {
		return s, err
	}
	defer r.Close()
	if r.Next() {
		err = r.Scan(&s.Queries, &s.Zero, &s.Partial, &s.Since)
	}
	return s, err
}
