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

	// 2026-11-20 改写（权限模型第二版，§14.3 原则一）：会话身份（宿主名、会话引用）只进审计作展示，不参与判定；
	// 会话类型、本人原话与授权单不再上报，也不入账。
	f.step("ACT-HCTX-09 会话身份透传到审计，会话类型不再上报", func(t *testing.T) {
		plugin := map[string]string{"APA_HARNESS": "acc-stub", "APA_SESSION": "hctx-s9", "APA_SESSION_KIND": "direct"}
		proposed := idOf(t, f.apaEnv(f.DEL, 0, plugin, "propose", "--kind", "task", "--title", "hctx9", "--project", f.Projects["acc-open"]).Stdout)
		dec := f.decision("acc-open", "hctx9-decision")
		f.apaEnv(f.DEL, 0, plugin, "decide", dec, "--option", "A")

		for _, a := range []actor{f.stepActor(proposed, "item.created"), f.stepActor(dec, "decision.resolved")} {
			if a.Harness != "acc-stub" || a.Session != "hctx-s9" {
				t.Errorf("Event 的 actor 应含 harness=acc-stub 与 session=hctx-s9: %+v", a)
			}
		}
		if a := f.stepActor(dec, "decision.resolved"); a.SessionKind != "" || a.OnBehalfOf != "" {
			t.Errorf("Agent 的回答记成它自己做的，不记会话类型与代本人: %+v", a)
		}
		pytest(t, "plugins/hermes/tests/tools/test_tools.py",
			"test_no_tool_offers_evidence_grant_session_kind_or_timeout_default", "test_decide_carries_hermes_session_and_never_evidence",
			"test_cli_calls_carry_harness_hermes", "test_plugin_does_not_report_a_session_kind")
		pytest(t, "plugins/hermes/tests/core/test_core.py", "test_hook_calls_carry_session_identity")
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

	f.step("ACT-HCTX-14 插件与内核同批：自检正常，经插件包装的代答端到端可用", func(t *testing.T) {
		selfcheck := filepath.Join(repoRoot(), "plugins", "hermes", "headless", "selfcheck.py")
		if out, code := py(t, selfcheck); code != 0 || !json.Valid([]byte(out)) {
			t.Fatalf("自检应输出 JSON（退出码 %d）: %s", code, out)
		}
		f.declareSelfcheck("acc-stub", []string{"python3", selfcheck})
		if st := f.hostPluginStatus("acc-stub"); st != "ok" {
			t.Errorf("doctor 的宿主插件项应正常，实际 %s", st)
		}
		// 经插件包的 CLI 包装（终端会话替身）发起代答：DEL 所属 Agent 的档位已放开。
		dec := f.decision("acc-open", "hctx14-decision")
		calls := f.pluginWrapper(filepath.Join(repoRoot(), "plugins", "hermes"), "hctx-s14", f.DEL, "decide", dec, "--option", "A")
		if calls.ExitCode != 0 {
			t.Fatalf("经包装工具的代答应退出码 0: %+v", calls)
		}
		if a := f.stepActor(dec, "decision.resolved"); a.Session != "hctx-s14" || a.Harness != "hermes" || a.SessionKind != "" {
			t.Errorf("Event 应记 harness=hermes、session=hctx-s14，不记会话类型: %+v", a)
		}
		if env := f.wrapperEnvLog(); strings.Contains(env, "APA_SESSION_KIND=") || !strings.Contains(env, "APA_HARNESS=hermes") || !strings.Contains(env, "APA_SESSION=hctx-s14") {
			t.Errorf("宿主上下文替身记录到的 apa 调用环境不对: %s", env)
		}
	})

	// 旧插件仍会带会话类型等自报内容：内核一律忽略，结果与新插件相同（§17.4）。
	f.step("ACT-HCTX-15 只升内核不升插件：自报内容被忽略，代答、读取与回执照常", func(t *testing.T) {
		old := f.extractV01Plugin()
		dec := f.decision("acc-open", "hctx15-decision")
		parent := f.add("acc-work", "hctx15-parent", "open")
		child := idOf(t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli").Stdout)

		if r := f.pluginWrapper(old, "hctx-s15", f.DEL, "decide", dec, "--option", "A"); r.ExitCode != 0 {
			t.Errorf("旧插件经档位放开的 Agent 代答应成功: %+v", r)
		}
		if r := f.pluginWrapper(old, "hctx-s15", f.DEL, "context", "--format", "inject"); r.ExitCode != 0 {
			t.Errorf("旧插件的读取应不受影响: %+v", r)
		}
		if r := f.pluginWrapper(old, "hctx-s15", f.WRK, "report", child, "--outcome", "progress", "--summary", "x"); r.ExitCode != 0 {
			t.Errorf("旧插件对绑定子事项的回执应不受影响: %+v", r)
		}
		if a := f.stepActor(dec, "decision.resolved"); a.SessionKind != "" || a.OnBehalfOf != "" {
			t.Errorf("旧插件自报的会话类型不应入账: %+v", a)
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

		// 超过无人领取时限：带进过对话的投递收尾，不再带给之后的会话，也不进 Web 未读。
		if !f.WaitFor(90*time.Second, func() bool { return !strings.Contains(peek(), line) }) {
			t.Fatalf("注入后超过无人领取时限，投递仍在等下一次对话:\n%s", peek())
		}
		var web struct {
			Deliveries []struct {
				Item string `json:"item"`
			} `json:"deliveries"`
		}
		if err := json.Unmarshal([]byte(f.webOK("GET", "/deliveries/unread", "", 200)), &web); err != nil {
			t.Fatal(err)
		}
		for _, d := range web.Deliveries {
			if d.Item == id {
				t.Errorf("注入过的投递不应进 Web 未读: %+v", web.Deliveries)
			}
		}
	})
}
