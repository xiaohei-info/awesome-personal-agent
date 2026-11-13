//go:build e2e

package inbound

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	inpSrc     = "src_inp" // INP_SRC：验收用脚本来源（source_type=acc，没有任何内置规则命中）
	wait       = 90 * time.Second
	exitDenied = 4
)

type kit struct {
	*baseline.Env
	t     *testing.T
	queue string // INP_EVENTS 所在目录：in/ 放待输出的事件文件，脚本输出后移到 done/
	seq   int

	// 替身日志的"清空"：记下当前条数，之后只看新增的请求。
	clsOff, modelOff int
}

// inpScript 是 INP_SCRIPT：把收到的 stdin 存盘，输出队列目录里的事件，末行给游标（Source 执行契约，§9.2）。
const inpScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

// newKit 建好环境：隔离 APA_HOME、真实 apa 二进制、五个凭据、四个项目、两个端点替身（ACT-ENV-09 接线，都是 local）。
func newKit(t *testing.T) *kit {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints()
	k := &kit{Env: e, t: t, queue: filepath.Join(e.Root, "inp-source")}
	k.must(os.MkdirAll(filepath.Join(k.queue, "in"), 0o755))
	k.must(os.MkdirAll(filepath.Join(k.queue, "done"), 0o755))
	k.must(os.WriteFile(filepath.Join(k.queue, "inp.sh"), []byte(inpScript), 0o755))
	return k
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
	r := k.Web(method, path, body)
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

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// ---- 替身日志与用例（$CLS_LOG、$MODEL_LOG、$CLS_CASES、$MODEL_CASES） ----

// clear 是验收文档的"清空 $CLS_LOG、$MODEL_LOG"：之后的断言只看清空以后收到的请求。
func (k *kit) clear() {
	k.clsOff = len(k.Cls.Batches())
	k.modelOff = len(k.ModelStub.Requests())
}

// plain 把请求体里的 JSON 转义还原：中文、链接在请求体里可能被 \u 转义。
func plain(s string) string {
	var v any
	if json.Unmarshal([]byte(s), &v) != nil {
		return s
	}
	var sb strings.Builder
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case string:
			sb.WriteString(t + "\n")
			var inner any
			if len(t) > 1 && (t[0] == '{' || t[0] == '[') && json.Unmarshal([]byte(t), &inner) == nil {
				walk(inner)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for k, e := range t {
				sb.WriteString(k + "\n")
				walk(e)
			}
		}
	}
	walk(v)
	return sb.String() + "\n" + s
}

// clsAbout 是清空以后 CLS_STUB 收到的、题目含 mark 的批次（一批可含多题）。
func (k *kit) clsAbout(mark string) []string {
	var out []string
	for _, b := range k.Cls.Batches()[k.clsOff:] {
		if p := plain(b.BodyText); strings.Contains(p, mark) {
			out = append(out, p)
		}
	}
	return out
}

func (k *kit) clsQuestions() int {
	n := 0
	for _, b := range k.Cls.Batches()[k.clsOff:] {
		n += b.Questions
	}
	return n
}

// modelAbout 是清空以后 MODEL_STUB 收到的、含 mark 的请求；judge 为 true 取判断题，false 取生成请求（起草、抽取）。
func (k *kit) modelAbout(mark string, judge bool) []string {
	var out []string
	for _, r := range k.ModelStub.Requests()[k.modelOff:] {
		if baseline.IsJudgeRequest(r) != judge {
			continue
		}
		if p := plain(r.BodyText); strings.Contains(p, mark) {
			out = append(out, p)
		}
	}
	return out
}

// modelAny 是清空以后 MODEL_STUB 收到的、含 mark 的全部请求（判断题与生成）。
func (k *kit) modelAny(mark string) int {
	return len(k.modelAbout(mark, true)) + len(k.modelAbout(mark, false))
}

func keyOf(contains string) modelstub.Key { return modelstub.Key{Contains: contains} }

