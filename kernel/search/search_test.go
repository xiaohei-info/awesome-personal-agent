package search

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

func agent(lvl authz.Level) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "a", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: lvl}
}

var (
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}
	worker   = agent(authz.LevelWorker)
	observer = agent(authz.LevelObserver)
)

type env struct {
	st  *store.Store
	x   *command.Executor
	now time.Time
	prj string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, materials.Module, clock.Module, items.Module, runner.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	e.x = command.New(st, func() time.Time { e.now = e.now.Add(time.Hour); return e.now })
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := materials.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := items.Register(e.x, nil); err != nil {
		t.Fatal(err)
	}
	e.prj = e.newProject(t, "acc-open")
	return e
}

func (e *env) do(t *testing.T, id command.Identity, cmd string, in any) (json.RawMessage, error) {
	t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", WebURL: "http://web/m"})
	return r.Value, err
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) json.RawMessage {
	t.Helper()
	v, err := e.do(t, id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return v
}

func (e *env) newProject(t *testing.T, name string) string {
	var out struct{ ID string }
	json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": name}), &out)
	return out.ID
}

// commit 经命令模型写一条自有型修订（minutes、reference 默认 direct，立即成为 current）。
func (e *env) commit(t *testing.T, in map[string]any) materials.Result {
	t.Helper()
	if in["reason"] == nil {
		in["reason"] = "r"
	}
	var out materials.Result
	json.Unmarshal(e.must(t, worker, "material.commit", in), &out)
	return out
}

func (e *env) note(t *testing.T, title, content string, tags ...string) materials.Result {
	return e.commit(t, map[string]any{"project": e.prj, "kind": "minutes", "title": title, "content": content, "tags": tags})
}

// hosted 直接落一行托管型登记：托管型登记命令不在本节点范围，这里只构造它的账本形态。
func (e *env) hosted(t *testing.T, title, loc string, tags ...string) string {
	t.Helper()
	b, _ := json.Marshal(tags)
	id := "mat_hosted_" + title
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO materials (id, project, kind, title, tags, location, current_rev) VALUES (?, ?, 'design', ?, ?, ?, '')",
			id, e.prj, title, string(b), loc)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (e *env) find(t *testing.T, q Query, gate ...Gate) Result {
	t.Helper()
	var g func(*store.Tx) Gate
	if len(gate) > 0 {
		g = func(*store.Tx) Gate { return gate[0] }
	}
	res, err := Search(bg, e.st, q, g)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (e *env) search(t *testing.T, q Query) []Hit { return e.find(t, q).Hits }

func ids(hs []Hit) []string {
	out := []string{}
	for _, h := range hs {
		out = append(out, h.ID)
	}
	return out
}

func (e *env) item(t *testing.T, project, phase, title, body string) string {
	t.Helper()
	var r struct{ ID string }
	json.Unmarshal(e.must(t, owner, "item.create", map[string]any{"kind": "task", "phase": phase, "title": title, "body": body, "project": project}), &r)
	return r.ID
}

func (e *env) progress(t *testing.T, id, text string) {
	t.Helper()
	e.must(t, owner, "item.progress", map[string]any{"id": id, "text": text})
}

func TestTokens(t *testing.T) {
	cases := []struct {
		in    string
		index bool
		want  []string
	}{
		{"数据库设计", false, []string{"数据", "据库", "库设", "设计"}},
		{"数", false, []string{"数"}},
		{"Hello, 世界 v2", false, []string{"hello", "世界", "v2"}},
		{"検索エンジン", false, []string{"検索", "索エ", "エン", "ンジ", "ジン"}},
		{"한국어", false, []string{"한국", "국어"}},
		{"数据", true, []string{"数据", "数", "据"}},
		{"  ,, ", false, nil},
	}
	for _, c := range cases {
		if got := tokens(c.in, c.index); !reflect.DeepEqual(got, c.want) {
			t.Errorf("tokens(%q, %v) = %q, want %q", c.in, c.index, got, c.want)
		}
	}
}

// 一整句不带空格的自然问法：任一二元组命中、按 bm25 字段加权排名，标题全中的排第一；
// 进展里命中的标明 progress；跨项目检索不需要 --project。
func TestNaturalQueryRanksAcrossProjects(t *testing.T) {
	e := setup(t)
	other := e.newProject(t, "北辰")
	target := e.item(t, e.prj, "open", "星图按预算比例设上下限", "")
	e.progress(t, target, "上限 30%，下限 10%")
	related := e.item(t, e.prj, "done", "预算规则梳理", "")
	e.item(t, e.prj, "open", "无关的事", "完全不相干")
	viaProgress := e.item(t, other, "open", "账号整理", "")
	e.progress(t, viaProgress, "北辰需要加账号")

	res := e.find(t, Query{Text: "星图按预算比例设上下限"})
	if got := ids(res.Hits); len(got) != 2 || got[0] != target || got[1] != related {
		t.Fatalf("ranking: %v", got)
	}
	if h := res.Hits[0]; !slices.Contains(h.MatchedIn, "title") || !slices.Contains(h.MatchedIn, "progress") || h.Coverage != "10/10" || h.ProjectName != "acc-open" || h.Phase != "open" {
		t.Fatalf("top hit: %+v", h)
	}
	cross := e.find(t, Query{Text: "北辰 加账号"})
	if len(cross.Hits) == 0 || cross.Hits[0].ID != viaProgress || !slices.Contains(cross.Hits[0].MatchedIn, "progress") {
		t.Fatalf("cross-project: %+v", cross.Hits)
	}
	if !strings.Contains(cross.Hits[0].Snippet, "加账号") {
		t.Fatalf("snippet should come from the matching progress: %q", cross.Hits[0].Snippet)
	}
	// 项目本身也可检索；Here 只加权不过滤。
	if got := ids(e.search(t, Query{Text: "北辰", Types: []string{TypeProject}})); !reflect.DeepEqual(got, []string{other}) {
		t.Fatalf("project hit: %v", got)
	}
	if got := ids(e.search(t, Query{Text: "预算", Here: other})); len(got) != 2 {
		t.Fatalf("here must not filter: %v", got)
	}
}

// 英文：整词、前缀与 porter 词形还原（merged 命中 merging），大小写不敏感，中英混排；带引号是短语。
func TestEnglishStemPrefixAndPhrase(t *testing.T) {
	e := setup(t)
	a := e.item(t, e.prj, "open", "Merging the release branch", "")
	b := e.item(t, e.prj, "open", "PR 合并 checklist", "connections are flaky")
	for q, want := range map[string]string{"merged": a, "MERGE": a, "connected": b, "connect": b, "pr 合并": b, "check": b} {
		if got := ids(e.search(t, Query{Text: q})); len(got) == 0 || got[0] != want {
			t.Errorf("%q = %v, want %s first", q, got, want)
		}
	}
	if got := e.search(t, Query{Text: "merged"}); !slices.Contains(got[0].MatchedIn, "title") || got[0].Coverage != "1/1" {
		t.Fatalf("stem hit must be explained: %+v", got[0])
	}
	if got := ids(e.search(t, Query{Text: `"release branch"`})); !reflect.DeepEqual(got, []string{a}) {
		t.Fatalf("phrase: %v", got)
	}
	if got := ids(e.search(t, Query{Text: `"branch release"`})); len(got) != 0 {
		t.Fatalf("phrase order must matter: %v", got)
	}
}

// 已放弃的进索引但默认不出，计入 withheld；显式要或按阶段筛时出现。二元组全落空时退到单字。
func TestDroppedWithheldAndSingleCharFallback(t *testing.T) {
	e := setup(t)
	live := e.item(t, e.prj, "open", "护照续签", "")
	gone := e.item(t, e.prj, "dropped", "护照照片", "")
	res := e.find(t, Query{Text: "护照"})
	if !reflect.DeepEqual(ids(res.Hits), []string{live}) || res.Total != 1 || res.Withheld["dropped"] != 1 {
		t.Fatalf("default: %+v", res)
	}
	if got := ids(e.search(t, Query{Text: "护照", IncludeDropped: true})); len(got) != 2 {
		t.Fatalf("include dropped: %v", got)
	}
	if got := ids(e.search(t, Query{Text: "护照", Phase: "dropped"})); !reflect.DeepEqual(got, []string{gone}) {
		t.Fatalf("phase dropped: %v", got)
	}
	res = e.find(t, Query{Text: "签护"}) // 没有这个二元组：按单字
	if !res.SingleChars || len(res.Hits) == 0 || res.Hits[0].ID != live {
		t.Fatalf("single-char fallback: %+v", res)
	}
}

// 决定的问题与选项随事项可搜（命中在正文）；简报与同样命中的事项相比排后。
func TestDecisionTextAndBriefRank(t *testing.T) {
	e := setup(t)
	var r struct{ ID string }
	json.Unmarshal(e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "发布安排", "project": e.prj,
		"ask": map[string]any{"question": "灰度先放哪个机房", "options": []map[string]any{{"id": "a", "label": "北京机房"}, {"id": "b", "label": "上海机房"}}}}), &r)
	if got := e.search(t, Query{Text: "灰度 上海"}); len(got) != 1 || got[0].ID != r.ID || !slices.Contains(got[0].MatchedIn, "body") {
		t.Fatalf("decision text: %+v", got)
	}
	task := e.item(t, e.prj, "open", "周报汇总", "")
	json.Unmarshal(e.must(t, owner, "item.create", map[string]any{"kind": "brief", "phase": "open", "title": "周报汇总", "project": e.prj}), &r)
	if got := ids(e.search(t, Query{Text: "周报汇总"})); !reflect.DeepEqual(got, []string{task, r.ID}) {
		t.Fatalf("brief must rank after the item: %v", got)
	}
}

