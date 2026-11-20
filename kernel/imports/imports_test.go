package imports

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg  = context.Background()
	now = time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
	web = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

type env struct {
	t       *testing.T
	st      *store.Store
	x       *command.Executor
	svc     *Service
	auth    *auth.Auth
	clk     *clock.Clock
	clock   time.Time
	waiting []string // 被要求排第一轮跟进的事项（测试替身：跟进服务在 kernel/proactive/followup，那边另有测试）
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, materials.Module, executors.Module, items.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, auth: auth.New(func() time.Time { return now }), clock: now}
	clk := func() time.Time { return e.clock }
	e.x = command.New(st, clk)
	if err := projects.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	e.svc, err = Register(e.x, Options{Kinds: items.NewKinds(), Now: clk, Waiting: func(_ *store.Tx, it domain.Item, at time.Time) error {
		if !at.Equal(e.clock) {
			t.Errorf("follow-up must start from the import time %v, got %v", e.clock, at)
		}
		e.waiting = append(e.waiting, it.ID)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if e.clk, err = clock.New(st, items.HandleTimer, clk); err != nil {
		t.Fatal(err)
	}
	return e
}

func lines(ls ...string) []Line {
	out := make([]Line, len(ls))
	for i, l := range ls {
		out[i] = Line{N: i + 1, Text: []byte(l)}
	}
	return out
}

func (e *env) run(id command.Identity, source string, dry bool, ls ...string) (*Result, error) {
	return e.svc.Run(bg, Params{Identity: id, Source: source, DryRun: dry, Lines: lines(ls...)})
}

func (e *env) must(id command.Identity, source string, dry bool, ls ...string) *Result {
	e.t.Helper()
	r, err := e.run(id, source, dry, ls...)
	if err != nil {
		e.t.Fatal(err)
	}
	return r
}

func view[T any](t *testing.T, e *env, f func(tx *store.Tx) (T, error)) T {
	t.Helper()
	var out T
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = f(tx); return }); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) head() int64 {
	return view(e.t, e, func(tx *store.Tx) (int64, error) { return eventlog.Head(tx) })
}

func (e *env) existingProject(name string) string {
	e.t.Helper()
	r, err := e.x.Do(bg, command.Request{Identity: web, Command: "project.create", Input: json.RawMessage(fmt.Sprintf(`{"name":%q}`, name))})
	if err != nil {
		e.t.Fatal(err)
	}
	var out struct{ ID string }
	_ = json.Unmarshal(r.Value, &out)
	return out.ID
}

// ---- 验收计划：flow-import-apply 的 ACT-IMP-01 ----

const (
	projAlpha = `{"type":"project","external_id":"acc/alpha","name":"Alpha Imp","status":"active","created_at":"2026-03-02T09:00:00+08:00"}`
	projHome  = `{"type":"project","external_id":"acc/home","name":"  ACC-HOME "}`
	itemOne   = `{"type":"item","external_id":"T-0001","project":"acc/alpha","item":{"kind":"task","title":"One","deadline":null,"tags":["imported"],"ext":{"legacy":{"slot":"14:00-16:00"}}},"history":[
{"at":"2026-10-01T09:00:00+08:00","event":"created","phase":"open"},
{"at":"2026-10-02T12:00:00+08:00","event":"progress","on":"2026-10-02","text":"p1","precision":"day"},
{"at":"2026-10-03T12:00:00+08:00","event":"progress","on":"2026-10-03","text":"p2","precision":"day"},
{"at":"2026-10-04T12:00:00+08:00","event":"progress","on":"2026-10-04","text":"p3","precision":"day"},
{"at":"2026-10-05T10:30:00+08:00","event":"progress","on":"2026-10-05","text":"p4"},
{"at":"2026-10-06T16:45:00+08:00","event":"progress","on":"2026-10-06","text":"p5"},
{"at":"2026-10-07T18:00:00+08:00","event":"transition","to":"dropped","reason":"not now"},
{"at":"2026-10-08T09:00:00+08:00","event":"transition","to":"open","reason":"back"},
{"at":"2026-10-09T18:00:00+08:00","event":"transition","to":"done","reason":"finished","precision":"day"}]}`
	itemTwo = `{"type":"item","external_id":"T-0002","project":"acc/home","item":{"kind":"task","title":"Two","deadline":"2026-11-16T03:00:00Z","waiting_on":{"kind":"external","ref":"vendor"}},"history":[
{"at":"2026-10-10T09:00:00+08:00","event":"created","phase":"open"},
{"at":"2026-10-11T09:00:00+08:00","event":"transition","to":"waiting","reason":"asked"}]}`
)

func plan() []string { return []string{projAlpha, projHome, itemOne, itemTwo} }

func outcomes(r *Result) []string {
	var o []string
	for _, x := range r.Results {
		o = append(o, x.Outcome)
	}
	return o
}

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t
}

