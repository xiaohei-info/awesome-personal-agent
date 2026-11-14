package model

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// soFake 是 system-one 判断接口的替身：记下每个请求体，按 reply 回应。
type soFake struct {
	mu    sync.Mutex
	paths []string
	auths []string
	reqs  []soReq
	reply func(n int, req soReq, w http.ResponseWriter)
}

type soReq struct {
	Model     string `json:"model"`
	State     string `json:"state"`
	Questions map[string]struct {
		Type         string `json:"type"`
		Instructions struct {
			Question  string          `json:"question"`
			Input     json.RawMessage `json:"input"`
			Material  string          `json:"material"`
			Truncated bool            `json:"material_truncated"`
		} `json:"instructions"`
		Criteria map[string]string `json:"criteria"`
	} `json:"questions"`
}

// answerAll 给每题一个按题号派生的 p_yes。
func answerAll(_ int, req soReq, w http.ResponseWriter) {
	ans := map[string]any{}
	for id := range req.Questions {
		ans[id] = map[string]any{"type": "noul", "noul": soP(id)}
	}
	json.NewEncoder(w).Encode(map[string]any{"model": "jev-1.14.0", "answers": ans, "usage": map[string]int{"input_tokens": 1, "output_tokens": 1}})
}

func soP(id string) float64 { return float64(len(id)) / 10 }

func soSetup(t *testing.T, reply func(int, soReq, http.ResponseWriter)) (*soFake, policy.JudgeEndpoint, *Client, *sleeper) {
	t.Helper()
	f := &soFake{reply: reply}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req soReq
		_ = json.Unmarshal(raw, &req)
		f.mu.Lock()
		n := len(f.reqs)
		f.paths, f.auths, f.reqs = append(f.paths, r.URL.Path), append(f.auths, r.Header.Get("Authorization")), append(f.reqs, req)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		f.reply(n, req, w)
	}))
	t.Cleanup(srv.Close)
	sl := &sleeper{}
	return f, policy.JudgeEndpoint{URL: srv.URL + "/v1", APIKey: "k-so", Protocol: policy.ProtocolSystemOne, Model: "jev-latest",
		TimeoutMs: 10000, MaxBatch: 32, MaxMaterialChars: 8000}, &Client{Sleep: sl.sleep}, sl
}

// 正常路径：路径追加 /systemone、带鉴权、题映射成 noul（问法、input、已裁剪资料进 instructions，判据进 criteria 两侧），
// 答案按题号对回，概率来源 native，模型取响应里的版本号。
func TestSystemOneMapsQuestionsAndAnswers(t *testing.T) {
	f, ep, c, _ := soSetup(t, answerAll)
	qs := []Question{
		{ID: "q1", Question: "值不值得看？", Criteria: "需要本人做事的算", Input: json.RawMessage(`{"subject":"A"}`), Material: "正文 A"},
		{ID: "q22", Question: "值不值得看？", Input: json.RawMessage(`{"subject":"B"}`)},
	}
	res, err := c.Judge(context.Background(), ep, qs, JudgeOptions{})
	if err != nil || !res.Healthy || res.Requests != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	for i, a := range res.Answers {
		if a.ErrCode != "" || a.PYes != soP(qs[i].ID) || a.Source != "native" || a.Model != "jev-1.14.0" {
			t.Fatalf("answer %d = %+v", i, a)
		}
	}
	if f.paths[0] != "/v1/systemone" || f.auths[0] != "Bearer k-so" {
		t.Fatalf("path=%q auth=%q", f.paths[0], f.auths[0])
	}
	r := f.reqs[0]
	q1, q2 := r.Questions["q1"], r.Questions["q22"]
	if r.Model != "jev-latest" || r.State == "" || q1.Type != "noul" || q1.Instructions.Question != "值不值得看？" ||
		q1.Instructions.Material != "正文 A" || string(q1.Instructions.Input) != `{"subject":"A"}` ||
		q1.Criteria["true"] != "需要本人做事的算" || q1.Criteria["false"] == "" {
		t.Fatalf("q1 wire = %+v", r)
	}
	if q2.Criteria != nil || q2.Instructions.Material != "" {
		t.Fatalf("没有判据或资料的题不应带 criteria/material: %+v", q2)
	}
	// URL 已带 /systemone 时不重复追加。
	ep.URL += "/systemone/"
	if _, err := c.Judge(context.Background(), ep, qs[:1], JudgeOptions{}); err != nil || f.paths[1] != "/v1/systemone" {
		t.Fatalf("path=%v err=%v", f.paths, err)
	}
}

