//go:build e2e

package sourcesv2

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// rssSource 构建 rss-source、起一个提供录制样本的订阅服务，并登记来源。
// 样本里的 @REMOTE@ 换成一个计数服务器的地址（正文引用的远程资源），返回请求计数。
func (k *kit) rssSource(id, sample string) *atomic.Int32 {
	k.t.Helper()
	programs(k.t)
	bin := filepath.Join(progDir, "rss-source")
	if _, err := os.Stat(bin); err != nil {
		cmd := exec.Command("go", "build", "-o", bin, "./sources/rss/cmd/rss-source")
		cmd.Dir = repoRoot()
		if out, err := cmd.CombinedOutput(); err != nil {
			k.t.Fatalf("go build rss-source: %v\n%s", err, out)
		}
	}
	feed, err := os.ReadFile(filepath.Join(repoRoot(), "sources", "rss", "testdata", sample))
	if err != nil {
		k.t.Fatal(err)
	}
	var remoteHits atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { remoteHits.Add(1) }))
	k.t.Cleanup(remote.Close)
	feed = []byte(strings.ReplaceAll(string(feed), "@REMOTE@", remote.URL))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/rss+xml")
		_, _ = w.Write(feed)
	}))
	k.t.Cleanup(srv.Close)
	k.webSource(map[string]any{"id": id, "name": id, "type": "script", "command": bin, "schedule": "1s", "timeout": 20,
		"source_type": "rss", "dedupe_domain": "feed-" + id, "default_project": k.Projects["acc-home"],
		"config": map[string]any{"url": srv.URL}})
	return &remoteHits
}

// e2e-source-raw（rss 部分）：ACT-OAD-02 全文转文本进 body、HTML 原样进 original、标题与作者不截、附带文件进 attachments，
// 只给摘要的条目不算 partial；ACT-OAD-03 HTML 转换失败时 body 留空、original 保存并以 partial 自报。
func TestFlowOtherAdaptersRawRSS(t *testing.T) {
	k := setup(t)
	k.step("ACT-OAD-02 全文、原文、不截断的标题与作者、附带文件", func(t *testing.T) {
		hits := k.rssSource("src_rss_long", "rss-long.xml")
		ev := k.settled("src_rss_long", "rss-long-1")
		r := k.fullEvent(ev)
		if r.Content.State != "full" {
			t.Errorf("取全了应是 full: %q", r.Content.State)
		}
		if len(r.Body) <= 4000 || !strings.Contains(r.Body, "THE-END-MARKER") || !strings.Contains(r.Body, "(https://example.com/p/059)") || strings.Contains(r.Body, "alert(1)") {
			t.Errorf("body 应是未截断的转换后文本: %d 字节", len(r.Body))
		}
		if r.Original.MediaType != "text/html" || !strings.Contains(r.Original.Text, "<script>alert(1)</script>") || !strings.Contains(r.Original.Text, "<p>THE-END-MARKER</p>") {
			t.Errorf("original 应是原样 HTML: %q", r.Original.MediaType)
		}
		if ev.Subject != strings.Repeat("T", 350) || ev.Sender != strings.Repeat("A", 350) {
			t.Errorf("标题与作者不应截断: %d %d", len(ev.Subject), len(ev.Sender))
		}
		if len(r.Attachments) != 2 || r.Attachments[0]["name"] != "https://example.com/files/report.pdf" || r.Attachments[0]["media_type"] != "application/pdf" ||
			r.Attachments[0]["size"] != float64(123456) || r.Attachments[0]["ref"] != nil || r.Attachments[1]["name"] != "https://example.com/files/audio.mp3" {
			t.Errorf("attachments: %v", r.Attachments)
		}
		for _, want := range []string{"| Name  | Qty |", "| apple | 3   |", "/pixel.png)", "/more)"} {
			if !strings.Contains(r.Body, want) {
				t.Errorf("body 缺 %q（表格结构与远程资源地址应保留）", want)
			}
		}
		if n := hits.Load(); n != 0 {
			t.Errorf("转换不能取远程资源，实际请求 %d 次", n)
		}
		// 订阅源只给摘要：取不到全文是对方的限制，不算 partial。
		s := k.fullEvent(k.settled("src_rss_long", "rss-summary-1"))
		if s.Content.State != "full" || s.Body != "Only a **teaser** here." || s.Original.Text != "<p>Only a <b>teaser</b> here.</p>" {
			t.Errorf("摘要条目: state=%q body=%q original=%q", s.Content.State, s.Body, s.Original.Text)
		}
	})
	k.step("ACT-OAD-03 HTML 转换失败：body 留空、original 保存、partial 自报", func(t *testing.T) {
		k.rssSource("src_rss_broken", "rss-broken.xml")
		ev := k.settled("src_rss_broken", "rss-broken-1")
		raw := k.rawRead(ev.ID)
		if raw.Body != nil && *raw.Body != "" {
			t.Errorf("body 应为空: %d 字节", len(*raw.Body))
		}
		if raw.Content.State != "partial" || len(raw.Content.Missing) != 1 || raw.Content.Missing[0].Part != "body" || raw.Content.Missing[0].Reason == "" {
			t.Errorf("content 应为 partial 并列出 body 及原因: %+v", raw.Content)
		}
		if f := k.fullEvent(ev); !strings.HasPrefix(f.Original.Text, "<p>x</p><div>") || f.Original.MediaType != "text/html" {
			t.Errorf("original 应照常保存: %q", f.Original.MediaType)
		}
	})
}
