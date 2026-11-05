package ext

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// ---- 测试用 ext：本测试二进制自身，按 EXT_MODE 扮演守规矩或违规的渠道 ext ----

func TestHelperExt(t *testing.T) {
	if os.Getenv("EXT_HELPER") != "1" {
		return
	}
	os.Exit(helperExt(os.Getenv("EXT_MODE")))
}

func helperExt(mode string) int {
	base, key := os.Getenv("APA_URL")+"/v1", os.Getenv("APA_EXT_KEY")
	call := func(method, path string, body any) (int, []byte) {
		var rdr io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rdr = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, base+path, rdr)
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return 0, nil
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, b
	}
	n := 0
	for start := time.Now(); time.Since(start) < 15*time.Second; {
		code, b := call("GET", "/deliveries/next?wait=1", nil)
		if code != 200 {
			continue
		}
		var d domain.Delivery
		_ = json.Unmarshal(b, &d)
		ref := "msg-" + d.ID // 幂等：同一投递永远得到同一个 ref
		if mode == "random-ref" {
			n++
			ref = fmt.Sprintf("msg-%s-%d", d.ID, n)
		}
		if mode == "no-ref" {
			ref = ""
		}
		report := map[string]any{"version": d.Version, "external_ref": ref}
		code, _ = call("POST", "/deliveries/"+d.ID+"/ack", report)
		for code == 500 { // 回执丢失：重发
			if mode == "random-ref" {
				n++
				report["external_ref"] = fmt.Sprintf("msg-%s-%d", d.ID, n)
			}
			code, _ = call("POST", "/deliveries/"+d.ID+"/ack", report)
		}
		if code == 409 && mode == "retry-stale" {
			call("POST", "/deliveries/"+d.ID+"/fail", map[string]any{"version": d.Version, "reason": "x"})
		}
		if code == 200 && d.Content.Title == AnswerTitle && mode != "no-answer" {
			sender := SimSender
			if mode == "bad-sender" {
				sender = "stranger"
			}
			call("POST", "/deliveries/"+d.ID+"/answer", map[string]any{"ref": ref, "sender": sender, "choice": d.Content.Options[0]})
			return 0
		}
	}
	return 3
}

