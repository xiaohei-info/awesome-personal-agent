//go:build live

// Package telegram 按验收文档 flow-channel-telegram（ACT-TG-01..06）用真实 apa 二进制、真实 Telegram ext 和真实
// Telegram Bot API（专用测试 Bot 与本人测试会话）走完整流程。本节点不接任何 Hermes。
//
// 需要本人在 Telegram 里点选项（ACT-TG-03）和回复"同意"（ACT-TG-04）：卡片的标题里写明了要做什么，测试最多等 waitHuman；
// 不会用 Bot 伪造回复。Bot token 只从 APA_LIVE_TELEGRAM_BOT_TOKEN_FILE 读取，只放进 ext 子进程的环境，不进入输出。
//
// ext 与 Telegram 之间经一个本机反向代理（只计数/记录 sendMessage，原样转发到真实 Bot API），ext 与内核之间经另一个
// 代理（ACT-TG-06 用它卡住 ack，以便在"已发送、未回报"的窗口 kill -9）。代理不改变任何内容。
package telegram

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	_ "modernc.org/sqlite"
)

const (
	channel   = "acc-chan"
	waitHuman = 30 * time.Minute
	waitKern  = 3 * time.Minute
)

func repoRoot() string { d, _ := filepath.Abs(filepath.Join("..", "..", "..")); return d }

// liveEnv 先看进程环境，没有就读 APA_LIVE_ENV_FILE（KEY=VALUE）。
func liveEnv(t *testing.T, k string) string {
	t.Helper()
	if v := os.Getenv(k); v != "" {
		return v
	}
	if f := os.Getenv("APA_LIVE_ENV_FILE"); f != "" {
		if fh, err := os.Open(f); err == nil {
			defer fh.Close()
			sc := bufio.NewScanner(fh)
			for sc.Scan() {
				if key, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), "="); ok && key == k {
					return strings.Trim(v, `"'`)
				}
			}
		}
	}
	t.Fatalf("缺少 %s（设置 APA_LIVE_ENV_FILE 指向 live.env）", k)
	return ""
}

// sentLog 记录经代理发往 Telegram 的 sendMessage 文本（不含 token）。
type sentLog struct {
	mu    sync.Mutex
	texts []string
}

func (s *sentLog) add(x string) { s.mu.Lock(); s.texts = append(s.texts, x); s.mu.Unlock() }
func (s *sentLog) all() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.texts...)
}
func (s *sentLog) count(sub string) (n int) {
	for _, x := range s.all() {
		if strings.Contains(x, sub) {
			n++
		}
	}
	return
}

func serveProxy(t *testing.T, target string, mod func(*http.Request, []byte) bool) string {
	t.Helper()
	u, _ := url.Parse(target)
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.Host = u.Host
		if mod != nil && !mod(r, b) {
			<-r.Context().Done() // 卡住直到调用方断开
			return
		}
		rp.ServeHTTP(w, r)
	})
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return "http://" + l.Addr().String()
}

// textOf 取 sendMessage 请求里的 text（SDK 用 multipart 表单，也兼容 JSON）。
func textOf(ct string, b []byte) string {
	if mt, params, err := mime.ParseMediaType(ct); err == nil && strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(bytes.NewReader(b), params["boundary"])
		for {
			p, err := mr.NextPart()
			if err != nil {
				return ""
			}
			if p.FormName() == "text" {
				v, _ := io.ReadAll(p)
				return string(v)
			}
		}
	}
	var m struct {
		Text string `json:"text"`
	}
	_ = json.Unmarshal(b, &m)
	return m.Text
}

type fx struct {
	*baseline.Env
	t      *testing.T
	sent   *sentLog
	token  string
	chat   string
	holdMu sync.Mutex
	hold   bool // 为真时卡住 ext → 内核的 ack
	acks   chan string
	extBin string
	extDir string
	state  string
	apaURL string // ext 使用的内核代理地址
	tgAPI  string
	ext    *exec.Cmd
	extOut *syncBuf
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func (f *fx) scrub(s string) string { return strings.ReplaceAll(s, f.token, "<bot-token>") }

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) task(title string, tags ...string) string {
	f.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建个任务"}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.DEL, 0, args...).Stdout), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %v", err)
	}
	return m.ID
}

func (f *fx) ask(id, question string) {
	f.t.Helper()
	f.apa(f.WRK, 0, "ask", id, "--question", question, "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A")
}

func (f *fx) item(id string) map[string]any {
	f.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Item
}

func (f *fx) resolution(id string) map[string]any {
	a, _ := f.item(id)["ask"].(map[string]any)
	r, _ := a["resolution"].(map[string]any)
	return r
}

type dlv struct {
	ID, State, Ref, Err string
	Claims              int
}

