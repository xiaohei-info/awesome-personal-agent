package telegram

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-telegram/bot/models"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
)

const testKey = "k-test"

type answerCall struct{ ID, Ref, Sender, Choice, Text string }

// fakeAPA 按设计 §10.3 的投递协议模拟内核：租约、版本、ack 幂等、回答规则。它是测试边界内的替身，
// 内核侧的真实行为由 api/deliveries 与 api/channel 的测试证明。
type fakeAPA struct {
	srv *httptest.Server
	mu  sync.Mutex

	queue    []*Delivery // 待领取（FIFO）
	leased   map[string]*Delivery
	acks     map[string][]string // 投递 id → 每次 ack 的 ref
	fails    map[string][]string
	answers  []answerCall
	ackErr   int             // >0 时 ack 回这个状态码（模拟回执丢失）
	stale    map[string]bool // 版本已作废：ack/fail 回 409
	answerFn func(a answerCall) (int, *APIError)
	refs     map[string]string // 投递 id → 首次 ack 的 ref（回答的 ref 校验）
}

func newFakeAPA(t *testing.T) *fakeAPA {
	f := &fakeAPA{leased: map[string]*Delivery{}, acks: map[string][]string{}, fails: map[string][]string{},
		stale: map[string]bool{}, refs: map[string]string{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/deliveries/next", f.next)
	mux.HandleFunc("POST /v1/deliveries/{id}/ack", f.report(true))
	mux.HandleFunc("POST /v1/deliveries/{id}/fail", f.report(false))
	mux.HandleFunc("POST /v1/deliveries/{id}/answer", f.answer)
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeAPA) enqueue(d Delivery) { f.mu.Lock(); f.queue = append(f.queue, &d); f.mu.Unlock() }

func apiErr(w http.ResponseWriter, status int, e APIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(e)
}

func (f *fakeAPA) authed(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("Authorization") != "Bearer "+testKey {
		apiErr(w, 401, APIError{Code: "unauthenticated", Why: "bad key"})
		return false
	}
	return true
}

func (f *fakeAPA) next(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	for i := 0; i < 10; i++ {
		f.mu.Lock()
		if len(f.queue) > 0 {
			d := f.queue[0]
			f.queue = f.queue[1:]
			f.leased[d.ID] = d
			f.mu.Unlock()
			_ = json.NewEncoder(w).Encode(d)
			return
		}
		f.mu.Unlock()
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (f *fakeAPA) report(isAck bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !f.authed(w, r) {
			return
		}
		var in struct {
			Version     int64  `json:"version"`
			ExternalRef string `json:"external_ref"`
			Reason      string `json:"reason"`
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		id := r.PathValue("id")
		f.mu.Lock()
		defer f.mu.Unlock()
		if f.stale[id] {
			apiErr(w, 409, APIError{Code: "conflict", Why: "delivery version is no longer current"})
			return
		}
		if !isAck {
			f.fails[id] = append(f.fails[id], in.Reason)
			_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
			return
		}
		f.acks[id] = append(f.acks[id], in.ExternalRef)
		if f.ackErr > 0 { // 内核已记下，但响应没到 ext
			apiErr(w, f.ackErr, APIError{Code: "internal", Why: "simulated lost receipt"})
			return
		}
		if _, ok := f.refs[id]; !ok {
			f.refs[id] = in.ExternalRef
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"id": id})
	}
}

// requeue 模拟租约到期：投递重新可领。
func (f *fakeAPA) requeue(id string) {
	if d, ok := f.leased[id]; ok {
		f.queue = append(f.queue, d)
	}
}

func (f *fakeAPA) answer(w http.ResponseWriter, r *http.Request) {
	if !f.authed(w, r) {
		return
	}
	var in struct{ Ref, Sender, Choice, Text string }
	_ = json.NewDecoder(r.Body).Decode(&in)
	a := answerCall{ID: r.PathValue("id"), Ref: in.Ref, Sender: in.Sender, Choice: in.Choice, Text: in.Text}
	f.mu.Lock()
	f.answers = append(f.answers, a)
	fn := f.answerFn
	f.mu.Unlock()
	if fn != nil {
		if st, e := fn(a); e != nil {
			apiErr(w, st, *e)
			return
		}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"id": "item-1", "replayed": false})
}

func (f *fakeAPA) snapshot() (acks map[string][]string, fails map[string][]string, answers []answerCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	acks, fails = map[string][]string{}, map[string][]string{}
	for k, v := range f.acks {
		acks[k] = append([]string(nil), v...)
	}
	for k, v := range f.fails {
		fails[k] = append([]string(nil), v...)
	}
	return acks, fails, append([]answerCall(nil), f.answers...)
}

const chat int64 = 4242

var owner = models.User{ID: 7, Username: "owner"}

// start 起一个 ext 连上 fakeAPA 与 fakebot，返回停止函数（等 ext 完全退出）。
func start(t *testing.T, f *fakeAPA, fb *fakebot.Bot, st Store) (stop func()) {
	t.Helper()
	e, err := New(Config{APAURL: f.srv.URL, APAKey: testKey, BotToken: "1:t", BotAPI: fb.URL, Chat: chat, Wait: 1,
		PollTimeout: 2 * time.Second}, st, testLogger(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for: %s", what)
}

func decision() Delivery {
	return Delivery{ID: "decision:item-1:3", Class: "decision", Version: 3, Content: Content{
		Title: "合并到哪个分支", Summary: "要合到 main 还是 release？", Options: []string{"main: 合到 main", "release: 合到 release"},
		Recommend: "main", WebURL: "/items/item-1"}}
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}
