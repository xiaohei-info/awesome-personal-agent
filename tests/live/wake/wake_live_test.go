//go:build live

// Package wake 按验收文档 flow-wake-lease（ACT-WAK-01..05）用真实 apa 内核、真实 Hermes 网关与真实 apa-wake 插件走唤醒租约流程。
//
// 真实主会话：Hermes 网关（webhook 平台，只监听回环）+ apa-wake 插件（allow_gateway_injection），插件与主会话持同一个
// delegate token（DEL）。唤醒回合是真实模型驱动的 agent 回合，它的 apa 调用结果取自 Hermes 自己的会话库，内核侧的事实取自账本审计。
//
// 环境（taiyi 的隔离安装见 /opt/apa-live/README.md；缺省即用这些路径，可用环境变量覆盖）：
//
//	APA_LIVE_HERMES_BIN   真实 hermes 可执行文件，缺省 /opt/apa-live/venv/bin/hermes。
//	                      不用 /opt/apa-live/bin/hermes：它的包装脚本把 HERMES_HOME 固定为共享目录，无法隔离每次运行。
//	APA_LIVE_HERMES_HOME  HERMES_HOME 模板（只复制其中 config.yaml 与 .env，权限 0600，不共用），缺省 /opt/apa-live/hermes-home
//
// 网关平台需要 aiohttp，隔离安装里没有时装到本次运行的临时目录（PIP_INDEX_URL 可覆盖镜像），不改动 /opt/apa-live。
// 凭据只在 HERMES_HOME/.env 里，不进入输出或证据。任一依赖缺失即失败（live 节点不得跳过或用替身）。
// 步骤顺序即验收顺序：前一步未通过，后续不得开始。
package wake

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

const (
	harness  = "hermes"
	packName = "acc-wake"
	rule     = packName + "/event_rules/wake-main"
)

var pack = map[string]string{
	"pack.yaml": `name: acc-wake
version: 1.0.0
description: 验收用：唤醒真实主会话
event_rules:
  - id: wake-main
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-wake]}
    do: [{action: start_background_run, runner: main_session, prompt: handle-item, access: read, cwd: run_dir}]
`,
	"prompts/handle-item.md": `---
run_as: main_session
timeout: 5m
---
ACCWAKE 你在处理事项 {item_id}，本人不在场。
`,
}

type run struct {
	ID     string `json:"id"`
	Launch string `json:"launch"`
	Runner string `json:"runner"`
	Target string `json:"target"`
	State  string `json:"state"`
}