func replyJSON(s string) modelstub.Reply { return modelstub.Reply{Content: s} }

// screenCls 配置 triage-screen：含 mark 的题，分类端点 p_yes。
func (k *kit) screenCls(mark string, p float64) { k.Cls.PYes(keyOf(mark), p) }

// draft 配置 triage-draft：请求体含 match 时返回 content（按添加顺序首个命中；match 取事件 id 这类不会出现在别的事项里的串）。
func (k *kit) draft(match string, content any) {
	b, err := json.Marshal(content)
	k.must(err)
	k.ModelStub.When(match, replyJSON(string(b)))
}

// chatJudge 配置对话端点对含 mark 的判断题的作答。
func (k *kit) chatJudge(mark, answer string, sufficient bool) {
	k.ModelStub.Judge(modelstub.ChatJudge{Key: keyOf(mark), Answer: answer, Sufficient: &sufficient})
}

// ---- 来源 ----

// Ev 是脚本来源要输出的一条标准事件。
type Ev struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Subject  string   `json:"subject"`
	Summary  string   `json:"summary,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Link     string   `json:"link,omitempty"`
	Sender   string   `json:"sender,omitempty"`
	Keys     []string `json:"correlation_keys,omitempty"`
	Verified bool     `json:"sender_verified"`
	At       string   `json:"occurred_at"`
}

// emit 向 $INP_EVENTS 追加一行标准事件（下一个周期被脚本输出）。type 缺省 acc.msg。
func (k *kit) emit(id, subject string, mod ...func(*Ev)) {
	k.t.Helper()
	e := Ev{ID: id, Type: "acc.msg", Subject: subject, Summary: "摘要 " + subject, Detail: "正文 " + subject,
		Sender: id + "@example.invalid", Verified: true}
	for _, m := range mod {
		m(&e)
	}
	k.emitEv(e)
}

func (k *kit) emitEv(e Ev) {
	k.t.Helper()
	k.seq++
	e.At = time.Now().UTC().Format(time.RFC3339)
	if e.Type == "" {
		e.Type = "acc.msg"
	}
	b, _ := json.Marshal(e)
	tmp := filepath.Join(k.queue, fmt.Sprintf("tmp-%d", k.seq))
	k.must(os.WriteFile(tmp, append(b, '\n'), 0o644))
	k.must(os.Rename(tmp, filepath.Join(k.queue, "in", fmt.Sprintf("%04d.jsonl", k.seq))))
}

// SrcEvent 是来源页的一条事件（GET /sources/{id}/events）。
type SrcEvent struct {
	ID          string   `json:"id"`
	Subject     string   `json:"subject"`
	Disposition string   `json:"disposition"`
	Item        string   `json:"item"`
	Items       []string `json:"items"`
	Judgment    string   `json:"judgment"`
	Unjudged    string   `json:"unjudged_reason"`
	MergeKey    string   `json:"merge_key"`
	DedupeKey   struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
}

func (k *kit) events(src, query string) []SrcEvent {
	k.t.Helper()
	var r baseline.Resp
	if strings.HasPrefix(src, "capture:") { // 内部来源只对 Web 登录放行（ACT-CAP-11）
		r = k.Web("GET", "/v1/sources/"+src+"/events?limit=500"+query, "")
	} else {
		r = k.APIRaw("GET", "/v1/sources/"+src+"/events?limit=500"+query, "", k.OBS)
	}
	if r.Code != 200 {
		k.t.Fatalf("来源事件: %d %s", r.Code, r.Body)
	}
	var out struct {
		Events []SrcEvent `json:"events"`
	}
	k.must(json.Unmarshal([]byte(r.Body), &out))
	return out.Events
}

// byEventID 取来源页上某个来源事件 id 的全部记录（去重键末段）。
func (k *kit) byEventID(src, id string) []SrcEvent {
	var out []SrcEvent
	for _, e := range k.events(src, "") {
		if e.DedupeKey.EventID == id {
			out = append(out, e)
		}
	}
	return out
}

// waitEvent 等来源事件 id 落盘并有处置（不再 pending），最多 limit。
func (k *kit) waitEvent(src, id string, limit time.Duration) SrcEvent {
	k.t.Helper()
	var found SrcEvent
	if !k.WaitFor(limit, func() bool {
		for _, e := range k.byEventID(src, id) {
			if e.Disposition != "pending" && e.Disposition != "" {
				found = e
				return true
			}
		}
		return false
	}) {
		k.t.Fatalf("来源 %s 的事件 %s 在 %v 内没有被处置: %+v\n%s", src, id, limit, k.events(src, ""), k.ServiceLog())
	}
	return found
}

// quiet 等够两个拉取周期再看"没有发生"类的断言。
func quiet() { time.Sleep(4 * time.Second) }

func (k *kit) registerSource(id, defaultProject string) {
	k.t.Helper()
	m := map[string]any{"id": id, "type": "script", "source_type": "acc",
		"command": filepath.Join(k.queue, "inp.sh") + " " + k.queue, "schedule": "1s", "timeout": 30}
	if defaultProject != "" {
		m["default_project"] = defaultProject
	}
	body, _ := json.Marshal(m)
	k.webOK("POST", "/v1/sources", string(body), 201)
}

func (k *kit) registerInp() { k.registerSource(inpSrc, "") }

// ---- 读取内核状态 ----

type proposal struct {
	Recommend     string   `json:"recommend"`
	Reason        string   `json:"reason"`
	AttachTo      string   `json:"attach_to"`
	LowConfidence bool     `json:"low_confidence"`
	Sources       []string `json:"sources"`
}

type itemView struct {
	ID         string    `json:"id"`
	Phase      string    `json:"phase"`
	Kind       string    `json:"kind"`
	Title      string    `json:"title"`
	Summary    string    `json:"summary"`
	Project    string    `json:"project"`
	Version    int       `json:"version"`
	Deadline   string    `json:"deadline"`
	PlannedFor string    `json:"planned_for"`
	Proposal   *proposal `json:"proposal"`
}

func (k *kit) item(id string) itemView {
	k.t.Helper()
	var out struct {
		Item itemView `json:"item"`
	}
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "item", "show", id).Stdout), &out))
	return out.Item
}

// proposed 是建议流（phase=proposed 的事项）标题列表。
func (k *kit) proposed() []itemView {
	k.t.Helper()
	var out struct {
		Items []itemView `json:"items"`
	}
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "item", "list", "--phase", "proposed").Stdout), &out))
	return out.Items
}

func (k *kit) countTitled(title string) int {
	n := 0
	for _, it := range k.proposed() {
		if it.Title == title {
			n++
		}
	}
	return n
}

type judgmentView struct {
	DecidedBy     string `json:"decided_by"`
	Answer        string `json:"answer"`
	LowConfidence bool   `json:"low_confidence"`
	Levels        []struct {
		Backend    string   `json:"backend"`
		Outcome    string   `json:"outcome"`
		PYes       *float64 `json:"p_yes"`
		Answer     string   `json:"answer"`
		Sufficient *bool    `json:"sufficient"`
	} `json:"levels"`
}

func (k *kit) judgment(id string) judgmentView {
	k.t.Helper()
	if id == "" {
		k.t.Fatal("事件没有判断记录 id")
	}
	var j judgmentView
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "judgment", "show", id).Stdout), &j))
	return j
}

type auditEvent struct {
	Type   string          `json:"type"`
	Object string          `json:"object"`
	Actor  json.RawMessage `json:"actor"`
}

func (k *kit) audit(typ string) []auditEvent {
	k.t.Helper()
	var out struct {
		Events []auditEvent `json:"events"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/audit/events?limit=500&type="+typ, "", 200)), &out))
	return out.Events
}

