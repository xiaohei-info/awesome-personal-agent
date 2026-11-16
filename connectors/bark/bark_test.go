package bark

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
)

// stub 是本地替身：记下收到的推送，按 status 回应。
type stub struct {
	mu     sync.Mutex
	got    []map[string]any
	auth   []string
	status int
}

func (s *stub) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, p, _ := r.BasicAuth()
	s.auth = append(s.auth, u+":"+p)
	if r.URL.Path == "/push" {
		var m map[string]any
		json.NewDecoder(r.Body).Decode(&m)
		s.got = append(s.got, m)
	}
	st := s.status
	if st == 0 {
		st = 200
	}
	w.WriteHeader(st)
	json.NewEncoder(w).Encode(map[string]any{"code": st, "message": "stub " + r.URL.Path})
}

func newCh(t *testing.T, s *stub, settings, secrets map[string]string) conn.Channel {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handler))
	t.Cleanup(srv.Close)
	cfg := conn.Config{Settings: map[string]string{"server": srv.URL + "/", "group": "APA", "time_sensitive": "true"}, Secrets: map[string]string{"device_key": "dev-KEY"}}
	for k, v := range settings {
		cfg.Settings[k] = v
	}
	for k, v := range secrets {
		cfg.Secrets[k] = v
	}
	ch, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func TestDeclarationIsValid(t *testing.T) {
	p := Plugin()
	if p.Decl.Type != "bark" || !p.Decl.Capabilities.ViewOnly() || len(p.Decl.SecretKeys()) != 2 {
		t.Fatalf("decl = %+v", p.Decl)
	}
}

// 决策：点通知打开表单链接，用时效性通知；web_only：只带 Web 链接；正文含推荐、过长截断；不用的字段不发。
func TestSendMapsMessage(t *testing.T) {
	s := &stub{}
	ch := newCh(t, s, nil, nil)
	ctx := context.Background()
	rc, err := ch.Send(ctx, conn.Message{Key: "d1@1", Class: "decision", Title: "要不要合并", Summary: "两个方案", Recommend: "合并",
		WebURL: "https://apa.example.invalid/items/i1", FormURL: "https://apa.example.invalid/forms/tok"})
	if err != nil || rc.Ref != "d1@1" {
		t.Fatalf("send: %v %+v", err, rc)
	}
	ch.Send(ctx, conn.Message{Key: "d2@1", Class: "decision", Title: "高风险", WebURL: "https://apa.example.invalid/items/i2", WebOnly: true})
	ch.Send(ctx, conn.Message{Key: "r1@1", Class: "reminder", Title: "提醒", Summary: strings.Repeat("长", 2000), WebURL: "https://apa.example.invalid/items/i3"})
	if len(s.got) != 3 {
		t.Fatalf("pushes = %d", len(s.got))
	}
	d, w, r := s.got[0], s.got[1], s.got[2]
	if d["device_key"] != "dev-KEY" || d["url"] != "https://apa.example.invalid/forms/tok" || d["level"] != "timeSensitive" ||
		d["group"] != "APA" || d["id"] != "d1@1" || !strings.Contains(d["body"].(string), "推荐：合并") {
		t.Fatalf("decision push = %v", d)
	}
	if w["url"] != "https://apa.example.invalid/items/i2" {
		t.Fatalf("web_only push = %v", w)
	}
	if _, ok := r["level"]; ok || len([]rune(r["body"].(string))) != maxBody {
		t.Fatalf("reminder push = %v", r)
	}
	if s.auth[0] != ":" {
		t.Fatalf("basic auth sent without a login: %v", s.auth)
	}
}

// 自建服务器的登录验证；状态码映射到问题代码。
func TestBasicAuthAndProblems(t *testing.T) {
	s := &stub{}
	ch := newCh(t, s, map[string]string{"auth_user": "me"}, map[string]string{"auth_secret": "pw"})
	if err := ch.Test(context.Background()); err != nil || s.auth[0] != "me:pw" {
		t.Fatalf("test: %v %v", err, s.auth)
	}
	for st, want := range map[int]string{401: conn.ProblemAuthFailed, 400: conn.ProblemRejected, 429: conn.ProblemRateLimited, 503: conn.ProblemUnreachable} {
		s.status = st
		if _, err := ch.Send(context.Background(), conn.Message{Key: "k", Title: "t"}); conn.ProblemOf(err) != want {
			t.Errorf("status %d: %v", st, err)
		}
	}
	if _, err := New(conn.Config{Settings: map[string]string{"server": "https://x.invalid", "auth_user": "me"}}); conn.ProblemOf(err) != conn.ProblemConfigInvalid {
		t.Fatalf("login without secret: %v", err)
	}
}
