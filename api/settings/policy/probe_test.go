package policy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
)

const (
	modelsPath = "/v1/settings/model-endpoints/models"
	testPath   = "/v1/settings/model-endpoints/test"
	storedKey  = "sk-stored-key-0001"
)

// fakeEndpoint 是对话补全兼容端点与概率判断端点的替身：记下收到的路径与鉴权头。
type fakeEndpoint struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string // "METHOD path auth"
	h    http.HandlerFunc
}

func newFake(t *testing.T, h http.HandlerFunc) *fakeEndpoint {
	f := &fakeEndpoint{h: h}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.seen = append(f.seen, r.Method+" "+r.URL.Path+" "+r.Header.Get("Authorization"))
		f.mu.Unlock()
		f.h(w, r)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeEndpoint) requests() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.seen...)
}

func okModels(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/v1/models":
		fmt.Fprint(w, `{"data":[{"id":"model-b"},{"id":"model-a"}]}`)
	case "/v1/chat/completions":
		fmt.Fprint(w, `{"choices":[{"message":{"content":"{\"answer\":\"yes\",\"sufficient\":true}"}}]}`)
	case "/v1/systemone":
		var req struct {
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		for id := range req.Questions {
			fmt.Fprintf(w, `{"model":"so-model-1","answers":{%q:{"type":"noul","noul":0.96}},"usage":{"input_tokens":1,"output_tokens":0}}`, id)
		}
	case "/judge":
		var req struct {
			Questions []struct{ ID string } `json:"questions"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		fmt.Fprintf(w, `{"protocol":"probability-judgment/1","model":"judge-model","probability_source":"native","results":[{"id":%q,"p_yes":0.97}]}`, req.Questions[0].ID)
	default:
		http.NotFound(w, r)
	}
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	return out
}

// 获取模型：沿用已存密钥（同源）、结果排序、响应与事件里都没有密钥；GET 只给末 4 位。
func TestProbeModelsReusesStoredKeyForSameOrigin(t *testing.T) {
	e := setup(t)
	f := newFake(t, okModels)
	if rec := e.do("PUT", routes[2], `{"generate_endpoint":{"url":"`+f.srv.URL+`/v1","model":"model-a","api_key":"`+storedKey+`","locality":"local"}}`, e.web); rec.Code != 200 {
		t.Fatalf("PUT %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", routes[2], "", e.web); !strings.Contains(rec.Body.String(), `"api_key_hint":"0001"`) || strings.Contains(rec.Body.String(), storedKey) {
		t.Fatalf("GET %s", rec.Body)
	}
	before := len(e.eventTypes())

	rec := e.do("POST", modelsPath, `{"endpoint":"generate","url":"`+f.srv.URL+`/v1","api_key":"***"}`, e.web)
	out := decode(t, rec)
	if rec.Code != 200 || out["ok"] != true || fmt.Sprint(out["models"]) != "[model-a model-b]" || strings.Contains(rec.Body.String(), storedKey) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := f.requests(); len(got) != 1 || got[0] != "GET /v1/models Bearer "+storedKey {
		t.Fatalf("endpoint saw %v", got)
	}

	rec = e.do("POST", testPath, `{"endpoint":"generate","config":{"url":"`+f.srv.URL+`/v1","model":"model-a","locality":"local"}}`, e.web)
	out = decode(t, rec)
	if rec.Code != 200 || out["status"] != "ok" || out["code"] != "ok" {
		t.Fatalf("test %d %s", rec.Code, rec.Body)
	}
	// 探测只读：不写 Event，不产生策略版本。
	if after := len(e.eventTypes()); after != before {
		t.Fatalf("events %d → %d", before, after)
	}
}

// URL 换到另一个源时不沿用已存密钥：返回 400，端点收不到任何请求。
func TestProbeRefusesStoredKeyForOtherOrigin(t *testing.T) {
	e := setup(t)
	saved := newFake(t, okModels)
	other := newFake(t, okModels)
	e.do("PUT", routes[2], `{"generate_endpoint":{"url":"`+saved.srv.URL+`/v1","model":"m","api_key":"`+storedKey+`"}}`, e.web)
	for _, c := range []struct{ path, body string }{
		{modelsPath, `{"endpoint":"generate","url":"` + other.srv.URL + `/v1"}`},
		{testPath, `{"endpoint":"generate","config":{"url":"` + other.srv.URL + `/v1","model":"m","api_key":"***"}}`},
	} {
		rec := e.do("POST", c.path, c.body, e.web)
		if rec.Code != 400 || errCode(t, rec) != "invalid_request" || !strings.Contains(rec.Body.String(), "enter the API key again") {
			t.Fatalf("%s: %d %s", c.path, rec.Code, rec.Body)
		}
	}
	if got := other.requests(); len(got) != 0 {
		t.Fatalf("other origin saw %v", got)
	}
	// 显式给出新密钥则照用。
	if rec := e.do("POST", modelsPath, `{"endpoint":"generate","url":"`+other.srv.URL+`/v1","api_key":"sk-new"}`, e.web); decode(t, rec)["ok"] != true {
		t.Fatalf("%s", rec.Body)
	}
}

// 探测失败是数据（200 + reason），各类原因可区分。
func TestProbeModelsFailureReasons(t *testing.T) {
	e := setup(t)
	for status, reason := range map[int]string{401: "unauthorized", 404: "not_found", 503: "http_status"} {
		f := newFake(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status) })
		out := decode(t, e.do("POST", modelsPath, `{"endpoint":"generate","url":"`+f.srv.URL+`","api_key":"k"}`, e.web))
		if out["ok"] != false || out["reason"] != reason {
			t.Fatalf("%d: %v", status, out)
		}
	}
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "<html>") })
	if out := decode(t, e.do("POST", modelsPath, `{"endpoint":"generate","url":"`+f.srv.URL+`","api_key":""}`, e.web)); out["reason"] != "bad_response" {
		t.Fatalf("%v", out)
	}
}

// 判断端点：概率判断协议没有模型列表；测试连接回答示例题并报告概率来源与端点自报的模型。
func TestProbeJudgeEndpoint(t *testing.T) {
	e := setup(t)
	f := newFake(t, okModels)
	rec := e.do("POST", modelsPath, `{"endpoint":"judge","url":"`+f.srv.URL+`/judge","protocol":"probability-judgment/1"}`, e.web)
	if rec.Code != 400 || errCode(t, rec) != "invalid_request" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = e.do("POST", testPath, `{"endpoint":"judge","config":{"url":"`+f.srv.URL+`/judge","protocol":"probability-judgment/1","locality":"local"}}`, e.web)
	out := decode(t, rec)
	data, _ := out["data"].(map[string]any)
	if rec.Code != 200 || out["code"] != "ok" || data["probability_source"] != "native" || data["model"] != "judge-model" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// 草稿不合规按保存时的同一套规则拒绝。
	rec = e.do("POST", testPath, `{"endpoint":"judge","config":{"url":"ftp://x","protocol":"probability-judgment/1"}}`, e.web)
	if rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// system-one 判断接口：获取模型走接口根下的 /models（URL 多填 /systemone 也一样），测试连接回答示例题。
func TestProbeJudgeSystemOne(t *testing.T) {
	e := setup(t)
	f := newFake(t, okModels)
	out := decode(t, e.do("POST", modelsPath, `{"endpoint":"judge","url":"`+f.srv.URL+`/v1/systemone","api_key":"k","protocol":"system-one"}`, e.web))
	if out["ok"] != true || fmt.Sprint(out["models"]) != "[model-a model-b]" {
		t.Fatalf("%v", out)
	}
	rec := e.do("POST", testPath, `{"endpoint":"judge","config":{"url":"`+f.srv.URL+`/v1","api_key":"k","protocol":"system-one","model":"so-latest","locality":"external"}}`, e.web)
	out = decode(t, rec)
	data, _ := out["data"].(map[string]any)
	if out["code"] != "ok" || data["probability_source"] != "native" || data["model"] != "so-model-1" || data["protocol"] != "system-one" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	got := f.requests()
	if got[0] != "GET /v1/models Bearer k" || got[1] != "POST /v1/systemone Bearer k" {
		t.Fatalf("%v", got)
	}
	// 缺模型名按保存时的规则拒绝。
	if rec := e.do("POST", testPath, `{"endpoint":"judge","config":{"url":"`+f.srv.URL+`/v1","protocol":"system-one"}}`, e.web); rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// 同一类端点同一时刻只跑一个探测。
func TestProbeConflictWhileRunning(t *testing.T) {
	e := setup(t)
	entered, release := make(chan struct{}), make(chan struct{})
	f := newFake(t, func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-release
		fmt.Fprint(w, `{"data":[]}`)
	})
	done := make(chan *httptest.ResponseRecorder)
	go func() {
		done <- e.do("POST", modelsPath, `{"endpoint":"generate","url":"`+f.srv.URL+`","api_key":"k"}`, e.web)
	}()
	<-entered
	rec := e.do("POST", modelsPath, `{"endpoint":"generate","url":"`+f.srv.URL+`","api_key":"k"}`, e.web)
	close(release)
	if rec.Code != 409 || errCode(t, rec) != "conflict" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if first := <-done; first.Code != 200 {
		t.Fatalf("first %d %s", first.Code, first.Body)
	}
}

// 只有本人的 Web 会话能用；Bearer 凭据一律 web_only。
func TestProbeWebOnly(t *testing.T) {
	e := setup(t)
	rec := e.do("POST", modelsPath, `{"endpoint":"generate","url":"https://x.example","api_key":"k"}`, e.bearer(auth.Delegate))
	if rec.Code != 403 || errCode(t, rec) != "web_only" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// 照 GET 的形状回传（含只读的 api_key_hint）时 PUT 忽略它，密钥不变。
func TestPutIgnoresKeyHint(t *testing.T) {
	e := setup(t)
	e.do("PUT", routes[2], `{"generate_endpoint":{"url":"https://m.example/v1","model":"m","api_key":"`+storedKey+`"}}`, e.web)
	rec := e.do("PUT", routes[2], `{"generate_endpoint":{"url":"https://m.example/v1","model":"m2","api_key_hint":"0001","locality":"local"}}`, e.web)
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.GenerateEndpoint.APIKey != storedKey || p.GenerateEndpoint.Model != "m2" {
		t.Fatalf("%+v", p.GenerateEndpoint)
	}
}
