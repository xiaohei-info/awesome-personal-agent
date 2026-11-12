package why

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

var (
	owner = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}
	rule  = domain.Actor{Type: domain.ActorSystem, Principal: "rules", Via: domain.ViaRule}
	agent = domain.Actor{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI}
)

func open(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, proc.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func seed(t *testing.T, s *store.Store) {
	t.Helper()
	evs := []domain.Event{
		{Type: domain.EvItemCreated, Object: "item_1", Actor: rule, Reason: "SECRET-REASON",
			Payload: json.RawMessage(`{"title":"SECRET-TITLE"}`), Basis: &domain.Basis{PolicyVersion: 12, RuleID: "inbound-triage"}, ChainDepth: 1},
		{Type: domain.EvRunStarted, Object: "run_1", Actor: agent, Payload: json.RawMessage(`{"item":"item_1"}`)},
		{Type: domain.EvDecisionResolved, Object: "item_1", Actor: owner, Payload: json.RawMessage(`{"answer":"A"}`),
			Evidence: "SECRET-QUOTE", ResolvedVia: "web"},
		{Type: domain.EvCommandRejected, Object: "item_1", Actor: agent, Payload: json.RawMessage(`{"exit":20}`)},
	}
	err := s.Update(ctx, func(tx *store.Tx) error {
		for _, e := range evs {
			if _, err := eventlog.Append(tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func replay(t *testing.T, s *store.Store, r Request) (Timeline, error) {
	t.Helper()
	var tl Timeline
	err := s.View(ctx, func(tx *store.Tx) error {
		var err error
		tl, err = Replay(tx, policy.Policy{}, r)
		return err
	})
	return tl, err
}

func TestOwnerSeesFullTimelineInOrder(t *testing.T) {
	s := open(t)
	seed(t, s)
	tl, err := replay(t, s, Request{Actor: owner, Level: authz.LevelWeb, ID: "item_1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(tl.Steps) != 3 { // run_1 的事件不属于 item_1
		t.Fatalf("steps = %d, want 3", len(tl.Steps))
	}
	first, mid, last := tl.Steps[0], tl.Steps[1], tl.Steps[2]
	if first.Type != domain.EvItemCreated || first.Basis == nil || first.Basis.RuleID != "inbound-triage" ||
		first.Basis.PolicyVersion != 12 || first.ChainDepth != 1 || first.Reason != "SECRET-REASON" {
		t.Fatalf("first step lost trigger/rule/basis: %+v", first)
	}
	if mid.Evidence != "SECRET-QUOTE" || mid.ResolvedVia != "web" || mid.Redacted {
		t.Fatalf("owner must see evidence: %+v", mid)
	}
	if last.Type != domain.EvCommandRejected { // 被拒绝的命令也可回放
		t.Fatalf("rejected command missing: %+v", last)
	}
	if !(first.Seq < mid.Seq && mid.Seq < last.Seq) {
		t.Fatal("not ordered by seq")
	}
}

func TestCredentialLevelTrimsContent(t *testing.T) {
	s := open(t)
	seed(t, s)
	cases := []struct {
		level                     authz.Level
		reason, payload, evidence bool
	}{
		{authz.LevelDelegate, true, true, true},
		{authz.LevelWorker, true, true, false},
		{authz.LevelRun, true, true, false},
		{authz.LevelObserver, false, false, false},
	}
	for _, c := range cases {
		tl, err := replay(t, s, Request{Actor: agent, Level: c.level, ID: "item_1"})
		if err != nil {
			t.Fatalf("%s: %v", c.level, err)
		}
		a, b := tl.Steps[0], tl.Steps[1]
		if (a.Reason != "") != c.reason || (len(a.Payload) > 0) != c.payload || (b.Evidence != "") != c.evidence {
			t.Errorf("%s: reason=%q payload=%s evidence=%q", c.level, a.Reason, a.Payload, b.Evidence)
		}
		// 授权依据与信封对所有凭据级别可见。
		if a.Basis == nil || a.Basis.RuleID != "inbound-triage" || a.Type == "" || a.Actor.Principal == "" {
			t.Errorf("%s: envelope/basis trimmed: %+v", c.level, a)
		}
		if c.level != authz.LevelDelegate && !b.Redacted {
			t.Errorf("%s: trimmed step must be marked redacted", c.level)
		}
	}
}

func TestScreeningPendingReturnsOnlyMarkerToNonWeb(t *testing.T) {
	s := open(t)
	seed(t, s)
	tl, err := replay(t, s, Request{Actor: agent, Level: authz.LevelDelegate, ID: "item_1", ScreeningPending: true})
	if err != nil {
		t.Fatal(err)
	}
	if !tl.ScreeningPending || len(tl.Steps) != 0 {
		t.Fatalf("non-web must get marker only: %+v", tl)
	}
	tl, err = replay(t, s, Request{Actor: owner, Level: authz.LevelWeb, ID: "item_1", ScreeningPending: true})
	if err != nil || len(tl.Steps) != 3 {
		t.Fatalf("owner sees full timeline while screening: %v %+v", err, tl)
	}
}

func TestTombstonedContentStaysErased(t *testing.T) {
	s := open(t)
	seed(t, s)
	err := s.Update(ctx, func(tx *store.Tx) error { _, err := eventlog.Tombstone(tx, "item_1"); return err })
	if err != nil {
		t.Fatal(err)
	}
	tl, err := replay(t, s, Request{Actor: owner, Level: authz.LevelWeb, ID: "item_1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range tl.Steps {
		if !st.Tombstone || st.Reason != "" || st.Evidence != "" || string(st.Payload) != "null" {
			t.Fatalf("tombstone content leaked: %+v", st)
		}
	}
	if tl.Steps[0].Actor.Principal != "rules" || tl.Steps[0].Type != domain.EvItemCreated {
		t.Fatal("envelope must survive tombstone")
	}
}

func TestReplayRunByID(t *testing.T) {
	s := open(t)
	seed(t, s)
	tl, err := replay(t, s, Request{Actor: owner, Level: authz.LevelWeb, ID: "run_1"})
	if err != nil || len(tl.Steps) != 1 || tl.Steps[0].Type != domain.EvRunStarted {
		t.Fatalf("run replay: %v %+v", err, tl)
	}
}

func TestErrors(t *testing.T) {
	s := open(t)
	seed(t, s)
	check := func(r Request, code domain.Code) {
		t.Helper()
		_, err := replay(t, s, r)
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != code {
			t.Fatalf("%+v: err = %v, want %s", r, err, code)
		}
	}
	check(Request{Actor: owner, Level: authz.LevelWeb, ID: "nope"}, domain.CodeNotFound)
	check(Request{Actor: owner, Level: authz.LevelWeb}, domain.CodeInvalidRequest)
	// Web 级只授予 Web 上的本人。
	check(Request{Actor: agent, Level: authz.LevelWeb, ID: "item_1"}, domain.CodePermissionDenied)
}

func TestReplayIsReadOnly(t *testing.T) {
	s := open(t)
	seed(t, s)
	var before, after int64
	s.View(ctx, func(tx *store.Tx) error { before, _ = eventlog.Head(tx); return nil })
	replay(t, s, Request{Actor: owner, Level: authz.LevelWeb, ID: "item_1"})
	s.View(ctx, func(tx *store.Tx) error { after, _ = eventlog.Head(tx); return nil })
	if before != after {
		t.Fatal("replay wrote to ledger")
	}
}

func audit(t *testing.T, s *store.Store, r AuditRequest) ([]Step, error) {
	t.Helper()
	var out []Step
	err := s.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = Audit(tx, policy.Policy{}, r)
		return err
	})
	return out, err
}

func TestAuditFilterTrimPendingAndLimits(t *testing.T) {
	s := open(t)
	seed(t, s)
	all, err := audit(t, s, AuditRequest{Actor: owner, Level: authz.LevelWeb})
	if err != nil || len(all) != 4 || all[0].Reason != "SECRET-REASON" {
		t.Fatalf("web audit = %v %v", all, err)
	}
	rej, _ := audit(t, s, AuditRequest{Actor: owner, Level: authz.LevelWeb, Filter: eventlog.Filter{Type: domain.EvCommandRejected}})
	if len(rej) != 1 || rej[0].Object != "item_1" {
		t.Fatalf("rejected = %v", rej)
	}
	// 非 Web 且对象初筛未返回：该对象的事件被跳过，其余照常，且翻页能越过被跳过的行。
	obs, _ := audit(t, s, AuditRequest{Actor: owner, Level: authz.LevelObserver, Pending: map[string]bool{"item_1": true}, Limit: 1})
	if len(obs) != 1 || obs[0].Object != "run_1" || obs[0].Payload != nil {
		t.Fatalf("observer = %v", obs)
	}
	web, _ := audit(t, s, AuditRequest{Actor: owner, Level: authz.LevelWeb, Pending: map[string]bool{"item_1": true}})
	if len(web) != 4 {
		t.Fatalf("web ignores pending, got %d", len(web))
	}
	for _, bad := range []AuditRequest{{Limit: -1}, {Limit: MaxAuditLimit + 1}, {After: -1}} {
		bad.Actor, bad.Level = owner, authz.LevelWeb
		if _, err := audit(t, s, bad); err == nil {
			t.Fatalf("%+v must fail", bad)
		}
	}
	if _, err := audit(t, s, AuditRequest{Actor: owner, Level: "bogus"}); err == nil {
		t.Fatal("invalid level must be denied")
	}
}

func f64(v float64) *float64 { return &v }

// seedJudged 写一条判断记录（JRC-MID：两级，对话端点定案）和三条 Event：
// 引用该记录的处置、v0.1 的 run.finished（category: judge）、不带 judgment 的普通 Event。
func seedJudged(t *testing.T, s *store.Store) record.Record {
	t.Helper()
	yes := true
	rec := record.Record{ID: "jd_mid", Use: "inbound_screen", Template: "triage/triage-screen@abc", Subject: "evt_1", Source: "src_1",
		Fingerprint: "fp", Band: record.Band{Low: 0.15, High: 0.60}, DecidedBy: record.DecidedChat, Answer: "yes",
		CreatedAt: time.Date(2026, 11, 1, 8, 0, 0, 0, time.UTC),
		Levels: []record.Level{
			{Backend: record.BackendClassifier, Model: "cls", PYes: f64(0.30), Outcome: record.OutcomeUncertain, DurationMs: 5},
			{Backend: record.BackendChat, Model: "chat", Answer: "yes", Sufficient: &yes, Outcome: record.OutcomeDecided, DurationMs: 90}}}
	ref, _ := json.Marshal(record.RefOf(rec))
	err := s.Update(ctx, func(tx *store.Tx) error {
		if err := record.Put(tx, rec); err != nil {
			return err
		}
		for _, e := range []domain.Event{
			{Type: "inbound.disposed", Object: "item_j", Actor: rule, Basis: &domain.Basis{PolicyVersion: 1, RuleID: "inbound-triage"}, Payload: ref},
			{Type: domain.EvRunFinished, Object: "item_j", Actor: agent, Payload: json.RawMessage(`{"category":"judge","probability":0.82,"confidence":0.9}`)},
			{Type: domain.EvItemCreated, Object: "item_j", Actor: rule, Basis: &domain.Basis{PolicyVersion: 1, RuleID: "inbound-triage"}, Payload: json.RawMessage(`{"title":"t"}`)},
		} {
			if _, err := eventlog.Append(tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestReplayExpandsJudgmentLevelsForEveryCredentialLevel(t *testing.T) {
	s := open(t)
	seedJudged(t, s)
	for _, l := range []authz.Level{authz.LevelWeb, authz.LevelObserver, authz.LevelRun} {
		who := agent
		if l == authz.LevelWeb {
			who = owner
		}
		tl, err := replay(t, s, Request{Actor: who, Level: l, ID: "item_j"})
		if err != nil {
			t.Fatalf("%s: %v", l, err)
		}
		j := tl.Steps[0].Judgment
		if j == nil || j.Expired || j.ID != "jd_mid" || j.DecidedBy != "chat" || j.Answer != "yes" || j.Record == nil || len(j.Record.Levels) != 2 {
			t.Fatalf("%s: judgment not expanded: %+v", l, j)
		}
		if lv := j.Record.Levels; *lv[0].PYes != 0.30 || lv[0].Outcome != "uncertain" || lv[1].Outcome != "decided" {
			t.Fatalf("%s: levels: %+v", l, lv)
		}
	}
}

func TestReplayAfterRetentionKeepsOnlyEventConclusion(t *testing.T) {
	s := open(t)
	seedJudged(t, s)
	if err := s.Update(ctx, func(tx *store.Tx) error { _, err := record.DeleteBefore(tx, time.Now()); return err }); err != nil {
		t.Fatal(err)
	}
	tl, err := replay(t, s, Request{Actor: agent, Level: authz.LevelObserver, ID: "item_j"})
	if err != nil {
		t.Fatalf("expired record must not break why: %v", err)
	}
	j := tl.Steps[0].Judgment
	if j == nil || !j.Expired || j.Record != nil || j.ID != "jd_mid" || j.Answer != "yes" || j.DecidedBy != "chat" {
		t.Fatalf("want conclusion from the Event only: %+v", j)
	}
	if len(tl.Steps) != 3 {
		t.Fatalf("events must stay: %d", len(tl.Steps))
	}
}

func TestV01JudgeEventReplaysUntouched(t *testing.T) {
	s := open(t)
	seedJudged(t, s)
	tl, err := replay(t, s, Request{Actor: owner, Level: authz.LevelWeb, ID: "item_j"})
	if err != nil {
		t.Fatal(err)
	}
	old := tl.Steps[1]
	if old.Type != domain.EvRunFinished || old.Judgment != nil || string(old.Payload) != `{"category":"judge","probability":0.82,"confidence":0.9}` {
		t.Fatalf("v0.1 judge event must replay as is: %+v", old)
	}
	if tl.Steps[2].Judgment != nil {
		t.Fatal("event without judgment field must have no judgment line")
	}
}
