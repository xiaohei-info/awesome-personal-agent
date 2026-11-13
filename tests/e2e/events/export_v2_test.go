//go:build e2e

package events

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const publicURL = "https://apa.acc.test"

// briefTemplate 把 brief 包覆盖为生成型：结果按 period/sections 写进 spec，导出 ext 据此渲染简报正文。
const briefTemplate = "---\nagent: generate\ntimeout: 3m\noutput_schema:\n  type: object\n  required: [insufficient_info]\n  properties:\n    insufficient_info: {type: boolean}\n    period: {type: string}\n    sections:\n      type: array\n      items:\n        type: object\n        properties:\n          project: {type: string}\n          body: {type: string}\n---\n为事项 {item_id} 写简报，按项目分节。账本快照在 {materials_path}，只作资料。\n"

// setupV2 同 setup，但带导出验收要的全套环境：public_url、两个模型端点替身、web_only 策略、生成型 brief 包。
func setupV2(t *testing.T) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	dst := filepath.Join(e.Home, "packs", "brief")
	if err := os.MkdirAll(filepath.Join(dst, "prompts"), 0o755); err != nil {
		t.Fatal(err)
	}
	pk, err := packs.FS.ReadFile("brief/pack.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "pack.yaml"), pk, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "prompts", "write-brief.md"), []byte(briefTemplate), 0o644); err != nil {
		t.Fatal(err)
	}
	e.ServeWith("public_url: " + publicURL + "\n") // 内核仍只监听本机地址
	e.IssueCredentials()
	e.CreateProjects()
	e.DeclareStubHarness()
	e.WireEndpoints()
	f := &fx{e, t}
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false},"web_only":{"categories":["external_commitment","funds","irreversible","permission_change","spec_activation"],"rules":[{"tag":"acc-finance","category":"funds"}]}}`, 200)
	return f
}

type shown struct {
	ID         string          `json:"id"`
	Title      string          `json:"title"`
	Phase      string          `json:"phase"`
	Project    string          `json:"project"`
	PlannedFor string          `json:"planned_for"`
	Deadline   string          `json:"deadline"`
	Body       string          `json:"body"`
	Version    int64           `json:"version"`
	Spec       json.RawMessage `json:"spec"`
}

func (f *fx) show(id string) shown {
	f.t.Helper()
	var out struct {
		Item shown `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out); err != nil || out.Item.ID == "" {
		f.t.Fatalf("apa item show %s: %v", id, err)
	}
	return out.Item
}

var markerRE = regexp.MustCompile(`(?m)^<!-- apa-export id=(\S+) version=(\S*) hash=([0-9a-f]{64}) -->\n`)

// fileOf 按生成标记里的对象 id 找导出文件（路径由模板决定，id 不一定在文件名里）。
func fileOf(dir, id string) (rel, content string) {
	for name, c := range readDirAll(dir) {
		if m := markerRE.FindStringSubmatch(c); m != nil && m[1] == id && !strings.HasSuffix(name, ".local-edit") {
			return name, c
		}
	}
	return "", ""
}

// readDirAll 同 readDir，但保留 .local-edit 副本；隐藏的游标与状态文件不算导出文件。
func readDirAll(dir string) map[string]string {
	out := map[string]string{}
	_ = filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && !strings.HasPrefix(d.Name(), ".") {
			b, _ := os.ReadFile(p)
			rel, _ := filepath.Rel(dir, p)
			out[rel] = string(b)
		}
		return nil
	})
	return out
}

