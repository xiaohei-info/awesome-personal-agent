//go:build e2e

package briefwb

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
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

type fx struct {
	*baseline.Env
	t       *testing.T
	stubs   map[string]string // 来源 id → 该来源专用的 SRC_STUB 所在目录
	seenRun map[string]bool   // 已读过的 STUB_HARNESS 保存目录
	mode    string            // 简报替身的模式文件
	offset  int64             // 测试时钟已推进的秒数（内核时间 = 真实时间 + offset）
}

// advance 推进测试时钟并记账，fixture 的"今天"以内核时间为准。
func (f *fx) advance(sec int64) {
	f.TCAdvance(sec)
	f.offset += sec
	f.WebLogin() // 推进得久了，登录会话已过期
}

// now 是内核此刻的时间。
func (f *fx) now() time.Time { return time.Now().Add(time.Duration(f.offset) * time.Second) }

// at 是内核本地日期 + days 天的 hour 点整。
func (f *fx) at(days, hour int) time.Time {
	y, m, d := f.now().Date()
	return time.Date(y, m, d+days, hour, 0, 0, 0, time.Local)
}

func (f *fx) step(name string, fn func(t *testing.T)) {
	f.t.Helper()
	parent := f.t
	ok := parent.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续动作不得开始", name)
	}
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) web(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) until(what string, cond func() bool) {
	f.t.Helper()
	if !f.WaitFor(waitMax, cond) {
		f.t.Fatalf("%v 内没有等到：%s\n%s", waitMax, what, f.ServiceLog())
	}
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%.500s", err, s)
	}
	return v
}

func idOf(t *testing.T, s string) string {
	t.Helper()
	return decode[struct {
		ID string `json:"id"`
	}](t, s).ID
}

// ---- 环境：默认 Agent 是读资料文件并写回简报的 STUB_HARNESS ----

const briefHarness = "acc-brief-stub"

func setup(t *testing.T) *fx {
	t.Helper()
	srcStubBin(t)
	e := baseline.Setup(t)
	f := &fx{Env: e, t: t, stubs: map[string]string{}, seenRun: map[string]bool{}}
	f.declareBriefHarness()
	// 出厂的默认简报实例（工作日 08:30）停用，免得测试恰好跑在到期时刻。
	for _, in := range decode[struct {
		Instances []struct {
			ID string `json:"id"`
		} `json:"instances"`
	}](t, f.apa(f.OBS, 0, "instance", "list").Stdout).Instances {
		f.apa(f.DEL, 0, "instance", "disable", in.ID)
	}
	return f
}

