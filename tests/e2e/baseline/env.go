// Package baseline 是全流程验收（tests/e2e）共用的环境脚本：隔离 APA_HOME、真实 apa 二进制、
// 五个凭据、四个验收项目、模型端点替身与无头运行替身声明（验收文档 flow-env-baseline，ACT-ENV-01..06）。
// 只用于测试与验收环境，不得被 apa 二进制或内核代码导入。
package baseline

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const (
	loopback = "127.0.0.1"
	// Password 是 Web 登录口令（apa init 设置）。
	Password = "e2e-baseline-password"
	// DefaultTestRepo 是 <TEST_REPO> 的占位地址；真实验收用 APA_E2E_TEST_REPO 指向专用可写测试仓库。
	DefaultTestRepo = "https://example.invalid/acc/test-repo.git"
)

// Env 是一套验收环境。字段名对应验收文档里的环境变量。
type Env struct {
	T         testing.TB
	Root      string            // 测试专用根目录，APA_HOME 在其内
	Home      string            // APA_HOME
	URL       string            // APA_URL
	NoSession bool              // 为真时带 --evidence 的命令也不设 APA_SESSION
	Bin       map[string]string // apa、stub-harness 的可执行文件
	TestRepo  string            // <TEST_REPO>

	OBS, WRK, WRK2, DEL, CHAN string

	Projects  map[string]string // 项目名 → 项目 id
	ModelStub *modelstub.Stub   // MODEL_STUB

	srv     *exec.Cmd
	srvDone chan struct{}
	logs    *syncBuf
	session string // Web 会话 cookie 值
	csrf    string
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Result 是一次 CLI 调用的结果。
type Result struct {
	Code           int
	Stdout, Stderr string
}

// Resp 是一次 HTTP 调用的结果。
type Resp struct {
	Code int
	Body string
}

var (
	buildOnce sync.Once
	buildDir  string
	buildErr  error
)

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

// buildBinaries 每个测试进程只构建一次。前端必须已构建（web/dist/index.html），否则登录页无从验证。
func buildBinaries() (map[string]string, error) {
	buildOnce.Do(func() {
		root := repoRoot()
		if _, err := os.Stat(filepath.Join(root, "web", "dist", "index.html")); err != nil {
			buildErr = fmt.Errorf("Web 前端未构建，先运行 `npm --prefix web ci && npm --prefix web run build`: %w", err)
			return
		}
		buildDir, buildErr = os.MkdirTemp("", "apa-e2e-bin-")
		if buildErr != nil {
			return
		}
		for name, pkg := range map[string]string{"apa": "./cli/apa", "stub-harness": "./tests/doubles/stubharness/cmd/stub-harness"} {
			cmd := exec.Command("go", "build", "-o", filepath.Join(buildDir, name), pkg)
			cmd.Dir = root
			if out, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("go build %s: %v\n%s", pkg, err, out)
				return
			}
		}
	})
	if buildErr != nil {
		return nil, buildErr
	}
	return map[string]string{"apa": filepath.Join(buildDir, "apa"), "stub-harness": filepath.Join(buildDir, "stub-harness")}, nil
}

// New 准备目录与二进制，但不启动任何进程；按 StartKernel、IssueCredentials、CreateProjects、StartModelStub、
// DeclareStubHarness 的顺序逐步建立环境，或直接用 Setup。
func New(t testing.TB) *Env {
	t.Helper()
	bin, err := buildBinaries()
	if err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp("", "apa-e2e-")
	if err != nil {
		t.Fatal(err)
	}
	repo := os.Getenv("APA_E2E_TEST_REPO")
	if repo == "" {
		repo = DefaultTestRepo
	}
	e := &Env{T: t, Root: root, Home: filepath.Join(root, "home"), Bin: bin, TestRepo: repo, Projects: map[string]string{}}
	t.Cleanup(func() {
		e.StopKernel()
		if e.ModelStub != nil {
			e.ModelStub.Close()
		}
		os.RemoveAll(root)
	})
	return e
}

// Setup 一次建好全部环境。
func Setup(t testing.TB) *Env {
	t.Helper()
	e := New(t)
	e.StartKernel()
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.DeclareStubHarness()
	return e
}

func (e *Env) fatal(format string, a ...any) {
	e.T.Helper()
	e.T.Fatalf(format, a...)
}

func (e *Env) env(token string, args []string) []string {
	env := append(os.Environ(), "APA_HOME="+e.Home, "APA_URL="+e.URL)
	if token != "" {
		env = append(env, "APA_TOKEN="+token)
	}
	// 带本人原话（--evidence）的第二档命令还要会话引用；harness 插件会设 APA_SESSION，这里替身。
	// NoSession 供要验证"只带原话不带会话"的用例关掉它。
	if !e.NoSession && slices.ContainsFunc(args, func(a string) bool { return a == "--evidence" || strings.HasPrefix(a, "--evidence=") }) {
		env = append(env, "APA_SESSION=e2e-session")
	}
	return env
}

// Apa 以 token 运行 apa 子命令（APA_HOME 与 APA_URL 指向本环境），stdin 可为空。
func (e *Env) Apa(token, stdin string, args ...string) Result {
	e.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Bin["apa"], args...)
	cmd.Env = e.env(token, args)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := Result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		e.fatal("运行 apa %v: %v", args, err)
	}
	return r
}

func freePort() (string, error) {
	l, err := net.Listen("tcp", loopback+":0")
	if err != nil {
		return "", err
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port, nil
}

// StartKernel 依次执行 `apa init` 与 `apa serve`（监听改为本机空闲端口，其余为默认配置），并以口令登录 Web。
func (e *Env) StartKernel() {
	e.T.Helper()
	e.Init()
	e.Serve()
}

// Init 在空的 APA_HOME 上运行 `apa init`，口令为 Password。
func (e *Env) Init() Result {
	e.T.Helper()
	r := e.Apa("", Password+"\n", "init", "--password-stdin")
	if r.Code != 0 {
		e.fatal("apa init 退出码 %d: %s%s", r.Code, r.Stdout, r.Stderr)
	}
	return r
}

// Serve 把监听改为本机空闲端口后启动 `apa serve` 并登录 Web；须先 Init。
func (e *Env) Serve() { e.T.Helper(); e.ServeWith("") }

// ServeWith 同 Serve，并把 extra（config.yaml 的附加行，如验收时调小的投递时限）追加到配置里。
func (e *Env) ServeWith(extra string) {
	e.T.Helper()
	port, err := freePort()
	if err != nil {
		e.fatal("%v", err)
	}
	cfg := filepath.Join(e.Home, "config.yaml")
	if err := os.WriteFile(cfg, []byte("listen: "+loopback+":"+port+"\n"+extra), 0o644); err != nil {
		e.fatal("%v", err)
	}
	e.startServe(port)
}

// StartKernelAgain 在同一 APA_HOME 上重新启动 apa serve（沿用 config.yaml 的监听地址）。
func (e *Env) StartKernelAgain() {
	e.T.Helper()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(e.URL, "http://"))
	e.startServe(port)
}

func (e *Env) startServe(port string) {
	e.T.Helper()
	e.logs = &syncBuf{}
	e.srv = exec.Command(e.Bin["apa"], "serve")
	e.srv.Env = e.env("", nil)
	e.srv.Stdout, e.srv.Stderr = e.logs, e.logs
	if err := e.srv.Start(); err != nil {
		e.fatal("apa serve: %v", err)
	}
	e.srvDone = make(chan struct{})
	go func(c *exec.Cmd, done chan struct{}) { _ = c.Wait(); close(done) }(e.srv, e.srvDone)
	e.URL = "http://" + loopback + ":" + port
	deadline := time.Now().Add(30 * time.Second)
	for {
		if strings.Contains(e.logs.String(), `"ok":true`) {
			break
		}
		select {
		case <-e.srvDone:
			e.fatal("apa serve 提前退出: %s", e.logs.String())
		default:
		}
		if time.Now().After(deadline) {
			e.fatal("apa serve 30 秒内未就绪: %s", e.logs.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	e.webLogin()
}

// ServePID 是当前 apa serve 进程的 pid，未运行时为 0。
func (e *Env) ServePID() int {
	if e.srv == nil || e.srv.Process == nil {
		return 0
	}
	return e.srv.Process.Pid
}

// StopKernel 停止 apa serve（可重复调用）。
func (e *Env) StopKernel() {
	if e.srv == nil || e.srv.Process == nil {
		return
	}
	_ = e.srv.Process.Signal(os.Interrupt)
	select {
	case <-e.srvDone:
	case <-time.After(10 * time.Second):
		_ = e.srv.Process.Kill()
		<-e.srvDone
	}
	e.srv = nil
}

func (e *Env) do(method, path, body string, hdr map[string]string) Resp {
	e.T.Helper()
	req, err := http.NewRequest(method, e.URL+path, strings.NewReader(body))
	if err != nil {
		e.fatal("%v", err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		e.fatal("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if cs := resp.Cookies(); len(cs) > 0 {
		for _, c := range cs {
			if c.Name == "apa_session" {
				e.session = c.Value
			}
		}
	}
	return Resp{resp.StatusCode, string(b)}
}

func (e *Env) webLogin() {
	e.T.Helper()
	r := e.do("POST", "/v1/auth/login", `{"password":"`+Password+`"}`, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil || out.CSRF == "" || e.session == "" {
		e.fatal("Web 登录失败: %d %s", r.Code, r.Body)
	}
	e.csrf = out.CSRF
}

// WebRaw 发一次 Web 请求；session 为 true 时带会话 Cookie 与 CSRF 头（本人在 Web 上操作）。
func (e *Env) WebRaw(method, path, body string, session bool) Resp {
	e.T.Helper()
	hdr := map[string]string{}
	if session {
		hdr["Cookie"] = "apa_session=" + e.session
		hdr["X-CSRF-Token"] = e.csrf
	}
	saved := e.session
	r := e.do(method, path, body, hdr)
	if !session {
		e.session = saved // 未登录的探测请求不改变已有会话
	}
	return r
}

// Web 以已登录的本人会话请求内核（等同于 Web 页面调用的 HTTP API）。
func (e *Env) Web(method, path, body string) Resp { return e.WebRaw(method, path, body, true) }

// APIRaw 以 Bearer 凭据请求内核。
func (e *Env) APIRaw(method, path, body, token string) Resp {
	e.T.Helper()
	return e.do(method, path, body, map[string]string{"Authorization": "Bearer " + token})
}

func (e *Env) webOK(method, path, body string, want int) string {
	e.T.Helper()
	r := e.Web(method, path, body)
	if r.Code != want {
		e.fatal("%s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

// IssueCredentials 在设置里签发 OBS、WRK、WRK2、DEL、CHAN，并维护执行者清单 work-host.cli、other-host.cli。
func (e *Env) IssueCredentials() {
	e.T.Helper()
	for _, x := range []string{"work-host.cli", "other-host.cli"} {
		e.webOK("POST", "/v1/settings/executors", `{"id":"`+x+`","kind":"external-cli"}`, 200)
	}
	issue := func(path, body string) string {
		var out struct {
			Secret string `json:"secret"`
		}
		if err := json.Unmarshal([]byte(e.webOK("POST", path, body, 201)), &out); err != nil || out.Secret == "" {
			e.fatal("签发 %s 没有返回明文: %v", body, err)
		}
		return out.Secret
	}
	e.OBS = issue("/v1/settings/tokens", `{"kind":"observer","label":"OBS"}`)
	e.WRK = issue("/v1/settings/tokens", `{"kind":"worker","label":"WRK","executor":"work-host.cli"}`)
	e.WRK2 = issue("/v1/settings/tokens", `{"kind":"worker","label":"WRK2","executor":"other-host.cli"}`)
	e.DEL = issue("/v1/settings/tokens", `{"kind":"delegate","label":"DEL"}`)
	e.CHAN = issue("/v1/settings/ext-keys", `{"label":"CHAN","scope":"channel.acc-chan"}`)
}

// CreateProjects 新建 acc-work、acc-open、acc-empty、acc-home，并设置 repos 与允许的执行者。
func (e *Env) CreateProjects() {
	e.T.Helper()
	for _, name := range []string{"acc-work", "acc-open", "acc-empty", "acc-home"} {
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal([]byte(e.webOK("POST", "/v1/projects", `{"name":"`+name+`"}`, 201)), &out); err != nil || out.ID == "" {
			e.fatal("新建项目 %s: %v", name, err)
		}
		e.Projects[name] = out.ID
	}
	e.webOK("POST", "/v1/projects/"+e.Projects["acc-work"]+"/repos", `{"op":"add_remote","remote":"`+e.TestRepo+`"}`, 200)
	allowed := func(name, body string) {
		e.webOK("PUT", "/v1/settings/projects/"+e.Projects[name]+"/allowed-executors", body, 200)
	}
	allowed("acc-work", `{"executors":["work-host.cli"]}`)
	allowed("acc-empty", `{"executors":[]}`)
}

// StartModelStub 启动 MODEL_STUB 并在 Web 设置里把模型端点 base_url 指向它。
func (e *Env) StartModelStub() {
	e.T.Helper()
	e.ModelStub = modelstub.NewAt(loopback + ":0")
	// 位置声明为 local：替身在本机；未声明按 external，外部端点在来源未选外发范围时不送内容（§14.14）。
	body, _ := json.Marshal(map[string]any{"generate_endpoint": map[string]any{
		"url": e.ModelStub.URL(), "model": "acc-model", "api_key": "stub-key", "locality": "local"}, "screening_timeout_ms": 2000})
	e.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
}

func (e *Env) chatCompletion(t *testing.T, prompt string) string {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "acc-model", "messages": []map[string]string{{"role": "user", "content": prompt}}})
	resp, err := http.Post(e.ModelStub.URL()+"/chat/completions", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

// DeclareStubHarness 经 `apa declare` 提交 acc-stub（read/edit/full 三档，声明 resume 与 session 取法）与
// acc-stub-noresume（不声明 resume）两份无头运行声明，并在 Web 确认。
func (e *Env) DeclareStubHarness() {
	e.T.Helper()
	bin := e.Bin["stub-harness"]
	decl := func(harness string, resume bool) string {
		h := map[string]any{
			"command": []string{bin, "{prompt}"},
			"session": map[string]any{"extract": "$.session_id"},
			"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
		}
		if resume {
			h["resume"] = []string{bin, "--resume", "{prompt}"}
		}
		b, _ := json.Marshal(map[string]any{"harness": harness, "headless": h})
		return string(b)
	}
	for harness, resume := range map[string]bool{"acc-stub": true, "acc-stub-noresume": false} {
		if r := e.Apa(e.DEL, decl(harness, resume), "declare", "-", "--evidence", "环境基线声明替身"); r.Code != 0 {
			e.fatal("apa declare %s 退出码 %d: %s%s", harness, r.Code, r.Stdout, r.Stderr)
		}
		e.webOK("POST", "/v1/settings/declarations/"+harness+"/1/confirm", "", 200)
	}
}

// Dependency 是一项真实外部依赖的就绪记录。
type Dependency struct {
	Name   string `json:"name"`
	Ready  bool   `json:"ready"`
	Detail string `json:"detail"`
}

// RecordRealDependencies 只记录 ACT-ENV-06 的检查结果，不判定通过：本人确认真实依赖，未就绪的依赖
// 会让依赖它的流程记为未执行。结果同时写入 $APA_E2E_EVIDENCE_DIR/env-06.json（设置时）。
// Telegram 只有在设置了 TG_BOT_TOKEN 时才调用 Bot API getMe。
func (e *Env) RecordRealDependencies() []Dependency {
	e.T.Helper()
	run := func(name string, args ...string) Dependency {
		out, err := exec.Command(name, args...).CombinedOutput()
		d := Dependency{Name: name, Ready: err == nil, Detail: firstLine(string(out))}
		if err != nil && d.Detail == "" {
			d.Detail = err.Error()
		}
		return d
	}
	deps := []Dependency{run("hermes", "--version"), run("himalaya", "--version"), run("gog", "--version"), run("gh", "auth", "status")}
	tg := Dependency{Name: "telegram"}
	if tok := os.Getenv("TG_BOT_TOKEN"); tok == "" {
		tg.Detail = "TG_BOT_TOKEN 未设置，未调用 getMe"
	} else if resp, err := http.Get("https://api.telegram.org/bot" + tok + "/getMe"); err != nil {
		tg.Detail = "getMe 请求失败"
	} else {
		resp.Body.Close()
		tg.Ready, tg.Detail = resp.StatusCode == 200, fmt.Sprintf("getMe HTTP %d", resp.StatusCode)
	}
	deps = append(deps, tg)
	if dir := os.Getenv("APA_E2E_EVIDENCE_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err == nil {
			b, _ := json.MarshalIndent(deps, "", "  ")
			_ = os.WriteFile(filepath.Join(dir, "env-06.json"), b, 0o644)
		}
	}
	return deps
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	return s
}
