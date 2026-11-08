package policy

import (
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func openGrants(t *testing.T) (*store.Store, *auth.Auth) {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, auth.New(nil)
}

func issueCred(t *testing.T, s *store.Store, a *auth.Auth, k auth.Kind) auth.Credential {
	t.Helper()
	var c auth.Credential
	if err := s.Update(ctx, func(tx *store.Tx) (err error) {
		c, _, err = a.Issue(tx, auth.Spec{Kind: k, Label: string(k)})
		return
	}); err != nil {
		t.Fatal(err)
	}
	return c
}

func validInput(cred string) GrantInput {
	return GrantInput{
		Grantee: Grantee{Credential: cred}, Scope: Scope{Projects: []string{"p1"}},
		Actions: []string{"decision.answer", "proposal.accept"}, ExpiresAt: time.Now().Add(7 * 24 * time.Hour), Reason: "away",
	}
}

func issue(t *testing.T, s *store.Store, a *auth.Auth, actor domain.Actor, in GrantInput) (Grant, error) {
	t.Helper()
	var g Grant
	err := s.Update(ctx, func(tx *store.Tx) (err error) { g, err = IssueGrant(tx, a, actor, time.Now(), in); return })
	return g, err
}

func revoke(t *testing.T, s *store.Store, actor domain.Actor, id string) (Grant, error) {
	t.Helper()
	var g Grant
	err := s.Update(ctx, func(tx *store.Tx) (err error) { g, err = RevokeGrant(tx, actor, time.Now(), id, "back home"); return })
	return g, err
}

func active(t *testing.T, s *store.Store, a *auth.Auth, cred string, at time.Time) []Grant {
	t.Helper()
	var out []Grant
	if err := s.View(ctx, func(tx *store.Tx) (err error) { out, err = ActiveGrants(tx, a, cred, at); return }); err != nil {
		t.Fatal(err)
	}
	return out
}

func grantEvents(t *testing.T, s *store.Store, typ string) []domain.Event {
	t.Helper()
	var out []domain.Event
	if err := s.View(ctx, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, e := range es {
			if e.Event.Type == typ {
				out = append(out, e.Event)
			}
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestIssueGrantStoresRecordAndLogsEvent(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	g, err := issue(t, s, a, owner, validInput(dlg.ID))
	if err != nil {
		t.Fatal(err)
	}
	if g.ID == "" || g.Principal != "me" || g.RiskCeiling != RiskNormal || g.CreatedAt.IsZero() || !g.RevokedAt.IsZero() {
		t.Fatalf("%+v", g)
	}
	got := active(t, s, a, dlg.ID, time.Now())
	if len(got) != 1 || got[0].ID != g.ID || got[0].Actions[0] != "decision.answer" {
		t.Fatalf("active: %+v", got)
	}
	evs := grantEvents(t, s, domain.EvGrantIssued)
	if len(evs) != 1 || evs[0].Object != g.ID || evs[0].Grant != g.ID || evs[0].Actor.Via != domain.ViaWeb {
		t.Fatalf("events: %+v", evs)
	}
}

func TestIssueGrantRejectsInvalidGrants(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	wrk := issueCred(t, s, a, auth.Worker)
	revoked := issueCred(t, s, a, auth.Delegate)
	if err := s.Update(ctx, func(tx *store.Tx) error { return a.Revoke(tx, revoked.ID) }); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*GrantInput){
		"empty scope":           func(in *GrantInput) { in.Scope = Scope{Projects: []string{" "}} },
		"no actions":            func(in *GrantInput) { in.Actions = nil },
		"wildcard action":       func(in *GrantInput) { in.Actions = []string{"*"} },
		"unknown action":        func(in *GrantInput) { in.Actions = []string{"item.delete"} },
		"web-only-only action":  func(in *GrantInput) { in.Actions = []string{"project.update"} },
		"risk above normal":     func(in *GrantInput) { in.RiskCeiling = "web_only" },
		"no expiry":             func(in *GrantInput) { in.ExpiresAt = time.Time{} },
		"expired":               func(in *GrantInput) { in.ExpiresAt = time.Now().Add(-time.Minute) },
		"over 90 days":          func(in *GrantInput) { in.ExpiresAt = time.Now().Add(91 * 24 * time.Hour) },
		"worker grantee":        func(in *GrantInput) { in.Grantee.Credential = wrk.ID },
		"revoked grantee":       func(in *GrantInput) { in.Grantee.Credential = revoked.ID },
		"unknown grantee":       func(in *GrantInput) { in.Grantee.Credential = "cred_nope" },
		"no grantee credential": func(in *GrantInput) { in.Grantee.Credential = "" },
	}
	for name, mut := range cases {
		in := validInput(dlg.ID)
		mut(&in)
		if _, err := issue(t, s, a, owner, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if n := len(grantEvents(t, s, domain.EvGrantIssued)); n != 0 {
		t.Fatalf("rejected grants must not log events, got %d", n)
	}
	in := validInput(dlg.ID)
	in.ExpiresAt = time.Now().Add(89 * 24 * time.Hour)
	if _, err := issue(t, s, a, owner, in); err != nil {
		t.Fatalf("89 days is within the limit: %v", err)
	}
}

func TestOnlyWebOwnerIssuesAndRevokes(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	for _, who := range []domain.Actor{agent, pack, cli} {
		_, err := issue(t, s, a, who, validInput(dlg.ID))
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeWebOnly {
			t.Errorf("issue by %+v: %v", who, err)
		}
	}
	g, err := issue(t, s, a, owner, validInput(dlg.ID))
	if err != nil {
		t.Fatal(err)
	}
	var de *domain.Error
	if _, err := revoke(t, s, agent, g.ID); !errors.As(err, &de) || de.Code != domain.CodeWebOnly {
		t.Fatalf("revoke by agent: %v", err)
	}
	if len(active(t, s, a, dlg.ID, time.Now())) != 1 {
		t.Fatal("agent must not be able to revoke")
	}
}

func TestRevokedExpiredAndForeignGrantsAreNotActive(t *testing.T) {
	s, a := openGrants(t)
	d1, d2 := issueCred(t, s, a, auth.Delegate), issueCred(t, s, a, auth.Delegate)
	g1, _ := issue(t, s, a, owner, validInput(d1.ID))
	g2, _ := issue(t, s, a, owner, validInput(d1.ID))
	if _, err := issue(t, s, a, owner, validInput(d2.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := revoke(t, s, owner, g1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := revoke(t, s, owner, g1.ID); err != nil { // 幂等：不重复记事件
		t.Fatal(err)
	}
	if n := len(grantEvents(t, s, domain.EvGrantRevoked)); n != 1 {
		t.Fatalf("revoked events = %d", n)
	}
	if got := active(t, s, a, d1.ID, time.Now()); len(got) != 1 || got[0].ID != g2.ID {
		t.Fatalf("d1 active: %+v", got)
	}
	if got := active(t, s, a, d1.ID, time.Now().Add(8*24*time.Hour)); len(got) != 0 {
		t.Fatalf("expired grants must drop out: %+v", got)
	}
	if err := s.Update(ctx, func(tx *store.Tx) error { return a.Revoke(tx, d1.ID) }); err != nil {
		t.Fatal(err)
	}
	if got := active(t, s, a, d1.ID, time.Now()); len(got) != 0 {
		t.Fatalf("revoking the token must invalidate its grants: %+v", got)
	}
	var all []Grant
	if err := s.View(ctx, func(tx *store.Tx) (err error) { all, err = ListGrants(tx); return }); err != nil || len(all) != 3 {
		t.Fatalf("Web list keeps revoked grants for audit: %d %v", len(all), err)
	}
	var de *domain.Error
	if _, err := revoke(t, s, owner, "grt_nope"); !errors.As(err, &de) || de.Code != domain.CodeNotFound {
		t.Fatalf("unknown grant: %v", err)
	}
}
