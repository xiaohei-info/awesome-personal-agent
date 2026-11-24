package inbound

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// retryEnv 先造出三种已归类的建议：被校验退回的两条（R，提现 840 USDT 被模型漏写）、模型自己留着的两条（K）、正常合并的两条（M）。
// 之后 R 那一组的回答：stillBad 为假是改好了（保留金额），为真是仍然漏写。
type retryEnv struct {
	*env
	r, k, m [2]domain.Item
}

func newRetryEnv(t *testing.T, stillBad bool) *retryEnv {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) {
		p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting = policy.MergeSame, 2, true
	})
	merge := func(title, summary, list string) modelstub.Reply {
		return reply(`{"group":"G1","topic":"t","members":["N1","N2"]}`,
			`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一件事","confident":true},
			 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一件事","confident":true}`,
			fmt.Sprintf(`{"merge":"M1","group":"G1","members":["N1","N2"],"title":%q,"summary":%q,"body":%q}`, title, summary, list))
	}
	asked := 0 // R 那一组被问到的次数：第一次漏写金额，之后按 stillBad 决定
	e.stub.WhenFunc(batchMark, func(body string) modelstub.Reply {
		switch {
		case strings.Contains(body, "提现 840 USDT"):
			asked++
			if asked == 1 || stillBad {
				return merge("提现回执", "提现已到账，回执补发", "- 提现已到账\n- 回执补发")
			}
			return merge("提现回执 840 USDT", "提现 840 USDT 已到账，回执补发", "- 提现 840 USDT 已到账\n- 回执补发")
		case strings.Contains(body, "周报提醒"):
			return keepAll()(body)
		}
		return merge("会议改期 2026-12-01", "会议改期到 2026-12-01，邮件已确认", "- 会议改期到 2026-12-01\n- 邮件确认 2026-12-01")
	})
	x := &retryEnv{env: e}
	for i, titles := range [][2]string{{"提现 840 USDT 已到账", "提现回执补发"}, {"周报提醒甲", "周报提醒乙"}, {"会议改期 2026-12-01 通知", "会议改期邮件 2026-12-01"}} {
		for j, title := range titles {
			e.now = e.now.Add(time.Minute)
			e.emit(fmt.Sprintf("src_%d%d", i, j), "ticket", ev(fmt.Sprintf("w%d%d", i, j), "ticket.opened", title, nil))
		}
		for j, title := range titles { // 整合会改载体的标题，先按原题取
			[]*[2]domain.Item{&x.r, &x.k, &x.m}[i][j] = e.byTitle(title)
		}
		if rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 2 {
			t.Fatalf("batch %d: %v %+v", i, err, rep)
		}
	}
	// 前提：R 被退回，K 留着没有拒绝码，M 合并了。
	for _, it := range x.r {
		if sw := e.item(it.ID).Proposal.Sweep; sw.Rejected != domain.RejectFactsMissing || sw.Action != domain.SweepKeep {
			t.Fatalf("setup: %s should be rejected: %+v", it.Title, sw)
		}
	}
	for _, it := range x.k {
		if sw := e.item(it.ID).Proposal.Sweep; sw.Rejected != "" || sw.Action != domain.SweepKeep || sw.State != "done" {
			t.Fatalf("setup: %s should be kept by the model: %+v", it.Title, sw)
		}
	}
	if n := countFolded(e, x.m); n != 1 {
		t.Fatalf("setup: M should be merged (1 folded), got %d", n)
	}
	return x
}

func countFolded(e *env, its [2]domain.Item) (n int) {
	for _, it := range its {
		if e.item(it.ID).Proposal.Sweep.Folded() {
			n++
		}
	}
	return
}

