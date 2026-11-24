package inbound

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const batchMark = "这一批待归类的建议"

func (e *env) setPolicy(f func(p *policy.Policy)) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "merge", func(p *policy.Policy) error { f(p); return nil })
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) byTitle(title string) domain.Item {
	for _, it := range e.allItems() {
		if it.Title == title {
			return it
		}
	}
	e.t.Fatalf("no item titled %q", title)
	return domain.Item{}
}

func (e *env) batchCalls() (n int) {
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, batchMark) {
			n++
		}
	}
	return
}

func (e *env) progressOf(id string) (out []domain.Progress) {
	e.view(func(tx *store.Tx) { out, _ = items.Progress(tx, id) })
	return
}

// reply 拼一份模型回答：groups、decisions、combined 三段（各是 JSON 数组的内容）。
func reply(groups, decisions, combined string) modelstub.Reply {
	return modelstub.Reply{Content: `{"groups":[` + groups + `],"decisions":[` + decisions + `],"combined":[` + combined + `]}`}
}

var memberRef = regexp.MustCompile(`(N\d+)［待归类的建议］`)

// keepAll 是一份按请求现算的回答：资料里的每个成员单独成组、留着；drop 里的编号不给结论（漏答）。
func keepAll(drop ...string) func(string) modelstub.Reply {
	return func(body string) modelstub.Reply {
		var gs, ds []string
		for _, m := range memberRef.FindAllStringSubmatch(body, -1) {
			if slices.Contains(drop, m[1]) {
				continue
			}
			gs = append(gs, fmt.Sprintf(`{"group":"G%s","topic":"","members":["%s"]}`, m[1], m[1]))
			ds = append(ds, fmt.Sprintf(`{"new":"%s","group":"G%s","step":3,"action":"keep","reason":"不是同一件事","confident":true}`, m[1], m[1]))
		}
		return reply(strings.Join(gs, ","), strings.Join(ds, ","), ``)
	}
}

func (e *env) lastBatchBody() (body string) {
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, batchMark) {
			body = r.BodyText
		}
	}
	return
}

