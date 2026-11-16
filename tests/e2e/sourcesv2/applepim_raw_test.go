//go:build e2e

package sourcesv2

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
)

// applepimSource 登记一个 apple-pim-source 来源（PATH 前缀 TOOL_STUB_BIN，录制样本替身）。
func (k *kit) applepimSource(id, sourceType string, config map[string]any) {
	k.t.Helper()
	k.webSource(map[string]any{"id": id, "name": id, "type": "script", "command": filepath.Join(progDir, "apple-pim-source"), "schedule": "1s", "timeout": 20,
		"source_type": sourceType, "dedupe_domain": "acc-" + id, "config": config})
}

// fullEvent 经 Web 读一条来源事件的原文（GET /source-events/{id}）。
func (k *kit) fullEvent(ev SrcEvent) (r struct {
	Body     string `json:"body"`
	Original struct {
		MediaType string `json:"media_type"`
		Text      string `json:"text"`
	} `json:"original"`
	Headers     map[string]any   `json:"headers"`
	Attachments []map[string]any `json:"attachments"`
	Content     struct {
		State string `json:"state"`
	} `json:"content"`
}) {
	k.t.Helper()
	if err := json.Unmarshal([]byte(k.webOK("GET", "/source-events/"+ev.ID, "", 200)), &r); err != nil {
		k.t.Fatal(err)
	}
	return r
}

// TestFlowAppleMailRaw 覆盖 ACT-MAD-01、ACT-MAD-03：桌面邮件适配器取全原文；同一封信再次拉到不重复入账。
func TestFlowAppleMailRaw(t *testing.T) {
	k := setup(t)
	programs(t)
	const src = "src_applemail"
	k.step("ACT-MAD-01 长 HTML 邮件取全正文、信头、附件与 HTML 原文", func(t *testing.T) {
		k.SetToolCase("mail-long-html")
		k.applepimSource(src, "mail", map[string]any{"kind": "mail", "account": "Acct", "lookback_days": 36500})
		ev := k.settled(src, "long-html@example.test")
		r := k.fullEvent(ev)
		if len(r.Body) <= 16384 || !strings.Contains(r.Body, "(https://example.test/p/259)") {
			t.Errorf("body 应是未截断的转换后文本: %d 字节", len(r.Body))
		}
		if r.Original.MediaType != "text/html" || !strings.Contains(r.Original.Text, "<script>alert(1)</script>") {
			t.Errorf("original 应是原样 HTML: %q", r.Original.MediaType)
		}
		for _, h := range []string{"From", "To", "Date", "Subject", "Message-Id"} {
			if r.Headers[h] == nil {
				t.Errorf("headers 缺 %s: %v", h, r.Headers)
			}
		}
		if len(r.Attachments) != 2 || r.Attachments[0]["name"] != "report.pdf" || r.Attachments[0]["size"] != float64(123456) || r.Attachments[1]["name"] != "chart.png" {
			t.Errorf("attachments: %v", r.Attachments)
		}
		if r.Content.State != "full" {
			t.Errorf("取全了应是 full: %q", r.Content.State)
		}
	})
	k.step("ACT-MAD-03 同一封信再次拉到：事件 id 不变，不重复入账", func(t *testing.T) {
		before := len(k.events(src))
		k.cycle(src, 2)
		if after := len(k.events(src)); after != before || before != 1 {
			t.Errorf("事件数 %d -> %d，应保持 1", before, after)
		}
		// 升级前的算法：id 就是 Message-ID。
		if _, ok := k.event(src, "long-html@example.test"); !ok {
			t.Error("事件 id 应仍是 Message-ID")
		}
	})
}

// TestFlowAppleReminderOpen 覆盖 ACT-REM-01、03、04 与 ACT-OAD-04 里提醒事项的 headers。
func TestFlowAppleReminderOpen(t *testing.T) {
	k := setup(t)
	programs(t)
	const src, plain = "src_rem", "src_rem_plain"
	byKey := func(s, key string) []SrcEvent {
		var out []SrcEvent
		for _, e := range k.events(s) {
			if len(e.CorrelationKeys) == 1 && e.CorrelationKeys[0] == key {
				out = append(out, e)
			}
		}
		return out
	}
	k.step("ACT-REM-01 undated_lists 里的无到期日提醒产出 reminder.open", func(t *testing.T) {
		k.SetToolCase("rem-undated")
		k.applepimSource(src, "reminder", map[string]any{"kind": "reminders", "undated_lists": []string{"随手记"}})
		k.until("RU1 与 RD1 入账", func() bool { return len(byKey(src, "RU1")) == 1 && len(byKey(src, "RD1")) == 1 })
		u := byKey(src, "RU1")[0]
		if u.Type != "reminder.open" {
			t.Errorf("RU1 类型: %s", u.Type)
		}
		if h := k.fullEvent(u).Headers; h["List"] != "随手记" || h["Priority"] != "5" {
			t.Errorf("提醒 headers 应含列表与优先级: %v", h)
		}
		if d := byKey(src, "RD1")[0]; d.Type != "reminder.due" || k.fullEvent(d).Headers["Due"] != "2026-01-10" {
			t.Errorf("RD1: %+v", d)
		}
		if n := len(byKey(src, "RW1")) + len(byKey(src, "RC1")); n != 0 {
			t.Errorf("其他列表的无到期日提醒与已完成提醒不应有事件: %d", n)
		}
	})
	k.step("ACT-REM-03 改标题、补到期日后关联键不变", func(t *testing.T) {
		k.SetToolCase("rem-undated-renamed")
		k.until("改标题后的新版本入账", func() bool { return len(byKey(src, "RU1")) == 2 })
		k.SetToolCase("rem-undated-dated")
		k.until("补到期日后的新版本入账", func() bool { return len(byKey(src, "RU1")) == 3 })
		var types []string
		for _, e := range byKey(src, "RU1") {
			types = append(types, e.Type)
		}
		if strings.Count(strings.Join(types, ","), "reminder.due") != 1 || strings.Count(strings.Join(types, ","), "reminder.open") != 2 {
			t.Errorf("类型: %v", types)
		}
	})
	k.step("ACT-REM-04 不配 undated_lists 时行为不变", func(t *testing.T) {
		k.SetToolCase("rem-undated")
		k.applepimSource(plain, "reminder", map[string]any{"kind": "reminders"})
		k.until("RD1 入账", func() bool { return len(byKey(plain, "RD1")) == 1 })
		k.cycle(plain, 1)
		for _, e := range k.events(plain) {
			if e.Type != "reminder.due" {
				t.Errorf("只应有 reminder.due: %+v", e)
			}
		}
	})
}
