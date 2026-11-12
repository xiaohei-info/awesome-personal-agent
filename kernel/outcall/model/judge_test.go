package model

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func f64(v float64) *float64 { return &v }

type sleeper struct {
	mu   sync.Mutex
	got  []time.Duration
	hook func()
}

func (s *sleeper) sleep(_ context.Context, d time.Duration) error {
	s.mu.Lock()
	s.got = append(s.got, d)
	s.mu.Unlock()
	if s.hook != nil {
		s.hook()
	}
	return nil
}

func clsSetup(t *testing.T) (*modelstub.ClsStub, policy.JudgeEndpoint, *Client, *sleeper) {
	t.Helper()
	s := modelstub.NewCls()
	t.Cleanup(s.Close)
	sl := &sleeper{}
	return s, policy.JudgeEndpoint{URL: s.URL(), Protocol: policy.ProtocolProbability, Model: "", TimeoutMs: 10000, MaxBatch: 32, MaxMaterialChars: 8000, APIKey: "k-1"},
		&Client{Sleep: sl.sleep}, sl
}

func qn(prefix string, n int) []Question {
	qs := make([]Question, n)
	for i := range qs {
		qs[i] = Question{ID: fmt.Sprintf("q%d", i+1), Question: "值不值得看？", Criteria: "c", Input: json.RawMessage(`{"subject":"` + fmt.Sprintf("%s-%02d", prefix, i+1) + `"}`), Material: "body"}
	}
	return qs
}

func codes(r JudgeResult) []string {
	var c []string
	for _, a := range r.Answers {
		c = append(c, a.ErrCode)
	}
	return c
}

// ACT-JDP-02：40 题分 32+8 两批；协议字段齐全；逐题 p_yes、native、端点自报模型。
func TestJudgeBatchesAndProtocolFields(t *testing.T) {
	s, ep, c, _ := clsSetup(t)
	s.PYes(modelstub.Key{Contains: "jdp-batch"}, 0.05)
	res, err := c.Judge(context.Background(), ep, qn("jdp-batch", 40), JudgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	bs := s.Batches()
	if len(bs) != 2 || bs[0].Questions != 32 || bs[1].Questions != 8 || res.Requests != 2 {
		t.Fatalf("batches %+v", bs)
	}
	if bs[0].Header.Get("Authorization") != "Bearer k-1" || bs[0].Header.Get("Content-Type") != "application/json" {
		t.Fatalf("headers %v", bs[0].Header)
	}
	var req struct {
		Protocol  string `json:"protocol"`
		RequestID string `json:"request_id"`
		Questions []struct {
			ID, Question string
			Input        map[string]any
			Material     string
		} `json:"questions"`
	}
	if err := json.Unmarshal(bs[0].Body, &req); err != nil || req.Protocol != "probability-judgment/1" || !strings.HasPrefix(req.RequestID, "jdg_") {
		t.Fatalf("req %s %v", bs[0].BodyText, err)
	}
	ids := map[string]bool{}
	for _, q := range req.Questions {
		if q.ID == "" || ids[q.ID] || q.Question == "" || q.Input == nil || q.Material == "" {
			t.Fatalf("bad question %+v", q)
		}
		ids[q.ID] = true
	}
	for i, a := range res.Answers {
		if a.ErrCode != "" || a.PYes != 0.05 || a.Source != "native" || a.Model != "cls-stub" || a.ID != fmt.Sprintf("q%d", i+1) {
			t.Fatalf("answer %d %+v", i, a)
		}
	}
	if !res.Healthy || res.Unavailable {
		t.Fatalf("health %+v", res)
	}
}

// ACT-JDP-03：单题错误与漏答只让该题交下一级，同批其他题照常定案，不重发。
func TestJudgePerQuestionErrorsDoNotRetryBatch(t *testing.T) {
	s, ep, c, sl := clsSetup(t)
	s.PYes(modelstub.Key{Contains: "jdp-q-ok"}, 0.05)
	s.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "jdp-q-big"}, Error: "input_too_large"})
	s.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "jdp-q-unsup"}, Error: "unsupported_question"})
	s.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "jdp-q-int"}, Error: "internal"})
	s.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "jdp-q-miss"}, Omit: true})
	qs := nil5()
	res, err := c.Judge(context.Background(), ep, qs, JudgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Batches()) != 1 || len(sl.got) != 0 {
		t.Fatalf("batches %d sleeps %v", len(s.Batches()), sl.got)
	}
	want := []string{"", "input_too_large", "unsupported_question", "internal", "missing_result"}
	for i, w := range want {
		if res.Answers[i].ErrCode != w {
			t.Fatalf("codes %v", codes(res))
		}
	}
	if res.Answers[0].PYes != 0.05 || len(res.Faults) != 0 || !res.Healthy {
		t.Fatalf("%+v", res)
	}
}

