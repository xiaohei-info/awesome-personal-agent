package inbound

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
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

// ACT-IRL-02：then: proposed 省略 kind 也直接成为 task 建议，不调模型。
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
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("model called %d times", n)
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
	if n := questions(e); n != 2 { // 只有 w2、w6 经过判断
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

// ACT-IRL-06/19：旧名 judge 等同 screen；规则的 screen 字段覆盖初筛模板名。
func TestScreenTemplateOverrideAndJudgeAlias(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	var templates []string
	for _, src := range []struct{ kind, typ string }{{"exits", "x.tpl"}, {"inbox", "mail.received"}} {
		if got := e.screenTemplate(src.kind, src.typ); got != "" {
			templates = append(templates, got)
		}
	}
	if len(templates) != 2 || templates[0] != "acc-screen" || templates[1] != "triage-screen" {
		t.Fatalf("templates %v", templates)
	}
}

// screenTemplate 发出一条事件，返回交给判断型的模板名。
func (e *env) screenTemplate(kind, typ string) string {
	var got string
	e.svc.o.Judge = recorder{inner: e.svc.o.Judge, on: func(tmpl string) { got = tmpl }}
	e.emit("src_"+kind, kind, ev("t-"+typ, typ, "s", nil))
	return got
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

// extract 出口交给注入的处理器。
func TestExtractExitRoutesToHandler(t *testing.T) {
	e := newEnv(t, true)
	var routed []string
	e.svc.o.Extract = func(ctx context.Context, src domain.Source, ev domain.SourceEvent, rule string) error {
		routed = append(routed, ev.ID+"@"+rule)
		return e.svc.dispose(ctx, nil, ev, disposeIn{Event: ev.ID, Rule: rule, Disposition: domain.DispositionIgnored})
	}
	e.emit("src_x", "exits", ev("c1", "x.chat", "群聊", nil))
	if len(routed) != 1 || !strings.HasSuffix(routed[0], "@inbound-triage/source_rules/x-chat") {
		t.Fatalf("routed %v", routed)
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
	on    func(template string)
}

func (r recorder) Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error) {
	r.on(c.Template)
	return r.inner.Judge(ctx, c)
}

func (r recorder) Do(ctx context.Context, c direct.Call) (direct.Outcome, error) {
	return r.inner.Do(ctx, c)
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

// ACT-IRL-04/06/07/18：自定义包里省略 screen、draft、prescreen、extract 的规则取内置包的内置模板，事件不卡在 pending。
func TestCustomPackRuleFallsBackToBuiltinTemplates(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"yes","confidence":0.95}`)
	e.draft(`{"candidates":[{"title":"回电话","kind":"task"}],"title":"回电话","summary":"s","kind":"task"}`)
	var seen []string
	e.svc.o.Judge = recorder{inner: e.svc.o.Judge, on: func(tmpl string) { seen = append(seen, tmpl) }}
	e.emit("src_acc", "accsrc", ev("a1", "acc.screen", "s", nil), ev("a2", "acc.judge", "j", nil), ev("a3", "acc.extract", "x", nil))
	for _, id := range []string{"a1", "a2"} {
		if d := e.disposition("src_acc", id); d.Disposition != domain.DispositionProposed {
			t.Errorf("%s: %+v", id, d)
		}
	}
	if d := e.disposition("src_acc", "a3"); d.Disposition == domain.DispositionPending {
		t.Errorf("extract event stuck pending: %+v", d)
	}
	if len(seen) == 0 || seen[0] != "triage-screen" {
		t.Fatalf("templates %v", seen)
	}
}

// ACT-JDP-02：同一次拉取落盘的 40 条自由文本事件，分类端点按 max_batch 收到整批而不是 40 个单题请求。
func TestInboundScreenSubmitsInBatches(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	var evs []map[string]any
	for i := 0; i < 40; i++ {
		evs = append(evs, ev(fmt.Sprintf("b%02d", i), "acc.screen", fmt.Sprintf("subject %d", i), nil))
	}
	e.emit("src_acc", "accsrc", evs...)
	var got []int
	for _, b := range e.cls.Batches() {
		got = append(got, b.Questions)
	}
	if len(got) != 2 || got[0]+got[1] != 40 || got[0] > 32 || got[1] > 32 {
		t.Fatalf("classifier batches %v, want 32+8", got)
	}
}
