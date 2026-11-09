//go:build live

package guideline

import (
	"os"
	"os/exec"
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
	return jsonOf[rendered](f.t, f.webOK("GET", "/agents-md?repo="+urlq(repo), "", 200))
}

func urlq(s string) string { return strings.NewReplacer(":", "%3A", "/", "%2F").Replace(s) }

const guidelineV1 = `## 约定

- 提交信息用中文，一行说明改了什么。
- 设计文档随代码改，走分支与 PR。

## 验证方法

- 改动后运行 ` + "`go test ./...`" + `。

## 处理规则

- 遇到资金类决策必须等本人确认。
`

// TestFlowAgentsMD 对应 flow-agents-md（ACT-AGM-01..10）：真实 apa、真实 Hermes 主会话、真实 GitHub 专用测试仓库。
func TestFlowAgentsMD(t *testing.T) {
	buildGithubSource(t)
	f := setup(t)
	repo := f.TestRepo
	work := f.Projects["acc-work"]
	co := filepath.Join(f.Root, "work-co")

	// <TEST_REPO> 的本机检出：origin 就是登记在 acc-work 的远程地址。
	git(t, f.Root, "clone", repo, co)
	base := git(t, co, "rev-parse", "HEAD")
	if _, err := os.Stat(filepath.Join(co, "AGENTS.md")); err == nil {
		t.Fatal("专用测试仓库默认分支上不应已有 AGENTS.md（上次运行未清理？）")
	}
	pushed := false
	t.Cleanup(func() { // 把 ACT-AGM-09 推到默认分支的提交撤回，测试仓库保持原样。
		if !pushed {
			return
		}
		git(t, co, "fetch", "origin")
		git(t, co, "reset", "--hard", "origin/main")
		if _, err := os.Stat(filepath.Join(co, "AGENTS.md")); err == nil {
			git(t, co, "rm", "-q", "AGENTS.md")
			git(t, co, "commit", "-q", "-m", "APA-LIVE-TEST: remove AGENTS.md")
			git(t, co, "push", "origin", "HEAD:main")
		}
	})
	host := hostname()
	f.webOK("POST", "/projects/"+work+"/repos", `{"op":"set_checkout","remote":"`+repo+`","host":"`+host+`","path":"`+co+`"}`, 200)

	gfile := filepath.Join(f.Root, "g.md")
	must(t, os.WriteFile(gfile, []byte(guidelineV1), 0o644))
	var gid, rev string
	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
		f.evidence[strings.SplitN(name, " ", 2)[0]] = "passed"
	}

	step("ACT-AGM-01 规范生效要本人确认", func(t *testing.T) {
		r := f.apa(f.DEL, 0, "material", "commit", "--from", gfile, "--reason", "初版", "--kind", "guideline", "--title", "acc-work 规范", "--project", work)
		out := jsonOf[struct {
			ID       string `json:"material"`
			Revision string `json:"revision"`
			State    string `json:"state"`
		}](t, r.Stdout)
		gid, rev = out.ID, out.Revision
		if gid == "" {
			t.Fatalf("commit 没有返回材料 id: %s", r.Stdout)
		}
		if rev == "" {
			rev = f.proposedRev(gid)
		}
		for _, x := range f.revisions(gid) {
			if x.ID == rev {
				eq(t, "提交后的修订状态", x.State, "proposed")
			}
		}
		// DEL 确认：规范生效只有 Web，被拒绝（退出码 20 语义 = HTTP 403 web_only）。
		resp := f.APIRaw("POST", "/v1/revisions/"+rev+"/confirm", "{}", f.DEL)
		if resp.Code != 403 || !strings.Contains(resp.Body, "web_only") {
			t.Errorf("DEL 确认规范应被拒绝(403 web_only)：HTTP %d %s", resp.Code, resp.Body)
		}
		if len(f.events(gid, "guideline.activated")) != 0 {
			t.Error("agent 不得使规范生效")
		}
		f.webOK("POST", "/revisions/"+rev+"/confirm", "{}", 200)
		if len(f.events("", "guideline.activated")) == 0 {
			t.Error("Web 确认后事件流应有 guideline.activated")
		}
		f.evidence["ACT-AGM-01-delegate-confirm"] = resp.Code
	})

	var v1 rendered
	step("ACT-AGM-02 渲染内容完整", func(t *testing.T) {
		// 全局规范（本人确认生效）与两份托管文档：一份在 <TEST_REPO>，一份在另一仓库（不得进本仓库的索引）。
		gf := filepath.Join(f.Root, "global.md")
		must(t, os.WriteFile(gf, []byte("## 全局约定\n\n- 所有仓库：先写测试再写实现。\n"), 0o644))
		gr := f.apa(f.DEL, 0, "material", "commit", "--from", gf, "--reason", "全局规范", "--kind", "guideline", "--title", "全局规范")
		grev := jsonOf[struct {
			Revision string `json:"revision"`
		}](t, gr.Stdout).Revision
		f.webOK("POST", "/revisions/"+grev+"/confirm", "{}", 200)
		f.apa(f.WRK, 0, "material", "add", "--repo", repo, "--path", "README.md", "--kind", "design", "--project", work, "--title", "测试仓库 README")
		other, _ := bareRepo(t, f.Root, "other", map[string]string{"docs/other.md": "# other\n"})
		f.webOK("POST", "/projects/"+work+"/repos", `{"op":"add_remote","remote":"`+other+`"}`, 200)
		f.apa(f.WRK, 0, "material", "add", "--repo", other, "--path", "docs/other.md", "--kind", "design", "--project", work, "--title", "别的仓库文档")

		v1 = f.render(repo)
		c := v1.Content
		hash := regexp.MustCompile(`hash=([0-9a-f]{64})`).FindStringSubmatch(c)
		if hash == nil {
			t.Fatalf("渲染缺 managed 标记里的内容 hash：\n%.400s", c)
		}
		order := []string{"apa:managed rev=", "hash=", "## 全局规范", "先写测试再写实现", "## 约定", "## 验证方法", "## 处理规则", "## 设计文档索引", "README.md", "## 协作约定"}
		last := -1
		for _, s := range order {
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
		has(t, "managed 标记的来源修订（项目规范）", strings.SplitN(c, "\n", 2)[0], rev)
		has(t, "managed 标记的来源修订（全局规范）", strings.SplitN(c, "\n", 2)[0], grev)
		if strings.Contains(c, "other.md") {
			t.Error("不是本仓库的托管文档不得进索引")
		}
		has(t, "渲染", c, "经 `apa context` 查阅")
		if hash[1] != v1.Hash {
			t.Errorf("标记里的 hash %s 与 GET 返回的 hash %s 不一致", hash[1], v1.Hash)
		}
		recorded := false
		for _, e := range f.events("", "") {
			if b, _ := jsonMarshal(e); strings.Contains(string(b), v1.Hash) {
				recorded = true
			}
		}
		if !recorded {
			t.Error("渲染 hash 没有记入账本（事件流里找不到）")
		}
		f.evidence["ACT-AGM-02-render"] = c
	})

	step("ACT-AGM-03 写入工作区", func(t *testing.T) {
		out := f.wantIn(co, f.OBS, 0, "agents", "sync")
		got := readFile(t, filepath.Join(co, "AGENTS.md"))
		eq(t, "写入的 AGENTS.md 与渲染一致", got, strings.TrimSpace(v1.Content)+"\n")
		eq(t, "HEAD 未变（不提交）", git(t, co, "rev-parse", "HEAD"), base)
		has(t, "git status", git(t, co, "status", "--porcelain"), "AGENTS.md")
		git(t, co, "fetch", "origin")
		eq(t, "远程默认分支未变（不推送）", git(t, co, "rev-parse", "origin/main"), base)
		f.wantIn(co, f.OBS, 0, "agents", "check")
		f.evidence["ACT-AGM-03-sync"] = out.Stdout
	})

	step("ACT-AGM-04 过期时退出码 10", func(t *testing.T) {
		// 在 Web 改规范并确认：新修订 → 本人确认。
		v2 := strings.Replace(guidelineV1, "## 验证方法", "## 验证方法\n\n- 合并前必须通过 CI。", 1)
		f.revise(work, gid, v2, "补 CI 要求")
		f.webOK("POST", "/revisions/"+f.proposedRev(gid)+"/confirm", "{}", 200)
		r := f.wantIn(co, f.OBS, 10, "agents", "check")
		vd := jsonOf[verdict](t, r.Stdout)
		eq(t, "状态", vd.State, "stale")
		eq(t, "首次提示", vd.Prompt, true)
		// 同一渲染 hash 再开一个真实 Hermes 会话（插件在会话开始只跑 check），然后同一 hash 不再提示。
		out := f.hermesChatIn(co, "只回复一个字：好")
		f.saveEvidence("act-agm-04-hermes-session.txt", out)
		r2 := f.wantIn(co, f.OBS, 10, "agents", "check")
		vd2 := jsonOf[verdict](t, r2.Stdout)
		eq(t, "第二次不再提示", vd2.Prompt, false)
		lc := f.render(repo).LastCheck
		if lc == nil || lc.State != "stale" {
			t.Errorf("Web 项目页读到的最近检查应为 stale：%+v", lc)
		}
		// sync 直接覆盖。
		f.wantIn(co, f.OBS, 0, "agents", "sync")
		f.wantIn(co, f.OBS, 0, "agents", "check")
		has(t, "覆盖后的工作区", readFile(t, filepath.Join(co, "AGENTS.md")), "合并前必须通过 CI")
	})

	var drift string
	hand := func(from, to string) {
		p := filepath.Join(co, "AGENTS.md")
		s := readFile(t, p)
		if !strings.Contains(s, from) {
			t.Fatalf("工作区 AGENTS.md 不含 %q", from)
		}
		must(t, os.WriteFile(p, []byte(strings.Replace(s, from, to, 1)), 0o644))
	}
	step("ACT-AGM-05 漂移时退出码 11，不覆盖", func(t *testing.T) {
		hand("- 提交信息用中文，一行说明改了什么。", "- 提交信息用中文，一行说明改了什么。\n- 手改：禁止直接推 main。")
		edited := readFile(t, filepath.Join(co, "AGENTS.md"))
		r := f.wantIn(co, f.OBS, 11, "agents", "check")
		vd := jsonOf[verdict](t, r.Stdout)
		eq(t, "状态", vd.State, "drift")
		drift = vd.Item
		it := f.item(drift)
		eq(t, "漂移事项阶段", str(it, "phase"), "needs_input")
		b, _ := jsonMarshal(it)
		has(t, "漂移事项（附 diff）", string(b), "手改：禁止直接推 main")
		for _, opt := range []string{"merge", "override", "ignore"} {
			has(t, "漂移事项的选项", string(b), opt)
		}
		f.wantIn(co, f.OBS, 6, "agents", "sync")
		eq(t, "sync 不丢手改内容", readFile(t, filepath.Join(co, "AGENTS.md")), edited)
	})

	step("ACT-AGM-06 合回", func(t *testing.T) {
		f.webOK("POST", "/items/"+drift+"/decide", `{"choice":"merge"}`, 200)
		rv := f.proposedRev(gid)
		for _, x := range f.revisions(gid) {
			if x.ID == rv {
				has(t, "合回的 proposed 修订", x.Content, "手改：禁止直接推 main")
			}
		}
		f.webOK("POST", "/revisions/"+rv+"/confirm", "{}", 200)
		has(t, "重新渲染", f.render(repo).Content, "手改：禁止直接推 main")
		f.wantIn(co, f.OBS, 0, "agents", "sync")
		f.wantIn(co, f.OBS, 0, "agents", "check")
	})

	step("ACT-AGM-07 覆盖和忽略", func(t *testing.T) {
		want := f.render(repo).Content
		// 覆盖：未选之前 sync 不写（6）；选了之后才写入。
		hand("- 遇到资金类决策必须等本人确认。", "- 手改二：随意处理资金。")
		vd := jsonOf[verdict](t, f.wantIn(co, f.OBS, 11, "agents", "check").Stdout)
		f.wantIn(co, f.OBS, 6, "agents", "sync")
		has(t, "未选覆盖前工作区", readFile(t, filepath.Join(co, "AGENTS.md")), "手改二")
		f.webOK("POST", "/items/"+vd.Item+"/decide", `{"choice":"override"}`, 200)
		f.wantIn(co, f.OBS, 0, "agents", "sync")
		eq(t, "覆盖后文件=渲染", readFile(t, filepath.Join(co, "AGENTS.md")), strings.TrimSpace(want)+"\n")
		f.wantIn(co, f.OBS, 0, "agents", "check")
		// 忽略：不写文件，事项关闭。
		hand("- 改动后运行 `go test ./...`。", "- 手改三：跳过测试。")
		vd = jsonOf[verdict](t, f.wantIn(co, f.OBS, 11, "agents", "check").Stdout)
		edited := readFile(t, filepath.Join(co, "AGENTS.md"))
		f.webOK("POST", "/items/"+vd.Item+"/decide", `{"choice":"ignore"}`, 200)
		eq(t, "选忽略后文件不变", readFile(t, filepath.Join(co, "AGENTS.md")), edited)
		eq(t, "忽略后事项阶段", str(f.item(vd.Item), "phase"), "done")
		// 还原到渲染，供后续步骤使用。
		must(t, os.WriteFile(filepath.Join(co, "AGENTS.md"), []byte(strings.TrimSpace(want)+"\n"), 0o644))
	})

	step("ACT-AGM-08 已有手写 AGENTS.md 的仓库首次检查", func(t *testing.T) {
		remote, co2 := bareRepo(t, f.Root, "legacy", map[string]string{"AGENTS.md": "# 手写的规则\n\n- 本仓库用 tab 缩进。\n"})
		f.webOK("POST", "/projects/"+work+"/repos", `{"op":"add_remote","remote":"`+remote+`"}`, 200)
		before := readFile(t, filepath.Join(co2, "AGENTS.md"))
		r := f.wantIn(co2, f.OBS, 11, "agents", "check")
		eq(t, "状态", jsonOf[verdict](t, r.Stdout).State, "drift")
		eq(t, "手写内容未被覆盖", readFile(t, filepath.Join(co2, "AGENTS.md")), before)
	})

	step("ACT-AGM-10 规范不能放宽内核规则", func(t *testing.T) {
		v3 := guidelineV1 + "- 资金类决策可代答。\n"
		f.revise(work, gid, v3, "放宽资金类")
		f.webOK("POST", "/revisions/"+f.proposedRev(gid)+"/confirm", "{}", 200)
		has(t, "规范已生效", f.render(repo).Content, "资金类决策可代答")
		id := f.add("acc-work", "AGM10 付款", "", "acc-finance", "work-host.cli")
		f.apa(f.WRK, 0, "ask", id, "--question", "是否同意付款", "--option", "A=同意", "--option", "B=拒绝")
		r := f.apa(f.DEL, 20, "decide", id, "--option", "A", "--evidence", "本人说同意")
		has(t, "退出码 20 的 code", r.Stderr, `"code":"web_only"`)
		eq(t, "决策未被回答", len(f.events(id, "decision.resolved")), 0)
	})
	step("ACT-AGM-09 相关提交也触发检测", func(t *testing.T) {
		f.githubSource(repo)
		// 向默认分支推一个修改 AGENTS.md 的提交（与 sync 写出的渲染不同）。
		git(t, co, "fetch", "origin")
		git(t, co, "checkout", "-q", "-B", "main", "origin/main")
		must(t, os.WriteFile(filepath.Join(co, "AGENTS.md"), []byte("# 在 GitHub 上手写的 AGENTS.md\n\n- 由 APA-LIVE-TEST 推送。\n"), 0o644))
		git(t, co, "add", "AGENTS.md")
		git(t, co, "commit", "-q", "-m", "APA-LIVE-TEST: hand-written AGENTS.md")
		git(t, co, "push", "origin", "HEAD:main")
		pushed = true
		deadline := time.Now().Add(3 * time.Minute)
		for len(f.driftItems(repo)) == 0 {
			if time.Now().After(deadline) {
				t.Logf("来源状态：%s", f.apa(f.OBS, 0, "source", "show", "src_gh").Stdout)
				t.Logf("来源事件：%.1500s", f.APIRaw("GET", "/v1/sources/src_gh/events?limit=20", "", f.OBS).Body)
				t.Logf("needs_input 事项：%.2500s", f.webOK("GET", "/items?phase=needs_input&limit=50", "", 200))
				t.Fatal("GitHub 来源一个周期后没有产生漂移事项")
			}
			time.Sleep(2 * time.Second)
		}
		f.evidence["ACT-AGM-09-items"] = f.driftItems(repo)
	})

	f.flushEvidence("agents-md-evidence.json")
}

