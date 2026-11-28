//go:build e2e

package materialbudget

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

const waitMax = 60 * time.Second

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
	t      *testing.T
	stubs  map[string]string // 来源 id → 该来源专用的 SRC_STUB 所在目录
	clsOff int
	modOff int
}

// setup 建一套基线环境：init、serve、凭据、项目，判断端点接 CLS_STUB、生成端点接 MODEL_STUB（都是 local）。
func setup(t *testing.T) *kit {
	t.Helper()
	srcStubBin(t)
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	e.CreateProjects()
	e.WireEndpoints()
	e.BatchAgent("") // 过了 judge 的事件由批处理替身逐条转为建议
	return &kit{Env: e, t: t, stubs: map[string]string{}}
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
		k.t.Fatalf("%v 内没有等到：%s\n%s", waitMax, what, tail(k.ServiceLog(), 3000))
	}
}

func tail(s string, n int) string {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}

// ---- 端点设置 ----

type endpoints struct {
	Generate struct {
		MaxMaterialTokens int    `json:"max_material_tokens"`
		Locality          string `json:"locality"`
	} `json:"generate_endpoint"`
	Judge struct {
		MaxMaterialChars int `json:"max_material_chars"`
	} `json:"judge_endpoint"`
}

func (k *kit) endpoints(token string) (int, endpoints) {
	k.t.Helper()
	var r baseline.Resp
	if token == "" {
		r = k.Web("GET", "/v1/settings/model-endpoints", "")
	} else {
		r = k.APIRaw("GET", "/v1/settings/model-endpoints", "", token)
	}
	var out endpoints
	if r.Code == 200 {
		if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
			k.t.Fatalf("model-endpoints 不是 JSON: %v %s", err, r.Body)
		}
	}
	return r.Code, out
}

// putEndpoints 在 Web 设置页写两个端点（与页面保存同一接口）：generate 的位置与资料预算由参数给出，judge 一直是 CLS_STUB（local）。
func (k *kit) putEndpoints(locality string, budget int) {
	k.t.Helper()
	gen := map[string]any{"url": k.ModelStub.URL(), "model": "acc-model", "locality": locality}
	if budget > 0 {
		gen["max_material_tokens"] = budget
	}
	body, _ := json.Marshal(map[string]any{
		"judge_endpoint":    map[string]any{"url": k.Cls.URL(), "protocol": "probability-judgment/1", "locality": "local"},
		"generate_endpoint": gen,
	})
	k.web("PUT", "/v1/settings/model-endpoints", string(body), 200)
}

// ---- SRC_STUB 来源 ----

// addStub 在 Web 登记一个运行 SRC_STUB 的脚本来源（周期 1 秒），并设外发范围。每个来源有自己的目录与 case 文件。
func (k *kit) addStub(id, sourceType, local, external string) {
	k.t.Helper()
	dir := filepath.Join(k.Root, "stubs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		k.t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(stubDir, "src-stub"))
	if err != nil {
		k.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src-stub"), raw, 0o755); err != nil {
		k.t.Fatal(err)
	}
	k.stubs[id] = dir
	k.setCase(id, "none-yet")
	body, _ := json.Marshal(map[string]any{"id": id, "name": id, "type": "script", "source_type": sourceType,
		"command": filepath.Join(dir, "src-stub"), "schedule": "1s", "timeout": 20})
	k.web("POST", "/v1/sources", string(body), 201)
	k.web("PUT", "/v1/settings/sources/"+id+"/outbound", `{"local":"`+local+`","external":"`+external+`"}`, 200)
}

// setCase 换 SRC_STUB 的 case（case 名；下一次拉取生效）。
func (k *kit) setCase(id, name string) {
	k.t.Helper()
	if err := os.WriteFile(filepath.Join(k.stubs[id], "case"), []byte(name), 0o644); err != nil {
		k.t.Fatal(err)
	}
}

// setJSONL 让 SRC_STUB 原样输出执行者给的事件（case 文件指向一个以 `{` 开头的 JSONL 文件，末行游标由替身补）。
func (k *kit) setJSONL(id, name string, evs ...map[string]any) {
	k.t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, e := range evs {
		if err := enc.Encode(e); err != nil {
			k.t.Fatal(err)
		}
	}
	_ = enc.Encode(map[string]string{"cursor": name})
	path := filepath.Join(k.stubs[id], name+".jsonl")
	if err := os.WriteFile(path, bytes.TrimRight(b.Bytes(), "\n"), 0o644); err != nil {
		k.t.Fatal(err)
	}
	k.setCase(id, path)
}

// stubOut 直接运行 SRC_STUB 取某 case 的事件（不含游标行）：验收里"SRC_STUB 的输出"，作为断言的基准。
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
	for _, ln := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(ln), &m); err != nil {
			k.t.Fatalf("src-stub 输出不是 JSONL: %v", err)
		}
		if _, ok := m["cursor"]; !ok {
			evs = append(evs, m)
		}
	}
	return evs
}

