// Package baseline 是全流程验收（tests/e2e）共用的环境脚本：隔离 APA_HOME、真实 apa 二进制、
// 五个凭据、四个验收项目、两个模型端点替身（CLS_STUB、MODEL_STUB）、无头运行替身、来源工具与渠道替身、
// 合成账本夹具与旧版本二进制、测试时钟（验收文档 flow-env-baseline，ACT-ENV-01..10）。不依赖任何真实外部账号。
// 只用于测试与验收环境，不得被 apa 二进制或内核代码导入。
package baseline

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const (
	loopback       = "127.0.0.1"
	reauthRequired = "re" + "auth_required"
	// Password 是 Web 登录口令（apa init 设置）。
	Password = "e2e-baseline-password"
	// DefaultTestRepo 是 <TEST_REPO> 的占位地址（仅供需要"一个远程地址"的用例）；用例需要可写远程时用 APA_E2E_TEST_REPO 指向它。
	DefaultTestRepo = "https://example.invalid/acc/test-repo.git"
)

// Env 是一套验收环境。字段名对应验收文档里的环境变量。
type Env struct {
	T    testing.TB
	Root string // 测试专用根目录，APA_HOME 在其内
	Home string // APA_HOME
	URL  string // APA_URL
	// WebSecret 是 Web 登录口令；缺省为 Password，在账本夹具上启动时是夹具自己的口令（ServeFixture）。
	WebSecret string
	Bin       map[string]string // apa（-tags apa_testclock 测试构建）、stub-harness、tool-stub、telegram-ext 的可执行文件
	TestRepo  string            // <TEST_REPO>
	LocalRepo string            // LOCAL_REPO：file:// 本地 git 仓库（已提交一次），acc-work 的 repos
	ToolBin   string            // TOOL_STUB_BIN，InstallToolStub 之后非空；之后启动的内核 PATH 以它开头

	OBS, WRK, WRK2, DEL, CHAN string
	// DELO 是没有归属任何 Agent 的 delegate 钥匙：档位恒为只整理（§14.5）。DEL 在 IssueCredentials 末尾换成归属 AgentID、
	// 档位为「连日常规矩也替我改」的钥匙，已交付流程里由 Agent 做的二档、3a 动作照常生效；3b 仍只在 Web。
	DELO, AgentID string

	Projects  map[string]string  // 项目名 → 项目 id
	ModelStub *modelstub.Stub    // MODEL_STUB（生成端点；判断题按 Key 配置）
	Cls       *modelstub.ClsStub // CLS_STUB（判断端点）

	srv      *exec.Cmd
	srvDone  chan struct{}
	logs     *syncBuf
	session  string // Web 会话 cookie 值
	csrf     string
	ownerURL string // OwnerApa 的本机转发层
	fixtures int
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
		// apa 一律是测试构建（apa_testclock）：发布物不带该标签，由 flow-release 单独验证。
		for name, pkg := range map[string]string{"apa": "./cli/apa", "stub-harness": "./tests/doubles/stubharness/cmd/stub-harness",
			"tool-stub": "./tests/doubles/tools/cmd/tool-stub", "telegram-ext": "./ext/telegram/cmd/telegram-ext"} {
			cmd := exec.Command("go", "build", "-tags", "apa_testclock", "-o", filepath.Join(buildDir, name), pkg)
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
	bin := map[string]string{}
	for _, n := range []string{"apa", "stub-harness", "tool-stub", "telegram-ext"} {
		bin[n] = filepath.Join(buildDir, n)
	}
	return bin, nil
}

// Run 跑完 m.Run 后删除本进程构建的二进制目录，用 defer 保证测试 panic 时也清。
// 每个用到 New/Setup 的测试包在 TestMain 里 os.Exit(baseline.Run(m))；二进制仍是每个进程只构建一次。
func Run(m *testing.M) int {
	defer func() {
		if buildDir != "" {
			os.RemoveAll(buildDir)
		}
	}()
	return m.Run()
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
	e.LocalRepo = e.newLocalRepo()
	t.Cleanup(func() {
		e.StopKernel()
		if e.ModelStub != nil {
			e.ModelStub.Close()
		}
		if e.Cls != nil {
			e.Cls.Close()
		}
		os.RemoveAll(root)
	})
	return e
}

// newLocalRepo 在临时目录 git init 一个本地仓库并提交一次，返回 file:// 地址。
func (e *Env) newLocalRepo() string {
	// 放在 APA_HOME 所在的 Root 之外：bootstrap 流程要核对 Root 内只有 APA_HOME 一份数据。
	dir, err := os.MkdirTemp("", "apa-e2e-repo-")
	if err != nil {
		e.fatal("%v", err)
	}
	e.T.Cleanup(func() { os.RemoveAll(dir) })
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=acc", "-c", "user.email=acc@example.invalid", "commit", "-q", "--allow-empty", "-m", "init"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			e.fatal("git %v: %v\n%s", args, err, out)
		}
	}
	return "file://" + dir
}

