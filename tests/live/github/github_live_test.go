//go:build live

// Package github 按验收文档 flow-source-github（ACT-GH-01..07）用真实 APA 二进制、真实 gh 和真实 GitHub
// 专用测试仓库（APA_E2E_TEST_REPO）走完整流程。模型端点用验收规定的 MODEL_STUB（ACT-GH-02 起明文要求），
// 其余依赖全部真实：GitHub API、gh、随发行附带的 github-source、apa serve。
//
// 凭据：来源自己的 gh 配置目录（$APA_HOME/sources/<id>/）里的 token 取自本机 `gh auth token`，只经文件交接，
// 不进入输出、证据或仓库。测试只在专用仓库建带 [APA-LIVE-TEST] 前缀的 Issue，测完关闭。
// ACT-GH-07 的"凭据失效"是把来源自己目录里的 token 改成无效值，绝不撤销本机真实 token。
// Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF），与 tests/live/mail 一致。
package github

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	waitMax = 120 * time.Second
	src     = "src_gh"
	prefix  = "[APA-LIVE-TEST] "
)

func run(t *testing.T, env []string, stdin string, name string, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	if env != nil {
		cmd.Env = env
	}
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return o.String(), e.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("运行 %s: %v", name, err)
	}
	return o.String(), e.String(), 0
}

type live struct {
	*baseline.Env
	t        *testing.T
	repo     string // owner/name
	cred     string // 来源的 gh 配置目录
	evidence map[string]any
}

type srcState struct {
	Enabled         bool   `json:"enabled"`
	Cursor          string `json:"cursor"`
	CredentialsPath string `json:"credentials_path"`
	Stats           struct {
		Runs           int    `json:"runs"`
		LastError      string `json:"last_error"`
		NeedsInputItem string `json:"needs_input_item"`
	} `json:"stats"`
}

func (l *live) show() srcState {
	l.t.Helper()
	r := l.Apa(l.OBS, "", "source", "show", src)
	var s srcState
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &s) != nil {
		l.t.Fatalf("source show: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
	return s
}

func (l *live) until(what string, cond func() bool) {
	l.t.Helper()
	for deadline := time.Now().Add(waitMax); !cond(); time.Sleep(time.Second) {
		if time.Now().After(deadline) {
			st := l.show()
			// 内核不保留拉取程序的 stderr：用同样的输入在同一目录里重跑一次，把原因带进失败信息。
			in := fmt.Sprintf(`{"source_id":%q,"config":{"repos":[%q]},"cursor":"","window":{"from":"","to":%q},"run_id":"diag","credentials_path":%q}`, src, l.repo, time.Now().UTC().Format(time.RFC3339), l.cred)
			_, se, code := run(l.t, nil, in, "github-source")
			l.t.Logf("重跑 github-source: exit=%d stderr=%q", code, se)
			l.t.Fatalf("%v 内没有等到：%s（runs=%d last_error=%q needs_input=%q）", waitMax, what, st.Stats.Runs, st.Stats.LastError, st.Stats.NeedsInputItem)
		}
	}
}

// newIssue 用本机真实 gh 账号在专用测试仓库建 Issue，测完关闭。返回标题和编号。
func (l *live) newIssue(tag string) (title, number string) {
	l.t.Helper()
	title = fmt.Sprintf("%s%s %d", prefix, tag, time.Now().UnixNano())
	out, errs, code := run(l.t, nil, "", "gh", "issue", "create", "-R", l.repo, "--title", title, "--body", "APA live e2e 测试 Issue，会被自动关闭。")
	if code != 0 {
		l.t.Fatalf("gh issue create: %d %s", code, errs)
	}
	number = out[strings.LastIndex(strings.TrimSpace(out), "/")+1:]
	number = strings.TrimSpace(number)
	l.t.Cleanup(func() { run(l.t, nil, "", "gh", "issue", "close", "-R", l.repo, number) })
	return title, number
}

type event struct {
	Subject     string `json:"subject"`
	Type        string `json:"type"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	Link        string `json:"link"`
	Key         struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
}

func (l *live) events() []event {
	l.t.Helper()
	r := l.APIRaw("GET", "/v1/sources/"+src+"/events?limit=500", "", l.OBS)
	if r.Code != 200 {
		l.t.Fatalf("读来源事件 HTTP %d: %s", r.Code, r.Body)
	}
	var out struct {
		Events []event `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		l.t.Fatal(err)
	}
	return out.Events
}

