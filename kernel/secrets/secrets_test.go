package secrets

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

func open(t *testing.T) (*store.Store, string) {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(Module); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, home
}

func put(t *testing.T, st *store.Store, v *Vault, owner, field, val string) {
	t.Helper()
	if err := st.Update(ctx, func(tx *store.Tx) error { return v.Put(tx, owner, field, val, time.Now()) }); err != nil {
		t.Fatal(err)
	}
}

func reveal(t *testing.T, st *store.Store, v *Vault, owner string, fields ...string) (m map[string]string) {
	t.Helper()
	if err := st.View(ctx, func(tx *store.Tx) (err error) { m, err = v.Reveal(tx, owner, fields); return }); err != nil {
		t.Fatal(err)
	}
	return m
}

// 密钥文件 0600、明文不进账本、密文挪到别的行解不开、只给要的字段。
func TestVaultKeepsPlaintextOutOfLedger(t *testing.T) {
	st, home := open(t)
	v, created, err := Open(ctx, st, home)
	if err != nil || !created {
		t.Fatalf("open: %v created=%v", err, created)
	}
	if fi, _ := os.Stat(filepath.Join(home, KeyFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key file mode %v", fi.Mode().Perm())
	}
	put(t, st, v, "ch-1", "token", "s3cret-token-value")
	put(t, st, v, "ch-1", "pass", "pw-value")
	if got := reveal(t, st, v, "ch-1", "token"); got["token"] != "s3cret-token-value" || len(got) != 1 {
		t.Fatalf("reveal = %v", got)
	}
	st.Close()
	raw, _ := os.ReadFile(filepath.Join(home, store.LedgerFile))
	if strings.Contains(string(raw), "s3cret-token-value") {
		t.Fatal("plaintext found in ledger file")
	}
	st2, err := store.Open(home, func() *store.Registry { r := store.NewRegistry(); r.Register(Module); return r }())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	// 把 ch-1/token 的密文挪到 ch-2/token：附加认证数据不同，解不开
	if err := st2.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO secrets_value SELECT 'ch-2', field, key_id, nonce, ciphertext, updated_at FROM secrets_value WHERE owner = 'ch-1' AND field = 'token'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := reveal(t, st2, v, "ch-2", "token"); len(got) != 0 {
		t.Fatalf("moved ciphertext decrypted: %v", got)
	}
}

// 主密钥丢失：照常打开（生成新密钥），旧行不可用；doctor 的数量能看出来。
func TestMissingKeyStartsAndMarksRowsUnusable(t *testing.T) {
	st, home := open(t)
	v, _, _ := Open(ctx, st, home)
	put(t, st, v, "ch-1", "token", "x")
	os.Remove(filepath.Join(home, KeyFile))
	v2, created, err := Open(ctx, st, home)
	if err != nil || !created || v2.ID() == v.ID() {
		t.Fatalf("reopen: %v created=%v", err, created)
	}
	var use map[string]bool
	var s Stats
	st.View(ctx, func(tx *store.Tx) (err error) {
		if use, err = v2.Usable(tx, "ch-1"); err != nil {
			return err
		}
		s, err = v2.Inspect(tx)
		return err
	})
	if use["token"] || s.Rows != 1 || s.OtherKey != 1 {
		t.Fatalf("usable=%v stats=%+v", use, s)
	}
	if got := reveal(t, st, v2, "ch-1", "token"); len(got) != 0 {
		t.Fatalf("revealed with wrong key: %v", got)
	}
}

// 换主密钥后旧密钥解不开、新密钥解得开；提交后、替换文件前中断的，下次 Open 走完。
func TestRekeyAndResumeAfterCrash(t *testing.T) {
	st, home := open(t)
	v, _, _ := Open(ctx, st, home)
	put(t, st, v, "ch-1", "token", "tok")
	n, nv, err := Rekey(ctx, st, v)
	if err != nil || n != 1 {
		t.Fatalf("rekey: n=%d err=%v", n, err)
	}
	if got := reveal(t, st, nv, "ch-1", "token"); got["token"] != "tok" {
		t.Fatalf("new key reveal = %v", got)
	}
	if got := reveal(t, st, v, "ch-1", "token"); len(got) != 0 {
		t.Fatal("old key still decrypts")
	}
	// 模拟中断：把当前密钥另存为 .new、把一个无关密钥放回原位
	key, _ := os.ReadFile(filepath.Join(home, KeyFile))
	os.WriteFile(filepath.Join(home, KeyFile+".new"), key, 0o600)
	os.WriteFile(filepath.Join(home, KeyFile), make([]byte, 32), 0o600)
	v3, _, err := Open(ctx, st, home)
	if err != nil || v3.ID() != nv.ID() {
		t.Fatalf("resume rekey: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, KeyFile+".new")); !os.IsNotExist(err) {
		t.Fatal(".new left behind")
	}
}

// Move 换属主：新属主能解开、旧属主的字段没了，未列出的字段与别的属主不受影响。
func TestMoveRebindsWithoutTouchingOthers(t *testing.T) {
	st, _ := open(t)
	v := Generate()
	put(t, st, v, "dr-1", "device_key", "dev-secret-value")
	put(t, st, v, "dr-1", "other", "left-alone")
	put(t, st, v, "ch-live", "device_key", "live-value")
	put(t, st, v, "ch-live", "auth_secret", "live-auth")
	if err := st.Update(ctx, func(tx *store.Tx) error { return v.Move(tx, "dr-1", "ch-live", []string{"device_key"}, time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if got := reveal(t, st, v, "ch-live", "device_key", "auth_secret"); got["device_key"] != "dev-secret-value" || got["auth_secret"] != "live-auth" {
		t.Fatalf("after move: %v", got)
	}
	if got := reveal(t, st, v, "dr-1", "device_key", "other"); got["device_key"] != "" || got["other"] != "left-alone" {
		t.Fatalf("source after move: %v", got)
	}
}
