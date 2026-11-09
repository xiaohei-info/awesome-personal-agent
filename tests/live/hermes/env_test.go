//go:build live

package hermes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const harness = "hermes"

var tokenRE = regexp.MustCompile(`apa_[A-Za-z0-9_]{12,}`)

type fx struct {
	*baseline.Env
	t         *testing.T
	top       *testing.T // 会话生命周期跟整个测试，不跟某个步骤
	hermesBN  string
	hermesPy  string // Hermes venv 的 python（多轮会话驱动用）
	hermesSrc string
	seed      string
	execBin   string // 执行者 work-host.cli 的真实可执行文件
	evidence  map[string]any
	n         int
	shimDir   string
	shimLog   string
}

// call 是 apa 垫片记下的一次 apa 调用（插件钩子与 agent 自己的调用都经过它）。
type call struct {
	Session, Args, Out, Cwd string
	Code                    int
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

// setup 建环境：init、serve、签发凭据与项目、MODEL_STUB（内核自己的模型调用），并确认真实 Hermes 可用。
func setup(t *testing.T, serveExtra string) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	e.ServeWith(serveExtra)
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	f := &fx{Env: e, t: t, top: t, evidence: map[string]any{}}
	f.hermesBN = envOr("APA_LIVE_HERMES_BIN", "/opt/apa-live/venv/bin/hermes")
	f.hermesPy = filepath.Join(filepath.Dir(f.hermesBN), "python")
	f.hermesSrc = envOr("APA_LIVE_HERMES_SRC", "/opt/apa-live/src/hermes-agent")
	f.seed = envOr("APA_LIVE_HERMES_SEED", "/opt/apa-live/hermes-home")
	for _, p := range []string{f.hermesBN, f.hermesPy, f.hermesSrc} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("真实 Hermes 不可用（%s）：%v", p, err)
		}
	}
	// 执行者 work-host.cli 是一个真实的外部 CLI 进程：只打印完成说明，不自行 apa report（回执交给插件捕获）。
	f.makeShim()
	f.execBin = filepath.Join(e.Root, "work-host-cli.sh")
	// 它被调用时在垫片日志里记一行，用来核对“先 apa delegate 再调用执行者”的先后。
	script := "#!/bin/sh\nprintf '\\n@@CALL session=<> rc=0 cwd=<> args=@@EXECUTOR %s\\n\\n' \"$*\" >> " + f.shimLog + "\necho 'work-host.cli 已处理 PR https://example.invalid/acc/pr/6'\n"
	must(t, os.WriteFile(f.execBin, []byte(script), 0o755))
	return f
}

// makeShim 在 PATH 最前放一个 apa 垫片：记录每次调用的会话、参数、退出码与 stdout，再原样执行真实 apa。
// 它只观察，不改变结果，用来证明“钩子跑了 apa agents check”“agent 调了 apa decide”。
func (f *fx) makeShim() {
	// 垫片与日志放在 APA_HOME 所在目录之外，agent 在检出目录里翻找时碰不到账本和日志。
	dir, err := os.MkdirTemp("", "apa-live.shim.")
	must(f.t, err)
	f.top.Cleanup(func() { _ = os.RemoveAll(dir) })
	f.shimDir = dir
	f.shimLog = filepath.Join(dir, "apa-calls.log")
	script := fmt.Sprintf(`#!/bin/sh
out=$(%q "$@"); rc=$?
printf '\n@@CALL session=<%%s> rc=%%s cwd=<%%s> args=%%s\n%%s\n' "$APA_SESSION" "$rc" "$(pwd -P)" "$*" "$out" >> %q
printf '%%s' "$out"
exit $rc
`, f.Bin["apa"], f.shimLog)
	must(f.t, os.WriteFile(filepath.Join(f.shimDir, "apa"), []byte(script), 0o755))
}

