package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// write 在临时仓库里按相对路径写入 fixture 文件。
func write(t *testing.T, root, rel, body string) {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func rules(t *testing.T, root string) []string {
	t.Helper()
	vs, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, v := range vs {
		out = append(out, v.Rule+" "+v.File)
	}
	return out
}

func has(got []string, want string) bool {
	for _, g := range got {
		if g == want {
			return true
		}
	}
	return false
}

func TestCleanRepoPasses(t *testing.T) {
	root := t.TempDir()
	// 自身模块路径含 github，不得被当作产品名；非 kernel 区域和测试文件不受 kernel 规则约束。
	write(t, root, "kernel/ledger/ledger.go", `package ledger

import (
	"database/sql"
	"net/url"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
)

var _ = sql.ErrNoRows
var _, _ = url.Parse, clock.Now
`)
	write(t, root, "kernel/ledger/ledger_test.go", "package ledger\nimport _ \"net/http/httptest\"\n")
	write(t, root, "api/api.go", "package api\nimport _ \"net/http\"\n")
	write(t, root, "plugins/hermes/plugin.yaml", "harness: hermes\n")
	write(t, root, "ext/telegram/main.go", "package main\n")
	if got := rules(t, root); len(got) != 0 {
		t.Fatalf("unexpected violations: %v", got)
	}
}

func TestKernelNetworkAndProcessImports(t *testing.T) {
	for _, imp := range []string{"net", "net/http", "net/rpc", "os/exec", "syscall", "golang.org/x/net/http2", "google.golang.org/grpc"} {
		root := t.TempDir()
		write(t, root, "kernel/ledger/x.go", "package ledger\nimport _ \""+imp+"\"\n")
		if got := rules(t, root); !has(got, "kernel-import kernel/ledger/x.go") {
			t.Errorf("import %q not caught: %v", imp, got)
		}
	}
}

func TestOutcallMayImportNetworkButNotProductNames(t *testing.T) {
	root := t.TempDir()
	write(t, root, "kernel/outcall/call.go", "package outcall\nimport (\n\t_ \"net/http\"\n\t_ \"os/exec\"\n)\n")
	if got := rules(t, root); len(got) != 0 {
		t.Fatalf("outcall must be exempt: %v", got)
	}
	write(t, root, "kernel/outcall/vendor.go", "package outcall\nconst TelegramURL = \"x\"\n")
	if got := rules(t, root); !has(got, "kernel-name kernel/outcall/vendor.go") {
		t.Fatalf("outcall still bound by name rule: %v", got)
	}
}

func TestKernelProductNames(t *testing.T) {
	cases := map[string]string{
		"ident":   "package k\nfunc sendToHermes() {}\n",
		"snake":   "package k\nvar claude_code = 1\n",
		"string":  "package k\nvar s = \"openai\"\n",
		"import":  "package k\nimport _ \"example.com/x/telegram-bot\"\n",
		"package": "package codex\n",
	}
	for name, src := range cases {
		root := t.TempDir()
		write(t, root, "kernel/m/m.go", src)
		if got := rules(t, root); !has(got, "kernel-name kernel/m/m.go") {
			t.Errorf("%s not caught: %v", name, got)
		}
	}
}

func TestNameMatchIsWordBasedAndIgnoresOwnModule(t *testing.T) {
	root := t.TempDir()
	// "Adshare" 不含单词 dsh；自身模块路径含 github，仅作为域名与前缀被忽略。
	write(t, root, "kernel/m/m.go", "package m\nvar Adshare = \"github.com/xiaohei-info/awesome-personal-agent/kernel/x\"\n")
	if got := rules(t, root); len(got) != 0 {
		t.Fatalf("false positive: %v", got)
	}
}

func TestHarnessNamesInSourcesExtPacks(t *testing.T) {
	for _, f := range []string{"sources/mail/a.go", "ext/chan/b.yaml", "packs/base/pack.yaml"} {
		root := t.TempDir()
		write(t, root, f, "target: Hermes\n")
		if got := rules(t, root); !has(got, "harness-name "+f) {
			t.Errorf("%s not caught: %v", f, got)
		}
	}
}

func TestViolationReportsLine(t *testing.T) {
	root := t.TempDir()
	write(t, root, "packs/p/p.yaml", "a: 1\nb: codex\n")
	vs, err := Check(root)
	if err != nil || len(vs) != 1 || vs[0].Line != 2 || !strings.Contains(vs[0].String(), "packs/p/p.yaml:2") {
		t.Fatalf("got %v, %v", vs, err)
	}
}

func TestUnparsableKernelFileIsAnError(t *testing.T) {
	root := t.TempDir()
	write(t, root, "kernel/m/bad.go", "package m\nfunc (")
	if _, err := Check(root); err == nil {
		t.Fatal("expected parse error, not a silent pass")
	}
}

func TestRunExitCodes(t *testing.T) {
	root := t.TempDir()
	var sb strings.Builder
	if code := Run(root, &sb); code != 0 {
		t.Fatalf("empty repo: code %d", code)
	}
	write(t, root, "kernel/m/m.go", "package m\nimport _ \"os/exec\"\n")
	sb.Reset()
	if code := Run(root, &sb); code != 1 || !strings.Contains(sb.String(), "kernel/m/m.go:2") {
		t.Fatalf("code %d out %q", code, sb.String())
	}
	if code := Run(filepath.Join(root, "nope"), &sb); code != 2 {
		t.Fatalf("missing root: code %d", code)
	}
}

func TestOversizeFilesAreStillScanned(t *testing.T) {
	pad := strings.Repeat("// pad\n", 400000) // > 2 MiB
	root := t.TempDir()
	write(t, root, "kernel/m/big.go", "package m\n"+pad+"import _ \"os/exec\"\n")
	write(t, root, "packs/p/big.yaml", pad+"x: hermes\n")
	got := rules(t, root)
	if !has(got, "kernel-import kernel/m/big.go") || !has(got, "harness-name packs/p/big.yaml") {
		t.Fatalf("oversize violation bypassed: %v", got)
	}
}

func TestCamelCaseBrandNamesAreCaught(t *testing.T) {
	for _, id := range []string{"GitHubClient", "NewOpenAI", "DingTalkBot", "WeChatPay", "OPENAIKey"} {
		root := t.TempDir()
		write(t, root, "kernel/m/m.go", "package m\nvar "+id+" = 1\n")
		if got := rules(t, root); !has(got, "kernel-name kernel/m/m.go") {
			t.Errorf("%s not caught: %v", id, got)
		}
	}
}

func TestAllDesignHarnessNames(t *testing.T) {
	for _, n := range []string{"gemini", "opencode", "openclaw", "OpenCode", "OpenClaw"} {
		root := t.TempDir()
		write(t, root, "packs/p/p.yaml", "target: "+n+"\n")
		write(t, root, "kernel/m/m.go", "package m\nvar Use"+strings.ToUpper(n[:1])+n[1:]+" = 1\n")
		got := rules(t, root)
		if !has(got, "harness-name packs/p/p.yaml") || !has(got, "kernel-name kernel/m/m.go") {
			t.Errorf("%s not caught: %v", n, got)
		}
	}
}

func TestDirectTimeNowOutsideClockIsCaught(t *testing.T) {
	cases := map[string]string{
		"call":    "package m\nimport \"time\"\nvar _ = time.Now()\n",
		"value":   "package m\nimport \"time\"\nvar f = time.Now\n",
		"since":   "package m\nimport \"time\"\nvar _ = time.Since(time.Time{})\n",
		"until":   "package m\nimport \"time\"\nvar _ = time.Until(time.Time{})\n",
		"aliased": "package m\nimport t \"time\"\nvar _ = t.Now()\n",
	}
	for name, src := range cases {
		for _, f := range []string{"kernel/m/m.go", "api/m/m.go", "cli/m/m.go"} {
			root := t.TempDir()
			write(t, root, f, src)
			if got := rules(t, root); !has(got, "time-now "+f) {
				t.Errorf("%s in %s not caught: %v", name, f, got)
			}
		}
	}
}

func TestTimeNowAllowedInClockTestsAndNonProduction(t *testing.T) {
	root := t.TempDir()
	src := "package m\nimport \"time\"\nvar _ = time.Now()\n"
	write(t, root, "kernel/clock/provider.go", src)
	write(t, root, "kernel/m/m_test.go", src)
	write(t, root, "tests/e2e/x.go", src)
	write(t, root, "tools/release/x.go", src)
	// 另一个同名标识符（非 time 包）不算。
	write(t, root, "kernel/m/ok.go", "package m\nimport \"time\"\ntype c struct{}\nfunc (c) Now() time.Time { return time.Time{} }\nvar _ = c{}.Now()\n")
	if got := rules(t, root); len(got) != 0 {
		t.Fatalf("false positive: %v", got)
	}
}
