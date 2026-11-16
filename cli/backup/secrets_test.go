package backup

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// apa secrets rekey：服务还在监听时拒绝；停止后换钥，旧密钥解不开、新密钥解得开。
func TestSecretsRekeyCommand(t *testing.T) {
	r := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, secrets.Module} {
		if err := r.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	UseRegistry(func() (*store.Registry, error) { return r, nil })
	home := t.TempDir()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(home, "config.yaml"), "listen: "+ln.Addr().String()+"\n", 0o644)
	st, err := store.Open(home, r)
	if err != nil {
		t.Fatal(err)
	}
	v, _, err := secrets.Open(context.Background(), st, home)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(context.Background(), func(tx *store.Tx) error { return v.Put(tx, "ch-1", "token", "tok", time.Now()) }); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if code, _, stderr := runCLI(t, home, "secrets", "rekey"); code != 6 || !strings.Contains(stderr, "stop the service") {
		t.Fatalf("rekey while serving: %d %s", code, stderr)
	}
	ln.Close()
	if code, out, stderr := runCLI(t, home, "secrets", "rekey"); code != 0 || !strings.Contains(out, `"reencrypted":1`) {
		t.Fatalf("rekey: %d %s %s", code, out, stderr)
	}
	nv, err := secrets.Load(home)
	if err != nil || nv.ID() == v.ID() {
		t.Fatalf("key not replaced: %v", err)
	}
	st, _ = store.Open(home, r)
	defer st.Close()
	var got map[string]string
	st.View(context.Background(), func(tx *store.Tx) (err error) { got, err = nv.Reveal(tx, "ch-1", []string{"token"}); return })
	if got["token"] != "tok" {
		t.Fatalf("new key reveal = %v", got)
	}
	if fi, _ := os.Stat(filepath.Join(home, secrets.KeyFile)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("key mode %o", fi.Mode().Perm())
	}
}