// 一批四条按优先级：能确定到进行中事项的并入（attach，进展逐字写出原文）、落到已完成事项的判定重复（事项不改，记溯源）、
// 同组两条同一件事的整合成一条新建议（载体改写标题与摘要、原文进 integrated，另一条折叠、消息与原摘要进溯源）；
// 已归类过的老建议不作去处。同一批再跑一遍零变化；撤销整合成员时整组还原。
func TestMergeBatchPriorityAndIntegration(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeOff })
	e.emit("src_a", "ticket", ev("old", "ticket.opened", "季度预算评审会议改到 11-28 14:00", nil)) // 合并关闭时到的老建议：不进批，也不作去处
	open := e.openItem("评审 AgentSafe 资产 B18630823")
	done := e.openItem("周报模板改版 W4701")
	e.transition(done, domain.PhaseDone)
	old := e.byTitle("季度预算评审会议改到 11-28 14:00")

	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame; p.Merge.BatchSize = 4 })
	e.stub.When(batchMark, reply(
		`{"group":"G1","topic":"AgentSafe 资产","members":["N1"]},{"group":"G2","topic":"周报模板","members":["N2"]},{"group":"G3","topic":"预算评审会议","members":["N3","N4"]}`,
		`{"new":"N1","group":"G1","step":1,"action":"attach","target":"E1","reason":"N1 与 E1 是同一个资产编号 B18630823","confident":true},
		 {"new":"N2","group":"G2","step":1,"action":"duplicate","target":"E2","reason":"同一次周报改版，已完成","confident":true},
		 {"new":"N3","group":"G3","step":2,"action":"merge","target":"M1","reason":"同一场预算评审会议","confident":true},
		 {"new":"N4","group":"G3","step":2,"action":"merge","target":"M1","reason":"同一场预算评审会议","confident":true}`,
		`{"merge":"M1","group":"G3","members":["N3","N4"],"title":"季度预算评审会议 11-28 14:00（301 会议室）","summary":"季度预算评审会议定在 11-28 14:00：\n- src_c：会议改到 11-28 14:00\n- src_d：地点 3 楼 301 会议室",
		  "body":"- src_c：季度预算评审会议改到 11-28 14:00，结论：时间已定\n- src_d：地点 3 楼 301 会议室，结论：地点已定"}`))
	e.emit("src_b", "ticket", ev("n1", "ticket.opened", "AgentSafe 资产确认（B18630823）", nil))
	e.now = e.now.Add(time.Minute) // 同批按建立先后编号
	e.emit("src_c", "ticket", ev("n2", "ticket.opened", "周报模板改版（W4701 第二次提醒）", nil))
	e.now = e.now.Add(time.Minute)
	e.emit("src_c", "ticket", ev("n3", "ticket.opened", "季度预算评审会议改到 11-28 14:00 了", nil))
	e.now = e.now.Add(time.Minute)
	e.emit("src_d", "ticket", ev("n4", "ticket.opened", "季度预算评审会议地点：3 楼 301 会议室", nil))
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("one model call for the whole batch: %v %d", err, e.batchCalls())
	}
	req := e.lastBatchBody()
	for _, s := range []string{"［进行中］", "［已完成］", "评审 AgentSafe 资产 B18630823", "周报模板改版 W4701", "N4［待归类的建议］"} {
		if !strings.Contains(req, s) {
			t.Fatalf("members and existing items must be in the batch input, missing %q", s)
		}
	}
	if strings.Contains(req, "季度预算评审会议改到 11-28 14:00\\n") {
		t.Fatal("an already sorted proposal is not a target any more")
	}

	n1, n2 := e.byTitle("AgentSafe 资产确认（B18630823）"), e.byTitle("周报模板改版（W4701 第二次提醒）")
	n4 := e.byTitle("季度预算评审会议地点：3 楼 301 会议室")
	carrier := e.byTitle("季度预算评审会议 11-28 14:00（301 会议室）")
	for _, c := range []struct {
		it             domain.Item
		action, target string
	}{{n1, domain.SweepAttach, open}, {n2, domain.SweepDuplicate, done}, {n4, domain.SweepMerge, carrier.ID}} {
		sw := c.it.Proposal.Sweep
		if c.it.Phase != domain.PhaseDropped || sw.State != "done" || sw.Action != c.action || sw.Target != c.target ||
			sw.By != "generate" || sw.Run == "" || sw.Size != 4 || sw.Chars == 0 || sw.Why == "" || sw.Group == "" || sw.Topic == "" {
			t.Fatalf("%s: %s %+v", c.it.Title, c.it.Phase, sw)
		}
	}
	if why := n1.Proposal.Sweep.Why; strings.Contains(why, "E1") || !strings.Contains(why, "「AgentSafe 资产确认") || !strings.Contains(why, "「评审 AgentSafe") {
		t.Fatalf("batch numbers in the reason become titles: %q", why)
	}
	if ps := e.progressOf(open); len(ps) != 1 || !strings.Contains(ps[0].Text, "原建议「AgentSafe 资产确认（B18630823）」") || !strings.Contains(ps[0].Text, "消息 1 条：ticket.opened×1") {
		t.Fatalf("attach writes the folded proposal out in full: %+v", ps)
	}
	if it := e.item(done); it.Title != "周报模板改版 W4701" || it.Phase != domain.PhaseDone || len(it.Proposal.Absorbed) != 1 || it.Proposal.Absorbed[0].Action != domain.SweepDuplicate {
		t.Fatalf("duplicate leaves the done item as it was, with a trail: %+v", it)
	}
	in := carrier.Proposal.Integrated
	if carrier.Phase != domain.PhaseProposed || carrier.Proposal.Sweep.Action != domain.SweepKeep || in == nil || in.Title != "季度预算评审会议改到 11-28 14:00 了" ||
		!strings.Contains(carrier.Summary, "301 会议室") || len(in.Members) != 2 || in.ToTitle != carrier.Title ||
		!strings.Contains(carrier.Body, "- src_c：") || !strings.Contains(carrier.Body, "- src_d：") || in.ToBody != carrier.Body || in.Body == carrier.Body {
		t.Fatalf("the carrier is rewritten and keeps its own original: %+v %+v", carrier, in)
	}
	ab := carrier.Proposal.Absorbed
	if len(ab) != 1 || ab[0].Item != n4.ID || ab[0].Title != n4.Title || len(ab[0].Events) != 1 || !slices.Contains(carrier.Proposal.Sources, ab[0].Events[0]) {
		t.Fatalf("the folded member's original and messages are on the carrier: %+v", ab)
	}
	if e.item(old.ID).Proposal.Sweep != nil {
		t.Fatal("the old proposal is untouched")
	}
	e.verify()

	// 同一批再跑：成员都已归类，零变化、不调模型。
	h := e.head()
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 || e.head() != h {
		t.Fatalf("second pass changes nothing: %v %d", err, e.batchCalls())
	}

	// 撤销整合成员：整组还原。
	if err := e.svc.Unmerge(bg, owner, n4.ID); err != nil {
		t.Fatal(err)
	}
	back := e.item(carrier.ID)
	if back.Title != "季度预算评审会议改到 11-28 14:00 了" || back.Body != in.Body || back.Proposal.Integrated != nil || len(back.Proposal.Absorbed) != 0 || !slices.Contains(back.Proposal.NotSame, n4.ID) {
		t.Fatalf("the carrier is back to its original: %+v", back)
	}
	if it := e.item(n4.ID); it.Phase != domain.PhaseProposed || it.Proposal.Sweep.Action != domain.SweepUndone || e.events("src_d")[0].Item != n4.ID {
		t.Fatalf("the member is back with its message: %+v", it)
	}
	for _, id := range []string{n1.ID, n2.ID} {
		if err := e.svc.Unmerge(bg, owner, id); err != nil {
			t.Fatalf("unmerge %s: %v", id, err)
		}
	}
	if ps := e.progressOf(open); len(ps) != 2 || !strings.Contains(ps[1].Text, "撤回上一条补充") {
		t.Fatalf("unmerge of attach appends a retraction: %+v", ps)
	}
	if err := e.svc.Unmerge(bg, owner, n1.ID); err == nil {
		t.Fatal("second unmerge must be a conflict")
	}
	e.verify()
}