// 增量：写一条进展后立即能按进展里的词搜到；丢掉索引后重建，结果逐字相同；Forget 后检索不到、索引里没有该行。
func TestIncrementalRebuildAndForget(t *testing.T) {
	e := setup(t)
	id := e.item(t, e.prj, "open", "季度复盘", "")
	if len(e.search(t, Query{Text: "香蕉"})) != 0 {
		t.Fatal("nothing yet")
	}
	e.progress(t, id, "结论：改用香蕉方案")
	first := e.find(t, Query{Text: "香蕉 复盘"})
	if !reflect.DeepEqual(ids(first.Hits), []string{id}) {
		t.Fatalf("progress not indexed incrementally: %v", ids(first.Hits))
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, s := range []string{"DELETE FROM search_index", "DELETE FROM search_doc", "DELETE FROM search_cursor"} {
			if _, err := tx.Exec(s); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if again := e.find(t, Query{Text: "香蕉 复盘"}); !reflect.DeepEqual(again, first) {
		t.Fatalf("rebuild differs:\n%+v\n%+v", again, first)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error { return Forget(tx, id) }); err != nil {
		t.Fatal(err)
	}
	var n int
	e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query("SELECT COUNT(*) FROM search_doc WHERE object = ?", id)
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Scan(&n)
	})
	if n != 0 {
		t.Fatal("forgotten object still in the index")
	}
}

