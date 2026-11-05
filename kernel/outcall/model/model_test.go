package model

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const judgeSchema = `{"type":"object","required":["choice","confidence"],"additionalProperties":false,
"properties":{"choice":{"type":"string","enum":["remind","wait"]},"confidence":{"type":"number","minimum":0,"maximum":1}}}`

func setup(t *testing.T) (*modelstub.Stub, policy.ModelEndpoint) {
	t.Helper()
	s := modelstub.New()
	t.Cleanup(s.Close)
	return s, policy.ModelEndpoint{URL: s.URL(), Model: "m1", APIKey: "sekret-key"}
}

// ACT-CAS-01：判断型按 schema 返回枚举与置信度；请求带上 schema、模型名与 key。
func TestJudgeStructuredOutput(t *testing.T) {
	s, ep := setup(t)
	s.SetDefault(modelstub.Reply{Content: `{"choice":"remind","confidence":0.9}`})
	resp, err := (&Client{}).Complete(context.Background(), ep, Request{System: "sys", User: "usr", Schema: json.RawMessage(judgeSchema)})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Choice     string  `json:"choice"`
		Confidence float64 `json:"confidence"`
	}
	if err := json.Unmarshal([]byte(resp.Content), &got); err != nil || got.Choice != "remind" || got.Confidence != 0.9 {
		t.Fatalf("got %+v %v", got, err)
	}
	rs := s.Requests()
	if len(rs) != 1 || rs[0].Header.Get("Authorization") != "Bearer sekret-key" {
		t.Fatalf("requests %+v", rs)
	}
	var sent struct {
		Model          string `json:"model"`
		ResponseFormat struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Schema map[string]any `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	_ = json.Unmarshal(rs[0].Body, &sent)
	if sent.Model != "m1" || sent.ResponseFormat.Type != "json_schema" || sent.ResponseFormat.JSONSchema.Schema["type"] != "object" {
		t.Fatalf("sent %s", rs[0].BodyText)
	}
}

func TestFreeTextWithoutSchema(t *testing.T) {
	s, ep := setup(t)
	s.SetDefault(modelstub.Reply{Content: "hello"})
	resp, err := (&Client{}).Complete(context.Background(), ep, Request{User: "hi"})
	if err != nil || resp.Content != "hello" {
		t.Fatalf("%v %v", resp, err)
	}
	if strings.Contains(s.Requests()[0].BodyText, "response_format") {
		t.Fatal("unexpected response_format")
	}
}

// ACT-CAS-05：不符合 schema 的输出是失败，不返回内容。
func TestSchemaViolationsAreFailures(t *testing.T) {
	for name, content := range map[string]string{
		"not json":      "sure!",
		"trailing":      `{"choice":"remind","confidence":0.5} extra`,
		"wrong enum":    `{"choice":"nope","confidence":0.5}`,
		"missing":       `{"choice":"remind"}`,
		"extra prop":    `{"choice":"remind","confidence":0.5,"note":"x"}`,
		"out of range":  `{"choice":"remind","confidence":1.5}`,
		"wrong type":    `{"choice":"remind","confidence":"high"}`,
		"not an object": `[1]`,
	} {
		t.Run(name, func(t *testing.T) {
			s, ep := setup(t)
			s.SetDefault(modelstub.Reply{Content: content})
			resp, err := (&Client{}).Complete(context.Background(), ep, Request{Schema: json.RawMessage(judgeSchema)})
			if !errors.Is(err, ErrSchema) || resp.Content != "" {
				t.Fatalf("resp=%v err=%v", resp, err)
			}
		})
	}
}

func TestSchemaKeywords(t *testing.T) {
	ok := func(schema, inst string) error {
		var s map[string]any
		_ = json.Unmarshal([]byte(schema), &s)
		var v any
		d := json.NewDecoder(strings.NewReader(inst))
		d.UseNumber()
		_ = d.Decode(&v)
		return validate(s, v, "$")
	}
	for _, c := range []struct {
		schema, inst string
		pass         bool
	}{
		{`{"type":"array","items":{"type":"integer"},"minItems":1,"maxItems":2}`, `[1,2]`, true},
		{`{"type":"array","items":{"type":"integer"}}`, `[1.5]`, false},
		{`{"type":"array","minItems":1}`, `[]`, false},
		{`{"type":"array","maxItems":1}`, `[1,2]`, false},
		{`{"type":"string","minLength":2,"maxLength":3}`, `"ab"`, true},
		{`{"type":"string","minLength":2}`, `"a"`, false},
		{`{"type":["string","null"]}`, `null`, true},
		{`{"type":["string","null"]}`, `1`, false},
		{`{"type":"boolean"}`, `true`, true},
		{`{"type":"null"}`, `null`, true},
		{`{"type":"bogus"}`, `1`, false},
		{`{"const":1}`, `1`, true},
		{`{"const":1}`, `2`, false},
		{`{"enum":[1,"a"]}`, `1`, true},
		{`{"enum":[1,"a"]}`, `"b"`, false},
		{`{"type":"object","properties":{"a":{"type":"string"}}}`, `{"a":1}`, false},
		{`{"type":"object"}`, `{"free":1}`, true},
		{`{"type":"number","minimum":2}`, `1`, false},
	} {
		if err := ok(c.schema, c.inst); (err == nil) != c.pass {
			t.Errorf("%s vs %s: err=%v want pass=%v", c.schema, c.inst, err, c.pass)
		}
	}
}

// ACT-TRI-01：端点 500 如实返回，不重试；未配置端点不发请求。
func TestFailuresAreReportedHonestly(t *testing.T) {
	s, ep := setup(t)
	s.SetDefault(modelstub.Reply{Status: 500})
	_, err := (&Client{}).Complete(context.Background(), ep, Request{User: "x"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 500 || se.Message != "scripted failure" {
		t.Fatalf("err=%v", err)
	}
	if n := len(s.Requests()); n != 1 {
		t.Fatalf("retried: %d requests", n)
	}

	s.Reset()
	for _, bad := range []policy.ModelEndpoint{{}, {URL: ep.URL}, {Model: "m"}} {
		if _, err := (&Client{}).Complete(context.Background(), bad, Request{}); !errors.Is(err, ErrNotConfigured) {
			t.Fatalf("err=%v", err)
		}
	}
	if len(s.Requests()) != 0 {
		t.Fatal("request sent without endpoint")
	}
}

func TestTimeoutAndCancel(t *testing.T) {
	s, ep := setup(t)
	s.SetDefault(modelstub.Reply{Content: "late", Delay: 2 * time.Second})
	start := time.Now()
	_, err := (&Client{}).Complete(context.Background(), ep, Request{Timeout: 50 * time.Millisecond})
	if !errors.Is(err, ErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(start))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (&Client{}).Complete(ctx, ep, Request{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v", err)
	}
}

func TestBadResponsesAndTransport(t *testing.T) {
	for name, h := range map[string]http.HandlerFunc{
		"garbage":    func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("<html>")) },
		"no choices": func(w http.ResponseWriter, r *http.Request) { w.Write([]byte(`{"choices":[]}`)) },
		"null body": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"choices":[{"message":{"content":null}}]}`))
		},
		"too big": func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"x":"` + strings.Repeat("a", maxBody) + `"}`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(h)
			defer srv.Close()
			_, err := (&Client{}).Complete(context.Background(), policy.ModelEndpoint{URL: srv.URL, Model: "m"}, Request{})
			if !errors.Is(err, ErrBadResponse) {
				t.Fatalf("err=%v", err)
			}
		})
	}

	// 连接失败：错误里不得泄漏 api_key（key 放进 URL 的场景）。
	srv := httptest.NewServer(nil)
	url := srv.URL
	srv.Close()
	_, err := (&Client{}).Complete(context.Background(), policy.ModelEndpoint{URL: url + "/sekret-key", Model: "m", APIKey: "sekret-key"}, Request{})
	if err == nil || strings.Contains(err.Error(), "sekret-key") {
		t.Fatalf("err=%v", err)
	}
	_, err = (&Client{}).Complete(context.Background(), policy.ModelEndpoint{URL: "http://\x7f", Model: "m"}, Request{})
	if err == nil {
		t.Fatal("want error for bad url")
	}
	if _, err := (&Client{}).Complete(context.Background(), policy.ModelEndpoint{URL: url, Model: "m"}, Request{Schema: json.RawMessage(`[`)}); err == nil {
		t.Fatal("want error for bad schema")
	}
}

func TestErrorBodyScrubbedAndTruncated(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
		json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "bad key sekret-key " + strings.Repeat("z", 500)}})
	}))
	defer srv.Close()
	_, err := (&Client{}).Complete(context.Background(), policy.ModelEndpoint{URL: srv.URL + "/", Model: "m", APIKey: "sekret-key"}, Request{})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 401 || strings.Contains(se.Message, "sekret-key") || len([]rune(se.Message)) != maxMessage {
		t.Fatalf("err=%v", err)
	}
	if (&StatusError{Code: 502}).Error() == "" {
		t.Fatal("empty")
	}
}
