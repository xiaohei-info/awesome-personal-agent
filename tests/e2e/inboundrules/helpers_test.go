//go:build e2e

package inboundrules

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const waitMax = 45 * time.Second

// irlScript 是 IRL_SCRIPT：把 stdin 存到 $W/stdin-<来源>-<序号>.json，原样输出 $W/out-<来源>.jsonl，末行给游标。
const irlScript = `#!/bin/sh
D="$1"; S="$2"
n=$(ls "$D"/stdin-"$S"-* 2>/dev/null | wc -l | tr -d ' ')
cat > "$D/stdin-$S-$n.json"
[ -f "$D/out-$S.jsonl" ] && cat "$D/out-$S.jsonl"
echo '{"cursor":"'"$(date +%s%N)"'"}'
`

type fx struct {
	*baseline.Env
	t *testing.T
	w string // W_IRL
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

func (f *fx) must(err error) {
	f.t.Helper()
	if err != nil {
		f.t.Fatal(err)
	}
}

// step 运行一个验收动作；失败后不再继续（对应"本流程不得继续"）。
func (f *fx) step(name string, fn func()) {
	f.t.Helper()
	parent := f.t
	ok := parent.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn()
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func (f *fx) until(what string, cond func() bool) {
	f.t.Helper()
	if !f.WaitFor(waitMax, cond) {
		f.t.Fatalf("%v 内没有等到：%s", waitMax, what)
	}
}

// ---- 来源与事件 ----

type event struct {
	ID        string `json:"id"`
	DedupeKey struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Type           string             `json:"type"`
	Subject        string             `json:"subject"`
	Disposition    string             `json:"disposition"`
	Item           string             `json:"item"`
	Items          []string           `json:"items"`
	Judgment       string             `json:"judgment"`
	UnjudgedReason string             `json:"unjudged_reason"`
	MergeKey       string             `json:"merge_key"`
	Score          *float64           `json:"score"`
	Metrics        map[string]float64 `json:"metrics"`
}

type source struct {
	f   *fx
	id  string
	out string
}

// register 登记一个 type=script 来源（周期取登记允许的最小值 1 秒，其余机制与分钟级周期相同）。
func (f *fx) register(id, sourceType, domain string) *source {
	f.t.Helper()
	script := filepath.Join(f.w, "irl-script.sh")
	f.must(os.WriteFile(script, []byte(irlScript), 0o755))
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "source_type": sourceType, "dedupe_domain": domain,
		"command": "/bin/sh " + script + " " + f.w + " " + id, "schedule": "1s", "timeout": 30})
	f.webOK("POST", "/sources", string(body), 201)
	return &source{f: f, id: id, out: filepath.Join(f.w, "out-"+id+".jsonl")}
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// ev 拼一条标准事件；extra 是附加的 JSON 片段（不带外层花括号）。
func ev(id, typ, subject, extra string) string {
	s := fmt.Sprintf(`{"id":%q,"type":%q,"subject":%q,"occurred_at":%q`, id, typ, subject, now())
	if extra != "" {
		s += "," + extra
	}
	return s + "}"
}

func (s *source) events() []event {
	s.f.t.Helper()
	r := s.f.APIRaw("GET", "/v1/sources/"+s.id+"/events?limit=500", "", s.f.OBS)
	if r.Code != 200 {
		s.f.t.Fatalf("读来源事件 HTTP %d: %s", r.Code, r.Body)
	}
	var out struct {
		Events []event `json:"events"`
	}
	s.f.must(json.Unmarshal([]byte(r.Body), &out))
	return out.Events
}

func (s *source) find(id string) (event, bool) {
	for _, e := range s.events() {
		if e.DedupeKey.EventID == id {
			return e, true
		}
	}
	return event{}, false
}

// put 让来源下一个周期只输出 lines，返回这些事件的去重 id。
func (s *source) put(lines ...string) []string {
	s.f.t.Helper()
	s.f.must(os.WriteFile(s.out+".tmp", []byte(strings.Join(lines, "\n")+"\n"), 0o644))
	s.f.must(os.Rename(s.out+".tmp", s.out))
	var ids []string
	for _, l := range lines {
		var m struct {
			ID string `json:"id"`
		}
		s.f.must(json.Unmarshal([]byte(l), &m))
		ids = append(ids, m.ID)
	}
	return ids
}

// wait 等 ids 都落盘且处置完毕（不再 pending）。
func (s *source) wait(ids ...string) map[string]event {
	s.f.t.Helper()
	got := map[string]event{}
	defer func() {
		if len(got) < len(ids) {
			evs, _ := json.Marshal(s.events())
			log := s.f.ServiceLog()
			if len(log) > 3000 {
				log = log[len(log)-3000:]
			}
			s.f.t.Logf("诊断：来源事件 %s\nCLS 批数 %d，MODEL 请求数 %d\n内核日志尾部：%s", evs, s.f.clsLines(), s.f.modelLines(), log)
		}
	}()
	s.f.until(s.id+" 的事件 "+strings.Join(ids, ",")+" 处置完毕", func() bool {
		for _, id := range ids {
			e, ok := s.find(id)
			if !ok || e.Disposition == "" || e.Disposition == "pending" {
				return false
			}
			got[id] = e
		}
		return true
	})
	return got
}

// emit = put + wait。
func (s *source) emit(lines ...string) map[string]event { return s.wait(s.put(lines...)...) }

// quiet 清空输出，并等来源再跑一次，使后续步骤不再受旧输出影响。
func (s *source) quiet() { _ = os.WriteFile(s.out, nil, 0o644) }

// ---- 事项与审计 ----

type item struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Phase   string `json:"phase"`
	Project string `json:"project"`
}

