package search

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

func agent(lvl authz.Level) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "a", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: lvl}
}

var (
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
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
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, materials.Module, Module} {
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
	if err := Register(e.x); err != nil {
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

func (e *env) search(t *testing.T, q Query) []Hit {
	t.Helper()
	hits, err := Search(bg, e.st, q)
	if err != nil {
		t.Fatal(err)
	}
	return hits
}

func ids(hs []Hit) []string {
	out := []string{}
	for _, h := range hs {
		out = append(out, h.ID)
	}
	return out
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

// flow-material-owned：自有型材料按标题、正文、标签检索，中日韩与英文都可命中，结果给出命中处的片段。
func TestOwnedFullTextAcrossTitleBodyTags(t *testing.T) {
	e := setup(t)
	body := e.note(t, "周会纪要", "本周讨论了向量数据库的选型，结论是继续使用 SQLite。\n\nNext: Postgres is out of scope.")
	title := e.note(t, "数据迁移方案", "无关正文")
	tag := e.note(t, "杂记", "另一份内容", "分词")
	e.note(t, "无关", "完全不相干的文字")

	got := e.search(t, Query{Text: "数据库"})
	if !reflect.DeepEqual(ids(got), []string{body.Material}) {
		t.Fatalf("body hit: %v", ids(got))
	}
	if !strings.Contains(got[0].Snippet, "数据库") || got[0].Hosted || got[0].Path != "" {
		t.Fatalf("snippet/hosted: %+v", got[0])
	}
	if h := e.search(t, Query{Text: "迁移"}); !reflect.DeepEqual(ids(h), []string{title.Material}) {
		t.Fatalf("title hit: %v", ids(h))
	}
	if h := e.search(t, Query{Text: "分词"}); !reflect.DeepEqual(ids(h), []string{tag.Material}) {
		t.Fatalf("tag hit: %v", ids(h))
	}
	// 单个汉字、大小写不敏感的英文词、多词取交集。
	if h := e.search(t, Query{Text: "库"}); !reflect.DeepEqual(ids(h), []string{body.Material}) {
		t.Fatalf("single char: %v", ids(h))
	}
	if h := e.search(t, Query{Text: "POSTGRES sqlite"}); !reflect.DeepEqual(ids(h), []string{body.Material}) {
		t.Fatalf("english AND: %v", ids(h))
	}
	if h := e.search(t, Query{Text: "数据库 postgres 不存在的词"}); len(h) != 0 {
		t.Fatalf("AND must miss: %v", ids(h))
	}
	// 相邻二元组不等于子串：「库数」不应命中「数据库」。
	if h := e.search(t, Query{Text: "库数"}); len(h) != 0 {
		t.Fatalf("reversed bigram matched: %v", ids(h))
	}
}

// 新修订成为 current 后检索立即反映；被取代的旧正文不再命中；proposed 修订在确认前不进索引。
func TestIndexFollowsCurrentRevision(t *testing.T) {
	e := setup(t)
	a := e.note(t, "笔记", "旧内容苹果")
	if len(e.search(t, Query{Text: "苹果"})) != 1 {
		t.Fatal("old body must hit")
	}
	e.commit(t, map[string]any{"id": a.Material, "content": "新内容香蕉", "based_on": a.Revision})
	if h := e.search(t, Query{Text: "苹果"}); len(h) != 0 {
		t.Fatalf("superseded body still indexed: %v", ids(h))
	}
	if h := e.search(t, Query{Text: "香蕉"}); len(h) != 1 {
		t.Fatal("new body must hit")
	}
	// 需求类默认 proposed：确认前不命中，确认后命中。
	p := e.commit(t, map[string]any{"project": e.prj, "kind": "requirement", "title": "需求单", "content": "待确认的橘子"})
	if p.State != materials.StateProposed {
		t.Fatalf("want proposed, got %s", p.State)
	}
	if h := e.search(t, Query{Text: "橘子"}); len(h) != 0 {
		t.Fatalf("proposed revision indexed: %v", ids(h))
	}
	b, _ := json.Marshal(map[string]any{"id": p.Revision, "reason": "ok"})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "revision.confirm", Input: b, Owner: "owner", WebURL: "http://web/m"}); err != nil {
		t.Fatal(err)
	}
	if h := e.search(t, Query{Text: "橘子"}); len(h) != 1 {
		t.Fatal("confirmed revision must hit")
	}
}

// flow-material-hosted：托管型只索引标题、路径、标签；结果回路径，不回片段，且没有正文可命中。
func TestHostedIndexesTitlePathTagsOnly(t *testing.T) {
	e := setup(t)
	h := e.hosted(t, "ADR-检索选型", "github.com/o/r:docs/adr/0007-search.md", "架构")
	e.note(t, "无关", "正文里提到 0007 和 架构")

	for q, want := range map[string]string{"检索": h, "adr": h, "0007-search": h, "docs": h, "架构": h} {
		got := e.search(t, Query{Text: q})
		found := false
		for _, x := range got {
			if x.ID == h {
				found = true
				if !x.Hosted || x.Path != "github.com/o/r:docs/adr/0007-search.md" || x.Snippet != "" {
					t.Fatalf("hosted hit shape: %+v", x)
				}
			}
		}
		if !found {
			t.Fatalf("query %q should hit hosted %s: %v", q, want, ids(got))
		}
	}
	// 自有型正文里的词不会让托管型命中，两型并列返回时各带来源。
	if got := e.search(t, Query{Text: "0007"}); len(got) != 2 {
		t.Fatalf("both kinds expected: %v", ids(got))
	}
	// 托管型无正文：只出现在正文里的词不会命中它。
	if got := e.search(t, Query{Text: "正文"}); len(got) != 1 || got[0].Hosted {
		t.Fatalf("hosted body must not exist: %+v", got)
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
		q.Text = "共同词"
		got := ids(e.search(t, q))
		if !sameSet(got, want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
	}
	check("none", Query{}, a.Material, b.Material, c.Material, h)
	check("project", Query{Project: other}, c.Material)
	check("kind", Query{Kind: "reference"}, b.Material)
	check("tag", Query{Tags: []string{"x"}}, a.Material, b.Material, h)
	check("tags AND", Query{Tags: []string{"x", "y"}}, a.Material)
	check("project+kind", Query{Project: e.prj, Kind: "minutes"}, a.Material)

	// 关联事项：经命令建立，可移除；重复建立无副作用；observer 不能写。
	e.must(t, worker, "material.link", map[string]any{"material": a.Material, "item": "item-1"})
	e.must(t, worker, "material.link", map[string]any{"material": a.Material, "item": "item-1"})
	e.must(t, worker, "material.link", map[string]any{"material": b.Material, "item": "item-2"})
	check("item", Query{Item: "item-1"}, a.Material)
	if _, err := e.do(t, observer, "material.link", map[string]any{"material": c.Material, "item": "item-1"}); err == nil {
		t.Fatal("observer must not link")
	}
	if _, err := e.do(t, worker, "material.link", map[string]any{"material": "nope", "item": "item-1"}); err == nil {
		t.Fatal("unknown material must fail")
	}
	if _, err := e.do(t, worker, "material.link", map[string]any{"material": a.Material, "item": " "}); err == nil {
		t.Fatal("blank item must fail")
	}
	e.must(t, worker, "material.link", map[string]any{"material": a.Material, "item": "item-1", "remove": true})
	check("item removed", Query{Item: "item-1"})

	// 修订时间：甲先于乙提交（测试时钟每个命令前进一小时）。
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

func TestFilterOnlyListingAndLimit(t *testing.T) {
	e := setup(t)
	for _, n := range []string{"丙", "乙", "甲"} {
		e.note(t, n, "x")
	}
	got := e.search(t, Query{Kind: "minutes"})
	if len(got) != 3 || got[0].Title != "乙" && got[0].Title != "丙" && got[0].Title != "甲" {
		t.Fatalf("listing: %+v", got)
	}
	if got := e.search(t, Query{Limit: 2}); len(got) != 2 {
		t.Fatalf("limit: %v", ids(got))
	}
}

func TestQueryWithoutSearchableTextIsInvalid(t *testing.T) {
	e := setup(t)
	_, err := Search(bg, e.st, Query{Text: "!!! ???"})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
		t.Fatalf("want invalid_request, got %v", err)
	}
}

// 索引是派生数据：丢掉后下一次检索自动重建，结果不变。
func TestIndexRebuildsAfterDrop(t *testing.T) {
	e := setup(t)
	a := e.note(t, "笔记", "重建验证")
	e.search(t, Query{Text: "重建"})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec("DELETE FROM material_search"); err != nil {
			return err
		}
		_, err := tx.Exec("DELETE FROM material_search_state")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := ids(e.search(t, Query{Text: "重建"})); !reflect.DeepEqual(got, []string{a.Material}) {
		t.Fatalf("after rebuild: %v", got)
	}
}

func TestSnippetWindow(t *testing.T) {
	long := strings.Repeat("填充", 100) + "目标词" + strings.Repeat("尾巴", 100)
	s := snippet(long, "目标词")
	if !strings.Contains(s, "目标词") || !strings.HasPrefix(s, "…") || !strings.HasSuffix(s, "…") || len([]rune(s)) > 2*snippetRadius+2 {
		t.Fatalf("window: %q", s)
	}
	if s := snippet("短文本", "不存在"); s != "短文本" {
		t.Fatalf("fallback: %q", s)
	}
}