func (k *kit) auditOf(typ, object string) []auditEvent {
	var out []auditEvent
	for _, e := range k.audit(typ) {
		if e.Object == object {
			out = append(out, e)
		}
	}
	return out
}

// writePack 把用户能力包写进 APA_HOME/packs（内核热加载；等价于在 Web 设置里加载），等包出现在 GET /v1/packs，
// 动作类规则还要等它出现在 GET /v1/rules。
func (k *kit) writePack(name, body string, ruleKeys ...string) {
	k.t.Helper()
	dir := filepath.Join(k.Home, "packs", name)
	k.must(os.MkdirAll(dir, 0o755))
	k.must(os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(body), 0o644))
	if !k.WaitFor(30*time.Second, func() bool { return strings.Contains(k.APIRaw("GET", "/v1/packs", "", k.OBS).Body, `"`+name+`"`) }) {
		k.t.Fatalf("能力包 %s 没有被加载\n%s", name, k.ServiceLog())
	}
	for _, key := range ruleKeys {
		if !k.WaitFor(30*time.Second, func() bool { return strings.Contains(k.webOK("GET", "/v1/rules", "", 200), key) }) {
			k.t.Fatalf("规则 %s 没有被加载\n%s", key, k.ServiceLog())
		}
	}
}

// removePack 停用用户包。
func (k *kit) removePack(name string) {
	k.t.Helper()
	k.must(os.RemoveAll(filepath.Join(k.Home, "packs", name)))
}

