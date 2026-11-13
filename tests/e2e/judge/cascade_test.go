//go:build e2e

package judge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func (f *fx) cls(subject string, p float64) {
	f.Cls.Judge(modelstub.ClsJudge{Key: key(subject), PYes: pf(p)})
}
func (f *fx) clsErr(subject, code string) {
	f.Cls.Judge(modelstub.ClsJudge{Key: key(subject), Error: code})
}
func (f *fx) chat(subject, answer string, sufficient bool) {
	f.ModelStub.Judge(modelstub.ChatJudge{Key: key(subject), Answer: answer, Sufficient: &sufficient})
}

// one 写一条事件并等它处置完，返回来源事件与它的判断记录。
func (f *fx) one(subject string, extra ...func(*ev)) (srcEvent, judgment) {
	f.t.Helper()
	e := ev{Subject: subject, Detail: subject + " 的正文", ID: subject}
	for _, x := range extra {
		x(&e)
	}
	return f.judgmentOf(f.write(e)[0])
}

// TestFlowAgentCascade 对应 flow-agent-cascade（ACT-CAS-01..26）。
func TestFlowAgentCascade(t *testing.T) {
	f := setup(t)
	f.installRelay()

	f.step(t, "ACT-CAS-01 判断型只写判断记录，不写 judge 类 Event，也不拉起子进程", func(t *testing.T) {
		f.reset()
		f.cls("cas-low", 0.05)
		e, j := f.one("cas-low")
		eq(t, "use", j.Use, "inbound_screen")
		eq(t, "decided_by", j.DecidedBy, "classifier")
		eq(t, "answer", j.Answer, "no")
		if n := len(f.runFinished("judge")); n != 0 {
			t.Errorf("不应有 category=judge 的 run.finished，实际 %d", n)
		}
		disposed := f.audit(e.ID, "inbound.disposed")
		if len(disposed) == 0 || disposed[0].Payload["judgment"] != j.ID {
			t.Errorf("inbound.disposed 载荷应带判断记录 %s: %+v", j.ID, disposed)
		}
		if rs := f.runs("--category", "task"); len(rs) != 0 {
			t.Errorf("判断型不应拉起子进程 Run: %+v", rs)
		}
		allowed := map[string]bool{"id": true, "use": true, "template": true, "subject": true, "source": true, "fingerprint": true, "band": true,
			"levels": true, "decided_by": true, "answer": true, "low_confidence": true, "shadow": true, "review": true, "created_at": true}
		for k := range f.rawJudgment(j.ID) {
			if !allowed[k] {
				t.Errorf("判断记录出现结构化字段之外的内容: %q", k)
			}
		}
	})

	f.step(t, "ACT-CAS-02 落入不确定带的题交对话端点重答同一题", func(t *testing.T) {
		f.reset()
		f.cls("cas-band", 0.40)
		f.chat("cas-band", "yes", true)
		_, j := f.one("cas-band", func(e *ev) { e.Detail = "cas-band 资料正文 UNIQUE-MATERIAL" })
		cb := parseCls(t, f.Cls.Batches()[0].BodyText)
		jr := f.judgeRequests()
		if len(jr) != 1 {
			t.Fatalf("MODEL_LOG 判断题请求 = %d，期望 1", len(jr))
		}
		q := cb.Questions[0]
		for _, must := range []string{q.Question, "cas-band", "UNIQUE-MATERIAL", "alice@example.invalid"} {
			if !strings.Contains(jr[0], jsonEsc(must)) {
				t.Errorf("对话端点的请求里缺少与分类端点一致的内容 %q", must)
			}
		}
		if strings.Contains(jr[0], "0.4") || strings.Contains(jr[0], "p_yes") {
			t.Errorf("对话端点的请求里不应带上一级的答案: %s", jr[0])
		}
		eq(t, "levels[0].outcome", j.Levels[0].Outcome, "uncertain")
		eq(t, "levels[1].backend", j.Levels[1].Backend, "chat")
		eq(t, "decided_by", j.DecidedBy, "chat")
		eq(t, "band.low", j.Band.Low, 0.15)
		eq(t, "band.high", j.Band.High, 0.60)
	})

	var relayItem string
	relay := func(title string) string {
		id := f.triggerItem(title, "acc-relay")
		return id
	}
	f.step(t, "ACT-CAS-03 生成型信息不足且声明了 relay_to 时接力到任务型", func(t *testing.T) {
		f.reset()
		f.mode("receipt")
		f.ModelStub.When("ACCRELAYGEN", reply(`{"insufficient_info":true}`))
		relayItem = relay("cas03")
		f.waitFor("接力的任务型 Run 结束", 60*time.Second, func() bool {
			rs := f.runs("--category", "task")
			return len(rs) == 1 && rs[0].State != "running" && rs[0].State != "queued"
		})
		gens := f.runFinished("generate")
		if len(gens) == 0 {
			t.Fatal("没有 category=generate 的 run.finished")
		}
		if js := f.judgmentsOf(relayItem); len(js) != 0 {
			t.Errorf("接力不应记成同题升级的判断记录: %+v", js)
		}
	})

	f.step(t, "ACT-CAS-04 任务型失败交本人，不重试", func(t *testing.T) {
		f.reset()
		f.mode("exit1")
		before := len(f.runs("--category", "task"))
		f.ModelStub.When("ACCRELAYGEN", reply(`{"insufficient_info":true}`))
		relay("cas04")
		f.waitFor("第二个任务型 Run 失败", 60*time.Second, func() bool {
			rs := f.runs("--category", "task")
			return len(rs) == before+1 && rs[len(rs)-1].State == "failed"
		})
		time.Sleep(3 * time.Second) // 不重试：再等一会儿，Run 数不增加
		rs := f.runs("--category", "task")
		if len(rs) != before+1 {
			t.Fatalf("任务型失败后不应重试，Run 数 %d → %d", before, len(rs))
		}
		last := rs[len(rs)-1]
		eq(t, "Run state", last.State, "failed")
		target := f.item(last.Target)
		eq(t, "目标事项阶段", target["phase"], "needs_input")
		opts := mustJSON(target["ask"])
		for _, want := range []string{"重新运行", "改派", "放弃"} {
			if !strings.Contains(opts, want) {
				t.Errorf("needs_input 的选项缺少 %q: %s", want, opts)
			}
		}
	})

	f.step(t, "ACT-CAS-05 生成型输出不符合 schema 视为失败", func(t *testing.T) {
		f.reset()
		f.mode("receipt")
		f.ModelStub.When("ACCRELAYGEN", reply(`{"note":"缺必填字段"}`))
		before := len(f.runFinished("generate"))
		id := relay("cas05")
		f.waitFor("生成型调用结束", 60*time.Second, func() bool { return len(f.runFinished("generate")) > before })
		last := f.runFinished("generate")[0]
		for _, e := range f.runFinished("generate") {
			if e.Object == id {
				last = e
			}
		}
		eq(t, "run.finished.state", last.Payload["state"], "failed")
		// 对本人已有事项的事件规则：失败只记 run.finished（failed）和 health.alert，事项阶段不变（§8.7）；否则转 needs_input。
		if p := f.phase(id); p == "open" && len(f.audit(id, "health.alert")) == 0 {
			t.Errorf("事项停在 open 而没有 health.alert，等于静默失败")
		}
		if spec := mustJSON(f.item(id)["spec"]); strings.Contains(spec, "缺必填字段") {
			t.Errorf("不合规内容进入了 spec: %s", spec)
		}
	})

	f.step(t, "ACT-CAS-06 stdout 末段兜底回执", func(t *testing.T) {
		f.reset()
		f.ModelStub.When("ACCRELAYGEN", reply(`{"insufficient_info":true}`))
		f.mode("summary-only")
		before := len(f.runs("--category", "task"))
		f.triggerItem("cas06a", "acc-sum")
		f.waitFor("第一次运行结束", 60*time.Second, func() bool {
			rs := f.runs("--category", "task")
			return len(rs) == before+1 && rs[len(rs)-1].State != "running" && rs[len(rs)-1].State != "queued"
		})
		first := f.runs("--category", "task")[before]
		if p := f.phase(first.Target); p != "done" {
			t.Errorf("兜底回执应按末段写回，事项阶段 = %s", p)
		}
		f.mode("no-receipt")
		f.triggerItem("cas06b", "acc-sum")
		f.waitFor("第二次运行结束", 60*time.Second, func() bool {
			rs := f.runs("--category", "task")
			return len(rs) == before+2 && rs[len(rs)-1].State != "running" && rs[len(rs)-1].State != "queued"
		})
		second := f.runs("--category", "task")[before+1]
		it := f.item(second.Target)
		eq(t, "都缺时事项阶段", it["phase"], "needs_input")
		if !strings.Contains(mustJSON(it["ask"]), "没有用运行凭据写过账本") || !strings.Contains(mustJSON(it["ask"]), "stdout") {
			t.Errorf("needs_input 应写明这次运行实际留下了什么: %s", mustJSON(it["ask"]))
		}
		if n := len(f.audit(first.Target, "item.transitioned")); n > 2 {
			t.Logf("事项转移事件 %d 条", n)
		}
	})

	f.step(t, "ACT-CAS-07 对话端点上同一输入复用结果", func(t *testing.T) {
		f.reset()
		f.webOK("PUT", "/settings/model-endpoints", `{"judge_endpoint": null}`, 200)
		defer f.WireEndpoints()
		f.chat("cas-reuse", "yes", true)
		same := ev{Subject: "cas-reuse", Detail: "完全相同的内容", Sender: "alice@example.invalid"}
		a := f.write(withID(same, "cas-reuse-1"))[0]
		f.judgmentOf(a)
		b := f.write(withID(same, "cas-reuse-2"))[0]
		_, jb := f.judgmentOf(b)
		if n := len(f.judgeRequests()); n != 1 {
			t.Errorf("MODEL_LOG 判断题请求 = %d，期望 1", n)
		}
		reused := false
		for _, l := range jb.Levels {
			reused = reused || l.Reused
		}
		if !reused {
			t.Errorf("第二条判断记录应标 reused: %+v", jb)
		}
	})

	f.step(t, "ACT-CAS-08 Event 统计随题目送出，且不进指纹", func(t *testing.T) {
		f.reset()
		f.cls("cas-st", 0.9) // 高于上限：进入建议流
		for i := 1; i <= 6; i++ {
			e, _ := f.one(fmt.Sprintf("cas-st-%d", i))
			if e.Item == "" {
				t.Fatalf("cas-st-%d 没有进入建议流: %+v", i, e)
			}
			switch {
			case i <= 2:
				f.webOK("POST", "/items/"+e.Item+"/accept", "{}", 200)
			case i <= 5:
				f.webOK("POST", "/items/"+e.Item+"/dismiss", `{"reason":"cas-stats"}`, 200)
			}
		}
		// 规则自动接受的 note 不计入：由验收包的 auto-accept-note 规则处理。
		f.ModelStub.When("为这条来源事件起草一条建议", reply(`{"title":"cas-auto","kind":"note"}`))
		f.cls("cas-st-auto", 0.9)
		f.one("cas-st-auto")
		f.Cls.Reset()
		f.cls("cas-stats", 0.05)
		same := ev{Subject: "cas-stats", Detail: "统计用", Sender: "alice@example.invalid"}
		first := f.write(withID(same, "cas-stats-1"))[0]
		f.judgmentOf(first)
		var seen []map[string]any
		for _, b := range f.Cls.Batches() {
			for _, q := range parseCls(t, b.BodyText).Questions {
				if q.Input["subject"] == "cas-stats" {
					st, _ := q.Input["stats"].(map[string]any)
					seen = append(seen, st)
				}
			}
		}
		if len(seen) != 1 || seen[0]["accepted"] != 2.0 || seen[0]["dismissed"] != 3.0 {
			t.Fatalf("input.stats 应为 {accepted:2, dismissed:3}（规则自动接受不计入），实际 %v", seen)
		}
		e6, _ := f.srcEvent("cas-st-6")
		f.webOK("POST", "/items/"+e6.Item+"/accept", "{}", 200)
		second := f.write(withID(same, "cas-stats-2"))[0]
		_, j2 := f.judgmentOf(second)
		if !j2.Levels[0].Reused {
			t.Errorf("统计变化不应触发重判，第二条应标 reused: %+v", j2)
		}
		if n := len(f.Cls.Batches()); n != 1 {
			t.Errorf("统计变化后不应再外呼，CLS_LOG 批次 = %d", n)
		}
	})

	f.step(t, "ACT-CAS-09 生成端点失败时退避重试且可见", func(t *testing.T) {
		f.reset()
		f.ModelStub.SetDefault(modelstub.Reply{Status: 500})
		f.cls("cas-gen-fail", 0.9)
		e, _ := f.one("cas-gen-fail")
		if got := len(f.genRequests()); got < 2 {
			t.Errorf("起草请求应按退避重试，MODEL_LOG 生成请求 = %d", got)
		}
		_, d := f.modelHealth()
		eq(t, "model_unavailable.detail.generate", d["generate"], "failing")
		if e.Item == "" || f.proposals()[e.Item] != "cas-gen-fail" {
			t.Errorf("起草失败不挡路，建议应进入建议流且标题取来源 subject: item=%q proposals=%v", e.Item, f.proposals())
		}
		f.reset()
		f.cls("cas-gen-ok", 0.9)
		f.one("cas-gen-ok")
		if _, d := f.modelHealth(); d["generate"] != nil && d["generate"] != "" {
			t.Errorf("恢复后 detail.generate = %v，期望信号消失", d["generate"])
		}
	})

	f.step(t, "ACT-CAS-10 harness 不可用时账本照常", func(t *testing.T) {
		f.reset()
		f.webOK("PUT", "/settings/policy", `{"default_harness":""}`, 200)
		defer f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harnessName+`"}`, 200)
		f.ModelStub.When("ACCRELAYGEN", reply(`{"insufficient_info":true}`))
		id := relay("cas10")
		_ = id
		f.waitFor("接力终止于 needs_input", 20*time.Second, func() bool { return f.hasPhase("cas10", "needs_input") })
		manual := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "cas10-ledger", "--phase", "open", "--project", f.Projects["acc-open"], "--evidence", "账本照常")
		mid := jsonMap(t, manual.Stdout)["id"].(string)
		if r := f.Web("GET", "/v1/items/"+mid, ""); r.Code != 200 {
			t.Errorf("Web 读事项 HTTP %d", r.Code)
		}
		f.reset()
		f.cls("cas10-judge", 0.05)
		_, j := f.one("cas10-judge")
		eq(t, "判断型照常", j.DecidedBy, "classifier")
	})

	const draftMark = "为这条来源事件起草一条建议"

	f.step(t, "ACT-CAS-11 概率低于下限按否定案，不再问对话端点", func(t *testing.T) {
		f.reset()
		f.cls("cas-below", 0.10)
		e, j := f.one("cas-below")
		if n := len(f.judgeRequests()); n != 0 {
			t.Errorf("MODEL_LOG 不应有这道题: %d", n)
		}
		eq(t, "decided_by", j.DecidedBy, "classifier")
		eq(t, "answer", j.Answer, "no")
		eq(t, "处置", e.Disposition, "ignored")
		if !strings.Contains(f.APIRaw("GET", "/v1/judgments/spot-check?limit=500", "", f.OBS).Body, j.ID) {
			t.Errorf("被忽略的事件应进入抽检池")
		}
	})

	f.step(t, "ACT-CAS-12 概率高于上限按是定案，进入起草", func(t *testing.T) {
		f.reset()
		f.cls("cas-above", 0.85)
		e, j := f.one("cas-above")
		if n := len(f.judgeRequests()); n != 0 {
			t.Errorf("MODEL_LOG 不应有判断题请求: %d", n)
		}
		drafts := 0
		for _, g := range f.genRequests() {
			if strings.Contains(g, draftMark) {
				drafts++
			}
		}
		eq(t, "起草请求数", drafts, 1)
		eq(t, "decided_by", j.DecidedBy, "classifier")
		eq(t, "answer", j.Answer, "yes")
		if _, ok := f.proposals()[e.Item]; !ok {
			t.Errorf("建议应进入建议流: %+v", e)
		}
	})

	f.step(t, "ACT-CAS-13 对话端点回答否且资料足够时按忽略定案", func(t *testing.T) {
		f.reset()
		f.cls("cas-chat-no", 0.40)
		f.chat("cas-chat-no", "no", true)
		e, j := f.one("cas-chat-no")
		eq(t, "decided_by", j.DecidedBy, "chat")
		eq(t, "answer", j.Answer, "no")
		eq(t, "处置", e.Disposition, "ignored")
		eq(t, "low_confidence", j.LowConfidence, false)
		if !strings.Contains(f.APIRaw("GET", "/v1/judgments/spot-check?limit=500", "", f.OBS).Body, j.ID) {
			t.Errorf("被忽略的事件应进入抽检池")
		}
	})

	lowConfidence := func(t *testing.T, subject string, e srcEvent, j judgment) {
		t.Helper()
		eq(t, subject+" decided_by", j.DecidedBy, "default")
		eq(t, subject+" answer", j.Answer, "yes")
		eq(t, subject+" low_confidence", j.LowConfidence, true)
		if e.Disposition != "proposed" || e.Item == "" {
			t.Errorf("%s 应留为建议: %+v", subject, e)
			return
		}
		if !strings.Contains(mustJSON(f.item(e.Item)), `"low_confidence":true`) {
			t.Errorf("%s 的建议应带 proposal.low_confidence: %s", subject, mustJSON(f.item(e.Item)))
		}
	}
	f.step(t, "ACT-CAS-14 对话端点自报资料不足时取保守默认，留为建议并标把握低", func(t *testing.T) {
		f.reset()
		f.cls("cas-insufficient", 0.40)
		f.chat("cas-insufficient", "no", false)
		e, j := f.one("cas-insufficient")
		lowConfidence(t, "cas-insufficient", e, j)
	})

	f.step(t, "ACT-CAS-15 对话端点输出不合 schema 视为未定", func(t *testing.T) {
		f.reset()
		f.cls("cas-badjson", 0.40)
		f.ModelStub.Judge(modelstub.ChatJudge{Key: key("cas-badjson"), Raw: `{"verdict": "maybe"}`})
		e, j := f.one("cas-badjson")
		if o := j.Levels[1].Outcome; o != "error" && o != "uncertain" {
			t.Errorf("levels[1].outcome = %q，期望 error 或 uncertain", o)
		}
		lowConfidence(t, "cas-badjson", e, j)
	})

	f.step(t, "ACT-CAS-16 对话端点不可用时入站事件记未判定，不放进建议流", func(t *testing.T) {
		f.reset()
		f.Cls.SetFault(modelstub.ClsFault{Status: 503})
		f.ModelStub.Judge(modelstub.ChatJudge{Key: key("cas-both-down"), Status: 503})
		id := f.write(ev{Subject: "cas-both-down", ID: "cas-both-down"})[0]
		down := f.settled(id, 90*time.Second)
		eq(t, "故障期间处置", down.Disposition, "unjudged")
		eq(t, "原因", down.Unjudged, "model_unavailable")
		if _, in := f.proposals()[down.Item]; in && down.Item != "" {
			t.Error("故障期间事件不应在建议流里")
		}
		f.reset()
		f.cls("cas-both-down", 0.05)
		f.TCAdvance(600) // 退避至多 10 分钟：把测试时钟拨过去，等后台自动重判
		f.waitFor("恢复后自动重判", 120*time.Second, func() bool {
			e, _ := f.srcEvent(id)
			return e.Disposition == "ignored"
		})
	})

	f.step(t, "ACT-CAS-17 对话端点自报的置信度不被读取", func(t *testing.T) {
		f.reset()
		f.cls("cas-selfconf", 0.40)
		f.ModelStub.Judge(modelstub.ChatJudge{Key: key("cas-selfconf"), Raw: `{"answer": "no", "sufficient": false, "confidence": 0.99}`})
		e, j := f.one("cas-selfconf")
		jr := f.judgeRequests()
		if len(jr) != 1 {
			t.Fatalf("判断题请求 = %d，期望 1", len(jr))
		}
		if strings.Contains(strings.ToLower(jr[0]), "confidence") {
			t.Errorf("输出 schema 不应含置信度字段: %s", jr[0])
		}
		if !contains(jr[0], "answer", "sufficient") {
			t.Errorf("输出 schema 应有 answer 与 sufficient: %s", jr[0])
		}
		lowConfidence(t, "cas-selfconf", e, j)
		if strings.Contains(mustJSON(f.rawJudgment(j.ID)), "0.99") {
			t.Error("判断记录里不应出现 0.99")
		}
	})

	f.step(t, "ACT-CAS-18 single_shot 模板不升级，落入带内即取保守默认", func(t *testing.T) {
		f.reset()
		f.setJudge(map[string]any{"timeout_ms": 1000})
		defer f.setJudge(nil)
		f.cls("ACCSS", 0.5)
		a := f.triggerItem("cas18a", "acc-ss")
		var ja []judgment
		f.waitFor("single_shot 判断记录", 5*time.Second, func() bool { ja = f.judgmentsOf(a); return len(ja) > 0 })
		eq(t, "①decided_by", ja[0].DecidedBy, "default")
		eq(t, "①answer", ja[0].Answer, "no")
		f.reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: key("ACCSS"), PYes: pf(0.5), DelayMS: 3000})
		b := f.triggerItem("cas18b", "acc-ss")
		var jb []judgment
		f.waitFor("超时那条的判断记录", 30*time.Second, func() bool { jb = f.judgmentsOf(b); return len(jb) > 0 })
		eq(t, "②decided_by", jb[0].DecidedBy, "default")
		eq(t, "②answer", jb[0].Answer, "no")
		if n := len(f.Cls.Batches()); n != 1 {
			t.Errorf("超时不重试，CLS_LOG 请求 = %d，期望 1", n)
		}
		if n := len(f.judgeRequests()); n != 0 {
			t.Errorf("single_shot 不应交给对话端点: %d", n)
		}
	})

	f.step(t, "ACT-CAS-19 生成型信息不足而没有 relay_to 时取该用途的默认处置", func(t *testing.T) {
		f.reset()
		before := len(f.runs("--category", "task"))
		f.ModelStub.When(draftMark, reply(`{"insufficient_info":true}`))
		f.cls("cas-noreplay", 0.9)
		e, _ := f.one("cas-noreplay")
		if n := len(f.runs("--category", "task")); n != before {
			t.Errorf("不应拉起任务型：Run %d → %d", before, n)
		}
		if f.proposals()[e.Item] != "cas-noreplay" {
			t.Errorf("建议仍应进入建议流，标题取来源 subject: %+v %v", e, f.proposals())
		}
	})

	f.step(t, "ACT-CAS-20 接力到任务型时含未进转发白名单的挂靠消息则转交本人", func(t *testing.T) {
		f.reset()
		f.mode("receipt")
		f.ModelStub.When("ACCRELAYGEN", reply(`{"insufficient_info":true}`))
		const pr = "https://example.invalid/acc/test-repo/pull/20"
		parent := jsonMap(t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "cas20-parent", "--phase", "open",
			"--project", f.Projects["acc-open"], "--evidence", "验收父事项").Stdout)["id"].(string)
		child := jsonMap(t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", "cas20-child", "--evidence", "委派").Stdout)["id"].(string)
		f.apa(f.WRK, 0, "report", child, "--outcome", "progress", "--summary", "已开 PR", "--pr", pr)
		raw, _ := json.Marshal(map[string]any{"id": "cas20-msg", "type": "acc.comment", "subject": "cas20 后续消息", "detail": "来自白名单之外的发送者",
			"sender": "stranger@elsewhere.invalid", "correlation_keys": []string{pr}, "occurred_at": "2030-01-01T00:00:00Z"})
		appendLine(t, f.events, string(raw))
		f.waitFor("挂靠消息落盘", 60*time.Second, func() bool { e, ok := f.srcEvent("cas20-msg"); return ok && e.Disposition == "attached" })
		before := len(f.runs("--category", "task"))
		f.apa(f.DEL, 0, "item", "update", child, "--tags", "acc-relay-open", "--reason", "触发接力", "--evidence", "验收")
		f.apa(f.DEL, 0, "item", "transition", child, "--to", "waiting", "--waiting-on", "external:cas20", "--reason", "go", "--evidence", "验收")
		f.apa(f.DEL, 0, "item", "transition", child, "--to", "open", "--reason", "go", "--evidence", "触发接力")
		// 接力另建目标事项（与触发事项同名），它转 needs_input 交本人。
		f.waitFor("接力转交本人", 60*time.Second, func() bool { return f.hasPhase("cas20-child", "needs_input") })
		if n := len(f.runs("--category", "task")); n != before {
			t.Errorf("不应拉起任务型：Run %d → %d", before, n)
		}
		if !strings.Contains(f.apa(f.OBS, 0, "item", "list", "--phase", "needs_input").Stdout, "白名单") {
			t.Error("needs_input 应写明原因是存在未进白名单的挂靠消息")
		}
	})

	f.step(t, "ACT-CAS-21 内置判断模板的不确定带起始值", func(t *testing.T) {
		type want struct {
			file      string
			low, high string
		}
		for _, w := range []want{
			{"packs/inbound-triage/prompts/triage-screen.md", "0.15", "0.60"},
			{"packs/inbound-triage/prompts/capture-prescreen.md", "0.10", ""},
			{"packs/decision-card/prompts/web-only-screening.md", "0.20", ""},
			{"packs/decision-card/prompts/delivery-defer.md", "", "0.85"},
			{"packs/follow-up/prompts/follow-up-defer.md", "", "0.85"},
			{"packs/delegation-verify/prompts/delegation-screen.md", "0.20", ""},
		} {
			fm := repoFile(t, w.file)
			band := ""
			for _, line := range strings.Split(fm, "\n") {
				if strings.HasPrefix(line, "band:") {
					band = line
				}
			}
			if w.low != "" && !strings.Contains(band, "low: "+w.low) || w.low == "" && strings.Contains(band, "low:") {
				t.Errorf("%s 的下限与 §8.9 不符: %q", w.file, band)
			}
			if w.high != "" && !strings.Contains(band, "high: "+w.high) || w.high == "" && strings.Contains(band, "high:") {
				t.Errorf("%s 的上限与 §8.9 不符: %q", w.file, band)
			}
		}
		// GET /packs 列出的包都在（实际路径以它为准）。
		for _, name := range []string{"inbound-triage", "decision-card", "follow-up", "delegation-verify"} {
			if !strings.Contains(f.APIRaw("GET", "/v1/packs", "", f.OBS).Body, name) {
				t.Errorf("GET /packs 没有列出 %s", name)
			}
		}
	})

	const screenQ = "这份回执需要本人核实吗"
	pair := func(name string) (parent, child string) {
		r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", name+"-parent", "--phase", "open", "--project", f.Projects["acc-open"], "--evidence", "验收父事项")
		parent = jsonMap(f.t, r.Stdout)["id"].(string)
		d := f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", name+"-child")
		child = jsonMap(f.t, d.Stdout)["id"].(string)
		return
	}
	report := func(child string) {
		f.apa(f.WRK, 0, "report", child, "--outcome", "done", "--summary", "已完成", "--link", "https://example.invalid/pr/1")
	}
	// verifyOutcome 等回执核实门控给出结论：返回判断记录（没有则 nil）与父事项上的核实决策请求。
	verified := func(parent, child string) (*judgment, bool) {
		var j *judgment
		f.WaitFor(20*time.Second, func() bool {
			js := f.judgmentsOf(child)
			if len(js) > 0 {
				j = &js[len(js)-1]
				return true
			}
			return false
		})
		time.Sleep(1500 * time.Millisecond)
		return j, f.item(parent)["ask"] != nil
	}

	f.step(t, "ACT-CAS-22 准备回执核实门控", func(t *testing.T) {
		f.webOK("PATCH", "/rules/"+url.PathEscape("delegation-verify/event_rules/verify-report")+"/mode", `{"mode":"auto"}`, 200)
		if !strings.Contains(f.webOK("GET", "/audit/events?limit=50&type=rule.mode_changed", "", 200), "verify-report") {
			t.Error("没有对应的 rule.mode_changed Event")
		}
	})

	f.step(t, "ACT-CAS-23 回执核实门控低于下限时不打扰本人", func(t *testing.T) {
		f.reset()
		f.cls(screenQ, 0.05)
		parent, child := pair("cas23")
		report(child)
		j, ask := verified(parent, child)
		if j == nil {
			t.Fatal("没有判断记录")
		}
		eq(t, "use", j.Use, "verify_gate")
		eq(t, "answer", j.Answer, "no")
		eq(t, "decided_by", j.DecidedBy, "classifier")
		if ask {
			t.Error("父事项上不应有核实决策请求")
		}
	})

	f.step(t, "ACT-CAS-24 回执核实门控高于下限时交本人核实", func(t *testing.T) {
		f.reset()
		f.cls(screenQ, 0.5)
		before := len(f.runs("--category", "task"))
		parent, child := pair("cas24")
		report(child)
		j, ask := verified(parent, child)
		if j == nil {
			t.Fatal("没有判断记录")
		}
		eq(t, "answer", j.Answer, "yes")
		if !ask {
			t.Error("父事项上应生成核实决策请求")
		}
		if n := len(f.judgeRequests()); n != 0 {
			t.Errorf("MODEL_LOG 不应有真假判断类请求: %d", n)
		}
		if n := len(f.runs("--category", "task")); n != before {
			t.Errorf("不应拉起任务型查证: %d → %d", before, n)
		}
	})

	f.step(t, "ACT-CAS-25 回执核实门控各级未定时交本人核实", func(t *testing.T) {
		f.reset()
		f.clsErr(screenQ, "internal")
		f.chat(screenQ, "no", false)
		parent, child := pair("cas25")
		report(child)
		j, ask := verified(parent, child)
		if j == nil {
			t.Fatal("没有判断记录")
		}
		eq(t, "decided_by", j.DecidedBy, "default")
		eq(t, "answer", j.Answer, "yes")
		if !ask {
			t.Error("各级未定应按保守默认交本人核实")
		}
	})

	f.step(t, "ACT-CAS-26 两个端点都不可用时门控不运行，回执标未核验", func(t *testing.T) {
		f.reset()
		f.webOK("PUT", "/settings/model-endpoints", `{"judge_endpoint": null, "generate_endpoint": null}`, 200)
		defer f.WireEndpoints()
		parent, child := pair("cas26")
		report(child)
		time.Sleep(3 * time.Second)
		if js := f.judgmentsOf(child); len(js) != 0 {
			t.Errorf("不应有判断记录: %+v", js)
		}
		if f.item(parent)["ask"] != nil {
			t.Error("不应有核实决策请求")
		}
		c := f.item(child)
		eq(t, "子事项阶段", c["phase"], "done")
		if !strings.Contains(mustJSON(c), "未核验") && !strings.Contains(mustJSON(f.audit(child, "")), "unverified") {
			t.Errorf("回执应标未核验: %s", mustJSON(c))
		}
		f.webOK("PATCH", "/rules/"+url.PathEscape("delegation-verify/event_rules/verify-report")+"/mode", `{"mode":"observe"}`, 200)
	})
}

func jsonEsc(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

func appendLine(t *testing.T, path, line string) {
	t.Helper()
	fh, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	must(t, err)
	defer fh.Close()
	_, err = fh.WriteString(line + "\n")
	must(t, err)
}
