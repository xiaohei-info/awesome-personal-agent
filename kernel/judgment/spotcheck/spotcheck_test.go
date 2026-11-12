package spotcheck

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

var web = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
var observer = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "obs", Via: domain.ViaCLI}, Level: authz.LevelObserver}

// drafter 是入站起草的替身：用真实的 item.create 建 proposed 事项，记录被调用次数。
type drafter struct {
	x     *command.Executor
	calls int
}

func (d *drafter) DraftProposal(ctx context.Context, id command.Identity, event string) (string, error) {
	d.calls++
	in, _ := json.Marshal(map[string]any{"kind": "task", "title": "from " + event, "phase": "proposed"})
	res, err := d.x.Do(ctx, command.Request{Identity: id, Command: "item.create", Input: in})
	if err != nil {
		return "", err
	}
	var it struct{ ID string }
	_ = json.Unmarshal(res.Value, &it)
	return it.ID, nil
}

type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	now time.Time
	d   *drafter
	svc *Service
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, runner.Module, proc.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)}
	e.x = command.New(st, func() time.Time { return e.now })
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.d = &drafter{x: e.x}
	e.svc = &Service{Store: st, Exec: e.x, Proposer: e.d}
	return e
}

func f64(v float64) *float64 { return &v }

// judged 造一条来源事件及其初筛判断记录，并按 disposition 设置事件处置；返回 (事件 id, 判断 id)。
func (e *env) judged(n int, mut func(*record.Record), disp domain.Disposition) (string, string) {
	e.t.Helper()
	var evID string
	jd := fmt.Sprintf("jd_%02d", n)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		sid := fmt.Sprintf("ev-%02d", n)
		if err := runner.AppendInternal(tx, domain.Source{ID: "src_1"}, domain.SourceEvent{ID: sid, Type: "n", Subject: "subject " + sid, Summary: "SECRET BODY", Link: "https://x.example/" + sid, OccurredAt: e.now}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, "src_1")
		if err != nil {
			return err
		}
		evID = evs[len(evs)-1].ID
		if err := runner.SetDisposition(tx, evID, disp, "", ""); err != nil {
			return err
		}
		r := record.Record{ID: jd, Use: UseInboundScreen, Template: "triage/triage-screen@abc", Subject: evID, Source: "src_1", Fingerprint: "fp" + jd,
			Band: record.Band{Low: 0.15, High: 0.60}, DecidedBy: record.DecidedClassifier, Answer: "no", CreatedAt: e.now.Add(-time.Hour),
			Levels: []record.Level{{Backend: record.BackendClassifier, PYes: f64(0.05), Outcome: record.OutcomeDecided}}}
		if mut != nil {
			mut(&r)
		}
		return record.Put(tx, r)
	}); err != nil {
		e.t.Fatal(err)
	}
	return evID, jd
}

func (e *env) pool(limit int) []Entry {
	e.t.Helper()
	var out []Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = Pool(tx, e.now, limit); return }); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func ids(es []Entry) (out []string) {
	for _, e := range es {
		out = append(out, e.Judgment)
	}
	return
}

func (e *env) event(id string) domain.SourceEvent {
	var ev domain.SourceEvent
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return ev
}

func (e *env) rec(id string) record.Record {
	var r record.Record
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { r, err = record.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return r
}

// ACT-JRC-17：池只含初筛忽略的事件；建议、规则忽略、影子、把握低、已抽检、过窗口、其他用途都不进池；同一批可重复读到。
func TestPoolOnlyHoldsScreeningIgnores(t *testing.T) {
	e := setup(t)
	var want []string
	for n := 1; n <= 6; n++ {
		_, jd := e.judged(n, nil, domain.DispositionIgnored)
		want = append(want, jd)
	}
	e.judged(10, func(r *record.Record) { r.Answer = "yes"; r.Levels[0].PYes = f64(0.9) }, domain.DispositionProposed)        // 建议
	e.judged(11, nil, domain.DispositionProposed)                                                                             // 判断是 no，但事件已不是 ignored
	e.judged(12, func(r *record.Record) { r.Shadow = true }, domain.DispositionIgnored)                                       // 影子只记分
	e.judged(13, func(r *record.Record) { r.Use = "risk_screen" }, domain.DispositionIgnored)                                 // 其他用途
	e.judged(14, func(r *record.Record) { r.CreatedAt = e.now.Add(-Window - time.Hour) }, domain.DispositionIgnored)          // 过了周期
	e.judged(15, func(r *record.Record) { r.Review = &record.Review{Result: Correct, At: e.now} }, domain.DispositionIgnored) // 已抽检
	e.judged(16, func(r *record.Record) {
		r.DecidedBy, r.LowConfidence, r.Levels[0].Outcome = record.DecidedDefault, true, record.OutcomeUncertain
	}, domain.DispositionIgnored)

	got := e.pool(5)
	if len(got) != 5 {
		t.Fatalf("pool = %v, want 5 entries", ids(got))
	}
	in := map[string]bool{}
	for _, id := range want {
		in[id] = true
	}
	for _, en := range got {
		if !in[en.Judgment] {
			t.Fatalf("%s must not be in the pool", en.Judgment)
		}
		if en.Subject == "" || en.Link == "" || en.WebPath == "" || len(en.Results) != 2 {
			t.Fatalf("entry %+v", en)
		}
	}
	if again := e.pool(5); fmt.Sprint(ids(again)) != fmt.Sprint(ids(got)) {
		t.Fatalf("two reads differ: %v vs %v", ids(got), ids(again))
	}
	if len(e.pool(100)) != 6 || len(e.pool(0)) != 0 {
		t.Fatal("limit not honoured")
	}
}

// ACT-JRC-18：点"该看"，事件转建议，判断记录带结论与时间，Event 留痕，池里不再有它。
func TestShouldSeeProposesTheEventAndCountsAsFalseDrop(t *testing.T) {
	e := setup(t)
	evID, jd := e.judged(1, nil, domain.DispositionIgnored)
	got, err := e.svc.Review(bg, web, jd, ShouldSee, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Review == nil || got.Review.Result != ShouldSee || !got.Review.At.Equal(e.now) {
		t.Fatalf("record %+v", got.Review)
	}
	ev := e.event(evID)
	if ev.Disposition != domain.DispositionProposed || ev.Item == "" {
		t.Fatalf("event %+v", ev)
	}
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, ev.Item); return }); err != nil || it.Phase != domain.PhaseProposed {
		t.Fatalf("item %+v %v", it, err)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, evID)
		if err != nil {
			return err
		}
		last := es[len(es)-1].Event
		if last.Type != EvDisposed || last.Actor.Principal != "owner" {
			t.Fatalf("audit event %+v", last)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(e.pool(5)) != 0 {
		t.Fatal("a reviewed entry stays in the pool")
	}
	if _, err := e.svc.Review(bg, web, jd, Correct, ""); code(err) != domain.CodeConflict {
		t.Fatalf("second review: %v", err)
	}
}

