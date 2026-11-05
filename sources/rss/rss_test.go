package rss

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const rssFeed = `<?xml version="1.0" encoding="UTF-8"?>
<rss version="2.0" xmlns:content="http://purl.org/rss/1.0/modules/content/" xmlns:dc="http://purl.org/dc/elements/1.1/"><channel><title>t</title>
<item><title>Old</title><link>https://e.com/1</link><guid>g-1</guid><pubDate>Mon, 02 Jan 2023 15:04:05 +0000</pubDate><description>old desc</description></item>
<item><title>New</title><link>https://e.com/2</link><guid>g-2</guid><pubDate>Tue, 03 Jan 2023 15:04:05 +0000</pubDate><description>new desc</description><content:encoded>full body</content:encoded><dc:creator>alice</dc:creator></item>
</channel></rss>`

const atomFeed = `<?xml version="1.0" encoding="utf-8"?>
<feed xmlns="http://www.w3.org/2005/Atom"><title>t</title>
<entry><title>A1</title><id>urn:a1</id><link rel="alternate" href="https://e.com/a1"/><updated>2023-02-01T10:00:00Z</updated><summary>sum</summary><content>body</content><author><name>bob</name></author></entry>
<entry><title>A2</title><id>urn:a2</id><link href="https://e.com/a2"/><published>2023-02-02T10:00:00Z</published><updated>2023-02-03T10:00:00Z</updated></entry>
</feed>`

func serve(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return s
}

type result struct {
	code   int
	events []map[string]any
	cursor string // 空表示没有游标行
	stdout string
}

func call(t *testing.T, in map[string]any) result {
	t.Helper()
	b, _ := json.Marshal(in)
	var out, errb bytes.Buffer
	r := result{code: Run(context.Background(), bytes.NewReader(b), &out, &errb), stdout: out.String()}
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("stdout 行不是 JSON: %q", l)
		}
		if c, ok := m["cursor"].(string); ok && len(m) == 1 {
			r.cursor = c
			continue
		}
		r.events = append(r.events, m)
	}
	return r
}

// cur 构造期望的游标字符串（与实现同样的编码）。
func cur(t string, seen ...string) string {
	hs := []string{}
	for _, id := range seen {
		hs = append(hs, shortHash(id))
	}
	b, _ := json.Marshal(map[string]any{"t": t, "seen": hs})
	return string(b)
}

func in(url, cursor string, cfg map[string]any) map[string]any {
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["url"] = url
	return map[string]any{"source_id": "s", "run_id": "r", "cursor": cursor, "config": cfg}
}

func TestRSSFirstRunEmitsAllOldestFirstAndAdvancesCursor(t *testing.T) {
	s := serve(t, 200, rssFeed)
	r := call(t, in(s.URL, "", nil))
	if r.code != 0 || len(r.events) != 2 {
		t.Fatalf("code=%d out=%s", r.code, r.stdout)
	}
	e := r.events[1]
	if r.events[0]["id"] != "g-1" || e["id"] != "g-2" || e["subject"] != "New" || e["link"] != "https://e.com/2" ||
		e["summary"] != "new desc" || e["detail"] != "full body" || e["sender"] != "alice" ||
		e["type"] != "feed.entry" || e["occurred_at"] != "2023-01-03T15:04:05Z" {
		t.Fatalf("事件映射不对: %v", e)
	}
	if r.cursor != cur("2023-01-03T15:04:05Z", "g-2") {
		t.Fatalf("cursor=%q", r.cursor)
	}
}

func TestOnlyNewEntriesAfterCursor(t *testing.T) {
	s := serve(t, 200, rssFeed)
	r := call(t, in(s.URL, "2023-01-02T20:00:00Z", nil))
	if r.code != 0 || len(r.events) != 1 || r.events[0]["id"] != "g-2" || r.cursor != cur("2023-01-03T15:04:05Z", "g-2") {
		t.Fatalf("code=%d out=%s", r.code, r.stdout)
	}
	r = call(t, in(s.URL, r.cursor, nil))
	if r.code != 0 || len(r.events) != 0 || r.cursor != cur("2023-01-03T15:04:05Z", "g-2") {
		t.Fatalf("没有新条目时游标应保持: %d %s", r.code, r.stdout)
	}
}

