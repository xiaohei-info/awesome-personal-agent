//go:build e2e

package screening

import (
	"strings"
	"testing"
	"time"
)

// TestFlowScreening 对应验收文档 flow-screening（ACT-SCR-01..13）。子测试按验收顺序共用一套环境。
//
// ACT-SCR-05（运行凭据在初筛中仍返回 4）见 TestACTSCR05RunCredential：当前 apa serve 没有登记
// start_background_run 动作，真实二进制拿不到运行凭据，该测试因此显式跳过并说明原因。
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
		for len(f.ModelStub.Requests()) == 0 && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		if n := len(f.ModelStub.Requests()); n != 1 {
			t.Errorf("MODEL_STUB 应收到 1 次初筛请求，实际 %d", n)
		}
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
		if n := len(f.ModelStub.Requests()); n != 0 {
			t.Errorf("规则已判 web_only 时不应初筛，MODEL_STUB 收到 %d 次请求", n)
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
		if n := len(f.ModelStub.Requests()); n != 1 {
			t.Errorf("失败后不应重试，MODEL_STUB 收到 %d 次请求", n)
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
		if n := len(f.ModelStub.Requests()); n != 1 {
			t.Errorf("超时后不应重试，MODEL_STUB 收到 %d 次请求", n)
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
		f.webOK("PUT", "/settings/model-endpoints", `{"model_endpoint":{"url":"","model":"","api_key":"","screening_timeout_ms":30000}}`, 200)
		h := f.task("scr-h")
		f.ask(h)
		eq(t, "screening_pending", f.pending(h), false)
		time.Sleep(time.Second)
		if n := len(f.ModelStub.Requests()); n != 0 {
			t.Errorf("未配置端点不应发出请求: %d", n)
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
		n := len(f.ModelStub.Requests())
		time.Sleep(2 * time.Second)
		if got := len(f.ModelStub.Requests()); got != n {
			t.Errorf("重启后不应重试初筛: %d → %d", n, got)
		}
		eq(t, "阶段", f.phase(i), "needs_input")
	})

	t.Run("ACT-SCR-13 初筛未结束时超时默认值不生效", func(t *testing.T) {
		f.t = t
		// 返回不升级：deadline 到达时不决定，初筛结束后按默认值决定。
		f.stub(okNormal, 12*time.Second, 0)
		j := f.task("scr-j")
		f.ask(j, "--deadline", time.Now().Add(6*time.Second).UTC().Format(time.RFC3339), "--default-on-timeout", "A")
		time.Sleep(9 * time.Second) // 已过 deadline，初筛仍未返回
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

		// 返回升级：默认值被清除且不决定。
		f.stub(okEscalate, 12*time.Second, 0)
		k := f.task("scr-k")
		f.ask(k, "--deadline", time.Now().Add(6*time.Second).UTC().Format(time.RFC3339), "--default-on-timeout", "A")
		f.waitCleared(k, 25*time.Second)
		time.Sleep(5 * time.Second)
		eq(t, "risk_class", f.askOf(k)["risk_class"], "web_only")
		if f.resolution(k) != nil {
			t.Error("升级后不应按默认值决定")
		}
		eq(t, "阶段", f.phase(k), "needs_input")
	})
}

// TestACTSCR05RunCredential 对应 ACT-SCR-05：初筛中由后台运行的运行凭据运行 `apa decide`，应返回 4。
// 运行凭据只在内核拉起后台运行时签发（POST /runs/{id}/credential 或运行环境）。当前 apa serve 只登记了
// call_agent，start_background_run 没有处理器（规则触发后记 health.alert "has no registered handler"），
// 所以真实二进制里没有任何办法拿到运行凭据。这是 kernel/background 未装配的实现缺陷，不在本节点范围，
// 因此让本测试失败（fail-closed）而不是跳过或用替身伪造运行凭据；装配完成后删除下面的 Fatal 并补全断言。
func TestACTSCR05RunCredential(t *testing.T) {
	f := setup(t)
	f.webOK("PUT", "/settings/policy", `{"default_harness":"acc-stub"}`, 200)
	f.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"edit","by_harness":{}}}`, 200)
	f.webOK("PATCH", "/rules/background-run%2Fevent_rules%2Fhandle-labelled/mode", `{"mode":"auto"}`, 200)
	out := f.apa(f.OBS, 0, "propose", "--kind", "task", "--title", "bg", "--project", f.Projects["acc-open"], "--tags", "agent-ok").Stdout
	id := out[strings.Index(out, "itm_"):][:20]
	f.webOK("POST", "/items/"+id+"/accept", `{}`, 200)
	time.Sleep(2 * time.Second)
	if runs := f.apa(f.OBS, 0, "run", "list").Stdout; strings.Contains(runs, id) {
		t.Fatalf("后台运行已能拉起，请补全 ACT-SCR-05 断言: %s", runs)
	}
	t.Fatal("ACT-SCR-05 无法验证：apa serve 未登记 start_background_run，真实二进制拿不到运行凭据；上游装配修复前本 gate 保持红色（见函数注释）")
}