// calls 返回垫片记下的、参数含 sub 的调用。
func (f *fx) calls(sub string) []call {
	b, _ := os.ReadFile(f.shimLog)
	var out []call
	for _, blk := range strings.Split(string(b), "\n@@CALL ")[1:] {
		head, body, _ := strings.Cut(blk, "\n")
		var c call
		c.Session = between(head, "session=<", ">")
		fmt.Sscanf(strings.SplitN(head, "> rc=", 2)[1], "%d", &c.Code)
		c.Cwd = between(head, "cwd=<", ">")
		c.Args = head[strings.Index(head, "> args=")+7:]
		c.Out = strings.TrimSpace(body)
		if strings.Contains(c.Args, sub) {
			out = append(out, c)
		}
	}
	return out
}

// step 按验收顺序执行，前一步未通过后续不得开始；结果记入证据。
func (f *fx) step(t *testing.T, name string, fn func(t *testing.T)) {
	t.Helper()
	parent := f.t
	ok := t.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn(t)
	})
	f.evidence[strings.Fields(name)[0]+"-result"] = map[string]any{"step": name, "passed": ok}
	if !ok {
		// 验收顺序：前一步未通过后续不得开始。APA_LIVE_CONTINUE=1 仅用于“取全证据”：继续跑后面各自独立的步骤，
		// 整个测试仍然失败。
		if os.Getenv("APA_LIVE_CONTINUE") == "1" {
			t.Errorf("%s 未通过（APA_LIVE_CONTINUE=1：继续后续步骤取证，整体仍失败）", name)
			return
		}
		t.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}

// hermesHome 建全新隔离 HERMES_HOME：只复制种子的 config.yaml 与 .env，挂上并启用 plugins/hermes 下被选中的插件。
func (f *fx) hermesHome(name string, plugins ...string) string {
	t := f.t
	home := filepath.Join(f.Root, "hermes-home-"+name)
	must(t, os.MkdirAll(filepath.Join(home, "plugins"), 0o700))
	for _, n := range []string{"config.yaml", ".env"} {
		b, err := os.ReadFile(filepath.Join(f.seed, n))
		must(t, err)
		if n == "config.yaml" {
			if strings.Contains("\n"+string(b), "\nplugins:") {
				t.Fatal("种子 config.yaml 已有 plugins 段，请改为无插件的全新种子")
			}
			b = append(b, []byte("\nplugins:\n  enabled:\n")...)
			for _, p := range plugins {
				b = append(b, []byte("    - apa-"+p+"\n")...)
			}
		}
		must(t, os.WriteFile(filepath.Join(home, n), b, 0o600))
	}
	for _, p := range plugins {
		must(t, os.Symlink(filepath.Join(repoRoot(), "plugins", "hermes", p), filepath.Join(home, "plugins", "apa-"+p)))
	}
	table, _ := json.Marshal([]map[string]any{{"id": "work-host.cli", "kind": "external-cli", "scenarios": []string{"coding"},
		"host": "work-host", "command": []string{f.execBin, "{prompt}"}}})
	must(t, os.WriteFile(filepath.Join(home, "apa-executors.json"), table, 0o600))
	return home
}

// env 是 Hermes 进程的环境：隔离的 HERMES_HOME，插件用的 token，apa 在 PATH 上。
func (f *fx) env(home, token string, extra ...string) []string {
	e := []string{"HOME=" + f.Root, "PATH=" + f.shimDir + ":" + os.Getenv("PATH"), "HERMES_HOME=" + home,
		"PYTHONPATH=" + f.hermesSrc, "APA_BIN=" + filepath.Join(f.shimDir, "apa"), "APA_URL=" + f.URL, "APA_TOKEN=" + token,
		"HERMES_YOLO_MODE=1", "HERMES_ACCEPT_HOOKS=1"}
	return append(e, extra...)
}

// checkout 为项目建立本机检出目录并登记（机器名为本机主机名），返回目录。
func (f *fx) checkout(project string) string {
	f.t.Helper()
	dir, err := filepath.EvalSymlinks(f.mkdir("checkout-" + project))
	must(f.t, err)
	host, _ := os.Hostname()
	remote := "https://example.invalid/acc/" + project + ".git"
	f.apa(f.DEL, 0, "project", "repos", "add", f.Projects[project], "--remote", remote, "--evidence", "验收检出")
	f.apa(f.WRK, 0, "project", "repos", "add", f.Projects[project], "--remote", remote, "--path", dir, "--host", host)
	return dir
}