// ACT-IMP-02：预检逐行给结果，项目名字绑定到已有项目，不写账本、不建批次、不占条数。
func TestDryRunWritesNothing(t *testing.T) {
	e := setup(t)
	home := e.existingProject("acc-home")
	head := e.head()
	r := e.must(web, "acc-imp", true, plan()...)
	if r.Batch != "" || r.Created != 3 || r.Existing != 1 || r.Invalid != 0 {
		t.Fatalf("dry run: %+v", r)
	}
	if got := strings.Join(outcomes(r), ","); got != "created,existing,created,created" || r.Results[1].Object != home {
		t.Fatalf("outcomes %s, project object %q want %q", got, r.Results[1].Object, home)
	}
	if e.head() != head {
		t.Fatal("a dry run must not write events")
	}
	if bs := view(t, e, items.ImportBatches); len(bs) != 0 {
		t.Fatalf("a dry run must not open a batch: %+v", bs)
	}
	if n := len(e.waiting); n != 0 {
		t.Fatalf("a dry run must not arm follow-ups: %d", n)
	}
}

// ACT-IMP-03/04/05：本人在 Web 导入不需要授权单；时间线与输入一致，事件带导入标记；项目按对照、名字绑定或新建。
func TestImportKeepsTheOriginalTimeline(t *testing.T) {
	e := setup(t)
	home := e.existingProject("acc-home")
	base := e.head()
	r := e.must(web, "acc-imp", false, plan()...)
	if !strings.HasPrefix(r.Batch, "imp_") || r.Created != 3 || r.Existing != 1 {
		t.Fatalf("import: %+v", r)
	}
	one := r.Results[2].Object
	// 恰好一条，origin 与 imported 正确。
	found := view(t, e, func(tx *store.Tx) ([]domain.Item, error) {
		return items.List(tx, items.Filter{OriginSource: "import:acc-imp", OriginRef: "T-0001"})
	})
	if len(found) != 1 || found[0].ID != one {
		t.Fatalf("origin query: %+v", found)
	}
	it := found[0]
	if it.Origin.Source != "import:acc-imp" || it.Origin.Ref != "T-0001" || it.Imported == nil || it.Imported.Batch != r.Batch || !it.Imported.At.Equal(now) {
		t.Fatalf("origin/imported: %+v %+v", it.Origin, it.Imported)
	}
	if it.Phase != domain.PhaseDone || it.Assignee != "" || string(it.Ext) != `{"legacy":{"slot":"14:00-16:00"}}` {
		t.Fatalf("final fields: %+v", it)
	}
	if !it.CreatedAt.Equal(mustTime("2026-10-01T09:00:00+08:00")) || !it.UpdatedAt.Equal(mustTime("2026-10-09T18:00:00+08:00")) {
		t.Fatalf("created_at %v updated_at %v must follow the original times", it.CreatedAt, it.UpdatedAt)
	}
	if it.Version != 9 {
		t.Fatalf("one version per history entry, got %d", it.Version)
	}
	// 事件：条数、类型、原始时刻、批次与写入时刻、precision 载荷。
	es := view(t, e, func(tx *store.Tx) ([]eventlog.Entry, error) { return eventlog.ReadObject(tx, one) })
	wantAt := []string{"2026-10-01T09:00:00+08:00", "2026-10-02T12:00:00+08:00", "2026-10-03T12:00:00+08:00", "2026-10-04T12:00:00+08:00", "2026-10-05T10:30:00+08:00",
		"2026-10-06T16:45:00+08:00", "2026-10-07T18:00:00+08:00", "2026-10-08T09:00:00+08:00", "2026-10-09T18:00:00+08:00"}
	wantTy := []string{"item.created", "item.progress_added", "item.progress_added", "item.progress_added", "item.progress_added", "item.progress_added",
		"item.phase_changed", "item.phase_changed", "item.phase_changed"}
	wantPrec := []string{"", "day", "day", "day", "", "", "", "", "day"}
	if len(es) != len(wantAt) {
		t.Fatalf("%d events", len(es))
	}
	for i, en := range es {
		var pl struct {
			Precision string `json:"precision"`
		}
		_ = json.Unmarshal(en.Event.Payload, &pl)
		if en.Event.Type != wantTy[i] || !en.Event.At.Equal(mustTime(wantAt[i])) || en.Event.Imported != r.Batch || !en.Event.RecordedAt.Equal(now) || pl.Precision != wantPrec[i] {
			t.Errorf("event %d: %s at %v batch %q recorded %v precision %q", i, en.Event.Type, en.Event.At, en.Event.Imported, en.Event.RecordedAt, pl.Precision)
		}
		if en.Event.Grant != "" || en.Event.Actor.Principal != "owner" {
			t.Errorf("event %d: grant %q actor %+v", i, en.Event.Grant, en.Event.Actor)
		}
	}
	prog := view(t, e, func(tx *store.Tx) ([]domain.Progress, error) { return items.Progress(tx, one) })
	if len(prog) != 5 || prog[0].Precision != "day" || prog[3].Precision != "" || prog[0].On != "2026-10-02" || !prog[0].At.Equal(mustTime(wantAt[1])) {
		t.Fatalf("progress: %+v", prog)
	}
	// 投影与事件折叠一致（回放得到同一份时间线）。
	if err := e.st.View(bg, func(tx *store.Tx) error {
		if err := items.Verify(tx); err != nil {
			return err
		}
		return projects.Verify(tx)
	}); err != nil {
		t.Fatal(err)
	}
	// 项目：新建的取 created_at 为事件时刻、不设名单；同名项目被绑定而不改、不重复建。
	ps := view(t, e, func(tx *store.Tx) ([]domain.Project, error) { return projects.List(tx) })
	if len(ps) != 2 {
		t.Fatalf("projects: %+v", ps)
	}
	alpha := r.Results[0].Object
	pe := view(t, e, func(tx *store.Tx) ([]eventlog.Entry, error) { return eventlog.ReadObject(tx, alpha) })
	if len(pe) != 1 || !pe[0].Event.At.Equal(mustTime("2026-03-02T09:00:00+08:00")) || pe[0].Event.Imported != r.Batch {
		t.Fatalf("project event: %+v", pe)
	}
	a := view(t, e, func(tx *store.Tx) (domain.Project, error) { return projects.Get(tx, alpha) })
	if a.Name != "Alpha Imp" || a.AllowedExecutors != nil || len(a.Repos) != 0 || a.Capture {
		t.Fatalf("alpha: %+v", a)
	}
	h := view(t, e, func(tx *store.Tx) (domain.Project, error) { return projects.Get(tx, home) })
	if h.Name != "acc-home" || r.Results[1].Object != home {
		t.Fatalf("existing project must stay untouched: %+v", h)
	}
	two := view(t, e, func(tx *store.Tx) (domain.Item, error) { return items.Get(tx, r.Results[3].Object) })
	if two.Project != home || two.Phase != domain.PhaseWaiting || two.WaitingOn == nil {
		t.Fatalf("T-0002: %+v", two)
	}
	// 批次：GET /imports 的数据。
	bs := view(t, e, Batches)
	if len(bs) != 1 || bs[0].ID != r.Batch || bs[0].Source != "acc-imp" || bs[0].Created != 3 || bs[0].Existing != 1 || bs[0].Actor != "owner" || bs[0].Grant != "" {
		t.Fatalf("batches: %+v", bs)
	}
	// 事项事件只进账本与折叠：现场读法只看到一条 import.completed（ACT-IMP-11）。
	live := view(t, e, func(tx *store.Tx) ([]eventlog.Entry, error) { return eventlog.ReadLive(tx, base, 1000) })
	var types []string
	for _, en := range live {
		types = append(types, en.Event.Type)
	}
	// 新建的 existingProject 事件在 base 之前；之后现场只有 import.completed。
	if strings.Join(types, ",") != "import.completed" {
		t.Fatalf("live events after import: %v", types)
	}
	var done struct {
		Source                     string
		Created, Existing, Invalid int
	}
	_ = json.Unmarshal(live[0].Event.Payload, &done)
	if live[0].Event.Imported != "" || live[0].Event.Object != r.Batch || done.Source != "acc-imp" || done.Created != 3 || done.Existing != 1 || !domain.IsRegisteredEventType(live[0].Event.Type) {
		t.Fatalf("import.completed: %+v %+v", live[0].Event, done)
	}
}