// 读取闸裁过的对象在裁后的原文上复核：只在被裁部分命中的整条不出、不计数；片段只取裁后的内容。
func TestGateRechecksTrimmedContent(t *testing.T) {
	e := setup(t)
	titleHit := e.item(t, e.prj, "open", "机密项目代号", "正文也提到代号")
	bodyOnly := e.item(t, e.prj, "open", "普通标题", "正文提到代号")
	hidden := e.item(t, e.prj, "open", "代号清单", "")
	meta := func(d Doc) (Doc, bool, error) {
		if d.ID == hidden {
			return d, false, nil
		}
		d.Body, d.Summary, d.Progress = "", "", nil // 元信息档：只留标题与结构化字段
		return d, true, nil
	}
	res := e.find(t, Query{Text: "代号"}, meta)
	if !reflect.DeepEqual(ids(res.Hits), []string{titleHit}) || res.Total != 1 {
		t.Fatalf("gated: %+v", res)
	}
	if h := res.Hits[0]; !reflect.DeepEqual(h.MatchedIn, []string{"title"}) || strings.Contains(h.Snippet, "正文") {
		t.Fatalf("gated hit leaks the body: %+v", h)
	}
	if got := ids(e.search(t, Query{Text: "代号"})); len(got) != 3 || !slices.Contains(got, bodyOnly) {
		t.Fatalf("ungated: %v", got)
	}
}

