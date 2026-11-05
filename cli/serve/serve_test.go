package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
)

type result struct {
	code        int
	out, errOut string
}

func envOf(home, userHome, stdin string, out, errb *bytes.Buffer) client.Env {
	vars := map[string]string{"APA_HOME": home}
	return client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: userHome,
		Stdin: strings.NewReader(stdin), Stdout: out, Stderr: errb, HTTP: http.DefaultClient}
}

func apa(t *testing.T, home, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := client.Main(args, envOf(home, t.TempDir(), stdin, &out, &errb))
	return result{code, out.String(), errb.String()}
}

const pw = "correct-horse"

// ACT-BOOT-01：空目录上 apa init 建立完整数据目录并设置 Web 登录，数据不落在 APA_HOME 之外。
func TestInitBuildsCompleteDataDirectoryInsideAPAHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "apa-home")
	userHome := t.TempDir()
	var out, errb bytes.Buffer
	code := client.Main([]string{"init"}, envOf(home, userHome, pw+"\n"+pw+"\n", &out, &errb))
	if code != 0 {
		t.Fatalf("init exit %d: %s", code, errb.String())
	}
	for _, name := range []string{"config.yaml", "ledger.db"} {
		if st, err := os.Stat(filepath.Join(home, name)); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	for _, name := range []string{"blobs", "packs", "sources", "runs", "logs"} {
		if st, err := os.Stat(filepath.Join(home, name)); err != nil || !st.IsDir() {
			t.Fatalf("%s/ missing: %v", name, err)
		}
	}
	if st, _ := os.Stat(filepath.Join(home, "sources")); st.Mode().Perm() != 0o700 {
		t.Fatalf("sources/ must be 0700, got %v", st.Mode().Perm())
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 {
		t.Fatalf("init wrote outside APA_HOME: %v", ents)
	}
	if ents, _ := os.ReadDir(userHome); len(ents) != 0 {
		t.Fatalf("init must not touch the user home when APA_HOME is set: %v", ents)
	}
	if strings.Contains(out.String()+errb.String(), pw+pw) || strings.Contains(out.String(), pw) {
		t.Fatalf("password leaked to stdout: %q", out.String())
	}
}

func TestInitDefaultsToDotApaUnderUserHome(t *testing.T) {
	userHome := t.TempDir()
	var out, errb bytes.Buffer
	e := envOf("", userHome, pw+"\n", &out, &errb)
	if code := client.Main([]string{"init", "--password-stdin"}, e); code != 0 {
		t.Fatalf("init exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(userHome, ".apa", "ledger.db")); err != nil {
		t.Fatal(err)
	}
}

func TestInitRejectsBadPasswordAndCreatesNoLedger(t *testing.T) {
	for name, stdin := range map[string]string{"short": "short\nshort\n", "mismatch": pw + "\nother-pass-1\n", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "h")
			r := apa(t, home, stdin, "init")
			if r.code != 2 {
				t.Fatalf("want exit 2, got %+v", r)
			}
			// 被拒绝的 init 即使留下了空账本，serve 也必须拒绝启动，而不是放行一个无法登录的内核。
			if s := apa(t, home, "", "serve"); s.code != 2 {
				t.Fatalf("serve must refuse a home without a login password: %+v", s)
			}
		})
	}
}

func TestInitKeepsEditedConfigOnRerun(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	cfg := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfg, []byte("listen: 127.0.0.1:9\nmax_subprocesses: 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), "max_subprocesses: 7") {
		t.Fatalf("rerun overwrote config.yaml: %q", b)
	}
}

func TestInitAndServeRejectUnknownArguments(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	for _, args := range [][]string{{"init", "extra"}, {"init", "--bogus"}, {"serve", "extra"}} {
		if r := apa(t, home, pw+"\n", args...); r.code != 2 {
			t.Fatalf("%v: want exit 2, got %+v", args, r)
		}
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("rejected arguments must not create the data directory")
	}
}

func TestServeRefusesUninitializedHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	r := apa(t, home, "", "serve")
	if r.code != 2 || !strings.Contains(r.errOut, "apa init") {
		t.Fatalf("want exit 2 pointing at apa init: %+v", r)
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("serve must not create the data directory")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// start 在已 init 的目录上启动 serve，返回监听地址与停止函数（停止后断言干净退出）。
func start(t *testing.T, home string) (net.Addr, func()) {
	t.Helper()
	var out, errb bytes.Buffer
	c := &client.Ctx{Env: envOf(home, t.TempDir(), "", &out, &errb)}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, c, func(a net.Addr) { ready <- a }) }()
	select {
	case a := <-ready:
		return a, func() {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("serve must stop cleanly: %v", err)
			}
		}
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not become ready")
	}
	return nil, nil
}

func login(t *testing.T, addr net.Addr, password string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	resp, err := http.Post("http://"+addr.String()+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// ACT-BOOT-02：默认配置下 serve 只监听回环；init 时设置的口令能登录，错误口令不能。
func TestServeListensOnLoopbackOnlyAndAcceptsInitPassword(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	cfg := filepath.Join(home, "config.yaml")
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), "listen: 127.0.0.1:7777") {
		t.Fatalf("default config must bind loopback: %q", b)
	}
	// 只把端口换成空闲端口（避免与本机 7777 冲突），地址部分保持 init 写出的默认值。
	patched := strings.Replace(string(b), "127.0.0.1:7777", "127.0.0.1:"+strconv.Itoa(freePort(t)), 1)
	if err := os.WriteFile(cfg, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, stop := start(t, home)
	defer stop()
	if !addr.(*net.TCPAddr).IP.IsLoopback() {
		t.Fatalf("default listen must be loopback, got %s", addr)
	}
	if got := login(t, addr, pw); got != 200 {
		t.Fatalf("init password must log in, got %d", got)
	}
	if got := login(t, addr, "wrong-password"); got != 401 {
		t.Fatalf("wrong password must be 401, got %d", got)
	}
}

func TestServeReportsBusyPortAsConflict(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen: 127.0.0.1:"+strconv.Itoa(port)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := apa(t, home, "", "serve")
	if r.code != 6 || !strings.Contains(r.errOut, "cannot listen") {
		t.Fatalf("want conflict exit 6: %+v", r)
	}
}

func TestServeRejectsInvalidConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	_ = os.WriteFile(filepath.Join(home, "config.yaml"), []byte("bogus: 1\n"), 0o644)
	if r := apa(t, home, "", "serve"); r.code != 2 {
		t.Fatalf("invalid config must fail with exit 2: %+v", r)
	}
}

// 终端式 stdin（字符设备）取不到口令时给出用法错误；没有结尾换行的最后一行也算有效输入。
func TestInitPasswordInputEdges(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	var out, errb bytes.Buffer
	e := envOf(filepath.Join(t.TempDir(), "h"), t.TempDir(), "", &out, &errb)
	e.Stdin = devnull
	if code := client.Main([]string{"init"}, e); code != 2 {
		t.Fatalf("empty terminal input must fail with exit 2, got %d", code)
	}
	home := filepath.Join(t.TempDir(), "h2")
	if r := apa(t, home, pw, "init", "--password-stdin"); r.code != 0 {
		t.Fatalf("last line without newline must be accepted: %+v", r)
	}
	if r := apa(t, home, pw+"\n"+pw, "init"); r.code != 0 { // 重复 init 更换口令，第二次输入无结尾换行
		t.Fatalf("%+v", r)
	}
}

// 终端输入时先关回显、读完（含失败）后恢复回显；用替身 stty 记录调用顺序。
func TestInitDisablesEchoWhileReadingFromTerminalAndRestoresIt(t *testing.T) {
	bin, logf := t.TempDir(), filepath.Join(t.TempDir(), "stty.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logf + "\n"
	if err := os.WriteFile(filepath.Join(bin, "stty"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	var out, errb bytes.Buffer
	e := envOf(filepath.Join(t.TempDir(), "h"), t.TempDir(), "", &out, &errb)
	e.Stdin = devnull
	client.Main([]string{"init"}, e)
	if got, _ := os.ReadFile(logf); string(got) != "-echo\necho\n" {
		t.Fatalf("stty calls = %q, want -echo then echo", got)
	}
}
