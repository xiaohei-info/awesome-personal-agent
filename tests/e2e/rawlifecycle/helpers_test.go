//go:build e2e

package rawlifecycle

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const waitMax = 40 * time.Second

// 退出码（附录 B.1）。
const exitDenied = 4

var (
	stubOnce sync.Once
	stubDir  string
	stubErr  error
)

// srcStubBin 每个测试进程只构建一次 SRC_STUB，返回可执行文件路径。
func srcStubBin(t *testing.T) string {
	t.Helper()
	stubOnce.Do(func() {
		stubDir, stubErr = os.MkdirTemp("", "apa-e2e-srcstub-")
		if stubErr != nil {
			return
		}
		cmd := exec.Command("go", "build", "-o", filepath.Join(stubDir, "src-stub"), "./tests/doubles/srcstub/cmd/src-stub")
		cmd.Dir = filepath.Join("..", "..", "..")
		if out, err := cmd.CombinedOutput(); err != nil {
			stubErr = fmt.Errorf("go build src-stub: %v\n%s", err, out)
		}
	})
	if stubErr != nil {
		t.Fatal(stubErr)
	}
	return filepath.Join(stubDir, "src-stub")
}

type kit struct {
	*baseline.Env
	t     *testing.T
	stubs map[string]string // 来源 id → 该来源专用的 SRC_STUB 所在目录
}

func newKit(t *testing.T, e *baseline.Env) *kit {
	return &kit{Env: e, t: t, stubs: map[string]string{}}
}

// setup 建一套基线环境：init、serve、凭据、项目、对话端点与 STUB_HARNESS 声明（baseline.Setup）。
func setup(t *testing.T) *kit {
	t.Helper()
	srcStubBin(t)
	return newKit(t, baseline.Setup(t))
}