// 分页：游标续页能翻完、不重不漏；零命中与只有部分命中计数，不记查询原文。
func TestCursorAndStats(t *testing.T) {
	e := setup(t)
	for i := range 5 {
		e.item(t, e.prj, "open", "周报 "+strconv.Itoa(i), "")
	}
	seen := map[string]bool{}
	q := Query{Text: "周报", Limit: 2, Record: true}
	for page := 0; ; page++ {
		res := e.find(t, q)
		if res.Total != 5 || len(res.Hits) > 2 {
			t.Fatalf("page %d: %+v", page, res)
		}
		for _, h := range res.Hits {
			if seen[h.ID] {
				t.Fatalf("duplicate %s", h.ID)
			}
			seen[h.ID] = true
		}
		if res.NextCursor == "" {
			break
		}
		q.Cursor = res.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("pages missed items: %d", len(seen))
	}
	e.find(t, Query{Text: "不存在的东西", Record: true})
	e.find(t, Query{Text: "周报 不存在", Record: true})
	var st Stats
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { st, err = CurrentStats(tx); return }); err != nil {
		t.Fatal(err)
	}
	if st.Queries != 3 || st.Zero != 1 || st.Partial != 1 || st.Since == "" { // 续页不计
		t.Fatalf("stats: %+v", st)
	}
}

// flow-material-owned：自有型材料按标题、正文、标签检索，结果给出命中处的片段。
func TestOwnedMaterialTitleBodyTags(t *testing.T) {
	e := setup(t)
	body := e.note(t, "周会纪要", "本周讨论了向量数据库的选型，结论是继续使用 SQLite。\n\nNext: Postgres is out of scope.")
	title := e.note(t, "数据迁移方案", "无关正文")
	tag := e.note(t, "杂记", "另一份内容", "分词")
	e.note(t, "无关", "完全不相干的文字")
	mat := []string{TypeMaterial}

	got := e.search(t, Query{Text: "数据库", Types: mat})
	if len(got) == 0 || got[0].ID != body.Material || !strings.Contains(got[0].Snippet, "数据库") || got[0].Path != "" {
		t.Fatalf("body hit: %+v", got)
	}
	if h := e.search(t, Query{Text: "迁移", Types: mat}); !reflect.DeepEqual(ids(h), []string{title.Material}) {
		t.Fatalf("title hit: %v", ids(h))
	}
	if h := e.search(t, Query{Text: "分词", Types: mat}); !reflect.DeepEqual(ids(h), []string{tag.Material}) {
		t.Fatalf("tag hit: %v", ids(h))
	}
	if h := e.search(t, Query{Text: "POSTGRES sqlite", Types: mat}); !reflect.DeepEqual(ids(h), []string{body.Material}) {
		t.Fatalf("english: %v", ids(h))
	}
}