func nil5() []Question {
	var qs []Question
	for i, k := range []string{"jdp-q-ok", "jdp-q-big", "jdp-q-unsup", "jdp-q-int", "jdp-q-miss"} {
		qs = append(qs, Question{ID: fmt.Sprintf("q%d", i+1), Question: "?", Input: json.RawMessage(`{"subject":"` + k + `"}`)})
	}
	return qs
}

// ACT-JDP-04：整批 400、协议标识不符、p_yes 越界都不重试，整批交下一级并给出健康故障。
func TestJudgeWholeBatchProtocolFailuresNoRetry(t *testing.T) {
	for name, tc := range map[string]struct {
		setup func(*modelstub.ClsStub)
		code  string
	}{
		"http400":        {func(s *modelstub.ClsStub) { s.SetFault(modelstub.ClsFault{Status: 400}) }, CodeInvalidRequest},
		"protocol9":      {func(s *modelstub.ClsStub) { s.SetFault(modelstub.ClsFault{Protocol: "probability-judgment/9"}) }, CodeProtocolError},
		"p_yes_over_one": {func(s *modelstub.ClsStub) { s.SetDefaultPYes(0.1); s.PYes(modelstub.Key{Contains: "q-02"}, 1.5) }, CodeProtocolError},
		"not_json":       {func(s *modelstub.ClsStub) { s.SetFault(modelstub.ClsFault{Raw: "<html>"}) }, CodeProtocolError},
	} {
		t.Run(name, func(t *testing.T) {
			s, ep, c, sl := clsSetup(t)
			tc.setup(s)
			res, err := c.Judge(context.Background(), ep, qn("q", 3), JudgeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(s.Batches()) != 1 || len(sl.got) != 0 {
				t.Fatalf("retried: %d requests", len(s.Batches()))
			}
			for _, a := range res.Answers {
				if a.ErrCode != tc.code {
					t.Fatalf("codes %v", codes(res))
				}
			}
			if len(res.Faults) != 1 || res.Faults[0] != tc.code || res.Healthy || res.Unavailable {
				t.Fatalf("%+v", res)
			}
		})
	}
}

// ACT-JDP-05：401 不重试并标不可用；之后一次成功清除。
func TestJudgeUnauthorizedMarksUnavailableUntilSuccess(t *testing.T) {
	for _, st := range []int{401, 403} {
		s, ep, c, sl := clsSetup(t)
		s.SetFault(modelstub.ClsFault{Status: st})
		res, _ := c.Judge(context.Background(), ep, qn("q", 2), JudgeOptions{})
		if len(s.Batches()) != 1 || len(sl.got) != 0 || !res.Unavailable || res.Faults[0] != CodeUnauthorized || res.Answers[0].ErrCode != CodeUnauthorized {
			t.Fatalf("%d: %+v", st, res)
		}
		s.SetFault(modelstub.ClsFault{})
		s.SetDefaultPYes(0.05)
		res, _ = c.Judge(context.Background(), ep, qn("q", 1), JudgeOptions{})
		if res.Unavailable || !res.Healthy || res.Answers[0].PYes != 0.05 {
			t.Fatalf("recovered: %+v", res)
		}
	}
}

// ACT-JDP-06：5xx 与超时退避重试至多 2 次（共 3 次请求，间隔递增），仍失败交下一级；single_shot 不重试。
func TestJudgeTimeoutAnd5xxBackoff(t *testing.T) {
	t.Run("503", func(t *testing.T) {
		s, ep, c, sl := clsSetup(t)
		s.SetFault(modelstub.ClsFault{Status: 503})
		res, _ := c.Judge(context.Background(), ep, qn("q", 1), JudgeOptions{})
		if len(s.Batches()) != 3 || res.Answers[0].ErrCode != CodeUnavailable || len(sl.got) != 2 || sl.got[1] <= sl.got[0] {
			t.Fatalf("n=%d %+v sleeps=%v", len(s.Batches()), res.Answers, sl.got)
		}
	})
	t.Run("504", func(t *testing.T) {
		s, ep, c, _ := clsSetup(t)
		s.SetFault(modelstub.ClsFault{Status: 504})
		res, _ := c.Judge(context.Background(), ep, qn("q", 1), JudgeOptions{})
		if len(s.Batches()) != 3 || res.Answers[0].ErrCode != CodeTimeout {
			t.Fatalf("n=%d %+v", len(s.Batches()), res.Answers)
		}
	})
	t.Run("slow-beyond-timeout_ms", func(t *testing.T) {
		s, ep, c, _ := clsSetup(t)
		ep.TimeoutMs = 40
		s.SetFault(modelstub.ClsFault{DelayMS: 400})
		res, _ := c.Judge(context.Background(), ep, qn("q", 1), JudgeOptions{})
		if len(s.Batches()) != 3 || res.Answers[0].ErrCode != CodeTimeout {
			t.Fatalf("n=%d %+v", len(s.Batches()), res.Answers)
		}
	})
	t.Run("single_shot", func(t *testing.T) {
		s, ep, c, sl := clsSetup(t)
		s.SetFault(modelstub.ClsFault{Status: 500})
		res, _ := c.Judge(context.Background(), ep, qn("q", 1), JudgeOptions{SingleShot: true})
		if len(s.Batches()) != 1 || len(sl.got) != 0 || res.Answers[0].ErrCode != CodeUnavailable {
			t.Fatalf("n=%d %+v", len(s.Batches()), res.Answers)
		}
	})
	t.Run("recovers-on-retry", func(t *testing.T) {
		s, ep, c, sl := clsSetup(t)
		s.SetFault(modelstub.ClsFault{Status: 503})
		sl.hook = func() { s.SetFault(modelstub.ClsFault{}); s.SetDefaultPYes(0.2) }
		res, _ := c.Judge(context.Background(), ep, qn("q", 1), JudgeOptions{})
		if len(s.Batches()) != 2 || res.Answers[0].PYes != 0.2 || res.Answers[0].ErrCode != "" {
			t.Fatalf("%+v", res)
		}
	})
}

// ACT-JDP-07：413 对半拆批各发一次；拆开后仍 413 不再拆，整批交下一级。
func TestJudgeSplitOn413(t *testing.T) {
	s, ep, c, _ := clsSetup(t)
	s.SetFault(modelstub.ClsFault{MaxQuestion: 4})
	s.SetDefaultPYes(0.05)
	res, _ := c.Judge(context.Background(), ep, qn("jdp-split", 8), JudgeOptions{})
	var sizes []int
	for _, b := range s.Batches() {
		sizes = append(sizes, b.Questions)
	}
	if fmt.Sprint(sizes) != "[8 4 4]" {
		t.Fatalf("sizes %v", sizes)
	}
	for _, a := range res.Answers {
		if a.ErrCode != "" || a.PYes != 0.05 {
			t.Fatalf("%+v", res.Answers)
		}
	}

	s2, ep2, c2, _ := clsSetup(t)
	s2.SetFault(modelstub.ClsFault{Status: 413})
	res, _ = c2.Judge(context.Background(), ep2, qn("jdp-split2", 8), JudgeOptions{})
	sizes = nil
	for _, b := range s2.Batches() {
		sizes = append(sizes, b.Questions)
	}
	if fmt.Sprint(sizes) != "[8 4 4]" {
		t.Fatalf("sizes %v", sizes)
	}
	for _, a := range res.Answers {
		if a.ErrCode != CodeBatchTooLarge {
			t.Fatalf("%v", codes(res))
		}
	}
}

// ACT-JDP-08：429 遵守 Retry-After 后重发；等待超过 timeout_ms 则不等，交下一级。
func TestJudgeRateLimitHonorsRetryAfter(t *testing.T) {
	s, ep, c, sl := clsSetup(t)
	s.SetFault(modelstub.ClsFault{Status: 429, RetryAfter: 2})
	sl.hook = func() { s.SetFault(modelstub.ClsFault{}); s.SetDefaultPYes(0.05) }
	res, _ := c.Judge(context.Background(), ep, qn("q", 1), JudgeOptions{})
	if len(sl.got) != 1 || sl.got[0] != 2*time.Second || len(s.Batches()) != 2 || res.Answers[0].ErrCode != "" {
		t.Fatalf("sleeps %v batches %d %+v", sl.got, len(s.Batches()), res.Answers)
	}

	s2, ep2, c2, sl2 := clsSetup(t)
	s2.SetFault(modelstub.ClsFault{Status: 429, RetryAfter: 30})
	res, _ = c2.Judge(context.Background(), ep2, qn("q", 1), JudgeOptions{})
	if len(sl2.got) != 0 || len(s2.Batches()) != 1 || res.Answers[0].ErrCode != CodeRateLimited {
		t.Fatalf("sleeps %v %+v", sl2.got, res.Answers)
	}
}

// ACT-JDP-09：self_reported 的概率不采信，这一级记为不可用并进健康视图。
func TestJudgeSelfReportedNotTrusted(t *testing.T) {
	s, ep, c, _ := clsSetup(t)
	s.SetFault(modelstub.ClsFault{Source: "self_reported"})
	s.SetDefaultPYes(0.01)
	res, _ := c.Judge(context.Background(), ep, qn("jdp-self", 1), JudgeOptions{})
	a := res.Answers[0]
	if a.ErrCode != CodeSelfReported || a.Source != "" || a.PYes != 0 || !res.Unavailable || res.Healthy || res.Faults[0] != CodeSelfReported {
		t.Fatalf("%+v", res)
	}
}

// ACT-JDP-10：资料超过 max_material_chars 按字符截断并标记；短资料不标。
func TestJudgeTruncatesMaterial(t *testing.T) {
	s, ep, c, _ := clsSetup(t)
	long := strings.Repeat("长", 9000)
	qs := []Question{{ID: "a", Question: "?", Material: long}, {ID: "b", Question: "?", Material: "short"}}
	res, _ := c.Judge(context.Background(), ep, qs, JudgeOptions{})
	var req struct {
		Questions []struct {
			ID        string `json:"id"`
			Material  string `json:"material"`
			Truncated bool   `json:"material_truncated"`
		} `json:"questions"`
	}
	_ = json.Unmarshal(s.Batches()[0].Body, &req)
	if n := len([]rune(req.Questions[0].Material)); n != 8000 || !req.Questions[0].Truncated {
		t.Fatalf("a: %d %v", n, req.Questions[0].Truncated)
	}
	if req.Questions[1].Truncated || req.Questions[1].Material != "short" || !res.Answers[0].Truncated || res.Answers[1].Truncated {
		t.Fatalf("b: %+v", req.Questions[1])
	}
	if long != qs[0].Material {
		t.Fatal("caller's question mutated")
	}
}

// 外发范围：只有 registry.Trimmed 能组成题，meta 范围下正文不出现在请求里。
func TestQuestionFromOnlySendsTrimmedContent(t *testing.T) {
	s, ep, c, _ := clsSetup(t)
	ev := fullEvent()
	trimmed, _ := registry.Trim(ev, registry.ScopeMeta)
	q := QuestionFrom("q1", "?", "", trimmed, json.RawMessage(`{"accepted":3}`))
	_, _ = c.Judge(context.Background(), ep, []Question{q}, JudgeOptions{})
	body := s.Batches()[0].BodyText
	if strings.Contains(body, "SECRET-BODY") || strings.Contains(body, "SECRET-SUMMARY") || strings.Contains(body, "token=abc") || !strings.Contains(body, "example.com") || !strings.Contains(body, `"accepted":3`) {
		t.Fatalf("leak or missing: %s", body)
	}
}

// ACT-JDP-13：相同输入同指纹；换模型、端点、问法、资料则指纹变；Event 统计不计入。
func TestFingerprintReuseAndModelChange(t *testing.T) {
	ep := policy.JudgeEndpoint{URL: "http://h/judge", Protocol: policy.ProtocolProbability, Model: "acc-model"}
	q := Question{Question: "?", Criteria: "c", Input: json.RawMessage(`{"b":1,"a":2,"stats":{"accepted":1}}`), Material: "m"}
	base := Fingerprint(ep, "v1", q)
	same := q
	same.Input = json.RawMessage(`{"a":2,"stats":{"accepted":99},"b":1}`)
	if Fingerprint(ep, "v1", same) != base {
		t.Fatal("stats or key order changed fingerprint")
	}
	ep2 := ep
	ep2.Model = "acc-model-2"
	ep3 := ep
	ep3.URL = "http://other/judge"
	q2 := q
	q2.Material = "m2"
	for name, fp := range map[string]string{"model": Fingerprint(ep2, "v1", q), "url": Fingerprint(ep3, "v1", q), "material": Fingerprint(ep, "v1", q2), "template": Fingerprint(ep, "v2", q)} {
		if fp == base {
			t.Fatalf("%s did not change fingerprint", name)
		}
	}
}

func TestJudgeNotConfiguredAndCancel(t *testing.T) {
	if _, err := (&Client{}).Judge(context.Background(), policy.JudgeEndpoint{}, qn("q", 1), JudgeOptions{}); err != ErrNotConfigured {
		t.Fatal(err)
	}
	s, ep, c, _ := clsSetup(t)
	s.SetFault(modelstub.ClsFault{DelayMS: 2000})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := c.Judge(ctx, ep, qn("q", 1), JudgeOptions{}); err == nil {
		t.Fatal("cancel must surface")
	}
	if _, err := c.Judge(context.Background(), ep, []Question{{ID: "x"}, {ID: "x"}}, JudgeOptions{}); err == nil {
		t.Fatal("duplicate ids must be rejected")
	}
}

func TestJudgeIgnoresUnknownIDsAndRejectsDuplicates(t *testing.T) {
	raw := func(results string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"protocol":"probability-judgment/1","model":"m","probability_source":"native","results":%s}`, results)
		}))
	}
	srv := raw(`[{"id":"q1","p_yes":0.3},{"id":"zzz","p_yes":0.9}]`)
	defer srv.Close()
	res, _ := (&Client{}).Judge(context.Background(), policy.JudgeEndpoint{URL: srv.URL}, qn("q", 2), JudgeOptions{})
	if res.Answers[0].PYes != 0.3 || res.Answers[1].ErrCode != CodeMissingResult {
		t.Fatalf("%+v", res.Answers)
	}
	srv2 := raw(`[{"id":"q1","p_yes":0.3},{"id":"q1","p_yes":0.4}]`)
	defer srv2.Close()
	res, _ = (&Client{}).Judge(context.Background(), policy.JudgeEndpoint{URL: srv2.URL}, qn("q", 1), JudgeOptions{})
	if res.Answers[0].ErrCode != CodeProtocolError {
		t.Fatalf("%+v", res.Answers)
	}
	srv3 := raw(`[{"id":"q1","p_yes":0.3,"error":{"code":"internal"}}]`)
	defer srv3.Close()
	res, _ = (&Client{}).Judge(context.Background(), policy.JudgeEndpoint{URL: srv3.URL}, qn("q", 1), JudgeOptions{})
	if res.Answers[0].ErrCode != CodeProtocolError {
		t.Fatalf("%+v", res.Answers)
	}
}

// ACT-JDP-12：chat-logprobs 换算。yes=-0.2、no=-1.8 → p_yes≈0.832；两者概率和不足 0.5 → no_answer_mass。
func TestChatLogprobsConversion(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		yes, no := -0.2, -1.8
		if strings.Contains(fmt.Sprint(b["messages"]), "jdp-lp-b") {
			yes, no = -3.0, -3.0
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":"yes"},"logprobs":{"content":[{"token":"yes","logprob":%v,"top_logprobs":[{"token":"Yes","logprob":%v},{"token":" no","logprob":%v},{"token":"maybe","logprob":-0.5}]}]}}]}`, yes, yes, no)
	}))
	defer srv.Close()
	ep := policy.JudgeEndpoint{URL: srv.URL + "/v1", Protocol: policy.ProtocolChatLogprobs, Model: "acc-model", MaxBatch: 2}
	qs := []Question{{ID: "a", Question: "?", Input: json.RawMessage(`{"subject":"jdp-lp-a"}`)}, {ID: "b", Question: "?", Input: json.RawMessage(`{"subject":"jdp-lp-b"}`)}, {ID: "c", Question: "?", Input: json.RawMessage(`{"subject":"jdp-lp-c"}`)}}
	res, err := (&Client{}).Judge(context.Background(), ep, qs, JudgeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := res.Answers[0], res.Answers[1]
	want := math.Exp(-0.2) / (math.Exp(-0.2) + math.Exp(-1.8))
	if a.ErrCode != "" || math.Abs(a.PYes-0.832) > 0.01 || math.Abs(a.PYes-want) > 1e-9 || a.Source != "logprobs" || a.Model != "acc-model" {
		t.Fatalf("a %+v", a)
	}
	if b.ErrCode != CodeNoAnswerMass || b.Source != "" {
		t.Fatalf("b %+v", b)
	}
	if len(bodies) != 3 || res.Requests != 3 {
		t.Fatalf("one request per question, got %d", len(bodies))
	}
	for _, x := range bodies {
		if x["max_tokens"] != float64(1) || x["logprobs"] != true || x["model"] != "acc-model" {
			t.Fatalf("request %v", x)
		}
	}
}

