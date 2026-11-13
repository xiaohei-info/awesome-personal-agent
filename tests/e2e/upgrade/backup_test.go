//go:build e2e

// Package upgrade 验收 flow-backup-restore（ACT-BAK-04、ACT-BAK-06、ACT-BAK-09）：真实 apa 二进制，
// 备份、校验、恢复到空目录，恢复出的目录能被 apa serve 启动；--no-secrets 恢复后 apa doctor 持续提示补齐密钥。
package upgrade

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

func apa(t *testing.T, e *baseline.Env, want int, args ...string) baseline.Result {
	t.Helper()
	r := e.Apa("", "", args...)
	if r.Code != want {
		t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func put(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRestore(t *testing.T) {
	e := baseline.New(t)
	e.Init()
	put(t, filepath.Join(e.Home, "sources", "src_mail", "cred"), "TOKEN")
	put(t, filepath.Join(e.Home, "webhook-keys", "hook"), "KEY")
	put(t, filepath.Join(e.Home, "sources.json"), `{"sources":[{"id":"src_mail","command":"/bin/true","schedule":"24h","timeout":30,"credentials_path":"`+
		filepath.Join(e.Home, "sources", "src_mail", "cred")+`"}]}`)
	e.Serve()
	e.StopKernel()

	t.Chdir(t.TempDir()) // 备份与恢复目标都用相对路径
	t.Run("ACT-BAK-04 relative backup path", func(t *testing.T) {
		apa(t, e, 0, "backup", "create", "--out", "relbk")
		r := apa(t, e, 0, "backup", "verify", "relbk")
		if !strings.Contains(r.Stdout, `"hashes":"match"`) {
			t.Fatalf("verify: %s", r.Stdout)
		}
	})
	t.Run("ACT-BAK-06 restored home with credentials_path serves", func(t *testing.T) {
		apa(t, e, 0, "backup", "restore", "relbk", "--home", "relhome")
		abs, _ := filepath.Abs("relhome")
		b, _ := os.ReadFile(filepath.Join(abs, "sources.json"))
		if !strings.Contains(string(b), filepath.Join(abs, "sources", "src_mail", "cred")) {
			t.Fatalf("credentials_path not rebased: %s", b)
		}
		e2 := baseline.New(t)
		e2.Home = abs
		e2.Serve() // 启动失败（退出码 2）会让 Serve 直接失败
		e2.StopKernel()
	})
	t.Run("ACT-BAK-09 no-secrets restore is flagged by doctor", func(t *testing.T) {
		apa(t, e, 0, "backup", "create", "--no-secrets", "--out", "nosec")
		apa(t, e, 0, "backup", "restore", "nosec", "--home", "nohome")
		abs, _ := filepath.Abs("nohome")
		e3 := baseline.New(t)
		e3.Home = abs
		r := apa(t, e3, 0, "doctor", "--json")
		for _, want := range []string{"the source credentials are missing", "the webhook receive key is missing"} {
			if !strings.Contains(r.Stdout, want) {
				t.Errorf("doctor does not flag %q:\n%s", want, r.Stdout)
			}
		}
	})
}