// Setup 一次建好全部环境（单端点的 legacy 接线）。需要判断/生成两个端点的流程在其后调用 WireEndpoints。
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

// AgentHarness 是 DEL 所属 Agent 卡片的宿主：只为建卡片声明，只读一档，不是任何流程的默认宿主。
const AgentHarness = "acc-agent"

// OpenDelegateTier 声明并确认 acc-agent 宿主，建一张它的 Agent 卡片，签一把归属它的 delegate 钥匙作为 DEL，
// 并在 Web 把它的档位设为 tier；原来那把未归属的 delegate 钥匙留作 DELO（只整理）。
func (e *Env) OpenDelegateTier(tier string) {
	e.T.Helper()
	decl, _ := json.Marshal(map[string]any{"harness": AgentHarness, "headless": map[string]any{
		"command": []string{e.Bin["stub-harness"], "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
		"access": map[string]any{"read": []string{"--access=read"}}}})
	if r := e.Apa(e.DEL, string(decl), "declare", "-"); r.Code != 0 {
		e.fatal("apa declare %s 退出码 %d: %s%s", AgentHarness, r.Code, r.Stdout, r.Stderr)
	}
	e.webOK("POST", "/v1/settings/declarations/"+AgentHarness+"/1/confirm", "", 200)
	var card struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(e.webOK("POST", "/v1/settings/agents", `{"harness":"`+AgentHarness+`","name":"acc-agent","enabled":true}`, 200)), &card); err != nil || card.ID == "" {
		e.fatal("建 Agent 卡片: %v", err)
	}
	var out struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(e.webOK("POST", "/v1/settings/tokens", `{"kind":"delegate","label":"DEL-agent","harness":"`+AgentHarness+`","agent_id":"`+card.ID+`"}`, 201)), &out); err != nil || out.Secret == "" {
		e.fatal("签发归属 Agent 的 delegate 钥匙: %v", err)
	}
	e.SetTier(card.ID, tier)
	e.AgentID, e.DELO, e.DEL = card.ID, e.DEL, out.Secret
}

// SetTier 在 Web 设一个 Agent 的档位（organize / decide / rules）。
func (e *Env) SetTier(agent, tier string) {
	e.T.Helper()
	e.webOK("PUT", "/v1/settings/agents/"+agent+"/tier", `{"tier":"`+tier+`"}`, 200)
}

func (e *Env) fatal(format string, a ...any) {
	e.T.Helper()
	e.T.Fatalf(format, a...)
}

func (e *Env) env(url, token string) []string {
	env := append(os.Environ(), "APA_HOME="+e.Home, "APA_URL="+url)
	if token != "" {
		env = append(env, "APA_TOKEN="+token)
	}
	return env
}

// Apa 以 token 运行 apa 子命令（APA_HOME 与 APA_URL 指向本环境），stdin 可为空。
func (e *Env) Apa(token, stdin string, args ...string) Result {
	e.T.Helper()
	return e.apaAt(e.URL, token, stdin, args...)
}

// OwnerApa 以本人的 Web 会话运行 apa 子命令，用于 3b 动作（只在 Web、须刚验过口令）的环境准备：CLI 只会带 Bearer，
// 这里经一个本机转发层把请求换成本人会话（Cookie + CSRF，去掉 Authorization），走的仍是页面调用的同一组路由。
func (e *Env) OwnerApa(stdin string, args ...string) Result {
	e.T.Helper()
	e.Reauth()
	if e.ownerURL == "" {
		target, err := url.Parse(e.URL)
		if err != nil {
			e.fatal("%v", err)
		}
		p := httputil.NewSingleHostReverseProxy(target)
		direct := p.Director
		p.Director = func(r *http.Request) {
			direct(r)
			r.Host = target.Host
			r.Header.Del("Authorization")
			r.Header.Set("Cookie", "apa_session="+e.session)
			r.Header.Set("X-CSRF-Token", e.csrf)
		}
		srv := httptest.NewServer(p)
		e.T.Cleanup(srv.Close)
		e.ownerURL = srv.URL
	}
	return e.apaAt(e.ownerURL, "owner-web-session", stdin, args...)
}

// CloseDelegation 以 token 把委派子事项转为 done：进入 done 要带结果证据，CLI 已不收 --evidence（那是本人原话的旧参数），
// 这里经 HTTP 体的 evidence 字段给出（同 apa report --outcome done 的结果证据）。
func (e *Env) CloseDelegation(token, id, reason string) {
	e.T.Helper()
	body, _ := json.Marshal(map[string]string{"to": "done", "reason": reason, "evidence": "https://example.invalid/pr/1"})
	if r := e.APIRaw("POST", "/v1/items/"+id+"/transition", string(body), token); r.Code != 200 {
		e.fatal("委派子事项 %s 转 done: HTTP %d %s", id, r.Code, r.Body)
	}
}