// 新修订成为 current 后检索立即反映；被取代的旧正文不再命中；proposed 修订在确认前不进索引。
func TestIndexFollowsCurrentRevision(t *testing.T) {
	e := setup(t)
	a := e.note(t, "笔记", "旧内容苹果")
	if len(e.search(t, Query{Text: "苹果"})) != 1 {
		t.Fatal("old body must hit")
	}
	var b materials.Result
	json.Unmarshal(e.must(t, owner, "material.commit", map[string]any{"id": a.Material, "content": "新内容香蕉", "based_on": a.Revision, "reason": "r"}), &b)
	if h := e.search(t, Query{Text: "苹果"}); len(h) != 0 {
		t.Fatalf("superseded body still indexed: %v", ids(h))
	}
	if h := e.search(t, Query{Text: "香蕉"}); len(h) != 1 {
		t.Fatal("new body must hit")
	}
	p := e.commit(t, map[string]any{"id": a.Material, "content": "待确认的橘子", "based_on": b.Revision})
	if p.State != materials.StateProposed {
		t.Fatalf("want proposed, got %s", p.State)
	}
	if h := e.search(t, Query{Text: "橘子"}); len(h) != 0 {
		t.Fatalf("proposed revision indexed: %v", ids(h))
	}
	in, _ := json.Marshal(map[string]any{"id": p.Revision, "reason": "ok"})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "revision.confirm", Input: in, Owner: "owner", WebURL: "http://web/m"}); err != nil {
		t.Fatal(err)
	}
	if h := e.search(t, Query{Text: "橘子"}); len(h) != 1 {
		t.Fatal("confirmed revision must hit")
	}
}

// flow-material-hosted：托管型只索引标题、路径、标签；结果回路径，不回片段。
func TestHostedIndexesTitlePathTagsOnly(t *testing.T) {
	e := setup(t)
	h := e.hosted(t, "ADR-检索选型", "github.com/o/r:docs/adr/0007-search.md", "架构")
	e.note(t, "无关", "正文里提到别的")
	for _, q := range []string{"检索", "adr", "0007", "docs", "架构"} {
		got := e.search(t, Query{Text: q, Types: []string{TypeMaterial}})
		if len(got) == 0 || got[0].ID != h || got[0].Path != "github.com/o/r:docs/adr/0007-search.md" || got[0].Snippet != "" {
			t.Fatalf("query %q: %+v", q, got)
		}
	}
}

func TestStructuredFilters(t *testing.T) {
	e := setup(t)
	other := e.newProject(t, "home")
	a := e.note(t, "甲", "共同词 alpha", "x", "y")
	b := e.commit(t, map[string]any{"project": e.prj, "kind": "reference", "title": "乙", "content": "共同词 beta", "tags": []string{"x"}})
	c := e.commit(t, map[string]any{"project": other, "kind": "minutes", "title": "丙", "content": "共同词 gamma", "tags": []string{"y"}})
	h := e.hosted(t, "共同词托管", "r:p.md", "x")

	check := func(name string, q Query, want ...string) {
		t.Helper()
		q.Text, q.Types = "共同词", []string{TypeMaterial}
		if got := ids(e.search(t, q)); !sameSet(got, want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
	}
	check("none", Query{}, a.Material, b.Material, c.Material, h)
	check("project", Query{Project: other}, c.Material)
	check("kind", Query{Kind: "reference"}, b.Material)
	check("tag", Query{Tags: []string{"x"}}, a.Material, b.Material, h)
	check("tags AND", Query{Tags: []string{"x", "y"}}, a.Material)

	// 关联事项：读 item.relate 以材料为终点的类型化关系，移除后不再命中；不带查询词也能按它列出。
	i1 := e.item(t, e.prj, "open", "item-1", "")
	e.must(t, owner, "item.relate", map[string]any{"from": i1, "to": a.Material, "type": domain.RelBasis})
	check("item", Query{Item: i1}, a.Material)
	if got := ids(e.search(t, Query{Item: i1, Types: []string{TypeMaterial}})); !reflect.DeepEqual(got, []string{a.Material}) {
		t.Fatalf("listing by item: %v", got)
	}
	e.must(t, owner, "item.relate", map[string]any{"from": i1, "to": a.Material, "type": domain.RelBasis, "remove": true})
	check("item removed", Query{Item: i1})

	// 修订时间：甲先于乙提交（测试时钟每个命令前进一小时）；托管型没有时间，设了时间筛选就不命中。
	var ra, rb materials.Revision
	e.st.View(bg, func(tx *store.Tx) error {
		var err error
		if _, ra, err = materials.Show(tx, a.Material, ""); err != nil {
			return err
		}
		_, rb, err = materials.Show(tx, b.Material, "")
		return err
	})
	check("after", Query{After: rb.At}, b.Material, c.Material)
	check("before", Query{Before: rb.At}, a.Material)
	check("time window", Query{After: ra.At, Before: rb.At}, a.Material)
}

func sameSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := map[string]int{}
	for _, x := range a {
		m[x]++
	}
	for _, x := range b {
		m[x]--
	}
	for _, n := range m {
		if n != 0 {
			return false
		}
	}
	return true
}

