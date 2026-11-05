package httpjson

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const feed = `{"data":{"items":[
 {"key":"a-1","kind":"issue.opened","title":"First","body":"b1","url":"http://x/1","ts":"2026-01-01T00:00:00Z","user":{"name":"alice"},"repo":"r#1"},
 {"key":"a-2","kind":"issue.closed","title":"Second","ts":1767312000,"user":{"name":"bob"},"repo":"r#2"}]}}`

func cfg(url string, extra string) string {
	return `{"source_id":"src_h","run_id":"r1","cursor":"` + extra + `","config":{"url":"` + url + `","items_path":"data.items",
 "mapping":{"id":"key","type":"kind","subject":"title","summary":"body","link":"url","occurred_at":"ts","sender":"user.name","correlation_keys":["repo"]}}}`
}

func do(t *testing.T, input string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := Run(context.Background(), strings.NewReader(input), &out, &errb)
	return code, out.String(), errb.String()
}

// ACT-HTTP-01：按 config 的地址与字段映射产出标准事件，末行是新游标。
func TestMapsItemsToStandardEventsWithCursor(t *testing.T) {
	var gotAuth, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
		_, _ = w.Write([]byte(feed))
	}))
	defer srv.Close()
	tok := filepath.Join(t.TempDir(), "token")
	_ = os.WriteFile(tok, []byte("s3cret\n"), 0o600)
	in := strings.Replace(cfg(srv.URL, ""), `"items_path"`, `"token_file":"`+tok+`","cursor_param":"since","items_path"`, 1)

	code, out, _ := do(t, in)
	if code != ExitOK {
		t.Fatalf("exit=%d", code)
	}
	if gotAuth != "Bearer s3cret" {
		t.Fatalf("auth=%q", gotAuth)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("lines=%v", lines)
	}
	var ev event
	_ = json.Unmarshal([]byte(lines[0]), &ev)
	want := event{ID: "a-1", Type: "issue.opened", Subject: "First", Summary: "b1", Link: "http://x/1",
		OccurredAt: "2026-01-01T00:00:00Z", CorrelationKeys: []string{"r#1"}, Sender: "alice"}
	if !equalEvent(ev, want) {
		t.Fatalf("ev=%+v", ev)
	}
	_ = json.Unmarshal([]byte(lines[1]), &ev)
	if ev.OccurredAt != "2026-01-02T00:00:00Z" {
		t.Fatalf("unix secs not normalized: %q", ev.OccurredAt)
	}
	if lines[2] != `{"cursor":"2026-01-02T00:00:00Z"}` {
		t.Fatalf("cursor line=%s", lines[2])
	}

	// 带游标再拉：游标作为查询参数发出，早于游标的条目不再输出，游标不回退。
	in = strings.Replace(cfg(srv.URL, "2026-01-02T00:00:00Z"), `"items_path"`, `"cursor_param":"since","items_path"`, 1)
	code, out, _ = do(t, in)
	if code != ExitOK || gotQuery != "since=2026-01-02T00%3A00%3A00Z" {
		t.Fatalf("exit=%d query=%q", code, gotQuery)
	}
	lines = strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], `"a-2"`) || lines[1] != `{"cursor":"2026-01-02T00:00:00Z"}` {
		t.Fatalf("lines=%v", lines)
	}
}

func equalEvent(a, b event) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return bytes.Equal(x, y)
}

func TestEmptyFeedKeepsOrSeedsCursor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"data":{"items":[]}}`)) }))
	defer srv.Close()
	_, out, _ := do(t, cfg(srv.URL, "2026-03-01T00:00:00Z"))
	if strings.TrimSpace(out) != `{"cursor":"2026-03-01T00:00:00Z"}` {
		t.Fatalf("out=%q", out)
	}
	_, out, _ = do(t, cfg(srv.URL, ""))
	if !strings.HasPrefix(out, `{"cursor":"`) {
		t.Fatalf("first run must still emit a valid cursor, got %q", out)
	}
}

// ACT-HTTP-02：错误状态码到退出码的映射，失败时 stdout 不输出任何事件或游标。
func TestStatusToExitCode(t *testing.T) {
	for status, want := range map[int]int{500: 1, 503: 1, 429: 1, 401: 2, 403: 2, 404: 2} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) }))
		code, out, _ := do(t, cfg(srv.URL, ""))
		srv.Close()
		if code != want || out != "" {
			t.Errorf("status %d: exit=%d out=%q want exit %d", status, code, out, want)
		}
	}
}

func TestFailureSemantics(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(feed)) }))
	defer ok.Close()
	html := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("<html>")) }))
	defer html.Close()
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	cases := []struct {
		name, in string
		want     int
	}{
		{"not json stdin", `nope`, 2},
		{"bad url scheme", cfg("ftp://x", ""), 2},
		{"missing mapping", `{"config":{"url":"http://x"}}`, 2},
		{"missing type", `{"config":{"url":"http://x","mapping":{"id":"a","subject":"b","occurred_at":"c"}}}`, 2},
		{"unreachable is temporary", cfg(deadURL, ""), 1},
		{"body not json is temporary", cfg(html.URL, ""), 1},
		{"items_path absent", strings.Replace(cfg(ok.URL, ""), "data.items", "data.nope", 1), 2},
		{"items_path not array", strings.Replace(cfg(ok.URL, ""), "data.items", "data", 1), 2},
		{"mapped id missing", strings.Replace(cfg(ok.URL, ""), `"id":"key"`, `"id":"nokey"`, 1), 2},
		{"bad occurred_at", strings.Replace(cfg(ok.URL, ""), `"occurred_at":"ts"`, `"occurred_at":"title"`, 1), 2},
		{"bad cursor", cfg(ok.URL, "yesterday"), 2},
		{"token file missing", strings.Replace(cfg(ok.URL, ""), `"items_path"`, `"token_file":"/nonexistent","items_path"`, 1), 2},
	}
	for _, c := range cases {
		code, out, errs := do(t, c.in)
		if code != c.want || out != "" || errs == "" {
			t.Errorf("%s: exit=%d out=%q err=%q want exit %d", c.name, code, out, errs, c.want)
		}
	}
}

func TestRootArrayAndFixedType(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"n":7,"t":"hello","at":"2026-05-05T10:00:00+02:00"}]`))
	}))
	defer srv.Close()
	code, out, _ := do(t, `{"config":{"url":"`+srv.URL+`","event_type":"poll.item","mapping":{"id":"n","subject":"t","occurred_at":"at"}}}`)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != 0 || len(lines) != 2 || !strings.Contains(lines[0], `"id":"7"`) || !strings.Contains(lines[0], `"type":"poll.item"`) ||
		!strings.Contains(lines[0], `"occurred_at":"2026-05-05T08:00:00Z"`) {
		t.Fatalf("code=%d out=%q", code, out)
	}
}
