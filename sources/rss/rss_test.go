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
	"time"
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
	return serveCT(t, status, "", &body)
}

// serveCT 每次请求都读 *body，测试中途改它即可模拟订阅更新。
func serveCT(t *testing.T, status int, ctype string, body *string) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		w.Write([]byte(*body))
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

func in(url, cursor string, cfg map[string]any) map[string]any {
	if cfg == nil {
		cfg = map[string]any{}
	}
	cfg["url"] = url
	return map[string]any{"source_id": "s", "run_id": "r", "cursor": cursor, "config": cfg}
}

func TestInvalidXMLLeavesCursorAndExitsTransient(t *testing.T) {
	for _, body := range []string{"<rss><channel><item>", "not xml at all", "<html><body>hi</body></html>", ""} {
		s := serve(t, 200, body)
		r := call(t, in(s.URL, someCursor, nil))
		if r.code != ExitTransient || r.cursor != "" || len(r.events) != 0 {
			t.Fatalf("body=%q code=%d out=%q", body, r.code, r.stdout)
		}
	}
}

func TestUnreachableLeavesCursorAndExitsTransient(t *testing.T) {
	s := serve(t, 200, rssFeed)
	url := s.URL
	s.Close()
	r := call(t, in(url, someCursor, nil))
	if r.code != ExitTransient || r.stdout != "" {
		t.Fatalf("code=%d out=%q", r.code, r.stdout)
	}
}

func TestHTTPStatusMapping(t *testing.T) {
	for status, want := range map[int]int{500: ExitTransient, 503: ExitTransient, 429: ExitTransient, 401: ExitAuth, 403: ExitAuth, 404: ExitPermanent, 410: ExitPermanent} {
		s := serve(t, status, "x")
		r := call(t, in(s.URL, someCursor, nil))
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

// someCursor 是一个合法游标，用于只关心失败路径的用例。
const someCursor = `{"t":"2023-01-01T00:00:00Z","id":"x"}`

func ids(r result) []string {
	var out []string
	for _, e := range r.events {
		out = append(out, e["id"].(string))
	}
	return out
}

func item(guid, date string) string {
	return `<item><guid>` + guid + `</guid><title>t-` + guid + `</title><pubDate>` + date + `</pubDate></item>`
}

func rss(items ...string) string {
	return `<rss version="2.0"><channel>` + strings.Join(items, "") + `</channel></rss>`
}

func decodeCursor(t *testing.T, s string) cursor {
	t.Helper()
	var c cursor
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatalf("游标 %q: %v", s, err)
	}
	return c
}

func TestRSSFirstRunEmitsAllOldestFirstAndAdvancesCursor(t *testing.T) {
	s := serve(t, 200, rssFeed)
	r := call(t, in(s.URL, "", nil))
	if r.code != 0 || fmt.Sprint(ids(r)) != "[g-1 g-2]" {
		t.Fatalf("code=%d out=%s", r.code, r.stdout)
	}
	e := r.events[1]
	if e["subject"] != "New" || e["link"] != "https://e.com/2" || e["summary"] != "new desc" || e["detail"] != "full body" ||
		e["sender"] != "alice" || e["type"] != "feed.entry" || e["occurred_at"] != "2023-01-03T15:04:05Z" {
		t.Fatalf("事件映射不对: %v", e)
	}
	if c := decodeCursor(t, r.cursor); c.ID != "g-2" || c.T.Format(time.RFC3339) != "2023-01-03T15:04:05Z" || c.More {
		t.Fatalf("cursor=%q", r.cursor)
	}
}

// 游标之后的条目照发；重叠窗口内已越过的条目重发（id 不变，内核去重），窗口之外的不再发。
func TestRunWithCursorEmitsNewAndOverlapOnly(t *testing.T) {
	body := rss(item("ancient", "Mon, 02 Jan 2022 15:04:05 +0000"), item("g-1", "Mon, 02 Jan 2023 15:04:05 +0000"))
	s := serveCT(t, 200, "", &body)
	r := call(t, in(s.URL, "", nil))
	if fmt.Sprint(ids(r)) != "[ancient g-1]" {
		t.Fatalf("out=%s", r.stdout)
	}
	body = rss(item("ancient", "Mon, 02 Jan 2022 15:04:05 +0000"), item("g-1", "Mon, 02 Jan 2023 15:04:05 +0000"), item("g-2", "Tue, 03 Jan 2023 15:04:05 +0000"))
	r2 := call(t, in(s.URL, r.cursor, nil))
	if r2.code != 0 || fmt.Sprint(ids(r2)) != "[g-1 g-2]" || decodeCursor(t, r2.cursor).ID != "g-2" {
		t.Fatalf("out=%s", r2.stdout)
	}
	if r3 := call(t, in(s.URL, r2.cursor, nil)); r3.cursor != r2.cursor {
		t.Fatalf("没有新条目时游标应保持: %s", r3.stdout)
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
	if a2["occurred_at"] != "2023-02-03T10:00:00Z" || decodeCursor(t, r.cursor).ID != "urn:a2" {
		t.Fatalf("a2=%v cursor=%s", a2, r.cursor)
	}
}

// 评审第 1–3 轮 blocker（输出超 64KB、seen 列表无界、maxSeen 丢条目）：内核已收下全部 stdout，
// 这里不再有字节预算。大于旧 64KB 的输出一次发完；条目数上限只把积压分批，游标定长，
// 分批读完之前每条恰好一次。
func TestLargeOutputEmittedInOneRun(t *testing.T) {
	var items []string
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprintf(`<item><guid>g%02d</guid><title>%s</title><pubDate>Tue, 03 Jan 2023 15:%02d:05 +0000</pubDate><description>%s</description></item>`,
			i, strings.Repeat("标", 250), i, strings.Repeat("&lt;p&gt;正文&lt;/p&gt;", 500)))
	}
	s := serve(t, 200, rss(items...))
	r := call(t, in(s.URL, "", nil))
	if r.code != 0 || len(r.events) != 60 || len(r.stdout) < 128<<10 || decodeCursor(t, r.cursor).ID != "g59" {
		t.Fatalf("code=%d events=%d bytes=%d", r.code, len(r.events), len(r.stdout))
	}
}