// 关键事实校验：整合文本漏掉成员的一个时刻（14:00）就不整合，两条都留着、载体文本不变，手动合并的报告计 check_failed。
// 同一仓库不同 PR 号的两条即使模型放进同一项整合也不合并（other_change）；PR 号相同的已有事项按强标识检索成满分候选，
// 另一个 PR 的事项检索不拦、照样是候选。
func TestMergeIntegrationChecks(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame; p.Merge.BatchSize = 4 })
	pr := e.openItem("[org/app] fix A (PR #220) 审阅 T-6661")
	e.stub.When(batchMark, reply(
		`{"group":"G1","topic":"预算会","members":["N1","N2"]},{"group":"G2","topic":"org/app PR","members":["N3","N4"]}`,
		`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一场会","confident":true},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一场会","confident":true},
		 {"new":"N3","group":"G2","step":2,"action":"merge","target":"M2","reason":"同一仓库","confident":true},
		 {"new":"N4","group":"G2","step":2,"action":"merge","target":"M2","reason":"同一仓库","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N1","N2"],"title":"季度预算评审会议","summary":"会议 11-28 开，地点 3 楼会议室","body":"- 会议 11-28 14:00\n- 地点 301"},
		 {"merge":"M2","group":"G2","members":["N3","N4"],"title":"org/app PR #220 与 PR #221","summary":"[org/app] fix A (PR #220)；[org/app] fix A (PR #221) T-6661","body":"- PR #220\n- PR #221 T-6661"}`))
	a := e.proposal("src_a", "a1", "季度预算评审会议改到 11-28 14:00", "")
	e.now = e.now.Add(time.Minute)
	b := e.proposal("src_b", "b1", "季度预算评审会议地点 301", "")
	e.now = e.now.Add(time.Minute)
	c := e.proposal("src_c", "c1", "[org/app] fix A (PR #220)", "")
	e.now = e.now.Add(time.Minute)
	d := e.proposal("src_d", "d1", "[org/app] fix A (PR #221) T-6661", "")
	e.now = e.now.Add(31 * time.Minute)

	dry, err := e.svc.PlanManual(bg, ManualFilter{})
	if err != nil || len(dry.Ask) != 4 {
		t.Fatalf("dry: %v %+v", err, dry)
	}
	for _, x := range dry.Ask {
		switch x.Item {
		case c.ID:
			if x.Change != "org/app#220" || len(x.Candidates) != 1 || x.Candidates[0].Item != pr || x.Candidates[0].Score != 1 {
				t.Fatalf("the same PR is a full-score candidate: %+v", x)
			}
		case d.ID:
			if len(x.Candidates) != 1 || x.Candidates[0].Item != pr {
				t.Fatalf("another PR of the same repository is still a candidate: %+v", x)
			}
		}
	}
	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || rep.CheckFailed != 1 || len(rep.Combined) != 2 || !slices.Equal(rep.Combined[0].Missing, []string{"14:00"}) || len(rep.Combined[0].MissingWhy) != 1 || len(rep.Groups) != 2 {
		t.Fatalf("one integration fails the fact check: %v %+v", err, rep)
	}
	for _, it := range []domain.Item{a, b, c, d} {
		got := e.item(it.ID)
		if got.Phase != domain.PhaseProposed || got.Title != it.Title || got.Proposal.Sweep.Action != domain.SweepKeep || got.Proposal.Integrated != nil {
			t.Fatalf("%s stays as it was: %+v", it.Title, got)
		}
	}
	if why := e.item(a.ID).Proposal.Sweep.Why; !strings.Contains(why, "缺少") || !strings.Contains(why, "「14:00」（日期时间：") {
		t.Fatalf("why names the missing fact: %q", why)
	}
	if sw := e.item(c.ID).Proposal.Sweep; sw.Rejected != domain.RejectOtherChange || !strings.Contains(sw.Why, "另一个 PR") {
		t.Fatalf("two PRs never merge: %+v", sw)
	}
	if sw := e.item(a.ID).Proposal.Sweep; sw.Rejected != domain.RejectFactsMissing {
		t.Fatalf("the fact check is a facts_missing rejection: %+v", sw)
	}
	if rep.Rejected[domain.RejectOtherChange] != 2 || rep.Rejected[domain.RejectFactsMissing] != 2 {
		t.Fatalf("the report counts rejections by code: %v", rep.Rejected)
	}
	e.verify()
}