func (f *fx) mkdir(name string) string {
	d := filepath.Join(f.Root, name)
	must(f.t, os.MkdirAll(d, 0o755))
	return d
}

// ---- 真实 Hermes 会话 ----

type turn struct {
	SessionID string `json:"session_id"`
	Final     string `json:"final"`
	Tools     []struct {
		Name string `json:"name"`
		Args string `json:"args"`
	} `json:"tools"`
	Secs      float64 `json:"secs"`
	HookSecs  float64 `json:"hook_secs"`
	Injected  string  `json:"injected"`
	Compacted bool    `json:"compacted"`
	Before    int     `json:"before"`
	After     int     `json:"after"`
}

// toolCalls 返回本回合里名字包含 sub 的工具调用数。
func (r turn) toolCalls(sub string) int {
	n := 0
	for _, tc := range r.Tools {
		if tc.Name == "tool_describe" { // 只是查看工具说明，没有执行
			continue
		}
		if strings.Contains(tc.Name, sub) || strings.Contains(tc.Args, sub) {
			n++
		}
	}
	return n
}

type sess struct {
	f    *fx
	cmd  *exec.Cmd
	in   io.WriteCloser
	out  *bufio.Reader
	logb *bytes.Buffer
	ID   string
}

// session 启动一个常驻的真实 Hermes 主会话（多轮同一进程），cwd 是会话所在目录。
func (f *fx) session(home, cwd, token string, extra ...string) *sess {
	f.t.Helper()
	cmd := exec.Command(f.hermesPy, filepath.Join(repoRoot(), "tests", "live", "hermes", "driver.py"))
	cmd.Dir = cwd
	cmd.Env = f.env(home, token, extra...)
	s := &sess{f: f, cmd: cmd, logb: &bytes.Buffer{}}
	cmd.Stderr = s.logb
	var err error
	s.in, err = cmd.StdinPipe()
	must(f.t, err)
	o, err := cmd.StdoutPipe()
	must(f.t, err)
	s.out = bufio.NewReaderSize(o, 1<<20)
	must(f.t, cmd.Start())
	f.top.Cleanup(s.close)
	return s
}

func (s *sess) send(cmd map[string]any) turn {
	s.f.t.Helper()
	b, _ := json.Marshal(cmd)
	_, err := s.in.Write(append(b, '\n'))
	must(s.f.t, err)
	type res struct {
		t   turn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		for {
			line, err := s.out.ReadString('\n')
			if err != nil {
				ch <- res{err: fmt.Errorf("Hermes 会话已退出：%v\n%s", err, s.f.redact(s.logb.String()))}
				return
			}
			if rest, ok := strings.CutPrefix(line, "APA-TURN "); ok {
				var r turn
				err = json.Unmarshal([]byte(rest), &r)
				ch <- res{t: r, err: err}
				return
			}
		}
	}()
	select {
	case r := <-ch:
		if r.err != nil {
			s.f.t.Fatal(r.err)
		}
		s.ID = r.t.SessionID
		return r.t
	case <-time.After(10 * time.Minute):
		s.f.t.Fatalf("Hermes 回合 10 分钟未返回\n%s", s.f.redact(s.logb.String()))
	}
	return turn{}
}

// say 发一条用户消息，返回（脱敏后的）结果。
func (s *sess) say(text string) turn {
	s.f.t.Helper()
	r := s.send(map[string]any{"op": "say", "text": text})
	r.Final = s.f.redact(r.Final)
	s.f.n++
	s.f.saveEvidence(fmt.Sprintf("%s-session-%03d.txt", s.f.t.Name()[:strings.Index(s.f.t.Name()+"/", "/")], s.f.n), "USER: "+text+"\nTOOLS: "+s.f.redact(fmt.Sprint(r.Tools))+"\nFINAL: "+r.Final+"\n")
	return r
}

func (s *sess) compress() turn { s.f.t.Helper(); return s.send(map[string]any{"op": "compress"}) }

