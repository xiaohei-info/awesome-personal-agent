//go:build e2e

package ops

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// script 在 Root/scripts 下写一个可执行脚本，返回路径。
func (f *fx) script(name, body string) string {
	f.t.Helper()
	dir := filepath.Join(f.Root, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// addScriptSource 在 Web 登记周期脚本来源（脚本来源只能在 Web 登记）。
func (f *fx) addScriptSource(id, command, schedule string, timeoutSec int) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "source_type": "acc", "command": command, "schedule": schedule, "timeout": timeoutSec})
	f.webOK("POST", "/sources", string(body), http.StatusCreated)
}

// webHealth 是 Web 健康页读的接口（GET /v1/health）。
func (f *fx) webHealth() healthView {
	f.t.Helper()
	return decode[healthView](f.t, f.webOK("GET", "/health", "", http.StatusOK))
}

// outcallDetail 是 outcall 信号的 detail。
type outcallDetail struct {
	Long, Short struct {
		Running int     `json:"running"`
		Queued  int     `json:"queued"`
		Longest float64 `json:"longest_wait_seconds"`
	}
	Timeouts []string `json:"timeouts"`
}

func (s signal) outcall(t *testing.T) outcallDetail {
	t.Helper()
	return decode[outcallDetail](t, string(s.Detail))
}

// the ten signal ids, in design §15.2 order.
var signalIDs = []string{"source_failures", "outcall", "delivery_backlog", "chain_trips", "agentsmd_drift", "pack_rejections", "model_unavailable", "unjudged_backlog", "rule_fallbacks", "receipt_failures", "other_alerts"}

// allSignalIDs 是健康视图恒有的十三项（§15.2、附录 B）：后两项是 v0.2.0 追加的提醒级信号，不影响 healthy，由 e2e-followup 造出异常。
var allSignalIDs = append(append([]string{}, signalIDs...), "stalled_delegations", "clock_lag")

// quietWindow 是 ACT-HLT-02 里"不做任何修复操作"的观察时长。验收文档为 10 分钟；默认按 30 秒缩放
// （合约命令不带 -timeout，go test 默认 10 分钟超时放不下 10 分钟窗口加造数），
// 设置 APA_E2E_HEALTH_QUIET=10m 并加 -timeout 30m 即按原文执行，窗口内占用型运行与来源的超时都长于 10 分钟。
func quietWindow(t *testing.T) time.Duration {
	if v := os.Getenv("APA_E2E_HEALTH_QUIET"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			t.Fatalf("APA_E2E_HEALTH_QUIET: %v", err)
		}
		return d
	}
	return 30 * time.Second // shortcut: go test 默认 10 分钟超时容不下完整 10 分钟窗口，见上方说明
}