func TestAtomMapping(t *testing.T) {
	s := serve(t, 200, atomFeed)
	r := call(t, in(s.URL, "", nil))
	if r.code != 0 || len(r.events) != 2 {
		t.Fatalf("code=%d out=%s", r.code, r.stdout)
	}
	a1, a2 := r.events[0], r.events[1]
	if a1["id"] != "urn:a1" || a1["link"] != "https://e.com/a1" || a1["summary"] != "sum" || a1["detail"] != "body" ||
		a1["sender"] != "bob" || a1["occurred_at"] != "2023-02-01T10:00:00Z" {
		t.Fatalf("atom 映射不对: %v", a1)
	}
	if a2["occurred_at"] != "2023-02-03T10:00:00Z" || r.cursor != cur("2023-02-03T10:00:00Z", "urn:a2") {
		t.Fatalf("a2=%v cursor=%s", a2, r.cursor)
	}
}

func TestMaxItemsCapsOldestFirstWithoutSkipping(t *testing.T) {
	s := serve(t, 200, rssFeed)
	r := call(t, in(s.URL, "", map[string]any{"max_items": 1}))
	if r.code != 0 || len(r.events) != 1 || r.events[0]["id"] != "g-1" || r.cursor != cur("2023-01-02T15:04:05Z", "g-1") {
		t.Fatalf("code=%d out=%s", r.code, r.stdout)
	}
	r = call(t, in(s.URL, r.cursor, map[string]any{"max_items": 1})) // 下一轮取到被截掉的条目
	if len(r.events) != 1 || r.events[0]["id"] != "g-2" || r.cursor != cur("2023-01-03T15:04:05Z", "g-2") {
		t.Fatalf("out=%s", r.stdout)
	}
}

func TestCapSplittingSameTimestampGroupLosesNothing(t *testing.T) {
	item := func(g string) string {
		return `<item><guid>` + g + `</guid><title>t</title><pubDate>Tue, 03 Jan 2023 15:04:05 +0000</pubDate></item>`
	}
	s := serve(t, 200, `<rss version="2.0"><channel>`+item("a")+item("b")+item("c")+`</channel></rss>`)
	c, got := "", map[string]int{}
	for i := 0; i < 4; i++ {
		r := call(t, in(s.URL, c, map[string]any{"max_items": 1}))
		for _, e := range r.events {
			got[e["id"].(string)]++
		}
		c = r.cursor
	}
	if len(got) != 3 || got["a"] != 1 || got["b"] != 1 || got["c"] != 1 {
		t.Fatalf("每条恰好一次: %v", got)
	}
}

func TestOutputStaysUnderRunnerRetainedLimitAndResumes(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<rss version="2.0"><channel>`)
	for i := 0; i < 60; i++ {
		sb.WriteString(fmt.Sprintf(`<item><guid>g%02d</guid><title>%s</title><link>https://e.com/%d?a=1&amp;b=2</link><pubDate>Tue, 03 Jan 2023 15:%02d:05 +0000</pubDate><description>%s</description></item>`,
			i, strings.Repeat("标", 250), i, i, strings.Repeat("<p>x</p>", 400)))
	}
	sb.WriteString(`</channel></rss>`)
	s := serve(t, 200, sb.String())
	seen, c := map[string]bool{}, ""
	for i := 0; i < 30 && len(seen) < 60; i++ {
		r := call(t, in(s.URL, c, map[string]any{"max_items": 200}))
		if r.code != 0 || len(r.stdout) >= 64<<10 || len(r.events) == 0 {
			t.Fatalf("round %d code=%d bytes=%d events=%d", i, r.code, len(r.stdout), len(r.events))
		}
		for _, e := range r.events {
			seen[e["id"].(string)] = true
		}
		c = r.cursor
	}
	if len(seen) != 60 {
		t.Fatalf("多轮后应取全: %d", len(seen))
	}
}

func TestRealWorldFeedQuirks(t *testing.T) {
	// ISO-8859-1 声明 + &nbsp; 实体 + 裸 &
	body := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><rss version=\"2.0\"><channel><item><guid>q</guid><title>caf\xe9 &nbsp;R&D & co</title><pubDate>Tue, 03 Jan 2023 15:04:05 +0000</pubDate></item></channel></rss>"
	s := serve(t, 200, body)
	r := call(t, in(s.URL, "", nil))
	if r.code != 0 || len(r.events) != 1 || r.events[0]["subject"] != "café \u00a0R&D & co" {
		t.Fatalf("code=%d out=%q", r.code, r.stdout)
	}
	s2 := serve(t, 200, "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?><rss><channel/></rss>")
	if r := call(t, in(s2.URL, "c", nil)); r.code != ExitTransient || r.stdout != "" {
		t.Fatalf("不支持的字符集应失败且游标不动: %d %q", r.code, r.stdout)
	}
}

