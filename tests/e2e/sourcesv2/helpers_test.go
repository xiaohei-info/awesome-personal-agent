//go:build e2e

// Package sourcesv2 是全流程验收 flow-source-github、flow-source-mail、flow-source-calendar 的端到端测试
// （验收文档 ACT-GH-*、ACT-MAIL-*、ACT-CAL-*），替身版：真实 apa 二进制（-tags apa_testclock）、随发行附带的
// 代码托管/邮件/日历拉取程序，外部世界是 TOOL_STUB 工具替身（gh、himalaya、gog、mail-cli…）与 CLS_STUB、MODEL_STUB。
// 来源按 1 秒周期登记以缩短等待（登记允许的最小周期，其余机制与分钟级周期相同）。
// 离不开真实依赖的断言（真实 gh/himalaya/gog 的输出形态、真实渠道账号）由本机升级验证 L1–L12 承接。
// Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）。
package sourcesv2

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	waitMax    = 40 * time.Second
	exitConfig = "exit status 2" // 拉取程序的"需要本人处理"退出码 2（设计 §9.2），来源统计的 last_error 以它开头
)

type kit struct {
	*baseline.Env
	t           *testing.T
	clockOffset int64 // 已写入测试时钟的偏移秒数
}

var (
	progOnce sync.Once
	progDir  string
	progErr  error
)

// programs 每个测试进程只构建一次随发行附带的拉取程序，返回所在目录。
func repoRoot() string { return filepath.Join("..", "..", "..") }

func programs(t *testing.T) string {
	t.Helper()
	progOnce.Do(func() {
		progDir, progErr = os.MkdirTemp("", "apa-e2e-sourcesv2-")
		if progErr != nil {
			return
		}
		root := filepath.Join("..", "..", "..")
		for name, pkg := range map[string]string{
			"github-source":    "./sources/github/cmd/github-source",
			"mail-himalaya":    "./sources/mail/cmd/mail-himalaya",
			"calendar":         "./sources/calendar/cmd/calendar",
			"apple-pim-source": "./sources/applepim/cmd/apple-pim-source",
		} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(progDir, name), pkg)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				progErr = fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
				return
			}
		}
	})
	if progErr != nil {
		t.Fatal(progErr)
	}
	return progDir
}

// setup 建环境：flow-env-baseline 的全套（init/serve/凭据/项目/MODEL_STUB/CLS_STUB/无头运行声明）+ 工具替身 + 渠道替身。
func setup(t *testing.T) *kit {
	t.Helper()
	dir := programs(t)
	// 登记的拉取程序只认裸名（设计 §9.1），apa serve 继承测试进程的 PATH 来找到它们；工具替身目录随后放在最前。
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	e := baseline.Setup(t)
	e.WireEndpoints()
	e.InstallToolStub()
	// gh 样本固定在 2026-11-10，而内核时钟不会倒退：平移到此刻之后，用测试时钟逐个放出（见 tests/doubles/tools/gh_rebase.go）。
	// 只在本包开启；环境基线的重放仍拿到录制时的原样输出。
	if err := os.WriteFile(filepath.Join(e.ToolBin, "gh-epoch"), []byte(time.Now().UTC().Format(time.RFC3339)), 0o644); err != nil {
		t.Fatal(err)
	}
	return &kit{Env: e, t: t}
}

