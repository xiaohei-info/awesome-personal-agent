//go:build e2e

package channel

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

func baselineNew(t *testing.T) *baseline.Env { t.Helper(); return baseline.New(t) }

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

type checkOut struct {
	OK      bool `json:"ok"`
	Reports []struct {
		Ext    string `json:"ext"`
		OK     bool   `json:"ok"`
		Checks []struct {
			Name   string `json:"name"`
			OK     bool   `json:"ok"`
			Detail string `json:"detail"`
		} `json:"checks"`
	} `json:"reports"`
}

// ACT-CHP-11：`apa ext check` 自测真实的 Telegram ext 全部通过，自测故意不幂等的 ext 被指出幂等失败。
func TestCHP11ExtCheck(t *testing.T) {
	t.Parallel()
	e := baseline.New(t)
	dir := t.TempDir()
	bins := t.TempDir()
	for name, pkg := range map[string]string{"telegram-ext": "./ext/telegram/cmd/telegram-ext", "bad-ext": "./tests/e2e/channel/badext"} {
		cmd := exec.Command("go", "build", "-tags", "e2e", "-o", filepath.Join(bins, name), pkg)
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", pkg, err, out)
		}
	}
	for ext, bin := range map[string]string{"telegram": "telegram-ext", "bad": "bad-ext"} {
		d := filepath.Join(dir, ext)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		yml := "kind: channel\ndescription: " + ext + "\ncommand: [" + filepath.Join(bins, bin) + "]\n"
		if err := os.WriteFile(filepath.Join(d, "ext.yaml"), []byte(yml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := func(name string) (int, checkOut) {
		cmd := exec.Command(e.Bin["apa"], "ext", "check", name, "--timeout", "25")
		cmd.Env = append(os.Environ(), "APA_HOME="+e.Home, "APA_EXT_DIR="+dir)
		out, err := cmd.Output()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		var co checkOut
		if json.Unmarshal(out, &co) != nil {
			t.Fatalf("ext check 输出不是 JSON: %s", out)
		}
		return code, co
	}

	code, good := run("telegram")
	want := []string{"claim", "ack", "idempotent", "stale-version", "answer"}
	if code != 0 || !good.OK || len(good.Reports) != 1 || len(good.Reports[0].Checks) != len(want) {
		t.Fatalf("Telegram ext 自测应全部通过: exit=%d %+v", code, good)
	}
	for i, c := range good.Reports[0].Checks {
		if c.Name != want[i] || !c.OK {
			t.Errorf("检查项 %d: %+v，期望 %s 通过", i, c, want[i])
		}
	}

	code, bad := run("bad")
	if code == 0 || bad.OK || len(bad.Reports) != 1 {
		t.Fatalf("坏 ext 不应通过自测: exit=%d %+v", code, bad)
	}
	idem := false
	for _, c := range bad.Reports[0].Checks {
		if c.Name == "idempotent" {
			idem = !c.OK
		}
	}
	if !idem {
		t.Errorf("坏 ext 的 idempotent 项应失败: %+v", bad.Reports[0].Checks)
	}
}
