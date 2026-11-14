//go:build e2e

package backlog

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// draftReply 是 triage-draft 生成请求的合法起草结果。
const draftReply = `{"title":"起草标题","summary":"起草摘要","kind":"task","recommend":"accept","reason":"值得看"}`

// kit 是一套"升级后夹具副本 + 真实二进制 + 两个模型端点替身"的验收环境（ACT-BKL-01、ACT-RST-01）。
type kit struct {
	*baseline.Env
	t   *testing.T
	fx  *baseline.Fixture
	cfg map[string]string

	// 替身日志的"清空"：记下当前条数，之后只看新增的请求。
	clsOff, modelOff int
}

// newKit 复制最新结构版本的账本夹具（LEDGER_FIXTURE_V<latest>），在其上启动 apa serve（unjudged_rejudge_days: 0、每日上限 5）。
// endpoints 为 false 时不接线任何模型端点（调用方自行配置）。
func newKit(t *testing.T, cfg map[string]string, wire bool) *kit {
	t.Helper()
	latest, err := ledgerfixture.Latest()
	if err != nil {
		t.Fatal(err)
	}
	e := baseline.New(t)
	fx := e.LedgerFixture(latest)
	c := map[string]string{"unjudged_rejudge_days": "0", "backlog_release_per_day": "5"}
	for k, v := range cfg {
		c[k] = v
	}
	e.ServeFixture(fx, c)
	k := &kit{Env: e, t: t, fx: fx, cfg: c}
	if wire {
		k.wire("local", "local", 0)
	}
	return k
}

