package inbound

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// 批量只有一个数：batch_size 既是"攒满就跑"的条数，也是一批的上限；不足时等满 wait_minutes 才跑。
func TestMergeBatchSizeIsOneNumber(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge, p.Merge.BatchSize = policy.MergeSame, 4 })
	e.stub.WhenFunc(batchMark, keepAll())
	add := func(n int) {
		for range n {
			e.now = e.now.Add(time.Second)
			k := len(e.allItems())
			e.emit(fmt.Sprintf("src_%02d", k), "ticket", ev(fmt.Sprintf("s%02d", k), "ticket.opened", fmt.Sprintf("互不相干的第 %02d 件", k), nil))
		}
	}
	add(3)
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 0 {
		t.Fatalf("3 < batch_size and not waited: %v %d", err, e.batchCalls())
	}
	add(3) // 6 条：攒满 4 条立刻跑，且一批只取 4 条
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("reaching batch_size runs at once: %v %d", err, e.batchCalls())
	}
	done, pending := 0, 0
	for _, it := range e.proposed() {
		switch sw := it.Proposal.Sweep; sw.State {
		case "pending":
			pending++
		default:
			done++
			if sw.Size != 4 {
				t.Fatalf("batch size: %+v", sw)
			}
		}
	}
	if done != 4 || pending != 2 {
		t.Fatalf("the same number caps the batch: done %d pending %d", done, pending)
	}
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("2 left wait: %v %d", err, e.batchCalls())
	}
	e.now = e.now.Add(policy.DefaultMergeWait * time.Minute)
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 2 {
		t.Fatalf("after wait_minutes the rest runs: %v %d", err, e.batchCalls())
	}
	e.verify()
}

