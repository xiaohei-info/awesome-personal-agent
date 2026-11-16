package modelstub

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

type cand struct {
	Title    string `json:"title"`
	Kind     string `json:"kind"`
	Summary  string `json:"summary"`
	Project  string `json:"project"`
	AttachTo string `json:"attach_to"`
	Refs     []int  `json:"refs"`
}

func extractOf(t *testing.T, base, key string) []cand {
	t.Helper()
	_, out, _ := chat(t, base, `{"model":"m","messages":[{"role":"user","content":"triage-extract `+key+`"}]}`)
	var v struct {
		Candidates []cand `json:"candidates"`
	}
	if err := json.Unmarshal([]byte(content(t, out)), &v); err != nil {
		t.Fatalf("%s 的输出不是 JSON: %v", key, err)
	}
	return v.Candidates
}

func draftOf(t *testing.T, base, key string) map[string]any {
	t.Helper()
	_, out, _ := chat(t, base, `{"model":"m","messages":[{"role":"user","content":"triage-draft `+key+`"}]}`)
	var v map[string]any
	if err := json.Unmarshal([]byte(content(t, out)), &v); err != nil {
		t.Fatalf("%s 的输出不是 JSON: %v", key, err)
	}
	return v
}

// ACT-WBENV-04：按匹配键分别得到三种抽取脚本与三种起草脚本。
func TestWBScriptReturnsCaseByKey(t *testing.T) {
	s := New()
	defer s.Close()
	s.Script(WBScript(WBParams{}))

	two := extractOf(t, s.URL(), "two-refs")
	if len(two) != 2 || two[0].Project != "acc-work" || two[1].Project != "acc-home" ||
		len(two[0].Refs) != 2 || len(two[1].Refs) != 1 || two[0].AttachTo != "X1" || two[1].AttachTo != "" {
		t.Fatalf("two-refs = %+v", two)
	}

	bad := extractOf(t, s.URL(), "bad-target")
	if len(bad) == 0 || bad[0].AttachTo != "no-such-item" || bad[0].Project != "no-such-project" {
		t.Fatalf("bad-target = %+v", bad)
	}

	_, out, _ := chat(t, s.URL(), `{"messages":[{"content":"legacy3"}]}`)
	var raw struct {
		Candidates []map[string]any `json:"candidates"`
	}
	json.Unmarshal([]byte(content(t, out)), &raw)
	if len(raw.Candidates) != 1 || len(raw.Candidates[0]) != 3 {
		t.Fatalf("legacy3 候选应只有 title/kind/summary: %+v", raw.Candidates)
	}

	if n := len(extractOf(t, s.URL(), "cands-25")); n != 25 {
		t.Fatalf("cands-25 候选数 = %d", n)
	}

	if d := draftOf(t, s.URL(), "urgent-true"); d["urgent"] != true || d["title"] == "" {
		t.Fatalf("urgent-true = %v", d)
	}
	if d := draftOf(t, s.URL(), "urgent-false"); d["urgent"] != false {
		t.Fatalf("urgent-false = %v", d)
	}
	if d := draftOf(t, s.URL(), "urgent-missing"); d["title"] == "" || d["kind"] == "" {
		t.Fatalf("urgent-missing 应是合法起草输出: %v", d)
	} else if _, ok := d["urgent"]; ok {
		t.Fatalf("urgent-missing 不应带 urgent: %v", d)
	}
}

// 自定义清单内事项 id 与项目。
func TestWBParamsOverride(t *testing.T) {
	s := New()
	defer s.Close()
	s.Script(WBScript(WBParams{ProjectA: "pa", ProjectB: "pb", AttachTo: "it-9"}))
	two := extractOf(t, s.URL(), "two-refs")
	if two[0].Project != "pa" || two[1].Project != "pb" || two[0].AttachTo != "it-9" {
		t.Fatalf("%+v", two)
	}
}

// UseWBCase：不靠匹配键，直接让后续所有请求得到该脚本（"MODEL_STUB 改用 legacy3"）。
func TestUseWBCaseSwitchesDefault(t *testing.T) {
	s := New()
	defer s.Close()
	if err := s.UseWBCase("legacy3", WBParams{}); err != nil {
		t.Fatal(err)
	}
	_, out, _ := chat(t, s.URL(), `{"messages":[{"content":"anything"}]}`)
	if !strings.Contains(content(t, out), `"summary"`) {
		t.Fatalf("content = %s", content(t, out))
	}
	if err := s.UseWBCase("nope", WBParams{}); err == nil {
		t.Fatal("未知用例应报错")
	}
}

// MODEL_LOG 完整记录请求体：open_items 清单、16 KiB 资料正文、中文都逐字节保留，并可经控制接口读回。
func TestRequestLogKeepsOpenItemsAndMaterial(t *testing.T) {
	s := New()
	defer s.Close()
	material := strings.Repeat("资料正文abc ", 2000)
	bodyBytes, _ := json.Marshal(map[string]any{"model": "m", "messages": []any{
		map[string]any{"role": "user", "content": "triage-extract two-refs"},
		map[string]any{"role": "user", "content": map[string]any{
			"open_items": []map[string]string{{"id": "X1", "title": "合同"}, {"id": "X2", "title": "会议室"}},
			"material":   material}},
	}})
	body := string(bodyBytes)
	chat(t, s.URL(), body)

	resp, err := http.Get(strings.TrimSuffix(s.URL(), "/v1") + "/_stub/requests")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var logged []Request
	if err := json.Unmarshal(raw, &logged); err != nil || len(logged) != 1 {
		t.Fatalf("log = %s err=%v", raw, err)
	}
	if logged[0].BodyText != body {
		t.Fatal("请求体应逐字节保留")
	}
	for _, want := range []string{`"open_items"`, `"id":"X1"`, `"id":"X2"`, material} {
		if !strings.Contains(logged[0].BodyText, want) {
			t.Errorf("请求体缺 %.40s", want)
		}
	}
}
