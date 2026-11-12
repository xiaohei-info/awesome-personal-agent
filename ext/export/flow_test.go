package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// flow-export-ext 的业务测试：内核是测试边界内的替身，导出目录是真实文件系统。
// 每个测试用 run() 起一个导出会话（启动对账 + 事件增量），断言落盘的文件。

type doc map[string]any

func (k *fakeKernel) setItem(d doc, prog ...doc) {
	b, _ := json.Marshal(map[string]any{"item": d, "relations": []any{}, "progress": prog})
	k.mu.Lock()
	k.objects["/items/"+d["id"].(string)] = string(b)
	k.mu.Unlock()
}

func (k *fakeKernel) setRelations(id string, rels []doc) {
	k.mu.Lock()
	defer k.mu.Unlock()
	var d map[string]any
	json.Unmarshal([]byte(k.objects["/items/"+id]), &d)
	d["relations"] = rels
	b, _ := json.Marshal(d)
	k.objects["/items/"+id] = string(b)
}

func (k *fakeKernel) purge(id string) {
	k.mu.Lock()
	delete(k.objects, "/items/"+id)
	k.mu.Unlock()
}

func (k *fakeKernel) event(typ, obj string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.add(len(k.events)+1, typ, obj)
}

func it(id, title string, extra doc) doc {
	d := doc{"id": id, "kind": "task", "phase": "open", "title": title, "project": "acc-work", "version": 1,
		"updated_at": time.Now().UTC().Format(time.RFC3339)}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

// run 起一个导出会话，等游标追上全部事件、对账完成后停止。
func run(t *testing.T, k *fakeKernel, dir string, o Options) {
	t.Helper()
	if err := o.normalize(); err != nil {
		t.Fatal(err)
	}
	k.mu.Lock()
	last := len(k.events)
	k.mu.Unlock()
	polled := k.count("GET /v1/events") // 对账发生在第一次取事件之前
	runUntilOpts(t, k, dir, o, func() bool {
		b, _ := os.ReadFile(filepath.Join(dir, ".apa-export-cursor"))
		return k.count("GET /v1/events") > polled+1 && strings.TrimSpace(string(b)) == fmt.Sprint(last)
	})
}

func accOpts() Options {
	return Options{
		PublicURL:   "https://apa.acc.test",
		Path:        "{project}/{kind}/{title}.md",
		FrontMatter: fmMap{{"title", "标题"}, {"status", "阶段"}, {"project", "项目"}, {"due", "截止"}, {"plan", "计划日期"}, {"url", "Web 地址"}},
		DateFormat:  "YYYY-MM-DD",
		Body:        []string{"summary", "body", "progress"},
		ActionLinks: true,
	}
}

func files(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, _ error) error {
		if d.IsDir() || strings.HasPrefix(d.Name(), ".apa-export") {
			return nil
		}
		r, _ := filepath.Rel(dir, p)
		out = append(out, filepath.ToSlash(r))
		return nil
	})
	return out
}

