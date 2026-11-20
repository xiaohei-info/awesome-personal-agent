package serve

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// barkStub 是本地替身：模拟推送服务的 /ping 与 /push，记下收到的推送。不连任何真实服务。
type barkStub struct {
	mu  sync.Mutex
	got []map[string]any
}

func (b *barkStub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/push" {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		b.mu.Lock()
		b.got = append(b.got, m)
		b.mu.Unlock()
	}
	json.NewEncoder(w).Encode(map[string]any{"code": 200, "message": "success"})
}

// waitPush 等替身收到标题为 title 的推送。
func (b *barkStub) waitPush(t *testing.T, title string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		for _, m := range b.got {
			if m["title"] == title {
				b.mu.Unlock()
				return m
			}
		}
		b.mu.Unlock()
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no push titled %q; got %v", title, b.got)
	return nil
}

func (s *session) json(method, p string, body any, want int) map[string]any {
	s.t.Helper()
	b, _ := json.Marshal(body)
	if body == nil {
		b = nil
	}
	code, out := s.do(method, p, string(b))
	if code != want {
		s.t.Fatalf("%s %s: %d %s", method, p, code, out)
	}
	var m map[string]any
	json.Unmarshal([]byte(out), &m)
	return m
}

// PR2 验收：添加 Bark → 测试 → 启用 → 建决策请求 → 替身收到推送，点开是需要登录的 Web 链接（一次性表单已取消）→ 本人在 Web 作答；
// 不再有高风险类：带 money 标签的问题同样推送，链接同样落在事项页。
func TestBarkChannelEndToEndWithStandIn(t *testing.T) {
	stub := &barkStub{}
	srv := httptest.NewServer(stub)
	defer srv.Close()

	home := initOnFreePort(t)
	addr, stop := start(t, home)
	defer stop()
	s := webLogin(t, addr)

	draft := map[string]any{"type": "bark", "config": map[string]string{"server": srv.URL}, "secrets": map[string]string{"device_key": "stand-in-key"}, "send": true}
	if r := s.json("POST", "/connections/test", draft, 200); r["ok"] != true {
		t.Fatalf("test: %v", r)
	}
	if m := stub.waitPush(t, "APA 测试提醒"); m["device_key"] != "stand-in-key" {
		t.Fatalf("test push = %v", m)
	}
	c := s.json("POST", "/connections", map[string]any{"type": "bark", "name": "我的 iPhone", "config": map[string]string{"server": srv.URL},
		"secrets": map[string]string{"device_key": "stand-in-key"}, "enabled": true}, 201)
	id, _ := c["id"].(string)

	// 「用它来送」：等你决定的提醒走这条渠道。
	pol := s.json("GET", "/settings/policy", nil, 200)
	disturb := pol["disturb"].(map[string]any)
	disturb["routes"].(map[string]any)["decision"] = "channel:" + id
	s.json("PUT", "/settings/policy", map[string]any{"disturb": disturb}, 200)

	ask := func(title string, tags []string) string {
		it := s.json("POST", "/items", map[string]any{"kind": "task", "title": title, "tags": tags, "phase": "open"}, 201)
		iid := it["id"].(string)
		s.json("POST", "/items/"+iid+"/ask", map[string]any{"ask": map[string]any{"question": title,
			"options": []map[string]string{{"id": "A", "label": "方案A"}, {"id": "B", "label": "方案B"}}, "recommend": "A"}}, 200)
		return iid
	}
	plain := ask("选哪个方案", nil)
	guarded := ask("付不付这笔钱", []string{"money"})

	base := "http://" + addr.String()
	m := stub.waitPush(t, "选哪个方案")
	if u, _ := m["url"].(string); u != base+"/items/"+plain || strings.Contains(mustJSON(m), "/forms/") || m["level"] != "timeSensitive" {
		t.Fatalf("decision push = %v", m)
	}
	s.json("POST", "/items/"+plain+"/decide", map[string]any{"choice": "A"}, 200)
	if it := s.json("GET", "/items/"+plain, nil, 200); !strings.Contains(mustJSON(it), `"choice":"A"`) {
		t.Fatalf("decision not resolved: %v", it)
	}

	w := stub.waitPush(t, "付不付这笔钱")
	if u, _ := w["url"].(string); u != base+"/items/"+guarded || strings.Contains(mustJSON(w), "/forms/") {
		t.Fatalf("money push = %v", w)
	}

	// 卡片现状：在用、最近成功
	l := s.json("GET", "/connections?direction=channel", nil, 200)
	cs := l["connections"].([]any)
	if len(cs) != 1 || cs[0].(map[string]any)["state"] != "ok" || cs[0].(map[string]any)["last_ok_at"] == nil {
		t.Fatalf("connections = %v", cs)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
