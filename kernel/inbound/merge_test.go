package inbound

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// mergeEnv 是入站环境，服务的时钟与账本一致（合并窗口按它算年龄）。
func mergeEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, false)
	e.svc.o.Now = e.Now
	return e
}

// sev 是来源事件（来源给的 id）在账本里的 id，proposal.sources 记的就是它。
func (e *env) sev(source string, eventIDs ...string) (out []string) {
	e.t.Helper()
	all := e.byID(source)
	for _, id := range eventIDs {
		out = append(out, all[id].ID)
	}
	return
}

func (e *env) transition(id string, to domain.Phase) {
	e.t.Helper()
	e.must("item.transition", map[string]any{"id": id, "to": to, "reason": "test"})
}

func (e *env) proposed() (out []domain.Item) {
	for _, it := range e.allItems() {
		if it.Phase == domain.PhaseProposed {
			out = append(out, it)
		}
	}
	return
}

// ACT-PRH-02：两个来源带同一链接（大小写、末尾斜杠不同）只有一条建议，列出两个来源，两条事件的合并键相同。
func TestSameLinkFromTwoSourcesIsOneProposal(t *testing.T) {
	e := mergeEnv(t)
	e.emit("src_a", "ticket", ev("prh-link-a", "ticket.opened", "季度会议安排", map[string]any{"link": "https://example.invalid/meet/1"}))
	e.emit("src_b", "ticket", ev("prh-link-b", "ticket.opened", "季度会议（转发）", map[string]any{"link": "https://EXAMPLE.invalid/meet/1/"}))

	ps := e.proposed()
	if len(ps) != 1 || len(e.allItems()) != 1 {
		t.Fatalf("want one proposal, got %d of %d items", len(ps), len(e.allItems()))
	}
	p := ps[0].Proposal
	if p == nil || !slices.Equal(p.Sources, append(e.sev("src_a", "prh-link-a"), e.sev("src_b", "prh-link-b")...)) {
		t.Fatalf("proposal %+v", p)
	}
	a, b := e.events("src_a")[0], e.events("src_b")[0]
	if a.MergeKey == "" || a.MergeKey != b.MergeKey || a.Item != ps[0].ID || b.Item != ps[0].ID || b.Disposition != domain.DispositionProposed {
		t.Fatalf("events %+v %+v", a, b)
	}
	if es := e.eventsOf(ps[0].ID); countType(es, domain.EvItemUpdated) != 2 { // 写入建议信息一条，并入第二个来源一条
		t.Fatalf("want item.updated for the proposal and for the merge: %+v", es)
	}
	e.verify() // 合并后的事项仍可从事件日志折叠重建
}

// ACT-PRH-03：无链接时按规范化标题加发送者合并；发送者不同的单独成一条。
func TestNoLinkMergesByTitleAndSender(t *testing.T) {
	e := mergeEnv(t)
	alice := map[string]any{"sender": "alice@example.invalid"}
	e.emit("src_a", "ticket", ev("prh-title-a", "ticket.opened", "prh 报销单待签", alice))
	e.emit("src_b", "ticket", ev("prh-title-b", "ticket.opened", "  PRH 报销单待签 ", alice),
		ev("prh-title-c", "ticket.opened", "  PRH 报销单待签 ", map[string]any{"sender": "bob@example.invalid"}))

	ps := e.proposed()
	if len(ps) != 2 {
		t.Fatalf("want two proposals, got %d", len(ps))
	}
	byFirst := map[string][]string{}
	for _, p := range ps {
		byFirst[p.Proposal.Sources[0]] = p.Proposal.Sources
	}
	a, b, c := e.sev("src_a", "prh-title-a")[0], e.sev("src_b", "prh-title-b")[0], e.sev("src_b", "prh-title-c")[0]
	if !slices.Equal(byFirst[a], []string{a, b}) || !slices.Equal(byFirst[c], []string{c}) {
		t.Fatalf("sources %+v", byFirst)
	}
}

