package tools_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/tools"
)

var (
	root     string // 仓库根
	stubExe  string // 构建出的 tool-stub
	adapters = map[string]string{}
)

func TestMain(m *testing.M) {
	wd, _ := os.Getwd()
	root = filepath.Join(wd, "..", "..", "..")
	dir, _ := os.MkdirTemp("", "toolstub")
	build := func(out, pkg string) string {
		p := filepath.Join(dir, out)
		cmd := exec.Command("go", "build", "-o", p, pkg)
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			os.Stderr.Write(b)
			os.Exit(2)
		}
		return p
	}
	stubExe = build("tool-stub", "./tests/doubles/tools/cmd/tool-stub")
	adapters["mail"] = build("mail-himalaya", "./sources/mail/cmd/mail-himalaya")
	adapters["calendar"] = build("calendar", "./sources/calendar/cmd/calendar")
	adapters["github"] = build("github-source", "./sources/github/cmd/github-source")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// newBin 用 `tool-stub install` 建 TOOL_STUB_BIN，返回目录。
func newBin(t *testing.T) string {
	t.Helper()
	bin := t.TempDir()
	if b, err := exec.Command(stubExe, "install", bin, root).CombinedOutput(); err != nil {
		t.Fatalf("install: %v %s", err, b)
	}
	return bin
}

func setCase(t *testing.T, bin, c string) {
	t.Helper()
	if b, err := exec.Command(stubExe, "case", bin, c).CombinedOutput(); err != nil {
		t.Fatalf("case: %v %s", err, b)
	}
}

// run 进程内调用 tools.Run（可计覆盖率）；env 只给显式列出的变量，模拟内核子进程白名单。可执行形态由 newBin 与适配器测试覆盖。
func run(t *testing.T, bin, tool string, env []string, args ...string) (string, string, int) {
	t.Helper()
	var o, e bytes.Buffer
	code := tools.Run(tool, args, env, bin, &o, &e)
	return o.String(), e.String(), code
}

func readLog(t *testing.T, path string) []tools.LogEntry {
	t.Helper()
	b, _ := os.ReadFile(path)
	var out []tools.LogEntry
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if l == "" {
			continue
		}
		var e tools.LogEntry
		if err := json.Unmarshal([]byte(l), &e); err != nil {
			t.Fatalf("坏日志行 %q", l)
		}
		out = append(out, e)
	}
	return out
}