// OwnerApaOK 同 OwnerApa，退出码不为 0 即失败。
func (e *Env) OwnerApaOK(stdin string, args ...string) Result {
	e.T.Helper()
	r := e.OwnerApa(stdin, args...)
	if r.Code != 0 {
		e.fatal("本人 apa %s 退出码 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r
}

func (e *Env) apaAt(url, token, stdin string, args ...string) Result {
	e.T.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Bin["apa"], args...)
	cmd.Env = e.env(url, token)
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
	e.srv.Env = e.env(e.URL, "")
	if e.ToolBin != "" { // 来源程序继承内核的 PATH：把工具替身目录放在最前
		e.srv.Env = append(e.srv.Env, "PATH="+e.ToolBin+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
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

// WebLogin 以口令重新登录 Web（测试时钟推进数日后原会话已过期）。
func (e *Env) WebLogin() { e.T.Helper(); e.webLogin() }

func (e *Env) webLogin() {
	e.T.Helper()
	r := e.do("POST", "/v1/auth/login", `{"password":"`+cmp.Or(e.WebSecret, Password)+`"}`, nil)
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil || out.CSRF == "" || e.session == "" {
		e.fatal("Web 登录失败: %d %s", r.Code, r.Body)
	}
	e.csrf = out.CSRF
}

// Reauth 再输一次口令（POST /v1/auth/reauth）：3b 动作要求会话刚验过口令，测试时钟推进后用它续上，与页面的弹框同一路由。
func (e *Env) Reauth() {
	e.T.Helper()
	if r := e.WebRaw("POST", "/v1/auth/reauth", `{"password":"`+cmp.Or(e.WebSecret, Password)+`"}`, true); r.Code != 204 {
		e.fatal("再输口令失败: %d %s", r.Code, r.Body)
	}
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
	// 与页面一致：3b 动作遇到"须再输口令"就再输一次口令后重试一次。
	if session && r.Code == 403 && strings.Contains(r.Body, `"`+reauthRequired+`"`) && path != "/v1/auth/reauth" {
		e.Reauth()
		hdr["Cookie"], hdr["X-CSRF-Token"] = "apa_session="+e.session, e.csrf
		r = e.do(method, path, body, hdr)
	}
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
	e.OpenDelegateTier("rules")
}

// CreateProjects 新建 acc-work（repos=[LOCAL_REPO]）、acc-open、acc-empty、acc-home，并设置允许的执行者。
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
	// LOCAL_REPO 是 acc-work 的主仓库；<TEST_REPO> 仍登记在案，因为已交付的映射、托管资料、健康检查流程按它取远程地址。
	for _, remote := range []string{e.LocalRepo, e.TestRepo} {
		e.webOK("POST", "/v1/projects/"+e.Projects["acc-work"]+"/repos", `{"op":"add_remote","remote":"`+remote+`"}`, 200)
	}
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
		"url": e.ModelStub.URL(), "model": "acc-model", "api_key": "stub-key", "locality": "local"}})
	e.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
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
		if r := e.Apa(e.DEL, decl(harness, resume), "declare", "-"); r.Code != 0 {
			e.fatal("apa declare %s 退出码 %d: %s%s", harness, r.Code, r.Stdout, r.Stderr)
		}
		e.webOK("POST", "/v1/settings/declarations/"+harness+"/1/confirm", "", 200)
	}
}

// Notice 是诊断页 other_alerts 的一条"动作没能完成"（GET /health 的 notices）：规则动作失败、运行没拉起（2026-11-20 起这些是事件不是问题）。
type Notice struct {
	Object  string            `json:"object"`
	Rule    string            `json:"rule"`
	Code    string            `json:"code"`
	Params  map[string]string `json:"params"`
	Item    string            `json:"item"`
	Dropped bool              `json:"dropped"`
}

// Notices 读诊断页当前的"动作没能完成"通知。
func (e *Env) Notices() []Notice {
	e.T.Helper()
	var rep struct {
		Signals []struct {
			ID      string   `json:"id"`
			Notices []Notice `json:"notices"`
		} `json:"signals"`
	}
	if err := json.Unmarshal([]byte(e.webOK("GET", "/v1/health", "", 200)), &rep); err != nil {
		e.T.Fatal(err)
	}
	for _, s := range rep.Signals {
		if s.ID == "other_alerts" {
			return s.Notices
		}
	}
	return nil
}

// NoticeWith 返回类别码为 code、原因（params.detail）含 detail 的通知数。
func (e *Env) NoticeWith(code, detail string) int {
	e.T.Helper()
	n := 0
	for _, x := range e.Notices() {
		if x.Code == code && strings.Contains(x.Params["detail"], detail) {
			n++
		}
	}
	return n
}
