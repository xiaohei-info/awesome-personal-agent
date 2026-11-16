package inbound

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const (
	urgentRule   = "inbound-triage/source_rules/x-urgent"
	urgentExRule = "inbound-triage/source_rules/x-urgent-ex"
)

func urgentEvent(id, link string) map[string]any {
	return ev(id, "x.urgent", "合同今天要签", map[string]any{"sender": "boss", "link": link, "summary": "来源摘要"})
}

// urgentSetup 让初筛通过，起草按 draftJSON 回答；auto 为真时本人把规则切到开。先发一条无关事件，让来源登记、规则对账。
func (e *env) urgentSetup(auto bool, draftJSON string) {
	e.t.Helper()
	e.svc.o.Now = e.Now // 合并窗口按账本时间算建议的年龄
	e.screen(0.95)
	e.draft(draftJSON)
	if auto {
		e.must("rule.set_mode", map[string]string{"rule": urgentRule, "mode": rules.ModeAuto})
		e.must("rule.set_mode", map[string]string{"rule": urgentExRule, "mode": rules.ModeAuto})
	}
}

func (e *env) deliveries() (out []domain.Delivery) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		unread, err := e.queue.Unread(tx)
		if err != nil {
			return err
		}
		pending, err := e.queue.Backlog(tx)
		out = append(unread, pending...)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) observedItems(rule string) (ids []string) {
	e.t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		all, err := eventlog.Query(tx, eventlog.Filter{Type: rules.EvObserved}, 0, 100)
		for _, en := range all {
			if en.Event.Actor.Principal == "rule:"+rule && strings.Contains(string(en.Event.Payload), "notify_urgent") {
				ids = append(ids, en.Event.Object)
			}
		}
		return err
	})
	return
}

const urgentDraft = `{"title":"签合同","kind":"task","summary":"今天下班前签","reason":"对方今晚截止","urgent":%s}`

// ACT-URG-01/02：规则初始 observe；urgent 为真的新建议写入 proposal.urgent，只记 rule.observed“本来会提醒”，不投递。
func TestNotifyUrgentObserveOnlyRecordsWouldNotify(t *testing.T) {
	e := newEnv(t, true)
	e.urgentSetup(false, fmt.Sprintf(urgentDraft, "true"))
	e.emit("src_u", "exits", urgentEvent("u1", "https://x.example/1"))
	d := e.events("src_u")[0]
	it := e.item(d.Item)
	if d.Disposition != domain.DispositionProposed || it.Phase != domain.PhaseProposed || it.Proposal == nil || it.Proposal.Urgent == nil || !*it.Proposal.Urgent {
		t.Fatalf("event %+v item %+v proposal %+v", d, it, it.Proposal)
	}
	if got := e.observedItems(urgentRule); len(got) != 1 || got[0] != it.ID {
		t.Fatalf("rule.observed for %v, want [%s]", got, it.ID)
	}
	if n := len(e.deliveries()); n != 0 {
		t.Fatalf("observe produced %d deliveries", n)
	}
}

// ACT-URG-03：开启后 urgent 为真的新建议提醒一次：投递 id 等于建议 id，不带事项，origin 记来源事件、规则、来源与建议，内容含标题、摘要、理由与 Web 链接；建议仍是 proposed。
func TestNotifyUrgentAutoDeliversReminderWithoutItem(t *testing.T) {
	e := newEnv(t, true)
	e.urgentSetup(true, fmt.Sprintf(urgentDraft, "true"))
	e.emit("src_u", "exits", urgentEvent("u1", "https://x.example/1"))
	d := e.events("src_u")[0]
	it := e.item(d.Item)
	ds := e.deliveries()
	if len(ds) != 1 {
		t.Fatalf("deliveries %+v", ds)
	}
	got := ds[0]
	if got.ID != it.ID || got.Item != "" || got.Class != domain.DeliveryReminder || got.Origin == nil ||
		*got.Origin != (domain.DeliveryOrigin{SourceEvent: d.ID, Rule: urgentRule, Source: "src_u", Item: it.ID}) {
		t.Fatalf("delivery %+v origin %+v", got, got.Origin)
	}
	c := got.Content
	if c.Title != "签合同" || !strings.Contains(c.Summary, "今天下班前签") || !strings.Contains(c.Summary, "对方今晚截止") || c.WebURL != "/items/"+it.ID {
		t.Fatalf("content %+v", c)
	}
	if it.Phase != domain.PhaseProposed || e.observedItems(urgentRule) != nil {
		t.Fatalf("item phase %s observed %v", it.Phase, e.observedItems(urgentRule))
	}
}