// declareBriefHarness 声明 acc-brief-stub：包装脚本按模式文件选 STUB_HARNESS 的 brief 或 brief-period 模式
// （运行子进程不继承内核环境，开关只能走脚本与文件），并设为 Web 里的默认 Agent。
func (f *fx) declareBriefHarness() {
	f.mode = filepath.Join(f.Root, "brief-mode")
	f.setMode("brief")
	wrapper := filepath.Join(f.Root, "acc-brief-harness.sh")
	script := fmt.Sprintf("#!/bin/sh\nexport STUB_HARNESS_MODE=$(cat %q)\nexport STUB_HARNESS_APA=%q\nexec %q \"$@\"\n", f.mode, f.Bin["apa"], f.Bin["stub-harness"])
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	decl, _ := json.Marshal(map[string]any{"harness": briefHarness, "headless": map[string]any{
		"command": []string{wrapper, "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := f.Apa(f.DEL, string(decl), "declare", "-"); r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.web("POST", "/v1/settings/declarations/"+briefHarness+"/1/confirm", "", 200)
	f.web("PUT", "/v1/settings/policy", `{"default_harness":"`+briefHarness+`"}`, 200)
}

func (f *fx) setMode(m string) {
	f.t.Helper()
	if err := os.WriteFile(f.mode, []byte(m), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// writePack 把用户能力包写进 APA_HOME/packs（内核热加载），等它出现在 GET /v1/packs。
func (f *fx) writePack(name, body string) {
	f.t.Helper()
	dir := filepath.Join(f.Home, "packs", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "pack.yaml"), []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.until("能力包 "+name+" 被加载", func() bool {
		return strings.Contains(f.APIRaw("GET", "/v1/packs", "", f.OBS).Body, `"`+name+`"`)
	})
}

// ---- SRC_STUB 来源：照给定的 JSONL 输出事件 ----

// addSource 在 Web 登记一个运行 SRC_STUB 的脚本来源（周期 1 秒）。external 为空表示外发范围保持"未选择"。
// 每个来源有自己的目录与 case 文件：来源子进程环境是白名单，SRC_CASE 走可执行文件同目录的 case 文件。
func (f *fx) addSource(id, name, sourceType, external string, lines []map[string]any) {
	f.t.Helper()
	dir := filepath.Join(f.Root, "stubs", id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(stubDir, "src-stub"))
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "src-stub"), raw, 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.stubs[id] = dir
	f.setLines(id, "c1", lines)
	body, _ := json.Marshal(map[string]any{"id": id, "name": name, "type": "script", "source_type": sourceType,
		"command": filepath.Join(dir, "src-stub"), "schedule": "1s", "timeout": 20})
	f.web("POST", "/v1/sources", string(body), 201)
	ext := "null"
	if external != "" {
		ext = `"` + external + `"`
	}
	f.web("PUT", "/v1/settings/sources/"+id+"/outbound", `{"local":"full","external":`+ext+`}`, 200)
}

// setLines 换来源的输出（下一次拉取生效）；已落盘的事件按 id 去重，不重复入账。
func (f *fx) setLines(id, cursor string, lines []map[string]any) {
	f.t.Helper()
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			f.t.Fatal(err)
		}
	}
	_ = enc.Encode(map[string]string{"cursor": cursor})
	path := filepath.Join(f.stubs[id], "custom-"+cursor+".jsonl")
	if err := os.WriteFile(path, bytes.TrimRight(b.Bytes(), "\n"), 0o644); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.stubs[id], "case"), []byte(path), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

// waitEvents 等某来源有 n 条事件，且处置都已是 disposition（规则处理是异步的）。
func (f *fx) waitEvents(id string, n int, disposition string) {
	f.t.Helper()
	f.until(fmt.Sprintf("来源 %s 落盘 %d 条事件且处置为 %s", id, n, disposition), func() bool {
		r := f.APIRaw("GET", "/v1/sources/"+id+"/events?limit=500", "", f.OBS)
		var out struct {
			Events []struct {
				Disposition string `json:"disposition"`
			} `json:"events"`
		}
		if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil || len(out.Events) < n {
			return false
		}
		got := 0
		for _, e := range out.Events {
			if e.Disposition == disposition {
				got++
			}
		}
		return got >= n
	})
}

// ---- 事项 ----

type item struct {
	ID      string          `json:"id"`
	Title   string          `json:"title"`
	Phase   string          `json:"phase"`
	Project string          `json:"project"`
	Spec    json.RawMessage `json:"spec"`
	Origin  *struct {
		Source string `json:"source"`
	} `json:"origin"`
	Ask *struct {
		Question string `json:"question"`
	} `json:"ask"`
}

func (f *fx) addItem(args ...string) string {
	f.t.Helper()
	return idOf(f.t, f.apa(f.DEL, 0, append([]string{"item", "add", "--kind", "task"}, args...)...).Stdout)
}

func (f *fx) show(id string) item {
	f.t.Helper()
	return decode[struct {
		Item item `json:"item"`
	}](f.t, f.apa(f.OBS, 0, "item", "show", id, "--full").Stdout).Item
}

func (f *fx) briefs() []item {
	f.t.Helper()
	return decode[struct {
		Items []item `json:"items"`
	}](f.t, f.apa(f.OBS, 0, "item", "list", "--kind", "brief", "--full").Stdout).Items
}

// ---- 实例与运行 ----

// addInstance 建一个简报实例（schedule 取永不到期的年度时刻，只靠"立即跑一次"触发）。
func (f *fx) addInstance(want int, params ...string) string {
	f.t.Helper()
	args := []string{"instance", "add", "brief", "--param", "schedule=0 3 1 1 *"}
	for _, p := range params {
		args = append(args, "--param", p)
	}
	r := f.apa(f.DEL, want, args...)
	if want != 0 {
		return ""
	}
	return idOf(f.t, r.Stdout)
}

// runResult 是一次简报运行：简报事项与 STUB_HARNESS 保存下来的运行目录文件（资料文件、指令文件、原文文件）。
type runResult struct {
	Item  item
	Dir   string            // 保存目录；运行目录是它的上一层
	Mats  map[string]string // 段名 → 该段的 JSON（materials.json 里每段一份）
	Files map[string]string // 运行目录里的其余文件（instruction.txt、source-event-NNN.txt）
}

// runBrief 对实例点"立即跑一次"（POST /instances/{id}/run），等简报事项离开 open，读 STUB_HARNESS 保存的资料。
func (f *fx) runBrief(inst string, wantPhase string) runResult {
	f.t.Helper()
	known := map[string]bool{}
	for _, b := range f.briefs() {
		known[b.ID] = true
	}
	f.TCAdvance(120)                                                         // 每次到期时刻不同：同一周期只一份（幂等键取实例与到期时刻）
	if r := f.Web("POST", "/v1/instances/"+inst+"/run", ""); r.Code != 202 { // Web 简报页的"立即跑一次"，本人登录会话
		f.t.Fatalf("立即跑一次: HTTP %d %s", r.Code, r.Body)
	}
	var got item
	f.until("实例 "+inst+" 的简报到达 "+wantPhase, func() bool {
		for _, b := range f.briefs() {
			if !known[b.ID] && b.Origin != nil && b.Origin.Source == "instance:"+inst && b.Phase == wantPhase {
				got = f.show(b.ID)
				return true
			}
		}
		return false
	})
	res := runResult{Item: got, Mats: map[string]string{}, Files: map[string]string{}}
	f.until("STUB_HARNESS 保存了运行目录", func() bool {
		dirs, _ := filepath.Glob(filepath.Join(f.Home, "runs", "*", "stub-harness-saved"))
		for _, d := range dirs {
			if f.seenRun[d] {
				continue
			}
			if raw, err := os.ReadFile(filepath.Join(d, "materials.json")); err == nil {
				f.seenRun[d] = true
				res.Dir = d
				var m struct {
					Materials []struct{ Name, Body string }
				}
				if err := json.Unmarshal(raw, &m); err != nil {
					f.t.Fatalf("materials.json: %v", err)
				}
				for _, x := range m.Materials {
					res.Mats[x.Name] = x.Body
				}
				return true
			}
		}
		return false
	})
	ents, _ := os.ReadDir(res.Dir)
	for _, e := range ents {
		if e.Name() == "materials.json" {
			continue
		}
		// 原文文件在运行目录里是只读的（ACT-BRF-05）；instruction.txt 内核写成 0600，不在验收的只读范围内，这里不断言。
		if strings.HasPrefix(e.Name(), "source-event-") {
			st, err := os.Stat(filepath.Join(filepath.Dir(res.Dir), e.Name()))
			if err != nil || st.Mode().Perm()&0o222 != 0 {
				f.t.Errorf("运行目录里的原文文件 %s 应只读: %v %v", e.Name(), st, err)
			}
		}
		b, err := os.ReadFile(filepath.Join(res.Dir, e.Name()))
		if err != nil {
			f.t.Fatal(err)
		}
		res.Files[e.Name()] = string(b)
	}
	return res
}

// ---- 固定账本夹具里"导入"的那一批 ----

// seedImported 向已停下的账本直接写两条带导入批次标记的事件：一条给已完成事项的进展，一条把开着的事项记为完成。
// 导入命令（I3）不在本节点的上游里；"导入事件不进当天活动"的断言只需要账本里确有这样的事件，写法与内核单测一致（eventlog.Append）。
func (f *fx) seedImported(doneItem, openItem string) {
	f.t.Helper()
	f.StopKernel()
	reg, err := ledgerfixture.RegistryAt(1 << 30)
	if err != nil {
		f.t.Fatal(err)
	}
	st, err := store.Open(f.Home, reg)
	if err != nil {
		f.t.Fatal(err)
	}
	now := f.now()
	actor := domain.Actor{Type: domain.ActorSystem, Principal: "kernel:import", Via: domain.ViaSource, OnBehalfOf: "owner"}
	err = st.Update(context.Background(), func(tx *store.Tx) error {
		prog, _ := json.Marshal(map[string]any{"progress": domain.Progress{ID: "prg_imported", Text: importedNote, At: now.Add(-time.Hour)}})
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemProgressAdded, Object: doneItem, Actor: actor, Reason: "import",
			Payload: prog, At: now.Add(-time.Hour), Imported: "imp_acc", RecordedAt: now}); err != nil {
			return err
		}
		ph, _ := json.Marshal(map[string]any{"item": domain.Item{ID: openItem, Phase: domain.PhaseDone}})
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemPhaseChanged, Object: openItem, Actor: actor, Reason: "import",
			Payload: ph, At: now.Add(-2 * time.Hour), Imported: "imp_acc", RecordedAt: now})
		return err
	})
	if err != nil {
		st.Close()
		f.t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		f.t.Fatal(err)
	}
	f.StartKernelAgain()
}
