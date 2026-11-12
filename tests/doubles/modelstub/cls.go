package modelstub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProtocolJudgment 是分类端点的协议标识（设计 §8.8）。
const ProtocolJudgment = "probability-judgment/1"

// ClsJudge 按题目内容配置一道题的结果。首个命中的规则生效（按添加顺序，不看调用顺序）。
// PYes 为空且 Error、Omit 都未设时，用默认概率。
type ClsJudge struct {
	Key     Key      `json:"key"`
	PYes    *float64 `json:"p_yes,omitempty"`
	Error   string   `json:"error,omitempty"` // input_too_large | unsupported_question | internal
	Omit    bool     `json:"omit,omitempty"`  // 漏答：响应里不含这道题
	DelayMS int      `json:"delay_ms,omitempty"`
}

// ClsFault 是整批层面的持久故障（不随调用次数变化，用 Reset 或 SetFault 清除）。
type ClsFault struct {
	Status      int    `json:"status,omitempty"`        // 400/401/403/408/413/429/5xx
	RetryAfter  int    `json:"retry_after,omitempty"`   // 429 的 Retry-After 秒数
	Protocol    string `json:"protocol,omitempty"`      // 非空：响应里的协议标识（用于制造不符）
	Source      string `json:"source,omitempty"`        // 非空：probability_source 覆盖，如 self_reported
	Raw         string `json:"raw,omitempty"`           // 非空：以 200 原样返回的响应体（如非法 JSON）
	MaxQuestion int    `json:"max_questions,omitempty"` // >0：批内题数超过则 413
	DelayMS     int    `json:"delay_ms,omitempty"`      // 整批时延
}

// ClsScript 是控制接口 POST /_stub/script 的载荷。
type ClsScript struct {
	Judges   []ClsJudge `json:"judges,omitempty"`
	Fault    *ClsFault  `json:"fault,omitempty"`
	DefaultP *float64   `json:"default_p_yes,omitempty"`
}

// ClsBatch 是收到的一批请求：完整请求体与题数。
type ClsBatch struct {
	Path       string      `json:"path"`
	Header     http.Header `json:"header"`
	Body       []byte      `json:"-"`
	BodyText   string      `json:"body"`
	Questions  int         `json:"questions"`
	ReceivedAt time.Time   `json:"received_at"`
}

// ClsStub 是讲 probability-judgment/1 的分类端点替身（CLS_STUB）。
// 只用于测试与验收环境，不得被 apa 二进制或内核代码导入。
type ClsStub struct {
	srv *httptest.Server

	mu      sync.Mutex
	judges  []ClsJudge
	fault   ClsFault
	defP    float64
	batches []ClsBatch
}

const clsDefaultP = 0.5

func NewCls() *ClsStub { return NewClsAt("") }

// NewClsAt 在指定地址启动；空串为随机端口。
func NewClsAt(addr string) *ClsStub {
	s := &ClsStub{defP: clsDefaultP}
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

// URL 是应配置为 judge_endpoint.url 的完整地址（路径由配置给全）。
func (s *ClsStub) URL() string { return s.srv.URL + "/judge" }
func (s *ClsStub) Close()      { s.srv.Close() }

// Judge 追加一条按题配置。
func (s *ClsStub) Judge(j ClsJudge) {
	s.mu.Lock()
	s.judges = append(s.judges, j)
	s.mu.Unlock()
}

// PYes 是 Judge 的简写：含 key 的题得到概率 p。
func (s *ClsStub) PYes(k Key, p float64) { s.Judge(ClsJudge{Key: k, PYes: &p}) }

func (s *ClsStub) SetDefaultPYes(p float64) { s.mu.Lock(); s.defP = p; s.mu.Unlock() }
func (s *ClsStub) SetFault(f ClsFault)      { s.mu.Lock(); s.fault = f; s.mu.Unlock() }

// Batches 返回已收到批次的拷贝（含被故障拒绝的批次）。
func (s *ClsStub) Batches() []ClsBatch {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]ClsBatch(nil), s.batches...)
}

func (s *ClsStub) Reset() {
	s.mu.Lock()
	s.judges, s.fault, s.defP, s.batches = nil, ClsFault{}, clsDefaultP, nil
	s.mu.Unlock()
}