func (f *fx) item(id string) item {
	f.t.Helper()
	var out struct {
		Item item `json:"item"`
	}
	f.must(json.Unmarshal([]byte(f.webOK("GET", "/items/"+id, "", 200)), &out))
	return out.Item
}

func (f *fx) proposedCount() int {
	f.t.Helper()
	var out struct {
		Items []item `json:"items"`
	}
	f.must(json.Unmarshal([]byte(f.webOK("GET", "/items?phase=proposed", "", 200)), &out))
	return len(out.Items)
}

type audit struct {
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Reason  string         `json:"reason"`
	Payload map[string]any `json:"payload"`
}

func (f *fx) audit(object, typ string) []audit {
	f.t.Helper()
	var out struct {
		Events []audit `json:"events"`
	}
	f.must(json.Unmarshal([]byte(f.webOK("GET", "/audit/events?limit=500&object="+object+"&type="+typ, "", 200)), &out))
	return out.Events
}

// ruleOf 返回事件的处置记录里命中的规则 id。
func (f *fx) ruleOf(e event) string {
	for _, a := range f.audit(e.ID, "inbound.disposed") {
		if r, _ := a.Payload["rule"].(string); r != "" {
			return r
		}
	}
	return ""
}

// ---- 两份模型替身日志 ----

func (f *fx) clsMentions(id string) int {
	n := 0
	for _, b := range f.Cls.Batches() {
		if strings.Contains(b.BodyText, id) {
			n++
		}
	}
	return n
}

func (f *fx) modelMentions(id string) int {
	n := 0
	for _, r := range f.ModelStub.Requests() {
		if strings.Contains(r.BodyText, id) {
			n++
		}
	}
	return n
}

func (f *fx) noModelCalls(id string) {
	f.t.Helper()
	if c, m := f.clsMentions(id), f.modelMentions(id); c != 0 || m != 0 {
		f.t.Errorf("%s 不应调用任何模型端点，实际 CLS_LOG %d 条、MODEL_LOG %d 条", id, c, m)
	}
}

func (f *fx) clsLines() int { return len(f.Cls.Batches()) }

func (f *fx) modelLines() int { return len(f.ModelStub.Requests()) }

// 两个端点的请求体里没有来源事件的去重 id（只有标题、摘要等内容），所以替身与日志断言都按事件标题里的唯一片段匹配（"题目内容"）。
func (f *fx) pYes(subject string, p float64) {
	f.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: subject}, PYes: &p})
}

func (f *fx) draft(subject string, reply any) {
	b, _ := json.Marshal(reply)
	f.ModelStub.When(subject, modelstub.Reply{Content: string(b)})
}

// judgment 读事件的判断记录（GET /judgments/{id}）；模板名只在记录里，不在发给端点的内容里。
func (f *fx) judgment(e event) map[string]any {
	f.t.Helper()
	if e.Judgment == "" {
		f.t.Fatalf("事件 %s 没有判断记录", e.DedupeKey.EventID)
	}
	var m map[string]any
	f.must(json.Unmarshal([]byte(f.webOK("GET", "/judgments/"+e.Judgment, "", 200)), &m))
	return m
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}