// ACT-JRC-19：点"确实不用"只记结论：事件仍是 ignored，没有建议，起草没被调用。
func TestCorrectOnlyRecordsTheVerdict(t *testing.T) {
	e := setup(t)
	evID, jd := e.judged(1, nil, domain.DispositionIgnored)
	got, err := e.svc.Review(bg, web, jd, Correct, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Review == nil || got.Review.Result != Correct || got.Review.At.IsZero() {
		t.Fatalf("record %+v", got.Review)
	}
	if ev := e.event(evID); ev.Disposition != domain.DispositionIgnored || ev.Item != "" || e.d.calls != 0 {
		t.Fatalf("event %+v calls %d", ev, e.d.calls)
	}
}

// 抽检结论只在 Web（§14.8）：非 Web 凭据被拒绝，且不会留下半成品事项。
func TestOnlyTheOwnerOnWebCanReview(t *testing.T) {
	e := setup(t)
	evID, jd := e.judged(1, nil, domain.DispositionIgnored)
	for _, res := range []string{ShouldSee, Correct} {
		if _, err := e.svc.Review(bg, observer, jd, res, ""); err == nil {
			t.Fatalf("%s accepted from an observer", res)
		}
	}
	if e.d.calls != 0 || e.event(evID).Disposition != domain.DispositionIgnored || e.rec(jd).Review != nil {
		t.Fatalf("rejected review left traces: calls %d", e.d.calls)
	}
}

func TestReviewRejectsWhatIsNotInThePool(t *testing.T) {
	e := setup(t)
	_, yes := e.judged(1, func(r *record.Record) { r.Answer = "yes" }, domain.DispositionProposed)
	_, moved := e.judged(2, nil, domain.DispositionProposed)
	_, shadow := e.judged(3, func(r *record.Record) { r.Shadow = true }, domain.DispositionIgnored)
	for _, jd := range []string{yes, moved, shadow} {
		if _, err := e.svc.Review(bg, web, jd, Correct, ""); err == nil {
			t.Fatalf("%s accepted", jd)
		}
	}
	if _, err := e.svc.Review(bg, web, "jd_none", Correct, ""); code(err) != domain.CodeNotFound {
		t.Fatalf("unknown id: %v", err)
	}
	e.judged(4, nil, domain.DispositionIgnored)
	if _, err := e.svc.Review(bg, web, "jd_04", "maybe", ""); code(err) != domain.CodeInvalidRequest {
		t.Fatalf("bad result: %v", err)
	}
}

// ACT-JRC-20：简报带策略 per_brief 条抽检条目，只带 id 与 Web 入口，不带来源正文。
func TestBriefGetsPerBriefEntries(t *testing.T) {
	e := setup(t)
	for n := 1; n <= 8; n++ {
		e.judged(n, nil, domain.DispositionIgnored)
	}
	var got []Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { got, err = ForBrief(tx, e.now); return }); err != nil {
		t.Fatal(err)
	}
	if len(got) != policy.DefaultSpotCheckPerBrief {
		t.Fatalf("default per_brief: %d entries", len(got))
	}
	b, _ := json.Marshal(got)
	if s := string(b); strings.Contains(s, "SECRET BODY") {
		t.Fatalf("entries leak source body: %s", s)
	}
}

func code(err error) domain.Code {
	if de, ok := err.(*domain.Error); ok {
		return de.Code
	}
	return ""
}

// 守住与入站模块的事件类型同值（本包为避免依赖入站包而复制了常量）。
func TestEventTypeMatchesInbound(t *testing.T) {
	if EvDisposed != inbound.EvDisposed {
		t.Fatalf("%q != %q", EvDisposed, inbound.EvDisposed)
	}
}
