package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type fakeAPI struct {
	pages map[string][]string // 路径前缀（不含 &page=）→ 每页 JSON
	err   error
	calls []string
}

func (f *fakeAPI) Get(_ context.Context, path string) ([]byte, error) {
	f.calls = append(f.calls, path)
	if f.err != nil {
		return nil, f.err
	}
	for k, pages := range f.pages {
		if strings.HasPrefix(path, k) {
			var n int
			fmt.Sscanf(path[strings.LastIndex(path, "&page=")+6:], "%d", &n)
			if n-1 < len(pages) {
				return []byte(pages[n-1]), nil
			}
			return []byte("[]"), nil
		}
	}
	return []byte("[]"), nil
}

var t0 = time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)

func now() time.Time { return t0.Add(48 * time.Hour) }

func run(t *testing.T, api API, in string) (int, []map[string]any, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), strings.NewReader(in), &out, &errb, api, now)
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		m := map[string]any{}
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout line not JSON: %q", l)
		}
		lines = append(lines, m)
	}
	return code, lines, errb.String()
}

const issuesJSON = `[
 {"number":1,"title":"old issue updated","body":"b","html_url":"https://github.com/o/r/issues/1","created_at":"2026-10-01T00:00:00Z","updated_at":"2026-11-01T10:00:00Z","user":{"login":"alice"}},
 {"number":2,"title":"new issue","body":"hello","html_url":"https://github.com/o/r/issues/2","created_at":"2026-11-01T11:00:00Z","updated_at":"2026-11-01T12:00:00Z","user":{"login":"bob"}},
 {"number":3,"title":"merged pr","html_url":"https://github.com/o/r/pull/3","created_at":"2026-10-02T00:00:00Z","updated_at":"2026-11-01T13:00:00Z","closed_at":"2026-11-01T13:00:00Z","user":{"login":"carol"},"pull_request":{"merged_at":"2026-11-01T13:00:00Z"}},
 {"number":4,"title":"closed issue","html_url":"https://github.com/o/r/issues/4","created_at":"2026-10-02T00:00:00Z","updated_at":"2026-11-01T14:00:00Z","closed_at":"2026-11-01T14:00:00Z","user":{"login":"dan"}}
]`

const notifJSON = `[{"id":"77","reason":"review_requested","updated_at":"2026-11-01T15:00:00Z","repository":{"full_name":"o/r"},"subject":{"title":"Please review","type":"PullRequest","url":"https://api.github.com/repos/o/r/pulls/9"}}]`

const input = `{"source_id":"src_github","config":{"repos":["o/r"],"notifications":true},"cursor":"2026-11-01T00:00:00Z","run_id":"r1"}`

