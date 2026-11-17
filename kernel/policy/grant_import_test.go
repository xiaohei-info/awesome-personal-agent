package policy

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func importInput(cred string) GrantInput {
	return GrantInput{Grantee: Grantee{Credential: cred}, Scope: Scope{All: true}, Actions: []string{ImportApply},
		Import: &ImportScope{Source: "notes", MaxRecords: 100}, ExpiresAt: time.Now().Add(24 * time.Hour)}
}

// ACT-IGR-01：导入专用单的签发规则。
func TestImportGrantIssueRules(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	g, err := issue(t, s, a, owner, importInput(dlg.ID))
	if err != nil || g.Import == nil || g.Import.Source != "notes" || g.Import.MaxRecords != 100 || !g.Scope.All || g.Grantee.Background {
		t.Fatalf("valid import grant: %+v %v", g, err)
	}
	// 长度恰为 7 天的单可签；Agent 式（非后台）的单可签，空 scope 记为 all。
	in := importInput("")
	in.Grantee, in.Scope, in.ExpiresAt = Grantee{Agent: "hermes"}, Scope{}, time.Now().Add(7*24*time.Hour-time.Minute)
	if g, err = issue(t, s, a, owner, in); err != nil || !g.Scope.All {
		t.Fatalf("agent import grant: %+v %v", g, err)
	}
	cases := map[string]func(*GrantInput){
		"no import segment":      func(in *GrantInput) { in.Import = nil },
		"blank source":           func(in *GrantInput) { in.Import.Source = " " },
		"zero max_records":       func(in *GrantInput) { in.Import.MaxRecords = 0 },
		"negative max_records":   func(in *GrantInput) { in.Import.MaxRecords = -1 },
		"no expiry":              func(in *GrantInput) { in.ExpiresAt = time.Time{} },
		"expiry beyond 7 days":   func(in *GrantInput) { in.ExpiresAt = time.Now().Add(7*24*time.Hour + time.Hour) },
		"mixed with other act":   func(in *GrantInput) { in.Actions = []string{ImportApply, "item.update"} },
		"background":             func(in *GrantInput) { in.Grantee = Grantee{Agent: "hermes", Background: true} },
		"narrow scope":           func(in *GrantInput) { in.Scope = Scope{Projects: []string{"p1"}} },
		"import on other action": func(in *GrantInput) { in.Actions = []string{"item.update"} },
	}
	for name, mut := range cases {
		in := importInput(dlg.ID)
		mut(&in)
		if _, err := issue(t, s, a, owner, in); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	if _, err := issue(t, s, a, agent, importInput(dlg.ID)); !isCode(err, domain.CodeWebOnly) {
		t.Fatalf("agent must not issue an import grant: %v", err)
	}
	if len(grantEvents(t, s, domain.EvGrantIssued)) != 2 {
		t.Fatal("only the two valid grants log events")
	}
}

// WB-19：item.progress 可授权；已签发的单内容不变。
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

func consume(s *store.Store, id, source string, n int) error {
	return s.Update(ctx, func(tx *store.Tx) error { return ConsumeImport(tx, id, source, n, time.Now()) })
}

func used(t *testing.T, s *store.Store, id string) int {
	t.Helper()
	var n int
	if err := s.View(ctx, func(tx *store.Tx) (err error) { n, err = ImportUsed(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return n
}

// ACT-IGR-08：条数累计扣减；预检不扣；来源不符、超限、撤销、过期都拒绝且不扣。
func TestConsumeImport(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	in := importInput(dlg.ID)
	in.Import.MaxRecords = 10
	g, err := issue(t, s, a, owner, in)
	if err != nil {
		t.Fatal(err)
	}
	check := func(source string, n int) error {
		return s.View(ctx, func(tx *store.Tx) error { return CheckImport(tx, g.ID, source, n, time.Now()) })
	}
	if err := check("notes", 10); err != nil || used(t, s, g.ID) != 0 {
		t.Fatalf("precheck must not deduct: %v used=%d", err, used(t, s, g.ID))
	}
	if err := consume(s, g.ID, "notes", 6); err != nil || used(t, s, g.ID) != 6 {
		t.Fatalf("first consume: %v used=%d", err, used(t, s, g.ID))
	}
	if err := check("notes", 5); !isCode(err, domain.CodeImportLimitExceeded) {
		t.Fatalf("precheck over limit: %v", err)
	}
	if err := consume(s, g.ID, "notes", 5); !isCode(err, domain.CodeImportLimitExceeded) || used(t, s, g.ID) != 6 {
		t.Fatalf("over limit must not deduct: %v used=%d", err, used(t, s, g.ID))
	}
	if err := consume(s, g.ID, "mail", 1); !isCode(err, domain.CodeImportSourceMismatch) || used(t, s, g.ID) != 6 {
		t.Fatalf("source mismatch: %v", err)
	}
	if err := consume(s, g.ID, "notes", 0); !errors.Is(err, ErrInvalid) {
		t.Fatalf("n must be positive: %v", err)
	}
	if err := consume(s, g.ID, "notes", 4); err != nil || used(t, s, g.ID) != 10 {
		t.Fatalf("exactly up to the limit: %v", err)
	}
	if err := s.Update(ctx, func(tx *store.Tx) error {
		return ConsumeImport(tx, g.ID, "notes", 1, time.Now().Add(25*time.Hour))
	}); !isCode(err, domain.CodeBasisRequired) {
		t.Fatalf("expired: %v", err)
	}
	if _, err := revoke(t, s, owner, g.ID); err != nil {
		t.Fatal(err)
	}
	if err := consume(s, g.ID, "notes", 1); !isCode(err, domain.CodeBasisRequired) {
		t.Fatalf("revoked: %v", err)
	}
	// 非导入单、不存在的单。
	other, _ := issue(t, s, a, owner, validInput(dlg.ID))
	if err := consume(s, other.ID, "notes", 1); !isCode(err, domain.CodeGrantRequired) {
		t.Fatalf("not an import grant: %v", err)
	}
	if err := consume(s, "grt_nope", "notes", 1); !isCode(err, domain.CodeGrantRequired) {
		t.Fatalf("unknown grant: %v", err)
	}
}

// 并发扣减：总和绝不超过上限。
func TestConsumeImportIsAtomicUnderConcurrency(t *testing.T) {
	s, a := openGrants(t)
	dlg := issueCred(t, s, a, auth.Delegate)
	in := importInput(dlg.ID)
	in.Import.MaxRecords = 20
	g, _ := issue(t, s, a, owner, in)
	var wg sync.WaitGroup
	var mu sync.Mutex
	ok := 0
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if consume(s, g.ID, "notes", 3) == nil {
				mu.Lock()
				ok++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if got := used(t, s, g.ID); got != ok*3 || got > 20 || ok != 6 {
		t.Fatalf("ok=%d used=%d", ok, got)
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
