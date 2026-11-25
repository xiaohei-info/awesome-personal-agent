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

// equalEvent 比较映射出的字段；original 由专门的测试断言。
func equalEvent(a, b event) bool {
	a.Original, b.Original = nil, nil
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

// 被映射的那一条 JSON 原样（含未映射字段、数字写法、键序）进 original，媒体类型 application/json。
func TestOriginalIsTheMappedItemVerbatim(t *testing.T) {
	const item1 = `{"key":"a-1","kind":"k","title":"T","ts":"2026-01-01T00:00:00Z","extra":{"n":1.50,"z":[1,  2]},"big":12345678901234567890}`
	const item2 = `{ "key" : "a-2", "kind":"k","title":"U","ts":1767312000 }`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"data":{"other":1,"items":[` + item1 + `,` + "\n " + item2 + `]}}`))
	}))
	defer srv.Close()
	code, out, _ := do(t, cfg(srv.URL, ""))
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if code != ExitOK || len(lines) != 3 {
		t.Fatalf("code=%d out=%q", code, out)
	}
	for i, want := range []string{item1, item2} {
		var ev struct {
			Original *struct {
				MediaType string `json:"media_type"`
				Text      string `json:"text"`
			} `json:"original"`
		}
		if err := json.Unmarshal([]byte(lines[i]), &ev); err != nil || ev.Original == nil {
			t.Fatalf("line %d: %v %s", i, err, lines[i])
		}
		if ev.Original.MediaType != "application/json" || ev.Original.Text != want {
			t.Errorf("original[%d] = %q %q，期望原样 %q", i, ev.Original.MediaType, ev.Original.Text, want)
		}
	}
	if strings.Contains(lines[2], "original") {
		t.Errorf("游标行不应带 original: %s", lines[2])
	}
}

// 根就是数组时同样取每个元素的原样文本。
func TestOriginalForRootArray(t *testing.T) {
	const item = `{"n":7,"t":"hello","at":"2026-05-05T10:00:00+02:00"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("[" + item + "]")) }))
	defer srv.Close()
	_, out, _ := do(t, `{"config":{"url":"`+srv.URL+`","event_type":"p","mapping":{"id":"n","subject":"t","occurred_at":"at"}}}`)
	var ev struct{ Original struct{ Text string } }
	_ = json.Unmarshal([]byte(strings.SplitN(out, "\n", 2)[0]), &ev)
	if ev.Original.Text != item {
		t.Fatalf("original=%q", ev.Original.Text)
	}
}

// 统一时间字段（设计 §9.2）：映射里加 due_at、starts_at、ends_at、all_day 四条路径就带上时间，不用改代码。
func TestMapsUnifiedTimeFields(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[{"key":"t-1","kind":"reminder.due","title":"交材料","ts":"2026-11-30T00:00:00Z","due":"2026-12-02","start":"2026-12-03T09:00:00+08:00","end":"2026-12-03T10:30:00+08:00","allday":false}]}`))
	}))
	defer srv.Close()
	in := `{"source_id":"src_h","config":{"url":"` + srv.URL + `","items_path":"items","mapping":{"id":"key","type":"kind","subject":"title",
		"occurred_at":"ts","due_at":"due","starts_at":"start","ends_at":"end","all_day":"allday"}}}`
	code, out, errOut := do(t, in)
	if code != ExitOK {
		t.Fatalf("exit=%d %s", code, errOut)
	}
	var ev event
	if err := json.Unmarshal([]byte(strings.Split(strings.TrimSpace(out), "\n")[0]), &ev); err != nil {
		t.Fatal(err)
	}
	if ev.DueAt != "2026-12-02" || ev.StartsAt != "2026-12-03T09:00:00+08:00" || ev.EndsAt != "2026-12-03T10:30:00+08:00" || ev.AllDay {
		t.Fatalf("event %+v", ev)
	}
}
