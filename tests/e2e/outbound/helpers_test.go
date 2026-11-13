//go:build e2e

package outbound

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
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	obdSrc = "src_obd"
	// 验收文档里 OBD_SRC 的事件内容。
	linkURL = "https://docs.example.org/path/secret-123"
	sender  = "alice@example.org"
)

type kit struct {
	*baseline.Env
	t     *testing.T
	top   *testing.T // 顶层测试：跨步骤存活的资源（PATH、订阅服务）挂在它上面
	queue string     // OBD_SRC 的待输出目录
	seq   int

	// 替身日志的"清空"：记下当前条数，之后只看新增的请求。
	clsOff, modelOff int
}

// obdScript 是 OBD_SRC：把收到的 stdin 存盘，输出队列目录里的事件，末行给游标。
const obdScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

func newKit(t *testing.T) *kit {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints()
	k := &kit{Env: e, t: t, top: t, queue: filepath.Join(e.Root, "obd-source")}
	k.must(os.MkdirAll(filepath.Join(k.queue, "in"), 0o755))
	k.must(os.MkdirAll(filepath.Join(k.queue, "done"), 0o755))
	k.must(os.WriteFile(filepath.Join(k.queue, "obd.sh"), []byte(obdScript), 0o755))
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

// ---- 端点位置与外发范围（Web 设置） ----

// endpoints 在 Web 设置页保存两个端点的位置（local / external）；判断端点指向 CLS_STUB，生成端点指向 MODEL_STUB。
func (k *kit) endpoints(judge, generate string) {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{
		"judge_endpoint":       map[string]any{"url": k.Cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1", "locality": judge},
		"generate_endpoint":    map[string]any{"url": k.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": generate},
		"screening_timeout_ms": 2000,
	})
	k.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
}

type endpointView struct {
	Judge    struct{ Locality string } `json:"judge_endpoint"`
	Generate struct{ Locality string } `json:"generate_endpoint"`
}

func (k *kit) endpointView() endpointView {
	k.t.Helper()
	var v endpointView
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/settings/model-endpoints", "", 200)), &v))
	return v
}

// scope 在 Web 把来源的外发范围设为 local / external（external 为 "" 表示未选择）。
func (k *kit) scope(src, local, external string) {
	k.t.Helper()
	var ext any
	if external != "" {
		ext = external
	}
	body, _ := json.Marshal(map[string]any{"local": local, "external": ext})
	k.webOK("PUT", "/v1/settings/sources/"+src+"/outbound", string(body), 200)
}

// ---- 替身日志 ----

// clear 是验收文档的"清空 $CLS_LOG、$MODEL_LOG"：之后的断言只看清空以后收到的请求。
func (k *kit) clear() {
	k.clsOff = len(k.Cls.Batches())
	k.modelOff = len(k.ModelStub.Requests())
}

func (k *kit) clsLog() []string {
	var out []string
	for _, b := range k.Cls.Batches()[k.clsOff:] {
		out = append(out, b.BodyText)
	}
	return out
}

func (k *kit) modelLog() []string {
	var out []string
	for _, r := range k.ModelStub.Requests()[k.modelOff:] {
		out = append(out, r.BodyText)
	}
	return out
}

// 把 JSON 里的转义还原后再比对：HEAD-MARK 这类 ASCII 标记不受影响，但链接、中文标题在请求体里可能被 \u 转义。
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
	return sb.String()
}

// requestsWith 返回日志里提到 mark 的请求体（已还原转义）。
func requestsWith(log []string, mark string) []string {
	var out []string
	for _, b := range log {
		if strings.Contains(b, mark) || strings.Contains(plain(b), mark) {
			out = append(out, plain(b)+"\n"+b)
		}
	}
	return out
}

// draftRequests 是 MODEL_STUB 收到的、提到 mark 的起草请求（非判断题）。
func (k *kit) draftRequests(mark string) []string {
	var out []string
	for _, r := range k.ModelStub.Requests()[k.modelOff:] {
		if baseline.IsJudgeRequest(r) {
			continue
		}
		if strings.Contains(r.BodyText, mark) || strings.Contains(plain(r.BodyText), mark) {
			out = append(out, plain(r.BodyText)+"\n"+r.BodyText)
		}
	}
	return out
}

// screenRequests 是 MODEL_STUB 收到的、提到 mark 的判断题（对话端点作答）。
func (k *kit) screenRequests(mark string) []string {
	var out []string
	for _, r := range k.ModelStub.Requests()[k.modelOff:] {
		if !baseline.IsJudgeRequest(r) {
			continue
		}
		if strings.Contains(r.BodyText, mark) || strings.Contains(plain(r.BodyText), mark) {
			out = append(out, plain(r.BodyText)+"\n"+r.BodyText)
		}
	}
	return out
}

func (k *kit) contains(label string, bodies []string, want ...string) {
	k.t.Helper()
	if len(bodies) == 0 {
		k.t.Fatalf("%s: 没有收到任何请求", label)
	}
	for _, b := range bodies {
		for _, w := range want {
			if !strings.Contains(b, w) {
				k.t.Fatalf("%s: 请求体缺 %q\n%s", label, w, b)
			}
		}
	}
}

func (k *kit) lacks(label string, bodies []string, banned ...string) {
	k.t.Helper()
	for _, b := range bodies {
		for _, w := range banned {
			if strings.Contains(b, w) {
				k.t.Fatalf("%s: 请求体不应出现 %q\n%s", label, w, b)
			}
		}
	}
}

// ---- 替身用例（$CLS_CASES、$MODEL_CASES） ----

// cases 配置验收文档 ACT-OBD-01 的用例：含 mark 的 triage-screen 题，分类端点 p_yes=0.90，对话端点 answer=yes、sufficient=true；
// triage-draft 生成请求返回合法起草结果。
func (k *kit) cases(mark string) {
	k.Cls.PYes(modelstub.Key{Contains: mark}, 0.90)
	yes := true
	k.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: mark}, Answer: "yes", Sufficient: &yes})
	draft, _ := json.Marshal(map[string]any{"title": "处理 " + mark, "summary": "来源事件需要处理", "kind": "task", "recommend": "accept", "reason": "需要本人处理"})
	k.ModelStub.When(mark, modelstub.Reply{Content: string(draft)})
}

