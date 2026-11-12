package modelstub

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func yesNoReq(model, user string) string {
	return `{"model":"` + model + `","messages":[{"role":"system","content":"只回答"},{"role":"user","content":` + jsonStr(user) + `}],` +
		`"response_format":{"type":"json_schema","json_schema":{"name":"j","schema":{"type":"object","properties":{"answer":{"enum":["yes","no"]},"sufficient":{"type":"boolean"}}}}}}`
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

func f(v float64) *float64 { return &v }
func b(v bool) *bool       { return &v }

// flow-agent-cascade: 同一匹配键下，分类端点与对话端点答案可以不同；对话端点结果不随调用顺序变化，
// 且收到的问法与输入就是分类端点那一级的（不含上一级答案）。
func TestSameQuestionTwoTiersDifferentAnswers(t *testing.T) {
	key := Key{Template: "triage-screen", EventID: "evt_42"}
	cls, chatStub := NewCls(), New()
	defer cls.Close()
	defer chatStub.Close()
	cls.PYes(key, 0.40) // 不确定带
	chatStub.Judge(ChatJudge{Key: key, Answer: "no", Sufficient: b(true)})
	chatStub.Judge(ChatJudge{Key: Key{EventID: "evt_43"}, Answer: "yes", Sufficient: b(false)})

	q := `{"id":"q1","question":"值不值得看?","criteria":"c","input":{"template":"triage-screen","event":"evt_42"},"material":"正文"}`
	r := call(t, cls, `{"protocol":"probability-judgment/1","request_id":"r","questions":[`+q+`]}`)
	if pOf(r, "q1") != 0.40 {
		t.Fatalf("cls %+v", r)
	}
	prompt := "值不值得看?\nc\n" + `{"template":"triage-screen","event":"evt_42"}` + "\n正文"
	for _, order := range [][]string{{"evt_42", "evt_43"}, {"evt_43", "evt_42"}, {"evt_42", "evt_42"}} {
		for _, ev := range order {
			up := prompt
			if ev == "evt_43" {
				up = strings.ReplaceAll(prompt, "evt_42", "evt_43")
			}
			out := chatContent(t, chatStub, yesNoReq("m", up))
			var got map[string]any
			json.Unmarshal([]byte(out), &got)
			if ev == "evt_42" && (got["answer"] != "no" || got["sufficient"] != true) {
				t.Fatalf("evt_42 -> %s", out)
			}
			if ev == "evt_43" && (got["answer"] != "yes" || got["sufficient"] != false) {
				t.Fatalf("evt_43 -> %s", out)
			}
		}
	}
	for _, rq := range chatStub.Requests() {
		if !strings.Contains(rq.BodyText, "值不值得看") || strings.Contains(rq.BodyText, "p_yes") || strings.Contains(rq.BodyText, "0.4") {
			t.Fatalf("recorded body should carry the question, not the previous tier's answer: %s", rq.BodyText)
		}
	}
}

func chatContent(t *testing.T, s *Stub, body string) string {
	t.Helper()
	_, out, _ := chat(t, s.URL(), body)
	return content(t, out)
}

func TestChatJudgeShapesAndFailures(t *testing.T) {
	s := New()
	defer s.Close()
	s.Judge(ChatJudge{Key: Key{Contains: "choose-me"}, Answer: "remind"})
	s.Judge(ChatJudge{Key: Key{Contains: "score-me"}, Score: f(0.8)})
	s.Judge(ChatJudge{Key: Key{Contains: "bad-schema"}, Raw: "not json"})
	s.Judge(ChatJudge{Key: Key{Contains: "boom"}, Status: 503})
	s.Judge(ChatJudge{Key: Key{Contains: "slow"}, DelayMS: 200})
	s.Judge(ChatJudge{Key: Key{Contains: "lowp"}, PYes: f(0.2)})

	schema := func(prop, text string) string {
		return `{"model":"m","messages":[{"role":"user","content":"` + text + `"}],"response_format":{"type":"json_schema","json_schema":{"schema":{"properties":{"` + prop + `":{},"sufficient":{}}}}}}`
	}
	if c := chatContent(t, s, schema("choice", "choose-me")); c != `{"choice":"remind","sufficient":true}` {
		t.Fatal(c)
	}
	if c := chatContent(t, s, schema("score", "score-me")); c != `{"score":0.8,"sufficient":true}` {
		t.Fatal(c)
	}
	if c := chatContent(t, s, schema("answer", "bad-schema")); c != "not json" {
		t.Fatal(c)
	}
	if c := chatContent(t, s, schema("answer", "lowp")); c != `{"answer":"no","sufficient":true}` {
		t.Fatal(c)
	}
	if st, _, _ := chat(t, s.URL(), schema("answer", "boom")); st != 503 {
		t.Fatalf("status %d", st)
	}
	if _, _, d := chat(t, s.URL(), schema("answer", "slow")); d.Milliseconds() < 190 {
		t.Fatal("delay not applied")
	}
}

// 首 token 对数概率：p_yes = P(yes)/(P(yes)+P(no)) 的换算路径可由替身驱动。
func TestChatLogprobs(t *testing.T) {
	s := New()
	defer s.Close()
	s.Judge(ChatJudge{Key: Key{Contains: "evt_lp"}, PYes: f(0.8)})
	_, out, _ := chat(t, s.URL(), `{"model":"m","max_tokens":1,"logprobs":true,"top_logprobs":5,"messages":[{"role":"user","content":"evt_lp ?"}]}`)
	ch := out["choices"].([]any)[0].(map[string]any)
	first := ch["logprobs"].(map[string]any)["content"].([]any)[0].(map[string]any)
	if first["token"] != "yes" || ch["message"].(map[string]any)["content"] != "yes" {
		t.Fatalf("%v", first)
	}
	var py, pn float64
	for _, c := range first["top_logprobs"].([]any) {
		m := c.(map[string]any)
		switch m["token"] {
		case "yes":
			py = math.Exp(m["logprob"].(float64))
		case "no":
			pn = math.Exp(m["logprob"].(float64))
		}
	}
	if got := py / (py + pn); math.Abs(got-0.8) > 1e-9 {
		t.Fatalf("p_yes = %v", got)
	}
}

// 保留原行为：生成请求与未命中配置的请求仍按脚本返回；判断配置不吞掉生成请求。
func TestGenerationRequestsStillScripted(t *testing.T) {
	s := New()
	defer s.Close()
	s.Judge(ChatJudge{Key: Key{Contains: "evt_1"}, Answer: "yes"})
	s.Enqueue(Reply{Content: "draft-1"})
	s.When("brief", Reply{Content: "brief-text"})
	gen := `{"model":"m","messages":[{"role":"user","content":"请起草 evt_1 的回信"}]}`
	if c := chatContent(t, s, gen); c != "draft-1" {
		t.Fatalf("generation must not be treated as judgment: %s", c)
	}
	if c := chatContent(t, s, `{"model":"m","messages":[{"role":"user","content":"brief"}]}`); c != "brief-text" {
		t.Fatal(c)
	}
	unmatched := `{"model":"m","messages":[{"role":"user","content":"zzz"}],"response_format":{"json_schema":{"schema":{"properties":{"answer":{},"sufficient":{}}}}}}`
	if c := chatContent(t, s, unmatched); c != "ok" {
		t.Fatalf("unmatched judgment falls back to script default: %s", c)
	}
}

func TestChatControlJudgesAndReset(t *testing.T) {
	s := New()
	defer s.Close()
	base := strings.TrimSuffix(s.URL(), "/v1")
	if resp, _ := post(t, base+"/_stub/script", `{"judges":[{"key":{"event_id":"evt_9"},"answer":"yes","sufficient":false}]}`); resp.StatusCode != 204 {
		t.Fatal(resp.StatusCode)
	}
	req := `{"messages":[{"role":"user","content":"evt_9"}],"response_format":{"json_schema":{"schema":{"properties":{"answer":{},"sufficient":{}}}}}}`
	if c := chatContent(t, s, req); c != `{"answer":"yes","sufficient":false}` {
		t.Fatal(c)
	}
	s.Reset()
	if c := chatContent(t, s, req); c != "ok" {
		t.Fatalf("reset should drop judges: %s", c)
	}
}