// 演练只列出将纳入哪些，不写账本；真跑只纳入上次被退回的两条：已保留的、已合并的原样不动（批号、版本不变），
// 来源写明是哪一次判定、什么拒绝码；改好的回答这次通过；连跑第二遍零变化、不再调模型。
func TestMergeRetryRejected(t *testing.T) {
	e := newRetryEnv(t, false)
	oldBatch := e.item(e.r[0].ID).Proposal.Sweep.Batch
	state := func(id string) string {
		it := e.item(id)
		return fmt.Sprintf("%s/%s/%s/%d/%v", it.Proposal.Sweep.Batch, it.Proposal.Sweep.Action, it.Proposal.Sweep.Target, it.Version, it.Proposal.Sweep.Retry)
	}
	before := map[string]string{}
	for _, it := range append(e.k[:], e.m[:]...) {
		before[it.ID] = state(it.ID)
	}

	// 不带开关：已归类的不再命中，跑了也是零变化。
	h, calls := e.head(), e.batchCalls()
	if rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 0 || rep.Retry != nil || e.head() != h {
		t.Fatalf("without the switch nothing is picked up: %v %+v", err, rep)
	}

	// 演练：列出将纳入的两条与来源，账本不动。
	dry, err := e.svc.PlanManual(bg, ManualFilter{RetryRejected: true})
	if err != nil || dry.Retry == nil || !dry.Retry.DryRun || len(dry.Retry.Requeued) != 2 || dry.Matched != 2 || e.head() != h {
		t.Fatalf("dry run lists what would be retried and writes nothing: %v %+v", err, dry)
	}
	if e.item(e.r[0].ID).Proposal.Sweep.State != "done" {
		t.Fatal("dry run did not touch the ledger")
	}

	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{RetryRejected: true})
	if err != nil || rep.Retry == nil || len(rep.Retry.Requeued) != 2 || rep.Retry.ByBatch[oldBatch] != 2 || rep.Retry.ByCode[domain.RejectFactsMissing] != 2 || rep.Retry.Notice != "" {
		t.Fatalf("two rejected proposals come back, from the batch that rejected them: %v %+v", err, rep.Retry)
	}
	for _, r := range rep.Retry.Requeued {
		if r.Of != oldBatch || r.Rejected != domain.RejectFactsMissing || !strings.Contains(r.Why, "缺少") || r.Group == "" {
			t.Fatalf("each one says which judgement it came from: %+v", r)
		}
	}
	if e.batchCalls() != calls+1 || rep.Matched != 2 || len(rep.Combined) != 1 || rep.Combined[0].Rejected != "" {
		t.Fatalf("the fixed answer keeps 840 USDT and goes through: %d %+v", e.batchCalls()-calls, rep.Combined)
	}
	if len(rep.Decisions) != 2 {
		t.Fatalf("decisions: %+v", rep.Decisions)
	}
	for _, d := range rep.Decisions {
		if d.RetryOf != oldBatch {
			t.Fatalf("decisions point back to the old judgement: %+v", d)
		}
	}
	if countFolded(e.env, e.r) != 1 {
		t.Fatalf("the retried pair is merged now")
	}
	for id, was := range before { // 已保留的、已合并的原样
		if now := state(id); now != was {
			t.Fatalf("%s is not touched: %s -> %s", id, was, now)
		}
	}
	e.verify()

	// 第二遍：零变化，不调模型，提示没有可纳入的。
	h, calls = e.head(), e.batchCalls()
	rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{RetryRejected: true})
	if err != nil || rep.Matched != 0 || e.head() != h || e.batchCalls() != calls || rep.Retry == nil || len(rep.Retry.Requeued) != 0 || !strings.Contains(rep.Retry.Notice, "没有可重新纳入") {
		t.Fatalf("second pass changes nothing and says why: %v %+v", err, rep.Retry)
	}
}

