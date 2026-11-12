//go:build e2e

package baseline

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// TestFlowEnvBaseline 按验收文档 flow-env-baseline 的顺序，用真实 apa 测试构建逐步建立环境并核对每个动作的"通过"判据
// （ACT-ENV-01..10）。任一步失败即停止，后续步骤不得开始（ACT-ENV-01 的失败语义）。
func TestFlowEnvBaseline(t *testing.T) {
	e := New(t)
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}

	step("ACT-ENV-01 隔离数据目录并启动测试构建内核", func(t *testing.T) {
		e.StartKernel()
		for _, f := range []string{"config.yaml", "ledger.db", "blobs", "packs", "sources", "runs", "logs"} {
			if _, err := os.Stat(filepath.Join(e.Home, f)); err != nil {
				t.Errorf("APA_HOME 缺少 %s: %v", f, err)
			}
		}
		if !strings.HasPrefix(e.Home, e.Root) {
			t.Errorf("APA_HOME 必须在测试专用目录内: %s", e.Home)
		}
		resp, err := http.Get(e.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		page, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Type"), "text/html") || !strings.Contains(string(page), "<div id=\"root\">") {
			t.Fatalf("Web 登录页打不开: %d %s", resp.StatusCode, page)
		}
		// 登录是真的：错误口令被拒绝且读不到账本；正确口令能读。
		if r := e.WebRaw("POST", "/v1/auth/login", `{"password":"wrong-password-x"}`, false); r.Code != http.StatusUnauthorized && r.Code != http.StatusForbidden {
			t.Errorf("错误口令应被拒绝，实际 %d", r.Code)
		}
		if r := e.Web("GET", "/v1/settings/executors", ""); r.Code != 200 {
			t.Errorf("Web 登录后应能读设置: %d %s", r.Code, r.Body)
		}
		// 测试构建：`apa version`（离线、无凭据）与 GET /version 同一份内容，test_clock 为 true。
		r := e.Apa("", "", "version")
		var cli struct {
			Version   string `json:"version"`
			TestClock bool   `json:"test_clock"`
		}
		if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &cli) != nil || !cli.TestClock || cli.Version == "" {
			t.Fatalf("apa version 应退出 0 且 test_clock 为 true: %d %s %s", r.Code, r.Stdout, r.Stderr)
		}
		if g := e.APIRaw("GET", "/v1/version", "", ""); g.Code != 200 || !strings.Contains(g.Body, `"test_clock":true`) {
			t.Errorf("GET /version 无需凭据且 test_clock 为 true: %d %s", g.Code, g.Body)
		}
		if out, err := exec.Command("sqlite3", "--version").CombinedOutput(); err != nil {
			t.Fatalf("本机缺少 sqlite3 CLI: %v %s", err, out)
		}
	})

	step("ACT-ENV-02 签发五个凭据并维护执行者清单", func(t *testing.T) {
		e.IssueCredentials()
		for name, tok := range map[string]string{"OBS": e.OBS, "WRK": e.WRK, "WRK2": e.WRK2, "DEL": e.DEL, "CHAN": e.CHAN} {
			if tok == "" {
				t.Errorf("%s 没有值", name)
			}
		}
		if r := e.Apa(e.OBS, "", "inbox"); r.Code != 0 {
			t.Errorf("OBS apa inbox 退出码 %d: %s", r.Code, r.Stderr)
		}
		for name, tok := range map[string]string{"WRK": e.WRK, "WRK2": e.WRK2, "DEL": e.DEL} {
			if r := e.Apa(tok, "", "inbox"); r.Code != 0 {
				t.Errorf("%s 不能认证: 退出码 %d %s", name, r.Code, r.Stderr)
			}
		}
		if r := e.APIRaw("GET", "/v1/deliveries/next?wait=0", "", e.CHAN); r.Code != http.StatusNoContent {
			t.Errorf("CHAN 应能认证并领到空队列(204)，实际 %d %s", r.Code, r.Body)
		}
		if r := e.Apa("invalid-token", "", "inbox"); r.Code != 3 {
			t.Errorf("无效 token 应是退出码 3，实际 %d", r.Code)
		}
		r := e.Apa(e.OBS, "", "executors")
		for _, id := range []string{"work-host.cli", "other-host.cli"} {
			if r.Code != 0 || !strings.Contains(r.Stdout, `"`+id+`"`) {
				t.Errorf("执行者清单缺少 %s: %s", id, r.Stdout)
			}
		}
		if !strings.Contains(r.Stdout, `"external-cli"`) {
			t.Errorf("执行者 kind 应是 external-cli: %s", r.Stdout)
		}
	})

	step("ACT-ENV-03 建立本地代码仓库与四个验收项目", func(t *testing.T) {
		e.CreateProjects()
		if !strings.HasPrefix(e.LocalRepo, "file://") {
			t.Fatalf("LOCAL_REPO 应是 file:// 地址: %s", e.LocalRepo)
		}
		if out, err := exec.Command("git", "-C", strings.TrimPrefix(e.LocalRepo, "file://"), "log", "--oneline").Output(); err != nil || len(out) == 0 {
			t.Errorf("本地仓库应是已提交过一次的 git 仓库: %v %s", err, out)
		}
		show := func(name string) map[string]json.RawMessage {
			r := e.Apa(e.OBS, "", "project", "show", e.Projects[name])
			if r.Code != 0 {
				t.Fatalf("project show %s: 退出码 %d %s", name, r.Code, r.Stderr)
			}
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil {
				t.Fatal(err)
			}
			return m
		}
		work := show("acc-work")
		if got := string(work["allowed_executors"]); got != `["work-host.cli"]` {
			t.Errorf("acc-work allowed_executors = %s", got)
		}
		if !strings.Contains(string(work["repos"]), e.LocalRepo) {
			t.Errorf("acc-work repos 缺少 LOCAL_REPO: %s", work["repos"])
		}
		if got := string(show("acc-open")["allowed_executors"]); got != "null" {
			t.Errorf("acc-open 应为未设置(null)，实际 %s", got)
		}
		if got := string(show("acc-empty")["allowed_executors"]); got != "[]" {
			t.Errorf("acc-empty 应为空列表，实际 %s", got)
		}
		if home := show("acc-home"); len(home["repos"]) > 0 && string(home["repos"]) != "null" {
			t.Errorf("acc-home 不应有 repos: %s", home["repos"])
		}
	})

	step("ACT-ENV-04 分类端点替身 CLS_STUB", func(t *testing.T) {
		e.StartCls()
		hi, lo := 0.91, 0.12
		e.Cls.PYes(modelstub.Key{Contains: "ACC-INV-HI"}, hi)
		e.Cls.PYes(modelstub.Key{Contains: "ACC-INV-LO"}, lo)
		ask := func(first, second string) map[string]float64 {
			return e.clsBatch(t, [][2]string{{"q1", first}, {"q2", second}})
		}
		if got := ask("ACC-INV-HI", "ACC-INV-LO"); got["q1"] != hi || got["q2"] != lo {
			t.Errorf("p_yes 应按内容对应: %v", got)
		}
		// 交换顺序后仍按内容对应，不随调用顺序变化。
		if got := ask("ACC-INV-LO", "ACC-INV-HI"); got["q1"] != lo || got["q2"] != hi {
			t.Errorf("交换题目顺序后 p_yes 应跟着内容走: %v", got)
		}
		b := e.Cls.Batches()
		if len(b) != 2 || b[0].Questions != 2 || !strings.Contains(b[0].BodyText, "ACC-INV-HI") || !strings.Contains(b[0].BodyText, "probability-judgment/1") {
			t.Errorf("CLS_LOG 应记录完整请求体与题数 2: %+v", b)
		}
		e.Cls.SetFault(modelstub.ClsFault{Status: 429})
		if code := e.clsStatus(t, [][2]string{{"q1", "ACC-INV-HI"}}); code != 429 {
			t.Errorf("配成 429 后应返回 429，实际 %d", code)
		}
		e.Cls.SetFault(modelstub.ClsFault{})
		e.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "ACC-INV-ERR"}, Error: "unsupported_question"})
		if raw := e.clsRaw(t, [][2]string{{"q1", "ACC-INV-ERR"}, {"q2", "ACC-INV-HI"}}); !strings.Contains(raw, "unsupported_question") || !strings.Contains(raw, `"p_yes":0.91`) {
			t.Errorf("单题错误不应影响同批其他题: %s", raw)
		}
	})

	step("ACT-ENV-05 对话端点替身 MODEL_STUB", func(t *testing.T) {
		e.StartModelStub()
		yes := 0.91
		e.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "ACC-INV-HI"}, Answer: "no", PYes: &yes})
		// 同一匹配键：分类端点给概率，对话端点给（可以不同的）答案。
		if got := e.clsBatch(t, [][2]string{{"q1", "ACC-INV-HI"}}); got["q1"] != 0.91 {
			t.Errorf("CLS_STUB 同键概率 = %v", got)
		}
		var ans struct {
			Answer     string `json:"answer"`
			Sufficient bool   `json:"sufficient"`
		}
		if err := json.Unmarshal([]byte(e.chatContent(t, e.judgeChat("ACC-INV-HI 判断题", false))), &ans); err != nil || ans.Answer != "no" || !ans.Sufficient {
			t.Errorf("MODEL_STUB 判断题答案 = %+v %v", ans, err)
		}
		e.ModelStub.Enqueue(modelstub.Reply{Content: "scripted-reply-1"})
		if got := e.chatContent(t, e.genChat("hello-env-05")); got != "scripted-reply-1" {
			t.Errorf("生成请求没有返回脚本内容: %q", got)
		}
		reqs := e.ModelStub.Requests()
		if len(reqs) != 2 || !IsJudgeRequest(reqs[0]) || IsJudgeRequest(reqs[1]) || !strings.Contains(reqs[1].BodyText, "hello-env-05") {
			t.Errorf("MODEL_LOG 应能区分判断题与生成请求并记录完整请求体: %+v", reqs)
		}
		if raw := e.chatRaw(t, e.judgeChat("ACC-INV-HI 判断题", true)); !strings.Contains(raw, `"logprobs"`) {
			t.Errorf("开启对数概率后响应应含 logprobs: %s", raw)
		}
		e.ModelStub.Enqueue(modelstub.Reply{Status: 500})
		if !strings.Contains(e.chatRaw(t, e.genChat("boom")), "error") {
			t.Error("HTTP 500 不可脚本化")
		}
	})

	step("ACT-ENV-06 无头运行替身及其声明", func(t *testing.T) {
		e.DeclareStubHarness()
		r := e.Web("GET", "/v1/settings/commands", "")
		if r.Code != 200 {
			t.Fatalf("读取已生效声明: %d %s", r.Code, r.Body)
		}
		for _, h := range []string{"acc-stub", "acc-stub-noresume"} {
			if !strings.Contains(r.Body, `"`+h+`"`) {
				t.Errorf("声明 %s 未生效: %s", h, r.Body)
			}
		}
		if p := e.Web("GET", "/v1/settings/declarations", ""); strings.Contains(p.Body, "acc-stub") {
			t.Errorf("声明仍在待确认: %s", p.Body)
		}
		// 声明的命令真能跑：no-receipt 模式退出 0；exit1 模式退出 1；运行目录里留下环境变量记录。
		for mode, want := range map[string]int{"no-receipt": 0, "exit1": 1} {
			dir := t.TempDir()
			cmd := exec.Command(e.Bin["stub-harness"], "STUB_MODE="+mode)
			cmd.Dir = dir
			err := cmd.Run()
			got := 0
			if ee, ok := err.(*exec.ExitError); ok {
				got = ee.ExitCode()
			} else if err != nil {
				t.Fatal(err)
			}
			if got != want {
				t.Errorf("STUB_HARNESS %s 退出码 %d，期望 %d", mode, got, want)
			}
			if _, err := os.Stat(filepath.Join(dir, "stub-harness-env.json")); err != nil {
				t.Errorf("STUB_HARNESS %s 没有留下环境变量记录: %v", mode, err)
			}
		}
	})

	step("ACT-ENV-07 来源工具替身与渠道替身", func(t *testing.T) {
		e.InstallToolStub()
		e.SetToolCase("mail-1.1.0")
		out, code := e.RunTool("himalaya", "envelope", "list", "--page-size", "50", "--page", "1", "--output", "json")
		want, err := os.ReadFile(filepath.Join(repoRoot(), "sources", "mail", "testdata", "1.1.0", "list.json"))
		if err != nil {
			t.Fatal(err)
		}
		if code != 0 || strings.TrimSpace(out) != strings.TrimSpace(string(want)) {
			t.Errorf("邮件 CLI 替身输出应与录制样本一致: exit=%d\n%s", code, out)
		}
		e.SetToolCase("mail-thread")
		if _, code := e.RunTool("himalaya", "envelope", "list", "--output", "json"); code != 0 {
			t.Errorf("case mail-thread 退出码 %d", code)
		}
		log := e.ToolLog()
		if len(log) != 2 || log[0].Tool != "himalaya" || log[0].Case != "mail-1.1.0" || log[1].Case != "mail-thread" {
			t.Errorf("TOOL_LOG 每次调用应新增一条: %+v", log)
		}
		// 来源程序用得上：重启后的内核进程 PATH 以 TOOL_STUB_BIN 开头。
		if !strings.HasPrefix(e.ServeEnvValue("PATH"), e.ToolBin+string(os.PathListSeparator)) {
			t.Errorf("内核的 PATH 应以 TOOL_STUB_BIN 开头: %s", e.ServeEnvValue("PATH"))
		}
		// 渠道：CHAN 领取投递，经随包 ext 发到机器人替身。
		bot := e.StartChannelExt()
		id := e.AskDecision("env-07 提醒")
		if !e.WaitFor(45*time.Second, func() bool {
			for _, m := range bot.Sent() {
				if strings.Contains(m.Text, "env-07 提醒") {
					return true
				}
			}
			return false
		}) {
			t.Fatalf("事项 %s 的决策没有经渠道 ext 到达机器人替身: %+v", id, bot.Sent())
		}
	})

	step("ACT-ENV-08 合成账本夹具与旧版本二进制", func(t *testing.T) {
		fx := e.LedgerFixture(2)
		for _, k := range []string{"schema_ledger", "tables", "event_max_seq", "event_payload_sha256", "payload_hash_method", "backlog",
			"structured_rule_hits", "attachable", "score_markers", "old_cascade_ignored", "rule_ignored", "user_ignored", "outside_window",
			"policy_versions", "rule_modes", "tokens", "web_password", "session_cookie", "frozen_commit", "migration_files"} {
			if _, ok := fx.Raw[k]; !ok {
				t.Errorf("manifest.json 缺少 %s", k)
			}
		}
		// 一次性副本：改副本不影响夹具。
		if err := os.WriteFile(filepath.Join(fx.Home, "scratch"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(fx.Source, "scratch")); err == nil {
			t.Error("夹具副本应与夹具目录隔离")
		}
		srv := fx.ServeV01(e.V01Bin())
		var h struct {
			Signals []struct {
				ID     string
				Count  int
				Detail json.RawMessage
			}
		}
		code, body := srv.Get("/v1/health", fx.M.Tokens["observer"])
		if code != 200 || json.Unmarshal(body, &h) != nil {
			t.Fatalf("旧版本二进制应能打开副本并给出 health: %d %s", code, body)
		}
		want := fx.M.Backlog.Health
		found := false
		for _, s := range h.Signals {
			if s.ID == "unjudged_backlog" {
				found = true
				var d map[string]any
				_ = json.Unmarshal(s.Detail, &d)
				num := func(k string) int { f, _ := d[k].(float64); return int(f) }
				if s.Count != want["count"] || num("expired") != want["expired"] || num("undecided") != want["undecided"] || num("model_unavailable") != want["model_unavailable"] {
					t.Errorf("health 积压 %+v 与 manifest %v 不一致", s, want)
				}
			}
		}
		if !found || want["count"] == 0 {
			t.Errorf("health 应有非空的 unjudged_backlog 信号: found=%v want=%v", found, want)
		}
	})

	step("ACT-ENV-09 接线两个模型端点替身", func(t *testing.T) {
		e.WireEndpoints()
		r := e.Web("GET", "/v1/settings/model-endpoints", "")
		var got struct {
			Judge    map[string]any `json:"judge_endpoint"`
			Generate map[string]any `json:"generate_endpoint"`
		}
		if r.Code != 200 || json.Unmarshal([]byte(r.Body), &got) != nil {
			t.Fatalf("读取端点设置: %d %s", r.Code, r.Body)
		}
		if got.Judge["url"] != e.Cls.URL() || got.Judge["protocol"] != "probability-judgment/1" || got.Judge["locality"] != "local" {
			t.Errorf("判断端点应指向 CLS_STUB: %v", got.Judge)
		}
		if got.Generate["url"] != e.ModelStub.URL() || got.Generate["locality"] != "local" {
			t.Errorf("生成端点应指向 MODEL_STUB: %v", got.Generate)
		}
		// 两个端点真能连通（`apa doctor` 尚未随内核交付，见交付说明）。
		if got := e.clsBatch(t, [][2]string{{"q1", "ACC-INV-HI"}}); got["q1"] != 0.91 {
			t.Errorf("CLS_STUB 不可达: %v", got)
		}
		if e.chatContent(t, e.genChat("ping")) == "" && len(e.ModelStub.Requests()) == 0 {
			t.Error("MODEL_STUB 不可达")
		}
	})

	step("ACT-ENV-10 测试时钟 TEST_CLOCK", func(t *testing.T) {
		task := func(title string, deadline time.Time) string {
			r := e.Apa(e.DEL, "", "item", "add", "--kind", "task", "--phase", "open", "--title", title, "--evidence", "owner: add "+title)
			var m struct {
				ID string `json:"id"`
			}
			if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &m) != nil || m.ID == "" {
				t.Fatalf("item add %s: %d %s %s", title, r.Code, r.Stdout, r.Stderr)
			}
			if p := e.Web("PATCH", "/v1/items/"+m.ID, `{"deadline":"`+deadline.UTC().Format(time.RFC3339)+`"}`); p.Code != 200 {
				t.Fatalf("设置到期时间: %d %s", p.Code, p.Body)
			}
			return m.ID
		}
		passed := func(id string) bool {
			r := e.Web("GET", "/v1/audit/events?object="+id, "")
			return r.Code == 200 && strings.Contains(r.Body, "item.deadline_passed")
		}
		a := task("tc-a 12 小时后到期", time.Now().Add(12*time.Hour))
		if passed(a) {
			t.Fatal("推进前不应到期")
		}
		e.TCAdvance(86400) // 不重启服务
		if !e.WaitFor(10*time.Second, func() bool { return passed(a) }) {
			t.Fatalf("推进一天后 10 秒内应触发到期（内核时钟应立即重算）\n%s", e.ServiceLog())
		}
		// 实现记的是 "test clock: offset advanced ... to=24h0m0s"；验收文档写的是 `test-clock offset` 与 86400，措辞不一致，见交付说明。
		if log := e.ServiceLog(); !strings.Contains(log, "offset advanced") || !strings.Contains(log, "24h0m0s") {
			t.Errorf("服务日志应记一行推进: %s", log)
		}
		// 只进不退：写回更小的值被忽略并记一行，时间仍停在 +1 天。
		e.TCSet(0)
		b := task("tc-b 真实时间 1 小时后到期", time.Now().Add(time.Hour))
		if !e.WaitFor(10*time.Second, func() bool { return passed(b) }) {
			t.Fatalf("偏移量被调小了：+1 天下 1 小时后到期的事项应已到期\n%s", e.ServiceLog())
		}
		if !strings.Contains(e.ServiceLog(), "offset ignored") {
			t.Errorf("调小偏移量应记一行拒绝: %s", e.ServiceLog())
		}
		e.TCSet(86400) // 实例停在 T0+1d
		if raw, _ := os.ReadFile(filepath.Join(e.Home, "test-clock-offset")); strings.TrimSpace(string(raw)) != "86400" {
			t.Errorf("偏移量文件 = %q", raw)
		}
	})
}

