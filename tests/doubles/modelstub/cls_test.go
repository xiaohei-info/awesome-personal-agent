package modelstub

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func batch(ids ...string) string {
	qs := []string{}
	for _, id := range ids {
		qs = append(qs, fmt.Sprintf(`{"id":%q,"question":"值不值得看?","input":{"template":"triage-screen","event":%q},"material":"正文"}`, id, "evt_"+id))
	}
	return `{"protocol":"probability-judgment/1","request_id":"jdg_1","questions":[` + strings.Join(qs, ",") + `]}`
}

type clsResp struct {
	Protocol string `json:"protocol"`
	Source   string `json:"probability_source"`
	Results  []struct {
		ID    string   `json:"id"`
		PYes  *float64 `json:"p_yes"`
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
	} `json:"results"`
}

func post(t *testing.T, url, body string) (*http.Response, string) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp, string(raw)
}

func call(t *testing.T, s *ClsStub, body string) clsResp {
	t.Helper()
	resp, raw := post(t, s.URL(), body)
	if resp.StatusCode != 200 {
		t.Fatalf("status %d %s", resp.StatusCode, raw)
	}
	var out clsResp
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func pOf(r clsResp, id string) float64 {
	for _, x := range r.Results {
		if x.ID == id && x.PYes != nil {
			return *x.PYes
		}
	}
	return -1
}

// flow-judge-protocol: p_yes 按题目内容配置，批内顺序与调用顺序都不改变结果；单题错误与漏答不影响同批其他题。
func TestClsPerQuestionOutcomeIndependentOfOrder(t *testing.T) {
	s := NewCls()
	defer s.Close()
	s.PYes(Key{EventID: "evt_a"}, 0.07)
	s.PYes(Key{Template: "triage-screen", Contains: "evt_b"}, 0.55)
	s.Judge(ClsJudge{Key: Key{EventID: "evt_c"}, Error: "input_too_large"})
	s.Judge(ClsJudge{Key: Key{EventID: "evt_d"}, Omit: true})

	r1 := call(t, s, batch("a", "b", "c", "d", "e"))
	r2 := call(t, s, batch("e", "d", "c", "b", "a"))
	r3 := call(t, s, batch("b"))
	for _, r := range []clsResp{r1, r2} {
		if pOf(r, "a") != 0.07 || pOf(r, "b") != 0.55 || pOf(r, "e") != 0.5 {
			t.Fatalf("p_yes drifted: %+v", r.Results)
		}
		if len(r.Results) != 4 { // d 漏答
			t.Fatalf("want 4 results (d omitted), got %d", len(r.Results))
		}
		for _, x := range r.Results {
			if x.ID == "c" && (x.Error == nil || x.Error.Code != "input_too_large" || x.PYes != nil) {
				t.Fatalf("c should be per-question error: %+v", x)
			}
		}
	}
	if r1.Protocol != ProtocolJudgment || r1.Source != "native" || pOf(r3, "b") != 0.55 {
		t.Fatalf("envelope/source: %+v", r1)
	}
}

// 记录每批完整请求体与题数，供 max_batch、外发裁剪、material_truncated 断言。
func TestClsRecordsEveryBatch(t *testing.T) {
	s := NewCls()
	defer s.Close()
	b1, b2 := batch("a", "b", "c"), batch("d")
	call(t, s, b1)
	call(t, s, b2)
	bs := s.Batches()
	if len(bs) != 2 || bs[0].Questions != 3 || bs[1].Questions != 1 || string(bs[0].Body) != b1 || bs[1].BodyText != b2 {
		t.Fatalf("batches: %+v", bs)
	}
}

// 整批错误：状态码、Retry-After、413 按题数触发（对半拆开后通过）、协议不符、自报概率、非法 JSON。
func TestClsBatchFaults(t *testing.T) {
	s := NewCls()
	defer s.Close()
	for _, st := range []int{400, 401, 403, 408, 413, 429, 500, 503, 504} {
		s.SetFault(ClsFault{Status: st, RetryAfter: 3})
		resp, raw := post(t, s.URL(), batch("a"))
		if resp.StatusCode != st {
			t.Fatalf("status %d got %d", st, resp.StatusCode)
		}
		var e struct{ Error struct{ Code string } }
		if json.Unmarshal([]byte(raw), &e) != nil || e.Error.Code == "" {
			t.Fatalf("error body: %s", raw)
		}
		if st == 429 && resp.Header.Get("Retry-After") != "3" {
			t.Fatal("missing Retry-After")
		}
	}
	s.SetFault(ClsFault{MaxQuestion: 2})
	if resp, _ := post(t, s.URL(), batch("a", "b", "c", "d")); resp.StatusCode != 413 {
		t.Fatalf("want 413, got %d", resp.StatusCode)
	}
	if r := call(t, s, batch("a", "b")); len(r.Results) != 2 {
		t.Fatal("halved batch should pass")
	}
	s.SetFault(ClsFault{Protocol: "other/9"})
	if r := call(t, s, batch("a")); r.Protocol != "other/9" {
		t.Fatalf("protocol: %s", r.Protocol)
	}
	s.SetFault(ClsFault{Source: "self_reported"})
	if r := call(t, s, batch("a")); r.Source != "self_reported" {
		t.Fatalf("source: %s", r.Source)
	}
	s.SetFault(ClsFault{Raw: "not json"})
	if _, raw := post(t, s.URL(), batch("a")); raw != "not json" {
		t.Fatalf("raw: %s", raw)
	}
	if n := len(s.Batches()); n != 9+1+1+1+1+1 {
		t.Fatalf("faulted batches must still be recorded, got %d", n)
	}
	s.Reset()
	if len(s.Batches()) != 0 || len(call(t, s, batch("a")).Results) != 1 {
		t.Fatal("reset should clear faults and records")
	}
}

// 请求不合协议时端点回 400，且仍被记录。
func TestClsRejectsBadProtocolRequest(t *testing.T) {
	s := NewCls()
	defer s.Close()
	if resp, _ := post(t, s.URL(), `{"protocol":"x","questions":[]}`); resp.StatusCode != 400 {
		t.Fatalf("got %d", resp.StatusCode)
	}
	if len(s.Batches()) != 1 {
		t.Fatal("not recorded")
	}
}

// 可注入时延：整批与按题；客户端超时的批次仍被记录。
func TestClsDelay(t *testing.T) {
	s := NewCls()
	defer s.Close()
	s.Judge(ClsJudge{Key: Key{EventID: "evt_slow"}, DelayMS: 150})
	start := time.Now()
	call(t, s, batch("slow"))
	if time.Since(start) < 140*time.Millisecond {
		t.Fatal("per-question delay not applied")
	}
	start = time.Now()
	call(t, s, batch("fast"))
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("unrelated batch delayed")
	}
	s.SetFault(ClsFault{DelayMS: 300})
	cl := http.Client{Timeout: 50 * time.Millisecond}
	if _, err := cl.Post(s.URL(), "application/json", strings.NewReader(batch("a"))); err == nil {
		t.Fatal("expected client timeout")
	}
	time.Sleep(20 * time.Millisecond)
	if len(s.Batches()) != 3 {
		t.Fatalf("timed-out batch must be recorded, got %d", len(s.Batches()))
	}
}

