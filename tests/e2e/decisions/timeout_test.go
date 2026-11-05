//go:build e2e

package decisions

import (
	"strings"
	"testing"
	"time"
)

// TestFlowDecisionTimeout 对应 flow-decision-timeout（ACT-TMO-01、ACT-TMO-02）：
// 两个决策请求同时到期（deadline = 当前 + 1 分钟），按验收文档最多等待 2 分钟。
func TestFlowDecisionTimeout(t *testing.T) {
	f := setup(t)
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"inject","decision":"inject","health":"web","reminder":"inject","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true}}`, 200)

	deadline := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	withDefault, noDefault := f.task("tmo-1", "acc-work"), f.task("tmo-2", "acc-work")
	f.ask(withDefault, "--deadline", deadline, "--default-on-timeout", "A")
	f.ask(noDefault, "--deadline", deadline)
	eq(t, "到期前 tmo-1", f.phase(withDefault), "needs_input")

	// 再提醒是一条 class=reminder 的投递（id 为 remind:ask:<事项>:<截止>）；还没被领取时在健康视图的投递积压里，
	// 退回 Web 后在未读里。两处任一出现即算产生。轮询而不是固定睡 2 分钟：两个条件都满足即停，最长 2 分钟。
	remindID := "remind:ask:" + noDefault
	reminded := func() bool {
		return strings.Contains(f.apa(f.OBS, 0, "health").Stdout, remindID) || strings.Contains(f.Web("GET", "/v1/deliveries/unread", "").Body, remindID)
	}
	stop := time.Now().Add(2 * time.Minute)
	for time.Now().Before(stop) {
		if f.resolution(withDefault) != nil && reminded() {
			break
		}
		time.Sleep(2 * time.Second)
	}

	t.Run("ACT-TMO-01 到期按默认值决定", func(t *testing.T) {
		r := f.resolution(withDefault)
		if r == nil {
			t.Fatal("到期后未决定")
		}
		eq(t, "resolution", r["choice"], "A")
		eq(t, "resolved_via", r["resolved_via"], "timeout")
		ev := f.last(withDefault, "decision.resolved")
		eq(t, "actor.type", ev.Actor.Type, "system")
		eq(t, "via", ev.Actor.Via, "clock")
	})

	t.Run("ACT-TMO-02 无默认值保持等待并再提醒", func(t *testing.T) {
		eq(t, "阶段", f.phase(noDefault), "needs_input")
		if f.resolution(noDefault) != nil {
			t.Error("无默认值的决策被自动决定")
		}
		if !reminded() {
			t.Error("没有产生再提醒")
		}
		if strings.Contains(f.apa(f.OBS, 0, "health").Stdout, "remind:ask:"+withDefault) {
			t.Error("有默认值的决策由超时决定，不应再提醒")
		}
	})
}