func fixture(t *testing.T, rel ...string) string {
	b, err := os.ReadFile(filepath.Join(append([]string{root}, rel...)...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// flow-env-baseline / flow-source-mail：mail-1.1.0 回放录制输出，TOOL_LOG 每次调用新增一条，只记环境变量名。
func TestMailReplayAndLog(t *testing.T) {
	bin := newBin(t)
	setCase(t, bin, "mail-1.1.0")
	logp := filepath.Join(t.TempDir(), "tool.log")
	env := []string{"TOOL_LOG=" + logp, "HIMALAYA_SECRET=hunter2"}

	for _, tc := range []struct {
		args []string
		file string
	}{
		{[]string{"--version"}, "version.txt"},
		{[]string{"envelope", "list", "--page-size", "50", "--page", "1", "--output", "json"}, "list.json"},
		{[]string{"message", "read", "--preview", "-H", "Message-ID", "4", "--output", "json"}, "read-4.json"},
	} {
		got, _, code := run(t, bin, "himalaya", env, tc.args...)
		if want := fixture(t, "sources", "mail", "testdata", "1.1.0", tc.file); code != 0 || strings.TrimSpace(got) != strings.TrimSpace(want) {
			t.Errorf("%v: exit=%d 输出与录制样本不一致:\n%s", tc.args, code, got)
		}
	}
	entries := readLog(t, logp)
	if len(entries) != 3 || entries[0].Tool != "himalaya" || entries[0].Case != "mail-1.1.0" {
		t.Fatalf("日志条数/内容不对: %+v", entries)
	}
	if strings.Join(entries[2].Argv, " ") != "message read --preview -H Message-ID 4 --output json" {
		t.Errorf("argv 记录不对: %v", entries[2].Argv)
	}
	raw, _ := os.ReadFile(logp)
	if strings.Contains(string(raw), "hunter2") || !strings.Contains(string(raw), "HIMALAYA_SECRET") {
		t.Errorf("日志必须只记环境变量名: %s", raw)
	}
}

// 日志路径回退：TOOL_LOG 不在白名单内时写 $TOOL_STUB_BIN/tool.log；case 文件优先于环境变量。
func TestLogFallbackAndCasePrecedence(t *testing.T) {
	bin := newBin(t)
	setCase(t, bin, "gh-empty")
	_, _, code := run(t, bin, "gh", []string{"TOOL_STUB_CASE=gh-auth-401"}, "api", "/notifications")
	if code != 0 {
		t.Fatalf("case 文件应优先于 TOOL_STUB_CASE, exit=%d", code)
	}
	if e := readLog(t, filepath.Join(bin, "tool.log")); len(e) != 1 || e[0].Case != "gh-empty" {
		t.Errorf("回退日志: %+v", e)
	}
}

// 失败语义：认证失败、版本越界、case 缺失/不匹配、非只读命令都要如实失败，不返回假成功。
func TestFailureCases(t *testing.T) {
	for _, tc := range []struct {
		c, tool string
		args    []string
		code    int
		stderr  string
	}{
		{"mail-auth-fail", "himalaya", []string{"envelope", "list", "--output", "json"}, 1, "Invalid credentials"},
		{"mail-version-0.1", "himalaya", []string{"--version"}, 0, ""},
		{"mail-version-0.1", "himalaya", []string{"envelope", "list"}, 1, "out-of-range"},
		{"mail-1.1.0", "himalaya", []string{"message", "send"}, tools.ExitRefused, "只读"},
		{"cal-auth-fail", "gog", []string{"--no-input", "--json", "--enable-commands=calendar", "--account=acc@example.com", "calendar", "events", "primary"}, 1, "No auth for calendar acc@example.com"},
		{"cal-base", "gog", []string{"--account=a", "calendar", "create", "x"}, tools.ExitRefused, "只读"},
		{"gh-auth-401", "gh", []string{"api", "/repos/acc-org/acc-repo/issues"}, 1, "Bad credentials (HTTP 401)"},
		{"gh-empty", "gh", []string{"auth", "login"}, tools.ExitRefused, "只读"},
		{"cal-base", "gh", []string{"api", "/notifications"}, tools.ExitNoRecord, "没有录制"},
		{"no-such-case", "gh", []string{"api", "/notifications"}, tools.ExitNoCase, "未知 case"},
		{"pim-reminders", "mail-cli", []string{"messages"}, tools.ExitNoRecord, "没有录制"},
	} {
		bin := newBin(t)
		setCase(t, bin, tc.c)
		_, stderr, code := run(t, bin, tc.tool, nil, tc.args...)
		if code != tc.code || !strings.Contains(stderr, tc.stderr) {
			t.Errorf("%s %s %v: exit=%d stderr=%q, 期望 %d %q", tc.c, tc.tool, tc.args, code, stderr, tc.code, tc.stderr)
		}
	}
	bin := newBin(t)
	if _, stderr, code := run(t, bin, "gh", nil, "api", "/notifications"); code != tools.ExitNoCase || !strings.Contains(stderr, "case") {
		t.Errorf("没设 case 应失败: %d %q", code, stderr)
	}
}

// 最小替身自测：把 gh 替身放到 PATH 前缀，GitHub 来源适配器按 since 增量读到录制事件。
func TestGitHubAdapterAgainstStub(t *testing.T) {
	bin := newBin(t)
	creds := t.TempDir()
	pull := func(c, cursor, to string) (string, string) {
		setCase(t, bin, c)
		in, _ := json.Marshal(map[string]any{"config": map[string]any{"repos": []string{"acc-org/acc-repo"}}, "cursor": cursor,
			"credentials_path": creds, "window": map[string]string{"from": "", "to": to}})
		cmd := exec.Command(adapters["github"])
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin", "TOOL_LOG=" + filepath.Join(bin, "tool.log")}
		cmd.Stdin = bytes.NewReader(in)
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		if err := cmd.Run(); err != nil {
			t.Fatalf("%s: %v %s", c, err, e.String())
		}
		lines := strings.Split(strings.TrimSpace(o.String()), "\n")
		var ids []string
		for _, l := range lines[:len(lines)-1] {
			var ev struct{ ID string }
			json.Unmarshal([]byte(l), &ev)
			ids = append(ids, ev.ID)
		}
		var cur struct{ Cursor string }
		json.Unmarshal([]byte(lines[len(lines)-1]), &cur)
		return strings.Join(ids, ","), cur.Cursor
	}
	ids, cur := pull("gh-empty", "", "2026-11-09T00:00:00Z")
	if strings.Contains(ids, "issue-") || strings.Contains(ids, "pr-") {
		t.Errorf("gh-empty 不应有 issue/PR 事件: %s", ids)
	}
	ids, cur = pull("gh-issue-opened", cur, "2026-11-10T09:30:00Z")
	if !strings.Contains(ids, "issue-acc-org-acc-repo#11-opened") {
		t.Errorf("gh-issue-opened: %s", ids)
	}
	ids, cur = pull("gh-pr-closed", cur, "2026-11-10T13:30:00Z")
	if !strings.Contains(ids, "pr-acc-org-acc-repo#5-merged") || !strings.Contains(ids, "pr-acc-org-acc-repo#6-closed-") {
		t.Errorf("gh-pr-closed: %s", ids)
	}
	if ids, _ = pull("gh-agentsmd-changed", cur, "2026-11-10T14:30:00Z"); !strings.Contains(ids, "repo-acc-org-acc-repo-agents-md-sha-b-") {
		t.Errorf("gh-agentsmd-changed: %s", ids)
	}
	// 每次拉取都是只读的 gh api 调用
	for _, e := range readLog(t, filepath.Join(bin, "tool.log")) {
		if e.Tool != "gh" || e.Argv[0] != "api" {
			t.Errorf("非只读调用: %+v", e)
		}
	}
}

// flow-source-mail：邮件适配器经 PATH 前缀使用替身；线程 case 带 References/In-Reply-To 头。
func TestMailAdapterAgainstStub(t *testing.T) {
	bin := newBin(t)
	logp := filepath.Join(bin, "tool.log")
	pull := func(c string) string {
		setCase(t, bin, c)
		cmd := exec.Command(adapters["mail"])
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin", "TOOL_LOG=" + logp}
		cmd.Stdin = strings.NewReader(`{"config":{"account":"acc@example.com"},"cursor":"","window":{"from":"2026-11-01T00:00:00Z","to":"2026-11-12T00:00:00Z"}}`)
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		err := cmd.Run()
		return o.String() + e.String() + map[bool]string{true: "", false: "\nEXIT-ERR"}[err == nil]
	}
	if got := pull("mail-1.1.0"); !strings.Contains(got, "<mid-4@example.com>") || !strings.Contains(got, "<mid-6@example.com>") {
		t.Errorf("mail-1.1.0: %s", got)
	}
	if got := pull("mail-auth-fail"); !strings.Contains(got, "EXIT-ERR") {
		t.Errorf("认证失败应让适配器失败: %s", got)
	}
	if got := pull("mail-version-0.1"); !strings.Contains(got, "EXIT-ERR") {
		t.Errorf("版本越界应让适配器失败: %s", got)
	}
	// 线程 case：适配器读正文时请求的头，替身按 -H 如实返回。
	setCase(t, bin, "mail-thread")
	out, _, _ := run(t, bin, "himalaya", nil, "message", "read", "--preview", "-H", "Message-ID", "-H", "References", "-H", "In-Reply-To", "12", "--output", "json")
	var text string
	json.Unmarshal([]byte(out), &text)
	if !strings.HasPrefix(text, "Message-ID: <mid-t2@example.com>\nReferences: <mid-t1@example.com>\nIn-Reply-To: <mid-t1@example.com>\n\n") {
		t.Errorf("线程头: %q", text)
	}
	out, _, _ = run(t, bin, "himalaya", nil, "message", "read", "--preview", "-H", "Message-ID", "-H", "References", "-H", "In-Reply-To", "13", "--output", "json")
	json.Unmarshal([]byte(out), &text)
	if strings.Contains(text, "References:") || !strings.Contains(text, "In-Reply-To: <mid-t1@example.com>") {
		t.Errorf("mid-t3 只带 In-Reply-To: %q", text)
	}
}

// flow-source-calendar：cal-updated 只改 a1 与 c1 首个实例，cal-new 追加 e1；日历适配器经 PATH 使用替身。
func TestCalendarCases(t *testing.T) {
	bin := newBin(t)
	events := func(c string) map[string]map[string]any {
		setCase(t, bin, c)
		o, _, code := run(t, bin, "gog", nil, "--no-input", "--json", "--enable-commands=calendar", "--account=acc@example.com", "calendar", "events", "primary")
		if code != 0 {
			t.Fatalf("%s exit %d", c, code)
		}
		var d struct{ Events []map[string]any }
		if err := json.Unmarshal([]byte(o), &d); err != nil {
			t.Fatal(err)
		}
		m := map[string]map[string]any{}
		for _, e := range d.Events {
			m[e["id"].(string)] = e
		}
		return m
	}
	base := events("cal-base")
	if v, _, _ := run(t, bin, "gog", nil, "--version"); !strings.Contains(v, "v0.9.0") {
		t.Errorf("version: %q", v)
	}
	var want struct{ Events []map[string]any }
	json.Unmarshal([]byte(fixture(t, "sources", "calendar", "testdata", "0.9.0", "events.json")), &want)
	if len(base) != len(want.Events) {
		t.Errorf("cal-base 应原样回放")
	}
	upd := events("cal-updated")
	if upd["a1"]["summary"] != "保险到期日（改到周五）" || upd["a1"]["updated"] != "2026-03-02T08:00:00.000Z" ||
		upd["c1_20260310T010000Z"]["summary"] != "周会（改线上）" || upd["c1_20260317T010000Z"]["summary"] != "周会" || upd["b1"]["summary"] != base["b1"]["summary"] {
		t.Errorf("cal-updated 派生不对: a1=%v c1=%v", upd["a1"], upd["c1_20260310T010000Z"])
	}
	if u2 := events("cal-updated-2"); u2["a1"]["summary"] == upd["a1"]["summary"] || u2["a1"]["updated"].(string) <= upd["a1"]["updated"].(string) {
		t.Errorf("cal-updated-2 应再改 summary 并推后 updated: %v", u2["a1"])
	}
	if nw := events("cal-new"); nw["e1"] == nil || len(nw) != len(base)+1 {
		t.Errorf("cal-new 应新增 e1")
	}
	// 适配器：版本越界/无授权时失败，正常时能解析 cal-updated。
	creds := filepath.Join(t.TempDir(), "keyring.env")
	os.WriteFile(creds, []byte("GOG_KEYRING_PASSWORD=pw\n"), 0o600)
	pull := func(c string) (string, error) {
		setCase(t, bin, c)
		cmd := exec.Command(adapters["calendar"])
		cmd.Env = []string{"PATH=" + bin + ":/usr/bin", "TOOL_LOG=" + filepath.Join(bin, "tool.log")}
		cmd.Stdin = strings.NewReader(`{"config":{"account":"acc@example.com","horizon_days":3650},"cursor":"","credentials_path":"` + creds + `","window":{"from":"","to":"2026-03-01T00:00:00Z"}}`)
		var o bytes.Buffer
		cmd.Stdout = &o
		err := cmd.Run()
		return o.String(), err
	}
	if o, err := pull("cal-updated"); err != nil || !strings.Contains(o, "保险到期日（改到周五）") {
		t.Errorf("cal-updated: %v %s", err, o)
	}
	for _, c := range []string{"cal-auth-fail", "cal-version-0.1"} {
		if _, err := pull(c); err == nil {
			t.Errorf("%s 应让适配器失败", c)
		}
	}
}

// apple-pim 三个 CLI：回放 sources/applepim/testdata；pim-mail-thread 带 References。
func TestPimReplay(t *testing.T) {
	bin := newBin(t)
	for _, tc := range []struct {
		c, tool, file string
		args          []string
	}{
		{"pim-mail", "mail-cli", "messages.json", []string{"messages", "--format=json", "--engine=sqlite"}},
		{"pim-mail", "mail-cli", "get-m1.json", []string{"get", "--format=json", "--engine=sqlite", "--id=m1@example.test"}},
		{"pim-calendar", "calendar-cli", "events.json", []string{"events", "--format=json"}},
		{"pim-reminders", "reminder-cli", "reminders-overdue.json", []string{"items", "--format=json", "--filter=overdue"}},
		{"pim-reminders", "reminder-cli", "reminders-upcoming.json", []string{"items", "--format=json", "--filter=upcoming"}},
	} {
		setCase(t, bin, tc.c)
		got, _, code := run(t, bin, tc.tool, nil, tc.args...)
		if code != 0 || strings.TrimSpace(got) != strings.TrimSpace(fixture(t, "sources", "applepim", "testdata", tc.file)) {
			t.Errorf("%s %v 与 %s 不一致 (exit %d)", tc.tool, tc.args, tc.file, code)
		}
	}
	setCase(t, bin, "pim-mail-thread-1")
	o, _, _ := run(t, bin, "mail-cli", nil, "messages", "--format=json", "--engine=sqlite")
	if strings.Contains(o, "mid-p2") {
		t.Errorf("thread-1 只有原信: %s", o)
	}
	setCase(t, bin, "pim-mail-thread")
	o, _, _ = run(t, bin, "mail-cli", nil, "get", "--format=json", "--engine=sqlite", "--id=mid-p2@example.test")
	if !strings.Contains(o, `mid-p1@example.test`) {
		t.Errorf("回信缺 references: %s", o)
	}
}

// flow-channel-telegram：渠道侧直接复用 ext/telegram/fakebot；机器人替身收到发出的消息并记录。
func TestChannelBotRecordsMessage(t *testing.T) {
	b, err := fakebot.New()
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	mw.WriteField("chat_id", "42")
	mw.WriteField("text", "普通提醒")
	mw.Close()
	resp, err := http.Post(b.URL+"/botTEST/sendMessage", mw.FormDataContentType(), &body)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("sendMessage: %v %v", err, resp)
	}
	resp.Body.Close()
	if sent := b.Sent(); len(sent) != 1 || sent[0].Chat != 42 || sent[0].Text != "普通提醒" {
		t.Errorf("机器人替身应记录发出的消息: %+v", sent)
	}
}

// 其余邮件、代码托管与 pim case 的可观察输出。
func TestMoreCases(t *testing.T) {
	bin := newBin(t)
	call := func(c, tool string, args ...string) (string, int) {
		setCase(t, bin, c)
		o, _, code := run(t, bin, tool, nil, args...)
		return o, code
	}
	if o, code := call("mail-2.0.0", "himalaya", "--version"); code != 0 || !strings.Contains(o, "2.0.0") {
		t.Errorf("2.0.0 version: %q", o)
	}
	if o, code := call("mail-2.0.0", "himalaya", "message", "read", "--preview", "201", "--json"); code != 0 || strings.TrimSpace(o) != strings.TrimSpace(fixture(t, "sources", "mail", "testdata", "2.0.0", "read-201.json")) {
		t.Errorf("2.0.0 read: %q", o)
	}
	if o, _ := call("mail-garbled", "himalaya", "message", "read", "--preview", "-H", "Message-ID", "4", "--output", "json"); strings.Contains(o, "Message-ID") {
		t.Errorf("garbled 不应有 Message-ID 行: %q", o)
	}
	if o, _ := call("mail-garbled-json", "himalaya", "message", "read", "--preview", "4", "--output", "json"); json.Valid([]byte(o)) {
		t.Errorf("garbled-json 应不是 JSON: %q", o)
	}
	if o, _ := call("mail-1.1.0-new", "himalaya", "envelope", "list", "--output", "json"); !strings.Contains(o, "mid probe 7") {
		t.Errorf("mail-1.1.0-new 缺新邮件: %s", o)
	}
	if o, _ := call("mail-1.1.0-new", "himalaya", "message", "read", "--preview", "-H", "Message-ID", "7", "--output", "json"); !strings.Contains(o, "<mid-7@example.com>") {
		t.Errorf("mail-1.1.0-new 读 7: %s", o)
	}
	if o, _ := call("mail-1.1.0", "himalaya", "envelope", "list", "--page", "2", "--output", "json"); strings.TrimSpace(o) != "[]" {
		t.Errorf("第 2 页应为空: %s", o)
	}
	if _, code := call("mail-1.1.0", "himalaya", "message", "read", "--preview", "99", "--output", "json"); code != 1 {
		t.Errorf("不存在的邮件应失败: %d", code)
	}
	for c, n := range map[string]int{"mail-thread-1": 1, "mail-thread-2": 2, "mail-old-reply": 1, "mail-old-reply-2": 2} {
		o, _ := call(c, "himalaya", "envelope", "list", "--output", "json")
		var l []any
		json.Unmarshal([]byte(o), &l)
		if len(l) != n {
			t.Errorf("%s 应有 %d 封: %s", c, n, o)
		}
	}
	if o, _ := call("mail-old-reply-2", "himalaya", "message", "read", "--preview", "-H", "Message-ID", "-H", "References", "22", "--output", "json"); !strings.Contains(o, "References: <mid-o1@example.com>") {
		t.Errorf("old-reply-2: %s", o)
	}
	if _, code := call("mail-thread", "himalaya", "message", "read", "99", "--output", "json"); code != 1 {
		t.Errorf("线程 case 读不存在的邮件应失败")
	}
	if o, _ := call("gh-issue-opened", "gh", "api", "-H", "Accept: application/vnd.github+json", "/repos/acc-org/acc-repo/issues?per_page=1&page=2&since=2026-01-01T00:00:00Z"); strings.TrimSpace(o) != "[]" {
		t.Errorf("第 2 页应为空: %s", o)
	}
	if _, code := call("gh-issue-opened", "gh", "api", "/repos/other/repo/issues"); code != 1 {
		t.Errorf("未录制的仓库应 404")
	}
	if o, _ := call("gh-pr-updated-7", "gh", "api", "/repos/acc-org/acc-repo/contents/AGENTS.md"); !strings.Contains(o, `"sha":"sha-a"`) {
		t.Errorf("AGENTS.md: %s", o)
	}
	if _, code := call("pim-calendar", "calendar-cli", "items"); code != tools.ExitRefused {
		t.Errorf("pim 未录制子命令应拒绝")
	}
	if o, _ := call("pim-mail-thread", "mail-cli", "messages", "--format=json", "--engine=sqlite"); !strings.Contains(o, "mid-p2") {
		t.Errorf("pim-mail-thread 应含回信")
	}
}
