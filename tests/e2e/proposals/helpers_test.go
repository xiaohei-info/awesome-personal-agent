//go:build e2e

package proposals

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

// 退出码（附录 B.1）。
const (
	exitDenied = 4
	exitBasis  = 23 // 代答缺依据
)

type kit struct {
	*baseline.Env
	t      *testing.T
	queues map[string]string // 脚本来源 id → 待输出目录
	seq    int

	// 替身日志的"清空"：记下当前条数，之后只看新增的请求。
	clsOff, modelOff int
}

// 脚本来源：把收到的 stdin 存盘，输出队列目录里的事件，末行给游标。
const sourceScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

// newKit 建好 flow-env-baseline 的替身环境：真实 apa（测试构建）、五个凭据、四个项目、两个端点替身、acc-stub 声明。
func newKit(t *testing.T) *kit {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints() // 判断端点 CLS_STUB、生成端点 MODEL_STUB（ACT-ENV-09）
	return &kit{Env: e, t: t, queues: map[string]string{}}
}

func (k *kit) must(err error) {
	k.t.Helper()
	if err != nil {
		k.t.Fatal(err)
	}
}

// step 运行一个验收动作；子测试期间 kit 与环境脚本的致命错误记在该子测试上，前一步失败后续不得开始。
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