// 既无链接又无标题的事件没有可靠的键：不合并，也不会查到任何事项。
func TestNoKeyNoMerge(t *testing.T) {
	if k := mergeKeyOf(domain.SourceEvent{Type: "x", Sender: "alice"}); k != "" {
		t.Fatalf("key %q", k)
	}
	e := mergeEnv(t)
	if into, att, err := e.svc.planMerge(bg, ""); into != "" || att != "" || err != nil {
		t.Fatalf("plan %q %q %v", into, att, err)
	}
}

// 对话捕获与聚合消息不推导合并键：标题相同不代表同一件事。
func TestConversationEventsDeriveNoKey(t *testing.T) {
	for _, ev := range []domain.SourceEvent{
		{SourceID: "capture:prj_x", Subject: "conversation delta"},
		{SourceID: "src_chat", Subject: "项目群", Link: "https://chat.invalid/room/1", Messages: []domain.EventMessage{{Sender: "a"}}},
	} {
		if k := mergeKeyOf(ev); k != "" {
			t.Fatalf("%s: key %q", ev.SourceID, k)
		}
	}
}

// 来源程序自带的合并键优先于内核推导的。
func TestSourceMergeKeyWins(t *testing.T) {
	e := mergeEnv(t)
	e.emit("src_a", "ticket", ev("k1", "ticket.opened", "甲", map[string]any{"merge_key": "mk-1", "link": "https://x.invalid/1"}))
	e.emit("src_b", "ticket", ev("k2", "ticket.opened", "乙", map[string]any{"merge_key": "mk-1", "link": "https://x.invalid/2"}))
	if ps := e.proposed(); len(ps) != 1 || len(ps[0].Proposal.Sources) != 2 {
		t.Fatalf("items %+v", e.allItems())
	}
}

// ACT-PRH-04：命中已确认的事项不自动合并：新建一条建议，推荐补到该事项；该事项不动。
func TestAcceptedItemIsRecommendedNotMerged(t *testing.T) {
	e := mergeEnv(t)
	link := map[string]any{"link": "https://example.invalid/meet/1"}
	e.emit("src_a", "ticket", ev("prh-link-a", "ticket.opened", "季度会议安排", link))
	m1 := e.proposed()[0].ID
	e.transition(m1, domain.PhaseOpen)
	before := len(e.eventsOf(m1))

	e.emit("src_a", "ticket", ev("prh-link-c", "ticket.opened", "季度会议改期", link))
	ps := e.proposed()
	if len(ps) != 1 || ps[0].ID == m1 {
		t.Fatalf("want a new proposal, got %+v", ps)
	}
	p := ps[0].Proposal
	if p.Recommend != "attach" || p.AttachTo != m1 || !slices.Equal(p.Sources, e.sev("src_a", "prh-link-c")) {
		t.Fatalf("proposal %+v", p)
	}
	if got := e.item(m1); got.Phase != domain.PhaseOpen || got.Proposal == nil || !slices.Equal(got.Proposal.Sources, e.sev("src_a", "prh-link-a")) || len(e.eventsOf(m1)) != before {
		t.Fatalf("accepted item must not change: %+v", got)
	}
	e.verify()
}

// ACT-PRH-05：已丢弃的建议不再接收合并，也不复活。
func TestDroppedProposalDoesNotMerge(t *testing.T) {
	e := mergeEnv(t)
	bob := map[string]any{"sender": "bob@example.invalid"}
	e.emit("src_a", "ticket", ev("prh-title-c", "ticket.opened", "prh 报销单待签", bob))
	c := e.proposed()[0].ID
	e.transition(c, domain.PhaseDropped)

	e.emit("src_a", "ticket", ev("prh-title-d", "ticket.opened", "prh 报销单待签", bob))
	ps := e.proposed()
	if len(ps) != 1 || ps[0].ID == c || !slices.Equal(ps[0].Proposal.Sources, e.sev("src_a", "prh-title-d")) || ps[0].Proposal.Recommend != "" {
		t.Fatalf("proposed %+v", ps)
	}
	if got := e.item(c); got.Phase != domain.PhaseDropped || len(got.Proposal.Sources) != 1 {
		t.Fatalf("dropped item changed: %+v", got)
	}
}