func (k *kit) apa(token string, want int, args ...string) baseline.Result {
	k.t.Helper()
	r := k.Apa(token, "", args...)
	if r.Code != want {
		k.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (k *kit) webOK(method, path, body string, want int) string {
	k.t.Helper()
	r := k.Web(method, "/v1"+path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (k *kit) get(path string) string {
	k.t.Helper()
	r := k.APIRaw("GET", path, "", k.OBS)
	if r.Code != 200 {
		k.t.Fatalf("GET %s: HTTP %d %s", path, r.Code, r.Body)
	}
	return r.Body
}

// until 轮询到条件成立，否则带上说明失败。
func (k *kit) until(what string, cond func() bool) {
	k.t.Helper()
	if !k.WaitFor(waitMax, cond) {
		k.t.Fatalf("%v 内没有等到：%s\n%s", waitMax, what, k.ServiceLog())
	}
}

func (k *kit) idOf(r baseline.Result) string {
	k.t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &m); err != nil || m.ID == "" {
		k.t.Fatalf("没有返回 id: %q %v", r.Stdout, err)
	}
	return m.ID
}

// ---- 来源与事件 ----

// addPull 用 `apa source add` 登记附带的拉取程序（type=pull，DEL 可登记），周期 1 秒。
func (k *kit) addPull(id, program, sourceType string, extra map[string]any, config any) {
	k.t.Helper()
	cfg, _ := json.Marshal(config)
	args := []string{"source", "add", "--id", id, "--type", "pull", "--command", program, "--schedule", "1s", "--timeout", "20",
		"--source-type", sourceType, "--config", string(cfg), "--evidence", "登记附带的拉取程序"}
	for flag, v := range extra {
		args = append(args, "--"+flag, fmt.Sprint(v))
	}
	k.apa(k.DEL, 0, args...)
}

// SrcState 是 `apa source show` 的输出里本测试关心的部分。
type SrcState struct {
	ID             string `json:"id"`
	Enabled        bool   `json:"enabled"`
	SourceType     string `json:"source_type"`
	DedupeDomain   string `json:"dedupe_domain"`
	DefaultProject string `json:"default_project"`
	Cursor         string `json:"cursor"`
	Stats          struct {
		Runs                int    `json:"runs"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		LastError           string `json:"last_error"`
		NeedsInputItem      string `json:"needs_input_item"`
	} `json:"stats"`
}

func (k *kit) show(id string) SrcState {
	k.t.Helper()
	var s SrcState
	r := k.apa(k.OBS, 0, "source", "show", id)
	if err := json.Unmarshal([]byte(r.Stdout), &s); err != nil {
		k.t.Fatalf("source show 输出不是 JSON: %q", r.Stdout)
	}
	return s
}

// cycle 等来源至少再完成 n 次拉取（无论成败），用于"等一个周期"。
func (k *kit) cycle(id string, n int) SrcState {
	k.t.Helper()
	start := k.show(id).Stats.Runs
	k.until(fmt.Sprintf("%s 再运行 %d 次", id, n), func() bool { return k.show(id).Stats.Runs >= start+n })
	return k.show(id)
}

// SrcEvent 是来源页的一条事件（GET /sources/{id}/events）。
type SrcEvent struct {
	ID        string `json:"id"`
	DedupeKey struct {
		Domain  string `json:"domain"`
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Type            string   `json:"type"`
	Subject         string   `json:"subject"`
	Disposition     string   `json:"disposition"`
	Item            string   `json:"item"`
	AttachState     string   `json:"attach_state"`
	CorrelationKeys []string `json:"correlation_keys"`
	SenderVerified  bool     `json:"sender_verified"`
	Rule            string   `json:"rule"`
	Raw             map[string]any
}

func (k *kit) events(src string) []SrcEvent {
	k.t.Helper()
	body := k.get("/v1/sources/" + src + "/events?limit=500")
	var out struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		k.t.Fatal(err)
	}
	var evs []SrcEvent
	for _, raw := range out.Events {
		var e SrcEvent
		if err := json.Unmarshal(raw, &e); err != nil {
			k.t.Fatal(err)
		}
		_ = json.Unmarshal(raw, &e.Raw)
		evs = append(evs, e)
	}
	return evs
}

func (k *kit) event(src, eventID string) (SrcEvent, bool) {
	for _, e := range k.events(src) {
		if e.DedupeKey.EventID == eventID {
			return e, true
		}
	}
	return SrcEvent{}, false
}

// settled 等事件入账并过完流水线（disposition 不再是 pending），返回该事件。
func (k *kit) settled(src, eventID string) SrcEvent {
	k.t.Helper()
	var ev SrcEvent
	k.until("事件 "+eventID+" 完成处置", func() bool {
		var ok bool
		ev, ok = k.event(src, eventID)
		return ok && ev.Disposition != "pending"
	})
	return ev
}

// settledPrefix 等事件 id 以 prefix 开头的事件入账并完成处置（id 带时间戳后缀的事件）。
func (k *kit) settledPrefix(src, prefix string) SrcEvent {
	k.t.Helper()
	var ev SrcEvent
	k.until("事件 "+prefix+"* 完成处置", func() bool {
		for _, e := range k.events(src) {
			if strings.HasPrefix(e.DedupeKey.EventID, prefix) && e.Disposition != "pending" {
				ev = e
				return true
			}
		}
		return false
	})
	return ev
}

// itemIDs 是账本里全部事项的 id（apa item list）。
func (k *kit) itemIDs() []string {
	k.t.Helper()
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(k.apa(k.OBS, 0, "item", "list").Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	var ids []string
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	return ids
}

// rejects 经 `apa source rejects` 读某来源没有入账的输出行。
func (k *kit) rejects(id string) []string {
	k.t.Helper()
	var out struct {
		Rejects []struct {
			Reason string `json:"reason"`
		} `json:"rejects"`
	}
	if err := json.Unmarshal([]byte(k.apa(k.OBS, 0, "source", "rejects", id).Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	var rs []string
	for _, r := range out.Rejects {
		rs = append(rs, r.Reason)
	}
	return rs
}

// ---- 事项 ----

type Item struct {
	ID      string   `json:"id"`
	Title   string   `json:"title"`
	Kind    string   `json:"kind"`
	Phase   string   `json:"phase"`
	Project string   `json:"project"`
	Tags    []string `json:"tags"`
	Raw     map[string]any
}

func (k *kit) item(id string) Item {
	k.t.Helper()
	r := k.apa(k.OBS, 0, "item", "show", id)
	var out struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	var it Item
	if err := json.Unmarshal(out.Item, &it); err != nil {
		k.t.Fatal(err)
	}
	_ = json.Unmarshal(out.Item, &it.Raw)
	return it
}

// auditEvents 读某类型的审计事件中对象为 object 的。
type Audit struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Reason string `json:"reason"`
	Actor  struct {
		Kind string `json:"kind"`
		ID   string `json:"id"`
		Via  string `json:"via"`
	} `json:"actor"`
}

func (k *kit) audit(typ, object string) []Audit {
	k.t.Helper()
	var out struct {
		Events []Audit `json:"events"`
	}
	body := k.webOK("GET", "/audit/events?limit=500&"+url.Values{"type": {typ}}.Encode(), "", 200)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		k.t.Fatal(err)
	}
	var m []Audit
	for _, e := range out.Events {
		if e.Object == object {
			m = append(m, e)
		}
	}
	return m
}

// ---- 模型替身 ----

// clsYes 给 CLS_STUB 的一道题配 p_yes（题目内容含 key）。
func (k *kit) clsYes(key string, p float64) { k.Cls.PYes(modelstub.Key{Contains: key}, p) }

// draft 让 MODEL_STUB 对标题为 subject 的事件的起草请求（triage-draft）返回给定 JSON。
// 起草的结构化输入里 subject 之后紧跟 today，判断题的输入没有 today，据此只命中起草请求。
func (k *kit) draft(subject string, out map[string]any) {
	b, _ := json.Marshal(out)
	k.ModelStub.When(`\"subject\":\"`+subject+`\",\"today\"`, modelstub.Reply{Content: string(b)})
}

// chatJudge 配 MODEL_STUB 的判断题作答（对话端点，题目内容含 subject）。
func (k *kit) chatJudge(subject, answer string, sufficient bool) {
	k.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: subject}, Answer: answer, Sufficient: &sufficient})
}

// modelLines 是两份模型替身日志（CLS_LOG、MODEL_LOG）的当前总行数。
func (k *kit) modelLines() int { return len(k.Cls.Batches()) + len(k.ModelStub.Requests()) }

// modelAbout 数两份模型替身里含 text 的请求数。
func (k *kit) modelAbout(text string) int {
	n := 0
	for _, r := range k.ModelStub.Requests() {
		if strings.Contains(r.BodyText, text) {
			n++
		}
	}
	for _, b := range k.Cls.Batches() {
		if strings.Contains(b.BodyText, text) {
			n++
		}
	}
	return n
}

func (k *kit) toolCalls(tool string) [][]string {
	var out [][]string
	for _, le := range k.ToolLog() {
		if le.Tool == tool {
			out = append(out, le.Argv)
		}
	}
	return out
}

// WhyStep 是 `apa why` 回放里的一步。
type WhyStep struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Reason string `json:"reason"`
	Actor  struct {
		Type      string `json:"type"`
		Principal string `json:"principal"`
		Via       string `json:"via"`
	} `json:"actor"`
	Basis struct {
		RuleID string `json:"rule_id"`
	} `json:"basis"`
	Payload  map[string]any `json:"payload"`
	Evidence string         `json:"evidence"`
	Judgment *struct {
		ID        string    `json:"id"`
		DecidedBy string    `json:"decided_by"`
		Record    *Judgment `json:"record"`
	} `json:"judgment"`
}