// 拿不准只提示、给的合并项不存在按留着；不够条数时等够时间再跑；关闭合并时待归类的按规则结清、不调模型。
func TestMergeBatchUndecidedWaitAndOff(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	open := e.openItem("季度预算评审会议")
	e.setPolicy(func(p *policy.Policy) { p.Merge.BatchSize = 20 })
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"预算会","members":["N1","N2"]}`,
		`{"new":"N1","group":"G1","step":1,"action":"attach","target":"E1","reason":"像是同一场会","confident":false},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M9","reason":"编造的合并项","confident":true}`, ``))
	e.emit("src_a", "ticket", ev("u1", "ticket.opened", "季度预算评审会议改期", nil))
	e.now = e.now.Add(time.Minute)
	e.emit("src_b", "ticket", ev("u2", "ticket.opened", "季度预算评审会议纪要", nil))
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 0 {
		t.Fatalf("below batch size and wait: %v %d", err, e.batchCalls())
	}
	e.now = e.now.Add(31 * time.Minute)
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("waited long enough: %v %d", err, e.batchCalls())
	}
	u1, u2 := e.byTitle("季度预算评审会议改期"), e.byTitle("季度预算评审会议纪要")
	if u1.Phase != domain.PhaseProposed || u1.Proposal.Sweep.Action != domain.SweepKeep || len(u1.Proposal.Similar) != 1 || u1.Proposal.Similar[0].Item != open {
		t.Fatalf("undecided keeps and hints: %+v", u1.Proposal)
	}
	if u2.Phase != domain.PhaseProposed || u2.Proposal.Sweep.Action != domain.SweepKeep || !strings.Contains(u2.Proposal.Sweep.Why, "合并项不存在") {
		t.Fatalf("made-up merge keeps: %+v", u2.Proposal.Sweep)
	}

	e.emit("src_c", "ticket", ev("u3", "ticket.opened", "季度预算评审会议改到周五", nil))
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeOff })
	e.now = e.now.Add(time.Hour)
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("merge off settles without the model: %v %d", err, e.batchCalls())
	}
	if sw := e.byTitle("季度预算评审会议改到周五").Proposal.Sweep; sw.State != "done" || sw.Action != domain.SweepKeep || sw.By != "rule" {
		t.Fatalf("settled by rule: %+v", sw)
	}
	e.verify()
}

// 不参与合并的来源不进批；不纳入已有事项时资料里没有已有事项；本人改过的提示词随调用送出。
func TestMergeBatchSettings(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.openItem("季度预算评审会议")
	body := builtinMergePrompt(t) + "\n自定义标记XYZ"
	e.setPolicy(func(p *policy.Policy) {
		p.Merge = policy.MergeSettings{ExcludeSources: []string{"src_x"}, SkipExisting: true, BatchSize: 2, Prompt: body}
	})
	e.stub.WhenFunc(batchMark, keepAll())
	e.emit("src_x", "ticket", ev("x1", "ticket.opened", "季度预算评审会议改期", nil))
	e.emit("src_a", "ticket", ev("a1", "ticket.opened", "季度预算评审会议改期了", nil))
	e.emit("src_b", "ticket", ev("b1", "ticket.opened", "导出报表加一列", nil))
	for _, it := range e.proposed() {
		if it.Origin.Source == "src_x" && it.Proposal.Sweep != nil {
			t.Fatalf("excluded source must not be marked: %+v", it.Proposal.Sweep)
		}
	}
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("two members make a batch: %v %d", err, e.batchCalls())
	}
	if req := e.lastBatchBody(); strings.Contains(req, "［进行中］") || !strings.Contains(req, "自定义标记XYZ") || strings.Contains(req, "改期\\n") {
		t.Fatalf("no existing items, the owner's prompt is sent, the excluded source is not: %s", req)
	}
	if sw := e.byTitle("导出报表加一列").Proposal.Sweep; sw.By != "generate" || sw.Action != domain.SweepKeep || sw.Why != "不是同一件事" {
		t.Fatalf("keeps with the model's reason: %+v", sw)
	}

	e.setPolicy(func(p *policy.Policy) { p.Merge.SkipExisting = false })
	e.emit("src_c", "ticket", ev("c1", "ticket.opened", "季度预算评审会议改期一事", nil))
	e.emit("src_d", "ticket", ev("d1", "ticket.opened", "另一件不相干的事", nil))
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 2 || !strings.Contains(e.lastBatchBody(), "［进行中］") {
		t.Fatalf("with existing items the open one is a candidate: %v %d", err, e.batchCalls())
	}
	e.verify()
}