// 批量：每批至多 max_batch 题，并按字符预算切批；资料超出 max_material_chars 截断并置标记。
func TestSystemOneBatching(t *testing.T) {
	f, ep, c, _ := soSetup(t, answerAll)
	ep.MaxBatch = 4
	res, err := c.Judge(context.Background(), ep, qn("b", 10), JudgeOptions{})
	if err != nil || res.Requests != 3 || len(f.reqs[0].Questions) != 4 || len(f.reqs[2].Questions) != 2 {
		t.Fatalf("requests=%d err=%v", res.Requests, err)
	}
	// 6 题各 8000 字符：按 40000 字符预算每批 4 题。
	f.reqs = nil
	ep.MaxBatch = 32
	qs := qn("c", 6)
	for i := range qs {
		qs[i].Material = strings.Repeat("字", 9000)
	}
	res, err = c.Judge(context.Background(), ep, qs, JudgeOptions{})
	if err != nil || len(f.reqs) != 2 || len(f.reqs[0].Questions) != 4 || len(f.reqs[1].Questions) != 2 {
		t.Fatalf("budget batches = %d err=%v", len(f.reqs), err)
	}
	m := f.reqs[0].Questions["q1"].Instructions
	if !res.Answers[0].Truncated || !m.Truncated || len([]rune(m.Material)) != 8000 {
		t.Fatalf("truncation: answer=%+v wire=%d %v", res.Answers[0], len([]rune(m.Material)), m.Truncated)
	}
}

// 错误映射：401/403 标不可用；422 与普通 400 是 invalid_request；均不重试，进健康视图。
func TestSystemOneWholeBatchFailures(t *testing.T) {
	for status, want := range map[int]string{401: CodeUnauthorized, 403: CodeUnauthorized, 422: CodeInvalidRequest, 400: CodeInvalidRequest} {
		_, ep, c, _ := soSetup(t, func(_ int, _ soReq, w http.ResponseWriter) {
			w.WriteHeader(status)
			fmt.Fprint(w, `{"detail":"nope"}`)
		})
		res, err := c.Judge(context.Background(), ep, qn("e", 3), JudgeOptions{})
		if err != nil || res.Requests != 1 || res.Healthy || strings.Join(codes(res), ",") != strings.Repeat(want+",", 2)+want ||
			len(res.Faults) != 1 || res.Faults[0] != want || res.Unavailable != (want == CodeUnauthorized) {
			t.Fatalf("%d: res=%+v err=%v", status, res, err)
		}
	}
}

