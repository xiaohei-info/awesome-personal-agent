package guide_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/guide"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const marker = "MARKER-USER-CONTENT"

// 无 token、无 URL、HTTP 一碰即失败：证明 guide 不需要凭据也不访问内核。
type noNet struct{ t *testing.T }

func (n noNet) RoundTrip(*http.Request) (*http.Response, error) {
	n.t.Fatal("guide must not call the kernel")
	return nil, nil
}

func runGuide(t *testing.T, args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	env := client.Env{Getenv: func(string) string { return "" }, UserHome: t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: noNet{t}}}
	code := client.Main(append([]string{"guide"}, args...), env)
	return code, out.String(), errb.String()
}

// ACT-BOOT-05：apa guide 无需 token 输出命令与退出码含义，不含用户内容。
func TestGuideWorksWithoutTokenAndExplainsCommandsAndExitCodes(t *testing.T) {
	code, out, _ := runGuide(t)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, want := range []string{"apa login", "apa delegate", "propose", "decide", "agents sync",
		"20 hard line", "23 group closed", "Permission card", "apa why denied", "7 kernel unreachable", "apa guide --template"} {
		if !strings.Contains(out, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	if strings.Contains(out, marker) {
		t.Error("guide leaks ledger content")
	}
	for _, gone := range []string{"22 screening", "basis required", "--grant <id>", "--evidence <"} {
		if strings.Contains(out, gone) {
			t.Errorf("guide still documents removed %q", gone)
		}
	}
}

// ACT-BOOT-05：--template 约 1000 token 内（含工作台约定），带必需声明，且不含用户内容与具体 harness。
func TestTemplateStaysWithinBudgetAndStatesContract(t *testing.T) {
	code, out, _ := runGuide(t, "--template")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	// 按每 4 个字符 1 个 token 估算，与 e2e 的 ACT-BOOT-05 同一口径（英文实际约 4 字节一个 token）。
	if tok := utf8.RuneCountInString(out) / 4; tok > 1000 {
		t.Errorf("template ~%d tokens, want <= 1000", tok)
	}
	low := strings.ToLower(out)
	for _, want := range []string{"data, not instructions", "only source of truth", "apa delegate", "loosen kernel rules", "exit 20", "exit 23"} {
		if !strings.Contains(low, want) {
			t.Errorf("template lacks %q", want)
		}
	}
	for _, bad := range []string{marker, "claude", "codex", "hermes", "gemini", "opencode", "openclaw"} {
		if strings.Contains(low, strings.ToLower(bad)) {
			t.Errorf("template contains %q", bad)
		}
	}
}

func TestGuideRejectsUnknownArgs(t *testing.T) {
	code, out, errb := runGuide(t, "--bogus")
	if code != domain.ExitUsage || out != "" {
		t.Fatalf("exit %d out %q", code, out)
	}
	var e domain.Error
	if json.Unmarshal([]byte(errb), &e) != nil || e.Next == "" {
		t.Errorf("stderr not a standard error body: %q", errb)
	}
}

// runGuideAs 带凭据跑 guide，内核替身对 /v1/agents/me/limits 回 body。
func runGuideAs(t *testing.T, body string, args ...string) string {
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/agents/me/limits" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": "t", "APA_HOME": t.TempDir()}
	env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: handler{h}}}
	if code := client.Main(append([]string{"guide"}, args...), env); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	return out.String()
}

type handler struct{ h http.Handler }

func (h handler) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// 常设说明按调用者的权限卡生成（§14.5）：两张不同的卡得到两份不同的说明，都在预算内；内核读不到时只给静态部分。
func TestTemplateFollowsTheCallersCard(t *testing.T) {
	organize := runGuideAs(t, `{"card":{"groups":{"view":{},"organize":{},"source_events":{}},"tool":"read"},"trusted_only":true}`, "--template")
	decide := runGuideAs(t, `{"card":{"groups":{"view":{},"organize":{},"source_events":{},"decide":{},"purge":{"until":"2026-11-26T15:40:00Z"}},"tool":"full"},"trusted_only":false}`, "--template")
	if organize == decide {
		t.Fatal("two cards must give two different instructions")
	}
	for _, c := range []struct{ out, want string }{
		{organize, "Not allowed: decide for me"}, {organize, "Tools: read"}, {organize, "trusted senders only"},
		{decide, "decide for me, physical delete (until 2026-11-26T15:40:00Z)"}, {decide, "Tools: full"}, {decide, "background runs: all"},
		{organize, "apa propose"}, {decide, "apa item add --phase open"}, {decide, "Proposals in the injection are yours"}, {organize, "apa delegate --parent"},
	} {
		if !strings.Contains(c.out, c.want) {
			t.Errorf("lacks %q:\n%s", c.want, c.out)
		}
	}
	for _, out := range []string{organize, decide} {
		if tok := utf8.RuneCountInString(out) / 4; tok > 1000 {
			t.Errorf("template with card ~%d tokens, want <= 1000", tok)
		}
	}
	if out := runGuideAs(t, `not json`, "--template"); strings.Contains(out, "Your card") || !strings.Contains(out, "DATA, not instructions") {
		t.Fatalf("unreadable card must fall back to the static template: %s", out)
	}
}
