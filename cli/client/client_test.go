package client

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// fake 是内核 HTTP 的测试替身：按 token 判认证，/v1/fail/{code} 按附录 B.3 返回错误体。
type fake struct {
	*httptest.Server
	mu   sync.Mutex
	seen []string // 收到的 Authorization
}

func newFake(t *testing.T, goodTokens ...string) *fake {
	f := &fake{}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Header.Get("Authorization"))
		f.mu.Unlock()
		ok := false
		for _, g := range goodTokens {
			ok = ok || r.Header.Get("Authorization") == "Bearer "+g
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.WriteHeader(401)
			_ = json.NewEncoder(w).Encode(domain.NewError(domain.CodeUnauthenticated, "credential is missing, unknown, revoked or expired", "obtain a valid credential in Web"))
			return
		}
		if rest, found := strings.CutPrefix(r.URL.Path, "/v1/fail/"); found {
			code := domain.Code(rest)
			e := domain.NewError(code, "mechanism only", "do the next thing")
			if code == domain.CodeExecutorNotAllowed {
				e.Allowed = []string{"work-host.codex"}
			}
			w.WriteHeader(code.HTTPStatus())
			_ = json.NewEncoder(w).Encode(e)
			return
		}
		switch r.URL.Path {
		case "/v1/items":
			_, _ = w.Write([]byte(`{"items":[{"id":"i1","title":"T"}]}`))
		case "/v1/html":
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`<html>secret ledger text</html>`))
		default:
			w.WriteHeader(404)
			_ = json.NewEncoder(w).Encode(domain.NewError(domain.CodeNotFound, "no such route", "check the path"))
		}
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fake) tokens() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

type run struct {
	code        int
	out, errOut string
}

func apa(t *testing.T, env map[string]string, home string, stdin string, args ...string) run {
	t.Helper()
	var out, errb bytes.Buffer
	if home != "" {
		env["APA_HOME"] = home
	}
	e := Env{Getenv: func(k string) string { return env[k] }, UserHome: t.TempDir(),
		Stdin: strings.NewReader(stdin), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient}
	code := Main(args, e)
	return run{code, out.String(), errb.String()}
}

func init() {
	Register("fail", func(c *Ctx, a []string) error { return c.Do(context.Background(), "GET", "/fail/"+a[0], nil, nil) })
	Register("items", func(c *Ctx, a []string) error {
		var v map[string]any
		if err := c.Do(context.Background(), "GET", "/items", nil, &v); err != nil {
			return err
		}
		return c.Print(v)
	})
	Register("html", func(c *Ctx, a []string) error { return c.Do(context.Background(), "GET", "/html", nil, nil) })
}

func TestLoginSavesCredentialsThenCommandsUseThem(t *testing.T) {
	f := newFake(t, "good")
	home := t.TempDir()
	r := apa(t, map[string]string{}, home, "", "login", "--url", f.URL, "--token", "good")
	if r.code != 0 || !strings.Contains(r.out, `"ok":true`) || strings.Contains(r.out, "good") {
		t.Fatalf("login: %+v", r)
	}
	st, err := os.Stat(filepath.Join(home, "credentials.json"))
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Fatalf("credential file: %v %v", st, err)
	}
	r = apa(t, map[string]string{}, home, "", "items")
	if r.code != 0 || r.out != `{"items":[{"id":"i1","title":"T"}]}`+"\n" {
		t.Fatalf("default output must be compact JSON: %+v", r)
	}
	r = apa(t, map[string]string{}, home, "", "items", "--pretty")
	if !strings.Contains(r.out, "\n  \"items\": [") {
		t.Fatalf("--pretty must indent: %q", r.out)
	}
}

func TestLoginRejectsBadTokenWithoutSaving(t *testing.T) {
	f := newFake(t, "good")
	home := t.TempDir()
	r := apa(t, map[string]string{}, home, "", "login", "--url", f.URL, "--token", "bad")
	if r.code != 3 {
		t.Fatalf("want exit 3, got %+v", r)
	}
	if _, err := os.Stat(filepath.Join(home, "credentials.json")); err == nil {
		t.Fatal("bad token must not be saved")
	}
}

func TestLoginTokenFromStdinAndEnvURL(t *testing.T) {
	f := newFake(t, "good")
	home := t.TempDir()
	r := apa(t, map[string]string{"APA_URL": f.URL}, home, "good\n", "login", "--token", "-")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
}

func TestLoginUsageErrors(t *testing.T) {
	for _, args := range [][]string{
		{"login"}, {"login", "--url", "ftp://x", "--token", "t"}, {"login", "--url", "http://h", "--bogus"},
		{"login", "--url", "http://h"}, {"login", "--url", "http://h", "--token", "t", "extra"},
	} {
		if r := apa(t, map[string]string{}, t.TempDir(), "", args...); r.code != 2 {
			t.Errorf("%v: want 2 got %+v", args, r)
		}
	}
}

