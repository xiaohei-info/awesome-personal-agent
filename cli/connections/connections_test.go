package connections

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
)

type handlerTransport struct{ h http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	h.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

// apa connection list 只是 GET /v1/connections 的透传：查询串照传，响应原样输出；用法错误退出码 2。
func TestConnectionListPassesThrough(t *testing.T) {
	var got, bodies []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		var b []byte
		if r.Body != nil {
			b, _ = io.ReadAll(r.Body)
		}
		bodies = append(bodies, string(b))
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"connections":[{"id":"ch-1","name":"我的手机","state":"ok","future_field":1}]}`))
	})
	stdin := ""
	apa := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": "apa_obs_x", "APA_HOME": t.TempDir()}
		env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: t.TempDir(), Stdin: strings.NewReader(stdin),
			Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: handlerTransport{h}}}
		return client.Main(args, env), out.String() + errb.String()
	}
	if code, out := apa("connection", "list", "--direction", "channel"); code != 0 || !strings.Contains(out, `"future_field":1`) {
		t.Fatalf("list: %d %s", code, out)
	}
	if len(got) != 1 || got[0] != "GET /v1/connections?direction=channel" {
		t.Fatalf("requests = %v", got)
	}
	// 草稿：凭据只从标准输入进请求体，不出现在输出里；配置与路由照传。
	got, bodies = nil, nil
	stdin = `{"device_key":"PLAIN-from-stdin"}`
	code, out := apa("connection", "propose", "--type", "bark", "--name", "我的手机", "--set", "server=https://push.example", "--route", "decision", "--route", "health", "--secret-stdin")
	if code != 0 || strings.Contains(out, "PLAIN-from-stdin") || got[0] != "POST /v1/connections/drafts" {
		t.Fatalf("propose: %d %s %v", code, out, got)
	}
	var sent map[string]any
	json.Unmarshal([]byte(bodies[0]), &sent)
	if sent["type"] != "bark" || sent["name"] != "我的手机" || sent["secrets"].(map[string]any)["device_key"] != "PLAIN-from-stdin" ||
		sent["config"].(map[string]any)["server"] != "https://push.example" || len(sent["routes"].([]any)) != 2 {
		t.Fatalf("propose body: %v", sent)
	}
	stdin = "not json PLAIN-bad"
	if code, out := apa("connection", "propose", "--type", "bark", "--secret-stdin"); code != 2 || strings.Contains(out, "PLAIN-bad") {
		t.Fatalf("bad stdin: %d %s", code, out)
	}
	got = nil
	apa("connection", "drafts")
	apa("connection", "test-draft", "dr-1")
	apa("connection", "discard", "dr-1")
	if strings.Join(got, "|") != "GET /v1/connections/drafts|POST /v1/connections/drafts/dr-1/test|DELETE /v1/connections/drafts/dr-1" {
		t.Fatalf("draft routes: %v", got)
	}
	for _, args := range [][]string{{"connection"}, {"connection", "add"}, {"connection", "list", "extra"}, {"connection", "propose"}, {"connection", "discard"}} {
		if code, out := apa(args...); code != 2 || !strings.Contains(out, "usage: apa connection list") {
			t.Errorf("%v: %d %s", args, code, out)
		}
	}
}
