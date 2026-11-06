//go:build e2e

package bgguards

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

type whyStep struct {
	Seq        int64  `json:"seq"`
	Type       string `json:"type"`
	ChainDepth int    `json:"chain_depth"`
}

func (f *fx) why(id string) []whyStep {
	f.t.Helper()
	var out struct {
		Steps []whyStep `json:"steps"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "why", id).Stdout), &out))
	return out.Steps
}

func (f *fx) depth(id string) int {
	f.t.Helper()
	d, _ := f.item(id)["chain_depth"].(float64)
	return int(d)
}

// loopScript 让每次运行委派一个子事项后结束（父事项转 waiting）。执行者（WRK）随后回执 done，父事项回到 open，
// 再次命中 loop 规则：这就是设计里"规则→运行→委派→回执→规则"的间接回环（§13.6、§14.9）。
const loopScript = preamble + `
CH=$($APA delegate --parent "$ITEM" --assignee work-host.cli --title loop 2>"$OUT/loop_delegate.err" | grep -o 'itm_[0-9a-f]*' | head -1)
echo "$CH" >> "$OUT/loop_children"
`

// round 等当前运行结束，然后以执行者身份回执它建的子事项；返回子事项 id（空表示这一轮没有新子事项）。
func (f *fx) round(parent string, n int) string {
	f.t.Helper()
	f.settled(parent, n)
	kids := strings.Fields(f.outFile("loop_children"))
	if len(kids) < n {
		return ""
	}
	child := kids[n-1]
	f.apa(f.WRK, 0, "report", child, "--outcome", "done", "--summary", "回环", "--link", "https://example.invalid/e2e/"+child)
	return child
}

// TestFlowChainDepth 对应验收文档 flow-chain-depth（ACT-DEP-01..03）。
// 阈值取默认 8（apa serve 没有调阈值的配置项），所以用真实的规则回环走到超限。
func TestFlowChainDepth(t *testing.T) {
	f := setup(t)
	f.auto("loop")
	var looped string
	const threshold = 8
	rounds := 0

	f.step("ACT-DEP-01 深度沿规则和回执继承", func(t *testing.T) {
		f.mode("summary-only", loopScript)
		looped = idOf(t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "dep-loop", "--phase", "proposed",
			"--project", f.Projects["acc-work"], "--tags", "acc-loop", "--evidence", "验收触发").Stdout)
		f.apa(f.DEL, 0, "item", "transition", looped, "--to", "open", "--reason", "开始回环", "--evidence", "e2e 开始回环")
		for rounds < 3 {
			rounds++
			if f.round(looped, rounds) == "" {
				t.Fatalf("第 %d 轮没有新的子事项", rounds)
			}
			f.waitFor("回执后规则再次命中", 20*time.Second, func() bool { return len(f.runsOf(looped)) > rounds })
		}
		steps := f.why(looped)
		last, increases := 0, 0
		for _, s := range steps {
			if s.ChainDepth < last {
				t.Errorf("链路深度被重置: seq %d 的 %s 深度 %d 小于之前的 %d", s.Seq, s.Type, s.ChainDepth, last)
			}
			if s.ChainDepth > last {
				increases++
			}
			last = s.ChainDepth
		}
		if last < 3 || increases < 3 {
			t.Errorf("走 3 轮后深度应逐步递增（至少 3 次增长、末值 ≥ 3），实际末值 %d、增长 %d 次: %+v", last, increases, steps)
		}
		kids := strings.Fields(f.outFile("loop_children"))
		if f.depth(kids[0]) < 1 || f.depth(kids[len(kids)-1]) <= f.depth(kids[0]) {
			t.Errorf("每轮委派的子事项深度应递增: %v", kids)
		}
	})

	f.step("ACT-DEP-02 超阈值只记录", func(t *testing.T) {
		for n := rounds + 1; n <= threshold+4 && len(f.events("", "health.alert")) == 0; n++ {
			rounds = n
			if f.round(looped, n) == "" {
				break
			}
			f.waitFor("回执后规则再次命中或熔断", 20*time.Second, func() bool {
				return len(f.runsOf(looped)) > n || len(f.events("", "health.alert")) > 0
			})
		}
		if len(f.events("", "health.alert")) == 0 {
			t.Fatalf("回环走了 %d 轮、深度 %d 仍未熔断（阈值 %d）", rounds, f.depth(looped), threshold)
		}
		time.Sleep(3 * time.Second) // 之后不应再有新的派发
		n := len(f.runsOf(looped))
		time.Sleep(3 * time.Second)
		eq(t, "熔断后 Run 数不再增长", len(f.runsOf(looped)), n)
		var trip event
		for _, e := range f.events("", "health.alert") {
			if e.Reason == "chain depth exceeded" {
				trip = e
				break
			}
		}
		if trip.Seq == 0 {
			t.Fatalf("应有一条 chain depth exceeded 的 health.alert: %+v", f.events("", "health.alert"))
		}
		if d, _ := trip.Payload["chain_depth"].(float64); int(d) <= threshold || trip.Payload["threshold"] != float64(threshold) {
			t.Errorf("熔断记录应写明超过阈值 %d 的深度: %+v", threshold, trip.Payload)
		}
		for _, r := range f.runsOf(looped) {
			if r.State == "queued" || r.State == "running" {
				t.Errorf("熔断后不应有在途运行: %+v", r)
			}
		}
		if !strings.Contains(f.webOK("GET", "/deliveries/unread", "", 200), "health") {
			t.Error("应产出健康类投递")
		}
		if h := f.apa(f.OBS, 0, "health").Stdout; !strings.Contains(h, "chain") {
			t.Errorf("健康视图应有链路熔断: %s", h)
		}
	})

	f.step("ACT-DEP-03 本人重新委派才清零", func(t *testing.T) {
		// 本人在 Web 重新委派被熔断的事项：新链路从头开始。
		steps := f.why(looped)
		if d := steps[len(steps)-1].ChainDepth; d <= threshold {
			t.Errorf("被熔断事项最近一条事件的深度应超过阈值 %d: %d", threshold, d)
		}
		f.mode("summary-only", "")
		body, _ := json.Marshal(map[string]string{"parent": looped, "assignee": "work-host.cli", "title": "dep-redelegated"})
		fresh := idFrom(f.webOK("POST", "/delegations", string(body), 201))
		eq(t, "重新委派的新子事项深度", f.depth(fresh), 0)

		// 另取深度大于 0 的事项：runner=task 的子事项由规则动作建出，深度是触发事项加一。
		f.auto("task-work")
		taskChild := func(title string) string {
			parent := f.task(title, "acc-work", "acc-task")
			var child string
			f.waitFor("任务型子事项的运行结束", 30*time.Second, func() bool {
				for _, r := range f.runs("--category", "background") {
					if r.Runner == "task" && r.Target != parent && f.item(r.Target)["title"] == title && r.State != "queued" && r.State != "running" {
						child = r.Target
					}
				}
				return child != ""
			})
			return child
		}

		// 经 apa ask 并在 Web 回答：不清零。
		f.mode("summary-only", preamble+`
$APA ask "$ITEM" --question 选哪个 --option A=甲 --option B=乙 > "$OUT/ask.out" 2>&1; rc ask
`)
		asked := taskChild("dep-ask")
		d0 := f.depth(asked)
		if d0 < 1 {
			t.Fatalf("规则动作建的子事项深度应大于 0: %d", d0)
		}
		eq(t, "运行内 apa ask 退出码 "+f.outFile("ask.out"), f.outFile("ask.rc"), "0")
		f.webOK("POST", "/items/"+asked+"/decide", `{"choice":"A","evidence":"本人在 Web 选 A"}`, 200)
		if got := f.depth(asked); got < d0 {
			t.Errorf("提问并回答后深度被清零: %d → %d", d0, got)
		}
		for _, e := range f.events(asked, "decision.resolved") {
			if e.ChainDepth < d0 {
				t.Errorf("回答事件的深度被清零: %+v", e)
			}
		}

		// 经定时：决策超时的 Timer 到期按默认值决定，不清零。
		f.mode("summary-only", preamble+`
DL=$(date -u -d '+3 seconds' +%Y-%m-%dT%H:%M:%SZ)
$APA ask "$ITEM" --question 超时默认 --option A=甲 --option B=乙 --deadline "$DL" --default-on-timeout B > "$OUT/timer.out" 2>&1; rc timer
`)
		timed := taskChild("dep-timer")
		eq(t, "运行内带 deadline 的 apa ask 退出码 "+f.outFile("timer.out"), f.outFile("timer.rc"), "0")
		t0 := f.depth(timed)
		f.waitFor("定时到期按默认值决定", 40*time.Second, func() bool { return len(f.events(timed, "decision.resolved")) == 1 })
		if got := f.depth(timed); got < t0 {
			t.Errorf("定时到期后深度被清零: %d → %d", t0, got)
		}
		for _, e := range f.events(timed, "decision.resolved") {
			if e.ChainDepth < t0 {
				t.Errorf("定时产生的事件深度被清零: %+v", e)
			}
		}
	})
}