// ---- 事件与事项 ----

type srcEvent struct {
	ID          string `json:"id"`
	Subject     string `json:"subject"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	Judgment    string `json:"judgment"`
	DedupeKey   struct {
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
}

func (k *kit) events(src string) []srcEvent {
	k.t.Helper()
	r := k.APIRaw("GET", "/v1/sources/"+src+"/events?limit=500", "", k.OBS)
	if r.Code != 200 {
		k.t.Fatalf("读来源事件 %s: HTTP %d %s", src, r.Code, r.Body)
	}
	var out struct {
		Events []srcEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Events
}

// disposed 等来源里出现 SRC_STUB 给的事件 id 且已有处置（不再 pending），返回它。
func (k *kit) disposed(src, stubID string) srcEvent {
	k.t.Helper()
	var got srcEvent
	k.until(src+" 的事件 "+stubID+" 处置完毕", func() bool {
		for _, e := range k.events(src) {
			if e.DedupeKey.EventID == stubID && e.Disposition != "" && e.Disposition != "pending" {
				got = e
				return true
			}
		}
		return false
	})
	return got
}

type itemView struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Summary  string `json:"summary"`
	Body     string `json:"body"`
	Phase    string `json:"phase"`
	Proposal *struct {
		Sources []string `json:"sources"`
	} `json:"proposal"`
}

func (k *kit) item(id string) itemView {
	k.t.Helper()
	var out struct {
		Item itemView `json:"item"`
	}
	if err := json.Unmarshal([]byte(k.apa(k.OBS, 0, "item", "show", id, "--full").Stdout), &out); err != nil {
		k.t.Fatalf("apa item show 输出不是 JSON: %v", err)
	}
	return out.Item
}

// ---- 替身日志 ----

// clear 是验收文档的"清空 $CLS_LOG、$MODEL_LOG"：之后的断言只看清空以后收到的请求。
func (k *kit) clear() {
	k.clsOff = len(k.Cls.Batches())
	k.modOff = len(k.ModelStub.Requests())
}

// plain 把请求体里的 JSON 转义还原成文本（中文、换行在请求体里是转义的）。
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
		case []any:
			for _, e := range t {
				walk(e)
			}
		case map[string]any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return sb.String()
}

// generateAbout 是清空以后 MODEL_STUB 收到的、含 mark 的生成请求（起草、抽取；判断题不算）的还原文本。
func (k *kit) generateAbout(mark string) []string {
	var out []string
	for _, r := range k.ModelStub.Requests()[k.modOff:] {
		if !baseline.IsJudgeRequest(r) { // 送资料给生成端点的只剩对话端点的判断题（摘要请求不算）
			continue
		}
		if p := plain(r.BodyText); strings.Contains(p, mark) {
			out = append(out, p)
		}
	}
	return out
}

func (k *kit) waitGenerate(mark string) string {
	k.t.Helper()
	var got string
	k.until("MODEL_LOG 出现含 "+mark+" 的生成请求", func() bool {
		if g := k.generateAbout(mark); len(g) > 0 {
			got = g[len(g)-1]
			return true
		}
		return false
	})
	return got
}

// escalate 让含 mark 的题落进不确定带，由对话端点重答"是"：送给生成端点的资料就是这道判断题的资料。
func (k *kit) escalate(mark string) {
	k.screenYes(mark, 0.4)
	yes := true
	k.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: mark}, Answer: "yes", Sufficient: &yes})
}

// screenYes 配置 CLS_STUB：题目含 mark 的初筛 p_yes。
func (k *kit) screenYes(mark string, p float64) {
	k.Cls.Judge(modelstub.ClsJudge{Key: modelstub.Key{Contains: mark}, PYes: &p})
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func runes(s string) int { return len([]rune(s)) }

// loadPack 把用户能力包写进 APA_HOME/packs 并重启 apa serve（来源规则在启动时加载，ACT-XAL-01 的做法），等包出现在 GET /v1/packs。
func (k *kit) loadPack(name, body string) {
	k.t.Helper()
	dir := filepath.Join(k.Home, "packs", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		k.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(body), 0o644); err != nil {
		k.t.Fatal(err)
	}
	k.StopKernel()
	k.StartKernelAgain()
	k.until("能力包 "+name+" 被加载", func() bool { return strings.Contains(k.APIRaw("GET", "/v1/packs", "", k.OBS).Body, `"`+name+`"`) })
}
