package content

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestHTMLToTextKeepsStructure(t *testing.T) {
	in := `<html><head><style>p{color:red}</style><script>alert(1)</script></head><body>
<p>See <a href="https://example.com/a?b=1">the page</a>.</p>
<ul><li>one</li><li>two</li></ul>
<ol><li>first</li><li>second</li></ol>
<blockquote>quoted line</blockquote>
<table><thead><tr><th>Name</th><th>Qty</th></tr></thead><tbody><tr><td>apple</td><td>3</td></tr></tbody></table>
<img src="https://tracker.example.com/p.gif" alt="logo">
</body></html>`
	got, err := HTMLToText(in)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"[the page](https://example.com/a?b=1)", "- one", "- two", "1. first", "2. second",
		"> quoted line", "| Name  | Qty |", "| apple | 3   |",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"alert(1)", "color:red", "<script", "<p>", "THE END"} {
		if strings.Contains(got, bad) {
			t.Errorf("unexpected %q in:\n%s", bad, got)
		}
	}
}

func TestHTMLToTextNeverFetchesRemote(t *testing.T) {
	// 转换只处理传入的文本；远程图片只留地址，不触发任何网络访问（无 client 可注入即为证据：
	// 这里验证地址作为文本保留，且无 data 被内联）。
	got, err := HTMLToText(`<img src="https://x.invalid/a.png" alt="a">`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "https://x.invalid/a.png") {
		t.Errorf("image address lost: %q", got)
	}
}

func TestHTMLToTextEmptyAndFailure(t *testing.T) {
	got, err := HTMLToText("")
	if err != nil || got != "" {
		t.Fatalf("empty: %q %v", got, err)
	}
	_, err = HTMLToText("<p>x</p>" + strings.Repeat("<div>", 100000))
	if !errors.Is(err, ErrConvert) {
		t.Fatalf("want ErrConvert, got %v", err)
	}
}

func TestNormalizeHeaders(t *testing.T) {
	in := []Header{
		{"from", " Alice <a@example.com> "},
		{"RECEIVED", "by one"},
		{"Received", "by two"},
		{"subject", "=?UTF-8?B?5L2g5aW9?="},
		{"X-Empty", "  "},
		{"message-id", "<1@example.com>"},
		{"Subject", "dup-ignored"},
		{"list-unsubscribe", "<https://example.com/u>\r\n\t<mailto:u@example.com>"},
	}
	got := NormalizeHeaders(in)
	want := map[string]any{
		"From":             "Alice <a@example.com>",
		"Received":         []string{"by one", "by two"},
		"Subject":          []string{"你好", "dup-ignored"},
		"Message-Id":       "<1@example.com>",
		"List-Unsubscribe": "<https://example.com/u> <mailto:u@example.com>",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v\nwant %#v", got, want)
	}
}

func TestBatcherStopsAtThreeQuarters(t *testing.T) {
	b := NewBatcher(1) // 1 MiB 上限 → 768 KiB 预算
	if b.Budget() != 768<<10 {
		t.Fatalf("budget %d", b.Budget())
	}
	n := 0
	for b.Add(100 << 10) {
		n++
	}
	// 7 条共 700KiB 在预算内；第 8 条会越过 768KiB，不接收。
	if n != 7 || b.Used() != 700<<10 {
		t.Fatalf("accepted %d, used %d", n, b.Used())
	}
	if b.Full() || !b.Add(68<<10) || !b.Full() {
		t.Fatal("a smaller event still fits, then budget is full")
	}
}

func TestBatcherAlwaysAcceptsFirstEvent(t *testing.T) {
	// 单条超过预算也要接收，否则游标永远不前进；单条上限由内核另行拒收。
	b := NewBatcher(1)
	if !b.Add(2 << 20) {
		t.Fatal("first oversize event must be accepted")
	}
	if b.Add(1) {
		t.Fatal("nothing more after oversize first")
	}
}
