package inbound

import (
	"fmt"
	"os"
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

const batchMark = "这一批新到的建议"

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

// 新数据和账本里全部已有记录一起比：三条来自三个来源的新建议攒成一批，一次模型调用分别归为
// 并入进行中的事项、与本人忽略过的记录重复、与已有建议合成一条；留痕写明去处、理由、判断方与成本；每条都能撤销。
func TestMergeBatchAgainstExistingAndUndo(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeOff })
	e.emit("src_a", "ticket", ev("old", "ticket.opened", "季度预算评审会议", nil)) // 合并关闭时到的：不进批，只作已有建议
	open := e.openItem("评审 AgentSafe 资产 B18630823")
	ignored := e.openItem("周报模板改版 W4701")
	e.transition(ignored, domain.PhaseDropped)
	old := e.byTitle("季度预算评审会议")
	if old.Proposal.Sweep != nil {
		t.Fatalf("merge off must not mark pending: %+v", old.Proposal.Sweep)
	}

	e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame; p.Merge.BatchSize = 3 })
	e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[
		{"new":"N1","action":"attach","target":"E1","reason":"N1 与 E1 是同一个资产编号 B18630823","confident":true},
		{"new":"N2","action":"duplicate","target":"E2","reason":"同一次周报改版，本人忽略过","confident":true},
		{"new":"N3","action":"merge","target":"E3","reason":"同一场预算评审会议","confident":true}]}`})
	e.emit("src_b", "ticket", ev("n1", "ticket.opened", "AgentSafe 资产确认（B18630823）", nil))
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 0 {
		t.Fatalf("one pending is below the batch size: err %v, calls %d", err, e.batchCalls())
	}
	e.now = e.now.Add(time.Minute) // 同批按建立先后编号
	e.emit("src_c", "ticket", ev("n2", "ticket.opened", "周报模板改版（W4701 第二次提醒）", nil))
	e.now = e.now.Add(time.Minute)
	e.emit("src_d", "ticket", ev("n3", "ticket.opened", "季度预算评审会议改期", nil))
	if err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if n := e.batchCalls(); n != 1 {
		t.Fatalf("one model call for the whole batch, got %d", n)
	}
	req := ""
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, batchMark) {
			req = r.BodyText
		}
	}
	for _, s := range []string{"进行中", "本人忽略过", "评审 AgentSafe 资产 B18630823", "周报模板改版 W4701"} {
		if !strings.Contains(req, s) {
			t.Fatalf("existing records must be in the batch input, missing %q", s)
		}
	}

	n1, n2, n3 := e.byTitle("AgentSafe 资产确认（B18630823）"), e.byTitle("周报模板改版（W4701 第二次提醒）"), e.byTitle("季度预算评审会议改期")
	for _, c := range []struct {
		it             domain.Item
		action, target string
	}{{n1, domain.SweepAttach, open}, {n2, domain.SweepDuplicate, ignored}, {n3, domain.SweepMerge, old.ID}} {
		sw := c.it.Proposal.Sweep
		if c.it.Phase != domain.PhaseDropped || sw.State != "done" || sw.Action != c.action || sw.Target != c.target ||
			sw.By != "generate" || sw.Run == "" || sw.Size != 3 || sw.Chars == 0 || sw.Why == "" || sw.Batch == "" {
			t.Fatalf("%s: %s %+v", c.it.Title, c.it.Phase, sw)
		}
	}
	if why := n1.Proposal.Sweep.Why; strings.Contains(why, "N1") || strings.Contains(why, "E1") || !strings.Contains(why, "「AgentSafe 资产确认") || !strings.Contains(why, "「评审 AgentSafe") {
		t.Fatalf("batch numbers in the reason become titles: %q", why)
	}
	if ps := e.progressOf(open); len(ps) != 1 || !strings.Contains(ps[0].Text, "AgentSafe 资产确认") || !strings.Contains(ps[0].Text, "B18630823") {
		t.Fatalf("attach appends a supplement to the open item: %+v", ps)
	}
	if it := e.item(open); it.Title != "评审 AgentSafe 资产 B18630823" || it.Phase != domain.PhaseOpen {
		t.Fatalf("attach must not change the open item itself: %+v", it)
	}
	ev3 := e.sev("src_d", "n3")[0]
	got := e.item(old.ID).Proposal
	if !slices.Contains(got.Sources, ev3) || len(got.Merges) != 1 || got.Merges[0].Item != n3.ID || got.Merges[0].By != "generate" ||
		got.Merges[0].Why != "同一场预算评审会议" || got.Merges[0].Source != "src_d" {
		t.Fatalf("merge trace on the target: %+v", got)
	}
	if e.events("src_d")[0].Item != old.ID {
		t.Fatal("merged event must point at the target")
	}
	e.verify()

	// 再跑一轮：没有待归类的，不调模型。
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		t.Fatalf("nothing pending must not call the model: %v %d", err, e.batchCalls())
	}

	for _, id := range []string{n3.ID, n1.ID, n2.ID} {
		if err := e.svc.Unmerge(bg, owner, id); err != nil {
			t.Fatalf("unmerge %s: %v", id, err)
		}
		if it := e.item(id); it.Phase != domain.PhaseProposed || it.Proposal.Sweep.Action != domain.SweepUndone {
			t.Fatalf("undone %s: %s %+v", id, it.Phase, it.Proposal.Sweep)
		}
	}
	got = e.item(old.ID).Proposal
	if slices.Contains(got.Sources, ev3) || len(got.Merges) != 0 || !slices.Contains(got.NotSame, n3.ID) || e.events("src_d")[0].Item != n3.ID {
		t.Fatalf("unmerge moves the source back: %+v", got)
	}
	if ps := e.progressOf(open); len(ps) != 2 || !strings.Contains(ps[1].Text, "撤回上一条补充") {
		t.Fatalf("unmerge of attach appends a retraction: %+v", ps)
	}
	if err := e.svc.Unmerge(bg, owner, n1.ID); err == nil {
		t.Fatal("second unmerge must be a conflict")
	}
	e.verify()
}

// 拿不准只提示、给的去处不在候选里按留着；不够条数时等够时间再跑；关闭合并时待归类的按规则结清、不调模型。
func TestMergeBatchUndecidedWaitAndOff(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	open := e.openItem("季度预算评审会议")
	e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[
		{"new":"N1","action":"attach","target":"E1","reason":"像是同一场会","confident":false},
		{"new":"N2","action":"merge","target":"E9","reason":"编造的去处","confident":true}]}`})
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
	if u2.Phase != domain.PhaseProposed || u2.Proposal.Sweep.Action != domain.SweepKeep || !strings.Contains(u2.Proposal.Sweep.Why, "不在候选里") {
		t.Fatalf("made-up target keeps: %+v", u2.Proposal.Sweep)
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

// 不参与合并的来源不进批；不纳入已有记录时只和建议比；本人改过的提示词随调用送出。
func TestMergeBatchSettings(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.openItem("季度预算评审会议")
	body := builtinMergePrompt(t) + "\n自定义标记XYZ"
	e.setPolicy(func(p *policy.Policy) {
		p.Merge = policy.MergeSettings{ExcludeSources: []string{"src_x"}, SkipExisting: true, BatchSize: 2, Prompt: body}
	})
	e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[{"new":"N1","action":"keep","target":null,"reason":"不是一回事","confident":true}]}`})
	e.emit("src_x", "ticket", ev("x1", "ticket.opened", "季度预算评审会议改期", nil))
	e.emit("src_a", "ticket", ev("a1", "ticket.opened", "季度预算评审会议改期了", nil))
	e.emit("src_b", "ticket", ev("b1", "ticket.opened", "导出报表加一列", nil))
	if x := e.sev("src_x", "x1")[0]; x == "" {
		t.Fatal("no event")
	}
	for _, it := range e.proposed() {
		if it.Origin.Source == "src_x" && it.Proposal.Sweep != nil {
			t.Fatalf("excluded source must not be marked: %+v", it.Proposal.Sweep)
		}
	}
	if err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	// 只和建议比：src_a 那条唯一的相似记录是被排除来源的建议与进行中的事项，都不算候选，所以不调模型。
	if n := e.batchCalls(); n != 0 {
		t.Fatalf("no candidates under these settings, got %d calls", n)
	}
	if sw := e.byTitle("导出报表加一列").Proposal.Sweep; sw.By != "rule" || sw.Action != domain.SweepKeep {
		t.Fatalf("%+v", sw)
	}

	e.setPolicy(func(p *policy.Policy) { p.Merge.SkipExisting = false })
	e.emit("src_c", "ticket", ev("c1", "ticket.opened", "季度预算评审会议改期一事", nil))
	e.emit("src_d", "ticket", ev("d1", "ticket.opened", "另一件不相干的事", nil))
	if err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if e.batchCalls() != 1 {
		t.Fatalf("with existing records included there is a candidate: %d", e.batchCalls())
	}
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, batchMark) && !strings.Contains(r.BodyText, "自定义标记XYZ") {
			t.Fatal("the owner's prompt must be sent")
		}
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
		e.setPolicy(func(p *policy.Policy) { p.MergeJudge = policy.MergeSame; p.Merge.BatchSize = 20; p.Merge.BudgetTokens = budget })
		e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[]}`})
		long := strings.Repeat("需要本人确认的资产信息与负责人说明", 6) // 标题按上限 120 字送出
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
		if got := similarScore(c.a, "", c.b, "") >= policy.SimilarityMin[""]; got != c.hit {
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

// sweep227 跑线上那次归类：一次模型调用，mid 与 gp 并进 keep。
func (e *env) sweep227() {
	e.t.Helper()
	e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[
		{"new":"N1","action":"merge","target":"N3","reason":"同一个 PR #227 的评审通知","confident":true},
		{"new":"N2","action":"merge","target":"N3","reason":"同一个 PR #227","confident":true}]}`})
	e.now = e.now.Add(31 * time.Minute)
	if err := e.svc.Sweep(bg); err != nil || e.batchCalls() != 1 {
		e.t.Fatalf("one model call: %v %d", err, e.batchCalls())
	}
}

// 线上 PR #227：平台上的三条更新挂到 gp，记进它的消息清单。归类把 mid、gp 并进 keep：被折那条名下的消息全部搬到 keep，
// 按到达先后排，每条带合并留痕（记着从哪条来）；撤销后原样回到原处。
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
	got := e.item(keep.ID).Proposal
	want := append(append(append([]string{}, i...), m...), g...)
	if !slices.Equal(got.Sources, want) || len(got.Merges) != 7 {
		t.Fatalf("every held message moves, in arrival order: %+v", got)
	}
	for k, x := range got.Merges {
		from := mid.ID
		if k >= 3 {
			from = gp.ID
		}
		if x.Event != want[k+1] || x.Item != from || x.By != "generate" || x.Batch == "" {
			t.Fatalf("merge trace %d: %+v", k, x)
		}
	}
	for _, src := range []string{"src_mail_icloud", "src_mail", "src_github"} {
		for _, x := range e.events(src) {
			if x.Item != keep.ID {
				t.Fatalf("event left behind: %+v", x)
			}
		}
	}
	for _, id := range []string{mid.ID, gp.ID} {
		if it := e.item(id); it.Phase != domain.PhaseDropped || it.Proposal.Sweep.Target != keep.ID {
			t.Fatalf("folded %s: %+v", id, it.Proposal.Sweep)
		}
	}
	e.verify()

	if err := e.svc.Unmerge(bg, owner, gp.ID); err != nil {
		t.Fatal(err)
	}
	got = e.item(keep.ID).Proposal
	if !slices.Equal(got.Sources, want[:4]) || len(got.Merges) != 3 || !slices.Equal(e.item(gp.ID).Proposal.Sources, g) {
		t.Fatalf("unmerge takes back exactly what was moved: %+v", got)
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

	e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[{"new":"N1","action":"attach","target":"E1","reason":"同一个资产编号 B18630823","confident":true}]}`})
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
	if ps := e.progressOf(open); len(ps) != 1 || !strings.Contains(ps[0].Text, "AgentSafe 资产确认") {
		t.Fatalf("still one supplement note: %+v", ps)
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

// 相似度档位（merge.similarity）：默认档就是改成档位前写死的 0.30；改档写进策略，同一个运行中的服务下一批即按新档初筛，不重启。
// 「季度预算评审会议」对「预算会议改期 / 延期 / 推迟」都是 0.33、对「季度总结 / 复盘 / 规划会议」都是 0.25：严档两条都不送模型，默认档只送前者，宽档两条都送。
func TestMergeBatchSimilarityTiers(t *testing.T) {
	if got := (policy.MergeSettings{}).MinScore(); got != 0.30 {
		t.Fatalf("the default tier must equal the former constant 0.30, got %v", got)
	}
	if policy.SimilarityMin[policy.SimilarityLoose] != 0.25 || policy.SimilarityMin[policy.SimilarityStrict] != 0.40 || len(policy.SimilarityMin) != 3 {
		t.Fatalf("tiers: %v", policy.SimilarityMin)
	}
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.openItem("季度预算评审会议")
	e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[]}`})
	n, done := 0, []string{}
	round := func(tier string, titles ...string) (asked []string) {
		t.Helper()
		// 前几轮的建议换个来源并排除在比对之外，每轮只和那条进行中的事项比。
		e.setPolicy(func(p *policy.Policy) { p.Merge.Similarity, p.Merge.ExcludeSources = tier, slices.Clone(done) })
		src := fmt.Sprintf("src_r%d", len(done))
		done = append(done, src)
		calls := e.batchCalls()
		for _, title := range titles {
			n++
			e.emit(src, "ticket", ev(fmt.Sprintf("tier-%d", n), "ticket.opened", title, nil))
		}
		e.now = e.now.Add(31 * time.Minute) // 等满成批的时间
		if err := e.svc.Sweep(bg); err != nil {
			t.Fatal(err)
		}
		if e.batchCalls() > calls {
			req := e.stub.Requests()
			body := req[len(req)-1].BodyText
			for _, title := range titles {
				if strings.Contains(body, "标题："+title) {
					asked = append(asked, title)
				}
			}
		}
		return
	}
	if got := round(policy.SimilarityStrict, "预算会议改期", "季度总结会议"); len(got) != 0 {
		t.Fatalf("strict sent %v to the model", got)
	}
	for _, it := range e.proposed() {
		if sw := it.Proposal.Sweep; sw == nil || sw.By != "rule" || sw.Action != domain.SweepKeep {
			t.Fatalf("strict: %s must be kept by rule, got %+v", it.Title, sw)
		}
	}
	if got := round("", "预算会议延期", "季度复盘会议"); !slices.Equal(got, []string{"预算会议延期"}) {
		t.Fatalf("default tier sent %v, want only the 0.33 one", got)
	}
	if got := round(policy.SimilarityLoose, "预算会议推迟", "季度规划会议"); len(got) != 2 {
		t.Fatalf("loose tier sent %v, want both", got)
	}
	// 档位不在三档里：策略写入拒绝。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "merge", func(p *policy.Policy) error { p.Merge.Similarity = "medium"; return nil })
		return err
	}); err == nil {
		t.Fatal("an unknown tier must be refused")
	}
	e.verify()
}
