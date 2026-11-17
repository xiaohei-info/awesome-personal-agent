package connections

import (
	"bytes"
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
	var got []string
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Method+" "+r.URL.RequestURI())
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"connections":[{"id":"ch-1","name":"我的手机","state":"ok","future_field":1}]}`))
	})
	apa := func(args ...string) (int, string) {
		var out, errb bytes.Buffer
		vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": "apa_obs_x", "APA_HOME": t.TempDir()}
		env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: t.TempDir(), Stdin: strings.NewReader(""),
			Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: handlerTransport{h}}}
		return client.Main(args, env), out.String() + errb.String()
	}
	if code, out := apa("connection", "list", "--direction", "channel"); code != 0 || !strings.Contains(out, `"future_field":1`) {
		t.Fatalf("list: %d %s", code, out)
	}
	if len(got) != 1 || got[0] != "GET /v1/connections?direction=channel" {
		t.Fatalf("requests = %v", got)
	}
	for _, args := range [][]string{{"connection"}, {"connection", "add"}, {"connection", "list", "extra"}} {
		if code, out := apa(args...); code != 2 || !strings.Contains(out, "usage: apa connection list") {
			t.Errorf("%v: %d %s", args, code, out)
		}
	}
}