// 资料上限（merge.budget_tokens）：同样 20 条，默认上限一次归完；改小到 2000 token 后超出，这批砍半，
// 前一半落账并记下留到下一轮的条数，后一半仍待归类、下一轮归完；每次送出的资料都不超过上限。
func TestMergeBatchBudgetHalves(t *testing.T) {
	run := func(budget *int) *env {
		e := newEnv(t, true)
		e.svc.o.Now = e.Now
		e.openItem("资产确认 B18630823")
		e.setPolicy(func(p *policy.Policy) {
			p.MergeJudge = policy.MergeSame
			p.Merge.BatchSize = 20
			p.Merge.BudgetTokens = budget
		})
		e.stub.WhenFunc(batchMark, keepAll())
		long := strings.Repeat("需要本人确认的资产信息与负责人说明", 6)
		for i := range 20 {
			e.now = e.now.Add(time.Second)
			e.emit(fmt.Sprintf("src_%02d", i), "ticket", ev(fmt.Sprintf("b%02d", i), "ticket.opened", fmt.Sprintf("%02d %s B18630823", i, long), nil))
		}
		if err := e.svc.Sweep(bg); err != nil {
			t.Fatal(err)
		}
		return e
	}
	tally := func(e *env) (done, pending, deferred, chars int) {
		for _, it := range e.proposed() {
			switch sw := it.Proposal.Sweep; {
			case sw == nil:
			case sw.State == "pending":
				pending++
			default:
				done++
				deferred, chars = max(deferred, sw.Deferred), max(chars, sw.Chars)
			}
		}
		return
	}

	e := run(nil)
	done, pending, deferred, chars := tally(e)
	if e.batchCalls() != 1 || done != 20 || pending != 0 || deferred != 0 || policy.TokensOfChars(chars) > policy.DefaultMergeBudgetTokens {
		t.Fatalf("default budget: calls %d, done %d, pending %d, deferred %d, %d chars", e.batchCalls(), done, pending, deferred, chars)
	}
	full := chars

	small := policy.MinBudgetTokens
	e = run(&small)
	done, pending, deferred, chars = tally(e)
	if policy.TokensOfChars(full) <= small {
		t.Fatalf("the full batch (%d chars) must be over %d tokens for this test", full, small)
	}
	if e.batchCalls() != 1 || done != 10 || pending != 10 || deferred != 10 || chars == 0 || policy.TokensOfChars(chars) > small {
		t.Fatalf("over budget halves: calls %d, done %d, pending %d, deferred %d, %d chars", e.batchCalls(), done, pending, deferred, chars)
	}
	e.now = e.now.Add(31 * time.Minute) // 余下的不够一批，等满时间成批
	if err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if done, pending, _, _ = tally(e); e.batchCalls() != 2 || done != 20 || pending != 0 {
		t.Fatalf("the rest goes next round: calls %d, done %d, pending %d", e.batchCalls(), done, pending)
	}
	e.verify()
}

func builtinMergePrompt(t *testing.T) string {
	b, err := os.ReadFile("../../packs/inbound-triage/prompts/" + MergeTemplate + ".md")
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.SplitN(string(b), "---\n", 3)
	return parts[2]
}

// 内置提示词本身过得了保存时的校验；缺段、空段、少结论词都拒绝。
func TestMergePromptCheck(t *testing.T) {
	def := builtinMergePrompt(t)
	if err := policy.CheckMergePrompt(def); err != nil {
		t.Fatalf("builtin prompt: %v", err)
	}
	for name, bad := range map[string]string{
		"missing section": strings.Replace(def, "## 判定依据", "## 依据", 1),
		"empty section":   def[:strings.Index(def, "## 不许做的事")] + "## 不许做的事\n   \n",
		"no keep":         strings.ReplaceAll(def, "keep", "stay"),
		"too long":        def + strings.Repeat("长", policy.MaxMergePrompt),
	} {
		if policy.CheckMergePrompt(bad) == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

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
		if got := similarScore(c.a, "", c.b, "") >= 0.30; got != c.hit {
			t.Errorf("%q vs %q: score %.2f", c.a, c.b, similarScore(c.a, "", c.b, ""))
		}
	}
}

// pr227 是线上 PR #227（2026-11-23，脱敏）在邮件还没带 PR 链接时的形状：两封通知标题只差 "Re:"，建成两条建议
// （keep、mid；同标题的一封按规则合入 mid，一封回信挂到 mid）；平台上的 PR 再建一条 gp，它之后的三条更新（合并、改动、请求评审）
// 按"来源对象更新"挂到 gp。i、m、g 是三个来源的事件，按到达先后。
func pr227(e *env) (keep, mid, gp domain.Item, i, m, g []string) {
	e.t.Helper()
	const gh, link = "notifications@github.com", "https://github.com/org/app/pull/227"
	subj := "[org/app] feat(desktop): 能力页「帮我安装」卡片移到最上方并改标题 (PR #227)"
	mail := func(id, subject, thread string) map[string]any {
		return ev(id, "mail.received", subject, map[string]any{"sender": gh, "correlation_keys": []string{thread}})
	}
	pr := func(id, typ string) map[string]any {
		return ev(id, typ, "feat(desktop): 能力页「帮我安装」卡片移到最上方并改标题", map[string]any{"sender": "dev", "link": link, "correlation_keys": []string{"org/app#227"}})
	}
	e.emit("src_mail_icloud", "mail", mail("i1", "Re: "+subj, "<org/app/pull/227/issue_event/101@github.com>"))
	e.now = e.now.Add(time.Minute) // 同批按建立先后编号
	e.emit("src_mail_icloud", "mail", mail("i2", subj, "<org/app/pull/227@github.com>"))
	e.now = e.now.Add(time.Minute)
	e.emit("src_mail", "mail", mail("m1", subj, "<org/app/pull/227/review/202@github.com>"), mail("m2", "Re: "+subj, "<org/app/pull/227/review/202@github.com>"))
	e.now = e.now.Add(time.Minute)
	e.emit("src_github", "pr", pr("g1", "pr.opened"), pr("g2", "pr.merged"), pr("g3", "pr.updated"), pr("g4", "notification.review_requested"))
	keep, mid, gp = e.byTitle("Re: "+subj), e.byTitle(subj), e.byTitle("feat(desktop): 能力页「帮我安装」卡片移到最上方并改标题")
	return keep, mid, gp, e.sev("src_mail_icloud", "i1", "i2"), e.sev("src_mail", "m1", "m2"), e.sev("src_github", "g1", "g2", "g3", "g4")
}

