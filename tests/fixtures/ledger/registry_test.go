package ledgerfixture

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

var modIdent = regexp.MustCompile(`\b([a-z][A-Za-z0-9]*)\.Module\b`)

func moduleIdents(t *testing.T, path, from, to string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	i := strings.Index(s, from)
	if i < 0 {
		t.Fatalf("%s: 找不到 %q", path, from)
	}
	s = s[i:]
	if j := strings.Index(s, to); j > 0 {
		s = s[:j]
	}
	var out []string
	for _, m := range modIdent.FindAllStringSubmatch(s, -1) {
		out = append(out, m[1])
	}
	sort.Strings(out)
	return out
}

// 夹具登记的模块必须和 apa serve / apa init 装配的同一份：少一个，夹具建出的结构就和真实账本对不上。
func TestModulesMatchTheKernelRegistry(t *testing.T) {
	serve := moduleIdents(t, filepath.Join("..", "..", "..", "cli", "serve", "serve.go"), "func registry()", "func serveCmd")
	mine := moduleIdents(t, "registry.go", "var Modules", "// Latest")
	if len(serve) < 30 {
		t.Fatalf("从 serve.go 只解析出 %d 个模块", len(serve))
	}
	if strings.Join(serve, ",") != strings.Join(mine, ",") {
		t.Errorf("模块清单漂移:\n  serve.go: %v\n  夹具:     %v", serve, mine)
	}
}

func TestMigrationFilesAreRealPaths(t *testing.T) {
	_, m := gen(t, 2)
	root := filepath.Join("..", "..", "..")
	for _, f := range m.MigrationFiles {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil || !strings.Contains(string(b), "store.Module{Name:") {
			t.Errorf("migration_files 里的 %s 不是迁移文件 (%v)", f, err)
		}
	}
	if len(m.MigrationFiles) < 30 {
		t.Errorf("只列出 %d 个迁移文件", len(m.MigrationFiles))
	}
}

// 命令行入口：缺省 -version 为每个已冻结版本各生成一份，并打印 export 行（验收的 LEDGER_FIXTURE_V<n>）。
func TestGenCommandWritesEveryFrozenFixture(t *testing.T) {
	out := filepath.Join(t.TempDir(), "fx")
	b, err := exec.Command("go", "run", "./cmd/gen", "-out", out).CombinedOutput()
	if err != nil {
		t.Fatalf("gen: %v\n%s", err, b)
	}
	for v := range Frozen {
		dir := filepath.Join(out, fmt.Sprintf("V%d", v))
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
			t.Errorf("V%d 没有 manifest.json: %v", v, err)
		}
		if !strings.Contains(string(b), fmt.Sprintf("export LEDGER_FIXTURE_V%d=%s", v, dir)) {
			t.Errorf("没有打印 V%d 的 export 行:\n%s", v, b)
		}
	}
	if b, err := exec.Command("go", "run", "./cmd/gen").CombinedOutput(); err == nil {
		t.Errorf("缺 -out 应失败:\n%s", b)
	}
}