// chat-logprobs 经对话端点替身：请求形状与 401 处置。
func TestChatLogprobsAgainstModelStub(t *testing.T) {
	s := modelstub.New()
	defer s.Close()
	s.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "jdp-lp-z"}, PYes: f64(0.8)})
	ep := policy.JudgeEndpoint{URL: s.URL(), Protocol: policy.ProtocolChatLogprobs, Model: "m"}
	res, err := (&Client{}).Judge(context.Background(), ep, []Question{{ID: "z", Question: "?", Input: json.RawMessage(`{"subject":"jdp-lp-z"}`)}}, JudgeOptions{})
	if err != nil || math.Abs(res.Answers[0].PYes-0.8) > 1e-6 || len(s.Requests()) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	s.Reset()
	s.SetDefault(modelstub.Reply{Status: 401})
	res, _ = (&Client{Sleep: (&sleeper{}).sleep}).Judge(context.Background(), ep, []Question{{ID: "z", Question: "?"}}, JudgeOptions{})
	if res.Answers[0].ErrCode != CodeUnauthorized || !res.Unavailable || len(s.Requests()) != 1 {
		t.Fatalf("%+v", res)
	}
}

func TestChatLogprobsEndpointWithoutLogprobsIsProtocolError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"content":"yes"}}]}`)
	}))
	defer srv.Close()
	res, _ := (&Client{}).Judge(context.Background(), policy.JudgeEndpoint{URL: srv.URL, Protocol: policy.ProtocolChatLogprobs, Model: "m"}, []Question{{ID: "a", Question: "?"}}, JudgeOptions{})
	if res.Answers[0].ErrCode != CodeProtocolError {
		t.Fatalf("%+v", res)
	}
}

func fullEvent() domain.SourceEvent {
	return domain.SourceEvent{Type: "mail", Subject: "hello", Sender: "a@b.c", Summary: "SECRET-SUMMARY", Detail: "SECRET-BODY", Link: "https://example.com/p?token=abc"}
}
