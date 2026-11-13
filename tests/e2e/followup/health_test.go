//go:build e2e

package followup

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// allSignalIDs 是健康视图恒有的十三项，顺序固定（设计 §15.2）。
var allSignalIDs = append(append([]string{}, signalIDs...), "stalled_delegations", "clock_lag")

// stall 造出一个长时间无回执的委派（ACT-HLT-04 前半）：HL-1 委派给 work-host.cli，执行者不回执，测试时钟推进 3 分钟（> delegation_stall_after 2m）。
// 返回委派子事项 id。
func (f *fx) stall() string {
	f.t.Helper()
	hl1 := f.task("HL-1", "x-plain")
	child := decode[struct {
		ID string `json:"id"`
	}](f.t, f.apa(f.DEL, 0, "delegate", "--parent", hl1, "--assignee", "work-host.cli", "--title", "HL-1 子事项", "--reason", "委派", "--evidence", "owner asked").Stdout).ID
	f.TCAdvance(180)
	return child
}

// lag 造出时钟迟到（ACT-HLT-05 前半）：HL-2 截止设为 1 分钟后，时钟一次越过 8 分钟（等价于停机 8 分钟再启动：定时晚于应触发时刻 5 分钟以上）。
func (f *fx) lag() string {
	f.t.Helper()
	hl2 := f.task("HL-2", "x-plain")
	f.webOK("PATCH", "/items/"+hl2, `{"deadline":"`+f.kernelNow().Add(time.Minute).UTC().Format(time.RFC3339)+`"}`, http.StatusOK)
	f.TCAdvance(8 * 60)
	return hl2
}