const pr227Title = "org/app PR #227「能力页「帮我安装」卡片移到最上方并改标题」：评审请求，已合并"

// sweep227 跑一次归类：同一 PR #227 的三条整合成一条新建议。
func (e *env) sweep227() {
	e.t.Helper()
	e.stub.When(batchMark, reply(`{"group":"G1","topic":"org/app PR #227","members":["N1","N2","N3"]}`,
		`{"new":"N1","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一个 PR #227 的评审通知","confident":true},
		 {"new":"N2","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一个 PR #227","confident":true},
		 {"new":"N3","group":"G1","step":2,"action":"merge","target":"M1","reason":"同一个 PR #227","confident":true}`,
		`{"merge":"M1","group":"G1","members":["N1","N2","N3"],"title":"`+pr227Title+`","summary":"org/app 的 PR #227 共三条通知：\n- 邮件：Re: 评审回复\n- 邮件：请求评审\n- 平台：已合并、有更新、请求评审",
		  "body":"- 邮件 org/app PR #227：Re: 评审回复\n- 邮件 org/app PR #227：请求评审\n- 平台 org/app PR #227：已合并、有更新、请求评审"}`))
	e.now = e.now.Add(31 * time.Minute)
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		e.t.Fatalf("one model call: %v %d", err, e.batchCalls())
	}
}

// 线上 PR #227：三条建议在同一批里整合成一条。载体是名下消息最多的平台那条（4 条），另两条名下的消息全部搬到它，
// 按搬入先后排，每条带合并留痕（记着从哪条来）；撤销其中一条时整组还原，消息原样回到原处，载体恢复原标题。
func TestMergeFoldMovesEveryHeldMessage(t *testing.T) {
	e := followEnv(t)
	keep, mid, gp, i, m, g := pr227(e)
	if len(e.allItems()) != 3 || !slices.Equal(keep.Proposal.Sources, i[:1]) || !slices.Equal(mid.Proposal.Sources, []string{i[1], m[0], m[1]}) || !slices.Equal(gp.Proposal.Sources, g) {
		t.Fatalf("before the sweep: %+v / %+v / %+v", keep.Proposal, mid.Proposal, gp.Proposal)
	}
	for _, x := range e.events("src_github")[1:] {
		if x.Item != gp.ID || x.Disposition != domain.DispositionAttached || x.AttachState != "processed" {
			t.Fatalf("platform updates are attached to the platform-built proposal: %+v", x)
		}
	}

	e.sweep227()
	if req := e.lastBatchBody(); !strings.Contains(req, "消息：4 条：pr.opened×1、pr.merged×1、pr.updated×1、notification.review_requested×1") {
		t.Fatalf("the model sees what each member's messages were: %s", req)
	}
	got := e.item(gp.ID)
	want := append(append(append([]string{}, g...), i[0]), i[1], m[0], m[1])
	if got.Title != pr227Title || got.Proposal.Integrated == nil || !slices.Equal(got.Proposal.Sources, want) || len(got.Proposal.Merges) != 4 || len(got.Proposal.Absorbed) != 2 {
		t.Fatalf("every held message moves to the carrier: %+v %+v", got, got.Proposal)
	}
	for k, x := range got.Proposal.Merges {
		from := keep.ID
		if k >= 1 {
			from = mid.ID
		}
		if x.Event != want[k+4] || x.Item != from || x.By != "generate" || x.Batch == "" {
			t.Fatalf("merge trace %d: %+v", k, x)
		}
	}
	for _, src := range []string{"src_mail_icloud", "src_mail", "src_github"} {
		for _, x := range e.events(src) {
			if x.Item != gp.ID {
				t.Fatalf("event left behind: %+v", x)
			}
		}
	}
	for _, id := range []string{keep.ID, mid.ID} {
		if it := e.item(id); it.Phase != domain.PhaseDropped || it.Proposal.Sweep.Target != gp.ID {
			t.Fatalf("folded %s: %+v", id, it.Proposal.Sweep)
		}
	}
	e.verify()

	if err := e.svc.Unmerge(bg, owner, mid.ID); err != nil {
		t.Fatal(err)
	}
	got = e.item(gp.ID)
	if got.Title != gp.Title || got.Proposal.Integrated != nil || !slices.Equal(got.Proposal.Sources, g) || len(got.Proposal.Merges) != 0 || len(got.Proposal.Absorbed) != 0 {
		t.Fatalf("undo puts the whole integration back: %+v", got.Proposal)
	}
	if !slices.Equal(e.item(keep.ID).Proposal.Sources, i[:1]) || e.item(keep.ID).Phase != domain.PhaseProposed || e.item(mid.ID).Phase != domain.PhaseProposed {
		t.Fatal("both members are back")
	}
	for k, x := range e.events("src_github") {
		if x.Item != gp.ID || (k == 0) != (x.Disposition == domain.DispositionProposed) {
			t.Fatalf("back where it was: %+v", x)
		}
	}
	e.verify()
}

