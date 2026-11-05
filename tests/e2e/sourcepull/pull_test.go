//go:build e2e

package sourcepull

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// feedServer 是本地替身：固定地址上的 HTTP 服务，响应体和状态码可随时改，也可停掉再原地重启。
type feedServer struct {
	t    *testing.T
	addr string

	mu     sync.Mutex
	status int
	ctype  string
	body   string
	srv    *http.Server
}

func newFeedServer(t *testing.T) *feedServer {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &feedServer{t: t, addr: l.Addr().String(), status: 200}
	s.serve(l)
	t.Cleanup(s.stop)
	return s
}

func (s *feedServer) serve(l net.Listener) {
	s.srv = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		status, ctype, body := s.status, s.ctype, s.body
		s.mu.Unlock()
		if ctype != "" {
			w.Header().Set("Content-Type", ctype)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})}
	go s.srv.Serve(l)
}

func (s *feedServer) url() string { return "http://" + s.addr + "/feed" }

func (s *feedServer) set(status int, ctype, body string) {
	s.mu.Lock()
	s.status, s.ctype, s.body = status, ctype, body
	s.mu.Unlock()
}

func (s *feedServer) stop() {
	if s.srv != nil {
		_ = s.srv.Close()
		s.srv = nil
	}
}

func (s *feedServer) start() {
	s.t.Helper()
	l, err := net.Listen("tcp", s.addr)
	if err != nil {
		s.t.Fatal(err)
	}
	s.serve(l)
}

// addPull 用 `apa source add` 登记附带的拉取程序（type=pull，DEL 可登记）。
func (f *fx) addPull(id, program, sourceType string, config any) {
	f.t.Helper()
	cfg, _ := json.Marshal(config)
	f.apa(f.DEL, 0, "source", "add", "--id", id, "--type", "pull", "--command", program,
		"--schedule", "1s", "--timeout", "20", "--source-type", sourceType, "--config", string(cfg))
}

// patchConfig 在 Web 改来源的 config。
func (f *fx) patchConfig(id string, config any) {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"config": config})
	if r := f.Web("PATCH", "/v1/sources/"+id, string(b)); r.Code != 200 {
		f.t.Fatalf("改 %s 的 config: HTTP %d %s", id, r.Code, r.Body)
	}
}

