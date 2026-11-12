package record

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

func open(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(proc.Module); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func f64(v float64) *float64 { return &v }
func yes() *bool             { b := true; return &b }

var t0 = time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC)

// mid 是 JRC-MID：分类端点落入不确定带，对话端点答是并定案。
func mid(id, subject string, at time.Time) Record {
	return Record{ID: id, Use: "inbound_screen", Template: "triage/triage-screen@abc123", Subject: subject, Source: "src_1",
		Fingerprint: "fp-" + id, Band: Band{Low: 0.15, High: 0.60}, DecidedBy: DecidedChat, Answer: "yes", CreatedAt: at,
		Levels: []Level{
			{Backend: BackendClassifier, Endpoint: "http://127.0.0.1:9001", Locality: "local", Model: "cls-1", PYes: f64(0.30), ProbabilitySource: "native", Outcome: OutcomeUncertain, DurationMs: 12},
			{Backend: BackendChat, Endpoint: "http://127.0.0.1:9002", Locality: "local", Model: "chat-1", Answer: "yes", Sufficient: yes(), Outcome: OutcomeDecided, DurationMs: 480},
		}}
}

func und(id string) Record {
	r := mid(id, "evt_und", t0)
	r.DecidedBy, r.Answer, r.LowConfidence = DecidedDefault, "no", true
	r.Levels[1] = Level{Backend: BackendChat, Model: "chat-1", Outcome: OutcomeUncertain, DurationMs: 9}
	return r
}

func TestPutGetKeepsEveryLevel(t *testing.T) {
	s := open(t)
	if err := s.Update(bg, func(tx *store.Tx) error { return Put(tx, mid("jd_1", "evt_mid", t0)) }); err != nil {
		t.Fatal(err)
	}
	var got Record
	if err := s.View(bg, func(tx *store.Tx) (err error) { got, err = Get(tx, "jd_1"); return }); err != nil {
		t.Fatal(err)
	}
	if got.DecidedBy != DecidedChat || got.Answer != "yes" || got.Band != (Band{Low: 0.15, High: 0.60}) || got.Source != "src_1" || !got.CreatedAt.Equal(t0) {
		t.Fatalf("record header: %+v", got)
	}
	if len(got.Levels) != 2 || got.Levels[0].Outcome != OutcomeUncertain || *got.Levels[0].PYes != 0.30 || got.Levels[0].Locality != "local" ||
		got.Levels[1].Outcome != OutcomeDecided || got.Levels[1].Answer != "yes" || !*got.Levels[1].Sufficient || got.Levels[1].DurationMs != 480 {
		t.Fatalf("levels: %+v", got.Levels)
	}
}

func TestPutRejectsBadRecordsAndDuplicates(t *testing.T) {
	s := open(t)
	bad := map[string]func(*Record){
		"empty subject":          func(r *Record) { r.Subject = "" },
		"unknown decided_by":     func(r *Record) { r.DecidedBy = "oracle" },
		"default w/o low conf":   func(r *Record) { r.DecidedBy, r.LowConfidence = DecidedDefault, false },
		"decider not in levels":  func(r *Record) { r.DecidedBy = DecidedClassifier },
		"p_yes out of range":     func(r *Record) { r.Levels[0].PYes = f64(1.5) },
		"unknown outcome":        func(r *Record) { r.Levels[0].Outcome = "maybe" },
		"unknown backend":        func(r *Record) { r.Levels[0].Backend = "oracle" },
		"inverted band":          func(r *Record) { r.Band = Band{Low: 0.7, High: 0.2} },
		"bad review":             func(r *Record) { r.Review = &Review{Result: "meh", At: t0} },
		"negative duration":      func(r *Record) { r.Levels[0].DurationMs = -1 },
		"zero created_at":        func(r *Record) { r.CreatedAt = time.Time{} },
		"default but level said": func(r *Record) { r.DecidedBy, r.LowConfidence = DecidedDefault, true },
	}
	for name, mut := range bad {
		r := mid("jd_bad", "evt", t0)
		mut(&r)
		err := s.Update(bg, func(tx *store.Tx) error { return Put(tx, r) })
		if e, ok := err.(*domain.Error); !ok || e.Code != domain.CodeInvalidRequest {
			t.Errorf("%s: want invalid_request, got %v", name, err)
		}
	}
	if err := s.Update(bg, func(tx *store.Tx) error { return Put(tx, mid("jd_1", "evt", t0)) }); err != nil {
		t.Fatal(err)
	}
	err := s.Update(bg, func(tx *store.Tx) error { return Put(tx, mid("jd_1", "evt", t0)) })
	if e, ok := err.(*domain.Error); !ok || e.Code != domain.CodeConflict {
		t.Fatalf("duplicate id must conflict, got %v", err)
	}
	// 影子打分只记分：没有级定案也可以写入。
	sh := mid("jd_sh", "evt", t0)
	sh.Shadow, sh.DecidedBy, sh.Levels = true, DecidedClassifier, sh.Levels[:1]
	if err := s.Update(bg, func(tx *store.Tx) error { return Put(tx, sh) }); err != nil {
		t.Fatalf("shadow record: %v", err)
	}
	// 默认定案：各级都没有定案。
	if err := s.Update(bg, func(tx *store.Tx) error { return Put(tx, und("jd_und")) }); err != nil {
		t.Fatalf("default-decided record: %v", err)
	}
}

