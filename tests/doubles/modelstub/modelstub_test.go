package modelstub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func chat(t *testing.T, base, body string) (int, map[string]any, time.Duration) {
	t.Helper()
	start := time.Now()
	resp, err := http.Post(base+"/chat/completions", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	return resp.StatusCode, out, time.Since(start)
}

func content(t *testing.T, out map[string]any) string {
	t.Helper()
	msg := out["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	return msg["content"].(string)
}

// flow-env-baseline: 发一次测试请求能返回脚本内容并留下请求记录。
func TestScriptedContentAndFullRequestRecorded(t *testing.T) {
	s := New()
	defer s.Close()
	s.Enqueue(Reply{Content: `{"choice":"remind","confidence":0.9}`})

	body := `{"model":"m1","messages":[{"role":"user","content":"hello"}],"response_format":{"type":"json_schema"}}`
	req, _ := http.NewRequest("POST", s.URL()+"/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("not json: %s", raw)
	}
	if resp.StatusCode != 200 || out["object"] != "chat.completion" || out["model"] != "m1" {
		t.Fatalf("bad envelope %d %s", resp.StatusCode, raw)
	}
	if got := content(t, out); got != `{"choice":"remind","confidence":0.9}` {
		t.Fatalf("content = %s", got)
	}
	rs := s.Requests()
	if len(rs) != 1 || rs[0].Method != "POST" || rs[0].Path != "/v1/chat/completions" ||
		rs[0].Header.Get("Authorization") != "Bearer sk-test" || string(rs[0].Body) != body {
		t.Fatalf("request log incomplete: %+v", rs)
	}
}

// flow-agent-cascade: 按调用顺序（队列）与按请求内容（规则）分别返回不同结果。
func TestQueueRulesAndDefaultPrecedence(t *testing.T) {
	s := New()
	defer s.Close()
	s.SetDefault(Reply{Content: "default"})
	s.When("judge", Reply{Content: "rule"})
	s.Enqueue(Reply{Content: "first"}, Reply{Content: "second"})

	var got []string
	for _, b := range []string{"x", "x", "x", "judge me"} {
		_, out, _ := chat(t, s.URL(), b)
		got = append(got, content(t, out))
	}
	if strings.Join(got, ",") != "first,second,default,rule" {
		t.Fatalf("got %v", got)
	}
	if len(s.Requests()) != 4 {
		t.Fatalf("recorded %d", len(s.Requests()))
	}
}

func TestScriptedHTTP500(t *testing.T) {
	s := New()
	defer s.Close()
	s.SetDefault(Reply{Status: 500})
	code, out, _ := chat(t, s.URL(), "{}")
	if code != 500 || out["error"] == nil {
		t.Fatalf("code=%d out=%v", code, out)
	}
	if len(s.Requests()) != 1 {
		t.Fatal("500 request must still be recorded")
	}
}

func TestScriptedDelay(t *testing.T) {
	s := New()
	defer s.Close()
	s.Enqueue(Reply{Content: "slow", Delay: 150 * time.Millisecond})
	_, out, d := chat(t, s.URL(), "{}")
	if d < 150*time.Millisecond || content(t, out) != "slow" {
		t.Fatalf("delay %v", d)
	}
}

// 客户端超时放弃时请求仍已记录（初筛超时场景）。
func TestClientTimeoutStillRecorded(t *testing.T) {
	s := New()
	defer s.Close()
	s.SetDefault(Reply{Content: "late", Delay: 2 * time.Second})
	c := http.Client{Timeout: 100 * time.Millisecond}
	if _, err := c.Post(s.URL()+"/chat/completions", "application/json", strings.NewReader("{}")); err == nil {
		t.Fatal("expected timeout")
	}
	if len(s.Requests()) != 1 {
		t.Fatal("not recorded")
	}
}

func TestResetAndUnknownRoute(t *testing.T) {
	s := New()
	defer s.Close()
	s.SetDefault(Reply{Content: "a"})
	chat(t, s.URL(), "{}")
	s.Reset()
	if len(s.Requests()) != 0 {
		t.Fatal("reset keeps requests")
	}
	// reset 清除脚本：回到内置默认
	_, out, _ := chat(t, s.URL(), "{}")
	if content(t, out) == "a" {
		t.Fatal("reset keeps script")
	}
	resp, _ := http.Get(s.URL() + "/nope")
	if resp.StatusCode != 404 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	resp, _ = http.Get(s.URL() + "/chat/completions")
	if resp.StatusCode != 405 {
		t.Fatalf("GET status %d", resp.StatusCode)
	}
}

// 独立运行时经控制接口编排与读取（验收执行者不写 Go）。
func TestControlEndpoints(t *testing.T) {
	s := New()
	defer s.Close()
	root := strings.TrimSuffix(s.URL(), "/v1")
	script := `{"queue":[{"content":"c1"},{"status":500}],"default":{"content":"d"},"rules":[{"match":"magic","reply":{"content":"r"}}]}`
	resp, err := http.Post(root+"/_stub/script", "application/json", strings.NewReader(script))
	if err != nil || resp.StatusCode != 204 {
		t.Fatalf("script: %v %v", err, resp)
	}
	var got []string
	for _, b := range []string{"a", "a", "a", "magic"} {
		code, out, _ := chat(t, s.URL(), b)
		if code == 500 {
			got = append(got, "500")
		} else {
			got = append(got, content(t, out))
		}
	}
	if strings.Join(got, ",") != "c1,500,d,r" {
		t.Fatalf("got %v", got)
	}
	resp, _ = http.Get(root + "/_stub/requests")
	var logged []struct {
		Path string `json:"path"`
		Body string `json:"body"`
	}
	json.NewDecoder(resp.Body).Decode(&logged)
	if len(logged) != 4 || logged[3].Body != "magic" || logged[0].Path != "/v1/chat/completions" {
		t.Fatalf("log %+v", logged)
	}
	resp, _ = http.Post(root+"/_stub/reset", "", bytes.NewReader(nil))
	if resp.StatusCode != 204 || len(s.Requests()) != 0 {
		t.Fatal("reset via control failed")
	}
	resp, _ = http.Post(root+"/_stub/script", "application/json", strings.NewReader("{bad"))
	if resp.StatusCode != 400 {
		t.Fatalf("bad script status %d", resp.StatusCode)
	}
}
