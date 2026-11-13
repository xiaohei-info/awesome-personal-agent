//go:build e2e

package judge

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// fx 是 flow-judge-protocol / flow-endpoint-config / flow-agent-cascade 共用的验收环境：
// flow-env-baseline 的两个模型端点替身 + 自由文本来源 ACC_TEXT（验收文档 ACT-JDP-01）。
type fx struct {
	*baseline.Env
	t      *testing.T
	events string // $ACC_TEXT_EVENTS
	src    string // ACC_TEXT 的来源 id
	n      int
	conf   string // harness 包装脚本读取的开关文件
}

const accScript = `#!/bin/sh
[ -f "$1" ] && cat "$1"
echo '{"cursor":"c"}'
exit 0
`

// setup 建环境并按 ACT-JDP-01 接线：判断端点 → CLS_STUB、生成端点 → MODEL_STUB（均 local），ACC_TEXT 外发范围 local=full。
func setup(t *testing.T) *fx {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints()
	f := &fx{Env: e, t: t, src: "src_acctext", events: filepath.Join(e.Root, "acc_text_events.jsonl")}
	f.registerText()
	return f
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// ev 是 ACC_TEXT 写出的一条自由文本标准事件。
type ev struct {
	Subject, Detail, Sender string
	ID                      string // 缺省按 subject+序号生成（dedupe_key）
}

// write 往 $ACC_TEXT_EVENTS 追加事件，返回各事件的来源 id（与 ev.ID 一致）。
func (f *fx) write(evs ...ev) []string {
	f.t.Helper()
	var ids []string
	fh, err := os.OpenFile(f.events, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	var sb strings.Builder
	for _, e := range evs {
		if e.ID == "" {
			f.n++
			e.ID = fmt.Sprintf("%s-%d", e.Subject, f.n)
		}
		if e.Sender == "" {
			e.Sender = "alice@example.invalid"
		}
		b, _ := json.Marshal(map[string]any{"id": e.ID, "type": "acc.text", "subject": e.Subject, "detail": e.Detail,
			"sender": e.Sender, "occurred_at": "2030-01-01T00:00:00Z"})
		sb.Write(b)
		sb.WriteByte('\n')
		ids = append(ids, e.ID)
	}
	if _, err := fh.WriteString(sb.String()); err != nil {
		f.t.Fatal(err)
	}
	return ids
}

// srcEvent 是来源事件在 API 里的样子（本流程关心的部分）。
type srcEvent struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	Judgment    string `json:"judgment"`
	Unjudged    string `json:"unjudged_reason"`
	DedupeKey   struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
}

func (f *fx) srcEvents() []srcEvent {
	f.t.Helper()
	var out struct {
		Events []srcEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(f.APIRaw("GET", "/v1/sources/"+f.src+"/events?limit=500", "", f.OBS).Body), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

func (f *fx) srcEvent(eventID string) (srcEvent, bool) {
	for _, e := range f.srcEvents() {
		if e.DedupeKey.EventID == eventID {
			return e, true
		}
	}
	return srcEvent{}, false
}

// settled 等事件离开 pending（已有处置），最多 limit。
func (f *fx) settled(eventID string, limit time.Duration) srcEvent {
	f.t.Helper()
	var got srcEvent
	if !f.WaitFor(limit, func() bool {
		e, ok := f.srcEvent(eventID)
		got = e
		return ok && e.Disposition != "" && e.Disposition != "pending"
	}) {
		f.t.Fatalf("事件 %s 在 %v 内没有处置: %+v\n%s", eventID, limit, got, f.ServiceLog())
	}
	return got
}

// ---- 判断记录、替身日志、健康视图 ----

type level struct {
	Backend           string   `json:"backend"`
	Model             string   `json:"model"`
	PYes              *float64 `json:"p_yes"`
	Answer            string   `json:"answer"`
	ProbabilitySource string   `json:"probability_source"`
	Outcome           string   `json:"outcome"`
	ErrorCode         string   `json:"error_code"`
	Reused            bool     `json:"reused"`
}

type judgment struct {
	ID            string                      `json:"id"`
	Use           string                      `json:"use"`
	Template      string                      `json:"template"`
	Subject       string                      `json:"subject"`
	Band          struct{ Low, High float64 } `json:"band"`
	Levels        []level                     `json:"levels"`
	DecidedBy     string                      `json:"decided_by"`
	Answer        string                      `json:"answer"`
	LowConfidence bool                        `json:"low_confidence"`
}

// judgmentsOf 读某个主体（来源事件或事项）的判断记录，按创建先后。
func (f *fx) judgmentsOf(subject string) []judgment {
	f.t.Helper()
	var out struct {
		Judgments []judgment `json:"judgments"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/judgments?subject="+subject, "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Judgments
}

// judgmentOf 等来源事件处置完毕，返回它的（最后一条）判断记录。
func (f *fx) judgmentOf(eventID string) (srcEvent, judgment) {
	f.t.Helper()
	e := f.settled(eventID, 60*time.Second)
	js := f.judgmentsOf(e.ID)
	if len(js) == 0 {
		f.t.Fatalf("事件 %s 没有判断记录: %+v", eventID, e)
	}
	return e, js[len(js)-1]
}

// reset 清空两个替身的脚本与日志（验收文档：每个动作开始前清空 CLS_LOG 与 MODEL_LOG）。
func (f *fx) reset() {
	f.Cls.Reset()
	f.ModelStub.Reset()
}

// judgeRequests 是 MODEL_STUB 收到的判断题请求（生成请求不算）。
func (f *fx) judgeRequests() (out []string) {
	for _, r := range f.ModelStub.Requests() {
		if baseline.IsJudgeRequest(r) {
			out = append(out, r.BodyText)
		}
	}
	return
}

// genRequests 是 MODEL_STUB 收到的生成请求。
func (f *fx) genRequests() (out []string) {
	for _, r := range f.ModelStub.Requests() {
		if !baseline.IsJudgeRequest(r) {
			out = append(out, r.BodyText)
		}
	}
	return
}

type clsReq struct {
	Protocol  string `json:"protocol"`
	RequestID string `json:"request_id"`
	Questions []struct {
		ID, Question, Material string
		Input                  map[string]any `json:"input"`
		MaterialTruncated      bool           `json:"material_truncated"`
	} `json:"questions"`
}

func parseCls(t *testing.T, body string) clsReq {
	t.Helper()
	var r clsReq
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("CLS_STUB 请求体不是 JSON: %v\n%s", err, body)
	}
	return r
}

// health 返回 model_unavailable 信号的 detail 与计数。
func (f *fx) modelHealth() (count int, detail map[string]any) {
	f.t.Helper()
	var rep struct {
		Signals []struct {
			ID     string `json:"id"`
			Count  int    `json:"count"`
			Detail any    `json:"detail"`
		} `json:"signals"`
	}
	if err := json.Unmarshal([]byte(f.APIRaw("GET", "/v1/health", "", f.OBS).Body), &rep); err != nil {
		f.t.Fatal(err)
	}
	for _, s := range rep.Signals {
		if s.ID == "model_unavailable" {
			d, _ := s.Detail.(map[string]any)
			return s.Count, d
		}
	}
	f.t.Fatal("GET /health 没有 model_unavailable 信号")
	return
}

// setJudge 在 Web 改判断端点的字段（其余沿用 WireEndpoints 的接线）。
func (f *fx) setJudge(extra map[string]any) {
	f.t.Helper()
	j := map[string]any{"url": f.Cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1", "locality": "local"}
	for k, v := range extra {
		j[k] = v
	}
	b, _ := json.Marshal(map[string]any{"judge_endpoint": j})
	f.webOK("PUT", "/settings/model-endpoints", string(b), 200)
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// step 跑一个验收动作；动作内的辅助方法用子测试的 t 报告，失败后后续动作不得开始。
func (f *fx) step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	ok := t.Run(name, func(t *testing.T) {
		parent := f.t
		f.t = t
		defer func() { f.t = parent }()
		fn(t)
	})
	if !ok {
		t.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}

func pf(p float64) *float64 { return &p }

func netListen() (string, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	return l.Addr().String(), nil
}

func reply(content string) modelstub.Reply { return modelstub.Reply{Content: content} }
