package judgments

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

type env struct {
	t              *testing.T
	st             *store.Store
	au             *auth.Auth
	srv            *core.Server
	now            time.Time
	itemA, itemB   string // A 是运行的目标事项
	run            string // 目标 A 的运行凭据
	hi, lo, orphan string // 判断记录 id：来源事件挂靠 A、挂靠 B、对象不存在
}

func f64(v float64) *float64 { return &v }

func rec(id, subject string, at time.Time) record.Record {
	return record.Record{ID: id, Use: "inbound_screen", Template: "triage/triage-screen@abc", Subject: subject, Source: "src_1",
		Fingerprint: "fp-" + id, Band: record.Band{Low: 0.15, High: 0.60}, DecidedBy: record.DecidedClassifier, Answer: "yes", CreatedAt: at,
		Levels: []record.Level{{Backend: record.BackendClassifier, Model: "cls", PYes: f64(0.9), ProbabilitySource: "native", Outcome: record.OutcomeDecided, DurationMs: 3}}}
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, core.Module, projects.Module, clock.Module,
		items.Module, runner.Module, proc.Module, runidentity.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC), au: auth.New(nil), hi: "jd_hi", lo: "jd_lo", orphan: "jd_orphan"}
	x := command.New(st, func() time.Time { return e.now })
	if err := items.Register(x, items.NewKinds()); err != nil {
		t.Fatal(err)
	}
	mk := func(title string) string {
		res, err := x.Do(bg, command.Request{Identity: command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb},
			Command: "item.create", Input: json.RawMessage(`{"kind":"task","title":"` + title + `"}`)})
		if err != nil {
			t.Fatal(err)
		}
		var it struct{ ID string }
		_ = json.Unmarshal(res.Value, &it)
		return it.ID
	}
	e.itemA, e.itemB = mk("a"), mk("b")
	if err := st.Update(bg, func(tx *store.Tx) error {
		src := domain.Source{ID: "src_1"}
		for _, id := range []string{"e_hi", "e_lo"} {
			if err := runner.AppendInternal(tx, src, domain.SourceEvent{ID: id, Type: "n", Subject: id, Summary: "s", Link: "https://x.example/" + id, OccurredAt: e.now}); err != nil {
				return err
			}
		}
		all, err := runner.Events(tx, "src_1")
		if err != nil {
			return err
		}
		if err := runner.SetDisposition(tx, all[0].ID, domain.DispositionProposed, e.itemA, ""); err != nil {
			return err
		}
		if err := runner.SetDisposition(tx, all[1].ID, domain.DispositionProposed, e.itemB, ""); err != nil {
			return err
		}
		for _, r := range []record.Record{rec(e.hi, all[0].ID, e.now), rec(e.lo, all[1].ID, e.now.Add(time.Second)), rec(e.orphan, "gone", e.now.Add(2*time.Second))} {
			if err := record.Put(tx, r); err != nil {
				return err
			}
		}
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunTask, Target: e.itemA, TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, e.now)
		if err != nil {
			return err
		}
		e.run, err = runidentity.Issue(tx, r.ID, e.now)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.srv, err = core.New(core.Deps{Store: st, Auth: e.au, Groups: []core.RouteGroup{Routes(st, nil)}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) token(kind auth.Kind) string {
	e.t.Helper()
	spec := auth.Spec{Kind: kind}
	if kind == auth.Ext || kind == auth.Webhook {
		spec.Scope = "x"
	}
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { _, secret, err = e.au.Issue(tx, spec); return }); err != nil {
		e.t.Fatal(err)
	}
	return secret
}

func (e *env) get(secret, path string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", path, nil)
	if secret != "" {
		req.Header.Set("Authorization", "Bearer "+secret)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func code(t *testing.T, r *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(r.Body.Bytes(), &de); err != nil {
		t.Fatalf("not an error body: %d %s", r.Code, r.Body)
	}
	return de.Code
}

func TestObserverReadsEveryRecordWithItsLevels(t *testing.T) {
	e := setup(t)
	obs := e.token(auth.Observer)
	r := e.get(obs, "/v1/judgments/"+e.hi)
	var got record.Record
	_ = json.Unmarshal(r.Body.Bytes(), &got)
	if r.Code != 200 || got.ID != e.hi || got.DecidedBy != "classifier" || len(got.Levels) != 1 || *got.Levels[0].PYes != 0.9 || got.Levels[0].Outcome != "decided" {
		t.Fatalf("get: %d %s", r.Code, r.Body)
	}
	var page struct {
		Judgments []record.Record `json:"judgments"`
		NextAfter string          `json:"next_after"`
	}
	r = e.get(obs, "/v1/judgments?source=src_1&use=inbound_screen")
	_ = json.Unmarshal(r.Body.Bytes(), &page)
	if r.Code != 200 || len(page.Judgments) != 3 || page.NextAfter != "" {
		t.Fatalf("list: %d %s", r.Code, r.Body)
	}
	r = e.get(obs, "/v1/judgments?limit=2")
	page.Judgments, page.NextAfter = nil, ""
	_ = json.Unmarshal(r.Body.Bytes(), &page)
	if len(page.Judgments) != 2 || page.NextAfter != e.lo {
		t.Fatalf("page 1: %s", r.Body)
	}
	r = e.get(obs, "/v1/judgments?limit=2&after="+page.NextAfter)
	page.Judgments, page.NextAfter = nil, ""
	_ = json.Unmarshal(r.Body.Bytes(), &page)
	if len(page.Judgments) != 1 || page.Judgments[0].ID != e.orphan || page.NextAfter != "" {
		t.Fatalf("page 2: %s", r.Body)
	}
	if r = e.get(obs, "/v1/judgments?subject=nothing"); r.Code != 200 || !strings.Contains(r.Body.String(), `"judgments":[]`) {
		t.Fatalf("empty list must be [] not null: %d %s", r.Code, r.Body)
	}
}

func TestRunCredentialReadsOnlyTargetItemRecords(t *testing.T) {
	e := setup(t)
	if r := e.get(e.run, "/v1/judgments/"+e.hi); r.Code != 200 {
		t.Fatalf("target item's record: %d %s", r.Code, r.Body)
	}
	if r := e.get(e.run, "/v1/judgments/"+e.lo); r.Code != 404 || code(t, r) != domain.CodeNotFound {
		t.Fatalf("other item's record must look absent: %d %s", r.Code, r.Body)
	}
	if r := e.get(e.run, "/v1/judgments/"+e.orphan); r.Code != 404 {
		t.Fatalf("record of an unknown subject: %d %s", r.Code, r.Body)
	}
	if r := e.get(e.run, "/v1/judgments"); r.Code != 400 {
		t.Fatalf("run list without subject: %d %s", r.Code, r.Body)
	}
	if r := e.get(e.run, "/v1/judgments?subject=e_lo"); r.Code != 200 || !strings.Contains(r.Body.String(), `"judgments":[]`) {
		t.Fatalf("out of scope subject lists nothing: %d %s", r.Code, r.Body)
	}
	var all struct{ Judgments []record.Record }
	var evID string
	_ = e.st.View(bg, func(tx *store.Tx) error {
		evs, err := runner.Events(tx, "src_1")
		evID = evs[0].ID
		return err
	})
	r := e.get(e.run, "/v1/judgments?subject="+evID)
	_ = json.Unmarshal(r.Body.Bytes(), &all)
	if r.Code != 200 || len(all.Judgments) != 1 || all.Judgments[0].ID != e.hi {
		t.Fatalf("in scope subject: %d %s", r.Code, r.Body)
	}
	// 事项本身作为 subject（对话增量、事项上的判断）也算范围内。
	if err := e.st.Update(bg, func(tx *store.Tx) error { return record.Put(tx, rec("jd_item", e.itemA, e.now.Add(time.Hour))) }); err != nil {
		t.Fatal(err)
	}
	if r := e.get(e.run, "/v1/judgments/jd_item"); r.Code != 200 {
		t.Fatalf("item subject: %d %s", r.Code, r.Body)
	}
}

func TestErrorsAndReservedWords(t *testing.T) {
	e := setup(t)
	obs := e.token(auth.Observer)
	if r := e.get("", "/v1/judgments"); r.Code != 401 {
		t.Fatalf("anonymous: %d", r.Code)
	}
	for _, k := range []auth.Kind{auth.Ext, auth.Webhook} {
		if r := e.get(e.token(k), "/v1/judgments/"+e.hi); r.Code != 403 {
			t.Fatalf("%s get: %d", k, r.Code)
		}
		if r := e.get(e.token(k), "/v1/judgments"); r.Code != 403 {
			t.Fatalf("%s list: %d", k, r.Code)
		}
	}
	if r := e.get(obs, "/v1/judgments/jd_nope"); r.Code != 404 {
		t.Fatalf("missing: %d", r.Code)
	}
	for _, bad := range []string{"limit=0", "limit=x", "limit=501", "after=jd_nope"} {
		if r := e.get(obs, "/v1/judgments?"+bad); r.Code != 400 {
			t.Fatalf("%s: %d %s", bad, r.Code, r.Body)
		}
	}
	for _, w := range []string{"stats", "spot-check"} {
		if r := e.get(obs, "/v1/judgments/"+w); r.Code != 404 || !strings.Contains(r.Body.String(), "no such route") {
			t.Fatalf("%s must not be read as a record id: %d %s", w, r.Code, r.Body)
		}
	}
}