func TestBacklogBeyondCapDrainsExactlyOnce(t *testing.T) {
	var items []string
	for i := 0; i < 3000; i++ { // 同一发布时间的大组，夹着前后不同时间的条目
		items = append(items, item(fmt.Sprintf("https://e.com/%s/%04d", strings.Repeat("p", 80), i), "Tue, 03 Jan 2023 00:00:00 +0000"))
	}
	items = append(items, item("early", "Mon, 02 Jan 2023 00:00:00 +0000"), item("late", "Wed, 04 Jan 2023 00:00:00 +0000"))
	s := serve(t, 200, rss(items...))
	got, c := map[string]int{}, ""
	for round := 0; ; round++ {
		r := call(t, in(s.URL, c, map[string]any{"max_items": 200}))
		if r.code != 0 || len(r.events) > 200 || len(r.cursor) > 256 {
			t.Fatalf("round %d code=%d events=%d cursor=%d字节", round, r.code, len(r.events), len(r.cursor))
		}
		for _, id := range ids(r) {
			got[id]++
		}
		c = r.cursor
		if !decodeCursor(t, c).More {
			break
		}
		if round > 20 {
			t.Fatal("没有收敛")
		}
	}
	if len(got) != 3002 || got["early"] != 1 || got["late"] != 1 {
		t.Fatalf("应取全: %d", len(got))
	}
	for id, n := range got {
		if n != 1 {
			t.Fatalf("%s 输出 %d 次", id, n)
		}
	}
	if cc := decodeCursor(t, c); cc.ID != "late" {
		t.Fatalf("cursor=%s", c)
	}
}

// 评审第 4 轮 blocker：游标之后才出现、发布时间等于游标时刻的条目（只写日期的博客同一天发第二篇），
// 无论 id 排在前后都要发出；发布时间早于游标但在重叠窗口内的迟到条目同样要发出。
func TestSameTimeAndLateEntriesAfterCursorAreEmitted(t *testing.T) {
	day := "Tue, 03 Jan 2023 00:00:00 +0000"
	body := rss(item("m", day))
	s := serveCT(t, 200, "", &body)
	r := call(t, in(s.URL, "", nil))
	body = rss(item("a", day), item("m", day), item("z", day), item("back", "Mon, 02 Jan 2023 12:00:00 +0000"))
	r2 := call(t, in(s.URL, r.cursor, nil))
	got := map[string]bool{}
	for _, id := range ids(r2) {
		got[id] = true
	}
	if r2.code != 0 || !got["a"] || !got["z"] || !got["back"] {
		t.Fatalf("同刻与迟到条目应发出: %s", r2.stdout)
	}
}