// 正文级整合：载体的正文换成模型给的逐条正文，原正文进 integrated.body；正文缺任一成员的数字或编号、或正文为空都退回保留。
// 同一批再跑零变化；撤销后正文还原。
func TestMergeIntegrationRewritesBody(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting = policy.MergeSame, 6, true })
	e.stub.When(batchMark, reply(
		`{"group":"G1","topic":"提现通知","members":["N1","N2"]},{"group":"G2","topic":"资金费","members":["N3","N4"]},{"group":"G3","topic":"扣款","members":["N5","N6"]}`,
		`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"都只作记录","confident":true},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"都只作记录","confident":true},
		 {"new":"N3","group":"G2","step":2,"action":"merge","target":"M2","reason":"都只作记录","confident":true},
		 {"new":"N4","group":"G2","step":2,"action":"merge","target":"M2","reason":"都只作记录","confident":true},
		 {"new":"N5","group":"G3","step":2,"action":"merge","target":"M3","reason":"都只作记录","confident":true},
		 {"new":"N6","group":"G3","step":2,"action":"merge","target":"M3","reason":"都只作记录","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N1","N2"],"title":"提现完成 500 与 120 USDT","summary":"两笔提现已完成：\n- 11-16 提现 500 USDT\n- 11-19 提现 120 USDT",
		  "body":"- 2026-11-16，邮件，交易所账户：提现 500 USDT，结论：已完成\n- 2026-11-19，邮件，交易所账户：提现 120 USDT，结论：已完成"},
		 {"merge":"M2","group":"G2","members":["N3","N4"],"title":"资金费结算 8.5 与 3.2 USDT","summary":"两次资金费：\n- 08:00 结算 8.5 USDT\n- 16:00 结算 3.2 USDT",
		  "body":"- 08:00 结算 8.5 USDT，结论：已结算\n- 另一次也已结算"},
		 {"merge":"M3","group":"G3","members":["N5","N6"],"title":"自动扣款成功 K3301 与 K3302","summary":"两笔扣款：K3301、K3302","body":"  "}`))
	for i, title := range []string{"提现 500 USDT 已完成", "提现 120 USDT 已完成", "资金费 08:00 结算 8.5 USDT", "资金费 16:00 结算 3.2 USDT", "自动扣款成功 K3301", "自动扣款成功 K3302"} {
		e.now = e.now.Add(time.Minute)
		e.emit(fmt.Sprintf("src_%d", i), "ticket", ev(fmt.Sprintf("w%d", i), "ticket.opened", title, nil))
	}
	n1 := e.byTitle("提现 500 USDT 已完成")
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || len(rep.Combined) != 3 {
		t.Fatalf("run: %v %+v", err, rep)
	}
	carrier := e.byTitle("提现完成 500 与 120 USDT")
	in := carrier.Proposal.Integrated
	if carrier.ID != n1.ID || in == nil || carrier.Body != in.ToBody || in.Body != n1.Body || !strings.Contains(carrier.Body, "- 2026-11-19，邮件，交易所账户：提现 120 USDT") {
		t.Fatalf("the body is rewritten item by item, the original kept: %q %+v", carrier.Body, in)
	}
	// M2 正文漏了 16:00 与 3.2：退回保留；M3 正文为空：整合不成立。两组都原样。
	for title, code := range map[string]string{"资金费 08:00 结算 8.5 USDT": domain.RejectFactsMissing, "资金费 16:00 结算 3.2 USDT": domain.RejectFactsMissing,
		"自动扣款成功 K3301": domain.RejectCombine, "自动扣款成功 K3302": domain.RejectCombine} {
		it := e.byTitle(title)
		if it.Phase != domain.PhaseProposed || it.Proposal.Integrated != nil || it.Proposal.Sweep.Rejected != code || it.Body != "外部正文："+title {
			t.Fatalf("%s stays as it was with %s: %+v", title, code, it.Proposal.Sweep)
		}
	}
	if c := rep.Combined[1]; c.Rejected != domain.RejectFactsMissing || strings.Join(c.Missing, ",") != "正文：16:00,正文：3.2" {
		t.Fatalf("the report names what the body misses: %+v", c)
	}
	if rep.CheckFailed != 1 || rep.Rejected[domain.RejectFactsMissing] != 2 || rep.Rejected[domain.RejectCombine] != 2 {
		t.Fatalf("report: %d %v", rep.CheckFailed, rep.Rejected)
	}
	e.verify()

	// 幂等：同一批再跑，第二遍零变化、不调模型。
	h, calls := e.head(), e.batchCalls()
	if rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 0 || e.head() != h || e.batchCalls() != calls {
		t.Fatalf("second pass changes nothing: %v %+v", err, rep)
	}

	// 撤销：正文还原成整合前的原文。
	if err := e.svc.Unmerge(bg, owner, e.byTitle("提现 120 USDT 已完成").ID); err != nil {
		t.Fatal(err)
	}
	if back := e.item(n1.ID); back.Title != n1.Title || back.Body != n1.Body || back.Proposal.Integrated != nil {
		t.Fatalf("undo restores the body: %+v", back)
	}
	e.verify()
}

// 主题可持续：没有历史时资料写"还没有"、主题全是新建；下一批资料列出最近几批的主题名，模型沿用的记 reused，新起的不记。
func TestMergeTopicsCarryOver(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge, p.Merge.SkipExisting = policy.MergeSame, true })
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"Bitget 提现通知","members":["N1","N2"]}`,
		`{"new":"N1","group":"G1","step":3,"action":"keep","reason":"r","confident":true},{"new":"N2","group":"G1","step":3,"action":"keep","reason":"r","confident":true}`, ``))
	e.emit("src_a", "ticket", ev("t1", "ticket.opened", "Bitget 提现 500 USDT 已完成", nil))
	e.emit("src_b", "ticket", ev("t2", "ticket.opened", "Bitget 提现 120 USDT 已完成", nil))
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || len(rep.Topics) != 0 || len(rep.Groups) != 1 || rep.Groups[0].Reused || !strings.Contains(e.lastBatchBody(), "# 已有主题（最近 20 批用过的）\\n\\n（还没有）") {
		t.Fatalf("no history: everything is new: %v %+v", err, rep.Groups)
	}

	e.stub.Reset()
	e.configureModel()
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"Bitget 提现通知","members":["N1"]},{"group":"G2","topic":"GitHub CI 失败","members":["N2"]}`,
		`{"new":"N1","group":"G1","step":3,"action":"keep","reason":"r","confident":true},{"new":"N2","group":"G2","step":3,"action":"keep","reason":"r","confident":true}`, ``))
	e.now = e.now.Add(time.Hour)
	e.emit("src_c", "ticket", ev("t3", "ticket.opened", "Bitget 提现 80 USDT 已完成", nil))
	e.emit("src_d", "ticket", ev("t4", "ticket.opened", "org/app CI 失败 run 9921", nil))
	rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || strings.Join(rep.Topics, ",") != "Bitget 提现通知" || !strings.Contains(e.lastBatchBody(), "已有主题（最近 20 批用过的）\\n\\n- Bitget 提现通知") {
		t.Fatalf("the previous batch's topics are offered: %v %v", err, rep.Topics)
	}
	if len(rep.Groups) != 2 || !rep.Groups[0].Reused || rep.Groups[1].Reused {
		t.Fatalf("reused vs new: %+v", rep.Groups)
	}
	if sw := e.byTitle("Bitget 提现 80 USDT 已完成").Proposal.Sweep; sw.Topic != "Bitget 提现通知" {
		t.Fatalf("the reused topic lands: %+v", sw)
	}
	e.verify()
}