type fx struct {
	*baseline.Env
	t          *testing.T
	hermesBin  string
	hermesHome string     // 无头运行用的临时 HERMES_HOME（模板的 config.yaml 与 .env 的副本）
	hermesTmpl string     // HERMES_HOME 模板目录
	top        *testing.T // 顶层测试：网关进程活到整个流程结束，不随子步骤清理
	gw         *exec.Cmd
	gwURL      string
	gwLog      *logBuf
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) apaOK(token string, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != 0 {
		f.t.Fatalf("apa %s 退出码 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) runs(target string) []run {
	f.t.Helper()
	var out struct {
		Runs []run `json:"runs"`
	}
	r := f.apaOK(f.OBS, "run", "list", "--target", target)
	must(f.t, json.Unmarshal([]byte(r.Stdout), &out))
	return out.Runs
}

func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	for deadline := time.Now().Add(d); !cond(); time.Sleep(250 * time.Millisecond) {
		if time.Now().After(deadline) {
			f.t.Fatalf("等待超时（%v）：%s", d, what)
		}
	}
}

// copyHermesHome 把模板里的 config.yaml 与 .env 复制到临时 HERMES_HOME（0600），不共用模板目录。
func copyHermesHome(t *testing.T, tmpl, dst string) {
	t.Helper()
	must(t, os.MkdirAll(dst, 0o700))
	for _, n := range []string{"config.yaml", ".env"} {
		b, err := os.ReadFile(filepath.Join(tmpl, n))
		must(t, err)
		must(t, os.WriteFile(filepath.Join(dst, n), b, 0o600))
	}
}

func setup(t *testing.T) *fx {
	t.Helper()
	bin, tmpl := envOr("APA_LIVE_HERMES_BIN", "/opt/apa-live/venv/bin/hermes"), envOr("APA_LIVE_HERMES_HOME", "/opt/apa-live/hermes-home")
	for _, p := range []string{bin, filepath.Join(tmpl, "config.yaml"), filepath.Join(tmpl, ".env")} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("live 节点需要真实 Hermes（见 /opt/apa-live/README.md；APA_LIVE_HERMES_BIN / APA_LIVE_HERMES_HOME）: %v", err)
		}
	}
	e := baseline.New(t)
	e.Init()
	for name, body := range pack {
		p := filepath.Join(e.Home, "packs", packName, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	e.Serve()
	f := &fx{Env: e, t: t, hermesBin: bin, hermesHome: filepath.Join(e.Root, "hermes-home"), hermesTmpl: tmpl, top: t}
	copyHermesHome(t, tmpl, f.hermesHome)
	e.IssueCredentials()
	e.CreateProjects()
	f.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"read","by_harness":{}}}`, 200)
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	return f
}

// declare 提交 hermes 声明并在 Web 确认；launch 为空表示不声明 main_session（默认 headless）。
func (f *fx) declare(launch string) {
	f.t.Helper()
	wrapper := filepath.Join(f.Root, "hermes-headless.sh")
	must(f.t, os.WriteFile(wrapper, []byte(fmt.Sprintf("#!/bin/sh\nexport HERMES_HOME=%q\nexec %q \"$@\"\n", f.hermesHome, f.hermesBin)), 0o755))
	d := map[string]any{"harness": harness, "headless": map[string]any{
		"command": []string{wrapper, "chat", "-Q", "--format", "stream-json", "-q", "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"-t", "file"}},
	}}
	if launch != "" {
		d["main_session"] = map[string]any{"launch": launch}
	}
	b, _ := json.Marshal(d)
	r := f.Apa(f.DEL, string(b), "declare", "-", "--evidence", "验收声明真实 Hermes")
	if r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	var out struct {
		Rev int64 `json:"rev"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil || out.Rev < 1 {
		f.t.Fatalf("apa declare 没有返回 rev: %q %v", r.Stdout, err)
	}
	f.webOK("POST", fmt.Sprintf("/settings/declarations/%s/%d/confirm", harness, out.Rev), "", 200)
}

func (f *fx) autoRule() {
	f.t.Helper()
	f.webOK("PATCH", "/rules/"+url.PathEscape(rule)+"/mode", `{"mode":"auto"}`, 200)
}

func (f *fx) task(title string) string { return f.add(title, "acc-wake") }

// add 建事项；tags 为空表示不带 acc-wake，规则不会命中它。
func (f *fx) add(title, tags string) string {
	f.t.Helper()
	r := f.apaOK(f.DEL, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--tags", tags, "--evidence", "验收触发")
	var m struct {
		ID string `json:"id"`
	}
	must(f.t, json.Unmarshal([]byte(r.Stdout), &m))
	if m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %s", r.Stdout)
	}
	return m.ID
}

// audit 返回对象上的审计事件（类型与原因），用于失败时说明内核为什么没有行动。
func (f *fx) audit(object string) string {
	var out struct {
		Events []struct{ Type, Reason string } `json:"events"`
	}
	must(f.t, json.Unmarshal([]byte(f.webOK("GET", "/audit/events?limit=100&object="+url.QueryEscape(object), "", 200)), &out))
	var parts []string
	for _, e := range out.Events {
		parts = append(parts, e.Type+"("+e.Reason+")")
	}
	return strings.Join(parts, "; ")
}

func (f *fx) step(name string, fn func(t *testing.T)) {
	parent := f.t
	ok := parent.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}

// logBuf 并发安全地收集网关输出，失败时用于说明 Hermes 为什么没有开回合。
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) { l.mu.Lock(); defer l.mu.Unlock(); return l.b.Write(p) }
func (l *logBuf) String() string              { l.mu.Lock(); defer l.mu.Unlock(); return l.b.String() }

