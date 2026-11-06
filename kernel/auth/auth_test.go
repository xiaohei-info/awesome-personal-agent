package auth

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var ctx = context.Background()

type env struct {
	s   *store.Store
	a   *Auth
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := &env{s: s, now: time.Unix(1_700_000_000, 0)}
	e.a = New(func() time.Time { return e.now })
	return e
}

func (e *env) issue(t *testing.T, sp Spec) (Credential, string) {
	t.Helper()
	var c Credential
	var secret string
	if err := e.s.Update(ctx, func(tx *store.Tx) (err error) { c, secret, err = e.a.Issue(tx, sp); return }); err != nil {
		t.Fatal(err)
	}
	return c, secret
}

func (e *env) resolve(secret string) (c Credential, err error) {
	err = e.s.View(ctx, func(tx *store.Tx) (err error) { c, err = e.a.Resolve(tx, secret); return })
	return
}

func code(err error) domain.Code {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

// 明文只出一次：账本文件里找不到明文，只有哈希；列表不含明文。
func TestSecretStoredOnlyAsHash(t *testing.T) {
	e := setup(t)
	c, secret := e.issue(t, Spec{Kind: Worker, Executor: "exec-1"})
	if !strings.HasPrefix(secret, "apa_wrk_") || len(secret) < 40 {
		t.Fatalf("secret shape %q", secret)
	}
	e.s.Close()
	db, err := sql.Open("sqlite", filepath.Join(filepath.Dir(e.s.Path()), store.LedgerFile))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var hash string
	if err := db.QueryRow("SELECT hash FROM auth_credential WHERE id = ?", c.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if hash != hashOf(secret) || strings.Contains(hash, secret) {
		t.Fatalf("hash column = %q", hash)
	}
	var n int
	// 在所有列里搜明文
	if err := db.QueryRow("SELECT count(*) FROM auth_credential WHERE id||kind||hash||label||executor||scope LIKE ?", "%"+secret+"%").Scan(&n); err != nil || n != 0 {
		t.Fatalf("plaintext found in ledger: n=%d err=%v", n, err)
	}
}

func TestIssueResolveAllKinds(t *testing.T) {
	e := setup(t)
	for _, sp := range []Spec{{Kind: Observer}, {Kind: Worker, Executor: "x"}, {Kind: Delegate}, {Kind: Ext, Scope: "chan-a"}, {Kind: Webhook, Scope: "src-1"}} {
		c, secret := e.issue(t, sp)
		got, err := e.resolve(secret)
		if err != nil || got.ID != c.ID || got.Kind != sp.Kind || got.Scope != sp.Scope || got.Executor != sp.Executor {
			t.Fatalf("%s: got %+v err %v", sp.Kind, got, err)
		}
	}
	// 两次签发的明文不同。
	_, a := e.issue(t, Spec{Kind: Observer})
	_, b := e.issue(t, Spec{Kind: Observer})
	if a == b {
		t.Fatal("secrets collide")
	}
}

func TestIssueValidation(t *testing.T) {
	e := setup(t)
	for _, sp := range []Spec{{Kind: "root"}, {Kind: Ext}, {Kind: Webhook}, {Kind: Observer, Scope: "s"}, {Kind: Delegate, Executor: "x"}} {
		err := e.s.Update(ctx, func(tx *store.Tx) error { _, _, err := e.a.Issue(tx, sp); return err })
		if code(err) != domain.CodeInvalidRequest {
			t.Fatalf("%+v: err %v", sp, err)
		}
	}
	if cs := e.list(t); len(cs) != 0 {
		t.Fatalf("rejected issue left rows: %v", cs)
	}
}

func (e *env) list(t *testing.T) (cs []Credential) {
	t.Helper()
	if err := e.s.View(ctx, func(tx *store.Tx) (err error) { cs, err = e.a.List(tx); return }); err != nil {
		t.Fatal(err)
	}
	return
}

// 吊销立即生效，且幂等；不存在的 id 报 not_found。
func TestRevokeImmediate(t *testing.T) {
	e := setup(t)
	c, secret := e.issue(t, Spec{Kind: Delegate})
	if _, err := e.resolve(secret); err != nil {
		t.Fatal(err)
	}
	revoke := func(id string) error { return e.s.Update(ctx, func(tx *store.Tx) error { return e.a.Revoke(tx, id) }) }
	if err := revoke(c.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.resolve(secret); code(err) != domain.CodeUnauthenticated {
		t.Fatalf("revoked still resolves: %v", err)
	}
	first := e.list(t)[0].RevokedAt
	e.now = e.now.Add(time.Hour)
	if err := revoke(c.ID); err != nil {
		t.Fatal(err)
	}
	if got := e.list(t)[0].RevokedAt; !got.Equal(first) || first.IsZero() {
		t.Fatalf("revoked_at changed %v -> %v", first, got)
	}
	if err := revoke("cred_nope"); code(err) != domain.CodeNotFound {
		t.Fatalf("err %v", err)
	}
}

// 吊销与事务一起回滚；未提交的吊销不生效。
func TestRevokeRollsBackWithTx(t *testing.T) {
	e := setup(t)
	c, secret := e.issue(t, Spec{Kind: Observer})
	boom := errors.New("boom")
	err := e.s.Update(ctx, func(tx *store.Tx) error {
		if err := e.a.Revoke(tx, c.ID); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if _, err := e.resolve(secret); err != nil {
		t.Fatalf("rolled-back revoke took effect: %v", err)
	}
}

func TestResolveRejectsUnknownEmptyAndTampered(t *testing.T) {
	e := setup(t)
	_, secret := e.issue(t, Spec{Kind: Observer})
	for _, bad := range []string{"", "apa_obs_nope", secret + "x", secret[:len(secret)-1], strings.ToUpper(secret)} {
		if _, err := e.resolve(bad); code(err) != domain.CodeUnauthenticated || err.(*domain.Error).ExitCode() != domain.ExitUnauthenticated {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

// webhook 密钥轮换：新旧在宽限期内并存，之后旧的失效；绑定与范围继承。
func TestRotateOverlap(t *testing.T) {
	e := setup(t)
	old, oldSecret := e.issue(t, Spec{Kind: Webhook, Scope: "src-1", Label: "gh"})
	var nc Credential
	var newSecret string
	if err := e.s.Update(ctx, func(tx *store.Tx) (err error) { nc, newSecret, err = e.a.Rotate(tx, old.ID, 10*time.Minute); return }); err != nil {
		t.Fatal(err)
	}
	if nc.Scope != "src-1" || nc.Kind != Webhook || nc.Label != "gh" || nc.ID == old.ID {
		t.Fatalf("rotated %+v", nc)
	}
	for _, s := range []string{oldSecret, newSecret} {
		if _, err := e.resolve(s); err != nil {
			t.Fatalf("overlap: %v", err)
		}
	}
	e.now = e.now.Add(10 * time.Minute)
	if _, err := e.resolve(oldSecret); code(err) != domain.CodeUnauthenticated {
		t.Fatalf("old still valid after grace: %v", err)
	}
	if _, err := e.resolve(newSecret); err != nil {
		t.Fatal(err)
	}
}

func TestRotateErrors(t *testing.T) {
	e := setup(t)
	c, _ := e.issue(t, Spec{Kind: Ext, Scope: "c"})
	rot := func(id string, g time.Duration) error {
		return e.s.Update(ctx, func(tx *store.Tx) error { _, _, err := e.a.Rotate(tx, id, g); return err })
	}
	if code(rot("cred_nope", 0)) != domain.CodeNotFound || code(rot(c.ID, -1)) != domain.CodeInvalidRequest {
		t.Fatal("unexpected rotate errors")
	}
	_ = e.s.Update(ctx, func(tx *store.Tx) error { return e.a.Revoke(tx, c.ID) })
	if code(rot(c.ID, 0)) != domain.CodeConflict {
		t.Fatal("rotating revoked credential must conflict")
	}
}

// 已轮换的凭据（宽限期中或已过期）不能再轮换：不签发新密钥，expires_at 不变，旧密钥不会复活。
func TestRotateRejectsRotatedCredential(t *testing.T) {
	e := setup(t)
	old, oldSecret := e.issue(t, Spec{Kind: Webhook, Scope: "src-1"})
	if err := e.s.Update(ctx, func(tx *store.Tx) error { _, _, err := e.a.Rotate(tx, old.ID, 10*time.Minute); return err }); err != nil {
		t.Fatal(err)
	}
	for _, step := range []time.Duration{time.Minute, 10 * time.Minute} { // 宽限期中、已过期
		e.now = e.now.Add(step)
		err := e.s.Update(ctx, func(tx *store.Tx) error { _, _, err := e.a.Rotate(tx, old.ID, time.Hour); return err })
		if code(err) != domain.CodeConflict {
			t.Fatalf("+%v: rotating rotated credential: %v", step, err)
		}
	}
	var all []Credential
	_ = e.s.View(ctx, func(tx *store.Tx) (err error) { all, err = e.a.List(tx); return })
	if len(all) != 2 {
		t.Fatalf("new key issued: %+v", all)
	}
	for _, c := range all {
		if c.ID == old.ID && !c.ExpiresAt.Equal(time.Unix(1_700_000_000, 0).Add(10*time.Minute)) {
			t.Fatalf("expires_at moved: %v", c.ExpiresAt)
		}
	}
	if _, err := e.resolve(oldSecret); code(err) != domain.CodeUnauthenticated {
		t.Fatalf("expired key revived: %v", err)
	}
}

// 请求凭据解析成 Actor，Actor 能直接写入 Event 日志（同一事务，经 eventlog 校验）。
func TestActorResolution(t *testing.T) {
	e := setup(t)
	cases := []struct {
		sp   Spec
		want domain.Actor
	}{
		{Spec{Kind: Observer}, domain.Actor{Type: domain.ActorAgent, Via: domain.ViaCLI, Harness: "h", Session: "s1"}},
		{Spec{Kind: Worker, Executor: "exec-1"}, domain.Actor{Type: domain.ActorAgent, Principal: "exec-1", Via: domain.ViaCLI, Harness: "h", Session: "s1"}},
		{Spec{Kind: Delegate}, domain.Actor{Type: domain.ActorAgent, Via: domain.ViaCLI, Harness: "h", Session: "s1"}},
		{Spec{Kind: Ext, Scope: "chan"}, domain.Actor{Type: domain.ActorSystem, Principal: "ext:chan", Via: domain.ViaExt}},
		{Spec{Kind: Webhook, Scope: "src-9"}, domain.Actor{Type: domain.ActorSystem, Principal: "source:src-9", Via: domain.ViaSource}},
	}
	for _, tc := range cases {
		c, secret := e.issue(t, tc.sp)
		got, err := e.resolve(secret)
		if err != nil {
			t.Fatal(err)
		}
		want := tc.want
		if want.Principal == "" {
			want.Principal = c.ID // 未绑定执行者：到 token 粒度
		}
		actor := got.Actor(Origin{Harness: "h", Session: "s1"})
		if actor != want {
			t.Fatalf("%s: actor %+v want %+v", tc.sp.Kind, actor, want)
		}
		err = e.s.Update(ctx, func(tx *store.Tx) error {
			_, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemCreated, Object: "i1", Actor: actor, Payload: []byte(`{}`)})
			return err
		})
		if err != nil {
			t.Fatalf("%s actor rejected by eventlog: %v", tc.sp.Kind, err)
		}
	}
}

// 回执只接受绑定执行者的 worker、或 delegate。
func TestCanReceiptFor(t *testing.T) {
	cases := []struct {
		c    Credential
		exec string
		want bool
	}{
		{Credential{Kind: Worker, Executor: "a"}, "a", true},
		{Credential{Kind: Worker, Executor: "a"}, "b", false},
		{Credential{Kind: Worker}, "", false},
		{Credential{Kind: Delegate}, "any", true},
		{Credential{Kind: Observer}, "a", false},
		{Credential{Kind: Ext, Scope: "a", Executor: "a"}, "a", false},
	}
	for i, tc := range cases {
		if got := tc.c.CanReceiptFor(tc.exec); got != tc.want {
			t.Fatalf("case %d: got %v", i, got)
		}
	}
}

func TestParseBearer(t *testing.T) {
	for in, want := range map[string]string{"Bearer abc": "abc", "Bearer  abc ": "abc", "Basic abc": "", "abc": "", "": ""} {
		if got := ParseBearer(in); got != want {
			t.Fatalf("%q: %q", in, got)
		}
	}
}
