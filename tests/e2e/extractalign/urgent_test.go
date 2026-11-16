//go:build e2e

package extractalign

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// urgentPack 是 acc-wb 里的紧急提醒规则（ACT-URG-01）：动作类，不写 mode。
const urgentPack = `name: acc-wb
version: 1.0.0
description: 紧急提醒验收包
source_rules:
  - {id: mail-urgent, match: {source_type: mail-urgent, event_type: [mail.received]}, then: screen, notify_urgent: true}
`

const urgentRuleID = "mail-urgent"

type ruleView struct {
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	Class string `json:"class"`
}

func (f *fx) rule() ruleView {
	f.t.Helper()
	var out struct {
		Rules []ruleView `json:"rules"`
	}
	jsonOf(f.t, f.webOK("GET", "/rules", "", 200), &out)
	for _, r := range out.Rules {
		if r.ID == urgentRuleID {
			return r
		}
	}
	f.t.Fatalf("规则 %s 不在 GET /rules 里: %+v", urgentRuleID, out.Rules)
	return ruleView{}
}

type today struct {
	Pushed             int `json:"pushed"`
	SuppressedDailyCap int `json:"suppressed_daily_cap"`
	SuppressedMuted    int `json:"suppressed_muted"`
}

func (f *fx) today() today {
	f.t.Helper()
	var d today
	jsonOf(f.t, f.webOK("GET", "/deliveries/today", "", 200), &d)
	return d
}

// disturb 写打扰策略：提醒与决策走渠道 acc-chan，不合并。
func (f *fx) disturb(cap int, mutes []map[string]any) {
	f.t.Helper()
	d := map[string]any{"routes": map[string]string{"decision": "acc-chan", "reminder": "acc-chan", "brief": "web", "health": "web", "wake": "web"},
		"merge_window_sec": 0, "daily_cap": cap}
	if mutes != nil {
		d["mutes"] = mutes
	}
	b, _ := json.Marshal(map[string]any{"disturb": d})
	f.webOK("PUT", "/settings/policy", string(b), 200)
}

func mail(id, subject, sender string) map[string]any {
	return map[string]any{"id": id, "type": "mail.received", "subject": subject, "sender": sender, "summary": subject,
		"detail": "对方来信：" + subject, "occurred_at": "2026-11-10T09:00:00Z", "correlation_keys": []string{"mail-" + id}}
}

// claim 以渠道 ext 的密钥领一条投递（200 返回正文，204 返回空串）。
func (f *fx) claim(waitSec string) (int, string) {
	r := f.APIRaw("GET", "/v1/deliveries/next?wait="+waitSec, "", f.CHAN)
	return r.Code, r.Body
}