func repoRoot() string {
	wd, err := os.Getwd() // tests/live/wake
	if err != nil {
		panic(err)
	}
	return filepath.Join(wd, "..", "..", "..")
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	defer l.Close()
	return fmt.Sprint(l.Addr().(*net.TCPAddr).Port)
}

const (
	route      = "wake-main"
	deliveryID = "apa-wake-live-main-session"
	firstWords = "回复一个词：好"
)

// sessionID 是被唤醒的真实主会话在网关会话库里的 key：webhook 平台以 (profile, 路由, 投递 id) 命名会话
// （webhook:v2:<base64url(JSON)>），网关只允许往这类已有会话里注入回合。
var sessionID = func() string {
	b, _ := json.Marshal([]string{"default", route, deliveryID})
	return "agent:main:webhook:webhook:webhook:v2:" + base64.RawURLEncoding.EncodeToString(b) + ":webhook:" + route
}()

// startGateway 启动带网关的真实 Hermes 主会话：api_server 平台 + apa-wake 插件（allow_gateway_injection），
// 插件与主会话同进程、持同一个 delegate token（DEL）。Hermes 自己的 HERMES_HOME 与无头运行分开，都在临时目录里。
// 网关平台需要 aiohttp；隔离安装里没有时，装到临时目录并经 PYTHONPATH 提供，不改动 /opt/apa-live。
func (f *fx) startGateway() {
	t := f.t
	t.Helper()
	home := filepath.Join(f.Root, "hermes-gateway-home")
	copyHermesHome(t, f.hermesTmpl, home)
	must(t, os.MkdirAll(filepath.Join(home, "plugins"), 0o700))
	must(t, os.Symlink(filepath.Join(repoRoot(), "plugins", "hermes", "wake"), filepath.Join(home, "plugins", "apa-wake")))
	port := freePort(t)
	cfg, err := os.OpenFile(filepath.Join(home, "config.yaml"), os.O_APPEND|os.O_WRONLY, 0o600)
	must(t, err)
	_, err = fmt.Fprintf(cfg, `
plugins:
  enabled:
    - apa-wake
  entries:
    apa-wake:
      allow_gateway_injection: true
platforms:
  webhook:
    enabled: true
    extra:
      host: 127.0.0.1
      port: %s
      routes:
        %s:
          secret: INSECURE_NO_AUTH
          prompt: "{message}"
          deliver: log
`, port, route)
	must(t, err)
	must(t, cfg.Close())

	py := filepath.Join(filepath.Dir(f.hermesBin), "python")
	pydeps := filepath.Join(f.Root, "pydeps")
	if exec.Command(py, "-c", "import aiohttp").Run() != nil {
		idx := os.Getenv("PIP_INDEX_URL")
		if idx == "" {
			idx = "https://pypi.tuna.tsinghua.edu.cn/simple" // taiyi 的全局 pip 源不通（/opt/apa-live/README.md）
		}
		c := exec.Command(py, "-m", "pip", "install", "-q", "--target", pydeps, "aiohttp")
		c.Env = append(os.Environ(), "PIP_INDEX_URL="+idx)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("安装网关依赖 aiohttp 失败: %v\n%s", err, out)
		}
	}

	f.gwLog = &logBuf{}
	c := exec.Command(f.hermesBin, "gateway", "run")
	c.Env = []string{ // 最小环境：不把 live 账号凭据交给网关进程；模型凭据在 HERMES_HOME/.env
		"PATH=" + os.Getenv("PATH"), "HOME=" + filepath.Join(f.Root, "gw-user"),
		"HERMES_HOME=" + home, "PYTHONPATH=" + pydeps,
		"APA_URL=" + f.URL, "APA_TOKEN=" + f.DEL, "APA_HOME=" + f.Home, "APA_BIN=" + f.Bin["apa"],
		"APA_WAKE_SESSION_KEY=" + sessionID,
	}
	c.Stdout, c.Stderr = f.gwLog, f.gwLog
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	must(t, c.Start())
	f.gw = c
	f.gwURL = "http://127.0.0.1:" + port
	f.top.Cleanup(func() { _ = syscall.Kill(-c.Process.Pid, syscall.SIGTERM); _ = c.Wait() })
	f.waitFor("网关 webhook 平台就绪", 120*time.Second, func() bool {
		r, err := http.Get(f.gwURL + "/health")
		if err != nil {
			return false
		}
		r.Body.Close()
		return r.StatusCode == 200
	})
	// 本人在这个主会话里说过一句话：网关只允许续接已有会话，不会凭空开会话（插件只能 continue，不能 open）。
	req, _ := http.NewRequest("POST", f.gwURL+"/webhooks/"+route, strings.NewReader(`{"message":"`+firstWords+`"}`))
	req.Header.Set("X-Request-ID", deliveryID)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: time.Minute}).Do(req)
	must(t, err)
	resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		t.Fatalf("在主会话里建立会话失败: HTTP %d\n%s", resp.StatusCode, f.gwLog.String())
	}
	f.waitFor("主会话回复了第一句话", 3*time.Minute, func() bool { return f.gwAssistantTurns() >= 1 })
}