// 413 与 400 max_tokens_exceeded 都按整批过大处理：对半拆开各发一次，拆后不再拆。
func TestSystemOneSplitsOnTooLarge(t *testing.T) {
	for name, fail := range map[string]func(http.ResponseWriter){
		"413": func(w http.ResponseWriter) { w.WriteHeader(413) },
		"maxTokens": func(w http.ResponseWriter) {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"detail":{"type":"max_tokens_exceeded"}}`)
		},
	} {
		f, ep, c, _ := soSetup(t, func(_ int, req soReq, w http.ResponseWriter) {
			if len(req.Questions) > 2 {
				fail(w)
				return
			}
			answerAll(0, req, w)
		})
		res, err := c.Judge(context.Background(), ep, qn("s", 4), JudgeOptions{})
		if err != nil || res.Requests != 3 || strings.Join(codes(res), "") != "" || len(f.reqs[1].Questions) != 2 {
			t.Fatalf("%s: requests=%d codes=%v err=%v", name, res.Requests, codes(res), err)
		}
		// 拆后仍过大：交下一级，不再拆。
		f.reply = func(_ int, _ soReq, w http.ResponseWriter) { fail(w) }
		res, _ = c.Judge(context.Background(), ep, qn("s", 4), JudgeOptions{})
		if res.Requests != 3 || codes(res)[0] != CodeBatchTooLarge {
			t.Fatalf("%s: still too large: %+v", name, res)
		}
	}
}

// 429 遵守 Retry-After；超过 timeout_ms 交下一级。529 退避重试至多 2 次。
func TestSystemOneRateLimitAndOverload(t *testing.T) {
	_, ep, c, sl := soSetup(t, func(n int, req soReq, w http.ResponseWriter) {
		if n == 0 {
			w.Header().Set("Retry-After", "3")
			w.WriteHeader(429)
			return
		}
		answerAll(n, req, w)
	})
	res, err := c.Judge(context.Background(), ep, qn("r", 1), JudgeOptions{})
	if err != nil || res.Answers[0].ErrCode != "" || res.Requests != 2 || len(sl.got) != 1 || sl.got[0] != 3*time.Second {
		t.Fatalf("res=%+v sleeps=%v err=%v", res, sl.got, err)
	}
	_, ep, c, _ = soSetup(t, func(_ int, _ soReq, w http.ResponseWriter) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
	})
	if res, _ := c.Judge(context.Background(), ep, qn("r", 1), JudgeOptions{}); res.Requests != 1 || res.Answers[0].ErrCode != CodeRateLimited {
		t.Fatalf("long retry-after: %+v", res)
	}
	_, ep, c, _ = soSetup(t, func(_ int, _ soReq, w http.ResponseWriter) { w.WriteHeader(529) })
	if res, _ := c.Judge(context.Background(), ep, qn("o", 1), JudgeOptions{}); res.Requests != 3 || res.Answers[0].ErrCode != CodeUnavailable {
		t.Fatalf("529: %+v", res)
	}
}

// 超过 timeout_ms 记 timeout；single_shot 不重试。
func TestSystemOneTimeout(t *testing.T) {
	_, ep, c, _ := soSetup(t, func(_ int, _ soReq, w http.ResponseWriter) { time.Sleep(300 * time.Millisecond) })
	ep.TimeoutMs = 50
	res, err := c.Judge(context.Background(), ep, qn("t", 1), JudgeOptions{SingleShot: true})
	if err != nil || res.Requests != 1 || res.Answers[0].ErrCode != CodeTimeout {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

// 缺某题答案只让该题交下一级；非 JSON、答案形状不对、概率越界整批 protocol_error。
func TestSystemOneMissingAndMalformed(t *testing.T) {
	_, ep, c, _ := soSetup(t, func(_ int, _ soReq, w http.ResponseWriter) {
		fmt.Fprint(w, `{"model":"jev-1.14.0","answers":{"q1":{"type":"noul","noul":0.9},"zz":{"type":"noul","noul":0.1}}}`)
	})
	res, _ := c.Judge(context.Background(), ep, qn("m", 2), JudgeOptions{})
	if !res.Healthy || res.Answers[0].PYes != 0.9 || res.Answers[1].ErrCode != CodeMissingResult {
		t.Fatalf("missing: %+v", res)
	}
	for name, body := range map[string]string{
		"not json":   `<html>`,
		"no answers": `{"model":"m"}`,
		"wrong type": `{"answers":{"q1":{"type":"choice","choice":"a"}}}`,
		"range":      `{"answers":{"q1":{"type":"noul","noul":1.5}}}`,
	} {
		_, ep, c, _ := soSetup(t, func(_ int, _ soReq, w http.ResponseWriter) { fmt.Fprint(w, body) })
		res, _ := c.Judge(context.Background(), ep, qn("p", 2), JudgeOptions{})
		if res.Healthy || codes(res)[0] != CodeProtocolError || codes(res)[1] != CodeProtocolError || res.Faults[0] != CodeProtocolError {
			t.Fatalf("%s: %+v", name, res)
		}
	}
}

// 换协议或模型，输入指纹随之变化，不复用旧结果。
func TestSystemOneFingerprintDiffersFromProbability(t *testing.T) {
	q := qn("f", 1)[0]
	so := policy.JudgeEndpoint{URL: "http://h/v1", Protocol: policy.ProtocolSystemOne, Model: "jev-latest"}
	pj := so
	pj.Protocol = policy.ProtocolProbability
	other := so
	other.Model = "jev-1.13.0"
	if a, b, c := Fingerprint(so, "v1", q), Fingerprint(pj, "v1", q), Fingerprint(other, "v1", q); a == b || a == c {
		t.Fatal("fingerprint must change with protocol and model")
	}
}

// 适配器与 CLS_STUB 的 system-one 形状互通（e2e 用的是同一个替身）。
func TestSystemOneAgainstClsStub(t *testing.T) {
	s := modelstub.NewCls()
	t.Cleanup(s.Close)
	s.PYes(modelstub.Key{Contains: "x-02"}, 0.9)
	s.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: "x-03"}, Omit: true})
	ep := policy.JudgeEndpoint{URL: s.SystemOneURL(), Protocol: policy.ProtocolSystemOne, Model: "cls-stub-latest", TimeoutMs: 10000, MaxBatch: 32, MaxMaterialChars: 8000}
	res, err := (&Client{}).Judge(context.Background(), ep, qn("x", 3), JudgeOptions{})
	if err != nil || res.Answers[0].PYes != 0.5 || res.Answers[1].PYes != 0.9 || res.Answers[2].ErrCode != CodeMissingResult || res.Answers[0].Model != "cls-stub-1.0" {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	ids, _, err := (&Client{}).ListModels(context.Background(), SystemOneBase(ep.URL+"/systemone"), "")
	if err != nil || strings.Join(ids, ",") != "cls-stub-latest" {
		t.Fatalf("ids=%v err=%v", ids, err)
	}
}

// 地区不可用：不重试、标不可用、进健康视图；普通 403 仍是 unauthorized。列模型的 StatusError 带 Region。
func TestRegionUnavailable(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		want   string
	}{
		{451, `{"title":"Typesafe is not available in your region.","status":451}`, CodeRegionUnavailable},
		{403, `{"detail":"Service is not available in your country"}`, CodeRegionUnavailable},
		{403, `{"detail":"invalid key"}`, CodeUnauthorized},
	} {
		_, ep, cl, _ := soSetup(t, func(_ int, _ soReq, w http.ResponseWriter) {
			w.WriteHeader(c.status)
			fmt.Fprint(w, c.body)
		})
		res, err := cl.Judge(context.Background(), ep, qn("g", 2), JudgeOptions{})
		if err != nil || res.Requests != 1 || !res.Unavailable || codes(res)[0] != c.want || len(res.Faults) != 1 || res.Faults[0] != c.want {
			t.Fatalf("%d %s: %+v err=%v", c.status, c.body, res, err)
		}
	}
	url := listServer(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(451) })
	_, _, err := (&Client{}).ListModels(context.Background(), url, "")
	var se *StatusError
	if !errors.As(err, &se) || !se.Region {
		t.Fatalf("err=%v", err)
	}
}