// kernelNow 是内核此刻的时间：真实时间加测试时钟偏移量。
func (f *fx) kernelNow() time.Time {
	f.t.Helper()
	off := int64(0)
	if b, err := os.ReadFile(filepath.Join(f.Home, "test-clock-offset")); err == nil {
		off, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	return time.Now().Add(time.Duration(off) * time.Second)
}

// ---- 全流程 ----

// TestFlowHealth 对应 flow-health（ACT-HLT-01..07）：十三类信号一次全造出，Web、CLI、HTTP 三处一致，只报不修；
// 提醒级的两项另在干净环境上验证恒为十三项、不影响 healthy、回执后与连续一小时无迟到后清除（TestFlowHealthWarnings）。
// 长时运行额度与短任务额度都调到 1，让两个队列都能排出队来；delegation_stall_after 调为 2m。
func TestFlowHealth(t *testing.T) {
	f := opsFx(t, "max_subprocesses: 1\nmax_short_subprocesses: 1\ndelegation_stall_after: 2m\n")
	open := f.Projects["acc-open"]
	var driftItem, tripItem string

	step(t, "全新环境十三项信号都正常", func(t *testing.T) {
		h := f.health()
		if !h.Healthy || len(h.Signals) != len(allSignalIDs) {
			t.Fatalf("全新环境应健康且恒有十三项信号: %+v", h)
		}
		for i, id := range allSignalIDs {
			if h.Signals[i].ID != id || !h.Signals[i].OK {
				t.Errorf("第 %d 个信号 = %+v，期望 %s 正常", i, h.Signals[i], id)
			}
		}
	})

	step(t, "造出：来源失败", func(t *testing.T) {
		f.addScriptSource("src_acc_fail", f.script("fail.sh", "cat >/dev/null\necho 'boom' >&2\nexit 1\n"), "1s", 30)
		f.waitFor("来源失败进入健康视图", 30*time.Second, func() bool { return !f.health().sig("source_failures").OK })
	})

	step(t, "造出：能力包加载失败", func(t *testing.T) {
		writeFile(t, filepath.Join(f.Home, "packs", "acc-bad", "pack.yaml"), `name: acc-bad
version: 1.0.0
description: 验收用：规则引用了不存在的提示词
event_rules:
  - id: broken
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-broken]}
    do: [{action: call_agent, prompt: no-such-prompt}]
`)
		f.waitFor("能力包加载失败进入健康视图", 30*time.Second, func() bool { return !f.health().sig("pack_rejections").OK })
	})

	step(t, "造出：规则回退 observe", func(t *testing.T) {
		f.auto("probe")
		p := filepath.Join(f.Home, "packs", "acc-ops", "pack.yaml")
		b, _ := os.ReadFile(p)
		edited := strings.Replace(string(b), "tag_any: [acc-probe]", "tag_any: [acc-probe-v2]", 1)
		if edited == string(b) {
			t.Fatal("没找到可改的规则内容")
		}
		writeFile(t, p, edited)
		f.waitFor("规则内容变化后回退 observe 进入健康视图", 30*time.Second, func() bool { return !f.health().sig("rule_fallbacks").OK })
	})

	step(t, "造出：回执被拒", func(t *testing.T) {
		// 回执被拒：WRK2 对 WRK 的委派子事项 apa report，内核拒绝（退出码 4）并记 health.alert。
		parent := f.task("HLT 回执被拒的父事项", "x-plain")
		child := decode[struct {
			ID string `json:"id"`
		}](t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", "只有 WRK 能回执", "--reason", "委派", "--evidence", "owner asked").Stdout).ID
		if r := f.Apa(f.WRK2, "", "report", child, "--outcome", "progress", "--summary", "越权回执"); r.Code == 0 {
			t.Fatalf("WRK2 不应能给 WRK 的委派回执: %s", r.Stdout)
		}
		f.waitFor("回执被拒进入健康视图", 30*time.Second, func() bool { return !f.health().sig("receipt_failures").OK })
	})

	step(t, "造出：其他健康告警", func(t *testing.T) {
		f.auto("bad-action")
		f.task("HLT 规则动作失败", "acc-bad")
		f.waitFor("规则动作失败进入健康视图", 30*time.Second, func() bool { return !f.health().sig("other_alerts").OK })
	})

	step(t, "造出：AGENTS.md 漂移", func(t *testing.T) {
		file := filepath.Join(t.TempDir(), "AGENTS.md")
		f.Apa(f.OBS, "", "agents", "sync", "--repo", f.TestRepo, "--file", file)
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, append(b, []byte("\n手改的一行\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		r := f.apa(f.OBS, 11, "agents", "check", "--repo", f.TestRepo, "--file", file)
		driftItem = decode[struct {
			Item string `json:"item"`
		}](t, r.Stdout).Item
		if f.health().sig("agentsmd_drift").OK {
			t.Fatal("漂移应进入健康视图")
		}
	})

	step(t, "造出：模型不可用和未判定积压", func(t *testing.T) {
		f.ModelStub.Reset()
		f.ModelStub.SetDefault(modelstub.Reply{Status: 500})
		f.webOK("PATCH", "/projects/"+open, `{"capture":true}`, http.StatusOK)
		f.capture("acc-open", "HLT 模型端点故障时提交", "s1")
		f.waitFor("模型不可用进入健康视图", 60*time.Second, func() bool { return !f.health().sig("model_unavailable").OK })
	})

	step(t, "造出：投递积压", func(t *testing.T) {
		f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"acc-chan","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false}}`, http.StatusOK)
		id := f.task("HLT 需要决策", "x-plain")
		f.apa(f.WRK, 0, "ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A")
		f.waitFor("投递积压进入健康视图", 30*time.Second, func() bool { return !f.health().sig("delivery_backlog").OK })
	})

	step(t, "造出：链路熔断", func(t *testing.T) {
		f.auto("loop")
		tripItem = f.task("HLT 链路循环", "x-loop")
		f.waitFor("链路熔断进入健康视图", 60*time.Second, func() bool { return !f.health().sig("chain_trips").OK })
		f.webOK("PATCH", "/rules/"+rulePath("loop")+"/mode", `{"mode":"observe"}`, http.StatusOK) // 熔断已造出，停止继续循环
		f.waitFor("循环的运行全部结束", 30*time.Second, func() bool {
			o := f.health().sig("outcall").outcall(t)
			return o.Long.Running == 0 && o.Long.Queued == 0
		})
	})

	step(t, "造出：外呼超时", func(t *testing.T) {
		f.auto("run-hang")
		f.task("HLT 超时", "acc-hang")
		f.waitFor("外呼超时进入健康视图", 60*time.Second, func() bool { return !f.health().sig("outcall").OK })
	})

	step(t, "造出：长时运行与短任务各自排队", func(t *testing.T) {
		f.auto("run-hold")
		for _, n := range []string{"A", "B", "C"} {
			f.task("HLT 占用 "+n, "acc-hold")
			time.Sleep(200 * time.Millisecond)
		}
		for _, n := range []string{"a", "b", "c"} {
			f.addScriptSource("src_acc_slow_"+n, f.script("slow-"+n+".sh", "cat >/dev/null\nsleep 7000\n"), "1s", 7200)
		}
		f.waitFor("长时运行 1 个在跑、2 个排队，短任务 1 个在跑、至少 1 个排队", 60*time.Second, func() bool {
			o := f.health().sig("outcall").outcall(t)
			return o.Long.Running == 1 && o.Long.Queued == 2 && o.Short.Running == 1 && o.Short.Queued >= 1
		})
		time.Sleep(1500 * time.Millisecond)
	})

	var stalledChild, lagItem string
	step(t, "造出：长时间无回执的委派（ACT-HLT-04）", func(t *testing.T) {
		stalledChild = f.stall()
		f.waitFor("长时间无回执的委派进入健康视图", 30*time.Second, func() bool { return !f.health().sig("stalled_delegations").OK })
	})
	step(t, "造出：时钟迟到（ACT-HLT-05）", func(t *testing.T) {
		lagItem = f.lag()
		f.waitFor("时钟迟到进入健康视图", 30*time.Second, func() bool { return !f.health().sig("clock_lag").OK })
		f.waitFor("HL-2 的到期补触发", 30*time.Second, func() bool { return len(f.eventsOf(lagItem, "item.deadline_passed")) == 1 })
	})

	var cli healthView
	step(t, "ACT-HLT-01 十三类信号都显示，三处一致，每类有修复入口", func(t *testing.T) {
		cli = f.health()
		web := f.webHealth()
		http := decode[healthView](t, f.APIRaw("GET", "/v1/health", "", f.OBS).Body)
		if cli.Healthy {
			t.Fatal("十一类问题都在时不应报健康")
		}
		if len(cli.Signals) != len(allSignalIDs) {
			t.Fatalf("应恒有十三项信号: %+v", cli.Signals)
		}
		for i, id := range allSignalIDs {
			s := cli.Signals[i]
			if s.ID != id || s.OK || s.Count == 0 || s.Why == "" {
				t.Errorf("信号 %s 应显示为异常并带原因: %+v", id, s)
			}
			if strings.TrimSpace(s.Next) == "" {
				t.Errorf("信号 %s 没有修复入口", id)
			}
			for name, other := range map[string]healthView{"Web": web, "HTTP": http} {
				if w := other.sig(id); w.OK != s.OK || w.Count != s.Count || w.Next != s.Next || w.Why != s.Why || w.Severity != s.Severity {
					t.Errorf("信号 %s 三处不一致: CLI %+v / %s %+v", id, s, name, w)
				}
			}
		}
		for _, id := range []string{"stalled_delegations", "clock_lag"} {
			if s := cli.sig(id); s.Severity != "warning" {
				t.Errorf("提醒级信号 %s 异常时 severity 应为 warning: %+v", id, s)
			}
		}
		if !strings.Contains(string(cli.sig("stalled_delegations").Detail), stalledChild) && !strings.Contains(cli.sig("stalled_delegations").Next, "delegations") {
			t.Errorf("长时间无回执的委派应指向任务委派页并列出子事项: %+v", cli.sig("stalled_delegations"))
		}
		o := cli.sig("outcall").outcall(t)
		eq(t, "长时运行在跑", o.Long.Running, 1)
		eq(t, "长时运行排队", o.Long.Queued, 2)
		eq(t, "短任务在跑", o.Short.Running, 1)
		if o.Short.Queued < 1 {
			t.Errorf("短任务应有排队: %+v", o.Short)
		}
		if o.Long.Longest < 1 || o.Short.Longest < 1 {
			t.Errorf("两个队列的最长等待都应 ≥ 1 秒（分开显示）: %+v", o)
		}
		if len(o.Timeouts) == 0 {
			t.Error("应列出超时的运行")
		}
	})

	step(t, "ACT-HLT-02 只报不修：健康视图本身不改任何事项、规则或来源", func(t *testing.T) {
		rulesBefore := f.rulesText()
		srcBefore := f.apa(f.OBS, 0, "source", "show", "src_acc_fail").Stdout
		whyBefore := map[string]string{}
		for _, id := range []string{tripItem, driftItem, stalledChild, lagItem} {
			whyBefore[id] = userSteps(t, f.apa(f.OBS, 0, "why", id).Stdout)
		}
		seq0 := f.lastSeq()
		if seq0 == 0 {
			t.Fatal("读不到审计事件，安静期检查会是空检查")
		}

		// 10 分钟不做任何修复：测试时钟每次走 1 分钟，期间反复查看健康视图（CLI 与 Web 两端）。
		for i := 0; i < 10; i++ {
			f.TCAdvance(60)
			for j := 0; j < 3; j++ {
				f.health()
				f.webHealth()
				time.Sleep(500 * time.Millisecond)
			}
		}

		eq(t, "规则模式（GET /v1/rules）", f.rulesText(), rulesBefore)
		for id, before := range whyBefore {
			eq(t, "事项回放（apa why "+id+"）", userSteps(t, f.apa(f.OBS, 0, "why", id).Stdout), before)
		}
		var srcAfter, srcPrev struct {
			Source struct{ Enabled bool } `json:"source"`
		}
		_ = json.Unmarshal([]byte(f.apa(f.OBS, 0, "source", "show", "src_acc_fail").Stdout), &srcAfter)
		_ = json.Unmarshal([]byte(srcBefore), &srcPrev)
		eq(t, "来源启停状态（apa source show）", srcAfter.Source.Enabled, srcPrev.Source.Enabled)

		window := f.eventsAfter(seq0)
		t.Logf("安静期间共 %d 条 Event", len(window))
		for _, e := range window {
			if e.Actor.Type == "system" { // 内核后台（时钟、运行回执等）的 Event 不是健康视图发起的
				continue
			}
			switch e.Type {
			case "rule.mode_changed", "item.created", "item.updated", "item.phase_changed", "delegation.created",
				"source.updated", "source.enabled", "source.disabled", "source.registered", "policy.changed", "allowlist.changed":
				t.Errorf("安静期间出现修复类 Event: seq=%d type=%s object=%s actor=%+v", e.Seq, e.Type, e.Object, e.Actor)
			}
		}
		// 十三类问题仍在：没有任何一类被自动修好。
		for _, id := range allSignalIDs {
			if f.health().sig(id).OK {
				t.Errorf("信号 %s 在没有人修复的情况下变正常了（健康视图不得自行修复）", id)
			}
		}
	})
}

// eventsOf 取某对象某类型的审计事件（Web 审计页）。
func (f *fx) eventsOf(object, typ string) []auditEvent {
	f.t.Helper()
	return decode[struct {
		Events []auditEvent `json:"events"`
	}](f.t, f.webOK("GET", "/audit/events?limit=500&object="+object+"&type="+typ, "", http.StatusOK)).Events
}

// TestFlowHealthWarnings 对应 ACT-HLT-03..07：两个提醒级信号（长时间无回执的委派、时钟迟到）。
// 干净环境上只让这两项异常，healthy 不受影响；回执后、连续一小时无迟到后清除；恢复配置后 healthy=true。
// 时钟用 baseline 的测试时钟推进，按验收文档的时长走完（含 65 分钟）。
func TestFlowHealthWarnings(t *testing.T) {
	e := newBare(t, "delegation_stall_after: 2m\n")
	f := &fx{e, t}
	// 没有宿主在线：提醒走 Web，免得默认的宿主注入路由把到期提醒排成"投递积压"而混进本流程要看的两项。
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true}}`, http.StatusOK)
	var child, hl2 string

	step(t, "ACT-HLT-03 信号恒为十三项，顺序固定，前十一项与 v0.1 相同", func(t *testing.T) {
		h := f.health()
		var ids []string
		for _, s := range h.Signals {
			ids = append(ids, s.ID)
		}
		if strings.Join(ids, ",") != strings.Join(allSignalIDs, ",") {
			t.Fatalf("id 列表 = %v，期望 %v", ids, allSignalIDs)
		}
		if web := f.webHealth(); len(web.Signals) != 13 {
			t.Errorf("Web 也应恒为十三项: %d", len(web.Signals))
		}
		if !h.Healthy {
			t.Fatalf("干净环境应健康: %+v", h)
		}
	})

	step(t, "造出：长时间无回执的委派与时钟迟到", func(t *testing.T) {
		child = f.stall()
		f.waitFor("stalled_delegations 异常", 30*time.Second, func() bool { return !f.health().sig("stalled_delegations").OK })
		hl2 = f.lag()
		f.waitFor("clock_lag 异常", 30*time.Second, func() bool { return !f.health().sig("clock_lag").OK })
	})

	step(t, "ACT-HLT-03 后两项异常时 severity=warning 且 healthy=true", func(t *testing.T) {
		h := f.health()
		if !h.Healthy {
			t.Errorf("提醒级信号不应让 healthy 变为 false: %+v", h)
		}
		for _, id := range []string{"stalled_delegations", "clock_lag"} {
			if s := h.sig(id); s.OK || s.Severity != "warning" {
				t.Errorf("%s 应异常且 severity=warning: %+v", id, s)
			}
		}
		for _, id := range signalIDs {
			if !h.sig(id).OK {
				t.Errorf("其余信号应正常: %+v", h.sig(id))
			}
		}
	})

	step(t, "ACT-HLT-04 长时间无回执的委派进健康视图，有回执后清除", func(t *testing.T) {
		s := f.health().sig("stalled_delegations")
		if s.Count != 1 || s.OK || s.Severity != "warning" || !strings.Contains(s.Next, "delegations") {
			t.Fatalf("stalled_delegations 应 count=1、warning、修复入口指向任务委派页: %+v", s)
		}
		if !strings.Contains(string(s.Detail), child) && !strings.Contains(string(s.Detail), "HL-1") {
			// 明细以委派子事项或其父事项标出该委派
			parent := decode[struct {
				Relations []struct{ From, To string } `json:"relations"`
			}](t, f.apa(f.OBS, 0, "item", "show", child).Stdout)
			found := false
			for _, r := range parent.Relations {
				found = found || strings.Contains(string(s.Detail), r.From) || strings.Contains(string(s.Detail), r.To)
			}
			if !found {
				t.Errorf("明细应标出该委派: %s", s.Detail)
			}
		}
		f.apa(f.WRK, 0, "report", child, "--outcome", "progress", "--summary", "开工")
		f.waitFor("回执之后该信号恢复", 30*time.Second, func() bool { return f.health().sig("stalled_delegations").OK })
	})

	step(t, "ACT-HLT-05 时钟迟到进健康视图，补触发且 why 说明迟到", func(t *testing.T) {
		s := f.health().sig("clock_lag")
		if s.OK || s.Severity != "warning" || !f.health().Healthy {
			t.Fatalf("clock_lag 应异常、warning，且 healthy 不受影响: %+v", s)
		}
		if n := len(f.eventsOf(hl2, "item.deadline_passed")); n != 1 {
			t.Errorf("HL-2 的到期应补触发恰一次，实际 %d", n)
		}
		if why := f.apa(f.OBS, 0, "why", hl2).Stdout; !strings.Contains(why, "deadline_passed") || !strings.Contains(why, "health.alert") {
			t.Errorf("why 应有补触发的到期记录和时钟迟到的告警: %s", why)
		}
		alerts := decode[struct {
			Events []struct {
				Payload struct {
					Kind        string `json:"kind"`
					LateSeconds int64  `json:"late_seconds"`
				} `json:"payload"`
			} `json:"events"`
		}](t, f.webOK("GET", "/audit/events?limit=50&object="+hl2+"&type=health.alert", "", http.StatusOK))
		if len(alerts.Events) != 1 || alerts.Events[0].Payload.Kind != "clock_lag" || alerts.Events[0].Payload.LateSeconds < 300 {
			t.Errorf("审计里应有一条 clock_lag 告警，实际触发比应触发晚 5 分钟以上: %+v", alerts)
		}
	})

	step(t, "ACT-HLT-06 时钟迟到在连续一小时没有迟到后清除", func(t *testing.T) {
		// 65 分钟：期间每 10 分钟给一条事项设 1 分钟后到期；时钟每步 ≤ 4 分钟，使这些定时都准时触发，不再制造新的迟到。
		for round := 0; round < 7; round++ {
			id := f.task("HL-tick-"+string(rune('0'+round)), "x-plain")
			f.webOK("PATCH", "/items/"+id, `{"deadline":"`+f.kernelNow().Add(time.Minute).UTC().Format(time.RFC3339)+`"}`, http.StatusOK)
			for left := 10 * 60; left > 0; left -= 240 {
				f.TCAdvance(int64(min(left, 240)))
				time.Sleep(300 * time.Millisecond)
			}
			if round < 5 && f.health().sig("clock_lag").OK {
				t.Fatalf("第 %d 轮（%d 分钟）时不应已清除：迟到只在连续一小时没有新迟到后才清除", round, (round+1)*10)
			}
		}
		f.waitFor("65 分钟后 clock_lag 恢复", 30*time.Second, func() bool { return f.health().sig("clock_lag").OK })
	})

	step(t, "ACT-HLT-07 恢复健康用例改动", func(t *testing.T) {
		cfg := filepath.Join(f.Home, "config.yaml")
		b, err := os.ReadFile(cfg)
		if err != nil {
			t.Fatal(err)
		}
		writeFile(t, cfg, strings.Replace(string(b), "delegation_stall_after: 2m\n", "", 1))
		f.StopKernel()
		f.StartKernelAgain()
		f.WebLogin()
		if h := f.health(); !h.Healthy {
			t.Errorf("恢复后 healthy 应为 true: %+v", h)
		}
		if !f.health().sig("stalled_delegations").OK {
			t.Error("stalled_delegations 应正常")
		}
	})
}
