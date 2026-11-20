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
		"expired":               func(in *GrantInput) { in.ExpiresAt = time.Now().Add(-time.Minute) },
		"all plus projects":     func(in *GrantInput) { in.Scope = Scope{All: true, Projects: []string{"p"}} },
		"agent and credential":  func(in *GrantInput) { in.Grantee.Agent = "hermes" },
		"agent with spaces":     func(in *GrantInput) { in.Grantee = Grantee{Agent: "her mes"} },
		"background on a token": func(in *GrantInput) { in.Grantee.Background = true },
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
	in.ExpiresAt = time.Time{}
	g, err := issue(t, s, a, owner, in)
	if err != nil {
		t.Fatalf("a grant without expiry lasts until revoked: %v", err)
	}
	if !g.Active(time.Now().Add(10 * 365 * 24 * time.Hour)) {
		t.Fatalf("long-lived grant must stay active: %+v", g)
	}
}

// Agent 式授权单（§6.13）：不随钥匙失效；替换在同一事务里撤旧签新；actions 为空只撤不签；只有 Web 本人能替换。
func TestReplaceAgentGrant(t *testing.T) {
	s, a := openGrants(t)
	replace := func(actor domain.Actor, in GrantInput) (rev []string, g *Grant, err error) {
		err = s.Update(ctx, func(tx *store.Tx) (err error) {
			rev, g, err = ReplaceAgentGrant(tx, a, actor, time.Now(), "hermes", in)
			return
		})
		return
	}
	agentGrants := func() []Grant {
		var out []Grant
		if err := s.View(ctx, func(tx *store.Tx) (err error) { out, err = AgentGrants(tx, "hermes", time.Now()); return }); err != nil {
			t.Fatal(err)
		}
		return out
	}
	in := GrantInput{Grantee: Grantee{Background: true}, Scope: Scope{All: true}, Actions: []string{"proposal.dismiss"}}
	if _, _, err := replace(agent, in); !isCode(err, domain.CodeWebOnly) {
		t.Fatalf("agent replacing: %v", err)
	}
	_, g1, err := replace(owner, in)
	if err != nil || g1 == nil || g1.Grantee.Agent != "hermes" || !g1.Grantee.Background || !g1.ExpiresAt.IsZero() {
		t.Fatalf("first: %+v %v", g1, err)
	}
	in.Actions = []string{"proposal.dismiss", "decision.answer"}
	rev, g2, err := replace(owner, in)
	if err != nil || len(rev) != 1 || rev[0] != g1.ID || g2 == nil {
		t.Fatalf("second: %v %+v %v", rev, g2, err)
	}
	if got := agentGrants(); len(got) != 1 || got[0].ID != g2.ID {
		t.Fatalf("only the new grant stays active: %+v", got)
	}
	bad := in
	bad.Actions = []string{"project.update"}
	if _, _, err := replace(owner, bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid replacement: %v", err)
	}
	if got := agentGrants(); len(got) != 1 || got[0].ID != g2.ID {
		t.Fatalf("a failed replacement must keep the old grant: %+v", got)
	}
	in.Actions = nil
	rev, g3, err := replace(owner, in)
	if err != nil || g3 != nil || len(rev) != 1 || len(agentGrants()) != 0 {
		t.Fatalf("empty actions only revokes: %v %+v %v", rev, g3, err)
	}
}

func isCode(err error, c domain.Code) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == c
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

func TestProgressIsGrantable(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	in := validInput(dlg.ID)
	in.Actions = []string{"item.progress"}
	g, err := issue(t, s, a, owner, in)
	if err != nil || g.Actions[0] != "item.progress" || g.Import != nil {
		t.Fatalf("%+v %v", g, err)
	}
	for _, bad := range []string{"delegate.dispatch", "delegate.dispatch_update"} {
		in.Actions = []string{bad}
		if _, err := issue(t, s, a, owner, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s must stay ungrantable in this node: %v", bad, err)
		}
	}
}

// 已有的 JSON 内容不变：旧单读出时没有 import 段。
func TestOldGrantHasNoImportSegment(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	g, _ := issue(t, s, a, owner, validInput(dlg.ID))
	got := active(t, s, a, dlg.ID, time.Now())
	if len(got) != 1 || got[0].Import != nil || got[0].ID != g.ID {
		t.Fatalf("%+v", got)
	}
}

// 导入已取消（§14.5）：import.apply 不能再签发；下线前签发的导入专用单仍能原样读出。
func TestImportApplyIsNoLongerGrantableButOldImportGrantsStillRead(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	in := validInput(dlg.ID)
	in.Actions, in.Scope = []string{"import.apply"}, Scope{All: true}
	if _, err := issue(t, s, a, owner, in); !errors.Is(err, ErrInvalid) {
		t.Fatalf("import.apply must not be grantable: %v", err)
	}
	doc := `{"id":"grt_old","principal":"owner","grantee":{"credential":"` + dlg.ID + `"},"scope":{"projects":[],"kinds":[],"items":[],"all":true},` +
		`"actions":["import.apply"],"import":{"source":"notes","max_records":50},"risk_ceiling":"normal","created_at":"2026-11-01T00:00:00Z"}`
	if err := s.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO policy_grant (id, credential, doc, expires_at, created_at, used_records) VALUES ('grt_old', ?, ?, 0, 1, 7)`, dlg.ID, doc)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	got := active(t, s, a, dlg.ID, time.Now())
	if len(got) != 1 || got[0].ID != "grt_old" || got[0].Import == nil || got[0].Import.Source != "notes" || got[0].Import.MaxRecords != 50 {
		t.Fatalf("old import grant must read back unchanged: %+v", got)
	}
}