// 两个环境互相隔离：不同 APA_HOME、不同端口、不同凭据；一个停止不影响另一个。
func TestEnvironmentsAreIsolated(t *testing.T) {
	a, b := Setup(t), Setup(t)
	if a.Home == b.Home || a.URL == b.URL || a.OBS == b.OBS {
		t.Fatal("两个环境共用了目录、地址或凭据")
	}
	if r := b.Apa(a.OBS, "", "inbox"); r.Code != 3 {
		t.Errorf("A 的凭据不应在 B 的内核上有效，实际退出码 %d", r.Code)
	}
	a.StopKernel()
	if r := b.Apa(b.OBS, "", "inbox"); r.Code != 0 {
		t.Errorf("停掉 A 不应影响 B: %d %s", r.Code, r.Stderr)
	}
	if r := a.Apa(a.OBS, "", "inbox"); r.Code != 7 {
		t.Errorf("内核不可达应是退出码 7，实际 %d", r.Code)
	}
}

// 重新启动内核后，已建立的环境数据仍在（后续流程可以停启内核）。
func TestSetupSurvivesKernelRestart(t *testing.T) {
	e := Setup(t)
	e.StopKernel()
	e.StartKernelAgain()
	if r := e.Apa(e.WRK, "", "executors"); r.Code != 0 || !strings.Contains(r.Stdout, "work-host.cli") {
		t.Fatalf("重启后环境数据丢失: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
}
