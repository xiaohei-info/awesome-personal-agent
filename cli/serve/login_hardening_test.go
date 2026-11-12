package serve

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ACT-LGN-01：设置口令时拒绝少于 12 个字符，不写入；12 个字符被接受。
func TestInitRefusesElevenCharactersAndAcceptsTwelve(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	r := apa(t, home, "elevenchars\nelevenchars\n", "init")
	if r.code != 2 || !strings.Contains(r.errOut, "12") {
		t.Fatalf("11 characters must be refused with the length floor: %+v", r)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("a refused password must write nothing, stat err = %v", err)
	}
	if r := apa(t, home, "twelve-chars\ntwelve-chars\n", "init"); r.code != 0 {
		t.Fatalf("12 characters must be accepted: %+v", r)
	}
}

func ledgerEventSeqs(t *testing.T, home string) []int64 {
	t.Helper()
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var seqs []int64
	if err := st.View(context.Background(), func(tx *store.Tx) error {
		all, err := eventlog.Read(tx, 0, 100000)
		for _, en := range all {
			seqs = append(seqs, en.Event.Seq)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return seqs
}

// ACT-LGN-11：主机上重设口令，账本不动；短口令被拒；旧口令失效、新口令可登录。
func TestResetPasswordKeepsLedgerAndSwapsTheLogin(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, "uninitialised\n", "init", "--reset-password", "--password-stdin"); r.code != 2 {
		t.Fatalf("reset on an uninitialised home must fail with usage: %+v", r)
	}
	if _, err := os.Stat(home); !os.IsNotExist(err) {
		t.Fatalf("reset must not create the home: %v", err)
	}
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	before := ledgerEventSeqs(t, home)

	if r := apa(t, home, "short-11-ch\nshort-11-ch\n", "init", "--reset-password"); r.code != 2 {
		t.Fatalf("11 characters on reset must be refused: %+v", r)
	}
	if got := ledgerEventSeqs(t, home); len(got) != len(before) {
		t.Fatalf("a refused reset must not write: %v -> %v", before, got)
	}
	const fresh = "a-much-longer-new-passphrase"
	if r := apa(t, home, fresh+"\n", "init", "--reset-password", "--password-stdin"); r.code != 0 {
		t.Fatalf("reset: %+v", r)
	}
	after := ledgerEventSeqs(t, home)
	if len(after) != len(before)+1 || (len(before) > 0 && after[0] != before[0]) {
		t.Fatalf("reset may only append one event at the end: %v -> %v", before, after)
	}

	cfg := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfg, []byte("listen: 127.0.0.1:"+strconv.Itoa(freePort(t))+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, stop := start(t, home)
	defer stop()
	if got := login(t, addr, pw); got != 401 {
		t.Fatalf("old password after reset: %d", got)
	}
	if got := login(t, addr, fresh); got != 200 {
		t.Fatalf("new password after reset: %d", got)
	}
}

// ACT-DPL-07/08/09：监听非本机地址而没有 https 的 public_url 拒绝启动，端口不被占用；只监听本机不要求。
func TestServeRefusesNonLocalListenWithoutHTTPSPublicURL(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	port := freePort(t)
	cfg := func(body string) {
		if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	listen := "listen: 0.0.0.0:" + strconv.Itoa(port) + "\n"

	cfg(listen)
	r := apa(t, home, "", "serve")
	if r.code == 0 || !strings.Contains(r.errOut, "public_url") || !strings.Contains(r.errOut, "https") {
		t.Fatalf("no public_url: want refusal naming https public_url, got %+v", r)
	}
	cfg(listen + "public_url: http://apa.example.com\n")
	r = apa(t, home, "", "serve")
	if r.code == 0 || !strings.Contains(r.errOut, "https") {
		t.Fatalf("http public_url: want refusal naming https, got %+v", r)
	}
	// 两次都没有占用端口。
	l, err := net.Listen("tcp", "127.0.0.1:"+strconv.Itoa(port))
	if err != nil {
		t.Fatalf("a refused serve must not hold the port: %v", err)
	}
	l.Close()

	cfg(listen + "public_url: https://apa.example.com\n")
	addr, stop := start(t, home)
	stop()
	if addr == nil {
		t.Fatal("https public_url must be accepted")
	}
	cfg("listen: 127.0.0.1:" + strconv.Itoa(port) + "\n")
	addr, stop = start(t, home)
	defer stop()
	if got := login(t, addr, pw); got != 200 {
		t.Fatalf("loopback deployment without public_url must work: %d", got)
	}
}