func (s *sess) compact() { s.f.t.Helper(); s.send(map[string]any{"op": "compact"}) }

func (s *sess) close() {
	if s.cmd.Process == nil {
		return
	}
	_ = s.in.Close()
	done := make(chan struct{})
	go func() { _ = s.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
	}
}

// ---- 账本与 Web ----

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func idOf(t *testing.T, stdout string) string {
	t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &m); err != nil || m.ID == "" {
		t.Fatalf("没有返回 id: %q %v", stdout, err)
	}
	return m.ID
}

// add 用 DEL 建任务事项。
func (f *fx) add(project, title, body, tags, assignee string) string {
	f.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--project", f.Projects[project], "--evidence", "验收准备"}
	if body != "" {
		args = append(args, "--body", body)
	}
	if tags != "" {
		args = append(args, "--tags", tags)
	}
	if assignee != "" {
		args = append(args, "--assignee", assignee)
	}
	return idOf(f.t, f.apa(f.DEL, 0, args...).Stdout)
}

// ask 建一条待本人回答的决策请求。
func (f *fx) ask(project, title, tags, question string, opts ...string) string {
	f.t.Helper()
	id := f.add(project, title, "", tags, "work-host.cli")
	args := []string{"ask", id, "--question", question}
	for _, o := range opts {
		args = append(args, "--option", o)
	}
	f.apa(f.WRK, 0, args...)
	return id
}

type event struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Reason  string         `json:"reason"`
	Actor   map[string]any `json:"actor"`
	Payload map[string]any `json:"payload"`

	ResolvedVia string `json:"resolved_via"`
	Evidence    string `json:"evidence"`
}

func (f *fx) events(object, typ string) []event {
	f.t.Helper()
	var out struct {
		Events []event `json:"events"`
	}
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + url.QueryEscape(object)
	}
	if typ != "" {
		path += "&type=" + url.QueryEscape(typ)
	}
	must(f.t, json.Unmarshal([]byte(f.webOK("GET", path, "", 200)), &out))
	return out.Events
}

func (f *fx) item(id string) map[string]any {
	f.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out))
	return out.Item
}

func (f *fx) phase(id string) string { s, _ := f.item(id)["phase"].(string); return s }

func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("等待超时（%v）：%s", d, what)
		}
		time.Sleep(time.Second)
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

func contains(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if !strings.Contains(text, s) {
			t.Errorf("%s 应含 %q: %s", what, s, text)
		}
	}
}

func lacks(t *testing.T, what, text string, subs ...string) {
	t.Helper()
	for _, s := range subs {
		if strings.Contains(text, s) {
			t.Errorf("%s 不应含 %q: %s", what, s, text)
		}
	}
}

// redact 去掉输出里的令牌（本环境签发的明文与任何 apa_ 开头的凭据）。
func (f *fx) redact(s string) string {
	for _, tok := range []string{f.OBS, f.WRK, f.WRK2, f.DEL, f.CHAN} {
		if tok != "" {
			s = strings.ReplaceAll(s, tok, "<redacted>")
		}
	}
	return tokenRE.ReplaceAllString(s, "<redacted>")
}

func (f *fx) saveEvidence(name, content string) {
	dir := os.Getenv("APA_LIVE_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	must(f.t, os.MkdirAll(dir, 0o755))
	must(f.t, os.WriteFile(filepath.Join(dir, name), []byte(f.redact(content)), 0o644))
}

func (f *fx) flushEvidence(name string) {
	b, _ := json.MarshalIndent(f.evidence, "", "  ")
	f.saveEvidence(name, string(b)+"\n")
}

func between(s, start, end string) string {
	_, rest, _ := strings.Cut(s, start)
	v, _, _ := strings.Cut(rest, end)
	return v
}

func hostname() (string, error) { return os.Hostname() }

func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

func replyJSON(content string) modelstub.Reply { return modelstub.Reply{Content: content} }

func modelstubDelayed(content string) modelstub.Reply {
	return modelstub.Reply{Content: content, Delay: 20 * time.Second}
}
