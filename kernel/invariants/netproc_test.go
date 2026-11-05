package invariants_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
)

const outcallDir = "kernel/outcall" // 唯一允许外呼的目录

var (
	bannedExact    = map[string]bool{"net": true, "syscall": true, "os/exec": true}
	bannedPrefixes = []string{"net/", "golang.org/x/net", "golang.org/x/sys", "google.golang.org/grpc", "nhooyr.io/websocket", "github.com/go-telegram"}
	parseOnly      = map[string]bool{"net/url": true, "net/netip": true} // 纯解析包，不发起外呼
)

// networkOrProcessImports 返回 root 下 kernel/ 里除外呼调用器外、import 了网络或子进程库的"文件: 包"。
func networkOrProcessImports(root fs.FS) ([]string, error) {
	var bad []string
	err := fs.WalkDir(root, "kernel", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		if p == outcallDir || strings.HasPrefix(p, outcallDir+"/") {
			return nil
		}
		src, err := fs.ReadFile(root, p)
		if err != nil {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, src, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, im := range f.Imports {
			path, _ := strconv.Unquote(im.Path.Value)
			banned := bannedExact[path]
			for _, pre := range bannedPrefixes {
				banned = banned || strings.HasPrefix(path, pre)
			}
			if banned && !parseOnly[path] {
				bad = append(bad, p+": "+path)
			}
		}
		return nil
	})
	return bad, err
}

// 不变量 10：除外呼调用器外，内核任何代码不发起网络请求，不拉起子进程。
func TestInv10OnlyOutcallReachesNetworkOrSubprocess(t *testing.T) {
	// 对照组：扫描器真的会抓到违规，也真的放过外呼调用器与纯解析包。
	probe := fstest.MapFS{
		"kernel/leak/a.go":           {Data: []byte("package leak\nimport _ \"net/http\"\n")},
		"kernel/leak/b.go":           {Data: []byte("package leak\nimport _ \"os/exec\"\n")},
		"kernel/leak/c.go":           {Data: []byte("package leak\nimport _ \"net\"\n")},
		"kernel/leak/ok.go":          {Data: []byte("package leak\nimport _ \"net/url\"\n")},
		"kernel/leak/a_test.go":      {Data: []byte("package leak\nimport _ \"net/http\"\n")},
		"kernel/outcall/proc/run.go": {Data: []byte("package proc\nimport _ \"os/exec\"\n")},
	}
	got, err := networkOrProcessImports(probe)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("scanner self-check: want 3 findings, got %v", got)
	}

	bad, err := networkOrProcessImports(os.DirFS("../.."))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad) != 0 {
		t.Fatalf("kernel code outside %s reaches the network or spawns processes:\n%s", outcallDir, strings.Join(bad, "\n"))
	}
}