func TestPullMapsIssuesPRsNotificationsAndAdvancesCursor(t *testing.T) {
	api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": {issuesJSON}, "/notifications": {notifJSON}}}
	code, lines, _ := run(t, api, input)
	if code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	want := []struct{ id, typ, link string }{
		{"issue-o-r#1-updated-1793527200", "issue.updated", "https://github.com/o/r/issues/1"},
		{"issue-o-r#2-opened", "issue.opened", ""},
		{"pr-o-r#3-merged", "pr.merged", ""},
		{"issue-o-r#4-closed-1793541600", "issue.closed", ""},
		{"notif-77-1793545200", "notification.review_requested", "https://github.com/o/r/pull/9"},
	}
	if len(lines) != len(want)+1 {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	for i, w := range want {
		ev := lines[i]
		if ev["id"] != w.id || ev["type"] != w.typ {
			t.Errorf("event %d = %v, want %s %s", i, ev["id"], w.id, w.typ)
		}
		if w.link != "" && ev["link"] != w.link {
			t.Errorf("event %d link = %v", i, ev["link"])
		}
		if ev["subject"] == "" || ev["occurred_at"] == "" {
			t.Errorf("event %d missing required field: %v", i, ev)
		}
	}
	if lines[1]["sender"] != "bob" || lines[1]["summary"] != "hello" {
		t.Errorf("sender/summary: %v", lines[1])
	}
	if k := lines[4]["correlation_keys"].([]any); k[0] != "o/r#9" {
		t.Errorf("notification keys %v", k)
	}
	if k := lines[0]["correlation_keys"].([]any); k[0] != "o/r#1" {
		t.Errorf("issue keys %v", k)
	}
	last := lines[len(lines)-1]
	if len(last) != 1 || last["cursor"] != "2026-11-01T15:00:00Z" {
		t.Errorf("last line = %v", last)
	}
	if !strings.Contains(api.calls[0], "since=2026-11-01T00:00:00Z") {
		t.Errorf("cursor not used as since: %s", api.calls[0])
	}
}

func TestIdsAreStableAcrossRuns(t *testing.T) {
	mk := func() []map[string]any {
		api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": {issuesJSON}}}
		_, l, _ := run(t, api, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"2026-11-01T00:00:00Z"}`)
		return l
	}
	a, b := mk(), mk()
	for i := range a {
		if a[i]["id"] != b[i]["id"] {
			t.Fatalf("unstable id %v vs %v", a[i]["id"], b[i]["id"])
		}
	}
}

func TestNoNewItemsKeepsCursor(t *testing.T) {
	code, lines, _ := run(t, &fakeAPI{}, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"2026-11-01T00:00:00Z"}`)
	if code != 0 || len(lines) != 1 || lines[0]["cursor"] != "2026-11-01T00:00:00Z" {
		t.Fatalf("code=%d lines=%v", code, lines)
	}
}

func TestFirstRunStartsFromWindowThenNow(t *testing.T) {
	api := &fakeAPI{}
	run(t, api, `{"source_id":"s","config":{"repos":["o/r"]},"window":{"from":"2026-10-30T00:00:00Z"}}`)
	if !strings.Contains(api.calls[0], "since=2026-10-30T00:00:00Z") {
		t.Errorf("window.from ignored: %s", api.calls[0])
	}
	api = &fakeAPI{}
	_, lines, _ := run(t, api, `{"source_id":"s","config":{"repos":["o/r"]}}`)
	if lines[0]["cursor"] != "2026-11-03T00:00:00Z" {
		t.Errorf("first run without window should start now, got %v", lines[0])
	}
}

func TestWindowToExcludesLaterItems(t *testing.T) {
	api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": {issuesJSON}}}
	_, lines, _ := run(t, api, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"2026-11-01T00:00:00Z","window":{"to":"2026-11-01T12:30:00Z"}}`)
	if len(lines) != 3 || lines[2]["cursor"] != "2026-11-01T12:00:00Z" {
		t.Fatalf("lines=%v", lines)
	}
}

func TestPaginationAndTruncationHoldsCursorBack(t *testing.T) {
	item := func(n int, upd string) string {
		return fmt.Sprintf(`{"number":%d,"title":"t","created_at":"2026-10-01T00:00:00Z","updated_at":"%s","user":{"login":"u"}}`, n, upd)
	}
	var pages []string
	n := 0
	for p := 0; p < maxPages; p++ {
		var items []string
		for i := 0; i < perPage; i++ {
			n++
			items = append(items, item(n, fmt.Sprintf("2026-11-01T%02d:00:00Z", 1+p)))
		}
		pages = append(pages, "["+strings.Join(items, ",")+"]")
	}
	api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": pages}}
	code, lines, _ := run(t, api, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"2026-11-01T00:00:00Z"}`)
	if code != 0 || len(lines) < 2 {
		t.Fatalf("code=%d lines=%d", code, len(lines))
	}
	// 输出被预算截断，游标不超过页数上限能保证读全的位置，也不超过已输出的最后一项
	got := lines[len(lines)-1]["cursor"].(string)
	lastEv := lines[len(lines)-2]["occurred_at"].(string)
	if got > fmt.Sprintf("2026-11-01T%02d:00:00Z", maxPages) || got > lastEv {
		t.Errorf("cursor = %v, last event %v", got, lastEv)
	}
}