// ACT-PRH-06：超过 7 天的建议不再接收合并。
func TestOldProposalDoesNotMerge(t *testing.T) {
	e := mergeEnv(t)
	link := map[string]any{"link": "https://example.invalid/old/1"}
	e.emit("src_a", "ticket", ev("prh-old-a", "ticket.opened", "旧事", link))
	old := e.proposed()[0].ID
	e.now = e.now.Add(8 * 24 * time.Hour)
	e.emit("src_b", "ticket", ev("prh-old-b", "ticket.opened", "旧事", link))

	if got := e.item(old); got.Phase != domain.PhaseProposed || !slices.Equal(got.Proposal.Sources, e.sev("src_a", "prh-old-a")) {
		t.Fatalf("old proposal %+v", got.Proposal)
	}
	if len(e.proposed()) != 2 {
		t.Fatalf("want a second proposal, got %+v", e.allItems())
	}
}

// 7 天内（含）仍在 proposed 的才并入。
func TestMergeWindowEdge(t *testing.T) {
	e := mergeEnv(t)
	link := map[string]any{"link": "https://example.invalid/edge/1"}
	e.emit("src_a", "ticket", ev("e1", "ticket.opened", "边界", link))
	e.now = e.now.Add(6 * 24 * time.Hour)
	e.emit("src_b", "ticket", ev("e2", "ticket.opened", "边界", link))
	if ps := e.proposed(); len(ps) != 1 || len(ps[0].Proposal.Sources) != 2 {
		t.Fatalf("items %+v", e.allItems())
	}
}

// 手动把积压事件转建议同样走合并（§9.3 "转建议只走第 6 步"）。
func TestManualProposeMerges(t *testing.T) {
	e := mergeEnv(t)
	link := map[string]any{"link": "https://example.invalid/m/1"}
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "合同", link))
	e.emit("src_m", "inbox", ev("m1", "mail.received", "合同（邮件）", link))
	id := e.events("src_m")[0].ID

	got, err := e.svc.ProposeUnjudged(bg, owner, id)
	if err != nil {
		t.Fatal(err)
	}
	ps := e.proposed()
	if len(ps) != 1 || got.Item != ps[0].ID || got.Disposition != domain.DispositionProposed || got.MergeKey == "" || len(ps[0].Proposal.Sources) != 2 {
		t.Fatalf("event %+v items %+v", got, ps)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, id)
		if last := es[len(es)-1].Event; last.Type != EvDisposed || last.Actor.Principal != "owner" {
			t.Fatalf("last %+v", last)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.verify()
}

// 建议信息只由入站流水线写入：任何凭据经 item.create 带 proposal 都被拒绝，没有事项被建出。
func TestItemCreateRejectsProposal(t *testing.T) {
	e := mergeEnv(t)
	_, err := e.do(owner, "item.create", "owner said so", map[string]any{"kind": "task", "title": "伪造",
		"proposal": map[string]any{"recommend": "attach", "attach_to": "itm_nope", "sources": []string{"x"}, "merge_key": "link:https://forged.invalid"}})
	if code(err) != domain.CodeInvalidRequest || len(e.allItems()) != 0 {
		t.Fatalf("err %v items %+v", err, e.allItems())
	}
}