// writeExt 在临时 ext 目录下登记一个 ext，并把 APA_EXT_DIR 指过去。
func writeExt(t *testing.T, name, manifestYAML string) string {
	t.Helper()
	root := t.TempDir()
	if name != "" {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name, "ext.yaml"), []byte(manifestYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv(EnvDir, root)
	return root
}

func helperManifest(mode string) string {
	return fmt.Sprintf("kind: channel\ndescription: helper\ncommand: [%q, \"-test.run=TestHelperExt\"]\n", os.Args[0]) 
}

func apa(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errb bytes.Buffer
	env := client.Env{Getenv: os.Getenv, Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb}
	code = client.Main(args, env)
	return code, out.String(), errb.String()
}

func TestListShowsExtsAndHandlesMissingDir(t *testing.T) {
	root := writeExt(t, "alpha", "kind: export\ndescription: notes sync\ncommand: [./run.sh]\n")
	os.MkdirAll(filepath.Join(root, "no-manifest"), 0o755) // 没有 ext.yaml 的目录不是 ext
	code, out, _ := apa(t, "ext", "list")
	var got struct{ Exts []Info }
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || len(got.Exts) != 1 ||
		got.Exts[0].Name != "alpha" || got.Exts[0].Kind != KindExport {
		t.Fatalf("code=%d out=%s", code, out)
	}
	if _, out, _ := apa(t, "ext", "list", "--pretty"); !strings.Contains(out, "alpha") || !strings.Contains(out, "notes sync") {
		t.Fatalf("pretty: %q", out)
	}

	t.Setenv(EnvDir, filepath.Join(t.TempDir(), "absent"))
	if code, out, _ := apa(t, "ext", "list"); code != 0 || !strings.Contains(out, `"exts":[]`) {
		t.Fatalf("missing dir: %d %s", code, out)
	}
	if _, out, _ := apa(t, "ext", "list", "--pretty"); !strings.Contains(out, "no exts") {
		t.Fatalf("pretty empty: %q", out)
	}
}

func TestListRejectsBrokenManifest(t *testing.T) {
	for name, y := range map[string]string{"bad-kind": "kind: nope\ncommand: [x]\n", "no-cmd": "kind: channel\n", "not-yaml": ":\n\t- ["} {
		writeExt(t, name, y)
		if code, _, errb := apa(t, "ext", "list"); code != domain.ExitUsage {
			t.Errorf("%s: code=%d %s", name, code, errb)
		}
	}
}

func TestRunStartsExtInItsDirWithEnvAndPropagatesFailure(t *testing.T) {
	root := writeExt(t, "echo", "kind: webhook\ncommand: [sh, -c, 'echo \"$APA_URL $(basename $PWD)\"']\n")
	t.Setenv("APA_URL", "http://kernel.test")
	code, out, _ := apa(t, "ext", "run", "echo")
	if code != 0 || strings.TrimSpace(out) != "http://kernel.test echo" {
		t.Fatalf("code=%d out=%q", code, out)
	}
	os.MkdirAll(filepath.Join(root, "boom"), 0o755)
	os.WriteFile(filepath.Join(root, "boom", "ext.yaml"), []byte("kind: export\ncommand: [sh, -c, 'exit 3']\n"), 0o644)
	if code, _, errb := apa(t, "ext", "run", "boom"); code != domain.ExitUnclassified || !strings.Contains(errb, "status 3") {
		t.Fatalf("code=%d %s", code, errb)
	}
	os.MkdirAll(filepath.Join(root, "nobin"), 0o755)
	os.WriteFile(filepath.Join(root, "nobin", "ext.yaml"), []byte("kind: export\ncommand: [./does-not-exist]\n"), 0o644)
	if code, _, _ := apa(t, "ext", "run", "nobin"); code != domain.ExitUnclassified {
		t.Fatalf("start failure code=%d", code)
	}
	if code, _, _ := apa(t, "ext", "run", "ghost"); code != domain.ExitNotFound {
		t.Fatalf("unknown ext code=%d", code)
	}
	for _, args := range [][]string{{"ext", "run"}, {"ext", "run", "../x"}, {"ext", "run", "a", "b"}, {"ext", "run", "-x"}, {"ext"}, {"ext", "wat"}, {"ext", "list", "x"}} {
		if code, _, _ := apa(t, args...); code != domain.ExitUsage && code != domain.ExitNotFound {
			t.Errorf("%v: code=%d", args, code)
		}
	}
}

func TestCheckPassesConformingExt(t *testing.T) {
	writeExt(t, "good", helperManifest(""))
	t.Setenv("EXT_HELPER", "1")
	t.Setenv("EXT_MODE", "good")
	code, out, errb := apa(t, "ext", "check", "good")
	var got struct {
		OK      bool
		Reports []Report
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != 0 || !got.OK || len(got.Reports) != 1 || len(got.Reports[0].Checks) != 5 {
		t.Fatalf("code=%d out=%s err=%s", code, out, errb)
	}
	// 无参数 = 检查全部渠道 ext
	if code, _, _ := apa(t, "ext", "check"); code != 0 {
		t.Fatalf("check all: %d", code)
	}
}

func TestCheckFlagsEachProtocolViolation(t *testing.T) {
	cases := map[string]string{
		"no-ref":      "ack",
		"random-ref":  "idempotent",
		"retry-stale": "stale-version",
		"no-answer":   "answer",
		"bad-sender":  "answer",
	}
	for mode, failing := range cases {
		t.Run(mode, func(t *testing.T) {
			writeExt(t, "bad", helperManifest(mode))
			t.Setenv("EXT_HELPER", "1")
			t.Setenv("EXT_MODE", mode)
			code, out, _ := apa(t, "ext", "check", "bad", "--timeout", "4")
			if code == 0 {
				t.Fatalf("violation not detected: %s", out)
			}
			var got struct{ Reports []Report }
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			found := false
			for _, c := range got.Reports[0].Checks {
				if c.Name == failing && !c.OK && c.Detail != "" {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s should fail: %s", failing, out)
			}
		})
	}
}

func TestCheckReportsExtThatNeverStartsOrExitsEarly(t *testing.T) {
	writeExt(t, "nobin", "kind: channel\ncommand: [./does-not-exist]\n")
	if code, out, _ := apa(t, "ext", "check", "nobin"); code == 0 || !strings.Contains(out, `"start"`) {
		t.Fatalf("code=%d %s", code, out)
	}
	writeExt(t, "quit", "kind: channel\ncommand: [sh, -c, 'exit 0']\n")
	if code, out, _ := apa(t, "ext", "check", "quit"); code == 0 || !strings.Contains(out, "exited early") {
		t.Fatalf("code=%d %s", code, out)
	}
}

func TestCheckArgumentErrors(t *testing.T) {
	writeExt(t, "", "")
	if code, _, _ := apa(t, "ext", "check"); code != domain.ExitUsage { // 没有渠道 ext
		t.Fatalf("no channel ext: %d", code)
	}
	writeExt(t, "exp", "kind: export\ncommand: [x]\n")
	for _, args := range [][]string{{"ext", "check", "exp"}, {"ext", "check", "--timeout", "0"}, {"ext", "check", "--bogus"}, {"ext", "check", "ghost"}} {
		if code, _, _ := apa(t, args...); code != domain.ExitUsage && code != domain.ExitNotFound {
			t.Errorf("%v: code=%d", args, code)
		}
	}
}

func TestSimulatorRejectsBadCalls(t *testing.T) {
	s := newSim()
	h := s.handler()
	do := func(method, path, body, auth string) int {
		req, _ := http.NewRequest(method, path, strings.NewReader(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		rec := &recorder{h: http.Header{}}
		h.ServeHTTP(rec, req)
		return rec.code
	}
	for _, c := range []struct {
		m, p, b, a string
		want       int
	}{
		{"GET", "/v1/deliveries/next", "", "", 401},
		{"GET", "/v1/deliveries/next?wait=-1", "", s.key, 400},
		{"POST", "/v1/deliveries/x/ack", `{"version":1}`, "wrong", 401},
		{"POST", "/v1/deliveries/x/ack", `{}`, s.key, 400},
		{"POST", "/v1/deliveries/nope/ack", `{"version":1,"external_ref":"r"}`, s.key, 404},
		{"POST", "/v1/deliveries/" + dAck + "/answer", `{}`, "wrong", 401},
		{"POST", "/v1/deliveries/" + dAck + "/answer", `{`, s.key, 400},
		{"POST", "/v1/deliveries/" + dAck + "/answer", `{}`, s.key, 404},
		{"POST", "/v1/deliveries/" + dAnswer + "/answer", `{"ref":"r","sender":"check-user","choice":"yes"}`, s.key, 400}, // 未送达
	} {
		if got := do(c.m, c.p, c.b, c.a); got != c.want {
			t.Errorf("%s %s: got %d want %d", c.m, c.p, got, c.want)
		}
	}
}

type recorder struct {
	h    http.Header
	code int
}

func (r *recorder) Header() http.Header { return r.h }
func (r *recorder) Write(b []byte) (int, error) {
	if r.code == 0 {
		r.code = 200
	}
	return len(b), nil
}
func (r *recorder) WriteHeader(c int) { r.code = c }
