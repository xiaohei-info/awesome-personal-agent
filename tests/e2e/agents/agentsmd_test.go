//go:build e2e

package agents

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

type verdict struct {
	State  string `json:"state"`
	Exit   int    `json:"exit"`
	Prompt bool   `json:"prompt"`
	Item   string `json:"item"`
}

type rendered struct {
	Content   string `json:"content"`
	Hash      string `json:"hash"`
	LastCheck *struct {
		State string `json:"state"`
	} `json:"last_check"`
}

func (f *fx) render(repo string) rendered {
	f.t.Helper()
	return decode[rendered](f.t, f.webOK("GET", "/agents-md?repo="+url2(repo), "", 200))
}

func url2(s string) string { return strings.NewReplacer(":", "%3A", "/", "%2F").Replace(s) }

// in 在 dir 里以 token 运行 apa 并断言退出码（agents 命令按当前检出的 origin 找仓库）。
func (f *fx) in(dir, token string, want int, args ...string) baseline2 {
	f.t.Helper()
	code, out, errs := f.apaEnv(token, dir, nil, args...)
	if code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), code, want, out, errs)
	}
	return baseline2{out, errs}
}

type baseline2 struct{ Stdout, Stderr string }

const guidelineV1 = `## 约定

- 提交信息用中文，一行说明改了什么。
- 设计文档随代码改，走分支与 PR。

## 验证方法

- 改动后运行 ` + "`go test ./...`" + `。

## 处理规则

- 遇到资金类决策必须等本人确认。
`

// bareAndClone 建本机 bare 远程和一个检出（origin 指向 bare），返回 (远程地址, 检出目录)。
func bareAndClone(t testing.TB, root, name string, files map[string]string) (string, string) {
	t.Helper()
	bare, co := filepath.Join(root, name+".git"), filepath.Join(root, name+"-co")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	git(t, root, "clone", "-q", bare, co)
	for p, c := range files {
		writeFile(t, co, p, c)
	}
	git(t, co, "add", "-A")
	git(t, co, "commit", "-q", "-m", "init")
	git(t, co, "push", "-q", "origin", "HEAD:main")
	return bare, co
}

// driftItems 是仓库的 AGENTS.md 漂移事项（needs_input，标题或正文含仓库地址）。
func (f *fx) driftItems(repo string) []string {
	f.t.Helper()
	var ids []string
	for _, it := range decode[struct{ Items []map[string]any }](f.t, f.webOK("GET", "/items?phase=needs_input&limit=500", "", 200)).Items {
		b, _ := json.Marshal(it)
		if strings.Contains(string(b), "AGENTS.md") && strings.Contains(string(b), repo) {
			ids = append(ids, it["id"].(string))
		}
	}
	return ids
}

func (f *fx) proposedRev(material string) string {
	f.t.Helper()
	var id string
	for _, r := range f.grevisions(material) {
		if r.State == "proposed" {
			if id != "" {
				f.t.Fatalf("材料 %s 有多于一条 proposed 修订", material)
			}
			id = r.ID
		}
	}
	if id == "" {
		f.t.Fatalf("材料 %s 没有 proposed 修订", material)
	}
	return id
}

// reviseGuideline 以 Web（本人）对项目规范提交新修订，基于当前修订。
func (f *fx) reviseGuideline(project, material, content, reason string) {
	f.t.Helper()
	for _, m := range f.guidelines(project) {
		if m.ID == material {
			f.webOK("POST", "/materials/"+material+"/revisions", `{"content":`+toJSON(content)+`,"reason":`+toJSON(reason)+`,"based_on":"`+m.CurrentRev+`"}`, 201)
			return
		}
	}
	f.t.Fatalf("项目 %s 没有规范 %s", project, material)
}

