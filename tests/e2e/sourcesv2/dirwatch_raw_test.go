//go:build e2e

package sourcesv2

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlowDirWatchRaw 覆盖 ACT-OAD-04 的 dir-watch 部分：事件照旧只有路径、修改时间与大小，
// link 指向文件本身，文件内容不进账本（附录 D.8）。
func TestFlowDirWatchRaw(t *testing.T) {
	dir := programs(t)
	build := exec.Command("go", "build", "-o", filepath.Join(dir, "dir-watch"), "./sources/dirwatch/cmd/dir-watch")
	build.Dir = repoRoot()
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build dir-watch: %v\n%s", err, out)
	}
	k := setup(t)
	const src, marker = "src_dirwatch_raw", "DIRWATCH-FILE-CONTENT-MARKER"
	watch := filepath.Join(t.TempDir(), "watch")
	if err := os.MkdirAll(watch, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(watch, "note.md")
	if err := os.WriteFile(file, []byte(marker), 0o644); err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-time.Hour)
	if err := os.Chtimes(file, at, at); err != nil {
		t.Fatal(err)
	}
	k.step("ACT-OAD-04 dir-watch 事件只有路径、修改时间与大小，link 指向文件，内容不入账", func(t *testing.T) {
		k.webSource(map[string]any{"id": src, "name": src, "type": "script", "command": filepath.Join(dir, "dir-watch"), "schedule": "1s", "timeout": 20,
			"source_type": "dir", "dedupe_domain": "acc-" + src, "config": map[string]any{"dir": watch}})
		k.until("文件事件入账", func() bool { return len(k.events(src)) == 1 })
		ev := k.events(src)[0]
		if ev.Type != "file.changed" || ev.Subject != "note.md" || !strings.HasPrefix(ev.DedupeKey.EventID, "changed:note.md:") ||
			!strings.HasSuffix(ev.DedupeKey.EventID, ":"+"28") {
			t.Errorf("事件应带路径、mtime、大小: %+v", ev)
		}
		for name, body := range map[string]string{"事件": k.get("/v1/sources/" + src + "/events?limit=500"), "原文": k.webOK("GET", "/source-events/"+ev.ID, "", 200)} {
			if strings.Contains(body, marker) {
				t.Errorf("文件内容不应进账本（%s）: %s", name, body)
			}
			if name == "原文" && !strings.Contains(body, "file://"+file) {
				t.Errorf("link 应指向文件 file://%s: %s", file, body)
			}
		}
	})
}