func TestUndatedEntryUsesStableIDAndDoesNotMoveCursor(t *testing.T) {
	s := serve(t, 200, `<rss version="2.0"><channel><item><title>x</title><link>https://e.com/x</link></item></channel></rss>`)
	r := call(t, in(s.URL, "2023-01-01T00:00:00Z", nil))
	if r.code != 0 || len(r.events) != 1 || r.events[0]["id"] != "https://e.com/x" || r.cursor != "2023-01-01T00:00:00Z" {
		t.Fatalf("code=%d out=%s", r.code, r.stdout)
	}
}

func TestInvalidXMLLeavesCursorAndExitsTransient(t *testing.T) {
	for _, body := range []string{"<rss><channel><item>", "not xml at all", "<html><body>hi</body></html>", ""} {
		s := serve(t, 200, body)
		r := call(t, in(s.URL, "2023-01-01T00:00:00Z", nil))
		if r.code != ExitTransient || r.cursor != "" || len(r.events) != 0 {
			t.Fatalf("body=%q code=%d out=%q", body, r.code, r.stdout)
		}
	}
}

func TestUnreachableLeavesCursorAndExitsTransient(t *testing.T) {
	s := serve(t, 200, rssFeed)
	url := s.URL
	s.Close()
	r := call(t, in(url, "c", nil))
	if r.code != ExitTransient || r.stdout != "" {
		t.Fatalf("code=%d out=%q", r.code, r.stdout)
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	for status, want := range map[int]int{500: ExitTransient, 503: ExitTransient, 429: ExitTransient, 401: ExitAuth, 403: ExitAuth, 404: ExitPermanent, 410: ExitPermanent} {
		s := serve(t, status, "x")
		r := call(t, in(s.URL, "c", nil))
		if r.code != want || r.stdout != "" {
			t.Fatalf("status=%d code=%d out=%q", status, r.code, r.stdout)
		}
	}
}

func TestBadConfigAndInput(t *testing.T) {
	for _, c := range []map[string]any{
		{"source_id": "s", "config": map[string]any{}},
		{"source_id": "s", "config": map[string]any{"url": "file:///etc/passwd"}},
		{"source_id": "s", "config": map[string]any{"url": "ftp://x/y"}},
		{"source_id": "s", "config": map[string]any{"url": "http://x/y", "max_items": -1}},
	} {
		if r := call(t, c); r.code != ExitBadConfig || r.stdout != "" {
			t.Fatalf("%v code=%d", c, r.code)
		}
	}
	var out, errb bytes.Buffer
	if code := Run(context.Background(), strings.NewReader("{nope"), &out, &errb); code != ExitBadConfig || out.Len() != 0 {
		t.Fatalf("code=%d", code)
	}
}

func TestLongSummaryAndDetailAreTruncated(t *testing.T) {
	long := strings.Repeat("字", 5000)
	s := serve(t, 200, `<rss version="2.0"><channel><item><guid>g</guid><title>t</title><pubDate>Tue, 03 Jan 2023 15:04:05 +0000</pubDate><description>`+long+`</description></item></channel></rss>`)
	r := call(t, in(s.URL, "", nil))
	if got := []rune(r.events[0]["summary"].(string)); len(got) > maxSummary+1 {
		t.Fatalf("summary 未截断: %d", len(got))
	}
}

func TestHugeSameTimestampGroupKeepsOutputAndCursorBounded(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`<rss version="2.0"><channel>`)
	for i := 0; i < 3000; i++ {
		sb.WriteString(fmt.Sprintf(`<item><guid>%s-%d</guid><title>t</title><pubDate>Tue, 03 Jan 2023 00:00:00 +0000</pubDate></item>`, strings.Repeat("长", 250), i))
	}
	sb.WriteString(`</channel></rss>`)
	s := serve(t, 200, sb.String())
	c, total := "", 0
	for i := 0; i < 200; i++ {
		r := call(t, in(s.URL, c, map[string]any{"max_items": 200}))
		if r.code != 0 || len(r.stdout) >= 64<<10 || r.cursor == "" {
			t.Fatalf("round %d code=%d bytes=%d", i, r.code, len(r.stdout))
		}
		total += len(r.events)
		if len(r.events) == 0 {
			break
		}
		c = r.cursor
	}
	if total == 0 || total > 3000 {
		t.Fatalf("total=%d", total)
	}
}