func TestFailureExitCodesAndNoStdout(t *testing.T) {
	cases := []struct {
		name string
		err  error
		in   string
		want int
	}{
		{"auth", classify("HTTP 401: Bad credentials"), input, ExitAuth},
		{"not logged in", classify("To get started with GitHub CLI, please run:  gh auth login"), input, ExitAuth},
		{"private repo", classify("gh: Not Found (HTTP 404)"), input, ExitAuth},
		{"rate limit", classify("gh: API rate limit exceeded (HTTP 403)"), input, ExitTemporary},
		{"server", classify("gh: Bad Gateway (HTTP 502)"), input, ExitTemporary},
		{"network", classify("dial tcp: lookup api.github.com: no such host"), input, ExitTemporary},
		{"gone", classify("gh: Gone (HTTP 410)"), input, ExitPermanent},
		{"plain error", fmt.Errorf("boom"), input, ExitTemporary},
		{"no config", nil, `{"source_id":"s","config":{}}`, ExitAuth},
		{"bad repo", nil, `{"source_id":"s","config":{"repos":["../x"]}}`, ExitAuth},
		{"bad cursor", nil, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"x"}`, ExitAuth},
		{"bad json", nil, `{`, ExitAuth},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		code := Run(context.Background(), strings.NewReader(c.in), &out, &errb, &fakeAPI{err: c.err}, now)
		if code != c.want {
			t.Errorf("%s: exit %d, want %d", c.name, code, c.want)
		}
		if out.Len() != 0 || errb.Len() == 0 {
			t.Errorf("%s: stdout=%q stderr=%q", c.name, out.String(), errb.String())
		}
	}
}

func TestMalformedResponseIsTemporary(t *testing.T) {
	api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": {`{"message":"x"}`}}}
	if code, _, _ := run(t, api, input); code != ExitTemporary {
		t.Fatalf("exit %d", code)
	}
}

// 用假 gh 脚本走真实的 exec 路径：验证凭据目录隔离、环境 token 不泄入、失败分类。
func fakeGH(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGHUsesSourceOwnCredentialDirAndIgnoresEnvToken(t *testing.T) {
	t.Setenv("GH_TOKEN", "leak")
	t.Setenv("GITHUB_TOKEN", "leak")
	bin := fakeGH(t, `[ "$1" = api ] || exit 9
[ -z "$GH_TOKEN$GITHUB_TOKEN" ] || { echo "HTTP 500 token leaked" >&2; exit 1; }
echo "{\"dir\":\"$GH_CONFIG_DIR\"}" | sed 's/^/[/;s/$/]/'`)
	g := GH{Bin: bin, ConfigDir: "/x/sources/src_github/gh"}
	out, err := g.Get(context.Background(), "/repos/o/r/issues?page=1")
	if err != nil || !strings.Contains(string(out), "/x/sources/src_github/gh") {
		t.Fatalf("out=%q err=%v", out, err)
	}
}

func TestGHFailureClassification(t *testing.T) {
	g := GH{Bin: fakeGH(t, `echo "gh: Bad credentials (HTTP 401)" >&2; exit 1`)}
	_, err := g.Get(context.Background(), "/x")
	if e, ok := err.(*Error); !ok || e.Exit != ExitAuth {
		t.Fatalf("err = %v", err)
	}
	_, err = GH{Bin: "/nonexistent/gh"}.Get(context.Background(), "/x")
	if e, ok := err.(*Error); !ok || e.Exit != ExitAuth {
		t.Fatalf("missing gh err = %v", err)
	}
}

func TestRunStdinPicksCredentialDirBySourceID(t *testing.T) {
	var got string
	api := &fakeAPI{}
	var out, errb bytes.Buffer
	code := RunStdin(context.Background(), strings.NewReader(`{"source_id":"src_github","config":{"repos":["o/r"]},"cursor":"2026-11-01T00:00:00Z"}`),
		&out, &errb, func(id string) (API, error) { got = id; return api, nil }, now)
	if code != 0 || got != "src_github" || len(api.calls) == 0 {
		t.Fatalf("code=%d id=%q", code, got)
	}
	code = RunStdin(context.Background(), strings.NewReader(`{"source_id":"../evil"}`), &out, &errb,
		func(id string) (API, error) { _, err := ConfigDir(id); return nil, err }, now)
	if code != ExitAuth {
		t.Fatalf("path traversal id: exit %d", code)
	}
	if code = RunStdin(context.Background(), strings.NewReader(`nope`), &out, &errb, nil, now); code != ExitAuth {
		t.Fatalf("bad json: %d", code)
	}
}

func TestConfigDirHonorsAPAHome(t *testing.T) {
	t.Setenv("APA_HOME", "/data/apa")
	d, err := ConfigDir("src_github")
	if err != nil || d != "/data/apa/sources/src_github/gh" {
		t.Fatalf("%q %v", d, err)
	}
	t.Setenv("APA_HOME", "")
	t.Setenv("HOME", "/home/u")
	if d, _ := ConfigDir("a"); d != "/home/u/.apa/sources/a/gh" {
		t.Fatalf("default dir %q", d)
	}
}

func TestAuthRunsGHLoginInSourceDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sources", "s", "gh")
	rec := filepath.Join(t.TempDir(), "rec")
	g := GH{Bin: fakeGH(t, `echo "$@ $GH_CONFIG_DIR" > `+rec), ConfigDir: dir}
	null, _ := os.Open(os.DevNull)
	if code := Auth(context.Background(), g, null, null, null); code != ExitOK {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(rec)
	if strings.TrimSpace(string(b)) != "auth login --insecure-storage "+dir {
		t.Errorf("gh invoked as %q", b)
	}
	if fi, err := os.Stat(dir); err != nil || fi.Mode().Perm() != 0o700 {
		t.Errorf("credential dir: %v %v", fi, err)
	}
	if code := Auth(context.Background(), GH{Bin: fakeGH(t, "exit 1"), ConfigDir: dir}, null, null, null); code != ExitAuth {
		t.Errorf("failed login exit %d", code)
	}
}

func TestRerunWithSavedCursorEmitsNoNewIds(t *testing.T) {
	mk := func(cursor string) (map[string]bool, string) {
		api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": {issuesJSON}}}
		_, lines, _ := run(t, api, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"`+cursor+`"}`)
		ids := map[string]bool{}
		for _, l := range lines[:len(lines)-1] {
			ids[l["id"].(string)] = true
		}
		return ids, lines[len(lines)-1]["cursor"].(string)
	}
	first, cur := mk("2026-11-01T00:00:00Z")
	second, _ := mk(cur)
	for id := range second {
		if !first[id] {
			t.Errorf("run 2 emitted new id %s from identical upstream data", id)
		}
	}
	if len(second) == 0 || second["issue-o-r#1-updated-1793527200"] {
		t.Errorf("boundary item must not re-emit updated: %v", second)
	}
}