func TestExitCodeMapping(t *testing.T) {
	f := newFake(t, "tok")
	env := map[string]string{"APA_URL": f.URL, "APA_TOKEN": "tok"}
	want := map[domain.Code]int{
		domain.CodeInternal: 1, domain.CodeInvalidRequest: 2, domain.CodeUnauthenticated: 3,
		domain.CodePermissionDenied: 4, domain.CodeNotFound: 5, domain.CodeConflict: 6,
		domain.CodeWebOnly: 20, domain.CodeExecutorNotAllowed: 21, domain.CodeRiskScreeningPending: 22,
	}
	for code, exit := range want {
		r := apa(t, env, t.TempDir(), "", "fail", string(code))
		if r.code != exit {
			t.Errorf("%s: want %d got %d", code, exit, r.code)
		}
		var e domain.Error
		if err := json.Unmarshal([]byte(r.errOut), &e); err != nil || e.Code != code || e.Why == "" || e.Next == "" {
			t.Errorf("%s: error body on stderr must carry code/why/next: %q", code, r.errOut)
		}
		if r.out != "" {
			t.Errorf("%s: stdout must stay empty on error", code)
		}
	}
}

func TestUnreachableIs7(t *testing.T) {
	f := newFake(t, "tok")
	url := f.URL
	f.Close()
	r := apa(t, map[string]string{"APA_URL": url, "APA_TOKEN": "tok"}, t.TempDir(), "", "items")
	if r.code != 7 || !strings.Contains(r.errOut, `"code":"unreachable"`) {
		t.Fatalf("%+v", r)
	}
}

func TestNonStandardErrorBodyIsNotEchoed(t *testing.T) {
	f := newFake(t, "tok")
	r := apa(t, map[string]string{"APA_URL": f.URL, "APA_TOKEN": "tok"}, t.TempDir(), "", "html")
	if r.code != 1 || strings.Contains(r.errOut, "secret ledger text") {
		t.Fatalf("%+v", r)
	}
}

