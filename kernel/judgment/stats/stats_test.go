package stats

import (
	"context"
	"encoding/json"
	"math"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
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

type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	now time.Time
}

func f64(v float64) *float64 { return &v }

func newEnv(t *testing.T) *env {
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
	e := &env{t: t, st: st, now: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)}
	e.x = command.New(st, func() time.Time { return e.now })
	if err := items.Register(e.x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	if err := inbound.RegisterAttach(e.x); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(cmd string, in any) json.RawMessage {
	e.t.Helper()
	b, _ := json.Marshal(in)
	res, err := e.x.Do(bg, command.Request{Identity: web, Command: cmd, Input: b})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return res.Value
}

func (e *env) item(phase string) string {
	var it struct{ ID string }
	_ = json.Unmarshal(e.do("item.create", map[string]any{"kind": "task", "title": "t", "phase": phase}), &it)
	return it.ID
}

// seed 写一条来源事件（可挂到事项 item）和它的判断记录。
func (e *env) seed(id, item string, levels []record.Level, by, answer string, mod func(*record.Record)) {
	e.t.Helper()
	r := record.Record{ID: "jd_" + id, Use: "inbound_screen", Template: "triage-screen", Subject: "e_" + id, Source: "src_1", Fingerprint: "f" + id,
		Band: record.Band{Low: 0.15, High: 0.6}, DecidedBy: by, Answer: answer, LowConfidence: by == record.DecidedDefault, CreatedAt: e.now, Levels: levels}
	if mod != nil {
		mod(&r)
	}
	e.now = e.now.Add(time.Second)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, domain.Source{ID: "src_1"}, domain.SourceEvent{ID: "e_" + id, Type: "n", Subject: id, Summary: "s", OccurredAt: e.now}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, "src_1")
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.DedupeKey.EventID == "e_"+id {
				r.Subject = ev.ID
				disp := domain.DispositionIgnored
				if item != "" {
					disp = domain.DispositionProposed
				}
				if err := runner.SetDisposition(tx, ev.ID, disp, item, ""); err != nil {
					return err
				}
			}
		}
		return record.Put(tx, r)
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) stats(f Filter) Result {
	e.t.Helper()
	var res Result
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { res, err = Compute(tx, f); return }); err != nil {
		e.t.Fatal(err)
	}
	return res
}