// ---- 来源 ----

// Ev 是 OBD_SRC 输出的一条标准事件。
type Ev struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Subject  string `json:"subject"`
	Summary  string `json:"summary,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Link     string `json:"link,omitempty"`
	Sender   string `json:"sender,omitempty"`
	Verified bool   `json:"sender_verified"`
	At       string `json:"occurred_at"`
}

// body2000 是 2000 个字符的正文：HEAD-MARK 开头，第 600 个字符处 MID-MARK，末尾 TAIL-MARK。
func body2000(head, mid, tail string) string {
	b := []byte(strings.Repeat("x", 2000))
	copy(b, head)
	copy(b[599:], mid)
	copy(b[2000-len(tail):], tail)
	return string(b)
}

// emit 让 OBD_SRC 在下一个周期输出 OBD-<n> 事件（验收文档 ACT-OBD-02 的事件形状）。
func (k *kit) emit(n int) string {
	k.t.Helper()
	subject := fmt.Sprintf("OBD-%d 外发测试", n)
	return k.emitEv(Ev{ID: fmt.Sprintf("obd-%d", n), Type: "obd.message", Subject: subject, Sender: sender, Verified: true,
		Link: linkURL, Summary: "SUM-MARK", Detail: body2000("HEAD-MARK", "MID-MARK", "TAIL-MARK")})
}

func (k *kit) emitEv(e Ev) string {
	k.t.Helper()
	k.seq++
	e.At = time.Now().UTC().Format(time.RFC3339)
	b, _ := json.Marshal(e)
	tmp := filepath.Join(k.queue, fmt.Sprintf("tmp-%d", k.seq))
	k.must(os.WriteFile(tmp, append(b, '\n'), 0o644))
	k.must(os.Rename(tmp, filepath.Join(k.queue, "in", fmt.Sprintf("%04d.jsonl", k.seq))))
	return e.ID
}

// SrcEvent 是来源页的一条事件（GET /sources/{id}/events）。
type SrcEvent struct {
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

func (k *kit) events(src, query string) []SrcEvent {
	k.t.Helper()
	var r baseline.Resp
	if strings.HasPrefix(src, "capture:") { // 内部来源只对 Web 登录放行
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

// waitEvent 等 subject 含 mark 的事件落盘并有处置（不再 pending），最多 limit。
func (k *kit) waitEvent(src, mark string, limit time.Duration) SrcEvent {
	k.t.Helper()
	var found SrcEvent
	if !k.WaitFor(limit, func() bool {
		for _, e := range k.events(src, "") {
			if strings.Contains(e.Subject, mark) && e.Disposition != "pending" && e.Disposition != "" {
				found = e
				return true
			}
		}
		return false
	}) {
		k.t.Fatalf("来源 %s 的事件 %s 在 %v 内没有被处置\n%s", src, mark, limit, k.ServiceLog())
	}
	return found
}

func (k *kit) registerObd() {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{"id": obdSrc, "type": "script", "source_type": "acc-obd",
		"command": filepath.Join(k.queue, "obd.sh") + " " + k.queue, "schedule": "1s", "timeout": 30})
	k.webOK("POST", "/v1/sources", string(body), 201)
}

// ---- 读取内核状态 ----

type outboundView struct {
	Local    string  `json:"local"`
	External *string `json:"external"`
}

type sourceView struct {
	DefaultProject string       `json:"default_project"`
	Outbound       outboundView `json:"outbound"`
}

// sourceShow 是 `APA_TOKEN=$OBS apa source show <id>`。
func (k *kit) sourceShow(id string) sourceView {
	k.t.Helper()
	var s sourceView
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "source", "show", id).Stdout), &s))
	return s
}

type judgmentView struct {
	DecidedBy string `json:"decided_by"`
	Levels    []struct {
		Backend  string `json:"backend"`
		Locality string `json:"locality"`
		Outcome  string `json:"outcome"`
	} `json:"levels"`
}

// judgment 是 `APA_TOKEN=$OBS apa judgment show <id>`。
func (k *kit) judgment(id string) judgmentView {
	k.t.Helper()
	if id == "" {
		k.t.Fatal("事件没有判断记录 id")
	}
	var j judgmentView
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "judgment", "show", id).Stdout), &j))
	return j
}

// proposals 是建议流里的事项条数（phase=proposed）。
func (k *kit) proposals() int {
	k.t.Helper()
	var out struct {
		Items []json.RawMessage `json:"items"`
	}
	k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "item", "list", "--phase", "proposed").Stdout), &out))
	return len(out.Items)
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

// sourceUpdates 是账本里 OBD_SRC 的 source.updated 事件条数。
func (k *kit) sourceUpdates() int {
	n := 0
	for _, e := range k.audit("source.updated") {
		if e.Object == obdSrc {
			n++
		}
	}
	return n
}

// captureOutbound 读项目的 capture_outbound（nil 为未选择）。
func (k *kit) captureOutbound(project string) *string {
	k.t.Helper()
	var v struct {
		Value *string `json:"value"`
	}
	k.must(json.Unmarshal([]byte(k.webOK("GET", "/v1/settings/projects/"+project+"/capture-outbound", "", 200)), &v))
	return v.Value
}

// requeuedBySystem 报告事件流里是否有一条操作者为 system、针对该来源事件的 inbound.requeued。
func (k *kit) requeuedBySystem(srcEvent string) bool {
	for _, e := range k.audit("inbound.requeued") {
		if e.Object == srcEvent && strings.Contains(string(e.Actor), "system") {
			return true
		}
	}
	return false
}

// installRSS 构建随包的 RSS 拉取程序（验收文档 ACT-OBD-11 "附带的 RSS 拉取程序"）并放到内核 PATH 的最前面：
// pull 来源的 command 是随包程序名 rss-source，由内核按 PATH 找到。需要重启内核才能让新 PATH 生效。
func (k *kit) installRSS() {
	k.t.Helper()
	dir := filepath.Join(k.Root, "bundled")
	k.must(os.MkdirAll(dir, 0o755))
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "rss-source"), "github.com/xiaohei-info/awesome-personal-agent/sources/rss/cmd/rss-source")
	if b, err := cmd.CombinedOutput(); err != nil {
		k.t.Fatalf("go build rss-source: %v\n%s", err, b)
	}
	// 内核与 apa doctor（在本机读数据目录）都按 PATH 找随包程序，所以放进整个测试进程的 PATH。
	k.top.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	k.StopKernel()
	k.StartKernelAgain()
}

type signal struct {
	OK       bool            `json:"ok"`
	Severity string          `json:"severity"`
	Next     string          `json:"next"`
	Detail   json.RawMessage `json:"detail"` // 各信号的 detail 形状不同
}

// outboundUnset 是 unjudged_backlog.detail.outbound_unset。
func (s signal) outboundUnset() int {
	var d struct {
		N int `json:"outbound_unset"`
	}
	_ = json.Unmarshal(s.Detail, &d)
	return d.N
}

func healthSignal(t *testing.T, raw, id string) signal {
	t.Helper()
	var h struct {
		Signals []struct {
			ID string `json:"id"`
			signal
		} `json:"signals"`
	}
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatalf("health 不是 JSON: %q", raw)
	}
	for _, s := range h.Signals {
		if s.ID == id {
			return s.signal
		}
	}
	t.Fatalf("health 没有 %s 信号: %s", id, raw)
	return signal{}
}

func healthHealthy(t *testing.T, raw string) bool {
	t.Helper()
	var h struct {
		Healthy bool `json:"healthy"`
	}
	if err := json.Unmarshal([]byte(raw), &h); err != nil {
		t.Fatal(err)
	}
	return h.Healthy
}

type doctorDetail struct {
	Subject string `json:"subject"`
	Status  string `json:"status"`
	Why     string `json:"why"`
}

// doctorSource 运行 `apa doctor --json` 并返回来源检查里点名该来源的那一条。
func (k *kit) doctorSource(id string) doctorDetail {
	k.t.Helper()
	r := k.Apa("", "", "doctor", "--json")
	var rep struct {
		Checks []struct {
			Name    string         `json:"name"`
			Details []doctorDetail `json:"details"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &rep); err != nil {
		k.t.Fatalf("apa doctor --json 退出码 %d，输出不是 JSON: %q %s", r.Code, r.Stdout, r.Stderr)
	}
	for _, c := range rep.Checks {
		for _, d := range c.Details {
			if d.Subject == id {
				return d
			}
		}
	}
	k.t.Fatalf("doctor 没有点名来源 %s: %s", id, r.Stdout)
	return doctorDetail{}
}

