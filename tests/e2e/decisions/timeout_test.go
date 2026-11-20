//go:build e2e

package decisions

import (
	"strings"
	"testing"
	"time"
)

// TestFlowDecisionTimeout 对应 flow-decision-timeout：到期的决策请求永不因超时被决定，只再提醒（deadline = 当前 + 1 分钟，最多等待 2 分钟）。
// 2026-11-20 起超时默认取消（§14.4、§6.14）：ask 带 --default-on-timeout 是用法错误，原 ACT-TMO-01「到期按默认值决定」删除。
func TestFlowDecisionTimeout(t *testing.T) {
	f := setup(t)
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"inject","decision":"inject","health":"web","reminder":"inject","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true}}`, 200)

	deadline := time.Now().Add(time.Minute).UTC().Format(time.RFC3339)
	id := f.task("tmo-2", "acc-work")
	if r := f.Apa(f.WRK, "", "ask", id, "--question", "选哪个", "--option", "A=甲", "--option", "B=乙", "--deadline", deadline, "--default-on-timeout", "A"); r.Code != 2 {
		t.Fatalf("带 --default-on-timeout 的 ask 应为用法错误（2）: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.ask(id, "--deadline", deadline)

	// 再提醒是一条 class=reminder 的投递（id 为 remind:ask:<事项>:<截止>）；还没被领取时在健康视图的投递积压里，
	// 退回 Web 后在未读里。两处任一出现即算产生。轮询而不是固定睡 2 分钟。
	remindID := "remind:ask:" + id
	reminded := func() bool {
		return strings.Contains(f.apa(f.OBS, 0, "health").Stdout, remindID) || strings.Contains(f.Web("GET", "/v1/deliveries/unread", "").Body, remindID)
	}
	for stop := time.Now().Add(2 * time.Minute); time.Now().Before(stop) && !reminded(); {
		time.Sleep(2 * time.Second)
	}

	t.Run("ACT-TMO-02 到期保持等待并再提醒", func(t *testing.T) {
		eq(t, "阶段", f.phase(id), "needs_input")
		if f.resolution(id) != nil {
			t.Error("到期的决策被自动决定")
		}
		if !reminded() {
			t.Error("没有产生再提醒")
		}
	})
}
