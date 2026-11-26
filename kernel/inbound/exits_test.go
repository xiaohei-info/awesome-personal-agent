package inbound

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
)

func (e *env) disposition(source, id string) domain.SourceEvent {
	e.t.Helper()
	for _, ev := range e.events(source) {
		if ev.ID == id || strings.HasSuffix(ev.DedupeKey.EventID, id) {
			return ev
		}
	}
	e.t.Fatalf("event %s not found in %v", id, e.events(source))
	return domain.SourceEvent{}
}

// ACT-IRL-02：then: proposed 省略 kind 也直接成为 task 建议，跳过第一题，只问第二题（不调生成端点）。
func TestProposedExitDefaultsToTaskWithoutModel(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_x", "exits", ev("p1", "x.prop", "续办证件", nil))
	d := e.disposition("src_x", "p1")
	if d.Disposition != domain.DispositionProposed || d.Item == "" {
		t.Fatalf("event %+v", d)
	}
	if it := e.item(d.Item); it.Phase != domain.PhaseProposed || it.Kind != "task" || it.Title != "续办证件" {
		t.Fatalf("item %+v", it)
	}
	if n := len(e.cls.Batches()); n != 1 || e.cls.Batches()[0].Questions != 1 {
		t.Fatalf("classifier batches %+v, want 1 batch with 1 urgent question", e.cls.Batches())
	}
}

// ACT-IRL-08：digest 出口只记 digest，不建事项、不调模型。
func TestDigestExitCreatesNothing(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_x", "exits", ev("d1", "x.news", "本周简讯", nil))
	d := e.disposition("src_x", "d1")
	if d.Disposition != domain.DispositionDigest || d.Item != "" || len(e.allItems()) != 0 || len(e.stub.Requests()) != 0 {
		t.Fatalf("event %+v items %d requests %d", d, len(e.allItems()), len(e.stub.Requests()))
	}
}

// ACT-IRL-10：eq 与 in 命中直接成建议；不命中的事件没有规则，走 screen（这里判断为不值得）。
func TestWhereEqAndIn(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	e.emit("src_x", "exits",
		ev("w1", "x.who", "a", map[string]any{"sender": "alice"}),
		ev("w2", "x.who", "b", map[string]any{"sender": "bob"}),
		ev("w5", "x.who-in", "c", map[string]any{"sender": "carol"}),
		ev("w6", "x.who-in", "d", map[string]any{"sender": "dave"}))
	want := map[string]domain.Disposition{"w1": domain.DispositionProposed, "w2": domain.DispositionIgnored,
		"w5": domain.DispositionProposed, "w6": domain.DispositionIgnored}
	for id, disp := range want {
		if got := e.disposition("src_x", id).Disposition; got != disp {
			t.Errorf("%s: %s, want %s", id, got, disp)
		}
	}
	if n := questions(e); n != 6 { // w1/w5 规则直通只问 1 题（急不急），w2/w6 走完整 judge 问 2 题
		t.Fatalf("model called %d times", n)
	}
}

// ACT-IRL-11：gte 10 且 lt 20 的边界。
func TestWhereNumericBounds(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	var evs []map[string]any
	for id, v := range map[string]float64{"n1": 9, "n2": 10, "n3": 19, "n4": 20} {
		evs = append(evs, ev(id, "x.size", id, map[string]any{"metrics": map[string]float64{"size": v}}))
	}
	e.emit("src_x", "exits", evs...)
	for id, disp := range map[string]domain.Disposition{"n1": domain.DispositionIgnored, "n2": domain.DispositionProposed,
		"n3": domain.DispositionProposed, "n4": domain.DispositionIgnored} {
		if got := e.disposition("src_x", id).Disposition; got != disp {
			t.Errorf("%s: %s, want %s", id, got, disp)
		}
	}
}

// FIX-4：统一 judge 固定使用内置包 inbound-triage/triage-screen，不同来源和自定义包共用同一份模板。
func TestUnifiedJudgeAlwaysUsesBuiltinTriageScreen(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	var calls []string
	e.svc.o.Judge = recorder{inner: e.svc.o.Judge, on: func(pack, tmpl string) { calls = append(calls, pack+"/"+tmpl) }}
	e.emit("src_acc", "accsrc", ev("t-acc", "acc.process", "s", nil))
	e.emit("src_inbox", "inbox", ev("t-mail", "mail.received", "s", nil))
	if len(calls) < 2 {
		t.Fatalf("expected judge calls, got %v", calls)
	}
	for _, got := range calls {
		if got != DefaultPack+"/"+Template {
			t.Fatalf("judge call used %q, want %s/%s", got, DefaultPack, Template)
		}
	}
}

// ACT-IRL-16/17、CAL-10：project_by fixed 取固定项目；none 取来源默认项目，默认项目清空后不归项目。
func TestProjectByFixedNoneAndSourceDefault(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_x", "exits", ev("f1", "x.fixed", "物业缴费", nil), ev("o1", "x.none", "默认项目事件", nil))
	if got := e.item(e.disposition("src_x", "f1").Item).Project; got != e.mail {
		t.Fatalf("fixed project %q", got)
	}
	if got := e.item(e.disposition("src_x", "o1").Item).Project; got != "" {
		t.Fatalf("none without default: %q", got)
	}
	sn := snapshot{}
	src := domain.Source{ID: "src_x", SourceKind: "exits", DefaultProject: e.web}
	m := &matched{pack: "inbound-triage", rule: ruleByID(t, e, "x-none")}
	if got, err := e.svc.project(sn, src, domain.SourceEvent{}, m); err != nil || got != e.web {
		t.Fatalf("none with default: %q %v", got, err)
	}
	src.DefaultProject = "" // 清空默认项目只影响之后的事件
	if got, _ := e.svc.project(sn, src, domain.SourceEvent{}, m); got != "" {
		t.Fatalf("cleared default: %q", got)
	}
	if got, _ := e.svc.project(sn, domain.Source{DefaultProject: e.web}, domain.SourceEvent{}, nil); got != e.web {
		t.Fatalf("no rule still takes the default: %q", got)
	}
}

