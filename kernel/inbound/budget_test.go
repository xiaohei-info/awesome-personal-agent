package inbound

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/budget"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) genBudget(n int) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "budget", func(p *policy.Policy) error { p.GenerateEndpoint.MaxMaterialChars = n; return nil })
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// materialOf 读事件处置 Event 载荷里的 material。
func (e *env) materialOf(evID string) *budget.Trace {
	e.t.Helper()
	var out *budget.Trace
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, _ := eventlog.ReadObject(tx, evID)
		for _, x := range es {
			if x.Event.Type == EvDisposed {
				var p struct {
					Material *budget.Trace `json:"material"`
				}
				_ = json.Unmarshal(x.Event.Payload, &p)
				out = p.Material
			}
		}
		return nil
	})
	return out
}

func numbered(n int) string { // 每个位置都不同的文本，便于逐字核对头尾
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%05d,", i)
	}
	return b.String()[:n]
}

// ACT-BUD-02/04/07：超预算的长正文按首 3/4 尾 1/4 送给起草；判断记录与处置载荷留痕；建议事项正文只写前 4000 并链到原文；账本原文不变。
func TestLongBodyIsTrimmedForDraftTracedAndProposalBodyCapped(t *testing.T) {
	e := newEnv(t, true)
	e.genBudget(3000)
	e.screen(0.95)
	e.draft(`{"title":"长邮件","summary":"s","kind":"task","reason":"r"}`)
	body := numbered(20000)
	e.emit("src_x", "other", ev("big1", "mail.received", "长邮件", map[string]any{"detail": body}))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionProposed || len([]rune(d.Detail)) != 20000 || d.Detail != body {
		t.Fatalf("event %+v", d.Disposition)
	}
	var draftReq string
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, "起草一条建议") {
			draftReq = r.BodyText
		}
	}
	if !strings.Contains(draftReq, body[:2200]) || !strings.Contains(draftReq, body[20000-700:]) || !strings.Contains(draftReq, "省略") || strings.Contains(draftReq, body[9000:9100]) {
		t.Fatal("起草收到的不是首 3/4 + 尾 1/4 加省略行")
	}
	m := e.materialOf(d.ID)
	if m == nil || m.Original < 20000 || m.Sent > 3000 || m.Sent < 2900 || m.Mode != budget.ModeHeadTail {
		t.Fatalf("处置载荷 material = %+v", m)
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		rec, err := record.Get(tx, d.Judgment)
		if err != nil {
			t.Fatal(err)
		}
		lm := rec.Levels[0].Material
		if lm == nil || lm.Original != m.Original || lm.Sent > 8000 || lm.Sent < 7900 || lm.Mode != budget.ModeHeadTail {
			t.Fatalf("判断记录初筛一级 material = %+v（判断端点预算 8000）", lm)
		}
		return nil
	})
	got := e.item(d.Item).Body
	if !strings.HasPrefix(got, body[:4000]+"\n") || !strings.Contains(got, "原文较长") || !strings.Contains(got, "/sources/events/"+d.ID) || strings.Contains(got, body[4000:4100]) {
		t.Fatalf("事项正文：%.60q…", got)
	}
}

// ACT-BUD-07：不超过 4000 字符的正文完整写入事项，且不超预算时留痕为 none。
func TestMidBodyIsCopiedWhole(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.95)
	e.draft(`{"title":"中等邮件","summary":"s","kind":"task","reason":"r"}`)
	body := numbered(3000)
	e.emit("src_x", "other", ev("mid1", "mail.received", "中等邮件", map[string]any{"detail": body}))
	d := e.events("src_x")[0]
	if got := e.item(d.Item).Body; got != body {
		t.Fatalf("短正文被改动：%d", len(got))
	}
	if m := e.materialOf(d.ID); m == nil || m.Mode != budget.ModeNone || m.Sent != m.Original {
		t.Fatalf("material = %+v", m)
	}
}

// ACT-BUD-03：聚合消息从最新一条往前整条保留，更早的整条省略并写明条数，抽取处置记 newest_messages。
func TestAggregatedMessagesKeepNewestWhole(t *testing.T) {
	e := newEnv(t, true)
	e.genBudget(3000)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"t","kind":"task"}]}`)
	var msgs []map[string]any
	for i := 1; i <= 60; i++ {
		msgs = append(msgs, map[string]any{"sender": "bob", "text": fmt.Sprintf("m%02d ", i) + strings.Repeat("字", 96), "at": "2026-03-01T00:00:00Z"})
	}
	e.emit("src_x", "exits", ev("c1", "x.chat", "群聊", map[string]any{"items": msgs}))
	d := e.events("src_x")[0]
	var req string
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, "m60") {
			req = r.BodyText
		}
	}
	if req == "" || strings.Contains(req, "m01 ") || !strings.Contains(req, "省略更早的") {
		t.Fatalf("应保留最新、丢最旧并写明省略：%v", req == "")
	}
	m := e.materialOf(d.ID)
	if m == nil || m.Mode != budget.ModeNewest || m.Sent > 3000 {
		t.Fatalf("material = %+v", m)
	}
	for i := 1; i <= 60; i++ { // 保留下来的每条都是完整的 100 字消息
		if tag := fmt.Sprintf("m%02d ", i); strings.Contains(req, tag) && !strings.Contains(req, tag+strings.Repeat("字", 96)) {
			t.Fatalf("消息 %d 被切断", i)
		}
	}
}

// ACT-BUD-06：外发范围先于预算：excerpt 范围的外部端点只收到开头 500 字符，留痕的"裁剪前"是范围裁剪后的长度。
func TestOutboundScopeAppliesBeforeBudget(t *testing.T) {
	e := newEnv(t, true)
	e.locality("external")
	e.genBudget(3000)
	e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
	e.registerObd("full", "excerpt")
	big := obdEvent("o9", "范围先于预算")
	big["detail"] = numbered(20000)
	e.emit(obdSrc, "inbox", big)
	d := e.events(obdSrc)[0]
	if m := e.materialOf(d.ID); m == nil || m.Original > 1000 || m.Mode != budget.ModeNone {
		t.Fatalf("material = %+v", m)
	}
	if strings.Contains(e.sent(0), numbered(20000)[1500:1600]) {
		t.Fatal("外部端点收到了超出外发范围的内容")
	}
}