// ACT-INA-03：已过期的截止照常触发一次，跟进从导入时刻起算。
func TestImportedDeadlineAndFollowUpStartAtImportTime(t *testing.T) {
	e := setup(t)
	e.existingProject("acc-home")
	r := e.must(web, "acc-imp", false, plan()...)
	two := r.Results[3].Object
	if len(e.waiting) != 1 || e.waiting[0] != two {
		t.Fatalf("only the waiting item arms a follow-up from the import time: %v", e.waiting)
	}
	tm, ok := view2(t, e, func(tx *store.Tx) (domain.Timer, bool, error) { return clock.Get(tx, "due_"+two) })
	if !ok || tm.Kind != domain.TimerDue || !tm.At.Equal(mustTime("2026-11-16T03:00:00Z")) {
		t.Fatalf("deadline timer: %+v %v", tm, ok)
	}
	one := r.Results[2].Object // done：不计时
	if _, ok := view2(t, e, func(tx *store.Tx) (domain.Timer, bool, error) { return clock.Get(tx, "due_"+one) }); ok {
		t.Fatal("a finished item has no deadline timer")
	}
	if n, err := e.clk.Tick(bg); err != nil || n == 0 {
		t.Fatalf("tick: %d %v", n, err)
	}
	if _, err := e.clk.Tick(bg); err != nil {
		t.Fatal(err)
	}
	es := view(t, e, func(tx *store.Tx) ([]eventlog.Entry, error) { return eventlog.ReadObjectLive(tx, two) })
	var passed int
	for _, en := range es {
		if en.Event.Type == domain.EvItemDeadlinePassed {
			passed++
			if en.Event.Imported != "" {
				t.Fatal("item.deadline_passed is a real event, not an imported one")
			}
		}
	}
	if passed != 1 {
		t.Fatalf("deadline_passed fired %d times", passed)
	}
}

func view2[A any, B any](t *testing.T, e *env, f func(tx *store.Tx) (A, B, error)) (A, B) {
	t.Helper()
	var a A
	var b B
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { a, b, err = f(tx); return }); err != nil {
		t.Fatal(err)
	}
	return a, b
}