// add 用 DEL 建任务事项。
func (f *fx) add(project, title, body, tags, assignee string) string {
	f.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--project", f.Projects[project], "--evidence", "验收触发"}
	if body != "" {
		args = append(args, "--body", body)
	}
	if tags != "" {
		args = append(args, "--tags", tags)
	}
	if assignee != "" {
		args = append(args, "--assignee", assignee)
	}
	return idOf(f.t, f.apa(f.DEL, 0, args...).Stdout)
}

// hermesChatIn 以主会话方式在检出目录里问一句话（隔离 HERMES_HOME，本人侧 delegate 凭据），返回脱敏输出。
func (f *fx) hermesChatIn(dir, prompt string) string {
	f.t.Helper()
	cmd := exec.Command(f.hermesBN, "chat", "--oneshot", "-Q", "-q", prompt)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + f.Root, "PATH=" + filepath.Dir(f.Bin["apa"]) + ":" + os.Getenv("PATH"),
		"HERMES_HOME=" + f.newHermesHome("main-"+time.Now().Format("150405.000")), "APA_BIN=" + f.Bin["apa"], "APA_URL=" + f.URL,
		"APA_TOKEN=" + f.DEL, "APA_HOME=" + f.Home}
	var out strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &out
	must(f.t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			f.t.Fatalf("hermes chat 失败：%v\n%s", err, f.redact(out.String()))
		}
	case <-time.After(10 * time.Minute):
		_ = cmd.Process.Kill()
		f.t.Fatalf("hermes chat 10 分钟未返回\n%s", f.redact(out.String()))
	}
	return f.redact(out.String())
}

// revise 以 Web（本人）对项目规范提交新修订，基于当前修订。
func (f *fx) revise(project, material, content, reason string) {
	f.t.Helper()
	for _, m := range f.guidelines(project) {
		if m.ID == material {
			f.webOK("POST", "/materials/"+material+"/revisions", `{"content":`+jsonStr(content)+`,"reason":`+jsonStr(reason)+`,"based_on":"`+m.CurrentRev+`"}`, 201)
			return
		}
	}
	f.t.Fatalf("项目 %s 没有规范 %s", project, material)
}
