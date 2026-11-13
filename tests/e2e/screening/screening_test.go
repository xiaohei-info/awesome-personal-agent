//go:build e2e

package screening

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlowScreening 对应验收文档 flow-screening（ACT-SCR-01..13）。子测试按验收顺序共用一套环境。
//
// ACT-SCR-05（运行凭据在初筛中仍返回 4）见 TestACTSCR05RunCredential：当前 apa serve 没有登记
// start_background_run 动作，真实二进制拿不到运行凭据，该测试因此失败（fail-closed）并说明原因。
func TestFlowScreening(t *testing.T) {
	f := setup(t)
	var a string

	t.Run("ACT-SCR-01 配置初筛环境并发出初筛", func(t *testing.T) {
		f.t = t
		f.stub(okNormal, slow, 0)
		a = f.task("scr-a")
		f.ask(a, "--deadline", "2099-01-01T00:00:00Z", "--default-on-timeout", "A")
		eq(t, "Web 显示初筛中", f.pending(a), true)
		eq(t, "阶段", f.phase(a), "needs_input")
		deadline := time.Now().Add(5 * time.Second)
		for f.screeningRequests() == 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if n := f.screeningRequests(); n != 1 {
			t.Errorf("CLS_LOG 应有 1 次 web-only-screening 请求，实际 %d", n)
		}
		eq(t, "初筛超时配置", f.webSetting("screening_timeout_ms"), float64(timeoutMs))
	})

	t.Run("ACT-SCR-02 初筛中只在 Web 可见", func(t *testing.T) {
		f.t = t
		unread := f.webOK("GET", "/deliveries/unread", "", 200)
		if !strings.Contains(f.askOf(a)["question"].(string), "选哪个方案") {
			t.Errorf("Web 应看得到完整决策请求: %v", f.askOf(a))
		}
		listed := f.webOK("GET", "/items?phase=needs_input", "", 200)
		if !strings.Contains(listed, a) || !strings.Contains(listed, `"screening_pending":true`) {
			t.Errorf("Web 决策待办应有带初筛中标记的卡片: %s", listed)
		}
		if inbox := f.apa(f.OBS, 0, "inbox").Stdout; strings.Contains(inbox, a) {
			t.Errorf("apa inbox 泄露了初筛中的决策: %s", inbox)
		}
		ctx := f.apa(f.DEL, 0, "context", "--session", "scr", "--scope", "project", "--project", f.Projects["acc-work"], "--format", "inject").Stdout
		if strings.Contains(ctx, a) || strings.Contains(ctx, "选哪个方案") {
			t.Errorf("apa context 泄露了初筛中的决策: %s", ctx)
		}
		if code, body := f.claim("1"); code != 204 {
			t.Errorf("CHAN 不应领到投递: %d %s", code, body)
		}
		if got := f.webOK("GET", "/deliveries/unread", "", 200); got != unread {
			t.Errorf("Web 未读投递不应变化: %s → %s", unread, got)
		}
	})

	t.Run("ACT-SCR-03 其他凭据读取只见标记", func(t *testing.T) {
		f.t = t
		for _, args := range [][]string{{"item", "show", a}, {"why", a}} {
			out := f.apa(f.DEL, 0, args...).Stdout
			if !strings.Contains(out, `"screening_pending":true`) {
				t.Errorf("apa %v 应带初筛中标记: %s", args, out)
			}
			for _, leak := range []string{"选哪个方案", "方案A", "方案B"} {
				if strings.Contains(out, leak) {
					t.Errorf("apa %v 泄露了 %q: %s", args, leak, out)
				}
			}
		}
	})

	t.Run("ACT-SCR-04 初筛中代答返回 22", func(t *testing.T) {
		f.t = t
		r := f.apa(f.DEL, 22, "decide", a, "--option", "A", "--evidence", "选 A")
		for _, want := range []string{`"code":"risk_screening_pending"`, `"why"`, `"web_url"`, "/items/" + a} {
			if !strings.Contains(r.Stderr, want) {
				t.Errorf("退出码 22 的输出缺 %s: %s", want, r.Stderr)
			}
		}
		eq(t, "阶段", f.phase(a), "needs_input")
		if f.resolution(a) != nil {
			t.Error("决策不应生效")
		}
		rejected := false
		for _, e := range f.events(a, "command.rejected") {
			rejected = rejected || e.Payload["exit"] == float64(22)
		}
		if !rejected {
			t.Error("没有退出码 22 的拒绝记录")
		}
	})

	t.Run("ACT-SCR-05 初筛中运行凭据仍返回 4", func(t *testing.T) {
		f.t = t
		decl, _ := json.Marshal(map[string]any{"harness": "acc-hang", "headless": map[string]any{
			"command": []string{"env", "STUB_HARNESS_MODE=hang", "STUB_HARNESS_HANG_FOR=3m", f.Bin["stub-harness"], "{prompt}"},
			"session": map[string]any{"extract": "$.session_id"},
			"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}}}})
		if r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "声明挂住的 harness 以取得运行凭据"); r.Code != 0 {
			t.Fatalf("apa declare acc-hang: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		f.webOK("POST", "/settings/declarations/acc-hang/1/confirm", "", 200)
		f.webOK("PUT", "/settings/policy", `{"default_harness":"acc-hang"}`, 200)
		f.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"edit","by_harness":{}}}`, 200)
		f.webOK("PATCH", "/rules/background-run%2Fevent_rules%2Fhandle-labelled/mode", `{"mode":"auto"}`, 200)
		out := f.apa(f.OBS, 0, "propose", "--kind", "task", "--title", "bg", "--project", f.Projects["acc-open"], "--tags", "agent-ok").Stdout
		f.webOK("POST", "/items/"+out[strings.Index(out, "itm_"):][:20]+"/accept", `{}`, 200)
		var token string
		stop := time.Now().Add(20 * time.Second)
		for token == "" && time.Now().Before(stop) {
			envs, _ := filepath.Glob(filepath.Join(f.Home, "runs", "*", "stub-harness-env.json"))
			for _, p := range envs {
				var e map[string]string
				if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &e) == nil {
					token = e["APA_TOKEN"]
					if token == "" && e["APA_RUN_CREDENTIAL_FILE"] != "" {
						b, _ := os.ReadFile(e["APA_RUN_CREDENTIAL_FILE"])
						token = strings.TrimSpace(string(b))
					}
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		if token == "" {
			t.Fatal("后台运行没有拿到运行凭据")
		}
		eq(t, "前置：事项仍在初筛中", f.pending(a), true)
		f.apa(token, 4, "decide", a, "--option", "A", "--evidence", "x")
		if f.resolution(a) != nil {
			t.Error("运行凭据的决策不应生效")
		}
	})

	t.Run("ACT-SCR-06 初筛中本人可以直接回答，结果只记录", func(t *testing.T) {
		f.t = t
		start := time.Now()
		f.webOK("POST", "/items/"+a+"/decide", `{"choice":"B"}`, 200)
		if d := time.Since(start); d > slow/2 {
			t.Errorf("Web 回答被初筛阻塞了 %v", d)
		}
		eq(t, "回答立即生效", f.resolution(a)["choice"], "B")
		f.waitCleared(a, 3*slow)
		eq(t, "初筛返回后的回答", f.resolution(a)["choice"], "B")
		eq(t, "resolved_via", f.resolution(a)["resolved_via"], "web")
		if why := f.apa(f.DEL, 0, "why", a).Stdout; !strings.Contains(why, "already answered") {
			t.Errorf("apa why 应记录初筛只记结果: %s", why)
		}
	})

	t.Run("ACT-SCR-07 初筛未升级则正常投递，代答恢复可用", func(t *testing.T) {
		f.t = t
		f.stub(okNormal, 3*time.Second, 0)
		b := f.task("scr-b")
		f.ask(b)
		if code, body := f.claim("1"); code != 204 {
			t.Fatalf("初筛返回前不应有投递: %d %s", code, body)
		}
		f.waitCleared(b, 3*slow)
		if !f.hasEvent(b, "item.updated", "did not escalate") {
			t.Error("没有记录初筛结果的 Event")
		}
		// 初筛结果由内核直接写回，投递在触达的下一次周期 Tick（默认 30 秒）里产出，所以长轮询等一个 Tick 周期。
		code, body := f.claim("40")
		if code != 200 || !strings.Contains(body, b) || !strings.Contains(body, `"class":"decision"`) {
			t.Errorf("初筛结束后应按策略投递: %d %s", code, body)
		}
		if !strings.Contains(f.apa(f.OBS, 0, "inbox").Stdout, b) {
			t.Error("apa inbox 应列出已初筛完的决策")
		}
		f.apa(f.DEL, 0, "decide", b, "--option", "A", "--evidence", "选 A")
		eq(t, "代答生效", f.resolution(b)["choice"], "A")
	})

	t.Run("ACT-SCR-08 初筛升级为 web_only", func(t *testing.T) {
		f.t = t
		f.stub(okEscalate, 2*time.Second, 0)
		c := f.task("scr-c")
		f.ask(c, "--deadline", "2099-01-01T00:00:00Z", "--default-on-timeout", "A")
		f.waitCleared(c, 3*slow)
		eq(t, "risk_class", f.askOf(c)["risk_class"], "web_only")
		if v := f.askOf(c)["default_on_timeout"]; v != nil && v != "" {
			t.Errorf("超时默认值应被清除: %v", v)
		}
		r := f.apa(f.DEL, 20, "decide", c, "--option", "A", "--evidence", "选 A")
		if !strings.Contains(r.Stderr, `"code":"web_only"`) {
			t.Errorf("退出码 20 的 code 应为 web_only: %s", r.Stderr)
		}
		eq(t, "阶段", f.phase(c), "needs_input")
	})

	t.Run("ACT-SCR-09 初筛不能降级", func(t *testing.T) {
		f.t = t
		f.stub(okNormal, 0, 0)
		d := f.task("scr-d", "acc-finance")
		f.ask(d)
		time.Sleep(2 * time.Second)
		if n, m := f.screeningRequests(), f.chatScreeningRequests(); n != 0 || m != 0 {
			t.Errorf("规则已判 web_only 时不应初筛，CLS_LOG %d 次、MODEL_LOG %d 次", n, m)
		}
		eq(t, "screening_pending", f.pending(d), false)
		eq(t, "risk_class", f.askOf(d)["risk_class"], "funds")
		f.apa(f.DEL, 20, "decide", d, "--option", "A", "--evidence", "选 A")
	})

	t.Run("ACT-SCR-10 初筛失败或超时按规则继续", func(t *testing.T) {
		f.t = t
		f.stub("", 0, 500)
		e := f.task("scr-e")
		f.ask(e)
		f.waitCleared(e, 10*time.Second)
		time.Sleep(2 * time.Second)
		if n := f.screeningRequests(); n != 1 || f.chatScreeningRequests() != 0 {
			t.Errorf("失败后不应重试也不交下一级，CLS_LOG %d 次、MODEL_LOG %d 次", n, f.chatScreeningRequests())
		}
		if !f.hasEvent(e, "item.updated", "screening failed") || !f.hasEvent(e, "health.alert", "did not complete") {
			t.Error("Event 没有记录初筛失败原因")
		}
		if !strings.Contains(f.health(), e) {
			t.Errorf("健康视图应有初筛失败信号: %s", f.health())
		}
		f.apa(f.DEL, 0, "decide", e, "--option", "A", "--evidence", "选 A") // 按规则继续：非高风险，代答可用

		f.endpoint(3000) // 初筛超时 3 秒（对应"5 秒"）
		f.stub(okNormal, 3*slow, 0)
		g := f.task("scr-g")
		f.ask(g)
		f.waitCleared(g, 10*time.Second)
		time.Sleep(2 * time.Second)
		if n := f.screeningRequests(); n != 1 || f.chatScreeningRequests() != 0 {
			t.Errorf("超时后不应重试也不交下一级，CLS_LOG %d 次、MODEL_LOG %d 次", n, f.chatScreeningRequests())
		}
		if !f.hasEvent(g, "item.updated", "timed out") || !strings.Contains(f.health(), g) {
			t.Errorf("超时没有 Event 或健康信号: %s", f.health())
		}
		eq(t, "阶段", f.phase(g), "needs_input")
		f.endpoint(timeoutMs)
	})

	t.Run("ACT-SCR-11 未配置模型端点时不做初筛", func(t *testing.T) {
		f.t = t
		f.stub(okEscalate, 0, 0)
		f.webOK("PUT", "/settings/model-endpoints", `{"judge_endpoint":null,"generate_endpoint":null,"screening_timeout_ms":30000}`, 200)
		h := f.task("scr-h")
		f.ask(h)
		eq(t, "screening_pending", f.pending(h), false)
		time.Sleep(time.Second)
		if n, m := f.screeningRequests(), f.chatScreeningRequests(); n != 0 || m != 0 {
			t.Errorf("未配置端点不应发出请求: CLS_LOG %d、MODEL_LOG %d", n, m)
		}
		if !strings.Contains(f.apa(f.OBS, 0, "inbox").Stdout, h) {
			t.Error("应按规则立刻进入决策流")
		}
		f.endpoint(timeoutMs)
	})

	t.Run("ACT-SCR-12 重启中断初筛时继续", func(t *testing.T) {
		f.t = t
		f.stub(okNormal, 10*slow, 0)
		i := f.task("scr-i")
		f.ask(i)
		eq(t, "重启前", f.pending(i), true)
		f.kill9()
		f.StartKernelAgain()
		f.waitCleared(i, 10*time.Second)
		if !f.hasEvent(i, "item.updated", "interrupted by restart") || !strings.Contains(f.health(), i) {
			t.Errorf("没有重启中断的 Event 或健康信号: %s", f.health())
		}
		n := f.screeningRequests()
		time.Sleep(2 * time.Second)
		if got := f.screeningRequests(); got != n {
			t.Errorf("重启后不应重试初筛: %d → %d", n, got)
		}
		eq(t, "阶段", f.phase(i), "needs_input")
	})

	t.Run("ACT-SCR-13 初筛未结束时超时默认值不生效", func(t *testing.T) {
		f.t = t
		// 返回不升级：deadline 到达时不决定，初筛结束后按默认值决定。
		f.stub(okNormal, 8*time.Second, 0)
		j := f.task("scr-j")
		f.ask(j, "--deadline", time.Now().Add(3*time.Second).UTC().Format(time.RFC3339), "--default-on-timeout", "A")
		time.Sleep(5 * time.Second) // 已过 deadline，初筛仍未返回
		eq(t, "deadline 到达时仍在初筛", f.pending(j), true)
		if f.resolution(j) != nil {
			t.Fatal("抢在初筛前决定了")
		}
		f.waitCleared(j, 20*time.Second)
		stop := time.Now().Add(30 * time.Second)
		for f.resolution(j) == nil && time.Now().Before(stop) {
			time.Sleep(500 * time.Millisecond)
		}
		r := f.resolution(j)
		if r == nil {
			t.Fatal("初筛结束后没有按默认值决定")
		}
		eq(t, "choice", r["choice"], "A")
		eq(t, "resolved_via", r["resolved_via"], "timeout")
		if !f.hasEvent(j, "item.updated", "did not escalate") { // 必须是分类端点真的答了"不升级"，不是失败后继续
			t.Error("没有记录初筛结果的 Event")
		}

		// 返回升级：默认值被清除且不决定。
		f.stub(okEscalate, 8*time.Second, 0)
		k := f.task("scr-k")
		f.ask(k, "--deadline", time.Now().Add(3*time.Second).UTC().Format(time.RFC3339), "--default-on-timeout", "A")
		f.waitCleared(k, 25*time.Second)
		time.Sleep(5 * time.Second)
		eq(t, "risk_class", f.askOf(k)["risk_class"], "web_only")
		if f.resolution(k) != nil {
			t.Error("升级后不应按默认值决定")
		}
		eq(t, "阶段", f.phase(k), "needs_input")
	})
}