// frontMatter 解析文件头部（第一段 --- … ---）。
func frontMatter(t *testing.T, doc string) map[string]any {
	t.Helper()
	rest, ok := strings.CutPrefix(doc, "---\n")
	if !ok {
		t.Fatalf("文件没有 front matter:\n%s", doc)
	}
	head, _, ok := strings.Cut(rest, "\n---\n")
	if !ok {
		t.Fatalf("front matter 没有结束:\n%s", doc)
	}
	m := map[string]any{}
	if err := yaml.Unmarshal([]byte(head), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *fx) exportCfg(path, root string, o map[string]any) {
	f.t.Helper()
	b, err := yaml.Marshal(o)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// webSession 是一个独立的浏览器会话（Cookie + CSRF），代表本人在 Web 上登录后的操作。
type webSession struct {
	f    *fx
	c    *http.Client
	csrf string
}

func (f *fx) newWebSession() *webSession {
	jar, _ := cookiejar.New(nil)
	return &webSession{f: f, c: &http.Client{Jar: jar, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
}

func (w *webSession) do(method, path, body string, hdr map[string]string) (int, string, http.Header) {
	w.f.t.Helper()
	req, err := http.NewRequest(method, w.f.URL+path, strings.NewReader(body))
	if err != nil {
		w.f.t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if w.csrf != "" {
		req.Header.Set("X-CSRF-Token", w.csrf)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := w.c.Do(req)
	if err != nil {
		w.f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func (w *webSession) login() {
	w.f.t.Helper()
	code, body, _ := w.do("POST", "/v1/auth/login", `{"password":"`+baseline.Password+`"}`, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if code != 200 || json.Unmarshal([]byte(body), &out) != nil || out.CSRF == "" {
		w.f.t.Fatalf("Web 登录失败: %d %s", code, body)
	}
	w.csrf = out.CSRF
}

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

// TestFlowExportExtMapping 按验收文档 flow-export-ext 的顺序执行 ACT-EXP-00、05..19：导出 ext 是 `apa ext run`
// 拉起的真实独立进程，只持只读 ext 密钥；账本变化全部经真实 apa 命令与 Web 会话产生。
// 手机浏览器渲染（ego-browser、360 像素视口）不在本机：ACT-EXP-16/17 用 Web 页面调用的同一套 HTTP 接口驱动，
// 页面本身由 web/src/features/action-confirm 的组件测试覆盖，浏览器渲染由本机升级验证 L1–L12 承接。
func TestFlowExportExtMapping(t *testing.T) {
	f := setupV2(t)
	top := t
	extDir := filepath.Join(repoRoot(), "ext")
	root := t.TempDir()
	cfgPath := filepath.Join(t.TempDir(), "export.yaml")
	exportKey := f.extKey("EXPK", "export")
	env := []string{"APA_EXT_DIR=" + extDir, "APA_EXT_KEY=" + exportKey, "APA_EXPORT_CONFIG=" + cfgPath}
	work, open := f.Projects["acc-work"], f.Projects["acc-open"]

	cfg := func(withPublicURL bool, phases []string) {
		o := map[string]any{
			"root":         root,
			"path":         "{project}/{kind}/{title}.md",
			"front_matter": map[string]string{"title": "标题", "status": "阶段", "project": "项目", "due": "截止", "plan": "计划日期", "url": "Web 地址"},
			"date_format":  "YYYY-MM-DD",
			"body":         []string{"summary", "body", "progress"},
			"action_links": true,
			"briefs":       true,
		}
		if withPublicURL {
			o["public_url"] = publicURL
		}
		if phases != nil {
			o["select"] = map[string]any{"phases": phases}
		}
		// yaml.Marshal 会重排 map 键，front_matter 的顺序不影响断言（只比键集合）。
		f.exportCfg(cfgPath, root, o)
	}
	var export *proc
	restart := func() {
		if export != nil {
			export.stop()
		}
		cur := f.t
		f.t = top // 进程的清理挂在整个流程上，不随子测试结束而被收掉
		export = f.start(env, "ext", "run", "export")
		f.t = cur
	}
	t.Cleanup(func() {
		if t.Failed() && export != nil {
			t.Logf("导出 ext 输出:\n%s", export.out.String())
		}
	})
	waitIn := func(t *testing.T, id, want string) string {
		t.Helper()
		var doc string
		eventually(t, 60*time.Second, id+" 的导出文件含 "+want, func() bool {
			_, doc = fileOf(root, id)
			return strings.Contains(doc, want)
		})
		return doc
	}
	gone := func(t *testing.T, rel string, within time.Duration) {
		t.Helper()
		eventually(t, within, rel+" 被移走", func() bool { _, err := os.Stat(filepath.Join(root, rel)); return err != nil })
	}
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		parent := f.t
		ok := t.Run(name, func(t *testing.T) {
			f.t, f.Env.T = t, t
			defer func() { f.t, f.Env.T = parent, parent }()
			fn(t)
		})
		if !ok {
			t.Fatalf("%s 未通过，后续步骤不得开始", name)
		}
	}
	webCreate := func(body map[string]any) string {
		t.Helper()
		b, _ := json.Marshal(body)
		var out struct {
			ID   string              `json:"id"`
			Item struct{ ID string } `json:"item"`
		}
		if err := json.Unmarshal([]byte(f.webOK("POST", "/items", string(b), 201)), &out); err != nil {
			t.Fatal(err)
		}
		return out.ID + out.Item.ID
	}

	step("ACT-EXP-00 准备导出目录与导出 ext 配置", func(t *testing.T) {
		cfg(true, nil)
		list := f.start([]string{"APA_EXT_DIR=" + extDir}, "ext", "list")
		<-list.done
		if !strings.Contains(list.out.String(), `"export"`) {
			t.Fatalf("apa ext list 没有列出导出 ext: %s", list.out.String())
		}
		restart()
		time.Sleep(1500 * time.Millisecond)
		select {
		case <-export.done:
			t.Fatalf("导出 ext 启动后退出了: %s", export.out.String())
		default:
		}
		if _, err := os.Stat(root); err != nil {
			t.Fatal(err)
		}
	})

	var titled, dupA, dupB string
	titledName := "季度/复盘: 草稿?"
	step("ACT-EXP-05 路径模板落位，标题清洗，重名追加事项 id", func(t *testing.T) {
		titled = f.task(titledName)
		waitIn(t, titled, "title:")
		dupA = f.task("同名事项")
		waitIn(t, dupA, "title: 同名事项")
		dupB = f.task("同名事项")
		waitIn(t, dupB, "title: 同名事项")
		relT, _ := fileOf(root, titled)
		// 验收文档里的 acc-work 指这个项目；事项的 project 字段（apa item show 也如此）取项目 id，{project} 展开为同一个值。
		if filepath.Dir(relT) != filepath.Join(work, "task") {
			t.Errorf("第一条应落在 acc-work 项目目录的 task/ 下（%s/task）: %s", work, relT)
		}
		if strings.ContainsAny(filepath.Base(relT), `/:?*"<>|\`) {
			t.Errorf("文件名含文件系统非法字符: %s", relT)
		}
		relA, _ := fileOf(root, dupA)
		relB, _ := fileOf(root, dupB)
		if relA == relB || relA == "" || relB == "" {
			t.Fatalf("同名事项互相覆盖: %q %q", relA, relB)
		}
		if strings.Contains(filepath.Base(relA), dupA) || !strings.Contains(filepath.Base(relB), dupB) {
			t.Errorf("第二份文件名应追加它的事项 id: %s / %s", relA, relB)
		}
		for name := range readDirAll(root) {
			if strings.Contains(name, "..") {
				t.Errorf("写出到了 root 之外: %s", name)
			}
		}
	})

	var plan string
	step("ACT-EXP-06 front matter 按映射写出，未映射字段不写", func(t *testing.T) {
		plan = webCreate(map[string]any{"kind": "task", "title": "exp-06 计划事项", "phase": "open", "project": work, "assignee": "work-host.cli",
			"deadline": "2026-12-31T00:00:00Z", "planned_for": "2026-12-01", "tags": []string{"acc-tag"}})
		doc := waitIn(t, plan, "title: exp-06")
		fm := frontMatter(t, doc)
		keys := []string{}
		for k := range fm {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		if want := []string{"due", "plan", "project", "status", "title", "url"}; !slices.Equal(keys, want) {
			t.Fatalf("front matter 的键 %v，期望恰好 %v\n%s", keys, want, doc)
		}
		s := f.show(plan)
		eq := func(k string, want any) {
			if got := fmt.Sprint(fm[k]); got != fmt.Sprint(want) {
				t.Errorf("front matter %s=%q，apa item show 为 %q", k, got, want)
			}
		}
		eq("title", s.Title)
		eq("status", s.Phase)
		eq("project", s.Project)
		eq("plan", "2026-12-01")
		eq("due", "2026-12-31")
		eq("url", publicURL+"/items/"+plan)
		if s.PlannedFor != "2026-12-01" || !strings.HasPrefix(s.Deadline, "2026-12-31") {
			t.Errorf("账本里的计划日期/截止与创建时不符: %+v", s)
		}
		if strings.Contains(doc, "acc-tag") {
			t.Errorf("未映射的标签被写出:\n%s", doc)
		}
	})

	step("ACT-EXP-07 正文带进展（含所指日期）与计划日期变化", func(t *testing.T) {
		f.apa(f.WRK, 0, "item", "progress", plan, "--text", "已联系对方", "--on", "2026-11-10")
		f.apa(f.DEL, 0, "item", "update", plan, "--planned-for", "2026-12-05", "--reason", "改计划", "--evidence", "验收")
		doc := waitIn(t, plan, "plan: \"2026-12-05\"")
		if !strings.Contains(doc, "已联系对方") || !strings.Contains(doc, "2026-11-10") {
			t.Errorf("进展一节缺内容或所指日期:\n%s", doc)
		}
		if !strings.Contains(doc, "## 进展") {
			t.Errorf("缺少进展一节:\n%s", doc)
		}
	})

	step("ACT-EXP-08 每个导出文件带生成标记", func(t *testing.T) {
		_, doc := fileOf(root, plan)
		m := markerRE.FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("没有生成标记:\n%s", doc)
		}
		if m[1] != plan || m[2] != fmt.Sprint(f.show(plan).Version) {
			t.Errorf("标记的事项 id/版本 %q/%q，期望 %s/%d", m[1], m[2], plan, f.show(plan).Version)
		}
		if got := sha(markerRE.ReplaceAllString(doc, "")); got != m[3] {
			t.Errorf("标记 hash %s 与去掉标记行后的内容 hash %s 不符", m[3], got)
		}
		for name, c := range readDirAll(root) {
			if !markerRE.MatchString(c) {
				t.Errorf("%s 没有生成标记", name)
			}
		}
	})

	step("ACT-EXP-09 手改过的导出文件不被静默覆盖", func(t *testing.T) {
		rel, doc := fileOf(root, plan)
		if err := os.WriteFile(filepath.Join(root, rel), []byte(doc+"本人手写\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		f.apa(f.DEL, 0, "item", "update", plan, "--body", "手改之后的新正文", "--reason", "改正文", "--evidence", "验收")
		waitIn(t, plan, "手改之后的新正文")
		copyRel := rel + ".local-edit"
		b, err := os.ReadFile(filepath.Join(root, copyRel))
		if err != nil || !strings.Contains(string(b), "本人手写") {
			t.Fatalf("没有含手写内容的 .local-edit 副本: %v %s", err, b)
		}
		if _, now := fileOf(root, plan); strings.Contains(now, "本人手写") {
			t.Errorf("原文件应被写成新内容")
		}
		st, _ := os.ReadFile(filepath.Join(root, ".apa-export-state.json"))
		var s struct {
			Edits []struct{ Path, Copy string } `json:"edits"`
		}
		_ = json.Unmarshal(st, &s)
		if len(s.Edits) != 1 || s.Edits[0].Path != rel {
			t.Errorf("导出状态里应有一条手改记录: %s", st)
		}
		if body := f.show(plan).Body; strings.Contains(body, "本人手写") || !strings.Contains(body, "手改之后的新正文") {
			t.Errorf("手改内容不应写回账本: %q", body)
		}
	})

	step("ACT-EXP-10 未被手改的文件正常覆盖，不产生副本", func(t *testing.T) {
		rel, _ := fileOf(root, titled)
		f.apa(f.DEL, 0, "item", "update", titled, "--body", "未手改的新正文", "--reason", "改正文", "--evidence", "验收")
		waitIn(t, titled, "未手改的新正文")
		if _, err := os.Stat(filepath.Join(root, rel+".local-edit")); err == nil {
			t.Errorf("未手改的文件不该有 .local-edit 副本")
		}
		st, _ := os.ReadFile(filepath.Join(root, ".apa-export-state.json"))
		if strings.Contains(string(st), titled+".md.local-edit") || strings.Count(string(st), `"copy"`) != 1 {
			t.Errorf("导出状态里只该有 ACT-EXP-09 的那一条手改记录: %s", st)
		}
	})

	var moved, archived string
	step("ACT-EXP-11 改标题/换项目移动文件，不再满足 select 时归档", func(t *testing.T) {
		oldRel, _ := fileOf(root, titled)
		f.retitle(titled, "新标题")
		eventually(t, 60*time.Second, "改标题后的新文件名", func() bool {
			r, _ := fileOf(root, titled)
			return strings.HasSuffix(r, "新标题.md")
		})
		gone(t, oldRel, 5*time.Second)

		moved = f.task("exp-11 换项目")
		waitIn(t, moved, "title: exp-11")
		f.apa(f.DEL, 0, "item", "update", moved, "--project", open, "--reason", "换项目", "--evidence", "验收")
		eventually(t, 60*time.Second, "换项目后的文件落在 acc-open 下", func() bool {
			r, _ := fileOf(root, moved)
			return strings.HasPrefix(r, open+string(filepath.Separator))
		})

		archived = f.task("exp-11 归档")
		waitIn(t, archived, "title: exp-11 归档")
		before, _ := fileOf(root, archived)
		f.transition(archived, "done")
		cfg(true, []string{"proposed", "open", "waiting", "needs_input"})
		restart()
		eventually(t, 60*time.Second, "不再满足 select 的文件移到 _archive 下", func() bool {
			r, _ := fileOf(root, archived)
			return strings.HasPrefix(r, "_archive"+string(filepath.Separator))
		})
		gone(t, before, 5*time.Second)
		if _, doc := fileOf(root, archived); doc == "" {
			t.Errorf("归档的文件不该被删除")
		}
	})

	step("ACT-EXP-12 物理删除后导出文件随之删除，其他文件不变", func(t *testing.T) {
		relB, _ := fileOf(root, dupB)
		snap := readDirAll(root)
		delete(snap, relB)
		f.webOK("POST", "/settings/purge", `{"object":"`+dupB+`"}`, 200) // 物理删除没有事件，靠 ext 的定时对账发现
		defer func() {
			if t.Failed() {
				r := f.APIRaw("GET", "/v1/items/"+dupB, "", exportKey)
				l := f.APIRaw("GET", "/v1/items", "", exportKey)
				t.Logf("清除后 GET /items/%s: %d %.200s；列表含该 id: %v", dupB, r.Code, r.Body, strings.Contains(l.Body, dupB))
			}
		}()
		gone(t, relB, 150*time.Second)
		after := readDirAll(root)
		for name, want := range snap {
			if after[name] != want {
				t.Errorf("其他文件 %s 被改动或误删", name)
			}
		}
		if r, _ := fileOf(root, dupA); r == "" {
			t.Errorf("同名的另一份文件被误删")
		}
	})

	step("ACT-EXP-13 简报各导出为一个文件", func(t *testing.T) {
		// 简报生成后处于 done，被 ACT-EXP-11 收窄的 select（只导出未终结）筛掉；这里把 select 恢复为缺省再导出简报。
		cfg(true, nil)
		restart()
		const briefJSON = `{"insufficient_info":false,"period":"2026-W46","sections":[{"project":"acc-work","body":"本周要点 BRIEF-BODY"}]}`
		f.ModelStub.SetDefault(modelstub.Reply{Content: briefJSON})
		inst := f.apa(f.DEL, 0, "--evidence", "验证生成型模板", "instance", "add", "brief", "--param", "schedule=2s").Stdout
		var done []string
		eventually(t, 60*time.Second, "至少两份已完成的简报", func() bool {
			var out struct{ Items []shown }
			_ = json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list", "--kind", "brief").Stdout), &out)
			done = done[:0]
			for _, b := range out.Items {
				if b.Phase == "done" {
					done = append(done, b.ID)
				}
			}
			return len(done) >= 2
		})
		var id struct {
			ID string `json:"id"`
		}
		if json.Unmarshal([]byte(inst), &id) == nil && id.ID != "" {
			f.apa(f.DEL, 0, "--evidence", "简报够了", "instance", "disable", id.ID)
		}
		f.ModelStub.SetDefault(modelstub.Reply{Status: 400})
		files := map[string]bool{}
		for _, b := range done[:2] {
			defer func() {
				if t.Failed() {
					rel, doc := fileOf(root, b)
					t.Logf("简报 %s 文件 %s:\n%s\n账本 spec: %s", b, rel, doc, f.show(b).Spec)
				}
			}()
			doc := waitIn(t, b, "BRIEF-BODY")
			rel, _ := fileOf(root, b)
			files[rel] = true
			web := f.webOK("GET", "/items/"+b, "", 200)
			if !strings.Contains(web, "本周要点 BRIEF-BODY") || !strings.Contains(doc, "周期：2026-W46") || !strings.Contains(doc, "## acc-work") {
				t.Errorf("简报文件与 Web 上的内容不一致:\n文件: %s\nWeb: %.300s", doc, web)
			}
		}
		if len(files) != 2 {
			t.Errorf("两份简报应各有一个文件，实际 %v", files)
		}
	})

	step("ACT-EXP-14 动作链接只含事项地址与动作参数，不含凭据", func(t *testing.T) {
		_, doc := fileOf(root, plan)
		links := regexp.MustCompile(`\]\((https://apa\.acc\.test/act/[^)\s]*)\)`).FindAllStringSubmatch(doc, -1)
		got := map[string]bool{}
		for _, m := range links {
			got[m[1]] = true
		}
		for _, do := range []string{"done", "progress", "plan"} {
			if !got[publicURL+"/act/"+plan+"?do="+do] {
				t.Errorf("缺动作链接 do=%s: %v", do, got)
			}
		}
		if len(got) != 3 {
			t.Errorf("动作链接应恰好三条且无多余参数: %v", got)
		}
		secrets := regexp.MustCompile(`token|APA_TOKEN|` + regexp.QuoteMeta(f.WRK) + `|` + regexp.QuoteMeta(f.DEL) + `|` + regexp.QuoteMeta(exportKey))
		for name, c := range readDirAll(root) {
			if m := secrets.FindString(c); m != "" {
				t.Errorf("%s 含凭据类内容 %q", name, m)
			}
		}
	})

	step("ACT-EXP-15 未配置 public_url 时不写动作链接", func(t *testing.T) {
		cfgFile := filepath.Join(f.Home, "config.yaml")
		orig, err := os.ReadFile(cfgFile)
		if err != nil {
			t.Fatal(err)
		}
		bare := regexp.MustCompile(`(?m)^public_url:.*\n`).ReplaceAllString(string(orig), "")
		if bare == string(orig) {
			t.Fatalf("config.yaml 里没有 public_url: %s", orig)
		}
		restartKernel := func(conf string) {
			f.StopKernel()
			if err := os.WriteFile(cfgFile, []byte(conf), 0o644); err != nil {
				t.Fatal(err)
			}
			f.StartKernelAgain()
		}
		restartKernel(bare)
		cfg(false, nil)
		restart()
		f.apa(f.DEL, 0, "item", "update", plan, "--body", "无链接时的正文", "--reason", "改正文", "--evidence", "验收")
		eventually(t, 60*time.Second, "重写后的文件不含动作链接", func() bool {
			_, doc := fileOf(root, plan)
			return strings.Contains(doc, "无链接时的正文") && !strings.Contains(doc, "/act/")
		})
		_, doc := fileOf(root, plan)
		if strings.Contains(doc, "apa.acc.test") || strings.Contains(doc, "127.0.0.1") || !strings.Contains(doc, "title: exp-06") {
			t.Errorf("无 public_url 时不该有任何主机地址，其余内容照常导出:\n%s", doc)
		}
		restartKernel(string(orig))
		cfg(true, nil)
		restart()
		eventually(t, 60*time.Second, "恢复 public_url 后动作链接回来", func() bool {
			_, doc := fileOf(root, plan)
			return strings.Contains(doc, publicURL+"/act/"+plan+"?do=done")
		})
	})

	var act string
	web := f.newWebSession()
	step("ACT-EXP-16 未登录打开动作链接，先登录再到确认页", func(t *testing.T) {
		act = webCreate(map[string]any{"kind": "task", "title": "exp-16 动作链接事项", "phase": "open", "project": work, "assignee": "work-host.cli", "planned_for": "2026-12-01"})
		_, doc := fileOf(root, act)
		eventually(t, 60*time.Second, "动作链接事项的导出文件", func() bool { _, doc = fileOf(root, act); return doc != "" })
		m := regexp.MustCompile(`\]\(` + regexp.QuoteMeta(publicURL) + `(/act/[^)\s]*\?do=done)\)`).FindStringSubmatch(doc)
		if m == nil {
			t.Fatalf("导出文件里没有「完成」动作链接:\n%s", doc)
		}
		// 把主机换成 APA_URL，用没有登录态的新会话打开。
		code, page, hdr := web.do("GET", m[1], "", nil)
		if code != 200 || !strings.Contains(hdr.Get("Content-Type"), "text/html") || !strings.Contains(page, "<div id=\"root\"") {
			t.Fatalf("动作链接应由内核提供 Web 应用外壳: %d %s %.200s", code, hdr.Get("Content-Type"), page)
		}
		if code, body, _ := web.do("GET", "/v1/items/"+act, "", nil); code != 401 {
			t.Errorf("未登录读事项应被拒（页面据此先跳登录）: %d %s", code, body)
		}
		if code, body, _ := web.do("POST", "/v1/items/"+act+"/transition", `{"to":"done","reason":"未登录"}`, nil); code != 401 && code != 403 {
			t.Errorf("未登录不能执行动作: %d %s", code, body)
		}
		if p := f.show(act).Phase; p != "open" {
			t.Fatalf("未登录时事项阶段变成了 %s", p)
		}
		web.login()
		if code, body, _ := web.do("GET", "/v1/items/"+act, "", nil); code != 200 || !strings.Contains(body, act) {
			t.Fatalf("登录后读事项: %d %s", code, body)
		}
		if p := f.show(act).Phase; p != "open" {
			t.Errorf("点确认之前事项阶段不该变，实际 %s", p)
		}
	})

	step("ACT-EXP-17 在确认页完成动作，记 Web 身份的 Event", func(t *testing.T) {
		version := func() string { return fmt.Sprint(f.show(act).Version) }
		call := func(method, path, body string, want int) {
			t.Helper()
			if code, resp, _ := web.do(method, path, body, map[string]string{"If-Match": version()}); code != want {
				t.Fatalf("%s %s: %d（期望 %d）%s", method, path, code, want, resp)
			}
		}
		call("POST", "/v1/items/"+act+"/transition", `{"to":"done","reason":"Web 动作链接确认"}`, 200)
		call("POST", "/v1/items/"+act+"/progress", `{"text":"通过动作链接写进展"}`, 201)
		call("PATCH", "/v1/items/"+act, `{"planned_for":"2026-12-20"}`, 200)
		s := f.show(act)
		if s.Phase != "done" || s.PlannedFor != "2026-12-20" {
			t.Errorf("动作未生效: %+v", s)
		}
		var why struct {
			Steps []struct {
				Type  string                                `json:"type"`
				Actor struct{ Type, Principal, Via string } `json:"actor"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "why", act).Stdout), &why); err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		for _, st := range why.Steps {
			switch st.Type {
			case "item.phase_changed", "item.progress_added", "item.updated":
				if st.Actor.Type != "human" || st.Actor.Principal != "owner" || st.Actor.Via != "web" {
					t.Errorf("%s 的操作者应是 Web 本人登录，实际 %+v", st.Type, st.Actor)
				}
				seen[st.Type] = true
			}
		}
		for _, typ := range []string{"item.phase_changed", "item.progress_added", "item.updated"} {
			if !seen[typ] {
				t.Errorf("apa why 里没有 %s（三条动作各一条 Event）：%v", typ, why.Steps)
			}
		}
		doc := waitIn(t, act, "通过动作链接写进展")
		if !strings.Contains(doc, "2026-12-20") || !strings.Contains(doc, "status: done") {
			t.Errorf("导出文件没有随之更新:\n%s", doc)
		}
	})
	step("ACT-EXP-18 web_only 初筛中的决策、挂靠消息与来源事件正文不导出", func(t *testing.T) {
		// 初筛延迟 25 秒返回：期间 ext 读到的事项只有标记，没有内容可导出。
		p := 0.05
		f.Cls.Reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: baseline.ScreeningPhrase}, PYes: &p, DelayMS: 25000})
		screening := f.task("exp-18 初筛中")
		f.apa(f.WRK, 0, "ask", screening, "--question", "选哪个方案 ACC-ASK-Q", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text")
		pending := func() bool {
			var out struct {
				Item struct {
					Ask struct {
						Pending bool `json:"screening_pending"`
					} `json:"ask"`
				} `json:"item"`
			}
			_ = json.Unmarshal([]byte(f.webOK("GET", "/items/"+screening, "", 200)), &out)
			return out.Item.Ask.Pending
		}
		if !pending() {
			t.Fatal("决策请求写入后应处于初筛中")
		}
		time.Sleep(4 * time.Second) // 足够 ext 的事件循环处理这条事项
		// 事项在写入决策请求前已被导出过（item.created）；初筛未返回期间它不能被重写成带决策内容的样子。
		if _, doc := fileOf(root, screening); strings.Contains(doc, "方案A") || strings.Contains(doc, "选哪个方案") {
			t.Errorf("初筛未返回期间导出文件里出现了决策请求:\n%s", doc)
		}
		secretIn := func(needle string) []string {
			var hits []string
			for name, c := range readDirAll(root) {
				if strings.Contains(c, needle) || strings.Contains(name, needle) {
					hits = append(hits, name)
				}
			}
			return hits
		}
		if hits := secretIn("ACC-ASK-Q"); len(hits) > 0 {
			t.Errorf("决策请求出现在导出目录: %v", hits)
		}
		eventually(t, 60*time.Second, "初筛返回", func() bool { return !pending() })
		f.apa(f.DEL, 0, "item", "update", screening, "--body", "初筛后的正文", "--reason", "改", "--evidence", "验收")
		waitIn(t, screening, "初筛后的正文")
		if hits := secretIn("ACC-ASK-Q"); len(hits) > 0 {
			t.Errorf("初筛返回后决策请求也不该导出: %v", hits)
		}

		// 挂靠消息：来源事件正文含 ACC-SECRET-BODY，挂到已委派事项上。
		queue := filepath.Join(f.Root, "acc-source")
		for _, d := range []string{"in", "done"} {
			if err := os.MkdirAll(filepath.Join(queue, d), 0o755); err != nil {
				t.Fatal(err)
			}
		}
		pack := filepath.Join(f.Home, "packs", "acc-inbound")
		if err := os.MkdirAll(pack, 0o755); err != nil {
			t.Fatal(err)
		}
		rules := "name: acc-inbound\nversion: 1.0.0\ndescription: 验收用来源规则\nsource_rules:\n  - id: acc-proposed\n    match: {source_type: acc, event_type: [issue.opened]}\n    kind: task\n    project_by: \"fixed:" + work + "\"\n    then: proposed\n"
		if err := os.WriteFile(filepath.Join(pack, "pack.yaml"), []byte(rules), 0o644); err != nil {
			t.Fatal(err)
		}
		script := filepath.Join(queue, "acc-source.sh")
		const accScript = "#!/bin/sh\nQ=\"$1\"\ncat > \"$Q/last-stdin.json\"\nfor f in \"$Q\"/in/*.jsonl; do\n  [ -e \"$f\" ] || continue\n  cat \"$f\"\n  mv \"$f\" \"$Q/done/\"\ndone\necho '{\"cursor\":\"c'\"$(ls \"$Q/done\" | wc -l | tr -d ' ')\"'\"}'\n"
		if err := os.WriteFile(script, []byte(accScript), 0o755); err != nil {
			t.Fatal(err)
		}
		f.StopKernel() // 新能力包在内核启动时装载
		f.StartKernelAgain()
		src, _ := json.Marshal(map[string]any{"id": "src_acc", "type": "script", "source_type": "acc", "command": script + " " + queue, "schedule": "1s", "timeout": 30})
		f.webOK("POST", "/sources", string(src), 201)
		const pr = "https://example.invalid/acc/test-repo/pull/18"
		parent := f.task("exp-18 父事项")
		var d struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--evidence", "委派").Stdout), &d)
		child := d.ID
		if child == "" {
			t.Fatal("委派没有返回子事项 id")
		}
		f.apa(f.WRK, 0, "report", child, "--outcome", "progress", "--summary", "已开 PR", "--pr", pr)
		ev, _ := json.Marshal(map[string]any{"id": "ev-18", "type": "comment.created", "subject": "PR 评论 ACC-SECRET-BODY", "summary": "摘要 ACC-SECRET-BODY",
			"detail": "正文 ACC-SECRET-BODY", "correlation_keys": []string{pr}, "sender": "alice", "sender_verified": true, "occurred_at": time.Now().UTC().Format(time.RFC3339)})
		if err := os.WriteFile(filepath.Join(queue, "tmp-1"), append(ev, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(filepath.Join(queue, "tmp-1"), filepath.Join(queue, "in", "0001.jsonl")); err != nil {
			t.Fatal(err)
		}
		eventually(t, 30*time.Second, "来源事件挂靠到已委派事项", func() bool {
			r := f.APIRaw("GET", "/v1/sources/src_acc/events?limit=100", "", f.OBS)
			return strings.Contains(r.Body, `"disposition":"attached"`) && strings.Contains(r.Body, child)
		})
		f.apa(f.DEL, 0, "item", "update", child, "--body", "挂靠之后的正文", "--reason", "改", "--evidence", "验收")
		waitIn(t, child, "挂靠之后的正文")

		// 托管型材料只导出位置。
		var m struct {
			ID       string `json:"id"`
			Material string `json:"material"`
		}
		_ = json.Unmarshal([]byte(f.apa(f.WRK, 0, "material", "add", "--repo", f.LocalRepo, "--path", "docs/ACC-HOSTED-BODY.md", "--kind", "design", "--project", work).Stdout), &m)
		mat := m.ID + m.Material
		if mat == "" {
			t.Fatal("登记托管型材料没有返回 id")
		}
		matRel := filepath.Join("materials", mat+".md")
		eventually(t, 60*time.Second, "托管型材料的导出文件", func() bool { _, ok := readDirAll(root)[matRel]; return ok })
		if doc := readDirAll(root)[matRel]; !strings.Contains(doc, "location:") || !strings.Contains(doc, "ACC-HOSTED-BODY.md") {
			t.Errorf("托管型材料应导出位置:\n%s", doc)
		}
		if hits := secretIn("ACC-SECRET-BODY"); len(hits) > 0 {
			t.Errorf("来源事件正文出现在导出目录: %v", hits)
		}
		if hits := secretIn("ACC-ASK-Q"); len(hits) > 0 {
			t.Errorf("决策请求出现在导出目录: %v", hits)
		}
	})

	step("ACT-EXP-19 清理", func(t *testing.T) {
		export.stop()
		select {
		case <-export.done:
		default:
			t.Fatal("导出 ext 进程没有退出")
		}
		if err := syscall.Kill(export.cmd.Process.Pid, 0); err == nil {
			t.Errorf("导出 ext 进程 %d 仍然存活", export.cmd.Process.Pid)
		}
		f.ModelStub.Reset() // 恢复两个替身的默认脚本
		f.Cls.Reset()
		if _, err := os.Stat(root); err != nil {
			t.Errorf("$EXP_ROOT 应保留作为证据: %v", err)
		}
	})
}