func TestCreatedAndMergedInOneWindowYieldsBoth(t *testing.T) {
	pr := `[{"number":5,"title":"quick","created_at":"2026-11-01T01:00:00Z","updated_at":"2026-11-01T02:00:00Z","closed_at":"2026-11-01T02:00:00Z","user":{"login":"u"},"pull_request":{"merged_at":"2026-11-01T02:00:00Z"}}]`
	api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": {pr}}}
	_, lines, _ := run(t, api, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"2026-11-01T00:00:00Z"}`)
	if len(lines) != 3 || lines[0]["type"] != "pr.opened" || lines[1]["type"] != "pr.merged" {
		t.Fatalf("lines=%v", lines)
	}
}

func TestPullConfigDirUsesRunnerCwdWithoutAPAHome(t *testing.T) {
	t.Setenv("APA_HOME", "")
	t.Setenv("HOME", "/home/u")
	src := filepath.Join(t.TempDir(), "sources", "src_github")
	if d, err := PullConfigDir("src_github", src); err != nil || d != filepath.Join(src, "gh") {
		t.Fatalf("%q %v", d, err)
	}
	// cwd 不是该来源目录（手工运行）时回退到 APA_HOME 规则
	if d, _ := PullConfigDir("src_github", "/somewhere/else"); d != "/home/u/.apa/sources/src_github/gh" {
		t.Fatalf("fallback %q", d)
	}
	if _, err := PullConfigDir("../x", src); err == nil {
		t.Fatal("bad id accepted")
	}
}

func TestLargeBacklogIsBoundedPerRunAndEventuallyEmitsEveryItemOnce(t *testing.T) {
	body := strings.Repeat("x", 3900)
	var items []string
	for i := 1; i <= 30; i++ {
		items = append(items, fmt.Sprintf(`{"number":%d,"title":"t%d","body":%q,"created_at":"2026-10-01T00:00:00Z","updated_at":"2026-11-01T00:%02d:00Z","user":{"login":"u"}}`, i, i, body, i))
	}
	all := "[" + strings.Join(items, ",") + "]"
	// 假 API 按 since 过滤，和真实接口一样
	api := &sinceAPI{all: items}
	_ = all
	seen := map[string]int{}
	cursor := "2026-11-01T00:00:00Z"
	for run := 0; run < 40; run++ {
		var out, errb bytes.Buffer
		in := `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"` + cursor + `"}`
		if code := Run(context.Background(), strings.NewReader(in), &out, &errb, api, now); code != 0 {
			t.Fatalf("run %d exit %d: %s", run, code, errb.String())
		}
		if out.Len() >= 64<<10 {
			t.Fatalf("run %d wrote %d bytes, kernel retains only 64KB", run, out.Len())
		}
		lines := strings.Split(strings.TrimSpace(out.String()), "\n")
		var last map[string]string
		json.Unmarshal([]byte(lines[len(lines)-1]), &last)
		for _, l := range lines[:len(lines)-1] {
			var ev struct{ ID string }
			json.Unmarshal([]byte(l), &ev)
			seen[ev.ID]++
		}
		if last["cursor"] == cursor {
			break
		}
		cursor = last["cursor"]
	}
	if len(seen) != 30 {
		t.Fatalf("emitted %d distinct ids, want 30", len(seen))
	}
}

type sinceAPI struct{ all []string }

func (a *sinceAPI) Get(_ context.Context, path string) ([]byte, error) {
	if strings.Contains(path, "&page=2") {
		return []byte("[]"), nil
	}
	i := strings.Index(path, "since=") + 6
	since, _ := time.Parse(time.RFC3339, path[i:i+20])
	var out []string
	for _, raw := range a.all {
		var it struct {
			UpdatedAt time.Time `json:"updated_at"`
		}
		json.Unmarshal([]byte(raw), &it)
		if !it.UpdatedAt.Before(since) {
			out = append(out, raw)
		}
	}
	return []byte("[" + strings.Join(out, ",") + "]"), nil
}

func TestReclosedIssueGetsNewClosedID(t *testing.T) {
	mk := func(closed string) string {
		api := &fakeAPI{pages: map[string][]string{"/repos/o/r/issues": {`[{"number":7,"title":"t","created_at":"2026-10-01T00:00:00Z","updated_at":"` + closed + `","closed_at":"` + closed + `","user":{"login":"u"}}]`}}}
		_, l, _ := run(t, api, `{"source_id":"s","config":{"repos":["o/r"]},"cursor":"2026-11-01T00:00:00Z"}`)
		return l[0]["id"].(string)
	}
	if mk("2026-11-01T01:00:00Z") == mk("2026-11-01T05:00:00Z") {
		t.Fatal("second close deduped into the first")
	}
}