// 重判后仍被退回的（模型还是漏写）不会被一直重复纳入：第二遍零变化，提示写明有几条重判过一次仍被退回；
// 来源批号与拒绝码留在记录里。
func TestMergeRetryRejectedStillBad(t *testing.T) {
	e := newRetryEnv(t, true)
	oldBatch := e.item(e.r[0].ID).Proposal.Sweep.Batch
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{RetryRejected: true})
	if err != nil || len(rep.Retry.Requeued) != 2 || rep.Rejected[domain.RejectFactsMissing] != 2 {
		t.Fatalf("retried and rejected again: %v %+v", err, rep)
	}
	sw := e.item(e.r[0].ID).Proposal.Sweep
	if sw.Retry == nil || sw.Retry.Of != oldBatch || sw.Retry.Rejected != domain.RejectFactsMissing || sw.Batch == oldBatch {
		t.Fatalf("the record keeps where it came from: %+v", sw)
	}
	h, calls := e.head(), e.batchCalls()
	rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{RetryRejected: true})
	if err != nil || e.head() != h || e.batchCalls() != calls || len(rep.Retry.Requeued) != 0 || rep.Retry.Skipped["already_retried"] != 2 || !strings.Contains(rep.Retry.Notice, "重判过一次") {
		t.Fatalf("a proposal rejected again is not retried again: %v %+v", err, rep.Retry)
	}
}

// 没有被退回的建议时给出明确提示：只有模型自己留着的。
func TestMergeRetryRejectedNothingToRetry(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) {
		p.MergeJudge, p.Merge.BatchSize, p.Merge.SkipExisting = policy.MergeSame, 2, true
	})
	e.stub.WhenFunc(batchMark, keepAll())
	for i, title := range []string{"周报提醒甲", "周报提醒乙"} {
		e.now = e.now.Add(time.Minute)
		e.emit(fmt.Sprintf("src_%d", i), "ticket", ev(fmt.Sprintf("w%d", i), "ticket.opened", title, nil))
	}
	if _, err := e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil {
		t.Fatal(err)
	}
	h, calls := e.head(), e.batchCalls()
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{RetryRejected: true})
	if err != nil || e.head() != h || e.batchCalls() != calls || rep.Retry == nil || len(rep.Retry.Requeued) != 0 || !strings.Contains(rep.Retry.Notice, "没有可重新纳入") {
		t.Fatalf("kept proposals are not picked up, and the report says so: %v %+v", err, rep.Retry)
	}
	if dry, err := e.svc.PlanManual(bg, ManualFilter{RetryRejected: true}); err != nil || dry.Retry == nil || dry.Retry.Notice == "" {
		t.Fatalf("the dry run says so too: %v %+v", err, dry.Retry)
	}
}

// 规则性的禁止（跨项目、同仓库不同 PR、本人拆开过）不重判：结果不会变，「拆开」还是本人的决定；其余拒绝码重判。
func TestRetryableCodes(t *testing.T) {
	mk := func(code string) domain.Item {
		return domain.Item{Phase: domain.PhaseProposed, Proposal: &domain.Proposal{Sweep: &domain.ProposalSweep{State: "done", Action: domain.SweepKeep, Rejected: code}}}
	}
	for code, want := range map[string]bool{domain.RejectFactsMissing: true, domain.RejectCombine: true, domain.RejectNotCandidate: true, domain.RejectActionMismatch: true,
		domain.RejectTargetChanged: true, domain.RejectNotLanded: true, domain.RejectCrossProject: false, domain.RejectOtherChange: false, domain.RejectNotSame: false, "": false} {
		if got := retryable(mk(code)); got != want {
			t.Errorf("%q: retryable %v, want %v", code, got, want)
		}
	}
	merged := mk(domain.RejectFactsMissing)
	merged.Proposal.Sweep.Action = domain.SweepMerge
	retried := mk(domain.RejectFactsMissing)
	retried.Proposal.Sweep.Retry = &domain.SweepRetry{Of: "mb_x"}
	closed := mk(domain.RejectFactsMissing)
	closed.Phase = domain.PhaseOpen
	for name, it := range map[string]domain.Item{"merged": merged, "already retried": retried, "no longer a proposal": closed} {
		if retryable(it) {
			t.Errorf("%s must not be retried", name)
		}
	}
}
