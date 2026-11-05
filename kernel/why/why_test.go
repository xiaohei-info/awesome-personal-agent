package why

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
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
	if err := reg.Register(eventlog.Module); err != nil {
		t.Fatal(err)
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