// gwDB 只读打开网关的 state.db（Hermes 的会话库，journal_mode=DELETE），用来核对真实回合里发生了什么。
func (f *fx) gwDB() *sql.DB {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Root, "hermes-gateway-home", "state.db")+"?mode=ro")
	must(f.t, err)
	f.t.Cleanup(func() { db.Close() })
	return db
}

func (f *fx) gwAssistantTurns() int {
	var n int
	err := f.gwDB().QueryRow(`SELECT count(*) FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.session_key = ? AND m.role = 'assistant' AND coalesce(m.content, '') != ''`, sessionID).Scan(&n)
	if err != nil {
		return 0
	}
	return n
}

// call 是主会话里一次 apa_wake_cli 工具调用及其结果（Hermes 会话库里的记录，不是内核的说法）。
type call struct {
	Args     []string
	RunID    string
	ExitCode int
	Stdout   string
	Stderr   string
}

func (f *fx) gwCalls() []call {
	rows, err := f.gwDB().Query(`SELECT m.role, coalesce(m.tool_name, ''), coalesce(m.tool_calls, ''), coalesce(m.content, '')
		FROM messages m JOIN sessions s ON s.id = m.session_id WHERE s.session_key = ? ORDER BY m.id`, sessionID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	type ask struct {
		Args  []string `json:"args"`
		RunID string   `json:"run_id"`
	}
	var asked []ask
	var out []call
	for rows.Next() {
		var role, tool, tcs, content string
		must(f.t, rows.Scan(&role, &tool, &tcs, &content))
		switch role {
		case "assistant":
			var tc []struct {
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			}
			if json.Unmarshal([]byte(tcs), &tc) != nil {
				continue
			}
			for _, c := range tc {
				switch c.Function.Name {
				case "apa_wake_cli":
					var a ask
					if json.Unmarshal([]byte(c.Function.Arguments), &a) == nil {
						asked = append(asked, a)
					}
				case "tool_call": // Hermes 把多个工具调用包进一次 tool_call
					var w struct {
						Calls []struct {
							Name      string `json:"name"`
							Arguments ask    `json:"arguments"`
						} `json:"calls"`
					}
					if json.Unmarshal([]byte(c.Function.Arguments), &w) == nil {
						for _, x := range w.Calls {
							if x.Name == "apa_wake_cli" {
								asked = append(asked, x.Arguments)
							}
						}
					}
				}
			}
		case "tool":
			if tool != "apa_wake_cli" {
				continue
			}
			var r struct {
				ExitCode int    `json:"exit_code"`
				Stdout   string `json:"stdout"`
				Stderr   string `json:"stderr"`
			}
			must(f.t, json.Unmarshal([]byte(content), &r))
			c := call{ExitCode: r.ExitCode, Stdout: r.Stdout, Stderr: r.Stderr}
			if i := len(out); i < len(asked) { // 结果按调用顺序返回
				c.Args, c.RunID = asked[i].Args, asked[i].RunID
			}
			out = append(out, c)
		}
	}
	return out
}

const (
	srcID  = "src_wake"
	mAlice = "WAKE-ALICE-OK" // alice 在转发白名单内
	mBob   = "WAKE-BOB-OUT"  // bob 不在白名单内
)

// accScript 是一个脚本来源：输出队列目录里的事件，末行给游标。
const accScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

// attachedChild 造一个已委派并带 PR 的子事项，并让白名单内外各一条消息挂靠到它上面；返回子事项 id。
func (f *fx) attachedChild(parent string) string {
	f.t.Helper()
	q := filepath.Join(f.Root, "wake-source")
	must(f.t, os.MkdirAll(filepath.Join(q, "in"), 0o755))
	must(f.t, os.MkdirAll(filepath.Join(q, "done"), 0o755))
	script := filepath.Join(q, "src.sh")
	must(f.t, os.WriteFile(script, []byte(accScript), 0o755))
	body, _ := json.Marshal(map[string]any{"id": srcID, "type": "script", "source_type": "acc",
		"command": script + " " + q, "schedule": "1s", "timeout": 30})
	f.webOK("POST", "/sources", string(body), 201)
	f.webOK("PUT", "/settings/allowlists", `{"forward_allowlist":{"`+srcID+`":["alice"]}}`, 200)

	var d struct {
		ID string `json:"id"`
	}
	must(f.t, json.Unmarshal([]byte(f.apaOK(f.DEL, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", "wake-child", "--evidence", "验收：委派带 PR 的子事项").Stdout), &d))
	const pr = "https://example.invalid/acc/test-repo/pull/201"
	f.apaOK(f.WRK, "report", d.ID, "--outcome", "progress", "--summary", "已开 PR", "--pr", pr)
	ids := map[string]string{}
	for i, m := range []struct{ subject, sender string }{{mAlice, "alice"}, {mBob, "bob"}} {
		id := fmt.Sprintf("ev-%d", i+1)
		ids[id] = m.subject
		b, _ := json.Marshal(map[string]any{"id": id, "type": "comment.created", "subject": m.subject, "summary": "摘要 " + m.subject,
			"correlation_keys": []string{pr}, "sender": m.sender, "sender_verified": true, "occurred_at": time.Now().UTC().Format(time.RFC3339)})
		tmp := filepath.Join(q, fmt.Sprintf("tmp-%d", i))
		must(f.t, os.WriteFile(tmp, append(b, '\n'), 0o644))
		must(f.t, os.Rename(tmp, filepath.Join(q, "in", fmt.Sprintf("%04d.jsonl", i+1))))
	}
	f.waitFor("两条消息挂靠到子事项", 30*time.Second, func() bool {
		var out struct {
			Events []struct {
				DedupeKey struct {
					EventID string `json:"event_id"`
				} `json:"dedupe_key"`
				Disposition string `json:"disposition"`
				Item        string `json:"item"`
			} `json:"events"`
		}
		r := f.APIRaw("GET", "/v1/sources/"+srcID+"/events?limit=100", "", f.OBS)
		if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil {
			return false
		}
		n := 0
		for _, e := range out.Events {
			if _, ok := ids[e.DedupeKey.EventID]; ok && e.Disposition == "attached" && e.Item == d.ID {
				n++
			}
		}
		return n == len(ids)
	})
	return d.ID
}

type event struct {
	Seq    int    `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Actor  struct {
		Principal string `json:"principal"`
		Run       string `json:"run"`
	} `json:"actor"`
	Reason  string         `json:"reason"`
	Payload map[string]any `json:"payload"`
}

// events 返回账本里的全部审计事件（Web 看到的，不经任何过滤）。
func (f *fx) events() []event {
	f.t.Helper()
	var out struct {
		Events []event `json:"events"`
	}
	must(f.t, json.Unmarshal([]byte(f.webOK("GET", "/audit/events?limit=500", "", 200)), &out))
	return out.Events
}

// find 取第一条满足条件的审计事件。
func (f *fx) find(pred func(event) bool) *event {
	for _, e := range f.events() {
		if pred(e) {
			return &e
		}
	}
	return nil
}

func (f *fx) phase(id string) string {
	f.t.Helper()
	var out struct {
		Item struct {
			Phase string `json:"phase"`
		} `json:"item"`
	}
	must(f.t, json.Unmarshal([]byte(f.apaOK(f.OBS, "item", "show", id).Stdout), &out))
	return out.Item.Phase
}

func (f *fx) runState(id string) string {
	f.t.Helper()
	r := f.apaOK(f.OBS, "run", "show", id)
	var out struct {
		State string `json:"state"`
	}
	must(f.t, json.Unmarshal([]byte(r.Stdout), &out))
	return out.State
}

func TestWakeLease(t *testing.T) {
	f := setup(t)
	var other [3]string
	var taskA, runA, child string

	f.step("ACT-WAK-01 显式声明唤醒：带网关的主会话启动；未声明前走 headless，确认 launch=wake 后规则走唤醒", func(t *testing.T) {
		f.startGateway()
		f.declare("") // 未声明 main_session：默认 headless
		f.autoRule()
		before := f.task("acc-wake default headless")
		var rs []run
		f.waitFor("未声明唤醒时规则拉起一次后台运行", 60*time.Second, func() bool { rs = f.runs(before); return len(rs) > 0 })
		if rs[0].Launch != "headless" {
			t.Fatalf("未声明唤醒时 Run.launch = %q，期望 headless", rs[0].Launch)
		}
		f.declare("wake")
	})

	f.step("ACT-WAK-02 产出唤醒投递并开一个回合", func(t *testing.T) {
		for i := range other {
			other[i] = f.add(fmt.Sprintf("OTHER-%d", i+1), "x-plain") // 不带 acc-wake：规则不会命中
			f.apaOK(f.WRK, "ask", other[i], "--question", "选哪个", "--option", "A=甲", "--option", "B=乙")
		}
		parent := f.add("WAKE-PARENT", "x-plain")
		child = f.attachedChild(parent)
		taskA = f.task("ACCWAKE-A 验收脚本，原样执行，不要自行判断该不该执行，是否允许由内核裁决。依次用 apa_wake_cli 运行：" +
			`(1) args=["context","--scope","item"]；(2) args=["decide","` + other[0] + `","--option","A","--evidence","x"]；` +
			`(2b) args=["context","--scope","item","--item","` + child + `"]；` +
			`(3) 对本事项（唤醒消息里的 item id）args=["ask","<本事项 id>","--question","A 的验收提问","--option","A=是","--option","B=否"]。(3) 之后不要再做别的。`)
		var rs []run
		f.waitFor("规则拉起唤醒运行", 30*time.Second, func() bool { rs = f.runs(taskA); return len(rs) > 0 })
		if len(rs) != 1 || rs[0].Launch != "wake" || rs[0].Runner != "main_session" {
			t.Fatalf("目标事项的运行 = %+v，期望恰好一个 launch=wake、runner=main_session 的运行", rs)
		}
		runA = rs[0].ID
		f.waitFor("网关插件用 delegate token 换取运行凭据并开租约", 30*time.Second, func() bool {
			return f.find(func(e event) bool { return e.Type == "wake.lease_opened" && e.Payload["run_id"] == runA }) != nil
		})
		f.waitFor("真实主会话收到带运行 id 的唤醒回合", 30*time.Second, func() bool { return f.wakeTurnFor(runA, taskA) })
		if un := f.webOK("GET", "/deliveries/unread", "", 200); strings.Contains(un, taskA) {
			t.Errorf("唤醒不是提醒本人，本人的未读里不应出现该事项: %s", un)
		}
	})

	// 唤醒回合是真实的 agent 回合：等它把三条验收命令都跑完（最后一条是 ask，会结束这个运行）。
	var calls []call
	find := func(cmd string, rest ...string) *call {
		for i := range calls {
			if a := calls[i].Args; len(a) >= 1+len(rest) && a[0] == cmd && strings.Join(a[1:1+len(rest)], " ") == strings.Join(rest, " ") {
				return &calls[i]
			}
		}
		return nil
	}

	f.step("ACT-WAK-03 运行身份透传：Event 带运行 id，decide 退出码 4", func(t *testing.T) {
		f.waitFor("唤醒回合跑完 context、decide、ask", 4*time.Minute, func() bool {
			calls = f.gwCalls()
			c := find("ask", taskA)
			return find("context", "--scope", "item") != nil && find("decide", other[0]) != nil && c != nil && find("context", "--scope", "item", "--item", child) != nil
		})
		for _, c := range calls {
			if c.RunID != runA {
				t.Fatalf("回合里的 apa 调用必须带这次唤醒的运行 id %s，实际 %q（%v）", runA, c.RunID, c.Args)
			}
		}
		if c := find("context", "--scope", "item"); c.ExitCode != 0 || !strings.Contains(c.Stdout, taskA) {
			t.Errorf("apa context --scope item 在运行身份下应返回目标事项 %s：退出码 %d，输出 %s %s", taskA, c.ExitCode, c.Stdout, c.Stderr)
		}
		if c := find("decide", other[0]); c.ExitCode != 4 {
			t.Errorf("唤醒回合里 apa decide 退出码 = %d，期望 4（stderr: %s）", c.ExitCode, c.Stderr)
		}
		// 挂靠消息按转发白名单过滤：白名单内的出现，白名单外的不出现（取自 Hermes 会话库里的工具结果）。
		cc := find("context", "--scope", "item", "--item", child)
		if cc == nil || cc.ExitCode != 0 {
			t.Fatalf("唤醒回合应读到子事项 %s 的 context：%+v", child, cc)
		}
		if !strings.Contains(cc.Stdout, mAlice) || strings.Contains(cc.Stdout, mBob) {
			t.Errorf("运行身份应只看到白名单内的挂靠消息（含 %s、不含 %s）：%s", mAlice, mBob, cc.Stdout)
		}
		if all := f.webOK("GET", "/items/"+child+"/messages", "", 200); !strings.Contains(all, mBob) {
			t.Errorf("白名单外的消息本人在 Web 仍应可见: %s", all)
		}
		rej := f.find(func(e event) bool { return e.Type == "command.rejected" && e.Object == other[0] })
		if rej == nil || rej.Actor.Run != runA || rej.Payload["exit"] != float64(4) || rej.Payload["action"] != "answer_decision" {
			t.Fatalf("内核应记下按运行身份拒绝的代答：%+v", rej)
		}
		if ph := f.phase(other[0]); ph != "needs_input" {
			t.Errorf("被拒绝的决策不应被回答，事项阶段 = %s", ph)
		}
		ex := f.find(func(e event) bool { return e.Type == "run.credential_exchanged" && e.Payload["run_id"] == runA })
		if ex == nil {
			t.Fatal("内核应记下运行凭据的换取")
		}
		if b, _ := json.Marshal(ex); strings.Contains(string(b), "apa_run_") {
			t.Errorf("换取记录不应含凭据明文: %s", b)
		}
	})

	var runB string
	f.step("ACT-WAK-04 唤醒租约内 delegate 不能代答，回执后恢复", func(t *testing.T) {
		// 另开一个唤醒运行，让它保持未结束：回合里什么都不做，也不 ask。
		taskB := f.task("ACCWAKE-B 验收脚本：本回合什么都不要做，不要运行任何 apa 命令，不要 ask，只回复 ok。")
		var rs []run
		f.waitFor("规则拉起第二个唤醒运行", 30*time.Second, func() bool { rs = f.runs(taskB); return len(rs) > 0 })
		runB = rs[0].ID
		f.waitFor("第二个运行的租约开启", 30*time.Second, func() bool {
			return f.find(func(e event) bool { return e.Type == "wake.lease_opened" && e.Payload["run_id"] == runB }) != nil
		})
		if st := f.runState(runB); st != "running" {
			t.Fatalf("运行 %s 应仍未结束，实际 %s", runB, st)
		}
		// 另一个持同一 DEL 的会话：这里就是测试进程用同一个 token 运行 apa。
		r := f.Apa(f.DEL, "", "decide", other[1], "--option", "A", "--evidence", "x")
		if r.Code != 4 {
			t.Fatalf("租约期间持同一 DEL 的会话 apa decide 退出码 = %d，期望 4\nstdout: %s\nstderr: %s", r.Code, r.Stdout, r.Stderr)
		}
		if ph := f.phase(other[1]); ph != "needs_input" {
			t.Fatalf("被拒绝的代答不应改变事项，阶段 = %s", ph)
		}
		// 本人在 Web 仍可决定同一件事。
		f.webOK("POST", "/items/"+other[1]+"/decide", `{"choice":"A"}`, 200)
		if ph := f.phase(other[1]); ph == "needs_input" {
			t.Errorf("本人在 Web 的决定应生效，事项仍是 needs_input")
		}
		// 回执：目标事项离开 open（本人在 Web 收起它）。运行按成功结束，租约释放。
		f.webOK("POST", "/items/"+taskB+"/transition", `{"to":"done","reason":"验收：回执"}`, 200)
		f.waitFor("回执后运行按成功结束", 30*time.Second, func() bool { return f.runState(runB) == "succeeded" })
		f.waitFor("租约释放", 30*time.Second, func() bool {
			return f.find(func(e event) bool { return e.Type == "wake.lease_released" && e.Payload["run_id"] == runB }) != nil
		})
		r = f.Apa(f.DEL, "", "decide", other[2], "--option", "A", "--evidence", "x")
		if r.Code != 0 {
			t.Fatalf("回执后 DEL 应恢复代答，apa decide 退出码 = %d\nstdout: %s\nstderr: %s", r.Code, r.Stdout, r.Stderr)
		}
	})

	f.step("ACT-WAK-05 需要本人时转 needs_input，不直接打扰", func(t *testing.T) {
		c := find("ask", taskA)
		if c.ExitCode != 0 {
			t.Fatalf("唤醒回合里 apa ask 退出码 = %d（stderr: %s）", c.ExitCode, c.Stderr)
		}
		if ph := f.phase(taskA); ph != "needs_input" {
			t.Fatalf("ask 后目标事项阶段 = %s，期望 needs_input", ph)
		}
		req := f.find(func(e event) bool { return e.Type == "decision.requested" && e.Object == taskA })
		if req == nil || req.Actor.Run != runA {
			t.Fatalf("决策请求应以运行身份写入：%+v", req)
		}
		if st := f.runState(runA); st != "succeeded" {
			t.Errorf("ask 是回执，运行 %s 应按成功结束，实际 %s", runA, st)
		}
		if f.find(func(e event) bool { return e.Type == "wake.lease_released" && e.Payload["run_id"] == runA }) == nil {
			t.Error("回执后租约应释放")
		}
		// 触达接手：决策投递排在本人的队列里（按打扰策略送达），唤醒回合自己没有向任何人发消息。
		ctx := f.apaOK(f.DEL, "context", "--scope", "all").Stdout
		if !strings.Contains(ctx, "decision:"+taskA) {
			t.Errorf("ask 之后应有该事项的决策投递交给触达排队：%s", ctx)
		}
		for _, c := range calls {
			if len(c.Args) > 0 && (c.Args[0] == "decide" || c.Args[0] == "accept" || c.Args[0] == "dismiss") && c.ExitCode == 0 {
				t.Errorf("唤醒回合不应有成功的代答类命令: %v", c.Args)
			}
		}
	})
}

// wakeTurnFor 判断网关的主会话里是否有一条形如 [APA wake] 的用户消息，带这个运行 id 与目标事项（插件领取投递后注入的回合）。
func (f *fx) wakeTurnFor(runID, item string) bool {
	var n int
	err := f.gwDB().QueryRow(`SELECT count(*) FROM messages m JOIN sessions s ON s.id = m.session_id
		WHERE s.session_key = ? AND m.role = 'user' AND m.content LIKE '[APA wake]%' AND m.content LIKE ? AND m.content LIKE ?`,
		sessionID, "%"+runID+"%", "%"+item+"%").Scan(&n)
	return err == nil && n > 0
}