// 第 4 层只检索、只排序：跨项目、同一仓库另一个 PR 的事项照样是候选；按分数从高到低，不设下限（低分的也在），截到 merge.candidates 条。
func TestMergeRetrievalRanksOnly(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	other := e.openIn("季度预算评审会议", e.mail) // 另一个项目
	same := e.openIn("季度预算评审会议纪要", e.web)
	pr := e.openItem("[org/app] fix A (PR #220) 审阅 T-6661")
	e.openItem("导出报表加一列")
	e.openItem("订阅续费提醒")
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame })
	m := e.proposal("src_a", "a1", "季度预算评审会议改期", e.web)
	d := e.proposal("src_b", "b1", "[org/app] fix A (PR #221) T-6661", "")
	cands := func(rep ManualReport, item string) []ManualRef {
		for _, a := range rep.Ask {
			if a.Item == item {
				return a.Candidates
			}
		}
		t.Fatalf("%s not asked", item)
		return nil
	}
	rep, err := e.svc.PlanManual(bg, ManualFilter{})
	if err != nil {
		t.Fatal(err)
	}
	all := cands(rep, m.ID)
	if len(all) != 5 {
		t.Fatalf("no threshold: every existing item is a candidate: %+v", all)
	}
	for i := 1; i < len(all); i++ {
		if all[i].Score > all[i-1].Score {
			t.Fatalf("sorted by score: %+v", all)
		}
	}
	if all[len(all)-1].Score >= 0.30 || (all[0].Item != other && all[0].Item != same) {
		t.Fatalf("the look-alikes come first, low scores are still there: %+v", all)
	}
	if c := cands(rep, d.ID); c[0].Item != pr {
		t.Fatalf("another PR of the same repository is not blocked: %+v", c)
	}

	e.setPolicy(func(p *policy.Policy) { p.Merge.Candidates = 2 })
	rep, _ = e.svc.PlanManual(bg, ManualFilter{})
	top := cands(rep, m.ID)
	if len(top) != 2 || top[0].Item != all[0].Item || top[1].Item != all[1].Item {
		t.Fatalf("cut to merge.candidates: %+v", top)
	}
	for _, x := range top {
		if x.Item == other {
			return // 跨项目的仍在前 2 条里：检索不拦
		}
	}
	t.Fatalf("the cross-project item is ranked, not blocked: %+v", top)
}