// 并入已有事项也搬消息：被折那条名下的消息（建出它的、挂上的回信）迁到该事项，记进它的消息清单、按先后、每条记从哪条来；
// 挂上的回信在事项上照样是挂靠消息。事项本身只多一条补充进展，标题、阶段不变。撤销时搬回，事项追加撤回说明。
func TestAttachFoldMovesMessagesToTheItem(t *testing.T) {
	e := followEnv(t)
	open := e.openItem("评审 AgentSafe 资产 B18630823")
	const thread = "<asset/B18630823@example.invalid>"
	e.emit("src_mail", "mail", ev("a1", "mail.received", "AgentSafe 资产确认（B18630823）", map[string]any{"sender": "ops@example.invalid", "correlation_keys": []string{thread}}))
	e.now = e.now.Add(time.Minute)
	e.emit("src_mail", "mail", ev("a2", "mail.received", "Re: AgentSafe 资产确认（B18630823）", map[string]any{"sender": "ops@example.invalid", "correlation_keys": []string{thread}}))
	ps := e.proposed()
	a := e.sev("src_mail", "a1", "a2")
	if len(ps) != 1 || !slices.Equal(ps[0].Proposal.Sources, a) || len(e.allItems()) != 2 {
		t.Fatalf("one proposal with its follow-up: %+v", ps)
	}
	n := ps[0]

	e.stub.When(batchMark, reply(`{"group":"G1","topic":"AgentSafe 资产","members":["N1"]}`,
		`{"new":"N1","group":"G1","step":1,"action":"attach","target":"E1","reason":"同一个资产编号 B18630823","confident":true}`, ``))
	e.now = e.now.Add(31 * time.Minute)
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("one model call: %v %d", err, e.batchCalls())
	}
	it := e.item(open)
	if it.Title != "评审 AgentSafe 资产 B18630823" || it.Phase != domain.PhaseOpen || it.Proposal == nil || !slices.Equal(it.Proposal.Sources, a) || len(it.Proposal.Merges) != 2 {
		t.Fatalf("messages move to the item, in order: %+v %+v", it, it.Proposal)
	}
	for k, x := range it.Proposal.Merges {
		if x.Event != a[k] || x.Item != n.ID || x.Source != "src_mail" || x.By != "generate" || x.Why != "同一个资产编号 B18630823" {
			t.Fatalf("merge trace %d: %+v", k, x)
		}
	}
	evs := e.events("src_mail")
	if evs[0].Item != open || evs[0].Disposition != domain.DispositionProposed || evs[1].Item != open || evs[1].Disposition != domain.DispositionAttached {
		t.Fatalf("events point at the item, state unchanged: %+v", evs)
	}
	var att []runner.Attachment
	e.view(func(tx *store.Tx) { att, _ = runner.Attachments(tx, open) })
	if len(att) != 1 || att[0].ID != a[1] {
		t.Fatalf("the reply is an attached message of the item: %+v", att)
	}
	if ps := e.progressOf(open); len(ps) != 1 || !strings.Contains(ps[0].Text, "AgentSafe 资产确认") || !strings.Contains(ps[0].Text, "消息 2 条") {
		t.Fatalf("still one supplement note, written out in full: %+v", ps)
	}
	e.verify()

	if err := e.svc.Unmerge(bg, owner, n.ID); err != nil {
		t.Fatal(err)
	}
	if it := e.item(open); len(it.Proposal.Sources) != 0 || len(it.Proposal.Merges) != 0 {
		t.Fatalf("unmerge takes the messages back: %+v", it.Proposal)
	}
	for _, x := range e.events("src_mail") {
		if x.Item != n.ID {
			t.Fatalf("back where it was: %+v", x)
		}
	}
	if back := e.item(n.ID); back.Phase != domain.PhaseProposed || !slices.Equal(back.Proposal.Sources, a) {
		t.Fatalf("the proposal is back as it was: %+v", back)
	}
	if ps := e.progressOf(open); len(ps) != 2 || !strings.Contains(ps[1].Text, "撤回上一条补充") {
		t.Fatalf("retraction note: %+v", ps)
	}
	e.verify()
}

// 仓库 + PR 号与关键事实的抽取。
func TestChangeRefAndKeyFacts(t *testing.T) {
	item := func(title, summary, ref, key string) domain.Item {
		it := domain.Item{Title: title, Summary: summary, Origin: &domain.Origin{Ref: ref}}
		if key != "" {
			it.Proposal = &domain.Proposal{MergeKey: key}
		}
		return it
	}
	for _, c := range []struct {
		it   domain.Item
		want string
	}{
		{item("fix: #207/#200 验收反馈", "", "https://git.example/Org/App/pull/208", ""), "org/app#208"},
		{item("Re: [org/app] fix: #207/#200 验收反馈 (PR #208)", "", "sev_1", ""), "org/app#208"},
		{item("审查 PR #214：行业方案", "", "sev_2", "title:[org/app] feat: 行业方案 (pr #214\x1fx"), "org/app#214"},
		{item("评审 PR #221：命名空间", "代码托管上 org/app PR #221 请你评审；它叠在 #220 上", "sev_3", ""), "org/app#221"},
		{item("评审 PR #221", "", "", ""), ""},
	} {
		if got := changeRef(c.it); got != c.want {
			t.Errorf("%q: got %q, want %q", c.it.Title, got, c.want)
		}
	}
	if !otherChange("org/app#220", "org/app#221") || otherChange("org/app#220", "org/app#220") || otherChange("org/app#220", "org/web#221") || otherChange("", "org/app#1") {
		t.Fatal("otherChange")
	}
}