// deliveries 只读打开内核账本，列出事项的投递（投递状态与 external_ref 没有对外读取接口）。
func (f *fx) deliveries(item string) []dlv {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Home, "ledger.db")+"?mode=ro")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, state, external_ref, last_error, claims FROM delivery_delivery WHERE item = ? ORDER BY created_at, rowid`, item)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []dlv
	for rows.Next() {
		var d dlv
		if err := rows.Scan(&d.ID, &d.State, &d.Ref, &d.Err, &d.Claims); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func (f *fx) until(limit time.Duration, what string, cond func() bool) {
	f.t.Helper()
	for end := time.Now().Add(limit); !cond(); time.Sleep(time.Second) {
		if time.Now().After(end) {
			f.t.Fatalf("%v 内没有等到：%s\next 输出: %s", limit, what, f.scrub(f.extOut.String()))
		}
	}
}

// startExt 用 `apa ext run telegram` 启动 ext，配置只经环境变量。
func (f *fx) startExt() {
	f.t.Helper()
	f.extOut = &syncBuf{}
	cmd := exec.Command(f.Bin["apa"], "ext", "run", "telegram")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "APA_HOME=" + f.Home, "APA_EXT_DIR=" + f.extDir,
		"APA_URL=" + f.apaURL, "APA_EXT_KEY=" + f.CHAN, "APA_WEB_URL=" + f.URL, "TG_BOT=" + f.token, "TG_CHAT=" + f.chat,
		"TG_API=" + f.tgAPI, "APA_EXT_STATE=" + f.state}
	cmd.Stdout, cmd.Stderr = f.extOut, f.extOut
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.ext = cmd
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	f.t.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	time.Sleep(3 * time.Second)
	select {
	case <-done:
		f.t.Fatalf("ext 启动后退出: %s", f.scrub(f.extOut.String()))
	default:
	}
}

// killExt 对 ext 进程（apa ext run 的子进程）发 SIGKILL，再停掉 apa ext run 本身。
func (f *fx) killExt() {
	f.t.Helper()
	out, _ := exec.Command("pgrep", "-P", strconv.Itoa(f.ext.Process.Pid)).Output()
	pids := strings.Fields(string(out))
	if len(pids) != 1 {
		f.t.Fatalf("找不到 ext 子进程: %q", out)
	}
	if err := exec.Command("kill", "-9", pids[0]).Run(); err != nil {
		f.t.Fatal(err)
	}
	_ = f.ext.Wait()
}

func TestFlowChannelTelegram(t *testing.T) {
	tokB, err := os.ReadFile(liveEnv(t, "APA_LIVE_TELEGRAM_BOT_TOKEN_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fx{t: t, token: strings.TrimSpace(string(tokB)), chat: liveEnv(t, "APA_LIVE_TELEGRAM_CHAT_ID"), sent: &sentLog{}, acks: make(chan string, 8)}
	botName := liveEnv(t, "APA_LIVE_TELEGRAM_BOT")

	// 回答人白名单：本人在 Telegram 里的用户名（没有用户名时是数字 id）。getChat 只读，用来查它。
	resp, err := http.Get("https://api.telegram.org/bot" + f.token + "/getChat?chat_id=" + f.chat)
	if err != nil {
		t.Fatalf("getChat 请求失败（网络）")
	}
	var chat struct {
		OK     bool `json:"ok"`
		Result struct {
			Username string `json:"username"`
		} `json:"result"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&chat)
	resp.Body.Close()
	if !chat.OK {
		t.Fatalf("getChat 失败 HTTP %d：Bot 与本人会话还没有建立过对话？", resp.StatusCode)
	}
	answerer := chat.Result.Username
	if answerer == "" {
		answerer = f.chat
	}

	f.Env = baseline.New(t)
	f.StartKernel()
	f.IssueCredentials()
	f.CreateProjects()

	// 代理：Telegram（记录 sendMessage 文本）与内核（可卡住 ack）。
	f.tgAPI = serveProxy(t, "https://api.telegram.org", func(r *http.Request, b []byte) bool {
		if strings.HasSuffix(r.URL.Path, "/sendMessage") {
			f.sent.add(textOf(r.Header.Get("Content-Type"), b))
		}
		return true
	})
	f.apaURL = serveProxy(t, f.URL, func(r *http.Request, b []byte) bool {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/ack") {
			f.holdMu.Lock()
			h := f.hold
			f.holdMu.Unlock()
			if h {
				select {
				case f.acks <- r.URL.Path:
				default:
				}
				return false
			}
		}
		return true
	})

	// 构建 ext 并写 ext.yaml（command 指向二进制，目录由 APA_EXT_DIR 指定）。
	root := t.TempDir()
	f.extBin = filepath.Join(root, "telegram-ext")
	cmd := exec.Command("go", "build", "-o", f.extBin, "./ext/telegram/cmd/telegram-ext")
	cmd.Dir = repoRoot()
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("构建 ext: %v\n%s", err, out)
	}
	f.extDir = filepath.Join(root, "ext")
	if err := os.MkdirAll(filepath.Join(f.extDir, "telegram"), 0o755); err != nil {
		t.Fatal(err)
	}
	yml := "kind: channel\ndescription: Telegram 渠道 ext\ncommand: [" + f.extBin + "]\n"
	if err := os.WriteFile(filepath.Join(f.extDir, "telegram", "ext.yaml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	f.state = filepath.Join(root, "ext-telegram.json")

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}

	var item, d1 string
	step("ACT-TG-01 启动 Telegram 渠道 ext，不依赖 Hermes", func(t *testing.T) {
		ls := exec.Command(f.Bin["apa"], "ext", "list")
		ls.Env = append(os.Environ(), "APA_EXT_DIR="+f.extDir)
		out, err := ls.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "telegram") || !strings.Contains(string(out), "channel") {
			t.Fatalf("apa ext list 没有列出 telegram 渠道 ext: %v %s", err, out)
		}
		// 决策路由到该渠道；回答人白名单在 Web 设置。
		b, _ := json.Marshal(map[string]any{"disturb": map[string]any{
			"routes": map[string]string{"decision": channel, "reminder": "web", "brief": "web", "health": "web", "wake": "web"}},
			"web_only": map[string]any{"categories": []string{"external_commitment", "funds", "irreversible", "permission_change", "spec_activation"},
				"rules": []map[string]string{{"tag": "acc-finance", "category": "funds"}}}})
		f.webOK("PUT", "/settings/policy", string(b), 200)
		b, _ = json.Marshal(map[string]any{"channel_answerers": map[string][]string{channel: {answerer}}})
		f.webOK("PUT", "/settings/allowlists", string(b), 200)
		f.startExt()
		if strings.Contains(strings.ToLower(f.extOut.String()), "hermes") || strings.Contains(strings.ToLower(yml), "hermes") {
			t.Error("ext 的配置或输出里出现 Hermes")
		}
	})

	step("ACT-TG-02 决策请求送达 Telegram", func(t *testing.T) {
		item = f.task("[APA-LIVE-TEST] telegram 决策")
		f.ask(item, "[APA-LIVE-TEST] 选哪个方案？请在 Telegram 点选项 A（验收步骤 ACT-TG-03）")
		f.until(waitKern, "投递 state=delivered 且有 external_ref", func() bool {
			ds := f.deliveries(item)
			return len(ds) > 0 && ds[0].State == "delivered" && ds[0].Ref != ""
		})
		ds := f.deliveries(item)
		d1 = ds[0].ID
		if !strings.HasPrefix(ds[0].Ref, f.chat+":") {
			t.Errorf("external_ref 应形如 <会话>:<消息id>: %q", ds[0].Ref)
		}
		var msg string
		for _, x := range f.sent.all() {
			if strings.Contains(x, "请在 Telegram 点选项 A") {
				msg = x
			}
		}
		for _, want := range []string{"选哪个方案", "A: 方案A", "B: 方案B", "推荐：A"} {
			if !strings.Contains(msg, want) {
				t.Errorf("Telegram 消息缺 %q:\n%s", want, msg)
			}
		}
		t.Logf("delivery=%s state=%s external_ref=%s bot=%s", ds[0].ID, ds[0].State, ds[0].Ref, botName)
	})

	step("ACT-TG-03 本人在 Telegram 回答（请在 Telegram 点选项 A）", func(t *testing.T) {
		t.Logf("等待本人在 Telegram（@%s）点选项 A，最多 %v", botName, waitHuman)
		f.until(waitHuman, "本人在 Telegram 回答", func() bool { return f.resolution(item) != nil })
		r := f.resolution(item)
		if r["resolved_via"] != channel || r["choice"] != "A" || r["answered_by"] != answerer {
			t.Errorf("回答记录不符: %v（期望 resolved_via=%s choice=A answered_by=%s）", r, channel, answerer)
		}
		if p := f.item(item)["phase"]; p == "needs_input" {
			t.Errorf("事项应继续，仍是 %v", p)
		}
		// 回答人在 Event 载荷里，observer 凭据会裁掉；本人（Web）可见完整时间线。
		if why := f.webOK("GET", "/why/"+item, "", 200); !strings.Contains(why, channel) || !strings.Contains(why, answerer) {
			t.Errorf("apa why 查不到渠道与回答人: %s", why)
		}
		if ds := f.deliveries(item); len(ds) == 0 || ds[0].ID != d1 || ds[0].State != "acknowledged" {
			t.Errorf("回答后投递应为 acknowledged: %+v", ds)
		}
	})

	step("ACT-TG-04 web_only 只给 Web 链接（请在 Telegram 回复「同意」）", func(t *testing.T) {
		fin := f.task("[APA-LIVE-TEST] telegram web_only", "acc-finance")
		f.ask(fin, "[APA-LIVE-TEST] 付款？请回复这条消息：同意（验收步骤 ACT-TG-04）")
		f.until(waitKern, "web_only 投递 delivered", func() bool {
			ds := f.deliveries(fin)
			return len(ds) > 0 && ds[0].State == "delivered" && ds[0].Ref != ""
		})
		var msg string
		for _, x := range f.sent.all() {
			if strings.Contains(x, "请回复这条消息：同意") {
				msg = x
			}
		}
		if !strings.Contains(msg, "/items/"+fin) || strings.Contains(msg, "选项：") || strings.Contains(msg, "作答链接") || strings.Contains(msg, "/forms/") {
			t.Errorf("web_only 消息应只带 Web 链接，不带选项/表单链接:\n%s", msg)
		}
		t.Logf("等待本人在 Telegram 回复「同意」，最多 %v", waitHuman)
		f.until(waitHuman, "ext 对本人的回复给出 Web 链接", func() bool { return f.sent.count("不能在 Telegram 回答") > 0 })
		var note string
		for _, x := range f.sent.all() {
			if strings.Contains(x, "不能在 Telegram 回答") {
				note = x
			}
		}
		if !strings.Contains(note, "/items/"+fin) {
			t.Errorf("拒绝回复应带 Web 链接:\n%s", note)
		}
		if p := f.item(fin)["phase"]; p != "needs_input" || f.resolution(fin) != nil {
			t.Errorf("决策不应生效: phase=%v resolution=%v", p, f.resolution(fin))
		}
	})

	step("ACT-TG-05 ext 不用 harness 网关，Bot 独立配置", func(t *testing.T) {
		cmd := exec.Command("go", "list", "-deps", "./ext/telegram/...")
		cmd.Dir = repoRoot()
		deps, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("go list: %v %s", err, deps)
		}
		for _, p := range strings.Fields(string(deps)) {
			if strings.Contains(p, "awesome-personal-agent/plugins") || strings.Contains(strings.ToLower(p), "hermes") {
				t.Errorf("ext 依赖了 harness 代码: %s", p)
			}
		}
		rl := exec.Command("go", "run", "./tools/redline")
		rl.Dir = repoRoot()
		if out, err := rl.CombinedOutput(); err != nil {
			t.Errorf("红线检查未通过: %v %s", err, out)
		}
		// Bot 只由 TG_BOT 指定：ext 进程环境里除它之外没有任何 Bot 配置，测试 Bot 是专用的 @<bot>。
		if botName == "" || strings.Contains(botName, "hermes") {
			t.Errorf("测试 Bot 应是专用 Bot: %q", botName)
		}
		t.Logf("ext 的依赖闭包里没有 plugins/ 或 hermes；红线检查通过；本次独立 Bot=@%s", botName)
	})

	step("ACT-TG-06 ext 崩溃重启后不重复发送", func(t *testing.T) {
		f.holdMu.Lock()
		f.hold = true
		f.holdMu.Unlock()
		crash := f.task("[APA-LIVE-TEST] telegram 崩溃重启")
		f.ask(crash, "[APA-LIVE-TEST] 崩溃重启验收，无需回答（ACT-TG-06）")
		select {
		case <-f.acks:
		case <-time.After(waitKern):
			t.Fatalf("ext 没有走到回报 ack: %s", f.scrub(f.extOut.String()))
		}
		if n := f.sent.count("崩溃重启验收"); n != 1 {
			t.Fatalf("kill 前应已发出 1 条: %d", n)
		}
		f.killExt()
		f.holdMu.Lock()
		f.hold = false
		f.holdMu.Unlock()
		if ds := f.deliveries(crash); len(ds) != 1 || ds[0].State == "delivered" {
			t.Fatalf("kill 时投递不应已回报: %+v", ds)
		}
		f.startExt()
		f.until(3*time.Minute, "租约到期后重新领取并回报 delivered", func() bool {
			ds := f.deliveries(crash)
			return len(ds) == 1 && ds[0].State == "delivered" && ds[0].Ref != ""
		})
		time.Sleep(10 * time.Second)
		if n := f.sent.count("崩溃重启验收"); n != 1 {
			t.Errorf("重启后本人收到 %d 条，应只有 1 条", n)
		}
		ds := f.deliveries(crash)
		t.Logf("delivery=%s state=%s external_ref=%s claims=%d", ds[0].ID, ds[0].State, ds[0].Ref, ds[0].Claims)
		if ds[0].Claims < 2 {
			t.Errorf("应被重新领取（claims=%d）", ds[0].Claims)
		}
	})
}