// then: process 统一走 judge 与队列，不调用生成端点抽取。
func TestProcessExitRunsUnifiedJudgeAndQueues(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.emit("src_x", "exits", ev("c1", "x.chat", "群聊", nil))
	got := e.disposition("src_x", "c1")
	if got.Judgment == "" || got.QueueLevel != runner.LevelUrgent || len(e.stub.Requests()) != 0 {
		t.Fatalf("got %+v requests %d", got, len(e.stub.Requests()))
	}
}

// 没有注入处理器时 alert 出口报真实错误，事件保持 pending，不假装处理（extract 出口由本包内置处理）。
func TestExitWithoutHandlerFailsLoudly(t *testing.T) {
	e := newEnv(t, true)
	e.wantErr = true
	e.svc.o.Alert = nil
	sample := func(id string, v float64) map[string]any {
		return ev(id, "x.metric", "磁盘", map[string]any{"correlation_keys": []string{"d"}, "metrics": map[string]float64{"value": v}})
	}
	e.emitRaw("src_x", "exits", sample("c0", 80), sample("c1", 92))
	select {
	case err := <-e.errs:
		if !strings.Contains(err.Error(), "no handler is configured") {
			t.Fatalf("error %v", err)
		}
	default:
		t.Fatal("missing handler was not reported")
	}
	if got := e.disposition("src_x", "c1").Disposition; got != domain.DispositionPending {
		t.Fatalf("disposition %s", got)
	}
}

type recorder struct {
	inner Judge
	on    func(pack, template string)
}

func (r recorder) Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error) {
	r.on(c.Pack, c.Template)
	return r.inner.Judge(ctx, c)
}

func questions(e *env) (n int) {
	for _, b := range e.cls.Batches() {
		n += b.Questions
	}
	return n
}

func ruleByID(t *testing.T, e *env, id string) packs.SourceRule {
	t.Helper()
	for _, p := range e.loadr.List() {
		for _, r := range p.SourceRules {
			if r.ID == id {
				return r
			}
		}
	}
	t.Fatalf("rule %s not found", id)
	return packs.SourceRule{}
}

// ACT-DGA-18：简报"知会"条目逐条转为建议：只转点的那条，操作者是本人，另一条仍是 digest；转过的不能再转。
func TestDigestEventCanBeProposedOneByOne(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_x", "exits", ev("d1", "x.news", "简讯一", nil), ev("d2", "x.news", "简讯二", nil))
	first, second := e.disposition("src_x", "d1"), e.disposition("src_x", "d2")
	if first.Disposition != domain.DispositionDigest || second.Disposition != domain.DispositionDigest {
		t.Fatalf("setup: %+v %+v", first, second)
	}
	got, err := e.svc.ProposeUnjudged(bg, owner, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != domain.DispositionProposed || got.Item == "" {
		t.Fatalf("event %+v", got)
	}
	if it := e.item(got.Item); it.Phase != domain.PhaseProposed || it.Title != "简讯一" {
		t.Fatalf("item %+v", it)
	}
	if d := e.disposition("src_x", "d2"); d.Disposition != domain.DispositionDigest || d.Item != "" {
		t.Fatalf("the other digest event must stay digest: %+v", d)
	}
	if _, err := e.svc.ProposeUnjudged(bg, owner, first.ID); code(err) != domain.CodeConflict {
		t.Fatalf("second propose: %v", err)
	}
}

// ACT-IRL-04/06/07/18：自定义包里的 then: process 规则固定取内置包 inbound-triage/triage-screen，事件进队列不调用生成端点。
func TestCustomPackRuleUsesBuiltinJudgeTemplate(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	var seen []string
	e.svc.o.Judge = recorder{inner: e.svc.o.Judge, on: func(pack, tmpl string) { seen = append(seen, pack+"/"+tmpl) }}
	e.emit("src_acc", "accsrc", ev("a1", "acc.process", "s", nil))
	if d := e.disposition("src_acc", "a1"); d.Disposition != domain.DispositionProposed || d.Queue != runner.QueueDone {
		t.Errorf("a1 should be processed via unified judge: %+v", d)
	}
	if len(seen) == 0 || seen[0] != DefaultPack+"/"+Template {
		t.Fatalf("templates %v", seen)
	}
}

// ACT-JDP-02：同一次拉取落盘的 40 条自由文本事件，分类端点按 max_batch 收到整批而不是 40 个单题请求。
func TestInboundScreenSubmitsInBatches(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	var evs []map[string]any
	for i := 0; i < 40; i++ {
		evs = append(evs, ev(fmt.Sprintf("b%02d", i), "acc.process", fmt.Sprintf("subject %d", i), nil))
	}
	e.emit("src_acc", "accsrc", evs...)
	var got []int
	for _, b := range e.cls.Batches() {
		got = append(got, b.Questions)
	}
	// 40 条事件 × 2 题 = 80 题，按 max_batch=32 分成 3 批（32+32+16）。
	if len(got) != 3 || got[0]+got[1]+got[2] != 80 || got[0] > 32 || got[1] > 32 || got[2] > 32 {
		t.Fatalf("classifier batches %v, want 32+32+16", got)
	}
}