func TestFlowUrgentNotify(t0 *testing.T) {
	t := t0
	f := newFx(t)
	f.Cls.SetDefaultPYes(0.95) // triage-screen p_yes=0.95
	f.disturb(20, nil)
	var src *source
	var bot *fakebot.Bot
	wantSent := 0
	var urg2 string
	quiet := func(what string) {
		t.Helper()
		time.Sleep(3 * time.Second) // 渠道 ext 长轮询领取，给它机会送出
		if n := len(bot.Sent()); n != wantSent {
			t.Fatalf("%s：机器人替身有 %d 条消息，期望 %d: %+v", what, n, wantSent, bot.Sent())
		}
	}
	// noReminder 断言之后机器人替身没有收到指向建议的提醒；"已达日上限"的说明不是提醒，允许出现。
	noReminder := func(what string) {
		t.Helper()
		time.Sleep(3 * time.Second)
		for _, m := range bot.Sent()[wantSent:] {
			if strings.Contains(m.Text, "/items/") {
				t.Fatalf("%s：机器人替身收到了提醒: %s", what, m.Text)
			}
		}
		wantSent = len(bot.Sent())
	}
	emit := func(id, sender string) event {
		t.Helper()
		e := src.emit(mail(id, "报价待回复 "+id, sender))[id]
		eq(t, id+" disposition", e.Disposition, "proposed")
		return e
	}

	f.step("ACT-URG-01 准备紧急提醒规则", func() {
		f.writePack(urgentPack)
		f.restart()
		src = f.register("src_accurg", "mail-urgent")
		r := f.rule()
		eq(t, "class", r.Class, "action")
		eq(t, "mode", r.Mode, "observe")
	})

	f.step("ACT-URG-02 观察模式只记本来会提醒", func() {
		f.draftWB("urgent-true", modelstub.WBParams{})
		e := emit("urg-1", "s1@example.com")
		eq(t, "proposal.urgent", f.proposal(e.Item)["urgent"], true)
		obs := f.audit("", "rule.observed")
		if len(obs) != 1 || obs[0].Object != e.Item || obs[0].Payload["would_do"] != "notify_urgent" {
			t.Fatalf("应有一条记\"本来会提醒\"的 rule.observed: %+v", obs)
		}
		if c, _ := f.claim("2"); c != 204 {
			t.Errorf("观察模式不应产生投递，领取返回 %d", c)
		}
		eq(t, "pushed", f.today().Pushed, 0)
	})

	f.step("ACT-URG-03 切到开后 urgent 为真的提醒一次，事项仍是建议", func() {
		f.webOK("PATCH", "/rules/"+url.PathEscape("acc-wb/source_rules/"+urgentRuleID)+"/mode", `{"mode":"auto"}`, 200)
		e := emit("urg-2", "s2@example.com")
		urg2 = e.Item
		code, raw := f.claim("10")
		if code != 200 {
			t.Fatalf("渠道应领到一条提醒，实际 %d %s", code, raw)
		}
		var d struct {
			ID      string `json:"id"`
			Version int    `json:"version"`
			Class   string `json:"class"`
			Item    string `json:"item"`
			Content struct {
				Title   string `json:"title"`
				Summary string `json:"summary"`
				WebURL  string `json:"web_url"`
			} `json:"content"`
			Origin struct {
				Event string `json:"source_event"`
				Rule  string `json:"rule"`
				Item  string `json:"item"`
			} `json:"origin"`
		}
		jsonOf(t, raw, &d)
		eq(t, "投递 id 等于建议事项 id", d.ID, e.Item)
		eq(t, "class", d.Class, "reminder")
		eq(t, "投递不带事项", d.Item, "")
		eq(t, "origin.event", d.Origin.Event, e.ID)
		eq(t, "origin.rule", d.Origin.Rule, "acc-wb/source_rules/"+urgentRuleID)
		eq(t, "origin.item", d.Origin.Item, e.Item)
		eq(t, "标题", d.Content.Title, "回复对方的报价")
		eq(t, "摘要", d.Content.Summary, "对方在等回复")
		if !strings.Contains(d.Content.WebURL, e.Item) {
			t.Errorf("链接应指向建议流里这一条: %q", d.Content.WebURL)
		}
		eq(t, "phase", f.webItem(e.Item)["phase"], "proposed")
		if r := f.APIRaw("POST", "/v1/deliveries/"+d.ID+"/ack", `{"version":`+fmt.Sprint(d.Version)+`,"external_ref":"urg-claim"}`, f.CHAN); r.Code != 200 {
			t.Fatalf("ack: %d %s", r.Code, r.Body)
		}
		eq(t, "pushed", f.today().Pushed, 1)
		bot, _ = f.startExt(t0) // 之后的提醒经随包渠道 ext 送到机器人替身
	})

	f.step("ACT-URG-04 可以等的与没给判断的不提醒", func() {
		f.draftWB("urgent-false", modelstub.WBParams{})
		emit("urg-3", "s3@example.com")
		f.draftWB("urgent-missing", modelstub.WBParams{})
		emit("urg-4", "s4@example.com")
		f.ModelStub.SetDefault(modelstub.Reply{Status: 500}) // 起草失败（5xx）
		failed := emit("urg-5", "s5@example.com")
		eq(t, "起草失败仍成为建议，标题取来源事件", f.webItem(failed.Item)["title"], "报价待回复 urg-5")
		quiet("三条都不提醒")
		eq(t, "pushed", f.today().Pushed, 1)
	})

	f.step("ACT-URG-05 同一条只提醒一次，并入的与已处置的不提醒", func() {
		f.draftWB("urgent-true", modelstub.WBParams{})
		// 合并键取来源事件的标题加发送者：与 urg-2 同标题同发送者的新事件并入已有建议。
		first := f.proposedTitled("回复对方的报价")
		e6 := src.emit(mail("urg-6", "报价待回复 urg-2", "s2@example.com"))["urg-6"]
		if got := len(f.proposedTitled("回复对方的报价")); got != len(first) {
			t.Errorf("urg-6 应并入已有建议，建议数 %d → %d", len(first), got)
		}
		eq(t, "urg-6 的 item 指向 urg-2 的建议", e6.Item, urg2)
		quiet("并入的不再提醒")

		// 另一条 urgent 建议提醒一次（经 ext 送到机器人替身），本人处置后其来源对象更新不再提醒。
		other := emit("urg-7", "s7@example.com")
		wantSent++
		f.until("机器人替身收到 urg-7 的提醒", func() bool { return len(bot.Sent()) >= wantSent })
		m := bot.Sent()[wantSent-1].Text
		if !strings.Contains(m, "回复对方的报价") || !strings.Contains(m, "对方在等回复") || !strings.Contains(m, other.Item) {
			t.Errorf("提醒应含标题、摘要与指向这一条的链接: %s", m)
		}
		// 来源对象在建议还没处置时更新：刷新原建议，不再提醒。
		upd := mail("urg-7a", "报价待回复 urg-7a", "s7@example.com")
		upd["correlation_keys"] = []string{"mail-urg-7"}
		eq(t, "更新刷新原建议", src.emit(upd)["urg-7a"].Item, other.Item)
		quiet("更新不再提醒")

		// 本人处置后：原建议不再有任何提醒（设计 §9.3），且仍是已处置。关闭的事项不被更新复活，
		// 之后的更新走普通流水线成为新的建议（设计 §9.4），那是另一条建议，自有一次提醒。
		f.webOK("POST", "/items/"+other.Item+"/dismiss", `{"reason":"不需要"}`, 200)
		upd = mail("urg-7b", "报价待回复 urg-7b", "s7@example.com")
		upd["correlation_keys"] = []string{"mail-urg-7"}
		next := src.emit(upd)["urg-7b"]
		if next.Item == other.Item {
			t.Fatalf("已处置的建议不应被更新复活: %s", next.Item)
		}
		wantSent++
		f.until("新建议的提醒", func() bool { return len(bot.Sent()) >= wantSent })
		quiet("每条建议只提醒一次")
		n := 0
		for _, s := range bot.Sent() {
			if strings.Contains(s.Text, other.Item) {
				n++
			}
		}
		eq(t, "已处置建议收到的提醒数", n, 1)
		eq(t, "已处置建议的阶段", f.webItem(other.Item)["phase"], "dropped")
	})

	f.step("ACT-URG-06 日上限与静音照常生效", func() {
		base := f.today()
		f.disturb(base.Pushed, nil)
		capped := emit("urg-8", "s8@example.com")
		noReminder("超日上限")
		d := f.today()
		eq(t, "suppressed_daily_cap", d.SuppressedDailyCap, base.SuppressedDailyCap+1)
		eq(t, "pushed", d.Pushed, base.Pushed)
		if !strings.Contains(f.webOK("GET", "/deliveries/unread", "", 200), capped.Item) {
			t.Error("超日上限的提醒应留在 Web 未读")
		}

		f.disturb(20, []map[string]any{{"scope": "source", "id": "src_accurg"}})
		muted := emit("urg-9", "s9@example.com")
		noReminder("来源被静音")
		d = f.today()
		eq(t, "suppressed_muted", d.SuppressedMuted, base.SuppressedMuted+1)
		eq(t, "pushed", d.Pushed, base.Pushed)
		if !strings.Contains(f.webOK("GET", "/deliveries/unread", "", 200), muted.Item) {
			t.Error("被静音的提醒应留在 Web 未读")
		}
	})

	f.step("ACT-URG-07 规则内容变化后回到观察", func() {
		eq(t, "修改前模式", f.rule().Mode, "auto")
		f.writePack(strings.Replace(urgentPack, "[mail.received]", "[mail.received, mail.updated]", 1))
		f.restart()
		eq(t, "模式", f.rule().Mode, "observe")
		ch := f.audit("", "rule.mode_changed")
		if len(ch) < 2 || ch[len(ch)-1].Payload["from"] != "auto" || ch[len(ch)-1].Payload["to"] != "observe" {
			t.Fatalf("应记一条 auto→observe 的 rule.mode_changed: %+v", ch)
		}
	})
}

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }
