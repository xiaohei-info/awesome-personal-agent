package baseline

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/tools"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// WaitFor 轮询直到 cond 成立；within 内没有成立返回 false。
func (e *Env) WaitFor(within time.Duration, cond func() bool) bool {
	for end := time.Now().Add(within); time.Now().Before(end); time.Sleep(100 * time.Millisecond) {
		if cond() {
			return true
		}
	}
	return cond()
}

// ---- 两个模型端点替身（ACT-ENV-04、05、09） ----

// StartCls 启动 CLS_STUB（讲 probability-judgment/1 的判断端点替身）。
func (e *Env) StartCls() {
	e.T.Helper()
	e.Cls = modelstub.NewClsAt(loopback + ":0")
}

// IsJudgeRequest 报告 MODEL_STUB 收到的请求是不是判断题（结构化输出 schema 含 sufficient，或要求 logprobs）；否则是生成请求。
func IsJudgeRequest(r modelstub.Request) bool {
	var b struct {
		Logprobs       bool `json:"logprobs"`
		ResponseFormat struct {
			JSONSchema struct {
				Schema struct {
					Properties map[string]json.RawMessage `json:"properties"`
				} `json:"schema"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if json.Unmarshal(r.Body, &b) != nil {
		return false
	}
	_, ok := b.ResponseFormat.JSONSchema.Schema.Properties["sufficient"]
	return ok || b.Logprobs
}

// WireEndpoints 在 Web 设置页把判断端点设为 CLS_STUB（probability-judgment/1，local），生成端点设为 MODEL_STUB（local）。
// 两个替身缺哪个就先启动哪个。需要"只配一个""都不配"的流程自行改设置并在末尾再调用本方法恢复。
func (e *Env) WireEndpoints() {
	e.T.Helper()
	if e.Cls == nil {
		e.StartCls()
	}
	if e.ModelStub == nil {
		e.ModelStub = modelstub.NewAt(loopback + ":0")
	}
	body, _ := json.Marshal(map[string]any{
		"judge_endpoint":       map[string]any{"url": e.Cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1", "locality": "local"},
		"generate_endpoint":    map[string]any{"url": e.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model", "locality": "local"},
		"screening_timeout_ms": 2000,
	})
	e.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
}

// ---- 测试时钟（ACT-ENV-10） ----

const clockOffsetFile = "test-clock-offset"

// TCAdvance 是验收文档的 tc_advance：偏移量文件里的秒数加 sec。内核不需重启。
func (e *Env) TCAdvance(sec int64) {
	e.T.Helper()
	cur := int64(0)
	if b, err := os.ReadFile(filepath.Join(e.Home, clockOffsetFile)); err == nil {
		cur, _ = strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	}
	e.TCSet(cur + sec)
}

// TCSet 直接写偏移量秒数（用来验证"只进不退"：写更小的值应被内核忽略）。
func (e *Env) TCSet(sec int64) {
	e.T.Helper()
	if err := os.WriteFile(filepath.Join(e.Home, clockOffsetFile), []byte(strconv.FormatInt(sec, 10)+"\n"), 0o644); err != nil {
		e.fatal("%v", err)
	}
}

// ServiceLog 是当前内核进程至今的输出（含 slog 服务日志）。
func (e *Env) ServiceLog() string {
	if e.logs == nil {
		return ""
	}
	return e.logs.String()
}

// ServeEnvValue 返回当前 apa serve 进程环境里的变量值。
func (e *Env) ServeEnvValue(key string) (v string) {
	if e.srv == nil {
		return ""
	}
	for _, kv := range e.srv.Env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val // 后者覆盖前者，与 exec 的语义一致
		}
	}
	return v
}

// ---- 来源工具替身与渠道替身（ACT-ENV-07） ----

// InstallToolStub 建 TOOL_STUB_BIN（与邮件、日历、代码托管 CLI 同名的可执行替身）；内核已在运行则重启一次，
// 使之后由内核拉起的来源程序用到替身（来源程序继承内核的 PATH）。
func (e *Env) InstallToolStub() {
	e.T.Helper()
	dir := filepath.Join(e.Root, "tool-stub-bin")
	if out, err := exec.Command(e.Bin["tool-stub"], "install", dir, repoRoot()).CombinedOutput(); err != nil {
		e.fatal("tool-stub install: %v\n%s", err, out)
	}
	e.ToolBin = dir
	if e.srv != nil {
		e.StopKernel()
		e.StartKernelAgain()
	}
}

// SetToolCase 写 $TOOL_STUB_BIN/case。
func (e *Env) SetToolCase(name string) {
	e.T.Helper()
	if err := tools.SetCase(e.ToolBin, name); err != nil {
		e.fatal("%v", err)
	}
}

// RunTool 以内核的 PATH 解析并运行工具替身（如 himalaya），返回 stdout 与退出码。
func (e *Env) RunTool(name string, args ...string) (string, int) {
	e.T.Helper()
	cmd := exec.Command(filepath.Join(e.ToolBin, name), args...)
	cmd.Env = append(os.Environ(), "PATH="+e.ToolBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var out strings.Builder
	cmd.Stdout = &out
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return out.String(), ee.ExitCode()
	} else if err != nil {
		e.fatal("运行 %s: %v", name, err)
	}
	return out.String(), 0
}

// ToolLog 是工具替身的调用记录（argv 与环境变量名）。TOOL_LOG 不在子进程环境里时替身写 $TOOL_STUB_BIN/tool.log。
func (e *Env) ToolLog() []tools.LogEntry {
	e.T.Helper()
	f, err := os.Open(filepath.Join(e.ToolBin, "tool.log"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []tools.LogEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var le tools.LogEntry
		if json.Unmarshal(sc.Bytes(), &le) != nil {
			e.fatal("TOOL_LOG 行不是 JSON: %s", sc.Text())
		}
		out = append(out, le)
	}
	return out
}

// AskDecision 用 DEL 建一个任务，再用 WRK 为它写入带两个选项的决策请求，返回事项 id。
func (e *Env) AskDecision(title string) string {
	e.T.Helper()
	r := e.Apa(e.DEL, "", "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", e.Projects["acc-work"], "--evidence", "建个任务")
	var m struct {
		ID string `json:"id"`
	}
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &m) != nil || m.ID == "" {
		e.fatal("item add %s: %d %s %s", title, r.Code, r.Stdout, r.Stderr)
	}
	if r := e.Apa(e.WRK, "", "ask", m.ID, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A"); r.Code != 0 {
		e.fatal("ask: %d %s %s", r.Code, r.Stdout, r.Stderr)
	}
	return m.ID
}

// StartChannelExt 把决策类投递路由到渠道 acc-chan，启动随包的 Telegram 渠道 ext，连接仓库内机器人替身，并用 CHAN 领取投递。
func (e *Env) StartChannelExt() *fakebot.Bot {
	e.T.Helper()
	pol, _ := json.Marshal(map[string]any{"disturb": map[string]any{
		"routes":           map[string]string{"decision": "acc-chan", "reminder": "web", "brief": "web", "health": "web", "wake": "web"},
		"merge_window_sec": 2}})
	e.webOK("PUT", "/v1/settings/policy", string(pol), 200)
	bot, err := fakebot.New()
	if err != nil {
		e.fatal("fakebot: %v", err)
	}
	cmd := exec.Command(e.Bin["telegram-ext"])
	cmd.Env = append(os.Environ(), "APA_URL="+e.URL, "APA_EXT_KEY="+e.CHAN, "TG_BOT=1:acc", "TG_CHAT=1", "TG_API="+bot.URL,
		"APA_EXT_STATE="+filepath.Join(e.Root, "ext-telegram.json"))
	logs := &syncBuf{}
	cmd.Stdout, cmd.Stderr = logs, logs
	if err := cmd.Start(); err != nil {
		e.fatal("telegram-ext: %v", err)
	}
	e.T.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		bot.Close()
	})
	return bot
}

// ---- 合成账本夹具与旧版本二进制（ACT-ENV-08） ----

// Fixture 是一份账本夹具的一次性副本。
type Fixture struct {
	Source string                  // 夹具目录（只读）
	Home   string                  // 副本，可作 APA_HOME
	M      *ledgerfixture.Manifest // manifest.json
	Raw    map[string]json.RawMessage
	e      *Env
}

// LedgerFixture 生成结构版本 version 的合成账本夹具（LEDGER_FIXTURE_V<n>）并拷贝出一份一次性副本（FX_HOME）。
func (e *Env) LedgerFixture(version int) *Fixture {
	e.T.Helper()
	e.fixtures++
	src := filepath.Join(e.Root, fmt.Sprintf("ledger-fixture-v%d-%d", version, e.fixtures))
	m, err := ledgerfixture.Generate(src, version)
	if err != nil {
		e.fatal("生成账本夹具 V%d: %v", version, err)
	}
	home := filepath.Join(e.Root, fmt.Sprintf("fx-home-%d", e.fixtures))
	if out, err := exec.Command("cp", "-R", src, home).CombinedOutput(); err != nil {
		e.fatal("cp -R: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(src, "manifest.json"))
	if err != nil {
		e.fatal("%v", err)
	}
	fx := &Fixture{Source: src, Home: home, M: m, e: e}
	if err := json.Unmarshal(raw, &fx.Raw); err != nil {
		e.fatal("manifest.json: %v", err)
	}
	return fx
}

var (
	v01Once sync.Once
	v01Bin  string
	v01Err  error
	v01Skip bool
)

// V01Bin 是旧版本主程序（APA_V01_BIN）：优先用同名环境变量，否则由 fetch-v01.sh 从冻结提交构建（缓存在系统临时目录，与夹具测试共用）。
// 冻结提交不在这份克隆里（浅克隆）时跳过当前测试。
func (e *Env) V01Bin() string {
	e.T.Helper()
	v01Once.Do(func() {
		if b := os.Getenv("APA_V01_BIN"); b != "" {
			if _, v01Err = os.Stat(b); v01Err == nil {
				v01Bin = b
			}
			return
		}
		out := filepath.Join(os.TempDir(), "apa-fixture-v01")
		commit := ledgerfixture.Frozen[2]
		v01Bin = filepath.Join(out, "apa-"+commit)
		if _, err := os.Stat(v01Bin); err == nil {
			return
		}
		cmd := exec.Command("bash", filepath.Join(repoRoot(), "tests", "fixtures", "ledger", "fetch-v01.sh"), out)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 3 {
				v01Skip = true
			}
			v01Err = fmt.Errorf("fetch-v01.sh: %w", err)
		}
	})
	if v01Skip {
		e.T.Skipf("冻结提交不在这份克隆里；设置 APA_V01_BIN 或 git fetch --unshallow: %v", v01Err)
	}
	if v01Err != nil {
		e.fatal("%v", v01Err)
	}
	return v01Bin
}

// V01Plugin 是旧版本宿主插件包路径（APA_V01_PLUGIN）：优先用同名环境变量，否则取 V01Bin 同目录里 fetch-v01.sh 的产物。
func (e *Env) V01Plugin() string {
	e.T.Helper()
	if p := os.Getenv("APA_V01_PLUGIN"); p != "" {
		return p
	}
	bin := e.V01Bin()
	p := filepath.Join(filepath.Dir(bin), "apa-hermes-plugin-"+strings.TrimPrefix(filepath.Base(bin), "apa-")+".tar.gz")
	if _, err := os.Stat(p); err != nil {
		e.fatal("旧版本插件包不存在: %v", err)
	}
	return p
}

// V01Server 是旧版本二进制在夹具副本上运行的 `apa serve`。
type V01Server struct {
	Base string
	cmd  *exec.Cmd
}

// ServeV01 用旧版本二进制在夹具副本上启动 `apa serve`（监听改为本机空闲端口，保留夹具的其余配置）。
func (f *Fixture) ServeV01(bin string) *V01Server {
	e := f.e
	e.T.Helper()
	port, err := freePort()
	if err != nil {
		e.fatal("%v", err)
	}
	cfgPath := filepath.Join(f.Home, "config.yaml")
	cfg, err := os.ReadFile(cfgPath)
	if err != nil {
		e.fatal("%v", err)
	}
	lines := strings.Split(string(cfg), "\n")
	for i, l := range lines {
		if strings.HasPrefix(l, "listen:") {
			lines[i] = "listen: " + loopback + ":" + port
		}
	}
	if err := os.WriteFile(cfgPath, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		e.fatal("%v", err)
	}
	cmd := exec.Command(bin, "serve")
	cmd.Env = append(os.Environ(), "APA_HOME="+f.Home)
	out, _ := cmd.StdoutPipe()
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		e.fatal("旧版本 apa serve: %v", err)
	}
	e.T.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(out).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		var r struct{ OK bool }
		if json.Unmarshal([]byte(line), &r) != nil || !r.OK {
			e.fatal("旧版本二进制没有起来: %q", line)
		}
	case <-time.After(30 * time.Second):
		e.fatal("旧版本二进制 30 秒内没有就绪")
	}
	return &V01Server{Base: "http://" + loopback + ":" + port, cmd: cmd}
}

// Get 以 Bearer token 读旧版本内核。
func (s *V01Server) Get(path, token string) (int, []byte) {
	req, _ := http.NewRequest("GET", s.Base+path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}