func TestQueryWithoutSearchableTextIsInvalid(t *testing.T) {
	e := setup(t)
	_, err := Search(bg, e.st, Query{Text: "!!! ???"}, nil)
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
		t.Fatalf("want invalid_request, got %v", err)
	}
}

func TestSnippetWindow(t *testing.T) {
	long := strings.Repeat("填充", 100) + "目标词" + strings.Repeat("尾巴", 100)
	s := snippet(Doc{Body: long}, [numCols][]string{colBody: {"目标"}})
	if !strings.Contains(s, "目标词") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || len([]rune(s)) > SnippetChars+2 {
		t.Fatalf("window: %q", s)
	}
	if s := snippet(Doc{Summary: "短文本"}, [numCols][]string{}); s != "短文本" {
		t.Fatalf("fallback: %q", s)
	}
}

// W26：范围读法含首尾；截止按本机日界落日；跨度与参数不合法时是请求错误。
func TestFilterItemsDateRange(t *testing.T) {
	at := func(d string, h, m, s int) *time.Time {
		day, _ := time.ParseInLocation("2006-01-02", d, time.Local)
		v := day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(s)*time.Second).UTC() // 存成 UTC，读时换回本机日
		return &v
	}
	its := []domain.Item{
		{ID: "first", PlannedFor: "2026-05-04"},
		{ID: "last", PlannedFor: "2026-05-10"},
		{ID: "before", PlannedFor: "2026-05-03"},
		{ID: "after", PlannedFor: "2026-05-11"},
		{ID: "due-end", Deadline: at("2026-05-10", 23, 59, 59)},
		{ID: "due-next", Deadline: at("2026-05-11", 0, 0, 0)},
		{ID: "due-prev", Deadline: at("2026-05-03", 23, 59, 59)},
		{ID: "plan-out-due-in", PlannedFor: "2026-04-01", Deadline: at("2026-05-04", 0, 0, 0)},
		{ID: "unscheduled"},
	}
	got, err := FilterItems(its, ItemQuery{From: "2026-05-04", To: "2026-05-10"})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, it := range got {
		ids = append(ids, it.ID)
	}
	if want := []string{"first", "last", "due-end", "plan-out-due-in"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("range: got %v want %v", ids, want)
	}
	if _, err := FilterItems(its, ItemQuery{From: "2026-05-01", To: "2026-07-01"}); err != nil { // 62 天整
		t.Fatalf("a 62-day range must be accepted: %v", err)
	}
	for _, q := range []ItemQuery{{From: "2026-05-04"}, {From: "2026-05-10", To: "2026-05-04"}, {From: "2026-05-01", To: "2026-07-02"}, {From: "5/4", To: "2026-05-10"}} {
		var de *domain.Error
		if _, err := FilterItems(its, q); !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
			t.Fatalf("%+v must be an invalid request, got %v", q, err)
		}
	}
}