// ACT-IMP-06：原样重放新增 0 条，已导入的事项不被比较或更新；只多一条 import.completed。
func TestReplayAddsNothing(t *testing.T) {
	e := setup(t)
	e.existingProject("acc-home")
	first := e.must(web, "acc-imp", false, plan()...)
	head := e.head()
	e.clock = e.clock.Add(time.Minute) // 第二个批次晚一分钟开始
	r := e.must(web, "acc-imp", false, plan()...)
	if r.Created != 0 || r.Existing != 4 || r.Invalid != 0 || r.Batch == first.Batch {
		t.Fatalf("replay: %+v", r)
	}
	for i, res := range r.Results {
		if res.Object != first.Results[i].Object {
			t.Errorf("line %d resolves to %q, want %q", i, res.Object, first.Results[i].Object)
		}
	}
	if got := e.head() - head; got != 1 {
		t.Fatalf("replay wrote %d events, want only import.completed", got)
	}
	bs := view(t, e, Batches)
	if len(bs) != 2 || bs[0].Created != 0 || bs[0].Existing != 4 {
		t.Fatalf("batches: %+v", bs)
	}
	// 同一文件里重复的外部 id 也只写一次。
	dup := e.must(web, "dup", false, itemTwo2("D-1"), itemTwo2("D-1"))
	if dup.Created != 1 || dup.Existing != 1 || dup.Results[1].Object != dup.Results[0].Object {
		t.Fatalf("duplicate in one file: %+v", dup)
	}
}

func itemTwo2(ext string) string {
	return `{"type":"item","external_id":"` + ext + `","item":{"kind":"task","title":"x"},"history":[{"at":"2026-10-10T09:00:00+08:00","event":"created","phase":"open"}]}`
}

// ACT-IMP-07：每行各犯一项规则的记录逐行 invalid，且一行不写任何事件；合法行不受连累。
func TestInvalidRowsAreSkippedOneByOne(t *testing.T) {
	e := setup(t)
	mk := func(item, history string) string {
		return `{"type":"item","external_id":"X","item":` + item + `,"history":[` + history + `]}`
	}
	created := `{"at":"2026-10-10T09:00:00+08:00","event":"created","phase":"open"}`
	okItem := `{"kind":"task","title":"t"}`
	var long []string
	for i := 0; i < 501; i++ {
		long = append(long, fmt.Sprintf(`{"at":"2026-10-10T09:%02d:00+08:00","event":"progress","on":"2026-10-10","text":"x"}`, i%60))
	}
	cases := []struct{ rule, line string }{
		{"history_first", mk(okItem, `{"at":"2026-10-10T09:00:00+08:00","event":"progress","on":"2026-10-10","text":"x"}`)},
		{"history_future", mk(okItem, created+`,{"at":"2099-01-01T00:00:00Z","event":"progress","on":"2099-01-01","text":"x"}`)},
		{"progress_on_late", mk(okItem, created+`,{"at":"2026-10-11T09:00:00+08:00","event":"progress","on":"2026-10-12","text":"x"}`)},
		{"kind_not_allowed", mk(`{"kind":"delegation","title":"t"}`, created)},
		{"kind_not_allowed", mk(`{"kind":"brief","title":"t"}`, created)},
		{"needs_input_not_allowed", mk(okItem, created+`,{"at":"2026-10-11T09:00:00+08:00","event":"transition","to":"needs_input","reason":"r"}`)},
		{"waiting_on_missing", mk(okItem, created+`,{"at":"2026-10-11T09:00:00+08:00","event":"transition","to":"waiting","reason":"r"}`)},
		{"reason_missing", mk(okItem, created+`,{"at":"2026-10-11T09:00:00+08:00","event":"transition","to":"dropped"}`)},
		{"assignee_not_allowed", mk(`{"kind":"task","title":"t","assignee":"agent-1"}`, created)},
		{"history_too_long", mk(okItem, created+","+strings.Join(long, ","))},
	}
	var ls []string
	for i, c := range cases {
		ls = append(ls, strings.Replace(c.line, `"external_id":"X"`, fmt.Sprintf(`"external_id":"X%d"`, i), 1))
	}
	ls = append(ls, itemTwo2("OK"), `not json`)
	head := e.head()
	r := e.must(web, "acc-imp2", false, ls...)
	if r.Created != 1 || r.Invalid != len(cases)+1 {
		t.Fatalf("counts: %+v", r.Counts)
	}
	for i, c := range cases {
		got := r.Results[i]
		if got.Outcome != OutcomeInvalid || got.Code != c.rule || got.Why == "" {
			t.Errorf("line %d: want invalid/%s, got %+v", i+1, c.rule, got)
		}
	}
	if last := r.Results[len(r.Results)-1]; last.Outcome != OutcomeInvalid || last.Code != "bad_json" {
		t.Errorf("non-JSON line: %+v", last)
	}
	if r.Results[len(cases)].Outcome != OutcomeCreated {
		t.Errorf("the valid row must be created: %+v", r.Results[len(cases)])
	}
	// 只写了合法那一条（一条 item.created）与 import.completed。
	if got := e.head() - head; got != 2 {
		t.Fatalf("invalid rows wrote events: %d", got)
	}
	if bs := view(t, e, Batches); bs[0].Created != 1 || bs[0].Invalid != len(cases)+1 {
		t.Fatalf("batch: %+v", bs)
	}
	// 非法来源名整个请求被拒，不写账本。
	for _, src := range []string{"Acc_Imp", strings.Repeat("a", 33), ""} {
		head = e.head()
		_, err := e.run(web, src, false, itemTwo2("S"))
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest || e.head() != head {
			t.Errorf("source %q: %v", src, err)
		}
	}
}