// 第 6 层：五类禁止规则各自拒绝并记类别码（跨项目、同仓库不同 PR、去处不在这一条的候选里、整合缺关键事实、结论与去处状态不相配），
// 被拒的都留着；报告按码计数。同一批再跑零变化。
func TestMergeVetoCodes(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	p1 := e.openIn("评审 AgentSafe 资产 B18630823", e.web)
	p2 := e.openItem("周报模板改版 W4701")
	e.transition(p2, domain.PhaseDone)
	p3 := e.openItem("[org/app] fix A (PR #220) 审阅 T-6661")
	p4 := e.openItem("导出报表加一列 Q7781")
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge, p.Merge.Candidates = policy.MergeSame, 1 })
	na := e.proposal("src_a", "a1", "AgentSafe 资产确认（B18630823）", e.mail)
	e.now = e.now.Add(time.Minute)
	nb := e.proposal("src_b", "b1", "[org/app] fix A (PR #221) T-6661", "")
	e.now = e.now.Add(time.Minute)
	nc := e.proposal("src_c", "c1", "周报模板改版（W4701 第二次提醒）", "")
	e.now = e.now.Add(time.Minute)
	nd := e.proposal("src_d", "d1", "导出报表加一列 Q7781 补充", "")
	e.now = e.now.Add(time.Minute)
	ne := e.proposal("src_e", "e1", "提现 500 USDT 已完成", "")
	e.now = e.now.Add(time.Minute)
	nf := e.proposal("src_f", "f1", "提现 120 USDT 已完成", "")

	dry, err := e.svc.PlanManual(bg, ManualFilter{})
	if err != nil || len(dry.Ask) != 6 {
		t.Fatalf("dry: %v %+v", err, dry)
	}
	for i, want := range []string{p1, p3, p2, p4} { // 每条只带分数最高的 1 条：E1..E4 依次是它们
		if c := dry.Ask[i].Candidates; len(c) != 1 || c[0].Item != want || c[0].Num != fmt.Sprintf("E%d", i+1) {
			t.Fatalf("N%d top candidate: %+v", i+1, c)
		}
	}
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"杂项","members":["N1","N2","N3","N4","N5","N6"]}`,
		`{"new":"N1","group":"G1","step":1,"action":"attach","target":"E1","reason":"同一个资产编号","confident":true},
		 {"new":"N2","group":"G1","step":1,"action":"attach","target":"E2","reason":"同一仓库","confident":true},
		 {"new":"N3","group":"G1","step":1,"action":"attach","target":"E3","reason":"同一次改版","confident":true},
		 {"new":"N4","group":"G1","step":1,"action":"attach","target":"E1","reason":"编到别人的候选","confident":true},
		 {"new":"N5","group":"G1","step":2,"action":"merge","target":"M1","reason":"都只作记录","confident":true},
		 {"new":"N6","group":"G1","step":2,"action":"merge","target":"M1","reason":"都只作记录","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N5","N6"],"title":"两笔提现已完成","summary":"提现 500 USDT、120 USDT 已完成","body":"- 提现 500 USDT 已完成"}`))
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		it   domain.Item
		code string
	}{{na, domain.RejectCrossProject}, {nb, domain.RejectOtherChange}, {nc, domain.RejectActionMismatch}, {nd, domain.RejectNotCandidate},
		{ne, domain.RejectFactsMissing}, {nf, domain.RejectFactsMissing}} {
		got := e.item(c.it.ID)
		if sw := got.Proposal.Sweep; got.Phase != domain.PhaseProposed || sw.Action != domain.SweepKeep || sw.Rejected != c.code || sw.Why == "" {
			t.Fatalf("%s must be kept with %s: %s %+v", c.it.Title, c.code, got.Phase, sw)
		}
	}
	want := map[string]int{domain.RejectCrossProject: 1, domain.RejectOtherChange: 1, domain.RejectActionMismatch: 1, domain.RejectNotCandidate: 1, domain.RejectFactsMissing: 2}
	if fmt.Sprint(rep.Rejected) != fmt.Sprint(want) {
		t.Fatalf("rejections by code: %v", rep.Rejected)
	}
	for _, id := range []string{p1, p2, p3, p4} {
		if it := e.item(id); it.Proposal != nil && len(it.Proposal.Absorbed) != 0 {
			t.Fatalf("nothing folded into %s", id)
		}
	}
	h, calls := e.head(), e.batchCalls()
	if rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 0 || e.head() != h || e.batchCalls() != calls {
		t.Fatalf("second pass changes nothing: %v %+v", err, rep)
	}
	e.verify()
}