// 评审第 1 轮 blocker（严格 XML、无字符集处理）与字符集 nit：XML 声明给出的非 UTF-8 字符集、
// 没有声明时 HTTP Content-Type 给出的字符集都能解析，HTML 实体与裸 & 照常接受。
func TestNonUTF8AndLenientFeeds(t *testing.T) {
	gbk := "\xd6\xd0\xce\xc4"  // "中文"
	sjis := "\x93\xfa\x96\x7b" // "日本"
	for _, tc := range []struct{ name, ctype, body, want string }{
		{"latin1 声明 + 实体 + 裸 &", "", "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?>" + rss("<item><guid>q</guid><title>caf\xe9 &nbsp;R&D & co</title></item>"), "café \u00a0R&D & co"},
		{"GBK 声明", "", "<?xml version=\"1.0\" encoding=\"GBK\"?>" + rss("<item><guid>q</guid><title>"+gbk+"</title></item>"), "中文"},
		{"Shift_JIS 声明", "", "<?xml version=\"1.0\" encoding=\"Shift_JIS\"?>" + rss("<item><guid>q</guid><title>"+sjis+"</title></item>"), "日本"},
		{"只有 HTTP charset", "application/rss+xml; charset=gb2312", rss("<item><guid>q</guid><title>" + gbk + "</title></item>"), "中文"},
		{"XML 声明优先于服务器默认的 charset", "text/xml; charset=utf-8", "<?xml version=\"1.0\" encoding=\"GBK\"?>" + rss("<item><guid>q</guid><title>"+gbk+"</title></item>"), "中文"},
	} {
		body := tc.body
		s := serveCT(t, 200, tc.ctype, &body)
		r := call(t, in(s.URL, "", nil))
		if r.code != 0 || len(r.events) != 1 || r.events[0]["subject"] != tc.want {
			t.Errorf("%s: code=%d out=%q", tc.name, r.code, r.stdout)
		}
	}
	body := "<?xml version=\"1.0\" encoding=\"x-no-such-charset\"?><rss><channel/></rss>"
	s := serve(t, 200, body)
	if r := call(t, in(s.URL, someCursor, nil)); r.code != ExitTransient || r.stdout != "" {
		t.Fatalf("未知字符集应失败且游标不动: %d %q", r.code, r.stdout)
	}
}

// nit：RSS 1.0（rdf:RDF 根、item 在 channel 外、dc:date）与 Atom 的 xhtml 正文。
func TestRSS1AndAtomXHTML(t *testing.T) {
	s := serve(t, 200, `<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#" xmlns="http://purl.org/rss/1.0/" xmlns:dc="http://purl.org/dc/elements/1.1/">
<channel rdf:about="https://e.com/"><title>t</title></channel>
<item rdf:about="https://e.com/r1"><title>R1</title><link>https://e.com/r1</link><dc:date>2023-01-03T10:00:00+08:00</dc:date></item></rdf:RDF>`)
	r := call(t, in(s.URL, "", nil))
	if r.code != 0 || len(r.events) != 1 || r.events[0]["id"] != "https://e.com/r1" || r.events[0]["occurred_at"] != "2023-01-03T02:00:00Z" {
		t.Fatalf("RSS 1.0: code=%d out=%s", r.code, r.stdout)
	}
	s = serve(t, 200, `<feed xmlns="http://www.w3.org/2005/Atom"><entry><id>x</id><title>X</title><updated>2023-01-03T00:00:00Z</updated>
<content type="xhtml"><div xmlns="http://www.w3.org/1999/xhtml">hi <b>there</b></div></content></entry></feed>`)
	if r := call(t, in(s.URL, "", nil)); r.code != 0 || !strings.Contains(fmt.Sprint(r.events[0]["detail"]), "there") {
		t.Fatalf("xhtml 正文丢了: %s", r.stdout)
	}
}

// 事件 id 只由条目决定：guid/id，其次 link，再其次内容哈希；跨次运行不变。
func TestEventIDFallbackIsStable(t *testing.T) {
	body := rss(`<item><title>with link</title><link>https://e.com/l</link></item>`, `<item><title>bare</title><description>d</description></item>`)
	s := serve(t, 200, body)
	a, b := call(t, in(s.URL, "", nil)), call(t, in(s.URL, "", nil))
	if a.code != 0 || len(a.events) != 2 || a.events[0]["id"] != "https://e.com/l" || !strings.HasPrefix(a.events[1]["id"].(string), "sha256:") ||
		fmt.Sprint(ids(a)) != fmt.Sprint(ids(b)) {
		t.Fatalf("a=%v b=%v", ids(a), ids(b))
	}
}

// nit：无日期条目每次都发、不计入上限，也不会把有日期条目挤掉；游标只由有日期条目推进。
func TestUndatedEntriesDoNotStarveDated(t *testing.T) {
	var items []string
	for i := 0; i < 60; i++ {
		items = append(items, fmt.Sprintf(`<item><guid>u%02d</guid><title>u</title><description>%s</description></item>`, i, strings.Repeat("字", 1500)))
	}
	items = append(items, item("d1", "Tue, 03 Jan 2023 15:04:05 +0000"))
	s := serve(t, 200, rss(items...))
	r := call(t, in(s.URL, "", map[string]any{"max_items": 1}))
	if r.code != 0 || len(r.events) != 61 || decodeCursor(t, r.cursor).ID != "d1" {
		t.Fatalf("code=%d events=%d cursor=%s", r.code, len(r.events), r.cursor)
	}
}

func TestBadCursorIsConfigFailure(t *testing.T) {
	s := serve(t, 200, rssFeed)
	if r := call(t, in(s.URL, "2023-01-01T00:00:00Z", nil)); r.code != ExitBadConfig || r.stdout != "" {
		t.Fatalf("code=%d out=%q", r.code, r.stdout)
	}
}