func (k *kit) webOK(method, path, body string, want int) string {
	k.t.Helper()
	r := k.Web(method, "/v1"+path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (k *kit) apa(token string, want int, args ...string) baseline.Result {
	k.t.Helper()
	r := k.Apa(token, "", args...)
	if r.Code != want {
		k.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (k *kit) idOf(r baseline.Result) string {
	k.t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil || m.ID == "" {
		k.t.Fatalf("没有返回事项 id: %q %v", r.Stdout, err)
	}
	return m.ID
}

// ---- 事项 ----

// proposal 用 OBS 新建 proposed 建议（项目 acc-open）。
func (k *kit) proposal(title string) string {
	k.t.Helper()
	return k.idOf(k.apa(k.OBS, 0, "propose", "--kind", "task", "--title", title, "--project", k.Projects["acc-open"]))
}

// openItem 用 DEL 代本人建一个 open 事项（acc-open）。
func (k *kit) openItem(title string) string {
	k.t.Helper()
	return k.idOf(k.apa(k.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open",
		"--project", k.Projects["acc-open"], "--evidence", "记一下"))
}

// item 读事项对象（OBS 的 `apa item show`）。
func (k *kit) item(id string) map[string]any {
	k.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	r := k.apa(k.OBS, 0, "item", "show", id)
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil || out.Item == nil {
		k.t.Fatalf("item show %s 输出不是事项: %q", id, r.Stdout)
	}
	return out.Item
}

func (k *kit) phase(id string) string { s, _ := k.item(id)["phase"].(string); return s }

// proposalOf 是事项的只读字段 proposal（没有时为 nil）。
func (k *kit) proposalOf(id string) map[string]any {
	p, _ := k.item(id)["proposal"].(map[string]any)
	return p
}

func (k *kit) stringsOf(v any) []string {
	var out []string
	l, _ := v.([]any)
	for _, x := range l {
		s, _ := x.(string)
		out = append(out, s)
	}
	return out
}

// streamItems 是 Web 建议流的数据：GET /v1/items?phase=proposed（本人会话）。
func (k *kit) streamItems() (items []map[string]any, rejections map[string]map[string]any) {
	k.t.Helper()
	var out struct {
		Items      []map[string]any          `json:"items"`
		Rejections map[string]map[string]any `json:"last_rejections"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/items?phase=proposed&limit=500", "", 200)), &out))
	return out.Items, out.Rejections
}

func (k *kit) inStream(id string) bool {
	items, _ := k.streamItems()
	for _, it := range items {
		if it["id"] == id {
			return true
		}
	}
	return false
}

// inbox 把 `apa inbox` 拆成待决与建议两部分的原文。
func (k *kit) inbox() (pending, suggestions string) {
	k.t.Helper()
	out := k.apa(k.OBS, 0, "inbox").Stdout
	pending, suggestions, _ = strings.Cut(out, `"suggestions"`)
	return
}

// ---- 审计事件 ----

type step struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Actor   actor          `json:"actor"`
	Reason  string         `json:"reason"`
	Ev      string         `json:"evidence"`
	Payload map[string]any `json:"payload"`
}

type actor struct {
	Type        string `json:"type"`
	Principal   string `json:"principal"`
	Via         string `json:"via"`
	OnBehalfOf  string `json:"on_behalf_of"`
	Session     string `json:"session"`
	SessionKind string `json:"session_kind"`
}

// events 取某对象（空表示全部）某类型（空表示全部）的审计事件，即 Web 审计页与"agent 变更"视图的数据源。
func (k *kit) events(object, typ string) []step {
	k.t.Helper()
	var out struct {
		Events []step `json:"events"`
	}
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + object
	}
	if typ != "" {
		path += "&type=" + typ
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", path, "", 200)), &out))
	return out.Events
}

func (k *kit) last(object, typ string) step {
	k.t.Helper()
	es := k.events(object, typ)
	if len(es) == 0 {
		k.t.Fatalf("%s 上没有 %s 事件", object, typ)
	}
	return es[len(es)-1]
}

// maxSeq 是审计页当前最大 Event 序号。
func (k *kit) maxSeq() int64 {
	var m int64
	for _, e := range k.events("", "") {
		m = max(m, e.Seq)
	}
	return m
}

// agentChanges 是"agent 变更"视图的过滤口径（与 AgentChangesPage 一致）：actor.type=agent 且不是被拒绝的命令。
func (k *kit) agentChanges() []step {
	var out []step
	for _, s := range k.events("", "") {
		if s.Actor.Type == "agent" && s.Type != "command.rejected" {
			out = append(out, s)
		}
	}
	return out
}

func (k *kit) unread() int {
	k.t.Helper()
	var out struct {
		Deliveries []json.RawMessage `json:"deliveries"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/deliveries/unread", "", 200)), &out))
	return len(out.Deliveries)
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// ---- 脚本来源与来源事件 ----

// Ev 是脚本来源输出的一条标准事件。
type Ev struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Subject  string   `json:"subject"`
	Summary  string   `json:"summary,omitempty"`
	Link     string   `json:"link,omitempty"`
	Sender   string   `json:"sender,omitempty"`
	Keys     []string `json:"correlation_keys,omitempty"`
	Verified bool     `json:"sender_verified"`
	At       string   `json:"occurred_at"`
}

// registerSource 登记一个脚本来源（type=script、source_type=acc、周期每分钟→验收缩短为 1 秒、默认项目 acc-open）。
func (k *kit) registerSource(id string) {
	k.t.Helper()
	q := filepath.Join(k.Root, "src-"+id)
	k.must(os.MkdirAll(filepath.Join(q, "in"), 0o755))
	k.must(os.MkdirAll(filepath.Join(q, "done"), 0o755))
	script := filepath.Join(q, "source.sh")
	k.must(os.WriteFile(script, []byte(sourceScript), 0o755))
	k.queues[id] = q
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "source_type": "acc",
		"command": script + " " + q, "schedule": "1s", "timeout": 30, "default_project": k.Projects["acc-open"]})
	k.webOK("POST", "/sources", string(body), 201)
}

// emit 让来源 src 在下一个周期输出这条事件。
func (k *kit) emit(src string, e Ev) string {
	k.t.Helper()
	k.seq++
	if e.Type == "" {
		e.Type = "prh.message"
	}
	e.Verified = true
	e.At = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(e)
	q := k.queues[src]
	tmp := filepath.Join(q, fmt.Sprintf("tmp-%d", k.seq))
	k.must(os.WriteFile(tmp, append(b, '\n'), 0o644))
	k.must(os.Rename(tmp, filepath.Join(q, "in", fmt.Sprintf("%04d.jsonl", k.seq))))
	return e.ID
}

// SrcEvent 是来源页的一条事件（GET /sources/{id}/events，附录 B.2）。
type SrcEvent struct {
	ID          string   `json:"id"`
	SourceID    string   `json:"source_id"`
	Subject     string   `json:"subject"`
	Link        string   `json:"link"`
	Disposition string   `json:"disposition"`
	Item        string   `json:"item"`
	Items       []string `json:"items"`
	MergeKey    string   `json:"merge_key"`
	AttachState string   `json:"attach_state"`
	DedupeKey   struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
}

func (k *kit) srcEvents(src string) []SrcEvent {
	k.t.Helper()
	var out struct {
		Events []SrcEvent `json:"events"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/sources/"+src+"/events?limit=500", "", 200)), &out))
	return out.Events
}

func (k *kit) srcEvent(src, id string) (SrcEvent, bool) {
	for _, e := range k.srcEvents(src) {
		if e.DedupeKey.EventID == id {
			return e, true
		}
	}
	return SrcEvent{}, false
}

// settled 等来源事件被拉取并过完流水线（disposition 不再是 pending），最多 limit。
func (k *kit) settled(src, id string) SrcEvent {
	k.t.Helper()
	var ev SrcEvent
	if !k.WaitFor(60*time.Second, func() bool {
		var ok bool
		ev, ok = k.srcEvent(src, id)
		return ok && ev.Disposition != "" && ev.Disposition != "pending"
	}) {
		k.t.Fatalf("来源 %s 的事件 %s 在 60 秒内没有被处置\n%s", src, id, k.ServiceLog())
	}
	return ev
}

// proposalFrom 取来源事件生成的建议事项 id（事件须已 proposed）。
func (k *kit) proposalFrom(src, id string) string {
	k.t.Helper()
	ev := k.settled(src, id)
	if ev.Disposition != "proposed" || ev.Item == "" {
		k.t.Fatalf("事件 %s 应为 proposed 且有事项: %+v", id, ev)
	}
	return ev.Item
}

// ---- 替身用例（$CLS_CASES、$MODEL_CASES） ----

func ptr[T any](v T) *T { return &v }

// screen 配置 triage-screen：含 key 的题，判断端点 p_yes=p；pNo 时对话端点重答 no 且资料不足（"把握低"）。
// 规则按添加顺序首个命中，具体的关键字须先于通用关键字配置。
func (k *kit) screen(key string, p float64) {
	k.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: key}, PYes: ptr(p)})
}

func (k *kit) unsure(key string) {
	k.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: key}, Answer: "no", Sufficient: ptr(false)})
}

// draft 配置 triage-draft：请求里的来源标题为 subject 时返回给定起草结果。
// 请求体里的输入 JSON 经转义，所以用 `subject":"X"` 的转义形态做键：候选清单里的事项标题不会误命中。
func (k *kit) draft(subject string, d map[string]any) {
	b, _ := json.Marshal(d)
	key := `subject\":\"` + subject + `\"`
	k.ModelStub.When(key, modelstub.Reply{Content: string(b)})
}

// draftDefault 配置缺省起草：title 取事件 subject 加前缀"起草："、recommend accept、reason "需要跟进"。
func (k *kit) draftDefault(subject string) {
	k.draft(subject, map[string]any{"title": "起草：" + subject, "summary": "摘要 " + subject, "kind": "task", "recommend": "accept", "reason": "需要跟进"})
}

func (k *kit) clear() {
	k.clsOff = len(k.Cls.Batches())
	k.modelOff = len(k.ModelStub.Requests())
}

// modelLogWith 是清空以来 MODEL_STUB 收到的、提到 mark 的请求。
func (k *kit) modelLogWith(mark string) (n int) {
	for _, r := range k.ModelStub.Requests()[k.modelOff:] {
		if strings.Contains(r.BodyText, mark) {
			n++
		}
	}
	return
}

func (k *kit) clsLogWith(mark string) (n int) {
	for _, b := range k.Cls.Batches()[k.clsOff:] {
		if strings.Contains(b.BodyText, mark) {
			n++
		}
	}
	return
}
