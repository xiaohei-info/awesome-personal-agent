//go:build e2e

package channel

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"io"
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

	"github.com/go-telegram/bot/models"
	_ "modernc.org/sqlite"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 按验收文档 flow-channel-telegram（机器人替身版，ACT-TG-01..06）用真实 apa 二进制、真实 Telegram ext 二进制
// 与仓库内的机器人替身 fakebot 走完整流程。不启动任何 harness（没有 STUB_HARNESS，也没有宿主插件）。
// 真实 Telegram 账号的送达与回复由本机升级验证 L9–L12 承接。

const (
	tgChannel  = "acc-chan"
	tgUser     = "acc-user" // 机器人替身里的账号（回答人白名单里的用户名）
	tgChatID   = int64(1)
	tgKernWait = 90 * time.Second
)

var tgAccount = models.User{ID: 7, Username: tgUser}

type tg struct {
	*baseline.Env
	t      *testing.T
	root   *testing.T // 顶层测试：ext 进程的清理挂在它上面，不随单个动作的子测试结束
	bot    *fakebot.Bot
	extDir string
	state  string
	apaURL string // ext 使用的内核代理地址（ACT-TG-06 用它卡住 ack）
	ext    *exec.Cmd
	extOut *syncBuffer

	holdMu sync.Mutex
	hold   bool
	acks   chan string
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// serveProxy 在本机回环起一个反向代理；mod 返回 false 时卡住该请求直到调用方断开。代理不改任何内容。
func serveProxy(t *testing.T, target string, mod func(*http.Request) bool) string {
	t.Helper()
	u, _ := url.Parse(target)
	rp := httputil.NewSingleHostReverseProxy(u)
	rp.FlushInterval = -1
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(b))
		r.Host = u.Host
		if !mod(r) {
			<-r.Context().Done()
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

func (f *tg) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *tg) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *tg) task(title string, tags ...string) string {
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

// ask 用 `apa ask` 提交两个选项和一个推荐的普通决策。
func (f *tg) ask(id, question string) {
	f.t.Helper()
	f.apa(f.WRK, 0, "ask", id, "--question", question, "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A")
}

type tgItem struct {
	Phase string `json:"phase"`
	Ask   struct {
		Resolution *struct {
			ResolvedVia string `json:"resolved_via"`
			Choice      string `json:"choice"`
			AnsweredBy  string `json:"answered_by"`
		} `json:"resolution"`
	} `json:"ask"`
}

func (f *tg) item(id string) tgItem {
	f.t.Helper()
	var out struct {
		Item tgItem `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Item
}

type tgDelivery struct {
	ID, State, Ref string
	Claims         int
}

// deliveries 只读打开内核账本，列出事项的投递（投递状态与 external_ref 没有对外读取接口）。
func (f *tg) deliveries(item string) []tgDelivery {
	f.t.Helper()
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Home, "ledger.db")+"?mode=ro")
	if err != nil {
		f.t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.Query(`SELECT id, state, external_ref, claims FROM delivery_delivery WHERE item = ? ORDER BY created_at, rowid`, item)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []tgDelivery
	for rows.Next() {
		var d tgDelivery
		if err := rows.Scan(&d.ID, &d.State, &d.Ref, &d.Claims); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, d)
	}
	return out
}

func (f *tg) until(limit time.Duration, what string, cond func() bool) {
	f.t.Helper()
	for end := time.Now().Add(limit); !cond(); time.Sleep(200 * time.Millisecond) {
		if time.Now().After(end) {
			f.t.Fatalf("%v 内没有等到：%s\next 输出: %s\n机器人替身收到: %+v\n服务日志: %s", limit, what, f.extOut.String(), f.bot.Sent(), f.ServiceLog())
		}
	}
}

// sentWith 是机器人替身收到的、文本含 sub 的消息。
func (f *tg) sentWith(sub string) (out []fakebot.Sent) {
	for _, m := range f.bot.Sent() {
		if strings.Contains(m.Text, sub) {
			out = append(out, m)
		}
	}
	return
}

// startExt 用 `apa ext run telegram` 启动 ext；配置只经环境变量，连机器人替身 fakebot。
func (f *tg) startExt() {
	f.t.Helper()
	f.extOut = &syncBuffer{}
	cmd := exec.Command(f.Bin["apa"], "ext", "run", "telegram")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "APA_HOME=" + f.Home, "APA_EXT_DIR=" + f.extDir,
		"APA_URL=" + f.apaURL, "APA_EXT_KEY=" + f.CHAN, "APA_WEB_URL=" + f.URL, "TG_BOT=1:acc", "TG_CHAT=" + strconv.FormatInt(tgChatID, 10),
		"TG_API=" + f.bot.URL, "APA_EXT_STATE=" + f.state}
	cmd.Stdout, cmd.Stderr = f.extOut, f.extOut
	if err := cmd.Start(); err != nil {
		f.t.Fatal(err)
	}
	f.ext = cmd
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	f.root.Cleanup(func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	time.Sleep(2 * time.Second)
	select {
	case <-done:
		f.t.Fatalf("ext 启动后退出: %s", f.extOut.String())
	default:
	}
}

// killExt 对 ext 进程（`apa ext run` 的子进程）发 SIGKILL，再回收 `apa ext run` 本身。
func (f *tg) killExt() {
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

func newTg(t *testing.T) *tg {
	t.Helper()
	bot, err := fakebot.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bot.Close)
	f := &tg{Env: baseline.Setup(t), t: t, root: t, bot: bot, acks: make(chan string, 8)}
	f.apaURL = serveProxy(t, f.URL, func(r *http.Request) bool {
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
	f.state = filepath.Join(f.Root, "ext-telegram.json")
	f.extDir = filepath.Join(f.Root, "ext")
	if err := os.MkdirAll(filepath.Join(f.extDir, "telegram"), 0o755); err != nil {
		t.Fatal(err)
	}
	yml := "kind: channel\ndescription: Telegram 渠道 ext\ncommand: [" + f.Bin["telegram-ext"] + "]\n"
	if err := os.WriteFile(filepath.Join(f.extDir, "telegram", "ext.yaml"), []byte(yml), 0o644); err != nil {
		t.Fatal(err)
	}
	return f
}

// TestFlowChannelTelegram 对应验收文档 flow-channel-telegram（机器人替身版）：ACT-TG-02..06；ACT-TG-01 是准备步骤。
func TestFlowChannelTelegram(t *testing.T) {
	f := newTg(t)
	step := func(name string, fn func(t *testing.T)) {
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

	var item, d1 string
	step("ACT-TG-01 启动渠道 ext，不依赖任何 harness", func(t *testing.T) {
		ls := exec.Command(f.Bin["apa"], "ext", "list")
		ls.Env = append(os.Environ(), "APA_EXT_DIR="+f.extDir)
		out, err := ls.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "telegram") || !strings.Contains(string(out), "channel") {
			t.Fatalf("apa ext list 没有列出 telegram 渠道 ext: %v %s", err, out)
		}
		// 决策路由到该渠道；web_only 的规则按标签 acc-finance 归 funds；回答人白名单在 Web 设置。
		pol, _ := json.Marshal(map[string]any{
			"disturb": map[string]any{"merge_window_sec": 2,
				"routes": map[string]string{"decision": tgChannel, "reminder": "web", "brief": "web", "health": "web", "wake": "web"}},
			"web_only": map[string]any{"categories": []string{"external_commitment", "funds", "irreversible", "permission_change", "spec_activation"},
				"rules": []map[string]string{{"tag": "acc-finance", "category": "funds"}}}})
		f.webOK("PUT", "/settings/policy", string(pol), 200)
		ans, _ := json.Marshal(map[string]any{"channel_answerers": map[string][]string{tgChannel: {tgUser}}})
		f.webOK("PUT", "/settings/allowlists", string(ans), 200)
		f.startExt()
		if low := strings.ToLower(f.extOut.String()); strings.Contains(low, "hermes") || strings.Contains(low, "harness") {
			t.Errorf("ext 的输出里出现 harness: %s", f.extOut.String())
		}
	})

	step("ACT-TG-02 决策请求送达 Telegram", func(t *testing.T) {
		item = f.task("tg 决策")
		f.ask(item, "TG02 选哪个方案？")
		f.until(tgKernWait, "投递 state=delivered 且有 external_ref", func() bool {
			ds := f.deliveries(item)
			return len(ds) > 0 && ds[0].State == "delivered" && ds[0].Ref != ""
		})
		ds := f.deliveries(item)
		d1 = ds[0].ID
		msgs := f.sentWith("TG02")
		if len(msgs) != 1 || msgs[0].Chat != tgChatID {
			t.Fatalf("机器人替身应记录到发给 %s 的 1 条消息: %+v", tgUser, f.bot.Sent())
		}
		for _, want := range []string{"选哪个方案", "A: 方案A", "B: 方案B", "推荐：A"} {
			if !strings.Contains(msgs[0].Text, want) {
				t.Errorf("消息缺 %q:\n%s", want, msgs[0].Text)
			}
		}
		if want := strconv.FormatInt(tgChatID, 10) + ":" + strconv.Itoa(msgs[0].ID); ds[0].Ref != want {
			t.Errorf("external_ref = %q，期望 %q", ds[0].Ref, want)
		}
	})

	step("ACT-TG-03 在 Telegram 回答", func(t *testing.T) {
		msg := f.sentWith("TG02")[0]
		f.bot.Reply(tgChatID, msg.ID, tgAccount, "A")
		f.until(tgKernWait, "事项被回答", func() bool { return f.item(item).Ask.Resolution != nil })
		it := f.item(item)
		r := it.Ask.Resolution
		if r.ResolvedVia != tgChannel || r.Choice != "A" || r.AnsweredBy != tgUser {
			t.Errorf("回答记录不符: %+v（期望 resolved_via=%s choice=A answered_by=%s）", r, tgChannel, tgUser)
		}
		if it.Phase == "needs_input" {
			t.Errorf("事项应继续，仍是 %s", it.Phase)
		}
		// 回答人在 Event 载荷里，observer 凭据会裁掉；本人（Web）可见完整时间线。
		if why := f.webOK("GET", "/why/"+item, "", 200); !strings.Contains(why, tgChannel) || !strings.Contains(why, tgUser) {
			t.Errorf("apa why 查不到渠道与回答人: %s", why)
		}
		f.until(tgKernWait, "投递 acknowledged", func() bool {
			ds := f.deliveries(item)
			return len(ds) == 1 && ds[0].ID == d1 && ds[0].State == "acknowledged"
		})
	})

	step("ACT-TG-04 web_only 只给 Web 链接", func(t *testing.T) {
		fin := f.task("tg web_only", "acc-finance")
		f.ask(fin, "TG04 付款？")
		f.until(tgKernWait, "web_only 投递 delivered", func() bool {
			ds := f.deliveries(fin)
			return len(ds) > 0 && ds[0].State == "delivered" && ds[0].Ref != ""
		})
		msgs := f.sentWith("TG04")
		if len(msgs) != 1 {
			t.Fatalf("web_only 决策应只发 1 条消息: %+v", f.bot.Sent())
		}
		m := msgs[0]
		if !strings.Contains(m.Text, "/items/"+fin) || strings.Contains(m.Text, "选项：") || strings.Contains(m.Text, "作答链接") ||
			strings.Contains(m.Text, "/forms/") || len(m.Buttons) != 0 {
			t.Errorf("web_only 消息应只带 Web 链接，不带选项、按钮或表单链接:\n%s %v", m.Text, m.Buttons)
		}
		f.bot.Reply(tgChatID, m.ID, tgAccount, "同意")
		f.until(tgKernWait, "ext 对回复给出 Web 链接", func() bool { return len(f.sentWith("不能在 Telegram 回答")) > 0 })
		if note := f.sentWith("不能在 Telegram 回答")[0]; !strings.Contains(note.Text, "/items/"+fin) {
			t.Errorf("拒绝回复应带 Web 链接:\n%s", note.Text)
		}
		time.Sleep(2 * time.Second) // 让可能的误采纳落盘后再看
		if it := f.item(fin); it.Phase != "needs_input" || it.Ask.Resolution != nil {
			t.Errorf("决策不应生效: phase=%s resolution=%+v", it.Phase, it.Ask.Resolution)
		}
	})

	step("ACT-TG-05 ext 不用 harness 网关", func(t *testing.T) {
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
	})

	step("ACT-TG-06 ext 崩溃重启后不重复发送", func(t *testing.T) {
		f.holdMu.Lock()
		f.hold = true
		f.holdMu.Unlock()
		crash := f.task("tg 崩溃重启")
		f.ask(crash, "TG06 崩溃重启验收？")
		select {
		case <-f.acks:
		case <-time.After(tgKernWait):
			t.Fatalf("ext 没有走到回报 ack: %s", f.extOut.String())
		}
		if n := len(f.sentWith("TG06")); n != 1 {
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
		time.Sleep(5 * time.Second)
		if n := len(f.sentWith("TG06")); n != 1 {
			t.Errorf("重启后机器人替身对该投递应只记录 1 条，实际 %d", n)
		}
		ds := f.deliveries(crash)
		if ds[0].Claims < 2 {
			t.Errorf("应被重新领取（claims=%d）", ds[0].Claims)
		}
		if want := strconv.FormatInt(tgChatID, 10) + ":" + strconv.Itoa(f.sentWith("TG06")[0].ID); ds[0].Ref != want {
			t.Errorf("重启后回报的 external_ref = %q，期望沿用第一次发送的 %q", ds[0].Ref, want)
		}
	})
}
