//go:build e2e

package hostplugin

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlowWakeLease 对应验收文档 flow-wake-lease（替身版，ACT-WAK-01..09，复验 dfx-5410a4c832ad）：
// 真实 apa 二进制 + STUB_HARNESS（未声明唤醒时拉起的无头运行）+ HTTP 调用扮演网关插件（持 DEL）+ 机器人替身 + 测试时钟。
// 真实网关里注入回合、插件工具可调用的部分由本机升级验证 L9 承接。
func TestFlowWakeLease(t *testing.T) {
	f := newWakeKernel(t)
	bot := f.StartChannelExt() // 决策路由到渠道 acc-chan，机器人替身记录向"本人"发出的消息
	for _, r := range []string{headlessR, wakeR, shortR} {
		f.autoRule(r)
	}
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"decision":"acc-chan","reminder":"web","brief":"web","health":"web"},"merge_window_sec":2}}`, 200)

	// 租约期间 DEL 连带原话的建事项也被拒，所以各步要用的普通决策事先备好。
	var dec [5]string
	for i := range dec {
		dec[i] = f.decision("acc-open", fmt.Sprintf("WAK-D%d", i+1))
	}

	var i1, child, run1, cred1 string // WAK-I1 与它的唤醒运行、换出的运行凭据
	var headlessRun string

	f.step("ACT-WAK-01 显式声明唤醒并建主会话角色规则", func(t *testing.T) {
		// 未声明 main_session.launch：命中 runner=main_session 规则的 Run 记 launch=headless。
		h := f.tagged("wak01-headless", "acc-wake-headless")
		r := f.waitRun(h)
		if r.Launch != "headless" || r.Runner != "main_session" {
			t.Fatalf("未声明唤醒时 Run 应为 runner=main_session、launch=headless: %+v", r)
		}
		headlessRun = r.ID

		// WRK 提交 acc-stub-wake 的声明（main_session.launch: wake，其余同 acc-stub），确认前不走唤醒。
		bin := f.Bin["stub-harness"]
		decl, _ := json.Marshal(map[string]any{"harness": wakeHN,
			"headless": map[string]any{"command": []string{bin, "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
				"resume": []string{bin, "--resume", "{prompt}"},
				"access": map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}}},
			"main_session": map[string]any{"launch": "wake"}})
		d := f.Apa(f.WRK, string(decl), "declare", "-", "--evidence", "声明唤醒")
		var out struct {
			Rev int `json:"rev"`
		}
		if d.Code != 0 || json.Unmarshal([]byte(d.Stdout), &out) != nil || out.Rev < 1 {
			t.Fatalf("apa declare: %d %s%s", d.Code, d.Stdout, d.Stderr)
		}
		pre := f.tagged("wak01-unconfirmed", "acc-wake")
		time.Sleep(3 * time.Second)
		for _, r := range f.runs(pre) {
			if r.Launch == "wake" {
				t.Fatalf("声明未确认就走了唤醒: %+v", r)
			}
		}
		f.webOK("POST", "/settings/declarations/"+wakeHN+"/"+itoa(out.Rev)+"/confirm", "", 200)

		if pending := f.webOK("GET", "/settings/declarations", "", 200); strings.Contains(pending, wakeHN) {
			t.Errorf("确认后 %s 不应还在待确认清单里: %s", wakeHN, pending)
		}
		if active := f.webOK("GET", "/settings/commands", "", 200); !strings.Contains(active, wakeHN) || !strings.Contains(active, `"wake"`) {
			t.Errorf("Web 设置页应显示 %s 的唤醒声明已确认: %s", wakeHN, active)
		}
		if rules := f.webOK("GET", "/rules", "", 200); !strings.Contains(rules, `"auto"`) {
			t.Errorf("Web 规则页应显示规则为 auto: %s", rules)
		}
	})

	f.step("ACT-WAK-02 产出唤醒投递，内核不外呼", func(t *testing.T) {
		i1 = f.tagged("WAK-I1", "acc-wake")
		r := f.waitRun(i1)
		run1 = r.ID
		if r.Launch != "wake" || r.State != "running" || r.Runner != "main_session" {
			t.Fatalf("Run 应为 launch=wake、状态 running: %+v", r)
		}
		if _, err := os.Stat(filepath.Join(f.Home, "runs", run1, "stub-harness-env.json")); err == nil {
			t.Error("STUB_HARNESS 不应被拉起，却留下了运行环境记录")
		}
		// 趁 DEL 还没有租约：备一个带 PR 的委派子事项，挂上白名单内外各一条跟进消息（ACT-WAK-05 用）。
		child = f.attachedChild(f.add("acc-open", "WAK-PARENT", "open"))
		d := f.gatewayClaim(f.DEL, 5)
		if d == nil || d.Class != "wake" || d.Run != run1 || d.Item != i1 {
			t.Fatalf("应领到 class=wake、带运行 id 的投递: %+v", d)
		}
		lacks(t, "唤醒投递", d.Raw, "apa_run_", "token", "credential")
		if r := f.APIRaw("POST", "/v1/deliveries/"+d.ID+"/ack", `{"version":9223372036854775807,"external_ref":"`+run1+`"}`, f.DEL); r.Code != 200 {
			t.Errorf("网关插件领取后回报送达: HTTP %d %s", r.Code, r.Body)
		}
		time.Sleep(3 * time.Second)
		for _, m := range bot.Sent() { // 前面步骤里失败的运行已按打扰策略通知过本人；唤醒本身不能向本人发消息
			if strings.Contains(m.Text, "WAK-I1") || strings.Contains(m.Text, i1) || strings.Contains(m.Text, run1) {
				t.Errorf("唤醒运行不应向本人发消息: %+v", m)
			}
		}
	})

	f.step("ACT-WAK-03 网关插件领取投递并用同一把 delegate token 换运行凭据", func(t *testing.T) {
		var exp string
		cred1, exp = f.exchange(f.DEL, run1, 200)
		if !strings.HasPrefix(cred1, "apa_run_") {
			t.Errorf("运行凭据应以 apa_run_ 开头")
		}
		_, r := f.runState(run1)
		var started, expires time.Time
		var err error
		if exp == "" {
			t.Fatal("换取没有返回 expires_at")
		}
		if expires, err = time.Parse(time.RFC3339, exp); err != nil {
			t.Fatal(err)
		}
		if started, err = time.Parse(time.RFC3339, r.StartedAt); err != nil {
			t.Fatal(err)
		}
		if expires.After(started.Add(time.Duration(r.Timeout) * time.Second)) {
			t.Errorf("expires_at %s 不应晚于运行开始时间 %s 加运行超时 %ds", exp, r.StartedAt, r.Timeout)
		}
		ex := f.waitEvent("换取运行凭据的记录", ofRun("run.credential_exchanged", run1))
		lacks(t, "换取记录", ex.Raw, cred1, "apa_run_")
		f.waitEvent("唤醒租约开启的记录", ofRun("wake.lease_opened", run1))
		pytest(t, "plugins/hermes/tests/wake/test_wake.py",
			"test_wake_delivery_opens_turn_with_run_id_and_no_credential", "test_turn_cli_calls_carry_run_identity", "test_expiring_credential_is_exchanged_again")
	})

	d1, d2 := dec[0], dec[1]
	f.step("ACT-WAK-04 唤醒租约内该 delegate token 不能代答，附原话或授权单也不行", func(t *testing.T) {
		if st, _ := f.runState(run1); st != "running" {
			t.Fatalf("运行应仍未结束: %s", st)
		}
		f.delAnswer(d1, 4)
		grant := f.grantFor("DEL")
		f.apaEnv(f.DEL, 4, nil, "decide", d1, "--option", "A", "--grant", grant)
		if ph := f.phase(d1); ph != "needs_input" {
			t.Errorf("被拒绝的代答不应改变事项，阶段 = %s", ph)
		}
		f.webOK("POST", "/items/"+d2+"/decide", `{"choice":"A"}`, 200)
		if ph := f.phase(d2); ph == "needs_input" {
			t.Errorf("本人在 Web 的决定应生效，事项仍是 needs_input")
		}
	})

	f.step("ACT-WAK-05 运行身份透传，运行凭据不能代答", func(t *testing.T) {
		out, _ := f.asRun(run1, cred1, 0, nil, "context", "--scope", "item")
		contains(t, "运行身份下的 context（默认读目标事项）", out, i1)
		out, _ = f.asRun(run1, cred1, 0, nil, "context", "--scope", "item", "--item", child)
		contains(t, "运行身份下的 context（白名单内的挂靠消息）", out, child, mAlice)
		lacks(t, "运行身份下的 context（白名单外的挂靠消息不出现）", out, mBob)
		if all := f.webOK("GET", "/items/"+child+"/messages", "", 200); !strings.Contains(all, mBob) {
			t.Errorf("白名单外的消息本人在 Web 仍应可见: %s", all)
		}
		f.asRun(run1, cred1, 4, map[string]string{"APA_SESSION_KIND": "direct"}, "decide", d1, "--option", "A", "--evidence", "x")
		rej := f.waitEvent("按运行身份拒绝的代答", func(e event) bool { return e.Type == "command.rejected" && e.Object == d1 && e.Actor.Run == run1 })
		if rej.Payload["exit"] != float64(4) {
			t.Errorf("拒绝记录应带退出码 4: %s", rej.Raw)
		}
		if ph := f.phase(d1); ph != "needs_input" {
			t.Errorf("决策不应被回答: %s", ph)
		}
	})

	f.step("ACT-WAK-06 回执后运行成功结束、租约释放、代答恢复", func(t *testing.T) {
		f.asRun(run1, cred1, 0, nil, "report", i1, "--outcome", "done", "--summary", "完成", "--link", "https://example.invalid/acc/result/1")
		if ph := f.phase(i1); ph != "done" {
			t.Errorf("WAK-I1 应转 done: %s", ph)
		}
		f.waitRunState(run1, "succeeded")
		f.waitEvent("租约释放", ofRun("wake.lease_released", run1))
		f.delAnswer(d1, 0)
		f.asRun(run1, cred1, 3, nil, "inbox")
	})

	f.step("ACT-WAK-07 运行超时也释放租约，事项转 needs_input", func(t *testing.T) {
		i2 := f.tagged("WAK-I2", "acc-wake-short")
		r := f.waitRun(i2)
		if r.Launch != "wake" || r.Timeout != 60 {
			t.Fatalf("规则的运行超时应为 1 分钟: %+v", r)
		}
		f.exchange(f.DEL, r.ID, 200)
		d3 := dec[2]
		f.delAnswer(d3, 4) // 租约内
		f.TCAdvance(120)   // 不回执，等待 2 分钟（测试时钟）
		f.waitRunState(r.ID, "timeout")
		var ph string
		if !f.WaitFor(30*time.Second, func() bool { ph = f.phase(i2); return ph == "needs_input" }) {
			t.Errorf("超时后 WAK-I2 应转 needs_input: %s", ph)
		}
		f.delAnswer(d3, 0)
	})

	f.step("ACT-WAK-08 worker token 换取不开租约；换取规则受限", func(t *testing.T) {
		i3 := f.tagged("WAK-I3", "acc-wake")
		r := f.waitRun(i3)
		first, _ := f.exchange(f.WRK, r.ID, 200)
		d4 := dec[3]
		f.delAnswer(d4, 0) // worker 换取不开租约，DEL 仍可代答
		f.exchange(f.OBS, r.ID, 403)
		f.exchange(first, r.ID, 403)
		f.exchange(f.DEL, headlessRun, 409)
		f.exchange(f.DEL, run1, 409)
		second, _ := f.exchange(f.WRK, r.ID, 200)
		f.asRun(r.ID, first, 3, nil, "inbox")
		f.asRun(r.ID, second, 0, nil, "inbox")
		if n := len(f.findEvents(ofRun("run.credential_exchanged", r.ID))); n != 2 {
			t.Errorf("WRK 的两次换取应各记一条 Event，实际 %d 条", n)
		}
		if n := len(f.findEvents(ofRun("wake.lease_opened", r.ID))); n != 0 {
			t.Errorf("worker token 换取不应开租约，实际 %d 条 wake.lease_opened", n)
		}
	})

	f.step("ACT-WAK-09 唤醒回合需要本人时转 needs_input，不直接打扰", func(t *testing.T) {
		i4 := f.tagged("WAK-I4", "acc-wake")
		r := f.waitRun(i4)
		cred, _ := f.exchange(f.DEL, r.ID, 200)
		f.asRun(r.ID, cred, 0, nil, "ask", i4, "--question", "要继续吗", "--option", "yes=是", "--option", "no=否")
		if ph := f.phase(i4); ph != "needs_input" {
			t.Fatalf("ask 后事项应转 needs_input: %s", ph)
		}
		f.waitRunState(r.ID, "succeeded")
		// 决策投递由触达按打扰策略产出（路由到渠道 acc-chan），不经唤醒通道。
		if d := f.gatewayClaim(f.DEL, 1); d != nil && d.Class == "decision" {
			t.Errorf("决策投递不应走唤醒通道: %s", d.Raw)
		}
		if !f.WaitFor(90*time.Second, func() bool {
			for _, m := range bot.Sent() {
				if strings.Contains(m.Text, "WAK-I4") && strings.Contains(m.Text, "要继续吗") {
					return true
				}
			}
			return false
		}) {
			t.Error("触达应按打扰策略把这条决策送到渠道")
		}
	})
}
