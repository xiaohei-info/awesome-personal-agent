//go:build e2e

package judgment

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const waitMax = 45 * time.Second

type fx struct {
	*baseline.Env
	t            *testing.T
	dir          string // JRC_SRC 的事件文件目录
	statTemplate string // stats 的 --template 取值
	runOut       string // 目标事项上的运行写结果的目录
	srcID        string
}

// setup 建基线环境并把两个端点接成 CLS_STUB / MODEL_STUB（ACT-ENV-09）。
func setup(t *testing.T) *fx {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints()
	return &fx{Env: e, t: t, statTemplate: "triage-screen"}
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) get(token, path string) string {
	f.t.Helper()
	r := f.APIRaw("GET", path, "", token)
	if r.Code != 200 {
		f.t.Fatalf("GET %s: HTTP %d %s", path, r.Code, r.Body)
	}
	return r.Body
}

func (f *fx) until(what string, cond func() bool) {
	f.t.Helper()
	if !f.WaitFor(waitMax, cond) {
		f.t.Fatalf("%v 内没有等到：%s\n%s", waitMax, what, f.ServiceLog())
	}
}

// ---- ACT-JRC-01：JRC_SRC 与替身用例 ----

// registerSource 登记 JRC_SRC：脚本每次拉取原样输出事件文件的内容（按 id 去重），末行输出游标。
func (f *fx) registerSource() {
	f.t.Helper()
	f.dir = filepath.Join(f.Root, "jrc-src")
	must(f.t, os.MkdirAll(f.dir, 0o755))
	script := filepath.Join(f.dir, "jrc.sh")
	body := fmt.Sprintf("#!/bin/sh\ncat >/dev/null\ncat %[1]s/events.jsonl 2>/dev/null\necho '{\"cursor\":\"'$(date +%%s%%N)'\"}'\n", f.dir)
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.srcID = "src_jrc"
	b, _ := json.Marshal(map[string]any{"id": f.srcID, "type": "script", "source_type": "acc-jrc", "command": script, "schedule": "1s", "timeout": 20})
	f.webOK("POST", "/sources", string(b), 201)
	f.webOK("PUT", "/settings/sources/"+f.srcID+"/outbound", `{"local":"full","external":"none"}`, 200)
}

