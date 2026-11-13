//go:build e2e

package judge

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func key(s string) modelstub.Key { return modelstub.Key{Contains: s} }

// chatSays 让对话端点对含 s 的判断题回答 answer（资料足够）。
func (f *fx) chatSays(s, answer string) {
	f.ModelStub.Judge(modelstub.ChatJudge{Key: key(s), Answer: answer})
}

// TestFlowJudgeProtocol 对应 flow-judge-protocol（ACT-JDP-02..13）：真实 apa 二进制 + CLS_STUB / MODEL_STUB，
// 事件经 ACC_TEXT 来源走真实入站流水线，按 triage-screen 模板的判断题与分类端点交互。
func TestFlowJudgeProtocol(t *testing.T) {
	f := setup(t)

	f.step(t, "ACT-JDP-02 一批是非题按协议整批提交，逐题回 p_yes", func(t *testing.T) {
		f.reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-batch"), PYes: pf(0.05)})
		var evs []ev
		for i := 1; i <= 40; i++ {
			evs = append(evs, ev{Subject: fmt.Sprintf("jdp-batch-%02d", i), Detail: "批量题", ID: fmt.Sprintf("jdp-batch-%02d", i)})
		}
		ids := f.write(evs...)
		for _, id := range ids {
			f.settled(id, 90*time.Second)
		}
		var sizes []int
		seen := map[string]bool{}
		for _, b := range f.Cls.Batches() {
			r := parseCls(t, b.BodyText)
			sizes = append(sizes, len(r.Questions))
			eq(t, "protocol", r.Protocol, "probability-judgment/1")
			if r.RequestID == "" {
				t.Error("request_id 为空")
			}
			ids := map[string]bool{}
			for _, q := range r.Questions {
				if q.ID == "" || ids[q.ID] {
					t.Errorf("题号缺失或批内重复: %q", q.ID)
				}
				ids[q.ID] = true
				if q.Question == "" || q.Input == nil || q.Material == "" {
					t.Errorf("题缺少 question/input/material: %+v", q)
				}
				seen[q.Input["subject"].(string)] = true
			}
		}
		if len(sizes) != 2 || !(sizes[0] == 32 && sizes[1] == 8 || sizes[0] == 8 && sizes[1] == 32) {
			t.Fatalf("40 题应分成 32 与 8 两批，实际批次题数 %v", sizes)
		}
		if len(seen) != 40 {
			t.Errorf("两批合计应覆盖 40 个不同的题，实际 %d", len(seen))
		}
		e, j := f.judgmentOf("jdp-batch-17")
		l := j.Levels[0]
		eq(t, "backend", l.Backend, "classifier")
		eq(t, "outcome", l.Outcome, "decided")
		eq(t, "probability_source", l.ProbabilitySource, "native")
		eq(t, "model", l.Model, "cls-stub")
		if l.PYes == nil || *l.PYes != 0.05 {
			t.Errorf("levels[0].p_yes = %v，期望 0.05", l.PYes)
		}
		eq(t, "decided_by", j.DecidedBy, "classifier")
		why := f.apa(f.OBS, 0, "why", e.ID).Stdout
		if !strings.Contains(why, j.ID) {
			t.Errorf("apa why 没有指向判断记录 %s:\n%s", j.ID, why)
		}
	})

	f.step(t, "ACT-JDP-03 单题错误只让该题交对话端点，同批其他题照常定案", func(t *testing.T) {
		f.reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-q-ok"), PYes: pf(0.05)})
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-q-big"), Error: "input_too_large"})
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-q-unsup"), Error: "unsupported_question"})
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-q-int"), Error: "internal"})
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-q-miss"), Omit: true})
		for _, s := range []string{"jdp-q-big", "jdp-q-unsup", "jdp-q-int", "jdp-q-miss"} {
			f.chatSays(s, "no")
		}
		subjects := []string{"jdp-q-ok", "jdp-q-big", "jdp-q-unsup", "jdp-q-int", "jdp-q-miss"}
		var evs []ev
		for _, s := range subjects {
			evs = append(evs, ev{Subject: s, ID: s})
		}
		for _, id := range f.write(evs...) {
			f.settled(id, 60*time.Second)
		}
		bs := f.Cls.Batches()
		if len(bs) != 1 || bs[0].Questions != 5 {
			t.Fatalf("CLS_LOG 应只有 1 批 5 题且没有重发，实际 %d 批 %+v", len(bs), bs)
		}
		if n := len(f.judgeRequests()); n != 4 {
			t.Errorf("MODEL_LOG 判断题请求 = %d，期望 4", n)
		}
		_, ok := f.judgmentOf("jdp-q-ok")
		eq(t, "jdp-q-ok decided_by", ok.DecidedBy, "classifier")
		for s, code := range map[string]string{"jdp-q-big": "input_too_large", "jdp-q-unsup": "unsupported_question", "jdp-q-int": "internal", "jdp-q-miss": "missing_result"} {
			_, j := f.judgmentOf(s)
			if len(j.Levels) < 2 {
				t.Errorf("%s 应有两级记录: %+v", s, j)
				continue
			}
			eq(t, s+" levels[0].outcome", j.Levels[0].Outcome, "error")
			eq(t, s+" levels[0].error_code", j.Levels[0].ErrorCode, code)
			eq(t, s+" levels[1].backend", j.Levels[1].Backend, "chat")
			eq(t, s+" decided_by", j.DecidedBy, "chat")
		}
	})

	f.step(t, "ACT-JDP-04 整批 400 与协议错误不重试，整批交对话端点并进健康视图", func(t *testing.T) {
		cases := []struct {
			name, code string
			fault      modelstub.ClsFault
		}{
			{"jdp-400", "invalid_request", modelstub.ClsFault{Status: 400}},
			{"jdp-proto", "protocol_error", modelstub.ClsFault{Protocol: "probability-judgment/9"}},
			{"jdp-range", "protocol_error", modelstub.ClsFault{Raw: `{"protocol":"probability-judgment/1","model":"m","probability_source":"native","results":[{"id":"q1","p_yes":1.5},{"id":"q2","p_yes":1.5},{"id":"q3","p_yes":1.5}]}`}},
		}
		for _, c := range cases {
			f.reset()
			f.Cls.SetFault(c.fault)
			f.chatSays(c.name, "no")
			var evs []ev
			for i := 1; i <= 3; i++ {
				evs = append(evs, ev{Subject: fmt.Sprintf("%s-%d", c.name, i), ID: fmt.Sprintf("%s-%d", c.name, i)})
			}
			ids := f.write(evs...)
			for _, id := range ids {
				f.settled(id, 60*time.Second)
			}
			if n := len(f.Cls.Batches()); n != 1 {
				t.Errorf("%s: CLS_LOG 请求次数 = %d，期望 1（不重试）", c.name, n)
			}
			if n := len(f.judgeRequests()); n != 3 {
				t.Errorf("%s: MODEL_LOG 判断题 = %d，期望全部 3 题", c.name, n)
			}
			for _, id := range ids {
				_, j := f.judgmentOf(id)
				eq(t, id+" levels[0].error_code", j.Levels[0].ErrorCode, c.code)
				eq(t, id+" decided_by", j.DecidedBy, "chat")
			}
			if n, _ := f.modelHealth(); n == 0 {
				t.Errorf("%s: 健康视图没有 model_unavailable 信号", c.name)
			}
			// 机制性说明只进诊断，不进提示词。
			for _, body := range f.judgeRequests() {
				if strings.Contains(body, "scripted failure") || strings.Contains(body, c.code) {
					t.Errorf("%s: 错误信息进入了对话端点的提示词: %s", c.name, body)
				}
			}
		}
	})

	f.step(t, "ACT-JDP-05 凭据无效时判断端点标为不可用直到下一次成功", func(t *testing.T) {
		f.reset()
		f.Cls.SetFault(modelstub.ClsFault{Status: 401})
		f.chatSays("jdp-auth", "no")
		a := f.write(ev{Subject: "jdp-auth-a", ID: "jdp-auth-a"})[0]
		f.settled(a, 60*time.Second)
		if n := len(f.Cls.Batches()); n != 1 {
			t.Errorf("401 那一批应不重试，请求次数 = %d", n)
		}
		_, ja := f.judgmentOf(a)
		eq(t, "levels[0].error_code", ja.Levels[0].ErrorCode, "unauthorized")
		eq(t, "decided_by", ja.DecidedBy, "chat")
		if _, d := f.modelHealth(); d["judge"] != "failing" {
			t.Errorf("model_unavailable.detail.judge = %v，期望 failing（detail=%v）", d["judge"], d)
		}
		before := len(f.Cls.Batches())
		b := f.write(ev{Subject: "jdp-auth-b", ID: "jdp-auth-b"})[0]
		f.settled(b, 60*time.Second)
		if extra := len(f.Cls.Batches()) - before; extra > 1 {
			t.Errorf("第 2 条事件期间判断端点应被视为不可用（至多一次探测），实际新增 %d 次请求", extra)
		}
		f.Cls.Reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-auth-c"), PYes: pf(0.05)})
		c := f.write(ev{Subject: "jdp-auth-c", ID: "jdp-auth-c"})[0]
		_, jc := f.judgmentOf(c)
		eq(t, "恢复后 decided_by", jc.DecidedBy, "classifier")
		if _, d := f.modelHealth(); d["judge"] != nil && d["judge"] != "" {
			t.Errorf("恢复且一次成功后 detail.judge = %v，期望为空", d["judge"])
		}
	})

	f.step(t, "ACT-JDP-06 超时与 5xx 退避重试至多 2 次，仍失败交下一级", func(t *testing.T) {
		// 超时按验收文档 15 秒 / 10 秒等比缩小为 3 秒 / 1 秒：把 timeout_ms 改成 1000。
		f.setJudge(map[string]any{"timeout_ms": 1000})
		for _, c := range []struct {
			name  string
			fault modelstub.ClsFault
			codes []string
		}{
			{"jdp-503", modelstub.ClsFault{Status: 503}, []string{"unavailable"}},
			{"jdp-slow", modelstub.ClsFault{DelayMS: 3000}, []string{"timeout"}},
		} {
			f.reset()
			f.Cls.SetFault(c.fault)
			f.chatSays(c.name, "yes")
			id := f.write(ev{Subject: c.name, ID: c.name})[0]
			_, j := f.judgmentOf(id)
			bs := f.Cls.Batches()
			if len(bs) != 3 {
				t.Fatalf("%s: CLS_LOG 请求次数 = %d，期望 3（首发加至多 2 次退避重试）", c.name, len(bs))
			}
			g1, g2 := bs[1].ReceivedAt.Sub(bs[0].ReceivedAt), bs[2].ReceivedAt.Sub(bs[1].ReceivedAt)
			if g2 <= g1 && c.name == "jdp-503" {
				t.Errorf("%s: 重试间隔应递增: %v, %v", c.name, g1, g2)
			}
			eq(t, c.name+" error_code", j.Levels[0].ErrorCode, c.codes[0])
			eq(t, c.name+" decided_by", j.DecidedBy, "chat")
		}
		f.setJudge(nil)
	})

	f.step(t, "ACT-JDP-07 413 时对半拆批各重发一次", func(t *testing.T) {
		f.reset()
		f.Cls.SetFault(modelstub.ClsFault{MaxQuestion: 4})
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-split-"), PYes: pf(0.05)})
		var evs []ev
		for i := 1; i <= 8; i++ {
			evs = append(evs, ev{Subject: fmt.Sprintf("jdp-split-%d", i), ID: fmt.Sprintf("jdp-split-%d", i)})
		}
		ids := f.write(evs...)
		for _, id := range ids {
			f.settled(id, 60*time.Second)
		}
		var sizes []int
		for _, b := range f.Cls.Batches() {
			sizes = append(sizes, b.Questions)
		}
		if fmt.Sprint(sizes) != "[8 4 4]" {
			t.Fatalf("应依次是 8 题一批（413）再两批各 4 题，实际 %v", sizes)
		}
		for _, id := range ids {
			_, j := f.judgmentOf(id)
			eq(t, id+" decided_by", j.DecidedBy, "classifier")
		}

		f.reset()
		f.Cls.SetFault(modelstub.ClsFault{Status: 413})
		f.chatSays("jdp-split2-", "no")
		evs = nil
		for i := 1; i <= 8; i++ {
			evs = append(evs, ev{Subject: fmt.Sprintf("jdp-split2-%d", i), ID: fmt.Sprintf("jdp-split2-%d", i)})
		}
		ids = f.write(evs...)
		for _, id := range ids {
			f.settled(id, 60*time.Second)
		}
		sizes = nil
		for _, b := range f.Cls.Batches() {
			sizes = append(sizes, b.Questions)
		}
		if fmt.Sprint(sizes) != "[8 4 4]" {
			t.Fatalf("一律 413 时应是 8、4、4 后不再拆，实际 %v", sizes)
		}
		if n := len(f.judgeRequests()); n != 8 {
			t.Errorf("8 题应全部交对话端点，MODEL_LOG 判断题 = %d", n)
		}
		for _, id := range ids {
			_, j := f.judgmentOf(id)
			eq(t, id+" error_code", j.Levels[0].ErrorCode, "batch_too_large")
			eq(t, id+" decided_by", j.DecidedBy, "chat")
		}
	})

	f.step(t, "ACT-JDP-08 429 遵守 Retry-After，等待不超过 timeout_ms", func(t *testing.T) {
		f.reset()
		f.Cls.SetFault(modelstub.ClsFault{Status: 429, RetryAfter: 2})
		id := f.write(ev{Subject: "jdp-rl-ok", ID: "jdp-rl-ok"})[0]
		if !f.WaitFor(30*time.Second, func() bool { return len(f.Cls.Batches()) >= 1 }) {
			t.Fatal("分类端点没有收到请求")
		}
		f.Cls.SetFault(modelstub.ClsFault{}) // 之后正常
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-rl-ok"), PYes: pf(0.05)})
		_, j := f.judgmentOf(id)
		bs := f.Cls.Batches()
		if len(bs) < 2 {
			t.Fatalf("429 之后应重发，请求次数 = %d", len(bs))
		}
		if gap := bs[1].ReceivedAt.Sub(bs[0].ReceivedAt); gap < 1900*time.Millisecond {
			t.Errorf("Retry-After: 2 之后 %v 就重发了", gap)
		}
		eq(t, "decided_by", j.DecidedBy, "classifier")

		f.reset()
		f.Cls.SetFault(modelstub.ClsFault{Status: 429, RetryAfter: 30})
		f.chatSays("jdp-rl-long", "no")
		start := time.Now()
		id = f.write(ev{Subject: "jdp-rl-long", ID: "jdp-rl-long"})[0]
		_, j = f.judgmentOf(id)
		if d := time.Since(start); d > 25*time.Second {
			t.Errorf("Retry-After: 30 超过 timeout_ms，内核不应等待，实际耗时 %v", d)
		}
		eq(t, "error_code", j.Levels[0].ErrorCode, "rate_limited")
		eq(t, "decided_by", j.DecidedBy, "chat")
	})

	f.step(t, "ACT-JDP-09 self_reported 的概率不被采信", func(t *testing.T) {
		f.reset()
		f.Cls.SetFault(modelstub.ClsFault{Source: "self_reported"})
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-self"), PYes: pf(0.01)})
		f.chatSays("jdp-self", "yes")
		e, j := f.judgmentOf(f.write(ev{Subject: "jdp-self", ID: "jdp-self"})[0])
		if o := j.Levels[0].Outcome; o != "error" && o != "skipped" {
			t.Errorf("levels[0].outcome = %q，期望 error 或 skipped", o)
		}
		eq(t, "decided_by", j.DecidedBy, "chat")
		eq(t, "answer", j.Answer, "yes")
		eq(t, "处置", e.Disposition, "proposed")
		if n, _ := f.modelHealth(); n == 0 {
			t.Error("健康视图没有分类端点不可用的信号")
		}
	})

	f.step(t, "ACT-JDP-10 资料超过 max_material_chars 被截断并标记", func(t *testing.T) {
		f.reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-long"), PYes: pf(0.05)})
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-short"), PYes: pf(0.05)})
		f.write(ev{Subject: "jdp-long", Detail: strings.Repeat("长", 9000), ID: "jdp-long"}, ev{Subject: "jdp-short", Detail: "短", ID: "jdp-short"})
		f.settled("jdp-long", 60*time.Second)
		f.settled("jdp-short", 60*time.Second)
		got := map[string]bool{}
		for _, b := range f.Cls.Batches() {
			for _, q := range parseCls(t, b.BodyText).Questions {
				switch q.Input["subject"] {
				case "jdp-long":
					if n := len([]rune(q.Material)); n > 8000 {
						t.Errorf("material 长度 %d，超过 8000", n)
					}
					if !q.MaterialTruncated {
						t.Error("material_truncated 应为 true")
					}
					got["long"] = true
				case "jdp-short":
					if q.MaterialTruncated {
						t.Error("短事件不应标 material_truncated")
					}
					got["short"] = true
				}
			}
		}
		if !got["long"] || !got["short"] {
			t.Fatalf("CLS_LOG 里没有找到两道题: %v", got)
		}
	})

	f.step(t, "ACT-JDP-11 设置不提供 chat-logprobs 选项", func(t *testing.T) {
		assertNoChatLogprobsOption(t, f)
	})

	f.step(t, "ACT-JDP-12 对数概率换算路径在替身上可用", func(t *testing.T) {
		assertChatLogprobs(t, f)
	})

	f.step(t, "ACT-JDP-13 同一输入指纹复用结果，换模型后不复用", func(t *testing.T) {
		f.reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: key("jdp-reuse"), PYes: pf(0.05)})
		same := ev{Subject: "jdp-reuse", Detail: "同样的内容", Sender: "alice@example.invalid"}
		first := f.write(withID(same, "jdp-reuse-1"))[0]
		_, j1 := f.judgmentOf(first)
		calls := f.callsOf("inbound_screen")
		second := f.write(withID(same, "jdp-reuse-2"))[0]
		_, j2 := f.judgmentOf(second)
		if len(f.Cls.Batches()) != 1 {
			t.Errorf("第二条不应外呼，CLS_LOG 批次 = %d", len(f.Cls.Batches()))
		}
		if !j2.Levels[0].Reused {
			t.Errorf("第二条判断记录应标 reused: %+v", j2)
		}
		if j1.Levels[0].Reused {
			t.Error("第一条不应标 reused")
		}
		if after := f.callsOf("inbound_screen"); after != calls {
			t.Errorf("reused 不计入调用次数：calls %d → %d", calls, after)
		}
		f.setJudge(map[string]any{"model": "acc-model-2"})
		third := f.write(withID(same, "jdp-reuse-3"))[0]
		_, j3 := f.judgmentOf(third)
		if len(f.Cls.Batches()) != 2 || j3.Levels[0].Reused {
			t.Errorf("换模型后应重新外呼：批次 %d，reused=%v", len(f.Cls.Batches()), j3.Levels[0].Reused)
		}
		f.setJudge(nil)
	})
}

func withID(e ev, id string) ev { e.ID = id; return e }