// ACT-URG-04：urgent 为假、没给 urgent、起草失败的新建议都照常成为建议，但不提醒。
func TestNotifyUrgentSkipsCanWaitMissingAndFailedDraft(t *testing.T) {
	for name, tc := range map[string]struct{ reply modelstub.Reply }{
		"false":   {modelstub.Reply{Content: fmt.Sprintf(urgentDraft, "false")}},
		"missing": {modelstub.Reply{Content: `{"title":"签合同","kind":"task","summary":"s"}`}},
		"failed":  {modelstub.Reply{Status: 500}},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true)
			e.urgentSetup(true, "{}")
			e.stub.SetDefault(tc.reply)
			e.emit("src_u", "exits", urgentEvent("u1", "https://x.example/1"))
			d := e.events("src_u")[0]
			if d.Disposition != domain.DispositionProposed || e.item(d.Item).Phase != domain.PhaseProposed {
				t.Fatalf("event %+v", d)
			}
			if n := len(e.deliveries()); n != 0 {
				t.Fatalf("%d deliveries", n)
			}
		})
	}
}

// ACT-URG-05：同一条建议只提醒一次——合并键相同的新事件并入已有建议，不再提醒，也不新建。
func TestNotifyUrgentMergedEventDoesNotRemindAgain(t *testing.T) {
	e := newEnv(t, true)
	e.urgentSetup(true, fmt.Sprintf(urgentDraft, "true"))
	e.emit("src_u", "exits", urgentEvent("u1", "https://x.example/1"))
	e.emit("src_u", "exits", urgentEvent("u2", "https://x.example/1"))
	evs := e.events("src_u")
	if len(e.allItems()) != 1 || len(evs) != 2 || evs[0].Item != evs[1].Item {
		t.Fatalf("items %d events %+v", len(e.allItems()), evs)
	}
	if n := len(e.deliveries()); n != 1 {
		t.Fatalf("deliveries %d, want 1", n)
	}
	e.emit("src_u", "exits", urgentEvent("u3", "https://x.example/other"))
	if n := len(e.deliveries()); n != 2 {
		t.Fatalf("a different suggestion should remind: %d", n)
	}
}