// ---- 对话捕获 ----

// capture 以 WRK 提交一段对话增量（POST /v1/capture）。
func (k *kit) capture(project, text string) {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{"text": text, "project": project})
	r := k.APIRaw("POST", "/v1/capture", string(body), k.WRK)
	if r.Code != 200 || !strings.Contains(r.Body, `"captured":true`) {
		k.t.Fatalf("POST /v1/capture: %d %s", r.Code, r.Body)
	}
}

// extractRequests 是 MODEL_STUB 收到的、提到 mark 的抽取请求（triage-extract，生成端点）。
func (k *kit) extractRequests(mark string) []string { return k.draftRequests(mark) }

// ---- 账本夹具 ----

// serveFixture 用待测二进制在夹具副本上启动另一个 `apa serve`（本机空闲端口），返回根地址。
func (k *kit) serveFixture(home string) string {
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
	cmd := exec.Command(k.Bin["apa"], "serve")
	cmd.Env = append(os.Environ(), "APA_HOME="+home)
	out, _ := cmd.StdoutPipe()
	var errb bytes.Buffer
	cmd.Stderr = &errb
	k.must(cmd.Start())
	k.t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
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
	return "http://" + addr
}

func (k *kit) fixtureGet(base, path, token string) string {
	k.t.Helper()
	req, _ := http.NewRequest("GET", base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	k.must(err)
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		k.t.Fatalf("GET %s: %d %s", path, resp.StatusCode, b)
	}
	return string(b)
}