// step 运行一个验收动作；失败后后续动作不得开始。
func (k *kit) step(name string, f func(t *testing.T)) {
	k.t.Helper()
	parent := k.t
	ok := parent.Run(name, func(t *testing.T) {
		k.t, k.Env.T = t, t
		defer func() { k.t, k.Env.T = parent, parent }()
		f(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续动作不得开始", name)
	}
}

func (k *kit) apa(token string, want int, args ...string) baseline.Result {
	k.t.Helper()
	r := k.Apa(token, "", args...)
	if r.Code != want {
		k.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (k *kit) web(method, path, body string, want int) string {
	k.t.Helper()
	r := k.Web(method, path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (k *kit) until(what string, cond func() bool) {
	k.t.Helper()
	if !k.WaitFor(waitMax, cond) {
		k.t.Fatalf("%v 内没有等到：%s", waitMax, what)
	}
}

// ---- SRC_STUB 来源 ----

// addStub 在 Web 登记一个运行 SRC_STUB 的脚本来源（周期 1 秒，登记允许的最小周期），并设外发范围。
// 每个来源有自己的目录与 case 文件：来源子进程环境是白名单，SRC_CASE 走可执行文件同目录的 case 文件（SRC_STUB README）。
// external 为空表示不设置（保持"未选择"）。
func (k *kit) addStub(id, sourceType, local, external string) {
	k.addProgram(id, "src-stub", sourceType, local, external)
}

// addProgram 同 addStub，程序文件名由 prog 给：接入声明按程序名找，"src-stub" 声明了支持取回（测试构建），
// "src-stub-nofetch"（同一份脚本换个名字）没有声明，即 acc-raw-nofetch：附件只有元数据。
func (k *kit) addProgram(id, prog, sourceType, local, external string) {
	k.t.Helper()
	dir := filepath.Join(k.Root, "stubs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		k.t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(stubDir, "src-stub"))
	if err != nil {
		k.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, prog), raw, 0o755); err != nil {
		k.t.Fatal(err)
	}
	k.stubs[id] = dir
	k.setCase(id, "mail-like")
	body, _ := json.Marshal(map[string]any{"id": id, "name": id, "type": "script", "source_type": sourceType,
		"command": filepath.Join(dir, prog), "schedule": "1s", "timeout": 20})
	k.web("POST", "/v1/sources", string(body), 201)
	ext := "null"
	if external != "" {
		ext = `"` + external + `"`
	}
	k.web("PUT", "/v1/settings/sources/"+id+"/outbound", `{"local":"`+local+`","external":`+ext+`}`, 200)
}

// setCase 换 SRC_STUB 的 case（case 名；下一次拉取生效）。
func (k *kit) setCase(id, name string) {
	k.t.Helper()
	if err := os.WriteFile(filepath.Join(k.stubs[id], "case"), []byte(name), 0o644); err != nil {
		k.t.Fatal(err)
	}
}

// setJSONL 让 SRC_STUB 原样输出执行者给的 JSONL（case 文件指向一个以 `{` 开头的文件，末行缺游标时由替身补）。
func (k *kit) setJSONL(id string, lines []map[string]any, cursor string) {
	k.t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			k.t.Fatal(err)
		}
	}
	_ = enc.Encode(map[string]string{"cursor": cursor})
	path := filepath.Join(k.stubs[id], "custom-"+cursor+".jsonl")
	if err := os.WriteFile(path, bytes.TrimRight(b.Bytes(), "\n"), 0o644); err != nil {
		k.t.Fatal(err)
	}
	k.setCase(id, path)
}

// stubOut 直接运行 SRC_STUB 取某 case 的标准输出（事件行，不含游标行）：验收里"SRC_STUB 的输出"，作为断言的基准。
func (k *kit) stubOut(name string) []map[string]any {
	k.t.Helper()
	cmd := exec.Command(filepath.Join(stubDir, "src-stub"))
	cmd.Env = append(os.Environ(), "SRC_CASE="+name)
	cmd.Stdin = strings.NewReader("{}")
	out, err := cmd.Output()
	if err != nil {
		k.t.Fatalf("src-stub %s: %v", name, err)
	}
	var evs []map[string]any
	for _, ln := range bytes.Split(bytes.TrimSpace(out), []byte("\n")) {
		var m map[string]any
		if err := json.Unmarshal(ln, &m); err != nil {
			k.t.Fatalf("src-stub 输出不是 JSONL: %v", err)
		}
		if _, ok := m["cursor"]; !ok {
			evs = append(evs, m)
		}
	}
	return evs
}

// ---- 读取 ----

type listed struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	Subject     string `json:"subject"`
	Detail      string `json:"detail"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	DedupeKey   struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Content content `json:"content"`
}

type content struct {
	State string `json:"state"`
	Bytes int    `json:"bytes"`
	Parts []struct {
		Part      string `json:"part"`
		MediaType string `json:"media_type"`
		Bytes     int    `json:"bytes"`
		SHA256    string `json:"sha256"`
	} `json:"parts"`
	Missing []struct {
		Part   string `json:"part"`
		Reason string `json:"reason"`
	} `json:"missing"`
}

func (c content) part(name string) (bytes int, sha string, ok bool) {
	for _, p := range c.Parts {
		if p.Part == name {
			return p.Bytes, p.SHA256, true
		}
	}
	return 0, "", false
}

// events 经 OBS 读某来源已落盘的事件（来源页同一接口，列表口径）。
func (k *kit) events(id string) []listed {
	k.t.Helper()
	r := k.APIRaw("GET", "/v1/sources/"+id+"/events?limit=500", "", k.OBS)
	if r.Code != 200 {
		k.t.Fatalf("读来源事件 %s: HTTP %d %s", id, r.Code, r.Body)
	}
	var out struct {
		Events []listed `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Events
}

// eventByStubID 等某来源里出现 SRC_STUB 给的事件 id（dedupe_key.event_id）并返回它。
func (k *kit) eventByStubID(src, stubID string) listed {
	k.t.Helper()
	var got listed
	k.until(src+" 出现事件 "+stubID, func() bool {
		for _, e := range k.events(src) {
			if e.DedupeKey.EventID == stubID {
				got = e
				return true
			}
		}
		return false
	})
	return got
}

type read struct {
	Event struct {
		ID      string `json:"id"`
		Subject string `json:"subject"`
		Summary string `json:"summary"`
		Detail  string `json:"detail"`
	} `json:"event"`
	Content  content `json:"content"`
	Body     *string `json:"body"`
	Original *struct {
		MediaType string `json:"media_type"`
		Text      string `json:"text"`
	} `json:"original"`
	Headers     json.RawMessage `json:"headers"`
	Attachments json.RawMessage `json:"attachments"`
	Messages    json.RawMessage `json:"messages"`
	Scope       string          `json:"scope"`
	Trimmed     *struct {
		TotalChars    int `json:"total_chars"`
		ReturnedChars int `json:"returned_chars"`
	} `json:"trimmed"`
}

// readEvent 以 token 读单条来源事件（token 为空时用 Web 登录），期望 HTTP 状态 want。
func (k *kit) readEvent(token, id, part string, want int) read {
	k.t.Helper()
	path := "/v1/source-events/" + id
	if part != "" {
		path += "?part=" + part
	}
	var r baseline.Resp
	if token == "" {
		r = k.Web("GET", path, "")
	} else {
		r = k.APIRaw("GET", path, "", token)
	}
	if r.Code != want {
		k.t.Fatalf("GET %s: HTTP %d（期望 %d）%.300s", path, r.Code, want, r.Body)
	}
	var out read
	if want == 200 {
		if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
			k.t.Fatalf("GET %s 不是 JSON: %v", path, err)
		}
	}
	return out
}

type rejectRow struct {
	Reason string `json:"reason"`
	Raw    string `json:"raw"`
	Line   int    `json:"line"`
}

func (k *kit) rejects(id string) []rejectRow {
	k.t.Helper()
	var out struct {
		Rejects []rejectRow `json:"rejects"`
	}
	r := k.apa(k.OBS, 0, "source", "rejects", id)
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		k.t.Fatalf("source rejects 输出不是 JSON: %q", r.Stdout)
	}
	return out.Rejects
}

type srcShow struct {
	ID     string `json:"id"`
	Cursor string `json:"cursor"`
	Stats  struct {
		Runs                int    `json:"runs"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		LastError           string `json:"last_error"`
		Events              int    `json:"events"`
		ContentBytes        int64  `json:"content_bytes"`
	} `json:"stats"`
}

func (k *kit) show(id string) srcShow {
	k.t.Helper()
	var s srcShow
	r := k.apa(k.OBS, 0, "source", "show", id)
	if err := json.Unmarshal([]byte(r.Stdout), &s); err != nil {
		k.t.Fatalf("source show 输出不是 JSON: %q", r.Stdout)
	}
	return s
}

// settle 等来源至少再跑 n 次（无论成败），用于断言"某件事没有发生"之前给调度足够的节拍。
func (k *kit) settle(id string, n int) srcShow {
	k.t.Helper()
	start := k.show(id).Stats.Runs
	k.until(fmt.Sprintf("%s 再运行 %d 次", id, n), func() bool { return k.show(id).Stats.Runs >= start+n })
	return k.show(id)
}

// setConfig 重写运行配置（保留本机监听地址，附加 extra）并重启 apa serve。extra 为空即恢复缺省。
func (k *kit) setConfig(extra string) {
	k.t.Helper()
	_, port, err := net.SplitHostPort(strings.TrimPrefix(k.URL, "http://"))
	if err != nil {
		k.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(k.Home, "config.yaml"), []byte("listen: 127.0.0.1:"+port+"\n"+extra), 0o644); err != nil {
		k.t.Fatal(err)
	}
	k.StopKernel()
	k.StartKernelAgain()
}

func sha(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func jsonEq(t *testing.T, what string, got json.RawMessage, want any) {
	t.Helper()
	w, _ := json.Marshal(want)
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("%s 不是 JSON: %q", what, got)
	}
	_ = json.Unmarshal(w, &b)
	ga, _ := json.Marshal(a)
	gb, _ := json.Marshal(b)
	if !bytes.Equal(ga, gb) {
		t.Errorf("%s 与 SRC_STUB 输出不一致:\n got  %s\n want %s", what, ga, gb)
	}
}

func asString(v any) string { s, _ := v.(string); return s }

func eq[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// ---- 本节点新增 ----

// setFetch 设 SRC_STUB 的 fetch 行为（同目录 fetch 文件：ok、fail[:码]、big、size:N）。
func (k *kit) setFetch(id, mode string) {
	k.t.Helper()
	if err := os.WriteFile(filepath.Join(k.stubs[id], "fetch"), []byte(mode), 0o644); err != nil {
		k.t.Fatal(err)
	}
}

// mailLike 是 mail-like case 的那封事件换了事件 id 与关联键的副本，同来源可以放多封互不相干的邮件。
func (k *kit) mailLike(id string) map[string]any {
	k.t.Helper()
	m := k.stubOut("mail-like")[0]
	m["id"] = id
	m["subject"] = "Subject " + id
	m["correlation_keys"] = []string{"<" + id + "@example.com>"}
	return m
}

// attachRef 是 SRC_STUB 事件第 n 个附件的取回句柄。
func attachRef(m map[string]any, n int) string {
	return asString(m["attachments"].([]any)[n].(map[string]any)["ref"])
}

// stubBytes 直接运行 SRC_STUB 的 fetch 模式取某 ref 的固定字节：断言的基准，不经内核。
func (k *kit) stubBytes(ref string) []byte {
	k.t.Helper()
	cmd := exec.Command(filepath.Join(stubDir, "src-stub"), "fetch")
	cmd.Env = append(os.Environ(), "SRC_FETCH=ok")
	cmd.Stdin = strings.NewReader(`{"attachment":{"ref":"` + ref + `"}}`)
	out, err := cmd.Output()
	if err != nil || len(out) == 0 {
		k.t.Fatalf("src-stub fetch %s: %v", ref, err)
	}
	return out
}

type auditEvent struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Object  string          `json:"object"`
	Payload json.RawMessage `json:"payload"`
}

// audit 读内部审计事件（GET /v1/audit/events，Web 登录），可按类型与对象筛选，按先后排列。
func (k *kit) audit(typ, object string) []auditEvent {
	k.t.Helper()
	q := "?limit=500"
	if typ != "" {
		q += "&type=" + typ
	}
	if object != "" {
		q += "&object=" + object
	}
	var out struct {
		Events []auditEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(k.web("GET", "/v1/audit/events"+q, "", 200)), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Events
}

// blobFile 是附件 blob（apa://blob/<哈希>）在数据目录里的文件路径。
func (k *kit) blobFile(ref string) string {
	return filepath.Join(k.Home, "blobs", strings.TrimPrefix(ref, "apa://blob/"))
}

func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// manifestItem 是取回响应与清单里的一个附件项。
type manifestItem struct {
	Name string `json:"name"`
	Ref  string `json:"ref"`
	Blob string `json:"blob"`
	Size int    `json:"size"`
}

// attachments 读事件的附件清单（Web 登录）。
func (k *kit) attachments(id string) []manifestItem {
	k.t.Helper()
	var out []manifestItem
	if err := json.Unmarshal(k.readEvent("", id, "", 200).Attachments, &out); err != nil {
		k.t.Fatal(err)
	}
	return out
}

// fetch 经 Web 登录取回第 n 个附件，返回 HTTP 状态与响应体。
func (k *kit) fetch(id string, n int) (int, string) {
	k.t.Helper()
	r := k.Web("POST", "/v1/source-events/"+id+"/attachments/"+strconv.Itoa(n)+"/fetch", "")
	return r.Code, r.Body
}

// waitBatch 等来源处理完游标为 cursor 的那一批（来源每秒拉一次，case 文件换了新游标）。
func (k *kit) waitBatch(src, cursor string) {
	k.t.Helper()
	k.until(src+" 处理到游标 "+cursor, func() bool { return k.show(src).Cursor == cursor })
}
