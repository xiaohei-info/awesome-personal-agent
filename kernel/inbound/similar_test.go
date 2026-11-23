package inbound

import (
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func (e *env) setMergeJudge(v string) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "merge", func(p *policy.Policy) error { p.MergeJudge = v; return nil })
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// 同一件事从两个来源来、标题不同：打分模型判为同一件事就合成一条，两个来源与合并依据都留在建议上；
// 本人拆分后各成一条、各留各的来源，重复拆分是冲突，不多建。
func TestCrossSourceSimilarMergeAndSplit(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.cls.PYes(modelstub.Key{Contains: "已有的建议"}, 0.95)
	e.emit("src_a", "ticket", ev("sim-a", "ticket.opened", "确认 AgentSafe AI 资产 B18630823", map[string]any{"sender": "alice"}))
	e.emit("src_b", "ticket", ev("sim-b", "ticket.opened", "AgentSafe 资产确认（B18630823）", map[string]any{"sender": "bob"}))

	ps := e.proposed()
	if len(ps) != 1 {
		t.Fatalf("want one proposal, got %d", len(ps))
	}
	a, b := e.sev("src_a", "sim-a")[0], e.sev("src_b", "sim-b")[0]
	p := ps[0].Proposal
	if !slices.Equal(p.Sources, []string{a, b}) || len(p.Merges) != 1 {
		t.Fatalf("proposal %+v", p)
	}
	m := p.Merges[0]
	if m.Event != b || m.Source != "src_b" || m.By != "classifier" || m.Score < similarMin || m.Judgment == "" || m.Title != "AgentSafe 资产确认（B18630823）" {
		t.Fatalf("trace %+v", m)
	}

	to, err := e.svc.Split(bg, owner, b, ps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	var from, split domain.Item
	for _, it := range e.allItems() {
		switch it.ID {
		case ps[0].ID:
			from = it
		case to:
			split = it
		}
	}
	if !slices.Equal(from.Proposal.Sources, []string{a}) || len(from.Proposal.Merges) != 0 || !slices.Equal(from.Proposal.NotSame, []string{to}) {
		t.Fatalf("from after split %+v", from.Proposal)
	}
	if split.Phase != domain.PhaseProposed || split.Title != m.Title || split.Origin.Source != "src_b" ||
		!slices.Equal(split.Proposal.Sources, []string{b}) || split.Proposal.SplitFrom != ps[0].ID {
		t.Fatalf("split item %+v %+v", split, split.Proposal)
	}
	if evb := e.events("src_b")[0]; evb.Item != to {
		t.Fatalf("event still points at %s", evb.Item)
	}
	if _, err := e.svc.Split(bg, owner, b, ps[0].ID); err == nil {
		t.Fatal("second split of the same source must be a conflict")
	}
	if n := len(e.proposed()); n != 2 {
		t.Fatalf("want two proposals after split, got %d", n)
	}
	e.verify()
}

// 拿不准（打分落在不确定带、对话端点也没给结论）时不合并，只在新建议上提示；关闭合并时不问模型、也不提示。
func TestSimilarUndecidedHintsAndOffSkips(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setMergeJudge(policy.MergeJudgeOnly) // 只用打分模型：带内不交写作模型，取保守默认
	e.cls.PYes(modelstub.Key{Contains: "已有的建议"}, 0.5)
	e.emit("src_a", "ticket", ev("h-a", "ticket.opened", "季度预算评审会议", nil))
	e.emit("src_b", "ticket", ev("h-b", "ticket.opened", "季度预算评审会议改期", nil))
	ps := e.proposed()
	if len(ps) != 2 {
		t.Fatalf("undecided must not merge: %d proposals", len(ps))
	}
	var hinted *domain.Proposal
	for _, it := range ps {
		if len(it.Proposal.Similar) > 0 {
			hinted = it.Proposal
		}
	}
	if hinted == nil || hinted.Similar[0].Score < similarMin {
		t.Fatalf("want a hint on the new proposal: %+v", ps)
	}

	e.setMergeJudge(policy.MergeOff)
	asked := len(e.cls.Batches())
	e.emit("src_c", "ticket", ev("h-c", "ticket.opened", "季度预算评审会议", nil))
	if len(e.cls.Batches()) != asked {
		t.Fatal("merge off must not call the model")
	}
	for _, it := range e.proposed() {
		if it.Origin.Source == "src_c" && len(it.Proposal.Similar) != 0 {
			t.Fatalf("merge off must not hint: %+v", it.Proposal)
		}
	}
}

// 相似度：共有的模板前缀不算相似；共享编号加分；一边是另一边的开头时照常比。
func TestSimilarScore(t *testing.T) {
	for _, c := range []struct {
		a, b string
		hit  bool
	}{
		{"起草：prh-rec-dismiss", "起草：prh 旧事", false},
		{"Re: [org/repo] fix: 登录页 500", "Re: [org/repo] feat: 导出报表", false},
		{"确认 AgentSafe AI 资产", "AgentSafe 资产确认（B18630823）", true},
		{"季度预算评审会议", "季度预算评审会议改期", true},
		{"处理工单 T-6661", "跟进 T-6661 的回复", true},
	} {
		if got := similarScore(c.a, "", c.b, "") >= similarMin; got != c.hit {
			t.Errorf("%q vs %q: score %.2f", c.a, c.b, similarScore(c.a, "", c.b, ""))
		}
	}
}