// TestFlowHealth 对应验收文档 flow-health（ACT-HLT-01、ACT-HLT-02）。
// 长时运行额度与短任务额度都调到 1，让两个队列都能排出队来。
func TestFlowHealth(t *testing.T) {
	f := opsFx(t, "max_subprocesses: 1\nmax_short_subprocesses: 1\n")
	open := f.Projects["acc-open"]
	var driftItem, tripItem string

	step(t, "全新环境十三项信号都正常", func(t *testing.T) {
		h := f.health()
		if !h.Healthy || len(h.Signals) != len(allSignalIDs) {
			t.Fatalf("全新环境应健康且有十三项信号: %+v", h)
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

	step(t, "造出：回执与捕获失败", func(t *testing.T) {
		// 只读凭据提交捕获：内核可达但拒绝（退出码 4），同一事务记 health.alert。
		body, _ := json.Marshal(map[string]string{"text": "HLT 只读凭据的捕获", "project": f.Projects["acc-open"]})
		if r := f.APIRaw("POST", "/v1/capture", string(body), f.OBS); r.Code != http.StatusForbidden {
			t.Fatalf("只读凭据的捕获应被拒绝: %d %s", r.Code, r.Body)
		}
		f.waitFor("回执与捕获失败进入健康视图", 30*time.Second, func() bool { return !f.health().sig("receipt_failures").OK })
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

	var cli healthView
	step(t, "ACT-HLT-01 十一类信号都显示，两端一致，每类有修复入口", func(t *testing.T) {
		cli = f.health()
		web := f.webHealth()
		if cli.Healthy {
			t.Fatal("十一类问题都在时不应报健康")
		}
		if len(cli.Signals) != len(signalIDs)+2 { // 前十一项 + stalled_delegations、clock_lag(提醒级,另测)
			t.Fatalf("应恒有十三项信号(前十一类已造出): %+v", cli.Signals)
		}
		for i, id := range signalIDs {
			s := cli.Signals[i]
			if s.ID != id || s.OK || s.Count == 0 || s.Why == "" {
				t.Errorf("信号 %s 应显示为异常并带原因: %+v", id, s)
			}
			if strings.TrimSpace(s.Next) == "" {
				t.Errorf("信号 %s 没有修复入口", id)
			}
			// 两端一致：同一时刻 Web 与 CLI 读到相同的信号、计数与入口。
			if w := web.sig(id); w.OK != s.OK || w.Count != s.Count || w.Next != s.Next || w.Why != s.Why {
				t.Errorf("信号 %s 两端不一致: CLI %+v / Web %+v", id, s, w)
			}
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
		whyBefore := userSteps(t, f.apa(f.OBS, 0, "why", tripItem).Stdout)
		driftWhyBefore := userSteps(t, f.apa(f.OBS, 0, "why", driftItem).Stdout)
		start := time.Now()
		seq0 := f.lastSeq()

		window := quietWindow(t)
		for end := start.Add(window); time.Now().Before(end); time.Sleep(2 * time.Second) {
			f.health() // 反复查看健康视图（CLI 与 Web 两端）
			f.webHealth()
		}

		eq(t, "规则模式（GET /v1/rules）", f.rulesText(), rulesBefore)
		eq(t, "触发链路熔断的事项回放（apa why）", userSteps(t, f.apa(f.OBS, 0, "why", tripItem).Stdout), whyBefore)
		eq(t, "漂移事项回放（apa why）", userSteps(t, f.apa(f.OBS, 0, "why", driftItem).Stdout), driftWhyBefore)
		var srcAfter, srcPrev struct {
			Source struct{ Enabled bool } `json:"source"`
		}
		_ = json.Unmarshal([]byte(f.apa(f.OBS, 0, "source", "show", "src_acc_fail").Stdout), &srcAfter)
		_ = json.Unmarshal([]byte(srcBefore), &srcPrev)
		eq(t, "来源启停状态（apa source show）", srcAfter.Source.Enabled, srcPrev.Source.Enabled)

		// 审计页筛选这段时间的 Event：没有任何修复类 Event（规则切换、来源登记变更、事项改动、重新委派、能力包或策略变化）。
		if seq0 == 0 {
			t.Fatal("读不到审计事件，安静期检查会是空检查")
		}
		window0 := f.eventsAfter(seq0)
		t.Logf("安静期间共 %d 条 Event", len(window0))
		for _, e := range window0 {
			if e.Actor.Type == "system" { // 内核后台（时钟、运行回执等）的 Event 不是健康视图发起的
				continue
			}
			switch e.Type {
			case "rule.mode_changed", "item.created", "item.updated", "item.phase_changed", "delegation.created",
				"source.updated", "source.enabled", "source.disabled", "source.registered", "policy.changed", "allowlist.changed":
				t.Errorf("安静期间出现修复类 Event: seq=%d type=%s object=%s actor=%+v", e.Seq, e.Type, e.Object, e.Actor)
			}
		}
		// 十一类问题仍在：没有任何一类被自动修好。
		for _, id := range signalIDs {
			if f.health().sig(id).OK {
				t.Errorf("信号 %s 在没有人修复的情况下变正常了（健康视图不得自行修复）", id)
			}
		}
	})
}

// userSteps 去掉 apa why 回放里内核后台（system 操作者）的步骤：模型替身故障下的卡片摘要起草会在安静期内
// 异步追加 run.finished 与 health.alert，它们不是健康视图发起的（与下方审计事件检查同一口径）。
func userSteps(t *testing.T, out string) string {
	t.Helper()
	var r struct {
		Steps []struct {
			Actor struct{ Type string } `json:"actor"`
			Seq   int64                 `json:"seq"`
			Type  string                `json:"type"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("apa why 输出不是 JSON: %v\n%s", err, out)
	}
	var kept []string
	for _, s := range r.Steps {
		if s.Actor.Type != "system" {
			kept = append(kept, fmt.Sprintf("%d:%s", s.Seq, s.Type))
		}
	}
	return strings.Join(kept, ",")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rulePath(rule string) string { return strings.ReplaceAll(rulePrefix+rule, "/", "%2F") }

func (f *fx) rulesText() string {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/rules", "", f.OBS)
	if r.Code != http.StatusOK {
		f.t.Fatalf("GET /v1/rules: %d %s", r.Code, r.Body)
	}
	return r.Body
}

type auditEvent struct {
	Seq    int64                                 `json:"seq"`
	Type   string                                `json:"type"`
	Object string                                `json:"object"`
	Actor  struct{ Type, Principal, Via string } `json:"actor"`
}

// eventsAfter 取 seq 之后的全部审计事件（Web 审计页的数据源，按序号分页读完）。
func (f *fx) eventsAfter(seq int64) []auditEvent {
	f.t.Helper()
	var out []auditEvent
	for {
		page := decode[struct {
			Events []auditEvent `json:"events"`
		}](f.t, f.webOK("GET", "/audit/events?limit=500&after="+itoa(seq), "", http.StatusOK)).Events
		out = append(out, page...)
		if len(page) < 500 {
			return out
		}
		seq = page[len(page)-1].Seq
	}
}

func (f *fx) lastSeq() int64 {
	f.t.Helper()
	var m int64
	for _, e := range f.eventsAfter(0) {
		m = max(m, e.Seq)
	}
	return m
}
