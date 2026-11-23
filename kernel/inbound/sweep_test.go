package inbound

import (
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
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
		{"new":"N1","action":"attach","target":"E1","reason":"同一个资产编号 B18630823","confident":true},
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
	e.stub.When(batchMark, modelstub.Reply{Content: `{"decisions":[{"new":"N1","action":"keep","reason":"不是一回事","confident":true}]}`})
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
		if got := similarScore(c.a, "", c.b, "") >= sweepMin; got != c.hit {
			t.Errorf("%q vs %q: score %.2f", c.a, c.b, similarScore(c.a, "", c.b, ""))
		}
	}
}