// step 运行一个验收动作；失败后后续步骤不得开始。
func (k *kit) step(name string, f func(t *testing.T)) {
	k.t.Helper()
	parent := k.t
	ok := parent.Run(name, func(t *testing.T) {
		k.t, k.Env.T = t, t
		defer func() { k.t, k.Env.T = parent, parent }()
		f(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func (k *kit) must(err error) {
	k.t.Helper()
	if err != nil {
		k.t.Fatal(err)
	}
}

func (k *kit) webOK(method, path, body string, want int) string {
	k.t.Helper()
	r := k.Web(method, path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

// get 以 observer 凭据读内核并解析 JSON。
func (k *kit) get(path string, out any) {
	k.t.Helper()
	r := k.APIRaw("GET", path, "", k.OBS)
	if r.Code != 200 {
		k.t.Fatalf("GET %s: %d %s", path, r.Code, r.Body)
	}
	k.must(json.Unmarshal([]byte(r.Body), out))
}

// wire 在 Web 设置页把判断端点指向 CLS_STUB、生成端点指向 MODEL_STUB（位置 judge/generate），judge 的 max_batch 为 maxBatch（0 即缺省 32）。
func (k *kit) wire(judge, generate string, maxBatch int) {
	k.t.Helper()
	if k.Cls == nil {
		k.StartCls()
	}
	if k.ModelStub == nil {
		k.ModelStub = modelstub.NewAt("127.0.0.1:0")
	}
	j := map[string]any{"url": k.Cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1", "locality": judge}
	if maxBatch > 0 {
		j["max_batch"] = maxBatch
	}
	body, _ := json.Marshal(map[string]any{"judge_endpoint": j,
		"generate_endpoint":    map[string]any{"url": k.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": generate},
		"screening_timeout_ms": 30000})
	k.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
	k.settleSummaries()
}

// settleSummaries 等一句话摘要服务（kernel/summaries，按事件游标追赶）追上当前的事件日志末尾。
// 夹具的旧形状 model_endpoint 指向一个不可达的地址：内核一启动，摘要服务就拿夹具里缺摘要的事项去问它，
// 外呼调用器退避重试约 7 秒才放弃，余下的事项随后发往刚接上的 MODEL_STUB，其中带来源事件正文的会被 eventRequests 数进去。
// 这些请求属于摘要服务的追赶，与积压放开无关；不等它追上，它们何时落进后面哪一步的统计窗口取决于机器快慢。
func (k *kit) settleSummaries() {
	k.t.Helper()
	seq := k.maxSeq()
	if !k.WaitFor(60*time.Second, func() bool { return k.summariesCursor() >= seq }) {
		k.t.Fatalf("摘要服务 60 秒内没有追上事件 %d（游标 %d）", seq, k.summariesCursor())
	}
}

// summariesCursor 读账本里摘要服务的游标（只读打开）。
func (k *kit) summariesCursor() int64 {
	k.t.Helper()
	out, err := exec.Command("sqlite3", "-readonly", filepath.Join(k.Home, "ledger.db"), "SELECT seq FROM summaries_cursor WHERE id = 1").CombinedOutput()
	if err != nil {
		k.t.Fatalf("读摘要游标: %v %s", err, out)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		k.t.Fatalf("摘要游标不是整数: %q", out)
	}
	return n
}

// wireGenerateOnly 只配生成端点，判断端点留空。
func (k *kit) wireGenerateOnly() {
	k.t.Helper()
	k.ModelStub = modelstub.NewAt("127.0.0.1:0")
	body, _ := json.Marshal(map[string]any{"judge_endpoint": nil,
		"generate_endpoint":    map[string]any{"url": k.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": "local"},
		"screening_timeout_ms": 30000})
	k.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
}

// cases 配置 $CLS_CASES 与 $MODEL_CASES（ACT-BKL-01）：HIGH 0.90、MID 0.40、LOW 0.05；对话端点 MID 答否，其余答是；起草返回合法结果。
func (k *kit) cases() {
	k.t.Helper()
	k.Cls.PYes(modelstub.Key{Contains: "[FX-HIGH]"}, 0.90)
	k.Cls.PYes(modelstub.Key{Contains: "[FX-MID]"}, 0.40)
	k.Cls.PYes(modelstub.Key{Contains: "[FX-LOW]"}, 0.05)
	yes := true
	k.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "[FX-MID]"}, Answer: "no", Sufficient: &yes})
	k.ModelStub.Judge(modelstub.ChatJudge{Answer: "yes", Sufficient: &yes})
	k.ModelStub.SetDefault(modelstub.Reply{Content: draftReply})
}

// clear 是验收文档的"清空 $CLS_LOG、$MODEL_LOG"。
func (k *kit) clear() {
	k.clsOff, k.modelOff = 0, 0
	if k.Cls != nil {
		k.clsOff = len(k.Cls.Batches())
	}
	if k.ModelStub != nil {
		k.modelOff = len(k.ModelStub.Requests())
	}
}

func (k *kit) clsLog() []modelstub.ClsBatch {
	if k.Cls == nil {
		return nil
	}
	b := k.Cls.Batches()
	if k.clsOff > len(b) { // Cls.Reset 清空了替身的日志
		k.clsOff = 0
	}
	return b[k.clsOff:]
}

func (k *kit) modelLog() []modelstub.Request {
	if k.ModelStub == nil {
		return nil
	}
	r := k.ModelStub.Requests()
	if k.modelOff > len(r) {
		k.modelOff = 0
	}
	return r[k.modelOff:]
}

// judgeRequests 是 MODEL_STUB 收到的判断题（非起草请求）。
func (k *kit) judgeRequests() (out []modelstub.Request) {
	for _, r := range k.modelLog() {
		if baseline.IsJudgeRequest(r) && carriesEvent(r.BodyText) {
			out = append(out, r)
		}
	}
	return
}

func carriesEvent(body string) bool {
	return has(plain(body), "Old cascade ignored", "[FX-", "synthetic body", "sev_")
}

// eventRequests 是清空日志以来两个替身收到的、带来源事件内容（标题、正文、事件 id）的请求。
// 内核启动后夹具里与积压无关的后台任务（如过期的跟进提醒）也可能问一次模型，那不属于积压放开，所以"无请求"只数带事件内容的。
func (k *kit) eventRequests() (out []string) {
	carries := carriesEvent
	for _, b := range k.clsLog() {
		if carries(b.BodyText) {
			out = append(out, b.BodyText)
		}
	}
	for _, r := range k.modelLog() {
		if carries(r.BodyText) {
			out = append(out, r.BodyText)
		}
	}
	return
}

// run 以 token 运行 apa 子命令，env 为附加环境变量（会话引用、会话类型）。
func (k *kit) run(token string, env []string, args ...string) baseline.Result {
	k.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, k.Bin["apa"], args...)
	cmd.Env = append(os.Environ(), "APA_HOME="+k.Home, "APA_URL="+k.URL, "APA_TOKEN="+token)
	cmd.Env = append(cmd.Env, env...)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := baseline.Result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		k.t.Fatalf("运行 apa %v: %v", args, err)
	}
	return r
}

// owner 是"本人在私聊里说话"：delegate 凭据 + 会话引用 + 原话。
func (k *kit) owner(words string, args ...string) baseline.Result {
	k.t.Helper()
	return k.Apa(k.DEL, "", append(args, "--evidence", words)...)
}

func (k *kit) ownerOK(words string, out any, args ...string) {
	k.t.Helper()
	r := k.owner(words, args...)
	if r.Code != 0 {
		k.t.Fatalf("apa %s 退出码 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	}
	if out != nil {
		k.must(json.Unmarshal([]byte(r.Stdout), out))
	}
}

// ---- 来源事件 ----

type srcEvent struct {
	ID          string   `json:"id"`
	Source      string   `json:"source_id"`
	Subject     string   `json:"subject"`
	Disposition string   `json:"disposition"`
	Item        string   `json:"item"`
	Judgment    string   `json:"judgment"`
	Unjudged    string   `json:"unjudged_reason"`
	Score       *float64 `json:"score"`
}

func (k *kit) sources() (ids []string) {
	var s struct{ Sources []struct{ ID string } }
	k.get("/v1/sources", &s)
	for _, x := range s.Sources {
		ids = append(ids, x.ID)
	}
	return
}

// events 读一个来源里处置在 dispositions 里的全部事件（逗号分隔；sort 为空或 score），翻页读完。
func (k *kit) events(src, dispositions, sort string) (out []srcEvent) {
	k.t.Helper()
	for after := "0"; ; {
		q := "/v1/sources/" + src + "/events?disposition=" + dispositions + "&limit=500&after=" + after
		if sort != "" {
			q += "&sort=" + sort
		}
		var page struct {
			Events    []srcEvent
			NextAfter int64 `json:"next_after"`
		}
		k.get(q, &page)
		for _, e := range page.Events {
			e.Source = src
			out = append(out, e)
		}
		if page.NextAfter == 0 {
			return
		}
		after = strconv.FormatInt(page.NextAfter, 10)
	}
}

// all 读全部来源里处置在 dispositions 里的事件。
func (k *kit) all(dispositions string) (out []srcEvent) {
	for _, s := range k.sources() {
		out = append(out, k.events(s, dispositions, "")...)
	}
	return
}

// backlog 是积压：未判定与已到期。
func (k *kit) backlog() []srcEvent { return k.all("unjudged,expired") }

func countBy(es []srcEvent, f func(srcEvent) string) map[string]int {
	m := map[string]int{}
	for _, e := range es {
		m[f(e)]++
	}
	return m
}

func (k *kit) distribution() string {
	return fmt.Sprint(countBy(k.backlog(), func(e srcEvent) string { return e.Disposition + "/" + e.Unjudged }))
}

// ---- 审计事件流 ----

type auditEvent struct {
	Seq     int64                      `json:"seq"`
	Type    string                     `json:"type"`
	Actor   struct{ Principal string } `json:"actor"`
	Object  string                     `json:"object"`
	Payload json.RawMessage            `json:"payload"`
}

// audit 读事件流里 type 类型、序号大于 after 的全部事件。
func (k *kit) audit(typ string, after int64) (out []auditEvent) {
	k.t.Helper()
	for {
		var page struct{ Events []auditEvent }
		k.get("/v1/audit/events?type="+typ+"&limit=500&after="+strconv.FormatInt(after, 10), &page)
		out = append(out, page.Events...)
		if len(page.Events) < 500 {
			return
		}
		after = page.Events[len(page.Events)-1].Seq
	}
}

// maxSeq 是当前最大 Event 序号。
func (k *kit) maxSeq() (seq int64) {
	k.t.Helper()
	for {
		var page struct{ Events []auditEvent }
		k.get("/v1/audit/events?limit=500&after="+strconv.FormatInt(seq, 10), &page)
		if len(page.Events) == 0 {
			return
		}
		seq = page.Events[len(page.Events)-1].Seq
	}
}

// inboundSince 是 seq 之后事件流里与来源事件处置相关的 Event（inbound.*、item.created）。
// 夹具的时间在过去，内核启动后会有与积压无关的后台 Event（如 followup.reminded），所以不比较最大序号。
func (k *kit) inboundSince(seq int64) (out []auditEvent) {
	k.t.Helper()
	for {
		var page struct{ Events []auditEvent }
		k.get("/v1/audit/events?limit=500&after="+strconv.FormatInt(seq, 10), &page)
		if len(page.Events) == 0 {
			return
		}
		for _, e := range page.Events {
			if strings.HasPrefix(e.Type, "inbound.") || e.Type == "item.created" {
				out = append(out, e)
			}
		}
		seq = page.Events[len(page.Events)-1].Seq
	}
}

type health struct {
	Signals []struct {
		ID     string
		Count  int
		Detail json.RawMessage
	}
}

func (k *kit) unjudgedHealth() (count int, detail map[string]any, signals int) {
	k.t.Helper()
	var h health
	k.get("/v1/health", &h)
	for _, s := range h.Signals {
		if s.ID == "unjudged_backlog" {
			count = s.Count
			k.must(json.Unmarshal(s.Detail, &detail))
		}
	}
	return count, detail, len(h.Signals)
}

func num(m map[string]any, key string) int { f, _ := m[key].(float64); return int(f) }

// proposed 是处置为 proposed 的事件集合（事件 id → 事件）。
func (k *kit) proposed() map[string]srcEvent {
	m := map[string]srcEvent{}
	for _, e := range k.all("proposed") {
		m[e.ID] = e
	}
	return m
}

type judgmentView struct {
	ID        string `json:"id"`
	DecidedBy string `json:"decided_by"`
	Answer    string `json:"answer"`
	Shadow    bool   `json:"shadow"`
	Levels    []struct {
		Backend  string   `json:"backend"`
		Locality string   `json:"locality"`
		Outcome  string   `json:"outcome"`
		PYes     *float64 `json:"p_yes"`
	} `json:"levels"`
}

func (k *kit) judgment(id string) judgmentView {
	k.t.Helper()
	if id == "" {
		k.t.Fatal("事件没有判断记录 id")
	}
	var j judgmentView
	k.must(json.Unmarshal([]byte(k.Apa(k.OBS, "", "judgment", "show", id).Stdout), &j))
	return j
}

// judgmentsOf 是一个来源事件的全部判断记录（含影子记录），按写入顺序。
func (k *kit) judgmentsOf(eventID string) []judgmentView {
	k.t.Helper()
	var out struct{ Judgments []judgmentView }
	k.get("/v1/judgments?subject="+eventID+"&limit=500", &out)
	return out.Judgments
}

// decision 是事件最近一条非影子判断记录；没有则 ok 为假。
func (k *kit) decision(eventID string) (j judgmentView, ok bool) {
	for _, r := range k.judgmentsOf(eventID) {
		if !r.Shadow {
			j, ok = r, true
		}
	}
	return
}

func has(s string, subs ...string) bool {
	for _, x := range subs {
		if strings.Contains(s, x) {
			return true
		}
	}
	return false
}

// clsAsk 向 CLS_STUB 发一道题，返回 p_yes。
func (k *kit) clsAsk(text string) float64 {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{"protocol": "probability-judgment/1", "questions": []map[string]any{
		{"id": "q1", "question": text, "criteria": "c", "input": map[string]string{"k": "v"}}}})
	resp, err := http.Post(k.Cls.URL(), "application/json", bytes.NewReader(body))
	k.must(err)
	defer resp.Body.Close()
	var out struct {
		Results []struct {
			PYes *float64 `json:"p_yes"`
		}
	}
	k.must(json.NewDecoder(resp.Body).Decode(&out))
	if len(out.Results) != 1 || out.Results[0].PYes == nil {
		k.t.Fatalf("CLS_STUB 没有给出 p_yes: %+v", out)
	}
	return *out.Results[0].PYes
}

// chatDraft 向 MODEL_STUB 发一条生成请求，返回回复内容。
func (k *kit) chatDraft(text string) string {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "acc-model", "messages": []map[string]string{{"role": "user", "content": text}}})
	resp, err := http.Post(k.ModelStub.URL()+"/chat/completions", "application/json", bytes.NewReader(body))
	k.must(err)
	defer resp.Body.Close()
	var out struct {
		Choices []struct{ Message struct{ Content string } }
	}
	k.must(json.NewDecoder(resp.Body).Decode(&out))
	if len(out.Choices) == 0 {
		k.t.Fatal("MODEL_STUB 没有回复")
	}
	return out.Choices[0].Message.Content
}