// followEnv 是开着归类、接上跟进消息归属（同 apa serve）的入站环境。
func followEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	fm, err := followmsg.New(followmsg.Options{Store: e.st, Exec: e.x, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.svc.o.Intercept = fm.Intercept
	return e
}

// 线上 2026-11-23 那批的形状（同一个 PR 的通知分三次拉取，来自两个邮箱和代码托管平台；邮件适配器已按附录 D.2 补上 PR 链接；
// 跟进消息归属照线上接入）：每一轮里后到的、以及之后几轮的，都按"同一链接"并进第一条，从头到尾只建 1 条建议，没有建了又丢的中间建议；
// 同一会话的回信与平台上同一 PR 的更新挂到这一条上，不改它的标题（它不是那个 PR 的事件建出的）；归类时它照常进批，没有可合的，留着。
func TestSamePRAcrossMailAndPlatformIsOneProposal(t *testing.T) {
	e := followEnv(t)
	const gh, link = "notifications@github.com", "https://github.com/org/app/pull/227"
	subj := "[org/app] feat(desktop): 能力页「帮我安装」卡片移到最上方并改标题 (PR #227)"
	mail := func(id, subject, thread string) map[string]any {
		return ev(id, "ticket.opened", subject, map[string]any{"sender": gh, "link": link, "correlation_keys": []string{"thread:" + thread}})
	}
	pr := func(id, typ string) map[string]any {
		return ev(id, typ, "feat(desktop): 能力页「帮我安装」卡片移到最上方并改标题", map[string]any{"sender": "dev", "link": link, "correlation_keys": []string{"org/app#227"}})
	}
	e.emit("src_mail_icloud", "ticket", mail("i1", "Re: "+subj, "org/app/pull/227/issue_event/1@github.com"), mail("i2", subj, "org/app/pull/227@github.com"))
	e.emit("src_mail", "ticket", mail("m1", subj, "<org/app/pull/227@github.com>"), mail("m2", "Re: "+subj, "<org/app/pull/227@github.com>"),
		mail("m3", "Re: "+subj, "<org/app/pull/227@github.com>"))
	e.emit("src_github", "ticket", pr("g1", "ticket.opened"), pr("g2", "ticket.updated"))

	all := e.allItems()
	if len(all) != 1 || all[0].Phase != domain.PhaseProposed || all[0].Title != "Re: "+subj {
		t.Fatalf("want exactly one proposal, titled by the first message, and nothing else, got %+v", all)
	}
	p := all[0].Proposal
	want := append(append(e.sev("src_mail_icloud", "i1", "i2"), e.sev("src_mail", "m1", "m2", "m3")...), e.sev("src_github", "g1", "g2")...)
	if !slices.Equal(p.Sources, want) || p.MergeKey != "link:"+link || len(p.Merges) != 6 {
		t.Fatalf("proposal %+v", p)
	}
	for i, m := range p.Merges {
		if m.By != "merge_key" || m.Item != "" || m.Event != want[i+1] {
			t.Fatalf("merged by the rule at creation, not folded from another proposal: %+v", m)
		}
	}
	for _, x := range append(append(e.events("src_mail_icloud"), e.events("src_mail")...), e.events("src_github")...) {
		if x.Item != all[0].ID || x.MergeKey != "link:"+link {
			t.Fatalf("event %+v", x)
		}
	}
	e.verify()
}

// 没被折叠的平台建议：同一对象之后的更新（合并、改动、请求评审）挂到它上面时记进它的消息清单，卡片上「N 条消息 → 这一条」列得到；
// 它们仍是挂靠消息（不是合并），没有合并留痕。
func TestPlatformUpdatesAreListedOnTheProposal(t *testing.T) {
	e := followEnv(t)
	pr := func(id, typ string) map[string]any {
		return ev(id, typ, "fix: 设置页保存按钮", map[string]any{"sender": "dev", "link": "https://github.com/org/app/pull/228", "correlation_keys": []string{"org/app#228"}})
	}
	e.emit("src_github", "pr", pr("h1", "pr.opened"))
	e.emit("src_github", "pr", pr("h2", "pr.updated"), pr("h3", "pr.merged"), pr("h4", "notification.review_requested"))
	ps := e.proposed()
	if len(ps) != 1 || len(e.allItems()) != 1 || !slices.Equal(ps[0].Proposal.Sources, e.sev("src_github", "h1", "h2", "h3", "h4")) || len(ps[0].Proposal.Merges) != 0 {
		t.Fatalf("one proposal listing all four: %+v", e.allItems())
	}
	for _, x := range e.events("src_github")[1:] {
		if x.Item != ps[0].ID || x.Disposition != domain.DispositionAttached || x.AttachState != "processed" {
			t.Fatalf("update stays an attached message: %+v", x)
		}
	}
	e.verify()
}

// ACT-PRH-02 的补充：并入已有建议时把新事件带来的统一时间字段补到事项上（§9.2、§9.3）。
// 只补缺的字段：已有值（含本人改过的）不覆盖；补上的字段记 source_* 溯源，跟随来源改期；
// 补进来的截止在建议被接受时按它排上截止 Timer（proposed 阶段按设计不计时，§6.8）。
func TestMergeFillsMissingTimesOnly(t *testing.T) {
	e := mergeEnv(t)
	key := map[string]any{"merge_key": "mk-time"}
	e.emit("src_a", "ticket", ev("tm-a", "ticket.opened", "季度评审", key))
	id := e.proposed()[0].ID
	if it := e.item(id); it.Deadline != nil || it.PlannedFor != "" {
		t.Fatalf("the first event brings no times: %+v", it)
	}

	// 第二个来源带来截止与起止：事项上还没有，补进去。
	e.emit("src_b", "ticket", ev("tm-b", "ticket.opened", "季度评审（定时间）", map[string]any{"merge_key": "mk-time",
		"due_at": "2026-03-20", "starts_at": "2026-03-21T09:00:00+08:00", "ends_at": "2026-03-21T10:00:00+08:00"}))
	it := e.item(id)
	loc := time.Local
	deadline := time.Date(2026, 3, 20, 23, 59, 59, 0, loc)
	start := time.Date(2026, 3, 21, 1, 0, 0, 0, time.UTC).In(loc)
	wantPlan, wantSlot := start.Format(time.DateOnly), start.Format("15:04")+" - "+start.Add(time.Hour).Format("15:04")
	if it.Deadline == nil || !it.Deadline.Equal(deadline) || it.PlannedFor != wantPlan || it.PlannedSlot != wantSlot {
		t.Fatalf("times of the merged event were not filled: deadline %v plan %q %q", it.Deadline, it.PlannedFor, it.PlannedSlot)
	}
	var ext map[string]string
	if err := json.Unmarshal(it.Ext, &ext); err != nil {
		t.Fatal(err)
	}
	if ext["source_deadline"] != deadline.Format(time.RFC3339) || ext["source_planned_for"] != wantPlan || ext["source_planned_slot"] != wantSlot {
		t.Fatalf("ext %+v", ext)
	}

	// 第三个来源带来别的截止：已有值不覆盖。
	e.emit("src_c", "ticket", ev("tm-c", "ticket.opened", "季度评审（又改）", map[string]any{"merge_key": "mk-time", "due_at": "2026-04-01"}))
	if got := e.item(id); got.Deadline == nil || !got.Deadline.Equal(deadline) {
		t.Fatalf("an existing deadline must not be overwritten: %v", got.Deadline)
	}

	// 接受后按补进来的截止排上 Timer。
	e.transition(id, domain.PhaseOpen)
	var (
		tm domain.Timer
		ok bool
	)
	e.view(func(tx *store.Tx) { tm, ok, _ = clock.Get(tx, "due_"+id) })
	if !ok || !tm.At.Equal(deadline) {
		t.Fatalf("due timer %+v, want the filled deadline %v", tm, deadline)
	}
	e.verify()
}