func failReply(status int) modelstub.Reply { return modelstub.Reply{Status: status} }

func readFile(p string) ([]byte, error) { return os.ReadFile(p) }

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

// ---- 账本夹具上的另一个 apa serve（ACT-INP-13、14） ----

// serveHome 用 bin 在夹具副本 home 上启动 `apa serve`（监听改到本机空闲端口），返回根地址与停止函数。
func (k *kit) serveHome(bin, home string) (string, func()) {
	k.t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	k.must(err)
	addr := l.Addr().String()
	k.must(l.Close())
	cfg := filepath.Join(home, "config.yaml")
	raw, err := os.ReadFile(cfg)
	k.must(err)
	lines := strings.Split(string(raw), "\n")
	found := false
	for i, ln := range lines {
		if strings.HasPrefix(ln, "listen:") {
			lines[i], found = "listen: "+addr, true
		}
	}
	if !found {
		lines = append(lines, "listen: "+addr)
	}
	k.must(os.WriteFile(cfg, []byte(strings.Join(lines, "\n")), 0o644))
	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	out, _ := cmd.StdoutPipe()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	k.must(cmd.Start())
	stop := func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() }
	k.t.Cleanup(stop)
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if !strings.Contains(line, `"ok":true`) {
			k.t.Fatalf("夹具上的 apa serve 没有起来: %q %s", line, errb.String())
		}
	case <-time.After(60 * time.Second):
		k.t.Fatalf("夹具上的 apa serve 60 秒内没有就绪: %s", errb.String())
	}
	return "http://" + addr, stop
}

// cli 用 bin 以 APA_URL/APA_TOKEN 运行一条 apa 命令。
func (k *kit) cli(bin, base, token string, args ...string) (string, int) {
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), "APA_URL="+base, "APA_TOKEN="+token)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		k.t.Fatalf("%s %v: %v", bin, args, err)
	}
	return out.String() + errb.String(), code
}

// fixtureHTTP 以 Bearer token（token 为空时不带）请求夹具内核。
func (k *kit) fixtureHTTP(base, method, path, token, body string, hdr map[string]string) (int, string, []*http.Cookie) {
	req, err := http.NewRequest(method, base+path, strings.NewReader(body))
	k.must(err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for h, v := range hdr {
		req.Header.Set(h, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	k.must(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Cookies()
}

func (k *kit) fixtureHTTP2(base, path string, hdr map[string]string) string {
	code, body, _ := k.fixtureHTTP(base, "GET", path, "", "", hdr)
	if code != 200 {
		k.t.Fatalf("GET %s: %d %s", path, code, body)
	}
	return body
}