// TestFlowAgentsMD 对应验收文档 flow-agents-md（ACT-AGM-01..10，本地仓库版）：规范一处修改，AGENTS.md 由账本渲染，漂移可检测。
// 代码托管来源换成 TOOL_STUB 的录制事件（gh-agentsmd-changed），仓库换成 LOCAL_REPO 的本机检出。
func TestFlowAgentsMD(t *testing.T) {
	useTools(t, "hosted-reader", "github-source")
	// 只读读取器按 GitHub 地址 clone，且只继承白名单环境变量（含 TestMain 设置的 XDG_CONFIG_HOME）：
	// 用其下的 git 配置（url.insteadOf）把这个地址落到本机的仓库，替身不碰网络（内容在 ACT-AGM-09 写入）。
	xdgGit := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "git")
	t.Cleanup(func() { os.RemoveAll(xdgGit) })
	f := setup(t, "")
	f.InstallToolStub()
	work, repo := f.Projects["acc-work"], f.LocalRepo
	co := filepath.Join(t.TempDir(), "work-co")
	git(t, filepath.Dir(co), "clone", "-q", repo, co) // LOCAL_REPO 的本机检出：origin 就是登记在 acc-work 的远程地址
	host, _ := os.Hostname()
	f.webOK("POST", "/projects/"+work+"/repos", `{"op":"set_checkout","remote":"`+repo+`","host":"`+host+`","path":"`+co+`"}`, 200)
	f.webOK("PUT", "/settings/policy", `{"web_only":{"categories":["external_commitment","funds","irreversible","permission_change","spec_activation"],"rules":[{"tag":"acc-finance","category":"funds"}]}}`, 200)

	var gid, rev, grev string
	var v1 rendered

	t.Run("ACT-AGM-01 规范生效要本人确认", func(t *testing.T) {
		r := f.apa(f.DEL, 0, "material", "commit", "--from", f.tmpFile("g.md", guidelineV1), "--reason", "初版", "--kind", "guideline", "--title", "acc-work 规范", "--project", work)
		out := decode[struct{ Material, Revision, State string }](t, r.Stdout)
		gid, rev = out.Material, out.Revision
		if gid == "" {
			t.Fatalf("commit 没有返回材料 id: %s", r.Stdout)
		}
		if rev == "" {
			rev = f.proposedRev(gid)
		}
		for _, x := range f.grevisions(gid) {
			if x.ID == rev {
				eq(t, "提交后的修订状态", x.State, "proposed")
			}
		}
		// DEL 确认：规范生效只有 Web，被拒绝（退出码 20 语义 = HTTP 403 web_only）。
		resp := f.APIRaw("POST", "/v1/revisions/"+rev+"/confirm", "{}", f.DEL)
		if resp.Code != 403 || !strings.Contains(resp.Body, "web_only") {
			t.Errorf("DEL 确认规范应被拒绝(403 web_only)：HTTP %d %s", resp.Code, resp.Body)
		}
		eq(t, "agent 不得使规范生效", len(f.events(gid, "guideline.activated")), 0)
		f.webOK("POST", "/revisions/"+rev+"/confirm", "{}", 200)
		if len(f.events("", "guideline.activated")) == 0 {
			t.Error("Web 确认后事件流应有 guideline.activated")
		}
	})

	t.Run("ACT-AGM-02 渲染内容完整", func(t *testing.T) {
		// 全局规范（本人确认生效）与两份托管文档：一份在本仓库，一份在另一仓库（不得进本仓库的索引）。
		gr := f.apa(f.DEL, 0, "material", "commit", "--from", f.tmpFile("global.md", "## 全局约定\n\n- 所有仓库：先写测试再写实现。\n"), "--reason", "全局规范", "--kind", "guideline", "--title", "全局规范")
		grev = decode[struct{ Revision string }](t, gr.Stdout).Revision
		f.webOK("POST", "/revisions/"+grev+"/confirm", "{}", 200)
		writeFile(t, f.repoDir(), "docs/guide.md", "# 指南\n")
		git(t, f.repoDir(), "add", ".")
		git(t, f.repoDir(), "commit", "-q", "-m", "docs")
		git(t, co, "pull", "-q")
		f.apa(f.WRK, 0, "material", "add", "--repo", repo, "--path", "docs/guide.md", "--kind", "design", "--project", work, "--title", "本仓库指南")
		other, _ := bareAndClone(t, t.TempDir(), "other", map[string]string{"docs/other.md": "# other\n"})
		f.webOK("POST", "/projects/"+work+"/repos", `{"op":"add_remote","remote":"`+other+`"}`, 200)
		f.apa(f.WRK, 0, "material", "add", "--repo", other, "--path", "docs/other.md", "--kind", "design", "--project", work, "--title", "别的仓库文档")

		v1 = f.render(repo)
		c := v1.Content
		hash := regexp.MustCompile(`hash=([0-9a-f]{64})`).FindStringSubmatch(c)
		if hash == nil {
			t.Fatalf("渲染缺 managed 标记里的内容 hash：\n%.400s", c)
		}
		last := -1
		for _, s := range []string{"apa:managed rev=", "hash=", "## 全局规范", "先写测试再写实现", "## 约定", "## 验证方法", "## 处理规则", "## 设计文档索引", "docs/guide.md", "## 协作约定"} {
			i := strings.Index(c, s)
			if i < 0 {
				t.Errorf("渲染缺 %q：\n%.1200s", s, c)
				continue
			}
			if i < last {
				t.Errorf("渲染顺序不对：%q 出现在前一项之前", s)
			}
			last = i
		}
		head := strings.SplitN(c, "\n", 2)[0]
		contains(t, "managed 标记的来源修订（项目规范）", head, rev)
		contains(t, "managed 标记的来源修订（全局规范）", head, grev)
		lacks(t, "不是本仓库的托管文档不得进索引", c, "other.md")
		contains(t, "固定指引", c, "经 `apa context` 查阅")
		eq(t, "标记里的 hash 与 GET 返回的一致", hash[1], v1.Hash)
		recorded := false
		for _, e := range f.events("", "") {
			if strings.Contains(toJSON(e), v1.Hash) {
				recorded = true
			}
		}
		if !recorded {
			t.Error("渲染 hash 没有记入账本（事件流里找不到）")
		}
	})

	t.Run("ACT-AGM-03 写入工作区，不提交不推送", func(t *testing.T) {
		head := git(t, co, "rev-parse", "HEAD")
		remoteHead := git(t, repo[len("file://"):], "rev-parse", "HEAD")
		f.in(co, f.OBS, 0, "agents", "sync")
		eq(t, "写入的 AGENTS.md 与渲染一致", readFile(t, filepath.Join(co, "AGENTS.md")), strings.TrimSpace(v1.Content)+"\n")
		eq(t, "HEAD 未变（不提交）", git(t, co, "rev-parse", "HEAD"), head)
		contains(t, "git status", git(t, co, "status", "--porcelain"), "AGENTS.md")
		eq(t, "远程未变（不推送）", git(t, repo[len("file://"):], "rev-parse", "HEAD"), remoteHead)
		f.in(co, f.OBS, 0, "agents", "check")
	})

	t.Run("ACT-AGM-04 过期时退出码 10，同一渲染 hash 只提示一次", func(t *testing.T) {
		v2 := strings.Replace(guidelineV1, "## 验证方法", "## 验证方法\n\n- 合并前必须通过 CI。", 1)
		f.reviseGuideline("acc-work", gid, v2, "补 CI 要求")
		f.webOK("POST", "/revisions/"+f.proposedRev(gid)+"/confirm", "{}", 200)
		vd := decode[verdict](t, f.in(co, f.OBS, 10, "agents", "check").Stdout)
		eq(t, "状态", vd.State, "stale")
		eq(t, "首次提示", vd.Prompt, true)
		// 两个新会话各开一次：会话开始时插件注入上下文并跑 check（plugins/hermes/inject）；同一渲染 hash 不再提示。
		for _, s := range []string{"acc-session-2", "acc-session-3"} {
			env := []string{"APA_SESSION=" + s}
			if code, out, errs := f.apaEnv(f.DEL, co, env, "context", "--format", "inject"); code != 0 {
				t.Fatalf("会话 %s 的 context 退出码 %d: %s%s", s, code, out, errs)
			}
			code, out, _ := f.apaEnv(f.DEL, co, env, "agents", "check")
			eq(t, "会话 "+s+" 的 check 退出码", code, 10)
			eq(t, "会话 "+s+" 不再提示", decode[verdict](t, out).Prompt, false)
		}
		if lc := f.render(repo).LastCheck; lc == nil || lc.State != "stale" {
			t.Errorf("Web 项目页读到的最近检查应为 stale：%+v", lc)
		}
		f.in(co, f.OBS, 0, "agents", "sync") // 过期时 sync 可直接覆盖
		f.in(co, f.OBS, 0, "agents", "check")
		contains(t, "覆盖后的工作区", readFile(t, filepath.Join(co, "AGENTS.md")), "合并前必须通过 CI")
	})

	hand := func(from, to string) {
		p := filepath.Join(co, "AGENTS.md")
		s := readFile(t, p)
		if !strings.Contains(s, from) {
			t.Fatalf("工作区 AGENTS.md 不含 %q", from)
		}
		if err := os.WriteFile(p, []byte(strings.Replace(s, from, to, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var drift string
	t.Run("ACT-AGM-05 漂移时退出码 11，不覆盖", func(t *testing.T) {
		hand("- 提交信息用中文，一行说明改了什么。", "- 提交信息用中文，一行说明改了什么。\n- 手改：禁止直接推 main。")
		edited := readFile(t, filepath.Join(co, "AGENTS.md"))
		vd := decode[verdict](t, f.in(co, f.OBS, 11, "agents", "check").Stdout)
		eq(t, "状态", vd.State, "drift")
		drift = vd.Item
		it := f.apa(f.OBS, 0, "item", "show", drift).Stdout
		contains(t, "漂移事项（needs_input 附 diff）", it, "needs_input", "手改：禁止直接推 main", "merge", "override", "ignore")
		f.in(co, f.OBS, 6, "agents", "sync")
		eq(t, "sync 不丢手改内容", readFile(t, filepath.Join(co, "AGENTS.md")), edited)
	})

	t.Run("ACT-AGM-06 合回", func(t *testing.T) {
		f.webOK("POST", "/items/"+drift+"/decide", `{"choice":"merge"}`, 200)
		rv := f.proposedRev(gid)
		for _, x := range f.grevisions(gid) {
			if x.ID == rv {
				contains(t, "合回的 proposed 修订", x.Content, "手改：禁止直接推 main")
			}
		}
		f.webOK("POST", "/revisions/"+rv+"/confirm", "{}", 200)
		contains(t, "重新渲染", f.render(repo).Content, "手改：禁止直接推 main")
		f.in(co, f.OBS, 0, "agents", "sync")
		f.in(co, f.OBS, 0, "agents", "check")
	})

	t.Run("ACT-AGM-07 覆盖和忽略", func(t *testing.T) {
		want := strings.TrimSpace(f.render(repo).Content) + "\n"
		hand("- 遇到资金类决策必须等本人确认。", "- 手改二：随意处理资金。")
		vd := decode[verdict](t, f.in(co, f.OBS, 11, "agents", "check").Stdout)
		f.in(co, f.OBS, 6, "agents", "sync")
		contains(t, "未选覆盖前工作区", readFile(t, filepath.Join(co, "AGENTS.md")), "手改二")
		f.webOK("POST", "/items/"+vd.Item+"/decide", `{"choice":"override"}`, 200)
		f.in(co, f.OBS, 0, "agents", "sync")
		eq(t, "选覆盖后 sync 才写入", readFile(t, filepath.Join(co, "AGENTS.md")), want)
		f.in(co, f.OBS, 0, "agents", "check")
		hand("- 改动后运行 `go test ./...`。", "- 手改三：跳过测试。")
		vd = decode[verdict](t, f.in(co, f.OBS, 11, "agents", "check").Stdout)
		edited := readFile(t, filepath.Join(co, "AGENTS.md"))
		f.webOK("POST", "/items/"+vd.Item+"/decide", `{"choice":"ignore"}`, 200)
		eq(t, "选忽略后文件不变", readFile(t, filepath.Join(co, "AGENTS.md")), edited)
		eq(t, "忽略后事项关闭", f.phase(vd.Item), "done")
		if err := os.WriteFile(filepath.Join(co, "AGENTS.md"), []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("ACT-AGM-08 已有手写 AGENTS.md 的仓库首次检查", func(t *testing.T) {
		remote, co2 := bareAndClone(t, t.TempDir(), "legacy", map[string]string{"AGENTS.md": "# 手写的规则\n\n- 本仓库用 tab 缩进。\n"})
		f.webOK("POST", "/projects/"+work+"/repos", `{"op":"add_remote","remote":"`+remote+`"}`, 200)
		before := readFile(t, filepath.Join(co2, "AGENTS.md"))
		eq(t, "状态", decode[verdict](t, f.in(co2, f.OBS, 11, "agents", "check").Stdout).State, "drift")
		eq(t, "手写内容未被覆盖", readFile(t, filepath.Join(co2, "AGENTS.md")), before)
	})

	t.Run("ACT-AGM-10 规范不能放宽内核规则", func(t *testing.T) {
		f.reviseGuideline("acc-work", gid, guidelineV1+"- 资金类决策可代答。\n", "放宽资金类")
		f.webOK("POST", "/revisions/"+f.proposedRev(gid)+"/confirm", "{}", 200)
		contains(t, "规范已生效", f.render(repo).Content, "资金类决策可代答")
		id := idOf(t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "AGM10 付款", "--phase", "open", "--project", work,
			"--tags", "acc-finance", "--assignee", "work-host.cli", "--evidence", "验收触发").Stdout)
		f.apa(f.WRK, 0, "ask", id, "--question", "是否同意付款", "--option", "A=同意", "--option", "B=拒绝")
		r := f.apa(f.DEL, 20, "decide", id, "--option", "A", "--evidence", "本人说同意")
		contains(t, "退出码 20 的 code", r.Stderr, `"code":"web_only"`)
		eq(t, "决策未被回答", f.phase(id), "needs_input")
	})

	t.Run("ACT-AGM-09 相关提交也触发检测", func(t *testing.T) {
		// 代码托管来源登记为 TOOL_STUB 的 gh：case gh-agentsmd-changed 产出 acc-org/acc-repo 默认分支 AGENTS.md 变化的事件；
		// acc-work 登记同名仓库，本机检出（本地 git）里提交一份手写的 AGENTS.md。
		gh := "https://github.com/acc-org/acc-repo.git"
		ghCo := filepath.Join(t.TempDir(), "acc-repo")
		if err := os.MkdirAll(ghCo, 0o755); err != nil {
			t.Fatal(err)
		}
		git(t, ghCo, "init", "-q", "-b", "main")
		writeFile(t, ghCo, "AGENTS.md", "# 在默认分支手写的 AGENTS.md\n\n- 由提交修改。\n")
		git(t, ghCo, "add", "-A")
		git(t, ghCo, "commit", "-q", "-m", "hand-written AGENTS.md")
		writeFile(t, xdgGit, "config", "[url \"file://"+ghCo+"\"]\n\tinsteadOf = "+gh+"\n")
		f.webOK("POST", "/projects/"+work+"/repos", `{"op":"add_remote","remote":"`+gh+`"}`, 200)
		if n := len(f.driftItems(gh)); n != 0 {
			t.Fatalf("登记来源前不应有漂移事项: %d", n)
		}
		f.SetToolCase("gh-agentsmd-changed")
		cfg, _ := json.Marshal(map[string]any{"repos": []string{"acc-org/acc-repo"}})
		cred := filepath.Join(f.Home, "sources", "src_gh")
		if err := os.MkdirAll(cred, 0o700); err != nil {
			t.Fatal(err)
		}
		f.apa(f.DEL, 0, "source", "add", "--id", "src_gh", "--type", "pull", "--command", "github-source", "--schedule", "5s", "--timeout", "60",
			"--source-type", "github", "--credentials-path", cred, "--dedupe-domain", "github", "--config", string(cfg), "--evidence", "登记代码托管来源")
		defer func() {
			if f.t.Failed() {
				t.Logf("来源：%s", f.apa(f.OBS, 0, "source", "show", "src_gh").Stdout)
				t.Logf("工具调用：%+v", f.ToolLog())
				t.Logf("needs_input：%.2000s", f.webOK("GET", "/items?phase=needs_input&limit=50", "", 200))
				t.Logf("日志：%.3000s", f.ServiceLog())
			}
		}()
		f.waitFor("来源一个周期后产生漂移事项", 30*time.Second, func() bool { return len(f.driftItems(gh)) > 0 })
		it := f.apa(f.OBS, 0, "item", "show", f.driftItems(gh)[0]).Stdout
		contains(t, "漂移事项", it, "needs_input", "AGENTS.md")
	})
}