func steps(t *testing.T) func(string, func(*testing.T)) {
	return func(name string, fn func(*testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}
}

func rssFeed(guids ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><rss version="2.0"><channel><title>t</title><link>http://x</link><description>d</description>`)
	for i, g := range guids {
		fmt.Fprintf(&b, `<item><guid>%s</guid><title>标题 %s</title><link>http://x/%s</link><pubDate>%s</pubDate></item>`,
			g, g, g, time.Date(2030, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC1123Z))
	}
	return b.String() + `</channel></rss>`
}

func atomFeed(ids ...string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>t</title><id>urn:feed</id><updated>2030-01-01T00:00:00Z</updated>`)
	for i, g := range ids {
		fmt.Fprintf(&b, `<entry><id>%s</id><title>标题 %s</title><link href="http://x/%s"/><updated>%s</updated></entry>`,
			g, g, g, time.Date(2030, 1, 1, 0, i, 0, 0, time.UTC).Format(time.RFC3339))
	}
	return b.String() + `</feed>`
}

func TestFlowSourceRSS(t *testing.T) {
	f := setup(t)
	srv := newFeedServer(t)
	step := steps(t)

	step("ACT-RSS-01 拉取新条目（RSS 与 Atom）", func(t *testing.T) {
		for _, c := range []struct {
			id, ctype string
			feed      func(...string) string
		}{{"src_rss", "application/rss+xml", rssFeed}, {"src_atom", "application/atom+xml", atomFeed}} {
			srv.set(200, c.ctype, c.feed("a", "b"))
			f.addPull(c.id, "rss-source", "rss", map[string]any{"url": srv.url()})
			f.until(c.id+" 拉到两条", func() bool { return len(f.events(c.id)) == 2 })
			srv.set(200, c.ctype, c.feed("a", "b", "c"))
			f.until(c.id+" 拉到新增的一条", func() bool { return len(f.events(c.id)) == 3 })
			f.settle(c.id, 2)
			got := ids(f.events(c.id))
			if len(got) != 3 || !contains(got, "c") || count(got, "a") != 1 || count(got, "b") != 1 {
				t.Fatalf("%s 每个条目应只有一条事件: %v", c.id, got)
			}
			if f.show(c.id).Stats.LastSuccessAt == "" {
				t.Errorf("%s 应有最近成功时间", c.id)
			}
			f.apa(f.DEL, 0, "source", "disable", c.id)
		}
	})

	step("ACT-RSS-02 格式错误或不可达时不丢游标", func(t *testing.T) {
		f.apa(f.DEL, 0, "source", "enable", "src_rss")
		srv.set(200, "application/rss+xml", rssFeed("a", "b", "c"))
		f.settle("src_rss", 1)
		before := f.show("src_rss")
		n := len(f.events("src_rss"))
		check := func(why string) {
			t.Helper()
			f.until(why+"：来源页可见失败", func() bool { return f.show("src_rss").Stats.ConsecutiveFailures >= 1 })
			s := f.show("src_rss")
			if s.Cursor != before.Cursor || len(f.events("src_rss")) != n || s.Stats.LastError == "" {
				t.Fatalf("%s：游标/事件应不动且有失败原因: cursor %q→%q events %d→%d err=%q", why, before.Cursor, s.Cursor, n, len(f.events("src_rss")), s.Stats.LastError)
			}
		}
		srv.set(200, "application/rss+xml", "<rss><channel><item>")
		check("非法 XML")
		srv.set(200, "application/rss+xml", rssFeed("a", "b", "c", "d"))
		f.until("恢复", func() bool {
			return f.show("src_rss").Stats.ConsecutiveFailures == 0 && len(f.events("src_rss")) == n+1
		})
		n = len(f.events("src_rss"))
		before = f.show("src_rss")
		srv.stop()
		check("服务不可达")
		srv.set(200, "application/rss+xml", rssFeed("a", "b", "c", "d", "e"))
		srv.start()
		f.until("恢复后拉到错过的条目", func() bool { return contains(ids(f.events("src_rss")), "e") })
		if got := ids(f.events("src_rss")); len(got) != 5 {
			t.Fatalf("恢复后共 5 条且不重复: %v", got)
		}
	})
}

func TestFlowSourceHTTPJSON(t *testing.T) {
	f := setup(t)
	srv := newFeedServer(t)
	step := steps(t)
	rows := func(ns ...int) string {
		var parts []string
		for _, n := range ns {
			parts = append(parts, fmt.Sprintf(`{"uid":"u%d","kind":"ticket","headline":"工单 %d","at":"2030-01-0%dT00:00:00Z"}`, n, n, n))
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	cfg := map[string]any{"url": srv.url(), "mapping": map[string]any{
		"id": "uid", "type": "kind", "subject": "headline", "occurred_at": "at"}}

	step("ACT-HTTP-01 轮询并映射", func(t *testing.T) {
		srv.set(200, "application/json", rows(1, 2))
		f.addPull("src_http", "http-json", "ticket", cfg)
		f.until("拉到两条", func() bool { return len(f.events("src_http")) == 2 })
		srv.set(200, "application/json", rows(1, 2, 3))
		f.until("拉到新增的一条", func() bool { return len(f.events("src_http")) == 3 })
		f.settle("src_http", 2)
		evs := f.events("src_http")
		if len(evs) != 3 {
			t.Fatalf("只应入新增的那条: %v", ids(evs))
		}
		for _, e := range evs {
			var n int
			fmt.Sscanf(e.Key.EventID, "u%d", &n)
			if n == 0 || e.Type != "ticket" || e.Subject != fmt.Sprintf("工单 %d", n) || !strings.HasPrefix(e.OccurredAt, fmt.Sprintf("2030-01-0%dT00:00:00", n)) {
				t.Errorf("字段映射错误: %+v", e)
			}
		}
	})

	step("ACT-HTTP-02 HTTP 错误时按失败处理", func(t *testing.T) {
		before, n := f.show("src_http"), len(f.events("src_http"))
		srv.set(500, "text/plain", "boom")
		f.until("500 计入连续失败", func() bool { return f.show("src_http").Stats.ConsecutiveFailures >= 2 })
		s := f.show("src_http")
		if s.Cursor != before.Cursor || len(f.events("src_http")) != n || s.Stats.NeedsInputItem != "" || !s.Enabled {
			t.Fatalf("500 应按临时失败处理（不动游标、不找本人、不停用）: %+v", s)
		}
		srv.set(401, "text/plain", "no")
		f.until("401 生成 needs_input", func() bool { return f.show("src_http").Stats.NeedsInputItem != "" })
		s = f.show("src_http")
		if !f.needsInputOf(s.Stats.NeedsInputItem) || s.Cursor != before.Cursor || len(f.events("src_http")) != n {
			t.Fatalf("401 应生成 needs_input 且游标不动: %+v", s)
		}
	})
}

func TestFlowSourceDirWatch(t *testing.T) {
	f := setup(t)
	step := steps(t)
	dir := filepath.Join(t.TempDir(), "watch")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "a.md")
	// 把 mtime 显式拉开，避免依赖文件系统的时间粒度。
	touch := func(content string, at time.Time) {
		t.Helper()
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(-time.Hour)

	step("ACT-DIR-01 新建和修改文件产生事件", func(t *testing.T) {
		f.addPull("src_dir", "dir-watch", "dir", map[string]any{"dir": dir})
		f.settle("src_dir", 2)
		if n := len(f.events("src_dir")); n != 0 {
			t.Fatalf("空目录不应有事件: %d", n)
		}
		touch("v1", base)
		f.until("新建产生一条事件", func() bool { return len(f.events("src_dir")) == 1 })
		f.settle("src_dir", 2)
		if n := len(f.events("src_dir")); n != 1 {
			t.Fatalf("未变更时不应重复: %d", n)
		}
		touch("v2", base.Add(time.Minute))
		f.until("修改产生一条事件", func() bool { return len(f.events("src_dir")) == 2 })
		f.settle("src_dir", 2)
		evs := f.events("src_dir")
		if len(evs) != 2 || evs[0].Key.EventID == evs[1].Key.EventID {
			t.Fatalf("新建和修改应各有一条不同的事件: %v", ids(evs))
		}
	})

	step("ACT-DIR-02 目录不存在时报告配置失败", func(t *testing.T) {
		before, n := f.show("src_dir"), len(f.events("src_dir"))
		f.patchConfig("src_dir", map[string]any{"dir": filepath.Join(dir, "missing")})
		f.until("生成 needs_input", func() bool { return f.show("src_dir").Stats.NeedsInputItem != "" })
		s := f.show("src_dir")
		if !f.needsInputOf(s.Stats.NeedsInputItem) || s.Cursor != before.Cursor || len(f.events("src_dir")) != n {
			t.Fatalf("应生成 needs_input 且游标不动、无新事件: %+v", s)
		}
	})
}
