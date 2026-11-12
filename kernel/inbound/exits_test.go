package inbound

import (
	"context"
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
	if n := len(e.stub.Requests()); n != 2 { // 只有 w2、w6 经过判断
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
	if len(templates) != 2 || templates[0] != "acc-screen" || templates[1] != "triage" {
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

// extract、alert 出口交给注入的处理器；alert 的变化条件按注入的序列上一次取值判断，没有序列时不成立。
func TestExtractAndAlertExitsRouteToHandlers(t *testing.T) {
	e := newEnv(t, true)
	e.judge(`{"choice":"ignore","confidence":0.95}`)
	var routed []string
	handler := func(ctx context.Context, src domain.Source, ev domain.SourceEvent, rule string) error {
		routed = append(routed, ev.ID+"@"+rule)
		return e.svc.dispose(ctx, nil, ev, disposeIn{Event: ev.ID, Rule: rule, Disposition: domain.DispositionAlerted})
	}
	e.svc.o.Extract, e.svc.o.Alert = handler, handler
	e.svc.o.Series = fixedSeries{prev: 80}
	e.emit("src_x", "exits",
		ev("c1", "x.chat", "群聊", nil),
		ev("m1", "x.metric", "磁盘", map[string]any{"metrics": map[string]float64{"value": 92}}),
		ev("m2", "x.metric", "磁盘", map[string]any{"metrics": map[string]float64{"value": 85}}))
	x := "inbound-triage/source_rules/"
	got := map[string]bool{}
	for _, r := range routed {
		got[r[strings.Index(r, "@")+1:]] = true
	}
	if len(routed) != 2 || !got[x+"x-chat"] || !got[x+"x-alert"] {
		t.Fatalf("routed %v", routed)
	}
	if got := e.disposition("src_x", "m2").Disposition; got != domain.DispositionIgnored {
		t.Fatalf("m2 (80→85 does not cross 90) %s", got)
	}
}

// 没有注入处理器时 extract 出口报真实错误，事件保持 pending，不假装处理。
func TestExitWithoutHandlerFailsLoudly(t *testing.T) {
	e := newEnv(t, true)
	e.wantErr = true
	e.emitRaw("src_x", "exits", ev("c1", "x.chat", "群聊", nil))
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

func (r recorder) Do(ctx context.Context, c direct.Call) (direct.Outcome, error) {
	r.on(c.Template)
	return r.inner.Do(ctx, c)
}

// fixedSeries 让每个序列的上一次取值都是 prev。
type fixedSeries struct{ prev float64 }

func (f fixedSeries) Previous(domain.SourceEvent, string) (float64, bool, error) {
	return f.prev, true, nil
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