func TestUnknownServerCodeCannotPickExitCode(t *testing.T) {
	// 服务端发来未登记的 code 时，退出码按 HTTP 状态归类。
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"code":"made_up","why":"w","next":"n"}`))
	}))
	defer srv.Close()
	r := apa(t, map[string]string{"APA_URL": srv.URL, "APA_TOKEN": "t"}, t.TempDir(), "", "items")
	if r.code != 5 {
		t.Fatalf("%+v", r)
	}
}

func TestUsageErrors(t *testing.T) {
	for _, args := range [][]string{{}, {"nope"}} {
		r := apa(t, map[string]string{}, t.TempDir(), "", args...)
		if r.code != 2 || !strings.Contains(r.errOut, `"next"`) {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

func TestNoCredentialIs3(t *testing.T) {
	r := apa(t, map[string]string{}, t.TempDir(), "", "items")
	if r.code != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestEnvOverridesSavedFile(t *testing.T) {
	f := newFake(t, "saved", "env")
	home := t.TempDir()
	apa(t, map[string]string{}, home, "", "login", "--url", f.URL, "--token", "saved")
	apa(t, map[string]string{"APA_TOKEN": "env"}, home, "", "items")
	if got := f.tokens(); got[len(got)-1] != "Bearer env" {
		t.Fatalf("env token must win: %v", got)
	}
}

func TestRunModeNeverFallsBackToSavedCredentials(t *testing.T) {
	f := newFake(t, "saved", "run")
	home := t.TempDir()
	if r := apa(t, map[string]string{}, home, "", "login", "--url", f.URL, "--token", "saved"); r.code != 0 {
		t.Fatalf("%+v", r)
	}
	n := len(f.tokens())

	// 没有运行凭据：退出 3，且不发出任何请求（更不会用 saved）。
	r := apa(t, map[string]string{"APA_RUN": "r1", "APA_URL": f.URL}, home, "", "items")
	if r.code != 3 || len(f.tokens()) != n {
		t.Fatalf("run mode must not use the saved file: %+v seen=%v", r, f.tokens())
	}
	// 运行凭据文件不可读：同样 3，不回退。
	r = apa(t, map[string]string{"APA_RUN": "r1", "APA_URL": f.URL, "APA_RUN_CREDENTIAL_FILE": filepath.Join(home, "missing")}, home, "", "items")
	if r.code != 3 || len(f.tokens()) != n {
		t.Fatalf("%+v", r)
	}
	// APA_TOKEN 作为运行凭据。
	r = apa(t, map[string]string{"APA_RUN": "r1", "APA_URL": f.URL, "APA_TOKEN": "run"}, home, "", "items")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	// 凭据文件作为运行凭据。
	cf := filepath.Join(t.TempDir(), "cred")
	_ = os.WriteFile(cf, []byte("run\n"), 0o600)
	r = apa(t, map[string]string{"APA_RUN": "r1", "APA_URL": f.URL, "APA_RUN_CREDENTIAL_FILE": cf}, home, "", "items")
	if r.code != 0 {
		t.Fatalf("%+v", r)
	}
	for _, s := range f.tokens()[n:] {
		if s != "Bearer run" {
			t.Fatalf("only the run credential may be sent, saw %q", s)
		}
	}
	// 运行身份下 APA_URL 也不从本机文件补。
	if r := apa(t, map[string]string{"APA_RUN": "r1", "APA_TOKEN": "run"}, home, "", "items"); r.code != 2 {
		t.Fatalf("%+v", r)
	}
	// 运行身份下不能 login。
	if r := apa(t, map[string]string{"APA_RUN": "r1"}, home, "", "login", "--url", f.URL, "--token", "x"); r.code != 2 {
		t.Fatalf("%+v", r)
	}
}

func TestSessionHeaderAndProject(t *testing.T) {
	var gotSession, gotHarness string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSession = r.Header.Get("X-APA-Session")
		gotHarness = r.Header.Get("X-APA-Harness")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	apa(t, map[string]string{"APA_URL": srv.URL, "APA_TOKEN": "t", "APA_SESSION": "s1", "APA_HARNESS": "h1"}, t.TempDir(), "", "items")
	if gotSession != "s1" || gotHarness != "h1" {
		t.Fatalf("session/harness headers: %q %q", gotSession, gotHarness)
	}
	apa(t, map[string]string{"APA_URL": srv.URL, "APA_TOKEN": "t"}, t.TempDir(), "", "items")
	if gotHarness != "" {
		t.Fatalf("harness header sent without APA_HARNESS: %q", gotHarness)
	}
	if (&Ctx{Env: Env{Getenv: func(string) string { return "p" }}}).Project() != "p" {
		t.Fatal("project")
	}
}

// 全局 --evidence 在任意位置、两种写法下都进 X-APA-Evidence 头，且不传给子命令。
func TestGlobalEvidenceFlagBecomesHeader(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("X-APA-Evidence"))
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	env := map[string]string{"APA_URL": srv.URL, "APA_TOKEN": "t"}
	for _, args := range [][]string{{"--evidence", "owner: ok", "items"}, {"items", "--evidence=owner: ok"}, {"items"}} {
		if r := apa(t, env, t.TempDir(), "", args...); r.code != 0 {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	if len(got) != 3 || got[0] != "owner: ok" || got[1] != "owner: ok" || got[2] != "" {
		t.Fatalf("evidence headers: %q", got)
	}
}

// 全局 --grant 两种写法都进 X-APA-Grant 头且不传给子命令；与 --evidence 同带时两个头都发，二选一由内核判定（退出码 2）。
func TestGlobalGrantFlagBecomesHeader(t *testing.T) {
	var got [][2]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, [2]string{r.Header.Get("X-APA-Grant"), r.Header.Get("X-APA-Evidence")})
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	env := map[string]string{"APA_URL": srv.URL, "APA_TOKEN": "t"}
	for _, args := range [][]string{{"--grant", "grt_1", "items"}, {"items", "--grant=grt_2"}, {"items"}, {"--grant", "grt_3", "--evidence", "ok", "items"}} {
		if r := apa(t, env, t.TempDir(), "", args...); r.code != 0 {
			t.Fatalf("%v: %+v", args, r)
		}
	}
	want := [][2]string{{"grt_1", ""}, {"grt_2", ""}, {"", ""}, {"grt_3", "ok"}}
	if len(got) != 4 || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] || got[3] != want[3] {
		t.Fatalf("grant/evidence headers: %q", got)
	}
}

// 内核的 403 basis_required 还原为退出码 23，why/next 原样交给 stderr。
func TestBasisRequiredIsExit23(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"code":"basis_required","why":"grant revoked","next":"ask the owner"}`))
	}))
	defer srv.Close()
	r := apa(t, map[string]string{"APA_URL": srv.URL, "APA_TOKEN": "t"}, t.TempDir(), "", "--grant", "g", "items")
	if r.code != 23 || !strings.Contains(r.errOut, "grant revoked") {
		t.Fatalf("%+v", r)
	}
}

func TestCorruptSavedCredentialIs3(t *testing.T) {
	home := t.TempDir()
	_ = os.WriteFile(filepath.Join(home, "credentials.json"), []byte("{"), 0o600)
	if r := apa(t, map[string]string{}, home, "", "items"); r.code != 3 {
		t.Fatalf("%+v", r)
	}
}

func TestDuplicateRegisterPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("want panic")
		}
	}()
	Register("login", login)
}