// 事项自身的校验同普通写入：标题必填、引用的项目必须存在（同来源的项目记录或已有项目 id）。
func TestItemRulesOfOrdinaryWritesApply(t *testing.T) {
	e := setup(t)
	noTitle := `{"type":"item","external_id":"A","item":{"kind":"task","title":" "},"history":[{"at":"2026-10-10T09:00:00+08:00","event":"created","phase":"open"}]}`
	noProj := `{"type":"item","external_id":"B","project":"nope","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00+08:00","event":"created","phase":"open"}]}`
	r := e.must(web, "acc-imp", false, noTitle, noProj)
	if r.Invalid != 2 || r.Results[1].Code != "project_unknown" || r.Results[0].Why == "" {
		t.Fatalf("%+v", r.Results)
	}
	// 引用已有项目的 id 与同来源项目记录的外部 id 都行；项目记录排在事项之后则事项不合法。
	pid := e.existingProject("P")
	byID := strings.Replace(itemTwo2("C"), `"item":`, `"project":"`+pid+`","item":`, 1)
	late := strings.Replace(itemTwo2("D"), `"item":`, `"project":"later","item":`, 1)
	r = e.must(web, "acc-imp", false, byID, late, `{"type":"project","external_id":"later","name":"Later"}`)
	if r.Results[0].Outcome != OutcomeCreated || r.Results[1].Code != "project_unknown" || r.Results[2].Outcome != OutcomeCreated {
		t.Fatalf("%+v", r.Results)
	}
}

// ACT-IMP-08：同一批次号沿用；批次不属于该来源名或其他调用者时被拒，不写账本。
func TestBatchIsSharedBySameSourceAndCaller(t *testing.T) {
	e := setup(t)
	first := e.must(web, "acc-imp", false, itemTwo2("A"))
	run := func(source, batch string, ls ...string) (*Result, error) {
		return e.svc.Run(bg, Params{Identity: web, Source: source, Batch: batch, Lines: lines(ls...)})
	}
	second, err := run("acc-imp", first.Batch, itemTwo2("B"))
	if err != nil || second.Batch != first.Batch {
		t.Fatalf("second request: %+v %v", second, err)
	}
	bs := view(t, e, Batches)
	if len(bs) != 1 || bs[0].Created != 2 {
		t.Fatalf("one batch with both requests: %+v", bs)
	}
	head := e.head()
	for name, c := range map[string]struct{ source, batch string }{"other source": {"other", first.Batch}, "unknown batch": {"acc-imp", "imp_deadbeef"}} {
		if _, err := run(c.source, c.batch, itemTwo2("Z")); err == nil || e.head() != head {
			t.Errorf("%s: err %v, head moved %v", name, err, e.head() != head)
		}
	}
	if n := len(view(t, e, func(tx *store.Tx) ([]domain.Item, error) {
		return items.List(tx, items.Filter{OriginSource: "import:other"})
	})); n != 0 {
		t.Fatalf("rejected request wrote %d items", n)
	}
}

// ACT-IMP-09：每条记录各一个事务，已写入的各自完整；重跑已写入的为 existing、其余 created，总数不变。
func TestResumeFromWhereItStopped(t *testing.T) {
	e := setup(t)
	var all []string
	for i := 0; i < 30; i++ {
		all = append(all, fmt.Sprintf(`{"type":"item","external_id":"R-%03d","item":{"kind":"task","title":"t%d"},"history":[
{"at":"2026-10-10T09:00:00+08:00","event":"created","phase":"open"},{"at":"2026-10-11T09:00:00+08:00","event":"progress","on":"2026-10-11","text":"x"}]}`, i, i))
	}
	for i := range all {
		all[i] = strings.ReplaceAll(all[i], "\n", "")
	}
	e.must(web, "resume", false, all[:12]...) // 服务在第 12 条之后停掉
	r := e.must(web, "resume", false, all...)
	if r.Existing != 12 || r.Created != 18 {
		t.Fatalf("rerun: %+v", r.Counts)
	}
	got := view(t, e, func(tx *store.Tx) ([]domain.Item, error) {
		return items.List(tx, items.Filter{OriginSource: "import:resume"})
	})
	if len(got) != 30 {
		t.Fatalf("%d items, want 30 with no duplicates", len(got))
	}
	for _, it := range got {
		es := view(t, e, func(tx *store.Tx) ([]eventlog.Entry, error) { return eventlog.ReadObject(tx, it.ID) })
		if len(es) != 2 || it.Version != 2 {
			t.Fatalf("%s has %d events, version %d: a record is written whole or not at all", it.ID, len(es), it.Version)
		}
	}
}