func cls(p float64, o string) record.Level {
	return record.Level{Backend: record.BackendClassifier, PYes: f64(p), Outcome: o, DurationMs: 3}
}
func chat(o string) record.Level {
	return record.Level{Backend: record.BackendChat, Answer: "yes", Outcome: o, DurationMs: 3}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// seedJRC 是 ACT-JRC-02 的四条：HIGH、LOW 分类端点定案，MID 交对话端点定案，UND 两级都不定案。
func (e *env) seedJRC() (high, mid, und string) {
	high, mid, und = e.item("proposed"), e.item("proposed"), e.item("proposed")
	e.seed("high", high, []record.Level{cls(0.9, record.OutcomeDecided)}, record.DecidedClassifier, "yes", nil)
	e.seed("low", "", []record.Level{cls(0.05, record.OutcomeDecided)}, record.DecidedClassifier, "no", nil)
	e.seed("mid", mid, []record.Level{cls(0.4, record.OutcomeUncertain), chat(record.OutcomeDecided)}, record.DecidedChat, "yes", nil)
	e.seed("und", und, []record.Level{cls(0.4, record.OutcomeUncertain), chat(record.OutcomeUncertain)}, record.DecidedDefault, "yes", nil)
	return
}

// ACT-JRC-08：按用途、模板、来源、后端计数，给出升级率与未定率。
func TestCountsByBackendAndEscalationUndecidedRates(t *testing.T) {
	e := newEnv(t)
	e.seedJRC()
	res := e.stats(Filter{Use: "inbound_screen", Template: "triage-screen", Source: "src_1"})
	want := map[string][3]int{record.BackendClassifier: {4, 2, 2}, record.BackendChat: {2, 1, 1}}
	if len(res.Rows) != 2 {
		t.Fatalf("rows: %+v", res.Rows)
	}
	for _, r := range res.Rows {
		w := want[r.Backend]
		if r.Calls != w[0] || r.Decided != w[1] || r.Uncertain != w[2] || r.Errors != 0 || r.Use != "inbound_screen" || r.Source != "src_1" {
			t.Fatalf("row %+v want calls/decided/uncertain %v", r, w)
		}
	}
	if len(res.Rates) != 1 || !near(res.Rates[0].Escalation, 0.5) || !near(res.Rates[0].Undecided, 0.25) {
		t.Fatalf("rates: %+v", res.Rates)
	}
	// 没有费用类字段。
	b, _ := json.Marshal(res)
	for _, bad := range []string{"cost", "price", "quota", "amount"} {
		if json.Unmarshal(b, new(map[string]any)) != nil || contains(string(b), bad) {
			t.Fatalf("stats must not carry %s: %s", bad, b)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// ACT-JRC-09：接受率把接受与补到已有事项都算作接受，丢弃算分母；还在等待的不进分母。
func TestAcceptanceCountsAcceptAndAttachButNotDismiss(t *testing.T) {
	e := newEnv(t)
	high, mid, und := e.seedJRC()
	f := Filter{Use: "inbound_screen", Template: "triage-screen", Source: "src_1"}
	if r := e.stats(f).Rates[0]; r.Acceptance != 0 {
		t.Fatalf("nothing resolved yet: %+v", r)
	}
	e.do("proposal.accept", map[string]any{"id": high})
	if r := e.stats(f).Rates[0]; !near(r.Acceptance, 1) {
		t.Fatalf("one accepted of one resolved: %+v", r)
	}
	e.do("proposal.dismiss", map[string]any{"id": mid})
	target := e.item("open")
	e.do(inbound.CmdAttach, map[string]any{"id": und, "to": target})
	r := e.stats(f).Rates[0]
	if !near(r.Acceptance, 2.0/3) {
		t.Fatalf("accept + attach of three: %+v", r)
	}
}

// ACT-JRC-18/19：误丢率 = 被判"该看"的抽检条数 / 已抽检条数。
func TestFalseDropRateFromSpotCheck(t *testing.T) {
	e := newEnv(t)
	rev := func(res string) func(*record.Record) {
		return func(r *record.Record) { r.Review = &record.Review{Result: res, At: e.now} }
	}
	e.seed("a", "", []record.Level{cls(0.05, record.OutcomeDecided)}, record.DecidedClassifier, "no", rev("should_see"))
	if r := e.stats(Filter{}).Rates[0]; r.Reviewed != 1 || !near(r.FalseDrop, 1) {
		t.Fatalf("%+v", r)
	}
	e.seed("b", "", []record.Level{cls(0.05, record.OutcomeDecided)}, record.DecidedClassifier, "no", rev("correct"))
	e.seed("c", "", []record.Level{cls(0.05, record.OutcomeDecided)}, record.DecidedClassifier, "no", nil)
	if r := e.stats(Filter{}).Rates[0]; r.Reviewed != 2 || !near(r.FalseDrop, 0.5) {
		t.Fatalf("%+v", r)
	}
}

// 复用与跳过不计调用；报错单独计；影子记录计调用但不进两项比率；窗口与分组生效。
func TestReusedSkippedErrorsShadowAndWindow(t *testing.T) {
	e := newEnv(t)
	old := e.now.Add(-100 * 24 * time.Hour)
	e.seed("old", "", []record.Level{cls(0.9, record.OutcomeDecided)}, record.DecidedClassifier, "yes", func(r *record.Record) { r.CreatedAt = old })
	reused := cls(0.9, record.OutcomeDecided)
	reused.Reused = true
	e.seed("re", "", []record.Level{reused}, record.DecidedClassifier, "yes", nil)
	e.seed("err", "", []record.Level{cls(0, record.OutcomeError), {Backend: record.BackendChat, Outcome: record.OutcomeSkipped}}, record.DecidedDefault, "yes", nil)
	e.seed("sh", "", []record.Level{cls(0.4, record.OutcomeUncertain)}, record.DecidedDefault, "yes", func(r *record.Record) { r.Shadow, r.DecidedBy, r.LowConfidence = true, record.DecidedClassifier, false })
	e.seed("other", "", []record.Level{cls(0.9, record.OutcomeDecided)}, record.DecidedClassifier, "yes", func(r *record.Record) { r.Template = "other" })

	res := e.stats(Filter{Template: "triage-screen", Since: e.now.Add(-30 * 24 * time.Hour)})
	if len(res.Rows) != 1 {
		t.Fatalf("rows: %+v", res.Rows)
	}
	if r := res.Rows[0]; r.Calls != 2 || r.Errors != 1 || r.Uncertain != 1 || r.Decided != 0 || r.Reused != 1 {
		t.Fatalf("%+v", r)
	}
	// 非影子记录两条（re、err）：未定 1/2；分类端点答过的题 0（shadow 不算，err 报错不算，re 复用不算）。
	if r := res.Rates[0]; !near(r.Undecided, 0.5) || r.Escalation != 0 {
		t.Fatalf("%+v", r)
	}
	if n := len(e.stats(Filter{}).Rates); n != 2 {
		t.Fatalf("grouped by template: %d", n)
	}
}

func TestHistogramBucketsClassifierProbabilities(t *testing.T) {
	e := newEnv(t)
	for i, p := range []float64{0.05, 0.07, 0.95, 1.0} {
		e.seed(string(rune('a'+i)), "", []record.Level{cls(p, record.OutcomeDecided)}, record.DecidedClassifier, "yes", nil)
	}
	h := e.stats(Filter{}).Histogram
	if len(h) != 2 || h[0].Low != 0 || h[0].Count != 2 || h[1].Low != 0.9 || h[1].Count != 2 {
		t.Fatalf("%+v", h)
	}
}

func TestEmptyLedgerGivesEmptyArrays(t *testing.T) {
	b, _ := json.Marshal(newEnv(t).stats(Filter{}))
	if string(b) != `{"rows":[],"rates":[],"histogram":[]}` {
		t.Fatal(string(b))
	}
}

func TestParseSince(t *testing.T) {
	now := time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)
	for in, want := range map[string]time.Time{"": now.Add(-DefaultWindow), "7d": now.Add(-7 * 24 * time.Hour), "36h": now.Add(-36 * time.Hour),
		"2026-10-01T00:00:00Z": time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)} {
		if got, err := ParseSince(in, now); err != nil || !got.Equal(want) {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	for _, bad := range []string{"x", "-1d", "0d", "7"} {
		if _, err := ParseSince(bad, now); err == nil {
			t.Fatalf("%q must be rejected", bad)
		}
	}
}

// 记录里的模板是 <包>/<模板>@<hash>；按模板名筛选必须命中，完整名也要命中，同名前缀不能误中。
func TestTemplateNameFilterMatchesPackQualified(t *testing.T) {
	e := newEnv(t)
	q := func(r *record.Record) { r.Template = "inbound-triage/triage-screen@abc123" }
	e.seed("q1", "", []record.Level{cls(0.9, record.OutcomeDecided)}, record.DecidedClassifier, "yes", q)
	e.seed("q2", "", []record.Level{cls(0.4, record.OutcomeUncertain)}, record.DecidedDefault, "yes", func(r *record.Record) { r.Template = "inbound-triage/triage-screen-2@abc123" })
	for _, tpl := range []string{"triage-screen", "inbound-triage/triage-screen@abc123"} {
		res := e.stats(Filter{Template: tpl})
		if len(res.Rows) != 1 || res.Rows[0].Calls != 1 || res.Rows[0].Decided != 1 || len(res.Rates) != 1 || len(res.Histogram) != 1 {
			t.Fatalf("template=%q: %+v", tpl, res)
		}
	}
}
