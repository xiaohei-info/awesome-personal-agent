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
	return func(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
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

const in = `{"source_id":"src_cal","run_id":"r1","config":{"account":"me@example.com"},"cursor":"","window":{"to":"2026-03-08T00:00:00Z"}}`

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
		OccurredAt: "2026-03-20T00:00:00Z", CorrelationKeys: []string{"uid-insurance@google.com|"}, Sender: "me@example.com",
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

// ACT-CAL-03：新建日程的 created 只到秒、updated 带毫秒，仍判为 event.created；改过之后才是 event.updated。
// 关联键是 UID|RECURRENCE-ID，同一日程的各版本共用。
func TestNewEventWithMillisecondUpdatedIsCreated(t *testing.T) {
	mk := func(updated string) event {
		_, lines, _ := do(t, fakeTool("0.9.2", `[{"id":"x","iCalUID":"u1","summary":"s","created":"2026-03-07T10:00:00.000Z","updated":"`+updated+`","start":{"dateTime":"2026-03-12T09:30:00Z"}}]`, nil, ""), in)
		return decode(t, lines)[0]
	}
	fresh, edited := mk("2026-03-07T10:00:00.537Z"), mk("2026-03-07T10:05:12.204Z")
	if fresh.Type != "event.created" || edited.Type != "event.updated" {
		t.Fatalf("fresh=%s edited=%s", fresh.Type, edited.Type)
	}
	if fresh.ID == edited.ID || len(fresh.CorrelationKeys) != 1 || fresh.CorrelationKeys[0] != "u1|" || edited.CorrelationKeys[0] != "u1|" {
		t.Fatalf("ids %q %q keys %v %v", fresh.ID, edited.ID, fresh.CorrelationKeys, edited.CorrelationKeys)
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

// 回归：updated 早于游标、但 start 在窗口内（刚进入前瞻窗口）的日程必须输出；游标只前进不回退。
func TestOldUpdatedEventsInWindowAreStillEmitted(t *testing.T) {
	version, events := recorded(t, "0.9.0")
	input := strings.Replace(in, `"cursor":""`, `"cursor":"2026-03-05T10:30:00Z"`, 1)
	_, lines, _ := do(t, fakeTool(version, events, nil, ""), input)
	if evs := decode(t, lines); len(evs) != 5 {
		t.Fatalf("窗口内全部日程都应输出, got %d", len(evs))
	}
	if lines[len(lines)-1] != `{"cursor":"2026-03-06T00:00:00Z"}` {
		t.Fatal(lines[len(lines)-1])
	}
	later := strings.Replace(in, `"cursor":""`, `"cursor":"2026-04-01T00:00:00Z"`, 1)
	_, lines, _ = do(t, fakeTool(version, events, nil, ""), later)
	if len(decode(t, lines)) != 5 || lines[len(lines)-1] != `{"cursor":"2026-04-01T00:00:00Z"}` {
		t.Fatalf("游标不得回退: %v", lines[len(lines)-1])
	}
}

// 只读调用：只调 calendar events，参数全部按 argv 传递，带免交互、顶层命令限制和账号；gog v0.9 没有 --readonly。
func TestInvokesToolReadOnlyWithArgv(t *testing.T) {
	var got []string
	ex := func(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
		if args[0] == "--version" {
			return []byte("0.9.1"), nil, nil
		}
		got = args
		return []byte(`[]`), nil, nil
	}
	input := `{"config":{"account":"me@example.com","calendar_id":"a b; rm -rf /","horizon_days":7},"window":{"to":"2026-03-08T00:00:00Z"}}`
	code, lines, _ := do(t, ex, input)
	want := "--no-input --json --enable-commands=calendar --account=me@example.com calendar events a b; rm -rf / --from 2026-03-08T00:00:00Z --to 2026-03-15T00:00:00Z"
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
		{"not installed", func(context.Context, []string, ...string) ([]byte, []byte, error) { return nil, nil, exec.ErrNotFound }, ExitAuth},
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

// 缺 account 时不调用工具，按配置失败返回 2（needs_input），提示里说明要填什么。
func TestMissingAccountNeedsInput(t *testing.T) {
	called := false
	ex := func(context.Context, []string, ...string) ([]byte, []byte, error) {
		called = true
		return nil, nil, nil
	}
	code, lines, stderr := do(t, ex, `{"config":{},"window":{"to":"2026-03-08T00:00:00Z"}}`)
	if code != ExitAuth || len(lines) != 0 || called || !strings.Contains(stderr, "account") {
		t.Fatalf("code=%d called=%v stderr=%q", code, called, stderr)
	}
}

// credentials_path 的 keyring 口令只注入给 gog 子进程（真实子进程路径），父进程同名变量不透传；
// 成功与凭据文件格式错误时，输出里都不出现口令。
func TestKeyringFromCredentialsFileReachesOnlyTool(t *testing.T) {
	const pw = "pw-s3cret-123"
	dir := t.TempDir()
	cred := filepath.Join(dir, "keyring.env")
	if err := os.WriteFile(cred, []byte("# gog keyring\nexport GOG_KEYRING_BACKEND=file\nexport GOG_KEYRING_PASSWORD='"+pw+"'\nOTHER=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n[ \"$GOG_KEYRING_PASSWORD\" = '" + pw + "' ] && [ \"$GOG_KEYRING_BACKEND\" = file ] && [ -z \"$OTHER\" ] || { echo bad keyring env >&2; exit 9; }\n" +
		"if [ \"$1\" = \"--version\" ]; then echo 'gog v0.9.0'; exit 0; fi\necho '[]'\n"
	if err := os.WriteFile(filepath.Join(dir, "gog"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/bin:/usr/bin")
	t.Setenv("GOG_KEYRING_PASSWORD", "from-parent")
	input := `{"config":{"account":"me@example.com"},"credentials_path":"` + cred + `","window":{"to":"2026-03-08T00:00:00Z"}}`
	var out, errb bytes.Buffer
	if code := Run(context.Background(), strings.NewReader(input), &out, &errb); code != ExitOK || strings.Contains(out.String()+errb.String(), pw) {
		t.Fatalf("code=%d out=%q stderr=%q", code, out.String(), errb.String())
	}

	if err := os.WriteFile(cred, []byte("GOG_KEYRING_PASSWORD "+pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errb.Reset()
	if code := Run(context.Background(), strings.NewReader(input), &out, &errb); code != ExitAuth || out.Len() != 0 || strings.Contains(errb.String(), pw) {
		t.Fatalf("malformed: code=%d out=%q stderr=%q", code, out.String(), errb.String())
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

func recordedStderr(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "0.9.0", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ACT-CAL-07 / MEAS-CAL-AUTH：账号没有令牌、keyring 口令不对，两种录制 stderr（退出码都是 1）都识别为认证失败，
// 返回 2、stdout 无内容（游标不动）；恢复后同样输入正常产出。
func TestRecordedAuthFailuresNeedInputAndRecover(t *testing.T) {
	version, events := recorded(t, "0.9.0")
	exitErr := &exec.ExitError{ProcessState: exitedState(t)}
	for _, name := range []string{"auth-no-token.stderr", "auth-bad-keyring.stderr"} {
		code, lines, stderr := do(t, fakeTool(version, "", exitErr, recordedStderr(t, name)), in)
		if code != ExitAuth || len(lines) != 0 || !strings.Contains(stderr, "认证失败") {
			t.Errorf("%s: code=%d lines=%v stderr=%q", name, code, lines, stderr)
		}
	}
	if code, lines, _ := do(t, fakeTool(version, events, nil, ""), in); code != ExitOK || len(decode(t, lines)) != 5 {
		t.Fatalf("恢复后应正常拉取: code=%d", code)
	}
}

// ACT-CAL-06：录制的 v0.1.0 版本输出在声明范围外：退出码 2、错误写明版本不符，且版本查询之后没有取数调用。
func TestVersionOutOfRangeStopsBeforeFetch(t *testing.T) {
	v, err := os.ReadFile(filepath.Join("testdata", "0.1.0", "version.txt"))
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	ex := func(_ context.Context, _ []string, args ...string) ([]byte, []byte, error) {
		calls = append(calls, args)
		return v, nil, nil
	}
	code, lines, stderr := do(t, ex, in)
	if code != ExitAuth || len(lines) != 0 || !strings.Contains(stderr, "0.1.0") || !strings.Contains(stderr, "不在声明范围") ||
		len(calls) != 1 || calls[0][0] != "--version" {
		t.Fatalf("code=%d lines=%v stderr=%q calls=%v", code, lines, stderr, calls)
	}
}

// ACT-CAL-08：口令只在子进程环境（含缺省后端 file），不进 argv；缺必填键退出码 2、不调用工具、报错不带文件内容。
func TestCredentialsInjectedViaEnvOnlyAndMissingKeyNeedsInput(t *testing.T) {
	const pw = "acc-keyring-pw"
	cred := filepath.Join(t.TempDir(), "keyring.env")
	if err := os.WriteFile(cred, []byte("GOG_KEYRING_PASSWORD="+pw+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	input := `{"config":{"account":"acc@example.com"},"credentials_path":"` + cred + `","window":{"to":"2026-03-08T00:00:00Z"}}`
	var envs, argvs []string
	ex := func(_ context.Context, env []string, args ...string) ([]byte, []byte, error) {
		envs = append(envs, env...)
		argvs = append(argvs, args...)
		if args[0] == "--version" {
			return []byte("0.9.0"), nil, nil
		}
		return []byte(`[]`), nil, nil
	}
	if code, lines, _ := do(t, ex, input); code != ExitOK || len(lines) != 1 {
		t.Fatalf("code=%d lines=%v", code, lines)
	}
	if !strings.Contains(strings.Join(envs, "\n"), "GOG_KEYRING_PASSWORD="+pw) || !strings.Contains(strings.Join(envs, "\n"), "GOG_KEYRING_BACKEND=file") ||
		strings.Contains(strings.Join(argvs, " "), pw) {
		t.Fatalf("env=%v argv=%v", envs, argvs)
	}

	if err := os.WriteFile(cred, []byte("GOG_KEYRING_BACKEND=file\nSECRET_LINE=leak-me\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	called := false
	code, lines, stderr := do(t, func(context.Context, []string, ...string) ([]byte, []byte, error) {
		called = true
		return nil, nil, nil
	}, input)
	if code != ExitAuth || len(lines) != 0 || called || !strings.Contains(stderr, "GOG_KEYRING_PASSWORD") || strings.Contains(stderr, "leak-me") {
		t.Fatalf("code=%d called=%v stderr=%q", code, called, stderr)
	}
}