// 回答必须对每个成员编号都给出结论：缺一个就整批不落账、同样大小重问一次（不复用刚才的回答），仍缺就降到 batch_size 的一半（60 → 30），
// 余下的留在待归类、之后的批也不超过 30；每次调用的耗时与输出字数记进报告与 run.finished。被截断、不合 schema 的同样算不完整。
// 同一批再跑零变化。
func TestMergeBatchIncompleteRetriesThenHalves(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge, p.Merge.BatchSize = policy.MergeSame, 60 })
	asked := 0
	e.stub.WhenFunc(batchMark, func(body string) modelstub.Reply {
		if asked++; asked <= 2 {
			return keepAll("N60")(body) // 前两次漏了最后一条
		}
		return keepAll()(body)
	})
	for i := range 60 {
		e.now = e.now.Add(time.Second)
		e.emit(fmt.Sprintf("src_%02d", i), "ticket", ev(fmt.Sprintf("c%02d", i), "ticket.opened", fmt.Sprintf("互不相干的第 %02d 件事 %s", i, strings.Repeat("字", i)), nil))
	}
	tally := func() (done, pending int) {
		for _, it := range e.proposed() {
			if sw := it.Proposal.Sweep; sw != nil && sw.State == "pending" {
				pending++
			} else if sw != nil {
				done++
			}
		}
		return
	}

	rep, err := e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err != nil || e.batchCalls() != 3 || len(rep.Calls) != 3 {
		t.Fatalf("two incomplete answers then a smaller batch: %v, calls %d, %+v", err, e.batchCalls(), rep.Calls)
	}
	c := rep.Calls
	if c[0].Size != 60 || c[0].Complete || c[0].Missing != 1 || c[0].Error != "missing" || c[0].Fresh ||
		c[1].Size != 60 || c[1].Complete || !c[1].Fresh || c[1].Reused ||
		c[2].Size != 30 || !c[2].Complete || c[2].OutputChars == 0 || c[2].Run == "" {
		t.Fatalf("calls: %+v", c)
	}
	if done, pending := tally(); done != 30 || pending != 30 || len(rep.Decisions) != 30 || len(rep.Ask) != 30 || len(rep.Deferred) != 30 {
		t.Fatalf("only the 30 asked last land: done %d, pending %d, decisions %d, ask %d, deferred %d", done, pending, len(rep.Decisions), len(rep.Ask), len(rep.Deferred))
	}
	if sw := e.item(rep.Decisions[0].Item).Proposal.Sweep; sw.Run != c[2].Run || sw.Deferred != 30 || sw.Size != 60 {
		t.Fatalf("landed from the complete call: %+v", sw)
	}
	var fin map[string]any
	for _, ev := range e.eventsOf(c[2].Run) {
		if ev.Type == domain.EvRunFinished {
			_ = json.Unmarshal(ev.Payload, &fin)
		}
	}
	if _, ok := fin["elapsed_ms"]; !ok || fin["output_chars"] != float64(c[2].OutputChars) {
		t.Fatalf("run record carries elapsed and output size: %v", fin)
	}
	var pol policy.Policy
	e.view(func(tx *store.Tx) { _, pol, _ = policy.Current(tx) })
	if e.svc.batchSize(pol) != 30 {
		t.Fatalf("later batches stay at half of batch_size, got %d", e.svc.batchSize(pol))
	}

	// 余下 30 条下一轮归完；再跑一遍零变化。
	if rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || len(rep.Calls) != 1 || !rep.Calls[0].Complete || len(rep.Decisions) != 30 {
		t.Fatalf("the rest: %v %+v", err, rep.Calls)
	}
	h, calls := e.head(), e.batchCalls()
	if rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{}); err != nil || rep.Matched != 0 || e.head() != h || e.batchCalls() != calls {
		t.Fatalf("second pass changes nothing: %v %+v", err, rep)
	}

	// 不合 schema（含截断）也算不完整：重问一次，不超过一半就不再降，什么都不落。
	e.stub.Reset()
	e.configureModel()
	e.stub.When(batchMark, modelstub.Reply{Content: `{"groups":[{"group":"G1","topic":"`})
	for i := range 3 {
		e.emit(fmt.Sprintf("src_t%d", i), "ticket", ev(fmt.Sprintf("t%d", i), "ticket.opened", fmt.Sprintf("截断测试第 %d 件", i), nil))
	}
	rep, err = e.svc.RunManual(bg, cliOwner, ManualFilter{})
	if err == nil || len(rep.Calls) != 2 || rep.Calls[0].Error == "" || !rep.Calls[1].Fresh || len(rep.Decisions) != 0 {
		t.Fatalf("malformed answers land nothing: %v %+v", err, rep.Calls)
	}
	if _, pending := tally(); pending != 3 {
		t.Fatalf("all three stay pending, got %d", pending)
	}
	e.verify()
}
