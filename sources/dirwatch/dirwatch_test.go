package dirwatch

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type outcome struct {
	exit   int
	events []map[string]any
	cursor string
	stderr string
}

func run(t *testing.T, dir, cursor string) outcome {
	t.Helper()
	var in Input
	in.Cursor = cursor
	in.Config.Dir = dir
	b, _ := json.Marshal(in)
	var so, se bytes.Buffer
	o := outcome{exit: Run(bytes.NewReader(b), &so, &se), stderr: se.String()}
	lines := strings.Split(strings.TrimSpace(so.String()), "\n")
	if o.exit != ExitOK {
		if so.Len() != 0 {
			t.Fatalf("failure must not write stdout: %q", so.String())
		}
		return o
	}
	last := lines[len(lines)-1]
	var c struct{ Cursor string }
	if err := json.Unmarshal([]byte(last), &c); err != nil || c.Cursor == "" {
		t.Fatalf("last line is not a cursor: %q", last)
	}
	o.cursor = c.Cursor
	for _, l := range lines[:len(lines)-1] {
		var e map[string]any
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		for _, f := range []string{"id", "type", "subject", "occurred_at"} {
			if e[f] == nil || e[f] == "" {
				t.Fatalf("event missing %s: %v", f, e)
			}
		}
		o.events = append(o.events, e)
	}
	return o
}

func write(t *testing.T, p, s string, mt time.Time) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mt, mt)
}

// ACT-DIR-01：新建和修改的文件转成标准事件；未变化的不重复；游标推进。
func TestNewAndModifiedFilesBecomeEvents(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	write(t, filepath.Join(dir, "a.txt"), "a", t0)
	write(t, filepath.Join(dir, "sub/b.txt"), "b", t0)

	first := run(t, dir, "")
	if first.exit != 0 || len(first.events) != 2 {
		t.Fatalf("first run: %+v", first)
	}
	if first.events[0]["type"] != "file.changed" || first.events[0]["subject"] != "a.txt" || first.events[1]["subject"] != "sub/b.txt" {
		t.Fatalf("unexpected events: %v", first.events)
	}
	if first.events[0]["occurred_at"] != "2026-01-01T00:00:00Z" {
		t.Fatalf("occurred_at should be mtime: %v", first.events[0])
	}
	if again := run(t, dir, first.cursor); len(again.events) != 0 || again.exit != 0 || again.cursor != first.cursor {
		t.Fatalf("unchanged dir must be silent with same cursor: %+v", again)
	}

	write(t, filepath.Join(dir, "a.txt"), "a2", t0.Add(time.Hour))
	write(t, filepath.Join(dir, "c.txt"), "c", t0.Add(2*time.Hour))
	os.Remove(filepath.Join(dir, "sub/b.txt"))
	third := run(t, dir, first.cursor)
	if len(third.events) != 2 || third.events[0]["subject"] != "a.txt" || third.events[1]["subject"] != "c.txt" {
		t.Fatalf("modified/created: %v", third.events)
	}
	if third.events[0]["id"] == first.events[0]["id"] {
		t.Fatal("modification must have a distinct stable id")
	}
	if rerun := run(t, dir, first.cursor); rerun.events[0]["id"] != third.events[0]["id"] {
		t.Fatal("event id not stable")
	}
}

// 数百个文件：每次 stdout 与游标都有界，多轮后每个文件恰好出现一次（含同一 mtime 的并列文件）。
func TestBoundedOutputAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const n = 600
	for i := 0; i < n; i++ {
		// 每 3 个文件共用一个 mtime，覆盖水位线上的并列。
		write(t, filepath.Join(dir, "d", strings.Repeat("x", 60)+"-"+strconv.Itoa(i)+".txt"), "x", t0.Add(time.Duration(i/3)*time.Second))
	}
	seen := map[string]int{}
	cursor, rounds := "", 0
	for ; rounds < 50; rounds++ {
		var in Input
		in.Cursor, in.Config.Dir = cursor, dir
		b, _ := json.Marshal(in)
		var so, se bytes.Buffer
		if code := Run(bytes.NewReader(b), &so, &se); code != 0 {
			t.Fatalf("exit %d: %s", code, se.String())
		}
		if so.Len() >= 48<<10+1024 {
			t.Fatalf("stdout %d bytes exceeds budget", so.Len())
		}
		o := run(t, dir, cursor)
		if len(o.cursor) > 512 {
			t.Fatalf("cursor grows: %d", len(o.cursor))
		}
		if len(o.events) == 0 {
			break
		}
		for _, e := range o.events {
			seen[e["subject"].(string)]++
		}
		cursor = o.cursor
	}
	if rounds < 2 {
		t.Fatalf("expected output to be split across runs, took %d", rounds)
	}
	if len(seen) != n {
		t.Fatalf("saw %d of %d files", len(seen), n)
	}
	for f, c := range seen {
		if c != 1 {
			t.Fatalf("%s emitted %d times", f, c)
		}
	}
}

// ACT-DIR-02：目录不存在返回配置失败（退出码 2），不输出事件和游标。
func TestMissingDirIsConfigFailure(t *testing.T) {
	o := run(t, filepath.Join(t.TempDir(), "nope"), "")
	if o.exit != ExitBadConfig || !strings.Contains(o.stderr, "不存在") {
		t.Fatalf("got %+v", o)
	}
}

func TestBadConfigAndInput(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "file")
	write(t, f, "x", time.Now())
	for name, d := range map[string]string{"empty": "", "relative": "rel/dir", "not-a-dir": f} {
		if o := run(t, d, ""); o.exit != ExitBadConfig {
			t.Errorf("%s: exit %d", name, o.exit)
		}
	}
	var so, se bytes.Buffer
	if code := Run(strings.NewReader("not json"), &so, &se); code != ExitBadConfig {
		t.Fatalf("bad stdin exit %d", code)
	}
}

func TestCorruptCursorTreatedAsFirstRun(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a"), "a", time.Now())
	if o := run(t, dir, "{garbage"); o.exit != 0 || len(o.events) != 1 {
		t.Fatalf("%+v", o)
	}
}

func TestUnreadableSubdirIsTemporary(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores permissions")
	}
	dir := t.TempDir()
	sub := filepath.Join(dir, "locked")
	write(t, filepath.Join(sub, "x"), "x", time.Now())
	os.Chmod(sub, 0)
	defer os.Chmod(sub, 0o755)
	if o := run(t, dir, ""); o.exit != ExitTemporary {
		t.Fatalf("exit %d", o.exit)
	}
}