// 本人已处置的建议不提醒：建议建好后、处置记完前本人已把它接受，这次处置不再为它提醒；同样的处置对仍是 proposed 的建议会提醒。
func TestNotifyUrgentSkipsSuggestionAlreadyDisposedByOwner(t *testing.T) {
	e := newEnv(t, true)
	e.urgentSetup(false, fmt.Sprintf(urgentDraft, "true"))
	e.emit("src_u", "exits", urgentEvent("u1", "https://x.example/1"))
	d := e.events("src_u")[0]
	replay := func() { // 模拟上一轮在记完处置前中断：来源事件回到 pending，接着处置
		t.Helper()
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE sources_runner_event SET disposition = 'pending', item = '' WHERE id = ?`, d.ID)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		e.must(CmdDispose, disposeIn{Event: d.ID, Disposition: domain.DispositionProposed, Item: d.Item, Rule: urgentRule,
			mergeFields: mergeFields{New: true}, NotifyUrgent: true})
	}
	e.must("item.transition", map[string]any{"id": d.Item, "to": domain.PhaseOpen, "reason": "owner accepted"})
	replay()
	if n := len(e.deliveries()); n != 0 {
		t.Fatalf("reminded for an accepted suggestion: %d", n)
	}
	e.must("item.transition", map[string]any{"id": d.Item, "to": domain.PhaseProposed, "reason": "back"})
	replay()
	if n := len(e.deliveries()); n != 1 {
		t.Fatalf("proposed suggestion not reminded: %d", n)
	}
}

// ACT-XAL-09/ACT-URG-03：抽取出口（prescreen: none）每条候选各带 urgent，只有 urgent 为真的候选提醒，观察档只记它。
func TestNotifyUrgentExtractRemindsOnlyUrgentCandidates(t *testing.T) {
	for _, auto := range []bool{false, true} {
		e := newEnv(t, true)
		e.urgentSetup(auto, `{"candidates":[
		 {"title":"回电话","kind":"task","reason":"客户在等","urgent":true,"refs":[0]},
		 {"title":"周末聚餐","kind":"task","urgent":false,"refs":[1]},
		 {"title":"整理相册","kind":"task","refs":[2]}]}`)
		e.emit("src_u", "exits", ev("c1", "x.urgent_ex", "群聊", map[string]any{"sender": "bob", "items": []map[string]any{
			{"sender": "bob", "text": "客户在催"}, {"sender": "bob", "text": "周末吃饭"}, {"sender": "bob", "text": "相册"}}}))
		d := e.events("src_u")[0]
		if len(d.Items) != 3 || len(e.cls.Batches()) != 0 {
			t.Fatalf("auto=%v event %+v classifier calls %d", auto, d, len(e.cls.Batches()))
		}
		urgent := d.Items[0]
		if auto {
			ds := e.deliveries()
			if len(ds) != 1 || ds[0].ID != urgent || ds[0].Item != "" || ds[0].Origin.Rule != urgentExRule || ds[0].Origin.Item != urgent || ds[0].Content.Title != "回电话" {
				t.Fatalf("deliveries %+v", ds)
			}
		} else if len(e.deliveries()) != 0 || len(e.observedItems(urgentExRule)) != 1 || e.observedItems(urgentExRule)[0] != urgent {
			t.Fatalf("observe: deliveries %d observed %v", len(e.deliveries()), e.observedItems(urgentExRule))
		}
	}
}

// ACT-URG-06：提醒受静默、日上限与静音约束——静默中先暂存，静默结束才投；超日上限与被静音的留在 Web 未读（suppressed）。
func TestNotifyUrgentHonoursQuietCapAndMute(t *testing.T) {
	e := newEnv(t, true)
	e.urgentSetup(true, fmt.Sprintf(urgentDraft, "true"))
	update := func(f func(p *policy.Policy)) {
		t.Helper()
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			_, err := policy.Update(tx, owner.Actor, "urgent", func(p *policy.Policy) error { f(p); return nil })
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	update(func(p *policy.Policy) {
		p.Disturb.Quiet = &policy.Quiet{Start: e.now.Add(-time.Hour).Format("15:04"), End: e.now.Add(30 * time.Minute).Format("15:04")}
	})
	e.emit("src_u", "exits", urgentEvent("u1", "https://x.example/1")) // 静默中
	if n := len(e.deliveries()); n != 0 {
		t.Fatalf("delivered during quiet hours: %d", n)
	}
	e.now = e.now.Add(time.Hour)
	if err := e.st.Update(bg, e.att.Tick); err != nil {
		t.Fatal(err)
	}
	if n := len(e.deliveries()); n != 1 {
		t.Fatalf("not released after quiet hours: %d", n)
	}
	update(func(p *policy.Policy) {
		p.Disturb.Quiet = nil
		p.Disturb.Routes = map[string]string{"reminder": "channel:bot"}
		p.Disturb.DailyCap = 1
	})
	e.emit("src_u", "exits", urgentEvent("u2", "https://x.example/2"), urgentEvent("u3", "https://x.example/3"))
	byID := map[string]domain.Delivery{}
	for _, d := range e.deliveries() {
		byID[d.ID] = d
	}
	var capped int
	for _, ev := range e.events("src_u")[1:] {
		if byID[ev.Item].Suppressed == delivery.SuppressedDailyCap {
			capped++
		}
	}
	if capped == 0 {
		t.Fatalf("no delivery suppressed by the daily cap: %+v", byID)
	}
	update(func(p *policy.Policy) {
		p.Disturb.DailyCap = 100
		p.Disturb.Mutes = []policy.Mute{{Scope: policy.MuteSource, ID: "src_u"}}
	})
	e.emit("src_u", "exits", urgentEvent("u4", "https://x.example/4"))
	last := e.events("src_u")[len(e.events("src_u"))-1]
	var muted domain.Delivery
	for _, d := range e.deliveries() {
		if d.ID == last.Item {
			muted = d
		}
	}
	if muted.Suppressed != delivery.SuppressedMuted || muted.Route != delivery.RouteWeb {
		t.Fatalf("muted delivery %+v", muted)
	}
}