// why 以本人身份（DEL）回放事项或来源事件。
func (k *kit) why(id string) []WhyStep {
	k.t.Helper()
	var out struct {
		Steps []WhyStep `json:"steps"`
	}
	if err := json.Unmarshal([]byte(k.apa(k.DEL, 0, "why", id).Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Steps
}

func stepOf(steps []WhyStep, typ string) (WhyStep, bool) {
	for _, s := range steps {
		if s.Type == typ {
			return s, true
		}
	}
	return WhyStep{}, false
}

// eventRule 报告来源事件的处置（apa why <来源事件>）命中的规则是否为 rule（规则 id 的末段）。
func (k *kit) eventRule(ev SrcEvent, rule string) bool {
	k.t.Helper()
	d, ok := stepOf(k.why(ev.ID), "inbound.disposed")
	r, _ := d.Payload["rule"].(string)
	return ok && strings.HasSuffix(r, "/"+rule)
}

// ordered 报告 steps 里这些类型的首次出现是否按给定顺序。
func ordered(steps []WhyStep, types ...string) bool {
	pos := -1
	for _, typ := range types {
		found := -1
		for i, s := range steps {
			if s.Type == typ {
				found = i
				break
			}
		}
		if found <= pos {
			return false
		}
		pos = found
	}
	return true
}

// ruleHit 报告事项的来龙去脉（apa why）里是否有一步依据是规则 rule。
func (k *kit) ruleHit(item, rule string) bool {
	k.t.Helper()
	for _, s := range k.why(item) {
		if s.Basis.RuleID == rule {
			return true
		}
	}
	return false
}

// Judgment 是判断记录（GET /judgments/{id}）里本测试关心的部分。
type Judgment struct {
	ID        string `json:"id"`
	Use       string `json:"use"`
	Template  string `json:"template"`
	DecidedBy string `json:"decided_by"`
	Answer    string `json:"answer"`
	Levels    []struct {
		Backend string   `json:"backend"`
		PYes    *float64 `json:"p_yes"`
		Outcome string   `json:"outcome"`
	} `json:"levels"`
}

// judgment 读来源事件上的初筛判断记录。
func (k *kit) judgment(ev SrcEvent) Judgment {
	k.t.Helper()
	id, _ := ev.Raw["judgment"].(string)
	if id == "" {
		k.t.Fatalf("事件 %s 没有判断记录", ev.ID)
	}
	var j Judgment
	if err := json.Unmarshal([]byte(k.get("/v1/judgments/"+id)), &j); err != nil {
		k.t.Fatal(err)
	}
	return j
}

// judgeRequests 数 MODEL_STUB 收到的、题目含 subject 的判断题请求（对话端点的重答）。
func (k *kit) judgeRequests(subject string) int {
	n := 0
	for _, r := range k.ModelStub.Requests() {
		if baseline.IsJudgeRequest(r) && strings.Contains(r.BodyText, subject) {
			n++
		}
	}
	return n
}

// suggestions 是建议流里的事项 id（apa inbox 的 suggestions）。
func (k *kit) suggestions() []string {
	k.t.Helper()
	var out struct {
		Suggestions []struct {
			ID string `json:"id"`
		} `json:"suggestions"`
	}
	if err := json.Unmarshal([]byte(k.apa(k.OBS, 0, "inbox").Stdout), &out); err != nil {
		k.t.Fatal(err)
	}
	var ids []string
	for _, s := range out.Suggestions {
		ids = append(ids, s.ID)
	}
	return ids
}

// hasDraftRequest 报告 MODEL_STUB 是否收到过标题含 subject 的起草请求（triage-draft）。
func (k *kit) hasDraftRequest(subject string) bool {
	for _, r := range k.ModelStub.Requests() {
		if strings.Contains(r.BodyText, "起草一条建议") && strings.Contains(r.BodyText, subject) {
			return true
		}
	}
	return false
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// step 运行一个验收动作；子测试期间 kit 与环境脚本的致命错误记在该子测试上，失败则后续动作不得开始。
func (k *kit) step(name string, f func(t *testing.T)) {
	k.t.Helper()
	parent := k.t
	ok := parent.Run(name, func(t *testing.T) {
		k.t, k.Env.T = t, t
		defer func() { k.t, k.Env.T = parent, parent }()
		f(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}

// ghAt 切换代码托管工具替身的 case，并把测试时钟推到该 case 样本之后 30 分钟：样本 N 小时的 case 对外时间为
// 安装时刻 + N 小时（见 tests/doubles/tools/gh_rebase.go），时钟推过它，下一个周期就读到。
func (k *kit) ghAt(c string, hour int) {
	k.t.Helper()
	b, err := os.ReadFile(filepath.Join(k.ToolBin, "gh-epoch"))
	if err != nil {
		k.t.Fatal(err)
	}
	epoch, err := time.Parse(time.RFC3339, strings.TrimSpace(string(b)))
	if err != nil {
		k.t.Fatal(err)
	}
	k.SetToolCase(c)
	target := epoch.Add(time.Duration(hour)*time.Hour + 30*time.Minute)
	if off := int64(time.Until(target) / time.Second); off > k.clockOffset {
		k.clockOffset = off
		k.TCSet(off)
	}
}