// 独立运行形态：经控制接口编排与取回记录。
func TestClsControlEndpoints(t *testing.T) {
	s := NewCls()
	defer s.Close()
	base := strings.TrimSuffix(s.URL(), "/judge")
	if resp, _ := post(t, base+"/_stub/script", `{"default_p_yes":0.2,"judges":[{"key":{"event_id":"evt_a"},"p_yes":0.9}]}`); resp.StatusCode != 204 {
		t.Fatalf("script %d", resp.StatusCode)
	}
	r := call(t, s, batch("a", "z"))
	if pOf(r, "a") != 0.9 || pOf(r, "z") != 0.2 {
		t.Fatalf("%+v", r.Results)
	}
	resp, err := http.Get(base + "/_stub/requests")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var got []ClsBatch
	if json.Unmarshal(raw, &got) != nil || len(got) != 1 || got[0].Questions != 2 || !strings.Contains(got[0].BodyText, "evt_a") {
		t.Fatalf("requests: %s", raw)
	}
	if resp, _ := post(t, base+"/_stub/reset", ""); resp.StatusCode != 204 || len(s.Batches()) != 0 {
		t.Fatal("reset")
	}
	if resp, _ := post(t, base+"/_stub/script", `{bad`); resp.StatusCode != 400 {
		t.Fatal("bad script should 400")
	}
}