func TestGetMissingIsNotFound(t *testing.T) {
	s := open(t)
	err := s.View(bg, func(tx *store.Tx) error { _, err := Get(tx, "nope"); return err })
	if e, ok := err.(*domain.Error); !ok || e.Code != domain.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}

func TestListFiltersAndPages(t *testing.T) {
	s := open(t)
	err := s.Update(bg, func(tx *store.Tx) error {
		for i := 0; i < 5; i++ {
			r := mid(fmt.Sprintf("jd_%d", i), fmt.Sprintf("evt_%d", i), t0.Add(time.Duration(i)*time.Minute))
			if i == 4 {
				r.Use, r.Source = "capture_prescreen", "src_2"
			}
			if err := Put(tx, r); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ids := func(rs []Record) string {
		var b []string
		for _, r := range rs {
			b = append(b, r.ID)
		}
		return strings.Join(b, ",")
	}
	list := func(f Filter) (string, bool) {
		var rs []Record
		var more bool
		if err := s.View(bg, func(tx *store.Tx) (err error) { rs, more, err = List(tx, f); return }); err != nil {
			t.Fatal(err)
		}
		return ids(rs), more
	}
	if got, _ := list(Filter{Source: "src_1", Use: "inbound_screen"}); got != "jd_0,jd_1,jd_2,jd_3" {
		t.Errorf("source+use filter: %s", got)
	}
	if got, _ := list(Filter{Subject: "evt_2"}); got != "jd_2" {
		t.Errorf("subject filter: %s", got)
	}
	if got, _ := list(Filter{Template: "nope"}); got != "" {
		t.Errorf("template filter: %q", got)
	}
	p1, more := list(Filter{Limit: 2})
	if p1 != "jd_0,jd_1" || !more {
		t.Fatalf("page 1: %s more=%v", p1, more)
	}
	p2, more := list(Filter{Limit: 2, After: "jd_1"})
	if p2 != "jd_2,jd_3" || !more {
		t.Fatalf("page 2: %s more=%v", p2, more)
	}
	if p3, more := list(Filter{Limit: 2, After: "jd_3"}); p3 != "jd_4" || more {
		t.Fatalf("page 3: %s more=%v", p3, more)
	}
	for _, f := range []Filter{{Limit: MaxLimit + 1}, {Limit: -1}, {After: "unknown"}} {
		err := s.View(bg, func(tx *store.Tx) error { _, _, err := List(tx, f); return err })
		if e, ok := err.(*domain.Error); !ok || e.Code != domain.CodeInvalidRequest {
			t.Errorf("%+v: want invalid_request, got %v", f, err)
		}
	}
}

func TestDeleteBeforeAndBySubject(t *testing.T) {
	s := open(t)
	_ = s.Update(bg, func(tx *store.Tx) error {
		for i, at := range []time.Time{t0, t0.Add(48 * time.Hour), t0.Add(96 * time.Hour)} {
			if err := Put(tx, mid(fmt.Sprintf("jd_%d", i), fmt.Sprintf("evt_%d", i), at)); err != nil {
				return err
			}
		}
		return nil
	})
	var n, m int64
	if err := s.Update(bg, func(tx *store.Tx) (err error) {
		if n, err = DeleteBefore(tx, t0.Add(72*time.Hour)); err != nil {
			return err
		}
		m, err = DeleteBySubject(tx, "evt_2", "evt_missing")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if n != 2 || m != 1 {
		t.Fatalf("deleted before=%d bySubject=%d", n, m)
	}
	var left []Record
	_ = s.View(bg, func(tx *store.Tx) (err error) { left, _, err = List(tx, Filter{}); return })
	if len(left) != 0 {
		t.Fatalf("records left: %+v", left)
	}
}

func TestRefOf(t *testing.T) {
	if r := RefOf(mid("jd_1", "e", t0)); r != (Ref{Judgment: "jd_1", Answer: "yes", DecidedBy: DecidedChat}) {
		t.Fatalf("%+v", r)
	}
}
