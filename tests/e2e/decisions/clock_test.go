//go:build e2e

package decisions

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// 验收文档的等待（1 分钟、3 分钟）缩成秒级：内核时钟按绝对时间触发，与等待多久无关；
// 要证明的是"没有任何会话在线也触发"和"停机期间错过的到期恢复后全部触发"。

func (f *fx) waitFor(within time.Duration, what string, cond func() bool) {
	f.t.Helper()
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
	}
	f.t.Fatalf("%s：%s 内未发生", what, within)
}

func (f *fx) setDeadline(id string, at time.Time) {
	f.t.Helper()
	f.webOK("PATCH", "/items/"+id, `{"deadline":"`+at.UTC().Format(time.RFC3339)+`"}`, 200)
}

func (f *fx) deadlineEvents(id string) []step { return f.events(id, "item.deadline_passed") }

// TestFlowClockDue 对应 flow-clock-due（ACT-CLK-01、ACT-CLK-02）。
func TestFlowClockDue(t *testing.T) {
	t.Run("ACT-CLK-01 事项到期由时钟触发", func(t *testing.T) {
		f := setup(t) // 不拉起任何 harness 会话
		id := f.task("due-soon", "acc-work")
		f.setDeadline(id, time.Now().Add(3*time.Second))
		if n := len(f.deadlineEvents(id)); n != 0 {
			t.Fatalf("到期前不应触发: %d", n)
		}
		f.waitFor(20*time.Second, "到期 Event", func() bool { return len(f.deadlineEvents(id)) > 0 })
		evs := f.deadlineEvents(id)
		if len(evs) != 1 || evs[0].Actor.Type != "system" || evs[0].Actor.Via != "clock" {
			t.Errorf("应恰有一条 actor=system、via=clock 的记录: %+v", evs)
		}
		if why := f.apa(f.OBS, 0, "why", id).Stdout; !strings.Contains(why, "item.deadline_passed") {
			t.Errorf("apa why 时间线缺少到期记录: %s", why)
		}
		eq(t, "阶段（到期不改阶段）", f.phase(id), "open")
	})

	t.Run("ACT-CLK-02 错过的一次性定时醒来全部触发", func(t *testing.T) {
		f := setup(t)
		// 缩短合并窗口并让提醒走 Web 未读，才能从外面数出"发了几次打扰"。
		f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"inject","decision":"inject","health":"web","reminder":"web","wake":"web"},"merge_window_sec":2,"remind_on_deadline":true}}`, 200)
		var ids []string
		due := time.Now().Add(8 * time.Second)
		for _, title := range []string{"missed-1", "missed-2", "missed-3"} {
			// 低优先级提醒才进合并窗口。
			id := f.idOf(f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--priority", "low",
				"--project", f.Projects["acc-work"], "--evidence", "建个任务"))
			f.setDeadline(id, due)
			ids = append(ids, id)
		}
		f.StopKernel() // 到期前停服务
		time.Sleep(time.Until(due) + 3*time.Second)
		f.StartKernelAgain()

		f.waitFor(30*time.Second, "三个事项都触发到期", func() bool {
			for _, id := range ids {
				if len(f.deadlineEvents(id)) == 0 {
					return false
				}
			}
			return true
		})
		for _, id := range ids {
			if evs := f.deadlineEvents(id); len(evs) != 1 || evs[0].Actor.Via != "clock" || evs[0].Actor.Type != "system" {
				t.Errorf("%s 应触发恰好一次由时钟写入的到期: %+v", id, evs)
			}
		}
		type delivery struct {
			Content struct {
				Summary string `json:"summary"`
			} `json:"content"`
		}
		unread := func() (out []delivery) {
			var body struct {
				Deliveries []delivery `json:"deliveries"`
			}
			_ = json.Unmarshal([]byte(f.Web("GET", "/v1/deliveries/unread", "").Body), &body)
			return body.Deliveries
		}
		f.waitFor(60*time.Second, "合并后的提醒进入 Web 未读", func() bool { return len(unread()) > 0 })
		time.Sleep(5 * time.Second) // 确认没有后续零散提醒
		got := unread()
		if len(got) != 1 {
			t.Fatalf("应合并为恰好一条打扰，实际 %d 条: %+v", len(got), got)
		}
		for _, title := range []string{"missed-1", "missed-2", "missed-3"} {
			if !strings.Contains(got[0].Content.Summary, title) {
				t.Errorf("合并摘要应覆盖 %s: %+v", title, got[0].Content)
			}
		}
	})
}