func (l *live) eventFor(title string) *event {
	for _, e := range l.events() {
		if e.Subject == title && e.Type == "issue.opened" {
			e := e
			return &e
		}
	}
	return nil
}

func (l *live) waitEvent(title string) event {
	l.t.Helper()
	l.until("来源产生 Issue 事件 "+title, func() bool { return l.eventFor(title) != nil })
	// 事件落盘后判断型才跑完，等 disposition 离开空值。
	l.until("事件被处理", func() bool { return l.eventFor(title).Disposition != "" })
	return *l.eventFor(title)
}

func (l *live) item(id string) map[string]any {
	l.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	r := l.Apa(l.OBS, "", "item", "show", id)
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &out) != nil {
		l.t.Fatalf("item show %s: %d %s %s", id, r.Code, r.Stdout, r.Stderr)
	}
	return out.Item
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

// suggestionIDs 是 Web 建议流（phase=proposed 的事项）当前的事项 id。
func (l *live) suggestionIDs() map[string]bool {
	l.t.Helper()
	r := l.Web("GET", "/v1/items?phase=proposed&limit=500", "")
	if r.Code != 200 {
		l.t.Fatalf("读建议流 HTTP %d: %s", r.Code, r.Body)
	}
	var out struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		l.t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, it := range out.Items {
		if str(it, "phase") == "proposed" {
			ids[str(it, "id")] = true
		}
	}
	return ids
}

func (l *live) deliveries() int {
	l.t.Helper()
	r := l.Web("GET", "/v1/deliveries/unread", "")
	var out struct {
		Deliveries []map[string]any `json:"deliveries"`
	}
	if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil {
		l.t.Fatalf("读未读投递: %d %s", r.Code, r.Body)
	}
	return len(out.Deliveries)
}

