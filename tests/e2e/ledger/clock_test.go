//go:build e2e

package ledger

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// waitFor 轮询直到 cond 成立。内核时钟每秒扫描一次，所以到期后几秒内必须触发。
func waitFor(t *testing.T, within time.Duration, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("%s：%s 内未发生", what, within)
}

func setDeadline(t *testing.T, e *baseline.Env, id string, at time.Time) {
	t.Helper()
	if r := e.Web("PATCH", "/v1/items/"+id, `{"deadline":"`+at.UTC().Format(time.RFC3339)+`"}`); r.Code != 200 {
		t.Fatalf("设置到期时间: %d %s", r.Code, r.Body)
	}
}

func deadlineEvents(t *testing.T, e *baseline.Env, id string) []step {
	return ofType(audit(t, e, "object="+id), "item.deadline_passed")
}

// 验收文档的等待时长（1 分钟、3 分钟）缩成秒级：内核时钟按绝对时间触发，与等待多久无关；
// 要证明的是"没有任何会话在线也触发"和"停机期间错过的到期恢复后全部触发"。

// TestFlowClockDue 按验收文档 flow-clock-due 执行 ACT-CLK-01、ACT-CLK-02。
func TestFlowClockDue(t *testing.T) {
	t.Run("ACT-CLK-01 事项到期由时钟触发", func(t *testing.T) {
		e := baseline.Setup(t)
		id := addTask(t, e, "due-soon")
		setDeadline(t, e, id, time.Now().Add(3*time.Second))
		if n := len(deadlineEvents(t, e, id)); n != 0 {
			t.Fatalf("到期前不应触发: %d", n)
		}
		waitFor(t, 20*time.Second, "到期 Event", func() bool { return len(deadlineEvents(t, e, id)) > 0 })
		evs := deadlineEvents(t, e, id)
		if len(evs) != 1 || evs[0].Actor.Type != "system" || evs[0].Actor.Via != "clock" {
			t.Errorf("应恰有一条 actor=system、via=clock 的记录: %+v", evs)
		}
		// 时间线（apa why）里同样可见。
		hit := false
		_, steps := why(t, e, e.OBS, id)
		for _, s := range steps {
			hit = hit || (s.Type == "item.deadline_passed" && s.Actor.Type == "system" && s.Actor.Via == "clock")
		}
		if !hit {
			t.Error("apa why 时间线缺少时钟触发的到期记录")
		}
		// 没有任何 harness 会话：没有运行记录被创建来驱动它。
		if r := e.Apa(e.OBS, "", "run", "list"); r.Code != 0 || strings.Contains(r.Stdout, `"harness"`) && strings.Contains(r.Stdout, "acc-stub") {
			t.Errorf("不应有 harness 运行参与: %d %s", r.Code, r.Stdout)
		}
		// 不改阶段、字段和版本（§6.8）。
		it := show(t, e, id)
		if it["phase"] != "open" {
			t.Errorf("到期不应改阶段: %v", it["phase"])
		}
	})

	t.Run("ACT-CLK-02 错过的一次性定时醒来全部触发", func(t *testing.T) {
		e := baseline.Setup(t)
		// 缩短合并窗口并让提醒走 Web 未读，才能从外面数出"发了几次打扰"。
		pol, _ := json.Marshal(map[string]any{"disturb": map[string]any{
			"routes":             map[string]string{"brief": "inject", "decision": "inject", "health": "web", "reminder": "web", "wake": "web"},
			"merge_window_sec":   2,
			"remind_on_deadline": true,
		}})
		if r := e.Web("PUT", "/v1/settings/policy", string(pol)); r.Code != 200 {
			t.Fatalf("设置打扰策略: %d %s", r.Code, r.Body)
		}
		var ids []string
		due := time.Now().Add(8 * time.Second)
		for _, title := range []string{"missed-1", "missed-2", "missed-3"} {
			id := addTask(t, e, title, "--priority", "low")
			setDeadline(t, e, id, due)
			ids = append(ids, id)
		}
		e.StopKernel() // 到期前停服务
		time.Sleep(time.Until(due) + 3*time.Second)
		e.StartKernelAgain()

		waitFor(t, 30*time.Second, "三个事项都触发到期", func() bool {
			for _, id := range ids {
				if len(deadlineEvents(t, e, id)) == 0 {
					return false
				}
			}
			return true
		})
		for _, id := range ids {
			evs := deadlineEvents(t, e, id)
			if len(evs) != 1 || evs[0].Actor.Via != "clock" || evs[0].Actor.Type != "system" {
				t.Errorf("%s 应触发恰好一次由时钟写入的到期: %+v", id, evs)
			}
		}
		// 三条低优先级提醒由打扰控制合并成一条摘要，而不是各发一次。
		type delivery struct {
			Content struct {
				Title   string `json:"title"`
				Summary string `json:"summary"`
			} `json:"content"`
		}
		unread := func() (out []delivery) {
			var body struct {
				Deliveries []delivery `json:"deliveries"`
			}
			if json.Unmarshal([]byte(e.Web("GET", "/v1/deliveries/unread", "").Body), &body) != nil {
				return nil
			}
			return body.Deliveries
		}
		waitFor(t, 60*time.Second, "合并后的提醒进入 Web 未读", func() bool { return len(unread()) > 0 })
		time.Sleep(5 * time.Second) // 多等一会儿，确认没有后续的零散提醒
		got := unread()
		if len(got) != 1 {
			t.Fatalf("应合并为恰好一条打扰，实际 %d 条: %+v", len(got), got)
		}
		d := got[0]
		for _, title := range []string{"missed-1", "missed-2", "missed-3"} {
			if !strings.Contains(d.Content.Summary, title) {
				t.Errorf("合并摘要应覆盖 %s: %+v", title, d.Content)
			}
		}
	})
}