// 物理删除事项会清掉对照，之后可以重导。
func TestPurgedItemCanBeImportedAgain(t *testing.T) {
	e := setup(t)
	first := e.must(web, "acc-imp", false, itemTwo2("A"))
	if err := e.st.Update(bg, func(tx *store.Tx) error { return items.Purge(tx, first.Results[0].Object) }); err != nil {
		t.Fatal(err)
	}
	if r := e.must(web, "acc-imp", false, itemTwo2("A")); r.Created != 1 || r.Results[0].Object == first.Results[0].Object {
		t.Fatalf("reimport after purge: %+v", r)
	}
}

// ---- 授权单（flow-import-grant） ----

type dlg struct {
	id command.Identity
	cr auth.Credential
}

func (e *env) delegate() dlg {
	e.t.Helper()
	var d dlg
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		d.cr, secret, err = e.auth.Issue(tx, auth.Spec{Kind: auth.Delegate, Label: "d", Harness: "hermes"})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	d.id = command.Bearer{Auth: e.auth, Secret: secret, Origin: auth.Origin{Harness: "hermes", Session: "s1", SessionKind: "direct"}, OnBehalfOf: "owner"}
	return d
}

func (e *env) grant(d dlg, source string, max int) string {
	e.t.Helper()
	var g policy.Grant
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		g, err = policy.IssueGrant(tx, e.auth, web.Actor, e.clock, policy.GrantInput{Grantee: policy.Grantee{Credential: d.cr.ID}, Scope: policy.Scope{All: true},
			Actions: []string{policy.ImportApply}, Import: &policy.ImportScope{Source: source, MaxRecords: max}, ExpiresAt: e.clock.Add(24 * time.Hour)})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return g.ID
}

func code(err error) (domain.Code, int) {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code, de.ExitCode()
	}
	return "", -1
}

func (e *env) runAs(d dlg, grant, evidence, source string, dry bool, ls ...string) (*Result, error) {
	return e.svc.Run(bg, Params{Identity: d.id, Source: source, DryRun: dry, Lines: lines(ls...), Grant: grant, Evidence: evidence})
}

// ACT-IGR-02：不带单与只带原话都是 23 grant_required，账本无新增；会话类型不改变结果。
func TestDelegateNeedsAGrantNotWords(t *testing.T) {
	e := setup(t)
	d := e.delegate()
	for _, kind := range []string{"direct", "group"} {
		d.id = command.Bearer{Auth: e.auth, Secret: secretOf(t, e, d), Origin: auth.Origin{Harness: "hermes", Session: "s1", SessionKind: kind}, OnBehalfOf: "owner"}
		for _, ev := range []string{"", "我说导入"} {
			before := len(view(t, e, func(tx *store.Tx) ([]domain.Item, error) { return items.List(tx, items.Filter{}) }))
			for _, dry := range []bool{false, true} {
				_, err := e.runAs(d, "", ev, "acc-imp3", dry, itemTwo2("A"), itemTwo2("B"))
				if c, exit := code(err); c != domain.CodeGrantRequired || exit != 23 {
					t.Fatalf("session %s evidence %q dry %v: %v", kind, ev, dry, err)
				}
			}
			if n := len(view(t, e, func(tx *store.Tx) ([]domain.Item, error) { return items.List(tx, items.Filter{}) })); n != before {
				t.Fatal("a refused import wrote items")
			}
		}
	}
}

func secretOf(t *testing.T, e *env, d dlg) string {
	t.Helper()
	return d.id.(command.Bearer).Secret
}

// ACT-IGR-03/04/05：凭单导入成功，事件带授权单 id，批次带授权单；来源名不符与累计超限整个请求被拒；预检不占条数。
func TestGrantImport(t *testing.T) {
	e := setup(t)
	d := e.delegate()
	g := e.grant(d, "acc-imp3", 3)
	r, err := e.runAs(d, g, "", "acc-imp3", false, itemTwo2("A"), itemTwo2("B"))
	if err != nil || r.Created != 2 {
		t.Fatalf("grant import: %+v %v", r, err)
	}
	es := view(t, e, func(tx *store.Tx) ([]eventlog.Entry, error) {
		return eventlog.Query(tx, eventlog.Filter{Imported: r.Batch}, 0, 100)
	})
	if len(es) != 2 {
		t.Fatalf("imported events: %d", len(es))
	}
	for _, en := range es {
		if en.Event.Grant != g || en.Event.Actor.Principal != d.cr.ID && en.Event.Actor.Principal == "" {
			t.Errorf("event %s: grant %q actor %+v", en.Event.Type, en.Event.Grant, en.Event.Actor)
		}
	}
	if bs := view(t, e, Batches); bs[0].Grant != g {
		t.Fatalf("batch grant: %+v", bs[0])
	}
	// 来源名不符：23 import_source_mismatch；累计超限：23 import_limit_exceeded；两者都不写账本。
	head := e.head()
	_, err = e.runAs(d, g, "", "acc-other", false, itemTwo2("O"))
	if c, exit := code(err); c != domain.CodeImportSourceMismatch || exit != 23 {
		t.Fatalf("source mismatch: %v", err)
	}
	_, err = e.runAs(d, g, "", "acc-imp3", false, itemTwo2("C"), itemTwo2("D"))
	if c, exit := code(err); c != domain.CodeImportLimitExceeded || exit != 23 {
		t.Fatalf("over the limit: %v", err)
	}
	if e.head() != head {
		t.Fatal("a refused request must not write anything, not even its first record")
	}
	// 预检三次不占条数，真实导入 1 条成功（累计 3）。
	for i := 0; i < 3; i++ {
		if r, err := e.runAs(d, g, "", "acc-imp3", true, itemTwo2("E")); err != nil || r.Created != 1 {
			t.Fatalf("dry run %d: %+v %v", i, r, err)
		}
	}
	if used := view(t, e, func(tx *store.Tx) (int, error) { return policy.ImportUsed(tx, g) }); used != 2 {
		t.Fatalf("dry runs consumed the grant: used %d", used)
	}
	if r, err := e.runAs(d, g, "", "acc-imp3", false, itemTwo2("E")); err != nil || r.Created != 1 {
		t.Fatalf("last record: %+v %v", r, err)
	}
	// 已有的记录不占条数：重放整份仍然可以。
	if r, err := e.runAs(d, g, "", "acc-imp3", false, itemTwo2("A"), itemTwo2("B"), itemTwo2("E")); err != nil || r.Existing != 3 {
		t.Fatalf("replay with an exhausted grant: %+v %v", r, err)
	}
	// 凭单也不能同时带原话。
	if _, err := e.runAs(d, g, "words", "acc-imp3", false, itemTwo2("F")); err == nil {
		t.Fatal("grant and words together must be refused")
	}
}