func (l *live) webOK(method, path, body string, want int) string {
	l.t.Helper()
	r := l.Web(method, path, body)
	if r.Code != want {
		l.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (l *live) record(k string, v any) { l.evidence[k] = v }

func TestFlowSourceGitHub(t *testing.T) {
	repoURL := os.Getenv("APA_E2E_TEST_REPO")
	if repoURL == "" {
		t.Fatal("缺少环境变量 APA_E2E_TEST_REPO（先 source APA_LIVE_ENV_FILE）")
	}
	repo := strings.TrimSuffix(strings.TrimPrefix(repoURL, "https://github.com/"), ".git")
	if _, _, code := run(t, nil, "", "gh", "auth", "status"); code != 0 {
		t.Fatal("本机 gh 未登录，无法建测试 Issue")
	}
	tokOut, _, code := run(t, nil, "", "gh", "auth", "token")
	token := strings.TrimSpace(tokOut)
	if code != 0 || token == "" {
		t.Fatal("取不到本机 gh token")
	}

	// 随发行附带的拉取程序按裸名登记（设计 §9.1），apa serve 继承测试进程的 PATH。
	progDir := t.TempDir()
	repoRoot, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	build := exec.Command("go", "build", "-o", filepath.Join(progDir, "github-source"), "./sources/github/cmd/github-source")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 github-source: %v\n%s", err, out)
	}
	t.Setenv("PATH", progDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	l := &live{t: t, repo: repo, evidence: map[string]any{"repo": repo}}
	l.Env = baseline.New(t)
	l.StartKernel()
	l.IssueCredentials()
	l.CreateProjects()
	l.StartModelStub()
	// 决策投递走渠道 acc-chan 且不合并；模型端点超时放宽（默认 2s 对真实流程偏紧）。
	l.webOK("PUT", "/v1/settings/policy", `{"disturb":{"routes":{"brief":"inject","decision":"channel:acc-chan","health":"web","reminder":"inject","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true}}`, 200)
	l.cred = filepath.Join(l.Home, "sources", src)
	stub := l.ModelStub

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
		l.record(strings.SplitN(name, " ", 2)[0], "passed")
	}

	var acceptedItem string

	step("ACT-GH-01 登记并认证 GitHub 来源", func(t *testing.T) {
		cfg, _ := json.Marshal(map[string]any{"repos": []string{repo}})
		r := l.Apa(l.DEL, "", "source", "add", "--id", src, "--type", "pull", "--command", "github-source", "--schedule", "5s",
			"--timeout", "60", "--source-type", "github", "--credentials-path", l.cred, "--dedupe-domain", "github", "--config", string(cfg), "--evidence", "登记附带的 GitHub 拉取程序")
		if r.Code != 0 {
			t.Fatalf("source add: %d %s %s", r.Code, r.Stdout, r.Stderr)
		}
		// `apa source auth` 只是在来源目录里启动 `gh auth login --insecure-storage`，那是交互式浏览器/设备码登录，
		// 无人值守跑不了（实测会一直等待）。这里执行同一个 gh、同一个落点（GH_CONFIG_DIR=来源目录、明文存储），
		// 只把交互换成 --with-token；token 取自本机 gh，经 stdin 交接，不进输出。
		if err := os.MkdirAll(l.cred, 0o700); err != nil {
			t.Fatal(err)
		}
		var env []string
		for _, kv := range os.Environ() {
			if k, _, _ := strings.Cut(kv, "="); k != "GH_TOKEN" && k != "GITHUB_TOKEN" && k != "GH_CONFIG_DIR" {
				env = append(env, kv)
			}
		}
		if _, _, code := run(t, append(env, "GH_CONFIG_DIR="+l.cred), token+"\n", "gh", "auth", "login", "--with-token", "--insecure-storage"); code != 0 {
			t.Fatalf("来源目录认证退出码 %d", code)
		}
		l.record("ACT-GH-01-auth", "gh auth login --with-token --insecure-storage（apa source auth 为交互式登录，无人值守不可用）")
		if !l.show().Enabled {
			t.Fatal("来源应为 enabled")
		}
		// 首次拉取只记游标起点；等它成功后再建 Issue，否则先建的 Issue 会落在起点之前被漏掉。
		l.until("首次拉取成功", func() bool { st := l.show(); return st.Cursor != "" && st.Stats.LastError == "" })
		if fi, err := os.Stat(l.cred); err != nil || !fi.IsDir() {
			t.Fatalf("凭据目录不存在: %v", err)
		}
		out, _, _ := run(t, nil, "", "grep", "-rl", token, l.Home)
		for _, f := range strings.Fields(out) {
			if !strings.HasPrefix(f, l.cred+string(os.PathSeparator)) {
				t.Errorf("token 出现在来源目录之外: %s", strings.TrimPrefix(f, l.Home))
			}
		}
		if !strings.Contains(out, l.cred) {
			t.Error("token 没有落在 $APA_HOME/sources/<id>/ 下")
		}
		if db, _, _ := run(t, nil, "", "grep", "-rl", token, filepath.Join(l.Home, "ledger.db")); strings.TrimSpace(db) != "" {
			t.Error("token 写进了 ledger.db")
		}
		pr := l.Web("GET", "/v1/projects/"+l.Projects["acc-work"], "")
		if !strings.Contains(pr.Body, repoURL) {
			t.Errorf("acc-work 的 repos 里没有 %s: %s", repoURL, pr.Body)
		}
	})

	step("ACT-GH-02 新 Issue 变成建议", func(t *testing.T) {
		stub.Reset()
		stub.SetDefault(modelstub.Reply{Content: `{"choice":"task","confidence":0.95}`})
		before := l.deliveries()
		title, num := l.newIssue("suggest")
		ev := l.waitEvent(title)
		if ev.Disposition != "proposed" || ev.Item == "" {
			t.Fatalf("事件应为 proposed 且带事项: %+v", ev)
		}
		it := l.item(ev.Item)
		if str(it, "phase") != "proposed" || str(it, "kind") != "task" {
			t.Errorf("事项应为 proposed/task: %v", it)
		}
		if str(it, "project") != l.Projects["acc-work"] {
			t.Errorf("事项应按远程地址归入 acc-work(%s): project=%v", l.Projects["acc-work"], it["project"])
		}
		if !l.suggestionIDs()[ev.Item] {
			t.Error("Web 建议流里没有该事项")
		}
		if !strings.HasSuffix(ev.Link, "/issues/"+num) {
			t.Errorf("事件链接应指向真实 Issue #%s: %s", num, ev.Link)
		}
		if got := l.deliveries(); got != before {
			t.Errorf("建议不应产生投递: %d → %d", before, got)
		}
		if r := l.Apa(l.DEL, "", "context", "--project", l.Projects["acc-work"]); r.Code != 0 || !strings.Contains(r.Stdout, ev.Item) && !strings.Contains(r.Stdout, title) {
			t.Errorf("apa context 应带出该建议: code=%d out=%.300s err=%.300s", r.Code, r.Stdout, r.Stderr)
		}
		acceptedItem = ev.Item
		l.record("ACT-GH-02-item", ev.Item)
	})

	step("ACT-GH-03 低置信时生成标题和摘要", func(t *testing.T) {
		stub.Reset()
		// 生成型提示词含"建议标题"；规则先命中它，其余请求（判断型）返回低置信。
		stub.When("建议标题", modelstub.Reply{Content: `{"insufficient_info":false,"title":"修复登录 500","summary":"登录接口返回 500，需要排查"}`})
		stub.SetDefault(modelstub.Reply{Content: `{"choice":"task","confidence":0.4}`})
		title, _ := l.newIssue("lowconf")
		ev := l.waitEvent(title)
		if ev.Disposition != "proposed" || ev.Item == "" {
			t.Fatalf("事件应为 proposed: %+v", ev)
		}
		it := l.item(ev.Item)
		if str(it, "title") != "修复登录 500" || str(it, "summary") != "登录接口返回 500，需要排查" {
			t.Errorf("标题和摘要应来自生成型: title=%v summary=%v", it["title"], it["summary"])
		}
		calls := 0
		for _, rq := range stub.Requests() {
			if strings.Contains(rq.BodyText, title) {
				calls++
			}
		}
		if calls < 2 {
			t.Errorf("应有判断型 + 生成型两级调用，实际 %d 次", calls)
		}
		// 两级调用的权威记录是审计流里的 run.finished（Web 审计页同源）：判断型低置信，紧接着生成型，
		// 然后才是该事件的 inbound.disposed 和事项创建。apa why <事项> 只列事项自己的步骤，不含这两次调用。
		type auditEv struct {
			Seq     int64          `json:"seq"`
			Type    string         `json:"type"`
			Object  string         `json:"object"`
			Reason  string         `json:"reason"`
			Payload map[string]any `json:"payload"`
		}
		audit := func(typ string) []auditEv {
			var out struct {
				Events []auditEv `json:"events"`
			}
			if err := json.Unmarshal([]byte(l.webOK("GET", "/v1/audit/events?limit=500&type="+typ, "", 200)), &out); err != nil {
				t.Fatal(err)
			}
			return out.Events
		}
		ext, _ := l.item(ev.Item)["ext"].(map[string]any)
		sev := str(ext, "source_event")
		var disposed *auditEv
		for _, e := range audit("inbound.disposed") {
			if e.Object == sev {
				e := e
				disposed = &e
			}
		}
		if disposed == nil {
			t.Fatalf("审计里没有事件 %s 的 inbound.disposed", sev)
		}
		var judge, gen *auditEv // 取 disposed 之前最近的一次判断型和一次生成型
		for _, e := range audit("run.finished") {
			e := e
			if e.Seq >= disposed.Seq {
				continue
			}
			switch e.Payload["category"] {
			case "judge":
				judge = &e
			case "generate":
				gen = &e
			}
		}
		if judge == nil || gen == nil {
			t.Fatalf("事件 %s 之前应各有一次判断型和生成型 run.finished: judge=%v gen=%v", sev, judge != nil, gen != nil)
		}
		if conf, _ := judge.Payload["confidence"].(float64); judge.Payload["choice"] != "task" || conf != 0.4 || judge.Payload["template"] != "triage" || judge.Payload["state"] != "succeeded" {
			t.Errorf("判断型应为低置信 task: %v", judge.Payload)
		}
		res, _ := gen.Payload["result"].(map[string]any)
		if gen.Payload["template"] != "triage-title" || gen.Payload["state"] != "succeeded" || str(res, "title") != "修复登录 500" {
			t.Errorf("生成型应产出标题: %v", gen.Payload)
		}
		if !(judge.Seq < gen.Seq && gen.Seq < disposed.Seq) || judge.Object == gen.Object {
			t.Errorf("应为判断型 → 生成型 → 处置，实际 seq %d/%d/%d", judge.Seq, gen.Seq, disposed.Seq)
		}
		// 事项自己的时间线记录了判断结果和命中规则。
		r := l.Apa(l.DEL, "", "why", ev.Item)
		if r.Code != 0 || !strings.Contains(r.Stdout, "judged task (confidence 0.40)") || !strings.Contains(r.Stdout, "github-issue") {
			t.Errorf("apa why 应带判断结果与规则: code=%d %.300s", r.Code, r.Stdout)
		}
		l.record("ACT-GH-03-runs", []string{judge.Object, gen.Object})
	})

	step("ACT-GH-04 判断为不值得时只记录", func(t *testing.T) {
		stub.Reset()
		stub.SetDefault(modelstub.Reply{Content: `{"choice":"ignore","confidence":0.95}`})
		beforeSug, beforeDel := len(l.suggestionIDs()), l.deliveries()
		title, _ := l.newIssue("ignore")
		ev := l.waitEvent(title)
		if ev.Disposition != "ignored" || ev.Item != "" {
			t.Fatalf("事件应为 ignored 且无事项: %+v", ev)
		}
		if n := len(l.suggestionIDs()); n != beforeSug {
			t.Errorf("建议流不应新增: %d → %d", beforeSug, n)
		}
		if n := l.deliveries(); n != beforeDel {
			t.Errorf("不应有投递: %d → %d", beforeDel, n)
		}
	})

	step("ACT-GH-05 接受后进入决策并送达", func(t *testing.T) {
		stub.Reset()
		stub.SetDefault(modelstub.Reply{Content: `{"probability":0.05,"confidence":0.95}`})
		l.webOK("POST", "/v1/items/"+acceptedItem+"/accept", `{}`, 200)
		if p := str(l.item(acceptedItem), "phase"); p != "open" {
			t.Fatalf("接受后应为 open，实际 %s", p)
		}
		r := l.Apa(l.DEL, "", "ask", acceptedItem, "--evidence", "请本人决定", "--question", "这个 Issue 先处理哪部分", "--option", "A=先排查", "--option", "B=先回复", "--next", "A=done", "--recommend", "A", "--free-text")
		if r.Code != 0 {
			t.Fatalf("apa ask: %d %s %s", r.Code, r.Stdout, r.Stderr)
		}
		if p := str(l.item(acceptedItem), "phase"); p != "needs_input" {
			t.Fatalf("ask 后应为 needs_input，实际 %s", p)
		}
		var got string
		l.until("CHAN 领到该事项的投递", func() bool {
			resp := l.APIRaw("GET", "/v1/deliveries/next?wait=5", "", l.CHAN)
			if resp.Code == 200 {
				got = resp.Body
			}
			return strings.Contains(got, acceptedItem)
		})
	})

	step("ACT-GH-06 决定后回放完整来龙去脉", func(t *testing.T) {
		l.webOK("POST", "/v1/items/"+acceptedItem+"/decide", `{"choice":"A"}`, 200)
		l.until("事项进入 done", func() bool { return str(l.item(acceptedItem), "phase") == "done" })
		r := l.Apa(l.DEL, "", "why", acceptedItem)
		if r.Code != 0 {
			t.Fatalf("apa why: %d %s", r.Code, r.Stderr)
		}
		var tl struct {
			Steps []struct {
				Type    string         `json:"type"`
				Reason  string         `json:"reason"`
				Payload map[string]any `json:"payload"`
				Actor   struct {
					Type, Principal, Via string
				} `json:"actor"`
			} `json:"steps"`
		}
		if err := json.Unmarshal([]byte(r.Stdout), &tl); err != nil {
			t.Fatalf("why 输出不是时间线: %v", err)
		}
		var types []string
		for _, s := range tl.Steps {
			types = append(types, s.Type)
		}
		l.record("ACT-GH-06-why-step-types", types)
		raw := r.Stdout
		for _, want := range []string{"item.created", "proposal.accept", "decision.requested", "decision.resolved"} {
			if !strings.Contains(raw, want) {
				t.Errorf("回放缺 %s；现有步骤 %v", want, types)
			}
		}
		if !strings.Contains(raw, src) {
			t.Errorf("回放缺来源事件（%s）；步骤 %v", src, types)
		}
		if !strings.Contains(raw, "github-issue") {
			t.Errorf("回放缺命中规则 github-issue；步骤 %v", types)
		}
		if !strings.Contains(raw, "task") || !strings.Contains(raw, "0.95") {
			t.Errorf("回放缺判断结果；步骤 %v", types)
		}
	})

	step("ACT-GH-07 GitHub 认证失败时找本人", func(t *testing.T) {
		hosts := filepath.Join(l.cred, "hosts.yml")
		good, err := os.ReadFile(hosts)
		if err != nil {
			t.Fatalf("来源凭据文件不存在: %v", err)
		}
		// 无效 token 运行时拼出，源码里不留 token 形态，避免触发 secret scanning。
		bad := []byte("github.com:\n    oauth_token: " + "ghp" + "_jcoscuyrdquviikpwbpvorcdvwtkfzxamaf2631" + "\n    user: nobody\n    git_protocol: https\n")
		if err := os.WriteFile(hosts, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		cursor := l.show().Cursor
		stub.Reset()
		stub.SetDefault(modelstub.Reply{Content: `{"choice":"task","confidence":0.95}`})
		title, _ := l.newIssue("authfail")
		l.until("以退出码 2 失败并生成 needs_input", func() bool {
			st := l.show()
			return st.Stats.NeedsInputItem != "" && strings.Contains(st.Stats.LastError, "exit status 2")
		})
		st := l.show()
		if !strings.Contains(st.Stats.LastError, "exit status 2") {
			t.Errorf("拉取程序应以退出码 2 失败: last_error=%q", st.Stats.LastError)
		}
		if st.Cursor != cursor {
			t.Errorf("游标不应前进: %q → %q", cursor, st.Stats.LastError)
		}
		if l.eventFor(title) != nil {
			t.Error("认证失败期间不应产生该 Issue 的事件")
		}
		// 程序直接运行，核对退出码 2 与 stdout 无输出（不输出新游标）。
		in := fmt.Sprintf(`{"source_id":%q,"config":{"repos":[%q]},"cursor":%q,"credentials_path":%q}`, src, repo, cursor, l.cred)
		o, _, code := run(t, nil, in, "github-source")
		if code != 2 || o != "" {
			t.Errorf("github-source 应退出码 2 且 stdout 为空: code=%d stdout=%q", code, o)
		}
		if err := os.WriteFile(hosts, good, 0o600); err != nil {
			t.Fatal(err)
		}
		l.until("恢复后拿到认证失败期间创建的 Issue", func() bool { return l.eventFor(title) != nil })
	})

	if dir := os.Getenv("APA_E2E_EVIDENCE_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			b, _ := json.MarshalIndent(l.evidence, "", "  ")
			_ = os.WriteFile(filepath.Join(dir, "source-github-live.json"), b, 0o644)
		}
	}
}
