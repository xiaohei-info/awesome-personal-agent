//go:build e2e

package hostplugin

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// newKernel 建一套环境；extra 追加到 config.yaml。
func newKernel(t *testing.T, extra string) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	e.ServeWith(extra)
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.DeclareStubHarness()
	return &fx{e, t}
}

func contains(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			t.Errorf("%s 缺少 %q:\n%s", what, sub, s)
		}
	}
}

func lacks(t *testing.T, what, s string, subs ...string) {
	t.Helper()
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			t.Errorf("%s 不应含 %q:\n%s", what, sub, s)
		}
	}
}

// actor 是 apa why 步骤里的行为人（只列验收关心的字段）。
type actor struct {
	Harness     string `json:"harness"`
	Session     string `json:"session"`
	SessionKind string `json:"session_kind"`
	OnBehalfOf  string `json:"on_behalf_of"`
}

// stepActor 返回 apa why <id> 里第一个类型为 typ 的步骤的行为人。
func (f *fx) stepActor(id, typ string) actor {
	f.t.Helper()
	var out struct {
		Steps []struct {
			Type  string `json:"type"`
			Actor actor  `json:"actor"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "why", id).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	for _, s := range out.Steps {
		if s.Type == typ {
			return s.Actor
		}
	}
	f.t.Fatalf("事项 %s 的 apa why 里没有 %s 步骤", id, typ)
	return actor{}
}

// TestFlowHermesContext 对应验收文档 flow-hermes-context（替身与插件契约测试版，ACT-HCTX-01、05、09、12..16）：
// 真实 apa 二进制 + STUB_HARNESS 扮演插件 + 宿主上下文替身驱动插件包的 CLI 包装 + Python 插件契约测试。
// 真实 Hermes 上的注入、钩子放行、恢复与压缩后重新注入沿用 v0.1 结果，由本机升级验证 L9 承接。
func TestFlowHermesContext(t *testing.T) {
	// 投递无人领取的时限调到 20 秒（默认 30 分钟），ACT-HCTX-16 要看到注入过的投递退回 Web 未读。
	f := newKernel(t, "delivery_unclaimed_after: 20s\ndelivery_backoff: 1s,2s\n")

	f.step("ACT-HCTX-01 准备插件契约测试环境", func(t *testing.T) {
		out, code := py(t, "-m", "pytest", "--version")
		if code != 0 {
			t.Fatalf("python3 -m pytest --version 退出码 %d: %s", code, out)
		}
	})

	f.step("ACT-HCTX-05 注入信封只带资料", func(t *testing.T) {
		const marker = "MARKER-BODY"
		id := f.add("acc-open", "hctx5-title", "open", "--body", marker)
		f.ask(id)
		other := f.decision("acc-work", "hctx5-other-project")

		out := f.apa(f.DEL, 0, "context", "--session", "s2", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		contains(t, "注入信封", out, "以下为资料", "账本是唯一真理源", id, "hctx5-title", "needs_input", "截止", "建议：apa decide "+id)
		lacks(t, "注入信封（不得带正文、不得混入其他项目）", out, marker, other)
	})

	f.step("ACT-HCTX-09 会话身份与会话类型透传到审计", func(t *testing.T) {
		plugin := map[string]string{"APA_HARNESS": "acc-stub", "APA_SESSION": "hctx-s9", "APA_SESSION_KIND": "direct"}
		proposed := idOf(t, f.apaEnv(f.DEL, 0, plugin, "propose", "--kind", "task", "--title", "hctx9", "--project", f.Projects["acc-open"]).Stdout)
		dec := f.decision("acc-open", "hctx9-decision")
		f.apaEnv(f.DEL, 0, plugin, "decide", dec, "--option", "A", "--evidence", "选 A")

		for _, a := range []actor{f.stepActor(proposed, "item.created"), f.stepActor(dec, "decision.resolved")} {
			if a.Harness != "acc-stub" || a.Session != "hctx-s9" {
				t.Errorf("Event 的 actor 应含 harness=acc-stub 与 session=hctx-s9: %+v", a)
			}
		}
		if a := f.stepActor(dec, "decision.resolved"); a.SessionKind != "direct" || a.OnBehalfOf != "owner" {
			t.Errorf("代答 Event 应记会话类型 direct 与 on_behalf_of=owner: %+v", a)
		}
		out := pytest(t, "plugins/hermes/tests/tools/test_tools.py",
			"test_every_tool_schema_has_no_session_kind_parameter", "test_decide_carries_hermes_session_and_evidence", "test_cli_calls_carry_harness_hermes")
		_ = out
		pytest(t, "plugins/hermes/tests/core/test_core.py", "test_hook_calls_carry_session_identity_and_kind")
	})

	f.step("ACT-HCTX-12 终端会话上报 direct", func(t *testing.T) {
		pytest(t, "plugins/hermes/tests/tools/test_tools.py", "test_terminal_session_calls_carry_session_kind_direct")
		pytest(t, "plugins/hermes/tests/core/test_core.py", "test_hook_calls_carry_session_identity_and_kind")
	})

	f.step("ACT-HCTX-13 网关会话按 SPIKE-SESSION-KIND 的取法上报，取不到报 unknown", func(t *testing.T) {
		// 附录 A.5 的 SPIKE-SESSION-KIND 行已给出取法（不再是"待实测"），所以用例须按平台判定。
		spike := ""
		doc, err := os.ReadFile(filepath.Join(repoRoot(), "docs", "appendix-a-harness.md"))
		if err != nil {
			t.Fatal(err)
		}
		for _, line := range strings.Split(string(doc), "\n") {
			if strings.HasPrefix(line, "| SPIKE-SESSION-KIND |") {
				spike = line
			}
		}
		if spike == "" || strings.Contains(spike, "| 待实测 |") {
			t.Fatalf("附录 A.5 的 SPIKE-SESSION-KIND 行缺失或仍是待实测，网关会话只能报 unknown，此处断言需要随结论更新: %q", spike)
		}
		contains(t, "A.5 SPIKE-SESSION-KIND", spike, "direct", "group", "unknown")
		out := pytest(t, "plugins/hermes/tests/tools/test_tools.py",
			"test_unreadable_gateway_binding_is_unknown", "test_no_hermes_session_module_is_unknown",
			"test_session_kind_is_not_inherited_from_the_gateway_environment", "test_every_tool_schema_has_no_session_kind_parameter")
		for _, kind := range []string{"direct", "group", "unknown"} { // 网关会话类型用例：direct、group、unknown 各有
			contains(t, "网关会话类型用例", out, "test_gateway_session_kind_follows_the_platform[", "-"+kind+"] PASSED")
		}
		pytest(t, "plugins/hermes/tests/wake/test_wake.py", "test_wake_cli_does_not_report_or_inherit_a_session_kind")
		// 原生工具的参数 schema 里没有模型可填的会话类型参数。
		for _, name := range []string{"cli.py", "__init__.py"} {
			src, err := os.ReadFile(filepath.Join(repoRoot(), "plugins", "hermes", "tools", name))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(strings.ToLower(string(src)), `"session_kind"`) {
				t.Errorf("plugins/hermes/tools/%s 的工具参数里出现了 session_kind", name)
			}
		}
	})

	f.step("ACT-HCTX-14 插件与内核同批：自检上报会话类型，凭原话的代答端到端可用", func(t *testing.T) {
		selfcheck := filepath.Join(repoRoot(), "plugins", "hermes", "headless", "selfcheck.py")
		out, code := py(t, selfcheck)
		var rep struct {
			ReportsSessionKind bool `json:"reports_session_kind"`
		}
		if code != 0 || json.Unmarshal([]byte(out), &rep) != nil || !rep.ReportsSessionKind {
			t.Fatalf("自检 JSON 应含 reports_session_kind:true（退出码 %d）: %s", code, out)
		}
		f.declareSelfcheck("acc-stub", []string{"python3", selfcheck})
		if st := f.hostPluginStatus("acc-stub"); st != "ok" {
			t.Errorf("doctor 的宿主插件项应正常，实际 %s", st)
		}

		// 经插件包的 CLI 包装（终端会话替身）发起凭原话的代答。
		dec := f.decision("acc-open", "hctx14-decision")
		calls := f.pluginWrapper(filepath.Join(repoRoot(), "plugins", "hermes"), "hctx-s14", f.DEL, "decide", dec, "--option", "A", "--evidence", "选 A")
		if calls.ExitCode != 0 {
			t.Fatalf("经包装工具的代答应退出码 0: %+v", calls)
		}
		if a := f.stepActor(dec, "decision.resolved"); a.SessionKind != "direct" || a.Session != "hctx-s14" || a.Harness != "hermes" {
			t.Errorf("Event 应记 harness=hermes、session=hctx-s14、会话类型 direct: %+v", a)
		}
		if env := f.wrapperEnvLog(); !strings.Contains(env, "APA_SESSION_KIND=direct") || !strings.Contains(env, "APA_HARNESS=hermes") || !strings.Contains(env, "APA_SESSION=hctx-s14") {
			t.Errorf("宿主上下文替身记录到的 apa 调用环境不对: %s", env)
		}
	})

	f.step("ACT-HCTX-15 只升内核不升插件：凭原话的代答被拒，读取与回执不受影响", func(t *testing.T) {
		old := f.extractV01Plugin()
		dec := f.decision("acc-open", "hctx15-decision")
		parent := f.add("acc-work", "hctx15-parent", "open")
		child := idOf(t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--evidence", "把它交给 work-host.cli").Stdout)

		if r := f.pluginWrapper(old, "hctx-s15", f.DEL, "decide", dec, "--option", "A", "--evidence", "选 A"); r.ExitCode != 23 || !strings.Contains(string(r.Stderr), "session_not_direct") {
			t.Errorf("旧插件的代答应被拒（退出码 23、session_not_direct）: %+v", r)
		}
		if env := f.wrapperEnvLog(); strings.Contains(env, "APA_SESSION_KIND=direct") {
			t.Errorf("v0.1 插件不应上报会话类型，调用环境: %s", env)
		}
		if r := f.pluginWrapper(old, "hctx-s15", f.DEL, "context", "--format", "inject"); r.ExitCode != 0 {
			t.Errorf("旧插件的读取应不受影响: %+v", r)
		}
		if r := f.pluginWrapper(old, "hctx-s15", f.WRK, "report", child, "--outcome", "progress", "--summary", "x"); r.ExitCode != 0 {
			t.Errorf("旧插件对绑定子事项的回执应不受影响: %+v", r)
		}
		if got := f.apa(f.OBS, 0, "item", "show", dec).Stdout; !strings.Contains(got, "needs_input") {
			t.Errorf("被拒的代答不应使决策生效，事项应仍在 needs_input: %s", got)
		}

		// 旧插件的自检不带 reports_session_kind：doctor 指出需要与内核一同升级。
		sc := filepath.Join(f.Root, "old-plugin-selfcheck.sh")
		if err := os.WriteFile(sc, []byte("#!/bin/sh\necho '{\"plugin_version\":\"0.1.1\",\"hooks\":[{\"name\":\"inject\",\"installed\":true}],\"overlaps\":[]}'\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		f.declareSelfcheck("acc-stub", []string{sc})
		why := f.hostPluginWhy("acc-stub")
		contains(t, "doctor 的宿主插件项", why, "does not report the session type", "upgrade the plugin together with the kernel")
		if st := f.hostPluginStatus("acc-stub"); st != "warning" {
			t.Errorf("旧插件的宿主插件项应为 warning，实际 %s", st)
		}
	})

	f.step("ACT-HCTX-16 钩子放行、注入不等于送达、常驻指令过期提示（插件契约测试 + 内核测试 + 实例）", func(t *testing.T) {
		pytest(t, "plugins/hermes/tests/inject/test_inject.py",
			"test_failures_let_the_message_through", "test_slow_kernel_is_abandoned_within_two_seconds", "test_stale_instruction_version_is_reported_to_kernel")
		pytest(t, "plugins/hermes/tests/receipt/test_receipt.py", "test_unreachable_kernel_keeps_the_request_and_resends_once_with_the_same_key")
		pytest(t, "plugins/hermes/tests/capture/test_capture.py")
		cmd := exec.Command("go", "test", "-count=1", "./kernel/context/", "-run", "TestInstructionVersion|TestInjectRoutedDeliveriesOncePerSession", "-v")
		cmd.Dir = repoRoot()
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go test ./kernel/context/ 失败: %v\n%s", err, out)
		}
		contains(t, "内核测试", string(out), "--- PASS: TestInstructionVersion", "--- PASS: TestInjectRoutedDeliveriesOncePerSession")

		// 实例（ACT-HCTX-06）：造一条路由为注入的投递，两个会话先后注入。
		f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"decision":"inject","reminder":"web","brief":"web","health":"web","wake":"web"}}}`, 200)
		id := f.decision("acc-open", "hctx16-inject")
		// 触达的周期 Tick（默认 30 秒）把投递写进队列；不带会话的只读查询不记录注入。
		line := "- 提醒 decision:" + id
		peek := func() string {
			return f.apa(f.OBS, 0, "context", "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		}
		if !f.WaitFor(60*time.Second, func() bool { return strings.Contains(peek(), line) }) {
			t.Fatalf("60 秒内这条路由为注入的投递没有出现在 apa context 里:\n%s", peek())
		}
		inject := func(session string) string {
			return f.apa(f.DEL, 0, "context", "--session", session, "--scope", "project", "--project", f.Projects["acc-open"], "--format", "inject").Stdout
		}
		contains(t, "会话 1 的注入", inject("acc-session-1"), line)
		contains(t, "会话 2 仍应带出这条投递（会话 1 注入不算确认）", inject("acc-session-2"), line)
		if again := inject("acc-session-1"); strings.Contains(again, line) {
			t.Errorf("同一会话不应重复注入:\n%s", again)
		}

		// 无人领取超时后退回 Web 未读：仍在未读列表里，记录了注入到哪些会话，状态不是 acknowledged。
		type dl struct {
			ID         string `json:"id"`
			Item       string `json:"item"`
			State      string `json:"state"`
			Injections []struct {
				Session string `json:"injected_to_session"`
			} `json:"injections"`
		}
		unread := func() (mine *dl) {
			var out struct {
				Deliveries []dl `json:"deliveries"`
			}
			if err := json.Unmarshal([]byte(f.webOK("GET", "/deliveries/unread", "", 200)), &out); err != nil {
				t.Fatal(err)
			}
			for i := range out.Deliveries {
				if out.Deliveries[i].Item == id {
					mine = &out.Deliveries[i]
				}
			}
			return
		}
		var mine *dl
		if !f.WaitFor(90*time.Second, func() bool { mine = unread(); return mine != nil }) {
			t.Fatal("注入后无人领取超限，投递应退回 Web 未读，实际没有出现在 GET /v1/deliveries/unread")
		}
		if mine.State == "acknowledged" {
			t.Errorf("注入后不应是 acknowledged: %+v", mine)
		}
		got := map[string]bool{}
		for _, in := range mine.Injections {
			got[in.Session] = true
		}
		if !got["acc-session-1"] || !got["acc-session-2"] {
			t.Errorf("投递应记录 injected_to_session 为 acc-session-1 与 acc-session-2: %+v", mine.Injections)
		}

		// 本人在 Web 点击后它才离开未读。
		f.webOK("POST", "/deliveries/"+mine.ID+"/read", "", 200)
		if still := unread(); still != nil {
			t.Errorf("Web 点击后投递应离开未读: %+v", still)
		}
	})
}
