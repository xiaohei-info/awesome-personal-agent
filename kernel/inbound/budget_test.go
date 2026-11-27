package inbound

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/budget"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// genBudget 把写作预算设成 n 个字符（折成 token：1.5 字符一个，n 取 1.5 的整数倍时折算无余数）。
func (e *env) genBudget(n int) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "budget", func(p *policy.Policy) error {
			p.GenerateEndpoint.MaxMaterialTokens = policy.TokensOfChars(n)
			return nil
		})
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

// ACT-BUD-02/04/07：账本原文不截断；建议事项正文只写前 4000 并链到原文。
func TestLongBodyIsKeptWholeInLedgerAndCappedOnItemBody(t *testing.T) {
	e := newEnv(t, false)
	body := numbered(20000)
	e.emit("src_t", "ticket", ev("big1", "ticket.opened", "长邮件", map[string]any{"detail": body}))
	d := e.events("src_t")[0]
	if d.Disposition != domain.DispositionProposed || d.Item == "" || len([]rune(d.Detail)) != 20000 || d.Detail != body {
		t.Fatalf("event %+v", d.Disposition)
	}
	ib := e.item(d.Item).Body
	if !strings.HasPrefix(ib, body[:4000]+"\n") || !strings.Contains(ib, "原文较长") || !strings.Contains(ib, "/sources/events/"+d.ID) || strings.Contains(ib, body[4000:4100]) {
		t.Fatalf("事项正文：%.60q…", ib)
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
}

// ACT-BUD-06：外发范围先于预算——excerpt 只含开头，meta 只有标题，均不超全文字数。
func TestOutboundPreviewAppliesScope(t *testing.T) {
	body := numbered(20000)
	ev := domain.SourceEvent{ID: "sev_o9", SourceID: "src_x", Type: "x.obd", Subject: "范围先于预算", Detail: body, Link: "https://example.test/a"}
	pv := OutboundPreview(ev, "")
	full, excerpt, meta := pv["full"], pv["excerpt"], pv["meta"]
	if len([]rune(full.Material)) < 10000 {
		t.Fatalf("full preview too short: %d", len([]rune(full.Material)))
	}
	if !strings.HasPrefix(excerpt.Material, body[:100]) || len([]rune(excerpt.Material)) > 600 {
		t.Fatalf("excerpt preview = %d chars", len([]rune(excerpt.Material)))
	}
	if meta.Material != "" || meta.Subject != "范围先于预算" {
		t.Fatalf("meta preview = %+v", meta)
	}
}
