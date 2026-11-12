// Package modelstub 是测试用的 chat completions 兼容本地服务（MODEL_STUB）。
// 只用于测试与验收环境，不得被 apa 二进制或内核代码导入。
package modelstub

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"
)

// Reply 是一次脚本化响应。Status 为 0 表示 200；非 2xx 返回 OpenAI 风格错误体。
type Reply struct {
	Content string        `json:"content,omitempty"`
	Delay   time.Duration `json:"-"`
	DelayMS int           `json:"delay_ms,omitempty"`
	Status  int           `json:"status,omitempty"`

	judged bool // 内部：由判断题配置产生
}

func (r Reply) delay() time.Duration {
	if r.Delay > 0 {
		return r.Delay
	}
	return time.Duration(r.DelayMS) * time.Millisecond
}

// Rule 在请求体包含 Match 时返回 Reply（持久，不消耗）。
type Rule struct {
	Match string `json:"match"`
	Reply Reply  `json:"reply"`
}

// Script 是控制接口 POST /_stub/script 的载荷。
type Script struct {
	Queue   []Reply     `json:"queue,omitempty"`
	Default *Reply      `json:"default,omitempty"`
	Rules   []Rule      `json:"rules,omitempty"`
	Judges  []ChatJudge `json:"judges,omitempty"`
}

// Request 是收到的完整请求。
type Request struct {
	Method     string      `json:"method"`
	Path       string      `json:"path"`
	Header     http.Header `json:"header"`
	Body       []byte      `json:"-"`
	BodyText   string      `json:"body"`
	ReceivedAt time.Time   `json:"received_at"`
}

// Stub 响应顺序：规则（按添加顺序首个命中）> 队列（先进先出，用完即弃）> 默认。
type Stub struct {
	srv *httptest.Server

	mu       sync.Mutex
	queue    []Reply
	rules    []Rule
	judges   []ChatJudge
	def      Reply
	requests []Request
}

var builtinDefault = Reply{Content: "ok"}

// New 在随机本地端口启动。
func New() *Stub { return NewAt("") }

// NewAt 在指定地址（如 127.0.0.1:18080）启动；空串为随机端口。
func NewAt(addr string) *Stub {
	s := &Stub{def: builtinDefault}
	h := http.HandlerFunc(s.handle)
	if addr == "" {
		s.srv = httptest.NewServer(h)
		return s
	}
	s.srv = httptest.NewUnstartedServer(h)
	l, err := listen(addr)
	if err != nil {
		panic(err)
	}
	s.srv.Listener.Close()
	s.srv.Listener = l
	s.srv.Start()
	return s
}

// URL 是应配置为模型端点 base_url 的地址（含 /v1）。
func (s *Stub) URL() string { return s.srv.URL + "/v1" }

func (s *Stub) Close() { s.srv.Close() }

func (s *Stub) Enqueue(rs ...Reply) {
	s.mu.Lock()
	s.queue = append(s.queue, rs...)
	s.mu.Unlock()
}

func (s *Stub) SetDefault(r Reply) {
	s.mu.Lock()
	s.def = r
	s.mu.Unlock()
}

func (s *Stub) When(match string, r Reply) {
	s.mu.Lock()
	s.rules = append(s.rules, Rule{Match: match, Reply: r})
	s.mu.Unlock()
}

// Requests 返回已收到请求的拷贝，含因客户端超时而未完成的请求。
func (s *Stub) Requests() []Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Request(nil), s.requests...)
}

// Reset 清空请求记录与全部脚本。
func (s *Stub) Reset() {
	s.mu.Lock()
	s.queue, s.rules, s.judges, s.requests, s.def = nil, nil, nil, nil, builtinDefault
	s.mu.Unlock()
}

func (s *Stub) pick(body string) Reply {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.rules {
		if strings.Contains(body, r.Match) {
			return r.Reply
		}
	}
	if len(s.queue) > 0 {
		r := s.queue[0]
		s.queue = s.queue[1:]
		return r
	}
	return s.def
}

func (s *Stub) handle(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/chat/completions", "/chat/completions":
		s.chat(w, r)
	case "/_stub/script", "/_stub/requests", "/_stub/reset":
		s.control(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (s *Stub) chat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.requests = append(s.requests, Request{r.Method, r.URL.Path, r.Header.Clone(), raw, string(raw), time.Now()})
	s.mu.Unlock()

	rep, lp := s.judgeReply(raw)
	if lp == nil && !rep.judged {
		rep = s.pick(string(raw))
	}
	if d := rep.delay(); d > 0 {
		select {
		case <-time.After(d):
		case <-r.Context().Done():
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	if rep.Status >= 300 {
		w.WriteHeader(rep.Status)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": "scripted failure", "type": "server_error"}})
		return
	}
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(raw, &req)
	choice := map[string]any{"index": 0, "finish_reason": "stop",
		"message": map[string]any{"role": "assistant", "content": rep.Content}}
	if lp != nil {
		choice["logprobs"] = lp
	}
	json.NewEncoder(w).Encode(map[string]any{
		"id": "chatcmpl-stub", "object": "chat.completion", "created": time.Now().Unix(), "model": req.Model,
		"choices": []any{choice},
		"usage":   map[string]int{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0},
	})
}

func (s *Stub) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_stub/requests" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(append([]Request{}, s.Requests()...))
	case r.URL.Path == "/_stub/reset" && r.Method == http.MethodPost:
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/_stub/script" && r.Method == http.MethodPost:
		var sc Script
		if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		s.Enqueue(sc.Queue...)
		if sc.Default != nil {
			s.SetDefault(*sc.Default)
		}
		for _, ru := range sc.Rules {
			s.When(ru.Match, ru.Reply)
		}
		for _, j := range sc.Judges {
			s.Judge(j)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