// ACT-IGR-06：运行凭据不能导入（退出码 4）；观察者、worker 同样不行。
func TestRunCredentialsAndLowLevelsCannotImport(t *testing.T) {
	e := setup(t)
	run := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run1", Via: domain.ViaCLI, Harness: "hermes"}, Level: authz.LevelRun, RunHarness: "hermes",
		RunTrusted: func(*store.Tx) (bool, error) { return true, nil }}
	for name, id := range map[string]command.Trusted{"run": run,
		"observer": {Actor: domain.Actor{Type: domain.ActorAgent, Principal: "o", Via: domain.ViaCLI}, Level: authz.LevelObserver},
		"worker":   {Actor: domain.Actor{Type: domain.ActorAgent, Principal: "w", Via: domain.ViaCLI}, Level: authz.LevelWorker}} {
		_, err := e.svc.Run(bg, Params{Identity: id, Source: "acc-imp3", Lines: lines(itemTwo2("A")), Grant: "grt_x"})
		if c, exit := code(err); c != domain.CodePermissionDenied || exit != 4 {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// 幂等键：同一键重放得到同一批次与同一结果，不重复写入。
func TestIdempotencyKeyReplaysTheSameBatch(t *testing.T) {
	e := setup(t)
	p := Params{Identity: web, Source: "acc-imp", Lines: lines(itemTwo2("A")), IdemKey: "k1"}
	a, err := e.svc.Run(bg, p)
	if err != nil {
		t.Fatal(err)
	}
	head := e.head()
	b, err := e.svc.Run(bg, p)
	if err != nil || b.Batch != a.Batch || b.Results[0].Object != a.Results[0].Object || e.head() != head {
		t.Fatalf("replay: %+v %v head moved %v", b, err, e.head() != head)
	}
}

func TestCompletedModeIsNotARecord(t *testing.T) {
	e := setup(t)
	for _, in := range []string{`{"source":"acc-imp"}`, `{"source":"acc-imp","record":{},"completed":{}}`, `{"source":"BAD","completed":{}}`, `{"source":"acc-imp","completed":{"created":-1}}`, `{"source":"x","bogus":1}`} {
		if _, err := e.x.Do(bg, command.Request{Identity: web, Command: Command, Input: json.RawMessage(in)}); err == nil {
			t.Errorf("%s must be refused", in)
		}
	}
}

// 导入的事项可以只靠事件重建：Rebuild 之后导入信息、进展的 precision 与时间线都在，投影仍与折叠一致。
func TestRebuildKeepsImportedProjection(t *testing.T) {
	e := setup(t)
	e.existingProject("acc-home")
	r := e.must(web, "acc-imp", false, plan()...)
	one := r.Results[2].Object
	before := view(t, e, func(tx *store.Tx) (domain.Item, error) { return items.Get(tx, one) })
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := items.Rebuild(tx); err != nil {
			return err
		}
		return items.Verify(tx)
	}); err != nil {
		t.Fatal(err)
	}
	after := view(t, e, func(tx *store.Tx) (domain.Item, error) { return items.Get(tx, one) })
	b1, _ := json.Marshal(before)
	b2, _ := json.Marshal(after)
	if string(b1) != string(b2) || after.Imported == nil {
		t.Fatalf("rebuild changed the item:\n%s\n%s", b1, b2)
	}
	if prog := view(t, e, func(tx *store.Tx) ([]domain.Progress, error) { return items.Progress(tx, one) }); len(prog) != 5 || prog[0].Precision != "day" {
		t.Fatalf("progress after rebuild: %+v", prog)
	}
}