type clsQuestion struct {
	ID       string          `json:"id"`
	Question string          `json:"question"`
	Criteria string          `json:"criteria"`
	Input    json.RawMessage `json:"input"`
	Material string          `json:"material"`
}

func (q clsQuestion) text() string {
	return q.Question + "\n" + q.Criteria + "\n" + string(q.Input) + "\n" + q.Material
}

var statusCode = map[int]string{
	400: "invalid_request", 401: "unauthorized", 403: "unauthorized", 408: "timeout",
	413: "batch_too_large", 429: "rate_limited", 504: "timeout",
}

func (s *ClsStub) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/_stub/") {
		s.control(w, r)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	raw, _ := io.ReadAll(r.Body)
	var req struct {
		Protocol  string        `json:"protocol"`
		Questions []clsQuestion `json:"questions"`
	}
	parseErr := json.Unmarshal(raw, &req)

	s.mu.Lock()
	s.batches = append(s.batches, ClsBatch{r.URL.Path, r.Header.Clone(), raw, string(raw), len(req.Questions), time.Now()})
	fault, judges, defP := s.fault, append([]ClsJudge(nil), s.judges...), s.defP
	s.mu.Unlock()

	delay := time.Duration(fault.DelayMS) * time.Millisecond
	for _, q := range req.Questions {
		for _, j := range judges {
			if j.Key.matches(q.text()) {
				if d := time.Duration(j.DelayMS) * time.Millisecond; d > delay {
					delay = d
				}
				break
			}
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}

	status := fault.Status
	if status == 0 && fault.MaxQuestion > 0 && len(req.Questions) > fault.MaxQuestion {
		status = 413
	}
	if status == 0 && (parseErr != nil || req.Protocol != ProtocolJudgment || len(req.Questions) == 0) {
		status = 400
	}
	w.Header().Set("Content-Type", "application/json")
	if status != 0 {
		if status == 429 && fault.RetryAfter > 0 {
			w.Header().Set("Retry-After", strconv.Itoa(fault.RetryAfter))
		}
		w.WriteHeader(status)
		code := statusCode[status]
		if code == "" {
			code = "unavailable"
		}
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code, "message": "scripted failure"}})
		return
	}
	if fault.Raw != "" {
		io.WriteString(w, fault.Raw)
		return
	}

	results := make([]any, 0, len(req.Questions))
	for _, q := range req.Questions {
		var hit *ClsJudge
		for i := range judges {
			if judges[i].Key.matches(q.text()) {
				hit = &judges[i]
				break
			}
		}
		switch {
		case hit != nil && hit.Omit:
		case hit != nil && hit.Error != "":
			results = append(results, map[string]any{"id": q.ID, "error": map[string]any{"code": hit.Error, "message": "scripted"}})
		case hit != nil && hit.PYes != nil:
			results = append(results, map[string]any{"id": q.ID, "p_yes": *hit.PYes})
		default:
			results = append(results, map[string]any{"id": q.ID, "p_yes": defP})
		}
	}
	proto, src := ProtocolJudgment, "native"
	if fault.Protocol != "" {
		proto = fault.Protocol
	}
	if fault.Source != "" {
		src = fault.Source
	}
	json.NewEncoder(w).Encode(map[string]any{"protocol": proto, "model": "cls-stub", "probability_source": src, "results": results})
}

func (s *ClsStub) control(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/_stub/requests" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(s.Batches())
	case r.URL.Path == "/_stub/reset" && r.Method == http.MethodPost:
		s.Reset()
		w.WriteHeader(http.StatusNoContent)
	case r.URL.Path == "/_stub/script" && r.Method == http.MethodPost:
		var sc ClsScript
		if err := json.NewDecoder(r.Body).Decode(&sc); err != nil {
			http.Error(w, fmt.Sprint(err), http.StatusBadRequest)
			return
		}
		for _, j := range sc.Judges {
			s.Judge(j)
		}
		if sc.Fault != nil {
			s.SetFault(*sc.Fault)
		}
		if sc.DefaultP != nil {
			s.SetDefaultPYes(*sc.DefaultP)
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "not found or method not allowed", http.StatusNotFound)
	}
}
