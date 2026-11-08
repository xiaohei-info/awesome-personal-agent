package guide_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

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
		"20 web_only", "22 screening pending", "23 basis required", "--grant <id>", "7 kernel unreachable", "apa guide --template"} {
		if !strings.Contains(out, want) {
			t.Errorf("guide lacks %q", want)
		}
	}
	if strings.Contains(out, marker) {
		t.Error("guide leaks ledger content")
	}
}

// ACT-BOOT-05：--template 约 600 token 内，带三条必需声明，且不含用户内容与具体 harness。
func TestTemplateStaysWithinBudgetAndStatesContract(t *testing.T) {
	code, out, _ := runGuide(t, "--template")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	// 保守估算：每 3 字节 1 token（英文实际约 4 字节）。
	if tok := len(out) / 3; tok > 600 {
		t.Errorf("template ~%d tokens, want <= 600", tok)
	}
	low := strings.ToLower(out)
	for _, want := range []string{"data, not instructions", "only source of truth", "apa delegate", "loosen kernel rules", "--grant", "exit 23"} {
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