// 项目记录与记录外形的规则：外形不对的行逐行 invalid，合法行不受连累；已有同名项目大小写与空白不敏感。
func TestProjectAndShapeRules(t *testing.T) {
	e := setup(t)
	cases := []struct{ rule, line string }{
		{"bad_type", `{"type":"material","external_id":"m"}`},
		{"bad_external_id", `{"type":"project","external_id":" ","name":"P"}`},
		{"bad_external_id", `{"type":"project","name":"P"}`},
		{"name_missing", `{"type":"project","external_id":"p1","name":" "}`},
		{"bad_status", `{"type":"project","external_id":"p2","name":"P2","status":"gone"}`},
		{"bad_tags", `{"type":"project","external_id":"p3","name":"P3","tags":[" "]}`},
		{"bad_created_at", `{"type":"project","external_id":"p4","name":"P4","created_at":"yesterday"}`},
		{"history_future", `{"type":"project","external_id":"p5","name":"P5","created_at":"2099-01-01T00:00:00Z"}`},
		{"bad_record", `{"type":"project","external_id":"p6","name":"P6","allowed_executors":[]}`},
		{"bad_record", `{"type":"item","external_id":"i1","item":{"kind":"task","title":"t","origin":{"source":"x"}},"history":[]}`},
		{"history_first", `{"type":"item","external_id":"i2","item":{"kind":"task","title":"t"},"history":[]}`},
		{"bad_event", `{"type":"item","external_id":"i3","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00Z","event":"created","phase":"open"},{"at":"2026-10-11T09:00:00Z","event":"edit"}]}`},
		{"history_created_twice", `{"type":"item","external_id":"i4","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00Z","event":"created","phase":"open"},{"at":"2026-10-11T09:00:00Z","event":"created","phase":"open"}]}`},
		{"bad_precision", `{"type":"item","external_id":"i5","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00Z","event":"created","phase":"open","precision":"hour"}]}`},
		{"history_at", `{"type":"item","external_id":"i6","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10","event":"created","phase":"open"}]}`},
		{"bad_phase", `{"type":"item","external_id":"i7","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00Z","event":"created","phase":"proposed"}]}`},
		{"progress_text", `{"type":"item","external_id":"i8","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00Z","event":"created","phase":"open"},{"at":"2026-10-10T10:00:00Z","event":"progress","on":"2026-10-10","text":" "}]}`},
		{"progress_on", `{"type":"item","external_id":"i9","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00Z","event":"created","phase":"open"},{"at":"2026-10-10T10:00:00Z","event":"progress","text":"x"}]}`},
		{"history_first", `{"type":"item","external_id":"i10","item":{"kind":"task","title":"t"},"history":[{"at":"2026-10-10T09:00:00Z","event":"created","phase":"open"},{"at":"2026-10-09T10:00:00Z","event":"progress","on":"2026-10-09","text":"x"}]}`},
	}
	var ls []string
	for _, c := range cases {
		ls = append(ls, c.line)
	}
	home := e.existingProject("acc-home")
	ls = append(ls, `{"type":"project","external_id":"ok","name":"Fine","intro":"i","tags":["a"],"status":"paused"}`, `{"type":"project","external_id":"home","name":"ACC-Home"}`)
	r := e.must(web, "shapes", false, ls...)
	for i, c := range cases {
		if got := r.Results[i]; got.Outcome != OutcomeInvalid || got.Code != c.rule {
			t.Errorf("case %d: want invalid/%s, got %+v", i, c.rule, got)
		}
	}
	n := len(cases)
	if r.Results[n].Outcome != OutcomeCreated || r.Results[n+1].Outcome != OutcomeExisting || r.Results[n+1].Object != home {
		t.Fatalf("valid projects: %+v", r.Results[n:])
	}
	p := view(t, e, func(tx *store.Tx) (domain.Project, error) { return projects.Get(tx, r.Results[n].Object) })
	if p.Status != domain.ProjectPaused || p.Intro != "i" || len(p.Tags) != 1 {
		t.Fatalf("project: %+v", p)
	}
}

// W26：时段是事项结构里的普通字段，导入通道照收；重放不产生重复，有时段没日期的整行不合法。
func TestImportCarriesPlannedSlot(t *testing.T) {
	e := setup(t)
	rec := func(ext, item string) string {
		return `{"type":"item","external_id":"` + ext + `","item":` + item + `,"history":[{"at":"2026-10-10T09:00:00+08:00","event":"created","phase":"open"}]}`
	}
	slotted := rec("S-1", `{"kind":"task","title":"会","planned_for":"2026-10-20","planned_slot":"14:00 - 16:00"}`)
	noDate := rec("S-2", `{"kind":"task","title":"会","planned_slot":"14:00 - 16:00"}`)
	r := e.must(web, "wb", false, slotted, noDate)
	if r.Results[0].Outcome != OutcomeCreated || r.Results[1].Outcome != OutcomeInvalid {
		t.Fatalf("%+v", r.Results)
	}
	it := view(t, e, func(tx *store.Tx) (domain.Item, error) { return items.Get(tx, r.Results[0].Object) })
	if it.PlannedFor != "2026-10-20" || it.PlannedSlot != "14:00 - 16:00" {
		t.Fatalf("imported item: %+v", it)
	}
	if again := e.must(web, "wb", false, slotted); again.Created != 0 || again.Existing != 1 {
		t.Fatalf("replay: %+v", again.Counts)
	}
}