func frontKeys(t *testing.T, content string) map[string]any {
	t.Helper()
	m := regexp.MustCompile(`(?s)^---\n(.*?)\n---\n`).FindStringSubmatch(content)
	if m == nil {
		t.Fatalf("no front matter:\n%s", content)
	}
	var out map[string]any
	if err := yaml.Unmarshal([]byte(m[1]), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// ACT-EXP-05：标题清洗、落位到模板路径、重名追加事项 id。
func TestPathTemplateSanitizesTitleAndDisambiguatesDuplicates(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	k.setItem(it("itm_1", "季度/复盘: 草稿?", nil))
	k.setItem(it("itm_2", "同名事项", nil))
	k.setItem(it("itm_3", "同名事项", nil))
	for i, id := range []string{"itm_1", "itm_2", "itm_3"} {
		k.add(i+1, "item.created", id)
	}
	run(t, k, dir, accOpts())

	got := files(t, dir)
	want := map[string]bool{"acc-work/task/季度_复盘_ 草稿_.md": true, "acc-work/task/同名事项.md": true, "acc-work/task/同名事项-itm_3.md": true}
	if len(got) != 3 {
		t.Fatalf("files = %v", got)
	}
	for _, f := range got {
		if !want[f] {
			t.Errorf("unexpected file %q in %v", f, got)
		}
	}
	// 重启后不产生副本，也不改名。
	run(t, k, dir, accOpts())
	if again := files(t, dir); strings.Join(again, "|") != strings.Join(got, "|") {
		t.Errorf("restart changed files: %v -> %v", got, again)
	}
}

// 路径模板不能把文件写出 root。
func TestPathTemplateEscapingRootIsRejected(t *testing.T) {
	o := Options{Path: "../{id}.md"}
	if err := o.normalize(); err == nil {
		t.Fatal("template escaping root must be rejected")
	}
	if p, err := expandPath("{project}/{title}.md", map[string]string{"project": "..", "title": "../../x"}); err != nil || strings.Contains(p, "..") && strings.Contains(p, string(filepath.Separator)+"..") {
		t.Errorf("values must be cleaned, got %q err %v", p, err)
	}
}

// ACT-EXP-06/07/08/14：front matter 只含映射的键；进展带所指日期；生成标记含 id、版本、hash；动作链接不含凭据。
func TestFrontMatterProgressMarkerAndActionLinks(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	dl := "2026-12-24T10:00:00Z"
	k.setItem(it("itm_1", "写周报", doc{"deadline": dl, "planned_for": "2026-12-05", "tags": []string{"acc-tag"}, "version": 7, "summary": "一句话", "body": "正文"}),
		doc{"text": "已联系对方", "on": "2026-11-10", "at": "2026-11-12T01:00:00Z"})
	k.add(1, "item.updated", "itm_1")
	run(t, k, dir, accOpts())

	p := filepath.Join(dir, "acc-work", "task", "写周报.md")
	c := read(t, p)
	fm := frontKeys(t, c)
	if len(fm) != 6 {
		t.Errorf("front matter keys = %v, want exactly the 6 mapped", fm)
	}
	for key, want := range map[string]string{"title": "写周报", "status": "open", "project": "acc-work", "due": "2026-12-24", "plan": "2026-12-05", "url": "https://apa.acc.test/items/itm_1"} {
		if fm[key] != want {
			t.Errorf("front matter %s = %v, want %s", key, fm[key], want)
		}
	}
	if !strings.Contains(c, "- 2026-11-10 已联系对方") || !strings.Contains(c, "计划日期：2026-12-05") {
		t.Errorf("progress section lacks dated entry / plan date:\n%s", c)
	}
	m := markerRE.FindStringSubmatch(c)
	if m == nil || m[1] != "itm_1" || m[2] != "7" {
		t.Fatalf("marker = %v\n%s", m, c)
	}
	if contentHash(markerRE.ReplaceAllString(c, "")) != m[3] {
		t.Error("marker hash does not match the file content")
	}
	for _, do := range []string{"done", "progress", "plan"} {
		if !strings.Contains(c, "https://apa.acc.test/act/itm_1?do="+do) {
			t.Errorf("missing action link %s", do)
		}
	}
	if regexp.MustCompile(`(?i)token|secret|sig=|key=|ext-key`).MatchString(c) {
		t.Errorf("file carries credential-like text:\n%s", c)
	}

	// ACT-EXP-15：没有 public_url 就不写动作链接，也不写 url，其余照常。
	o := accOpts()
	o.PublicURL = ""
	k.setItem(it("itm_1", "写周报", doc{"planned_for": "2026-12-05", "version": 8}))
	k.event("item.updated", "itm_1")
	run(t, k, dir, o)
	c = read(t, p)
	if strings.Contains(c, "/act/") || strings.Contains(c, "url:") || !strings.Contains(c, "title: 写周报") {
		t.Errorf("without public_url:\n%s", c)
	}
}

// ACT-EXP-09/10：手改过的文件另存 .local-edit 再写入新内容并记录；未手改的直接覆盖。
func TestHandEditedFileIsPreservedNotSilentlyOverwritten(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	k.setItem(it("itm_1", "被手改", doc{"body": "旧正文"}))
	k.setItem(it("itm_2", "没手改", doc{"body": "旧正文"}))
	k.add(1, "item.created", "itm_1")
	k.add(2, "item.created", "itm_2")
	run(t, k, dir, accOpts())

	edited, clean := filepath.Join(dir, "acc-work", "task", "被手改.md"), filepath.Join(dir, "acc-work", "task", "没手改.md")
	f, _ := os.OpenFile(edited, os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("本人手写\n")
	f.Close()

	k.setItem(it("itm_1", "被手改", doc{"body": "新正文", "version": 2}))
	k.setItem(it("itm_2", "没手改", doc{"body": "新正文", "version": 2}))
	k.event("item.updated", "itm_1")
	k.event("item.updated", "itm_2")
	run(t, k, dir, accOpts())

	if c := read(t, edited+".local-edit"); !strings.Contains(c, "本人手写") || !strings.Contains(c, "旧正文") {
		t.Errorf(".local-edit lacks the hand edit:\n%s", c)
	}
	if c := read(t, edited); !strings.Contains(c, "新正文") || strings.Contains(c, "本人手写") {
		t.Errorf("edited file must carry the new export:\n%s", c)
	}
	if c := read(t, clean); !strings.Contains(c, "新正文") {
		t.Errorf("clean file not overwritten:\n%s", c)
	}
	if _, err := os.Stat(clean + ".local-edit"); err == nil {
		t.Error("unedited file must not produce a .local-edit copy")
	}
	st, err := loadState(dir)
	if err != nil || len(st.Edits) != 1 || !strings.HasSuffix(st.Edits[0].Path, "被手改.md") {
		t.Errorf("state edits = %+v err %v, want exactly one record", st, err)
	}
	// 再改一次：现在的文件是 ext 自己写的，不再算手改。
	k.setItem(it("itm_1", "被手改", doc{"body": "第三版", "version": 3}))
	k.event("item.updated", "itm_1")
	run(t, k, dir, accOpts())
	if st, _ := loadState(dir); len(st.Edits) != 1 {
		t.Errorf("rewrite of an unedited file recorded an edit: %+v", st.Edits)
	}
}

// ACT-EXP-11：改标题、换项目移动文件；不再满足 select 时归档，不删除。
func TestRenameMoveAndArchiveWhenNoLongerSelected(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	k.setItem(it("itm_1", "旧标题", nil))
	k.setItem(it("itm_2", "换项目", nil))
	k.setItem(it("itm_3", "要终结", nil))
	for i, id := range []string{"itm_1", "itm_2", "itm_3"} {
		k.add(i+1, "item.created", id)
	}
	run(t, k, dir, accOpts())

	old := time.Now().AddDate(0, 0, -60).UTC().Format(time.RFC3339)
	k.setItem(it("itm_1", "新标题", doc{"version": 2}))
	k.setItem(it("itm_2", "换项目", doc{"project": "acc-open", "version": 2}))
	k.setItem(it("itm_3", "要终结", doc{"phase": "done", "version": 2, "updated_at": old}))
	for _, id := range []string{"itm_1", "itm_2", "itm_3"} {
		k.event("item.updated", id)
	}
	run(t, k, dir, accOpts())

	got := strings.Join(files(t, dir), "|")
	for _, want := range []string{"acc-work/task/新标题.md", "acc-open/task/换项目.md", "_archive/acc-work/task/要终结.md"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %s in %s", want, got)
		}
	}
	if strings.Contains(got, "旧标题") || strings.Contains(got, "|acc-work/task/换项目") || strings.Contains(got, "|acc-work/task/要终结") {
		t.Errorf("old paths left behind: %s", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "acc-open")); err != nil {
		t.Error(err)
	}

	// select 收窄后重启：没有事件也要对账归档（阶段限定为未终结）。
	o := accOpts()
	o.Select.Phases = []string{"proposed", "open", "waiting", "needs_input"}
	k.setItem(it("itm_2", "换项目", doc{"project": "acc-open", "phase": "done", "version": 3}))
	k.event("item.updated", "itm_2")
	run(t, k, dir, o)
	if got := strings.Join(files(t, dir), "|"); !strings.Contains(got, "_archive/acc-open/task/换项目.md") || strings.Contains(got, "|acc-open/task/换项目") {
		t.Errorf("not archived after narrowing select: %s", got)
	}
}

// select 在启动对账时生效，不需要事件：配置收窄后，不满足的文件归档而不是删除。
func TestSelectChangeIsReconciledOnRestartWithoutEvents(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	k.setItem(it("itm_1", "a", doc{"project": "p1"}))
	k.setItem(it("itm_2", "b", doc{"project": "p2"}))
	k.add(1, "item.created", "itm_1")
	k.add(2, "item.created", "itm_2")
	run(t, k, dir, accOpts())
	o := accOpts()
	o.Select.Projects = []string{"p1"}
	run(t, k, dir, o)
	got := strings.Join(files(t, dir), "|")
	if !strings.Contains(got, "p1/task/a.md") || !strings.Contains(got, "_archive/p2/task/b.md") || strings.Contains(got, "|p2/") {
		t.Errorf("files = %s", got)
	}
}

// ACT-EXP-12：物理删除后导出文件随之删除，其他文件不变；停机期间的删除靠对账补上。
func TestPhysicalDeleteRemovesOnlyThatFile(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	k.setItem(it("itm_1", "同名事项", nil))
	k.setItem(it("itm_2", "同名事项", nil))
	k.setItem(it("itm_3", "另一条", nil))
	for i, id := range []string{"itm_1", "itm_2", "itm_3"} {
		k.add(i+1, "item.created", id)
	}
	run(t, k, dir, accOpts())
	before := read(t, filepath.Join(dir, "acc-work", "task", "同名事项.md"))

	k.purge("itm_2")
	k.event("purge.executed", "itm_2")
	run(t, k, dir, accOpts())
	if got := files(t, dir); len(got) != 2 {
		t.Errorf("files after purge = %v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "acc-work", "task", "同名事项-itm_2.md")); err == nil {
		t.Error("purged item's file still exists")
	}
	if read(t, filepath.Join(dir, "acc-work", "task", "同名事项.md")) != before {
		t.Error("other file changed")
	}

	// 停机期间删除、没有事件可看：重启对账发现列表里没有它，再确认 404 后删除。
	k.purge("itm_3")
	run(t, k, dir, accOpts())
	if got := files(t, dir); len(got) != 1 {
		t.Errorf("files after reconcile delete = %v", got)
	}
}

// ACT-EXP-13：简报各导出为一个文件，内容取 spec；briefs:false 不导出。
func TestBriefsExportOneFileEachAndCanBeDisabled(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	spec := doc{"period": "2026-11-09 10:50", "sections": []doc{{"project": "acc-work", "body": "两件事到期"}}}
	k.setItem(it("itm_b1", "简报一", doc{"kind": "brief", "phase": "done", "spec": spec}))
	k.setItem(it("itm_b2", "简报二", doc{"kind": "brief", "phase": "done", "spec": spec}))
	k.add(1, "item.updated", "itm_b1")
	k.add(2, "item.updated", "itm_b2")
	run(t, k, dir, accOpts())
	if got := files(t, dir); len(got) != 2 {
		t.Fatalf("files = %v", got)
	}
	if c := read(t, filepath.Join(dir, "acc-work", "brief", "简报一.md")); !strings.Contains(c, "两件事到期") || !strings.Contains(c, "2026-11-09 10:50") {
		t.Errorf("brief content:\n%s", c)
	}
	off := false
	o := accOpts()
	o.Briefs = &off
	run(t, k, dir, o)
	if got := files(t, dir); len(got) != 0 {
		t.Errorf("briefs:false left files %v", got)
	}
}

// ACT-EXP-18：初筛未返回的事项（内核只给 id/阶段/版本）不产生文件；挂靠消息从不读取；托管型材料只导出位置。
func TestWebOnlyScreeningAndMessagesAreNeverExported(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	k.setItem(doc{"id": "itm_w", "phase": "needs_input", "version": 2, "kind": "task", "ask": doc{"screening_pending": true}})
	k.setItem(it("itm_1", "正常", doc{"body": "正文"}))
	k.objects["/items/itm_1/messages"] = `{"messages":[{"detail":"ACC-SECRET-BODY"}]}`
	k.objects["/materials/mat_h"] = `{"material":{"id":"mat_h","kind":"doc","title":"托管","location":"repo://x/doc.md"},"revision":{"content":"ACC-SECRET-BODY"}}`
	k.add(1, "item.created", "itm_w")
	k.add(2, "item.created", "itm_1")
	k.add(3, "material.revised", "mat_h")
	run(t, k, dir, accOpts())

	for _, f := range files(t, dir) {
		if strings.Contains(f, "itm_w") {
			t.Errorf("screening-pending item exported: %s", f)
		}
		if c := read(t, filepath.Join(dir, f)); strings.Contains(c, "ACC-SECRET-BODY") || strings.Contains(c, "screening_pending") {
			t.Errorf("%s leaks blocked content:\n%s", f, c)
		}
	}
	if k.count("/messages") != 0 {
		t.Error("exporter read the messages endpoint")
	}
	if len(files(t, dir)) != 2 {
		t.Errorf("files = %v, want the item and the hosted material", files(t, dir))
	}
}

// 子事项与关联材料以相对链接写进正文。
func TestSubitemsAndMaterialsSections(t *testing.T) {
	k, dir := newKernel(), t.TempDir()
	k.setItem(it("itm_p", "父", nil))
	k.setItem(it("itm_c", "子", doc{"project": "acc-work"}))
	k.setRelations("itm_p", []doc{{"type": "parent", "from": "itm_c", "to": "itm_p"}, {"type": "output", "from": "itm_p", "to": "mat_x"}})
	k.add(1, "item.created", "itm_c")
	k.add(2, "item.created", "itm_p")
	o := accOpts()
	o.Body = []string{"subitems", "materials"}
	run(t, k, dir, o)
	c := read(t, filepath.Join(dir, "acc-work", "task", "父.md"))
	if !strings.Contains(c, "[itm_c](<子.md>)") || !strings.Contains(c, "[mat_x](<../../materials/mat_x.md>)") {
		t.Errorf("sections:\n%s", c)
	}
}

func TestOptionsValidationAndFileLoading(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "export.yaml")
	os.WriteFile(cfg, []byte("path: \"{project}/{title}.md\"\nfront_matter:\n  title: 标题\n  plan: 计划日期\ndate_format: YYYY/MM/DD\nbody: [body, progress]\naction_links: true\npublic_url: https://apa.acc.test/\n"), 0o600)
	env := map[string]string{"APA_URL": "http://k", "APA_EXT_KEY": "s", "APA_EXPORT_DIR": "/o", "APA_EXPORT_CONFIG": cfg}
	c, err := ConfigFromEnv(func(n string) string { return env[n] })
	if err != nil || c.Opts.PublicURL != "https://apa.acc.test" || c.Opts.FrontMatter[1].Field != "planned_for" || c.Opts.fmtDay("2026-12-05") != "2026/12/05" {
		t.Fatalf("%+v %v", c.Opts, err)
	}
	for name, y := range map[string]string{
		"unknown field":   "front_matter:\n  x: 不存在\n",
		"unknown section": "body: [nope]\n",
		"unknown key":     "bogus: 1\n",
	} {
		os.WriteFile(cfg, []byte(y), 0o600)
		if _, err := ConfigFromEnv(func(n string) string { return env[n] }); err == nil {
			t.Errorf("%s must be a configuration error", name)
		}
	}
	env["APA_EXPORT_CONFIG"] = filepath.Join(t.TempDir(), "missing.yaml")
	if _, err := ConfigFromEnv(func(n string) string { return env[n] }); err == nil {
		t.Error("missing config file must be an error")
	}
}