// emit 追加事件：subject 含关键字，detail 含 JRC-BODY（资料正文，判断记录里不得出现）。
func (f *fx) emit(keys ...string) {
	f.t.Helper()
	fh, err := os.OpenFile(filepath.Join(f.dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	for _, k := range keys {
		line, _ := json.Marshal(map[string]any{"id": "ev-" + k, "type": "acc.jrc", "subject": k + " 事件", "summary": k, "detail": "JRC-BODY 资料正文 " + k,
			"sender": "alice", "occurred_at": time.Now().UTC().Format(time.RFC3339)})
		fmt.Fprintln(fh, string(line))
	}
}

func (f *fx) cls(key string, p float64) { f.Cls.PYes(modelstub.Key{Contains: key}, p) }

type sourceEvent struct {
	Key struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	ID          string   `json:"id"`
	Disposition string   `json:"disposition"`
	Item        string   `json:"item"`
	Judgment    string   `json:"judgment"`
	Score       *float64 `json:"score"`
}

func (f *fx) sourceEvents(query string) []sourceEvent {
	f.t.Helper()
	var out struct {
		Events []sourceEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(f.get(f.OBS, "/v1/sources/"+f.srcID+"/events?limit=500"+query)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

func (f *fx) eventOf(key string) sourceEvent {
	f.t.Helper()
	var found sourceEvent
	f.until("来源事件 ev-"+key+" 已处置", func() bool {
		for _, e := range f.sourceEvents("") {
			if e.Key.EventID == "ev-"+key && e.Disposition != "" && e.Disposition != "pending" {
				found = e
				return true
			}
		}
		return false
	})
	return found
}

// ---- 记录、审计与统计的读取 ----

type level struct {
	Backend           string   `json:"backend"`
	Endpoint          string   `json:"endpoint"`
	Locality          string   `json:"locality"`
	Model             string   `json:"model"`
	PYes              *float64 `json:"p_yes"`
	Answer            string   `json:"answer"`
	Sufficient        *bool    `json:"sufficient"`
	ProbabilitySource string   `json:"probability_source"`
	Outcome           string   `json:"outcome"`
	DurationMs        *int64   `json:"duration_ms"`
}

type record struct {
	ID        string                      `json:"id"`
	Use       string                      `json:"use"`
	Template  string                      `json:"template"`
	Subject   string                      `json:"subject"`
	Source    string                      `json:"source"`
	Band      struct{ Low, High float64 } `json:"band"`
	Levels    []level                     `json:"levels"`
	DecidedBy string                      `json:"decided_by"`
	Answer    string                      `json:"answer"`
	LowConf   bool                        `json:"low_confidence"`
	Shadow    bool                        `json:"shadow"`
	Review    *struct {
		Result string `json:"result"`
		At     string `json:"at"`
	} `json:"review"`
}

func (f *fx) records(query string) []record {
	f.t.Helper()
	var out struct {
		Judgments []record `json:"judgments"`
	}
	if err := json.Unmarshal([]byte(f.get(f.OBS, "/v1/judgments?limit=500&"+query)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Judgments
}

func (f *fx) record(id string) record {
	f.t.Helper()
	var r record
	if err := json.Unmarshal([]byte(f.get(f.OBS, "/v1/judgments/"+id)), &r); err != nil {
		f.t.Fatal(err)
	}
	return r
}

// final 是事件的定案记录：非影子的那一条（影子记录不作为处置依据）。
func (f *fx) final(subject string) record {
	f.t.Helper()
	var out []record
	defer func() {
		if len(out) == 0 {
			b, _ := json.Marshal(f.records("subject=" + subject))
			f.t.Logf("事件 %s 的全部判断记录: %s", subject, b)
		}
	}()
	f.until("事件 "+subject+" 有非影子的判断记录", func() bool {
		out = out[:0]
		for _, r := range f.records("subject=" + subject) {
			if !r.Shadow {
				out = append(out, r)
			}
		}
		return len(out) > 0
	})
	return out[len(out)-1]
}

type auditStep struct {
	Seq      int64                                 `json:"seq"`
	Type     string                                `json:"type"`
	Object   string                                `json:"object"`
	Actor    struct{ Type, Principal, Via string } `json:"actor"`
	Payload  json.RawMessage                       `json:"payload"`
	Judgment *struct {
		ID        string  `json:"id"`
		Answer    string  `json:"answer"`
		DecidedBy string  `json:"decided_by"`
		Expired   bool    `json:"expired"`
		Record    *record `json:"record"`
	} `json:"judgment"`
}

// audit 以本人会话读全部 Event（带载荷）；typ 非空时按类型筛。
func (f *fx) audit(typ string) []auditStep {
	f.t.Helper()
	var all []auditStep
	after := int64(0)
	for {
		q := fmt.Sprintf("/audit/events?limit=500&after=%d", after)
		if typ != "" {
			q += "&type=" + typ
		}
		var out struct {
			Events []auditStep `json:"events"`
		}
		if err := json.Unmarshal([]byte(f.webOK("GET", q, "", 200)), &out); err != nil {
			f.t.Fatal(err)
		}
		if len(out.Events) == 0 {
			return all
		}
		all = append(all, out.Events...)
		after = out.Events[len(out.Events)-1].Seq
	}
}

func payloadField(raw json.RawMessage, key string) string {
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

type statRow struct {
	Backend                                   string `json:"backend"`
	Calls, Decided, Uncertain, Errors, Reused int
}

type statRate struct {
	Escalation float64 `json:"escalation"`
	Undecided  float64 `json:"undecided"`
	Acceptance float64 `json:"acceptance"`
	FalseDrop  float64 `json:"false_drop"`
	Reviewed   int     `json:"reviewed"`
}

type stats struct {
	Rows      []statRow  `json:"rows"`
	Rates     []statRate `json:"rates"`
	Histogram []struct {
		Low, High float64
		Count     int
	} `json:"histogram"`
}

func (f *fx) statsAPI(extra string) (stats, string) {
	f.t.Helper()
	body := f.get(f.OBS, "/v1/judgments/stats?use=inbound_screen&template="+url.QueryEscape(f.statTemplate)+"&source="+f.srcID+extra)
	var s stats
	if err := json.Unmarshal([]byte(body), &s); err != nil {
		f.t.Fatal(err)
	}
	return s, body
}

func (s stats) row(backend string) statRow {
	for _, r := range s.Rows {
		if r.Backend == backend {
			return r
		}
	}
	return statRow{}
}

func near(a, b float64) bool { return math.Abs(a-b) < 0.01 }

// httpPost 直接向替身发一道题（验收文档 ACT-JRC-01 的 curl）。
func httpPost(t *testing.T, url string, body any) string {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return string(raw)
}
