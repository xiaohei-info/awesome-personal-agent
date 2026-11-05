package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = func() time.Time { return time.Date(2026, 3, 8, 0, 0, 0, 0, time.UTC) }

// fakeTool 返回录制输出；--version 与 events 分别应答。
func fakeTool(version, events string, evErr error, evStderr string) execFn {
	return func(_ context.Context, args ...string) ([]byte, []byte, error) {
		if len(args) == 1 && args[0] == "--version" {
			return []byte(version), nil, nil
		}
		return []byte(events), []byte(evStderr), evErr
	}
}

func do(t *testing.T, ex execFn, input string) (int, []string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), strings.NewReader(input), &out, &errb, ex, fixedNow)
	var lines []string
	if s := strings.TrimSpace(out.String()); s != "" {
		lines = strings.Split(s, "\n")
	}
	return code, lines, errb.String()
}

func recorded(t *testing.T, ver string) (version, events string) {
	t.Helper()
	v, err := os.ReadFile(filepath.Join("testdata", ver, "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	e, err := os.ReadFile(filepath.Join("testdata", ver, "events.json"))
	if err != nil {
		t.Fatal(err)
	}
	return string(v), string(e)
}

func decode(t *testing.T, lines []string) []event {
	t.Helper()
	var evs []event
	for _, l := range lines[:len(lines)-1] {
		var e event
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatal(err)
		}
		evs = append(evs, e)
	}
	return evs
}

const in = `{"source_id":"src_cal","run_id":"r1","config":{},"cursor":"","window":{"to":"2026-03-08T00:00:00Z"}}`

// ACT-CAL-05：每个声明的版本都有录制输出，转换结果有断言。
func TestRecordedSamplesCoverEveryDeclaredVersionAndConvert(t *testing.T) {
	if minVersion != [3]int{0, 9, 0} || maxVersion != [3]int{0, 10, 0} {
		t.Fatalf("声明范围变了，先给新版本加录制样本再放宽: %v %v", minVersion, maxVersion)
	}
	version, events := recorded(t, "0.9.0")
	code, lines, stderr := do(t, fakeTool(version, events, nil, ""), in)
	if code != ExitOK {
		t.Fatalf("exit=%d %s", code, stderr)
	}
	evs := decode(t, lines)
	if len(evs) != 5 {
		t.Fatalf("events=%d", len(evs))
	}
	want := event{
		ID: "uid-insurance@google.com||2026-03-01T08:00:00.000Z", Type: "event.created", Subject: "保险到期日",
		Summary: "2026-03-20", Detail: "续保前联系经纪人", Link: "https://calendar.google.com/event?eid=a1",
		OccurredAt: "2026-03-20T00:00:00Z", CorrelationKeys: []string{"cal:uid-insurance@google.com"}, Sender: "me@example.com",
	}
	if g := evs[0]; g.ID != want.ID || g.Type != want.Type || g.Subject != want.Subject || g.Summary != want.Summary ||
		g.Detail != want.Detail || g.Link != want.Link || g.OccurredAt != want.OccurredAt || g.Sender != want.Sender ||
		len(g.CorrelationKeys) != 1 || g.CorrelationKeys[0] != want.CorrelationKeys[0] {
		t.Fatalf("got %+v", g)
	}
	if evs[1].Type != "event.updated" || evs[1].Summary != "2026-03-12T09:30:00+08:00 @ 市口腔医院" || evs[1].OccurredAt != "2026-03-12T01:30:00Z" {
		t.Fatalf("updated event %+v", evs[1])
	}
	if evs[4].Type != "event.cancelled" {
		t.Fatalf("cancelled event %+v", evs[4])
	}
	if strings.Contains(strings.Join(lines, "\n"), "sender_verified") {
		t.Fatal("工具无可核实结论，不得设 sender_verified")
	}
	if lines[len(lines)-1] != `{"cursor":"2026-03-06T00:00:00Z"}` {
		t.Fatalf("cursor line %s", lines[len(lines)-1])
	}
}

// ACT-CAL-03：重复日程每个实例 id 不同；同一日程更新后得到新 id，未更新时 id 不变。
func TestEventIDDistinguishesInstancesAndUpdates(t *testing.T) {
	version, events := recorded(t, "0.9.0")
	_, lines, _ := do(t, fakeTool(version, events, nil, ""), in)
	evs := decode(t, lines)
	if evs[2].ID == evs[3].ID || !strings.HasPrefix(evs[2].ID, "uid-weekly@google.com|2026-03-10T09:00:00+08:00|") ||
		!strings.HasPrefix(evs[3].ID, "uid-weekly@google.com|2026-03-17T09:00:00+08:00|") {
		t.Fatalf("instances: %q %q", evs[2].ID, evs[3].ID)
	}
	_, again, _ := do(t, fakeTool(version, events, nil, ""), in)
	if decode(t, again)[1].ID != evs[1].ID {
		t.Fatal("未更新的日程 id 必须稳定")
	}
	bumped := strings.Replace(events, "2026-03-05T10:30:00.000Z", "2026-03-07T11:00:00.000Z", 1)
	_, after, _ := do(t, fakeTool(version, bumped, nil, ""), in)
	if g := decode(t, after)[1]; g.ID == evs[1].ID || !strings.HasPrefix(g.ID, "uid-dentist@google.com||") {
		t.Fatalf("更新应产生同 UID 的新 id: %q", g.ID)
	}
}

// 缺少更新时间时用内容摘要区分更新。
func TestContentDigestMarksUpdateWithoutUpdatedField(t *testing.T) {
	mk := func(sum string) string {
		return `[{"id":"x","iCalUID":"u1","summary":"` + sum + `","start":{"dateTime":"2026-03-12T09:30:00Z"}}]`
	}
	id := func(sum string) string {
		_, lines, _ := do(t, fakeTool("0.9.2", mk(sum), nil, ""), in)
		return decode(t, lines)[0].ID
	}
	a, b := id("原标题"), id("新标题")
	if a == b || id("原标题") != a || !strings.HasPrefix(a, "u1||sha256:") {
		t.Fatalf("%q %q", a, b)
	}
}

// 游标：只输出更新时间不早于游标的日程，游标推进到最大更新时间。
func TestCursorFiltersOlderUpdates(t *testing.T) {
	version, events := recorded(t, "0.9.0")
	input := strings.Replace(in, `"cursor":""`, `"cursor":"2026-03-05T10:30:00Z"`, 1)
	_, lines, _ := do(t, fakeTool(version, events, nil, ""), input)
	evs := decode(t, lines)
	if len(evs) != 2 || evs[0].Type != "event.updated" || evs[1].Type != "event.cancelled" {
		t.Fatalf("events %+v", evs)
	}
	if lines[len(lines)-1] != `{"cursor":"2026-03-06T00:00:00Z"}` {
		t.Fatal(lines[len(lines)-1])
	}
}

// 只读调用：参数全部按 argv 传递，带只读与免交互开关。
func TestInvokesToolReadOnlyWithArgv(t *testing.T) {
	var got []string
	ex := func(_ context.Context, args ...string) ([]byte, []byte, error) {
		if args[0] == "--version" {
			return []byte("0.9.1"), nil, nil
		}
		got = args
		return []byte(`[]`), nil, nil
	}
	input := `{"config":{"calendar_id":"a b; rm -rf /","horizon_days":7},"window":{"to":"2026-03-08T00:00:00Z"}}`
	code, lines, _ := do(t, ex, input)
	want := "--readonly --no-input --json calendar events a b; rm -rf / --from 2026-03-08T00:00:00Z --to 2026-03-15T00:00:00Z"
	if code != ExitOK || strings.Join(got, " ") != want || len(lines) != 1 || lines[0] != `{"cursor":"2026-03-08T00:00:00Z"}` {
		t.Fatalf("code=%d args=%q lines=%v", code, got, lines)
	}
}

// ACT-CAL-04：未安装、版本越界、认证失败、输出无法解析都返回 2，且 stdout 无内容（游标不动）；临时错误返回 1。
func TestToolFailuresMapToExitCodesWithoutCursor(t *testing.T) {
	_, events := recorded(t, "0.9.0")
	exitErr := &exec.ExitError{ProcessState: exitedState(t)}
	cases := []struct {
		name string
		ex   execFn
		want int
	}{
		{"not installed", func(context.Context, ...string) ([]byte, []byte, error) { return nil, nil, exec.ErrNotFound }, ExitAuth},
		{"version too new", fakeTool("v0.10.0", events, nil, ""), ExitAuth},
		{"version too old", fakeTool("v0.8.9", events, nil, ""), ExitAuth},
		{"version unrecognised", fakeTool("gog dev", events, nil, ""), ExitAuth},
		{"auth revoked", fakeTool("0.9.0", "", exitErr, "error: token revoked, run gog auth login"), ExitAuth},
		{"unparsable output", fakeTool("0.9.0", "<html>oops</html>", nil, ""), ExitAuth},
		{"network error", fakeTool("0.9.0", "", exitErr, "dial tcp: i/o timeout"), ExitTemporary},
		{"bad config", fakeTool("0.9.0", events, nil, ""), ExitAuth},
	}
	for _, c := range cases {
		input := in
		if c.name == "bad config" {
			input = `{"cursor":"yesterday"}`
		}
		code, lines, stderr := do(t, c.ex, input)
		if code != c.want || len(lines) != 0 || stderr == "" {
			t.Errorf("%s: code=%d lines=%v stderr=%q", c.name, code, lines, stderr)
		}
	}
	if code, _, _ := do(t, fakeTool("0.9.0", events, nil, ""), `not json`); code != ExitAuth {
		t.Errorf("bad stdin: %d", code)
	}
}

// 真实子进程路径：PATH 里没有 gog 时返回 2；有可执行的假工具时端到端产出事件。环境只放行白名单。
func TestRealSubprocessPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var out, errb bytes.Buffer
	if code := Run(context.Background(), strings.NewReader(in), &out, &errb); code != ExitAuth || out.Len() != 0 {
		t.Fatalf("missing tool: code=%d out=%q", code, out.String())
	}

	dir := t.TempDir()
	script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 'gog v0.9.4'; exit 0; fi\n" +
		"[ -z \"$SECRET_TOKEN\" ] || { echo leaked >&2; exit 9; }\n" +
		"echo '{\"events\":[{\"id\":\"e\",\"iCalUID\":\"u\",\"summary\":\"s\",\"updated\":\"2026-03-01T00:00:00Z\",\"start\":{\"date\":\"2026-03-20\"}}]}'\n"
	if err := os.WriteFile(filepath.Join(dir, "gog"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv("SECRET_TOKEN", "x")
	out.Reset()
	errb.Reset()
	if code := Run(context.Background(), strings.NewReader(in), &out, &errb); code != ExitOK {
		t.Fatalf("code=%d %s", code, errb.String())
	}
	if !strings.Contains(out.String(), `"id":"u||2026-03-01T00:00:00Z"`) {
		t.Fatalf("out=%s", out.String())
	}
}

func TestWhitelistEnv(t *testing.T) {
	got := whitelistEnv([]string{"PATH=/bin", "HOME=/h", "XDG_CONFIG_HOME=/c", "SECRET=1", "APA_TOKEN=2"})
	if strings.Join(got, ",") != "PATH=/bin,HOME=/h,XDG_CONFIG_HOME=/c" {
		t.Fatal(got)
	}
}

func TestToolTimeoutIsTemporary(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := toolErr(ctx, errors.New("signal: killed"), nil)
	var re *runErr
	if !errors.As(err, &re) || re.exit != ExitTemporary {
		t.Fatal(err)
	}
}

func exitedState(t *testing.T) *os.ProcessState {
	t.Helper()
	cmd := exec.Command("sh", "-c", "exit 3")
	_ = cmd.Run()
	return cmd.ProcessState
}
