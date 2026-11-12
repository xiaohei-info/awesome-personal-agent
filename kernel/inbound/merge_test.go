package inbound

import (
	"slices"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// mergeEnv 是入站环境，服务的时钟与账本一致（合并窗口按它算年龄）。
func mergeEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, false)
	e.svc.o.Now = e.Now
	return e
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
	if p == nil || !slices.Equal(p.Sources, []string{"prh-link-a", "prh-link-b"}) {
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
	if !slices.Equal(byFirst["prh-title-a"], []string{"prh-title-a", "prh-title-b"}) || !slices.Equal(byFirst["prh-title-c"], []string{"prh-title-c"}) {
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
	if p.Recommend != "attach" || p.AttachTo != m1 || !slices.Equal(p.Sources, []string{"prh-link-c"}) {
		t.Fatalf("proposal %+v", p)
	}
	if got := e.item(m1); got.Phase != domain.PhaseOpen || got.Proposal == nil || !slices.Equal(got.Proposal.Sources, []string{"prh-link-a"}) || len(e.eventsOf(m1)) != before {
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
	if len(ps) != 1 || ps[0].ID == c || !slices.Equal(ps[0].Proposal.Sources, []string{"prh-title-d"}) || ps[0].Proposal.Recommend != "" {
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

	if got := e.item(old); got.Phase != domain.PhaseProposed || !slices.Equal(got.Proposal.Sources, []string{"prh-old-a"}) {
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
