//go:build live

// Package mail 按验收文档 flow-source-mail（ACT-MAIL-01..09）用真实 APA 二进制、真实 himalaya 和真实测试邮箱
// （AgentMail）走完整流程。凭据只经 APA_LIVE_* 指向的文件读取：口令复制进测试临时目录供 himalaya 的
// password.cmd 使用，不进入 APA_HOME、仓库或日志。himalaya 配置放临时 HOME（macOS 读 ~/Library/Application Support），
// 不碰用户配置。Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF），与 tests/e2e/sourcepull 一致。
package mail

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net/smtp"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const waitMax = 120 * time.Second

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("缺少环境变量 %s（先 source APA_LIVE_ENV_FILE）", k)
	}
	return v
}

func run(t *testing.T, dir string, name string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	if env != nil {
		cmd.Env = env
	}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e // himalaya 的 tracing 警告在 stderr，不能混进 JSON
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return o.String() + e.String(), ee.ExitCode()
	}
	if err != nil {
		t.Fatalf("运行 %s: %v", name, err)
	}
	return o.String(), 0
}

type live struct {
	*baseline.Env
	t          *testing.T
	addr, pwFn string // 邮箱地址；tmp 内口令文件
	home       string // himalaya 用的 HOME
	ctl        string // 控制目录：path 文件决定适配器的 PATH
	origPath   string
	smtpHP     string
	pw         string
	src        string // 被测邮件来源 id
	adapter    string // 适配器二进制
}

func (l *live) himalaya(args ...string) (string, int) {
	l.t.Helper()
	return run(l.t, "", "himalaya", []string{"PATH=" + l.origPath, "HOME=" + l.home}, args...)
}

func (l *live) setPath(p string) {
	l.t.Helper()
	if err := os.WriteFile(filepath.Join(l.ctl, "path"), []byte(p), 0o644); err != nil {
		l.t.Fatal(err)
	}
}

func (l *live) send(subject string) {
	l.t.Helper()
	host := strings.SplitN(l.smtpHP, ":", 2)[0]
	c, err := tls.Dial("tcp", l.smtpHP, &tls.Config{ServerName: host})
	if err != nil {
		l.t.Fatal(err)
	}
	sc, err := smtp.NewClient(c, host)
	if err != nil {
		l.t.Fatal(err)
	}
	defer sc.Close()
	if err := sc.Auth(smtp.PlainAuth("", l.addr, l.pw, host)); err != nil {
		l.t.Fatal(err)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n\r\nlive test body\r\n", l.addr, l.addr, subject)
	if err := sc.Mail(l.addr); err != nil {
		l.t.Fatal(err)
	}
	if err := sc.Rcpt(l.addr); err != nil {
		l.t.Fatal(err)
	}
	w, _ := sc.Data()
	_, _ = w.Write([]byte(msg))
	if err := w.Close(); err != nil {
		l.t.Fatal(err)
	}
	_ = sc.Quit()
}

type envelope struct {
	ID      string   `json:"id"`
	Flags   []string `json:"flags"`
	Subject string   `json:"subject"`
}

func (l *live) envelopes(folder string) []envelope {
	l.t.Helper()
	out, code := l.himalaya("envelope", "list", "-f", folder, "--page-size", "200", "--output", "json")
	var es []envelope
	if code != 0 || json.Unmarshal([]byte(out), &es) != nil {
		l.t.Fatalf("himalaya envelope list %s 失败 (%d)", folder, code)
	}
	return es
}

func (l *live) find(subject string) *envelope {
	for _, e := range l.envelopes("INBOX") {
		if e.Subject == subject {
			e := e
			return &e
		}
	}
	return nil
}

func (l *live) until(what string, cond func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(waitMax)
	for !cond() {
		if time.Now().After(deadline) {
			st := l.show(l.src)
			l.t.Fatalf("%v 内没有等到：%s（来源 %s: runs=%d last_error=%q needs_input=%q）", waitMax, what, l.src, st.Stats.Runs, st.Stats.LastError, st.Stats.NeedsInputItem)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

type srcState struct {
	Enabled bool   `json:"enabled"`
	Cursor  string `json:"cursor"`
	Stats   struct {
		Runs           int    `json:"runs"`
		LastError      string `json:"last_error"`
		NeedsInputItem string `json:"needs_input_item"`
	} `json:"stats"`
}

func (l *live) show(id string) srcState {
	l.t.Helper()
	r := l.Apa(l.OBS, "", "source", "show", id)
	var s srcState
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &s) != nil {
		l.t.Fatalf("source show %s: %d %s %s", id, r.Code, r.Stdout, r.Stderr)
	}
	return s
}

// events 返回来源已落盘的事件（原样 map，便于检查 sender_verified 等字段）。
func (l *live) events(id string) []map[string]any {
	l.t.Helper()
	r := l.APIRaw("GET", "/v1/sources/"+id+"/events?limit=500", "", l.OBS)
	if r.Code != 200 {
		l.t.Fatalf("读来源事件 HTTP %d: %s", r.Code, r.Body)
	}
	var out struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		l.t.Fatal(err)
	}
	return out.Events
}

func (l *live) eventBySubject(id, subject string) map[string]any {
	for _, e := range l.events(id) {
		if e["subject"] == subject {
			return e
		}
	}
	return nil
}

func eventID(e map[string]any) string {
	k, _ := e["dedupe_key"].(map[string]any)
	s, _ := k["event_id"].(string)
	return s
}

// settle 等来源至少再运行 n 次（无论成败）。
func (l *live) settle(id string, n int) {
	l.t.Helper()
	start := l.show(id).Stats.Runs
	l.until(fmt.Sprintf("%s 再运行 %d 次", id, n), func() bool { return l.show(id).Stats.Runs >= start+n })
}

func (l *live) register(id, cmd, srcType, dedupe string) {
	l.t.Helper()
	body := map[string]any{"id": id, "type": "script", "source_type": srcType, "command": cmd, "schedule": "1s", "timeout": 100, "dedupe_domain": dedupe}
	b, _ := json.Marshal(body)
	if r := l.Web("POST", "/v1/sources", string(b)); r.Code != 201 {
		l.t.Fatalf("登记 %s: HTTP %d %s", id, r.Code, r.Body)
	}
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestFlowSourceMail(t *testing.T) {
	addr := need(t, "APA_LIVE_MAIL_ADDRESS")
	imap := strings.SplitN(need(t, "APA_LIVE_MAIL_IMAP"), ":", 2)
	smtpHP := need(t, "APA_LIVE_MAIL_SMTP")
	pwRaw, err := os.ReadFile(need(t, "APA_LIVE_MAIL_PASSWORD_FILE"))
	if err != nil {
		t.Fatal(err)
	}
	pw := strings.TrimSpace(string(pwRaw))
	if _, err := exec.LookPath("himalaya"); err != nil {
		t.Fatal("本机没有 himalaya")
	}

	root := t.TempDir()
	l := &live{t: t, addr: addr, smtpHP: smtpHP, pw: pw, origPath: os.Getenv("PATH"),
		home: filepath.Join(root, "home"), ctl: filepath.Join(root, "ctl"), pwFn: filepath.Join(root, "pw")}
	for _, d := range []string{l.home, l.ctl} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(l.pwFn, []byte(pw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgDir := filepath.Join(l.home, "Library", "Application Support", "himalaya")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[accounts.live]\ndefault = true\nemail = %q\nbackend.type = \"imap\"\nbackend.host = %q\nbackend.port = %s\nbackend.encryption.type = \"tls\"\nbackend.login = %q\nbackend.auth.type = \"password\"\nbackend.auth.cmd = %q\n",
		addr, imap[0], imap[1], addr, "cat "+l.pwFn)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	l.setPath(l.origPath)

	// 构建随发行附带的邮件适配器，经包装脚本按控制文件设定 PATH 后启动（用于 ACT-MAIL-04/05/07 改 PATH）。
	adapter := filepath.Join(root, "mail-himalaya")
	repo, _ := filepath.Abs(filepath.Join("..", "..", ".."))
	if out, code := run(t, repo, "go", nil, "build", "-o", adapter, "./sources/mail/cmd/mail-himalaya"); code != 0 {
		t.Fatalf("构建适配器: %s", out)
	}
	wrapper := filepath.Join(root, "run.sh")
	writeExec(t, wrapper, fmt.Sprintf("#!/bin/sh\nPATH=\"$(cat %s/path)\" exec %s\n", l.ctl, adapter))

	origHome := os.Getenv("HOME") // go 的模块缓存在真实 HOME 下，ACT-MAIL-08 要用
	l.adapter = adapter
	l.Env = baseline.New(t)
	// apa serve 继承测试进程环境，适配器经白名单拿到 HOME（himalaya 配置所在）。
	t.Setenv("HOME", l.home)
	l.Env.StartKernel()
	l.Env.IssueCredentials()
	cmd := "/bin/sh " + wrapper

	step := func(name string, fn func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, fn) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}
	const src = "src_mail"
	l.src = src
	tag := fmt.Sprintf("%d", time.Now().UnixNano())
	subj := "[APA-LIVE-TEST] mail " + tag

	step("ACT-MAIL-01 登记适配器，凭据不进数据目录", func(t *testing.T) {
		// himalaya 自己的命令能连上测试邮箱。
		if _, code := l.himalaya("folder", "list"); code != 0 {
			t.Fatalf("himalaya 连不上测试邮箱: %d", code)
		}
		l.register(src, cmd, "mail", "mail")
		if !l.show(src).Enabled {
			t.Fatal("来源应为 enabled")
		}
		if out, _ := run(t, "", "grep", nil, "-rl", pw, l.Home); strings.TrimSpace(out) != "" {
			t.Fatalf("口令出现在 APA_HOME: %s", out)
		}
	})

	step("ACT-MAIL-02 新邮件成为标准事件", func(t *testing.T) {
		l.send(subj)
		l.until("来源产生该邮件事件", func() bool { return l.eventBySubject(src, subj) != nil })
		ev := l.eventBySubject(src, subj)
		id := eventID(ev)
		// 独立核对：id 必须等于 himalaya 直接读出的 Message-ID 头。
		e := l.find(subj)
		if e == nil {
			t.Fatal("邮箱里找不到测试邮件")
		}
		hdr, _ := l.himalaya("message", "read", "--preview", "-H", "Message-ID", e.ID)
		if !strings.HasPrefix(id, "<") || !strings.Contains(hdr, id) {
			t.Errorf("事件 id %q 不是该邮件的 Message-ID（himalaya 头部: %q）", id, hdr)
		}
		if ev["type"] != "mail.received" || ev["subject"] != subj {
			t.Errorf("事件字段不全: %v", ev)
		}
		if v, _ := ev["sender_verified"].(bool); v {
			t.Error("未核实发件人时 sender_verified 不应为真")
		}
		// inbound-triage 的 mail-received 规则按 source_type=mail 匹配，then: judge（判断型）→ 待判断。
		if ev["disposition"] != "unjudged" {
			t.Errorf("能力包规则应按 source_type=mail 匹配并走判断型 (unjudged): disposition=%v item=%v", ev["disposition"], ev["item"])
		}
	})

	step("ACT-MAIL-03 拉取不改变邮箱状态", func(t *testing.T) {
		// 暂停来源，发信并确认其未读，再恢复来源拉取。
		if r := l.Web("PATCH", "/v1/sources/"+src, `{"enabled":false}`); r.Code != 200 {
			t.Fatalf("disable: %d %s", r.Code, r.Body)
		}
		s3 := subj + " B"
		l.settle0(src)
		l.send(s3)
		l.until("邮件到达邮箱", func() bool { return l.find(s3) != nil })
		before := l.find(s3)
		sentBefore := len(l.envelopes("Sent"))
		seen := func(e *envelope) bool {
			for _, f := range e.Flags {
				if strings.EqualFold(f, "Seen") {
					return true
				}
			}
			return false
		}
		if seen(before) {
			t.Fatal("测试邮件拉取前就是已读，无法验证")
		}
		if r := l.Web("PATCH", "/v1/sources/"+src, `{"enabled":true}`); r.Code != 200 {
			t.Fatalf("enable: %d %s", r.Code, r.Body)
		}
		l.until("来源拉到邮件", func() bool { return l.eventBySubject(src, s3) != nil })
		after := l.find(s3)
		if seen(after) {
			t.Error("拉取把邮件标成了已读")
		}
		if n := len(l.envelopes("Sent")); n != sentBefore {
			t.Errorf("已发送文件夹变化: %d → %d", sentBefore, n)
		}
	})

	cursorUnchanged := func(t *testing.T, before string) {
		t.Helper()
		if c := l.show(src).Cursor; c != before {
			t.Errorf("游标不应前进: %q → %q", before, c)
		}
	}
	other := "src_other"
	step("ACT-MAIL-04 himalaya 不在 PATH 时生成 needs_input", func(t *testing.T) {
		// 另一来源照常：登记一个每次输出新事件的脚本来源。
		otherScript := filepath.Join(root, "other.sh")
		writeExec(t, otherScript, "#!/bin/sh\nn=$(date +%s%N)\nprintf '{\"id\":\"o%s\",\"type\":\"acc.event\",\"subject\":\"other\",\"occurred_at\":\"2030-01-01T00:00:00Z\"}\\n{\"cursor\":\"%s\"}\\n' \"$n\" \"$n\"\n")
		l.register(other, "/bin/sh "+otherScript, "acc", "acc")
		c := l.show(src).Cursor
		l.setPath("/usr/bin:/bin")
		runs := l.show(other).Stats.Runs
		l.until("生成 needs_input", func() bool { return l.show(src).Stats.NeedsInputItem != "" })
		cursorUnchanged(t, c)
		l.until("其他来源照常运行", func() bool { return l.show(other).Stats.Runs > runs+1 })
		l.adapterRefuses("/usr/bin:/bin", "himalaya")
	})

	fake := func(name, body string) string {
		d := filepath.Join(root, "fake-"+name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		writeExec(t, filepath.Join(d, "himalaya"), body)
		return d
	}
	failsWithNeedsInput := func(t *testing.T, path, wantErr string) {
		t.Helper()
		l.setPath(l.origPath) // 先恢复，清掉上一个 needs_input 的影响不依赖它
		c := l.show(src).Cursor
		l.setPath(path)
		l.settle(src, 2)
		s := l.show(src)
		cursorUnchanged(t, c)
		if s.Stats.NeedsInputItem == "" || !strings.Contains(s.Stats.LastError, "exit status 2") {
			t.Fatalf("应以退出码 2 生成 needs_input: item=%q last_error=%q", s.Stats.NeedsInputItem, s.Stats.LastError)
		}
		l.adapterRefuses(path, wantErr)
	}
	step("ACT-MAIL-05 版本不在声明范围", func(t *testing.T) {
		d := fake("version", "#!/bin/sh\n[ \"$1\" = \"--version\" ] && { echo 'himalaya v9.9.9'; exit 0; }\necho SHOULD-NOT-PARSE >&2\nexit 1\n")
		failsWithNeedsInput(t, d+":/usr/bin:/bin", "9.9.9")
	})

	step("ACT-MAIL-06 认证失败生成 needs_input，恢复后补拉", func(t *testing.T) {
		l.setPath(l.origPath)
		l.settle(src, 1)
		missed := subj + " C"
		l.send(missed)
		l.until("邮件到达邮箱", func() bool { return l.find(missed) != nil })
		if err := os.WriteFile(l.pwFn, []byte("wrong-password"), 0o600); err != nil {
			t.Fatal(err)
		}
		// 邮件可能在改口令前已被拉走，则改发一封新的以保证"错过"。
		if l.eventBySubject(src, missed) != nil {
			missed = subj + " D"
		}
		c := l.show(src).Cursor
		if missed == subj+" D" {
			// 口令已是错的，发信走 SMTP 不受影响。
			l.send(missed)
		}
		l.settle(src, 2)
		s := l.show(src)
		cursorUnchanged(t, c)
		if s.Stats.NeedsInputItem == "" {
			t.Errorf("认证失败应生成 needs_input: last_error=%q", s.Stats.LastError)
		}
		if err := os.WriteFile(l.pwFn, []byte(pw), 0o600); err != nil {
			t.Fatal(err)
		}
		l.until("恢复后拉到错过的邮件", func() bool { return l.eventBySubject(src, missed) != nil })
	})

	step("ACT-MAIL-07 输出无法解析时不前进", func(t *testing.T) {
		d := fake("garbage", "#!/bin/sh\ncase \"$1\" in --version) echo 'himalaya v1.1.0';; *) echo 'this is not json';; esac\n")
		failsWithNeedsInput(t, d+":/usr/bin:/bin", "JSON")
		d2 := fake("nomid", "#!/bin/sh\ncase \"$1\" in --version) echo 'himalaya v1.1.0';; envelope) echo '[{\"id\":\"1\",\"flags\":[],\"subject\":\"x\",\"from\":{\"name\":\"a\",\"addr\":\"a@b.c\"},\"date\":\"2099-01-01 00:00+00:00\"}]';; message) echo '\"no header here\"';; esac\n")
		failsWithNeedsInput(t, d2+":/usr/bin:/bin", "Message-ID")
		l.setPath(l.origPath)
	})

	step("ACT-MAIL-08 适配器契约测试", func(t *testing.T) {
		out, code := run(t, repo, "go", []string{"PATH=" + l.origPath, "HOME=" + origHome}, "test", "-count=1", "./sources/...")
		if code != 0 {
			t.Fatalf("go test ./sources/... 失败:\n%s", out)
		}
		if strings.Contains(out, addr) {
			t.Error("契约测试输出不应含真实账号")
		}
	})

	step("ACT-MAIL-09 换工具不动规则和内核", func(t *testing.T) {
		// 同一封邮件由另一实现（脚本）输出同一 Message-ID，沿用 source_type=mail 与 dedupe_domain=mail。
		ev := l.eventBySubject(src, subj)
		id := eventID(ev)
		count := func() int {
			n := 0
			for _, e := range l.events(src) {
				if eventID(e) == id {
					n++
				}
			}
			return n
		}
		items := fmt.Sprint(l.Web("GET", "/v1/items?limit=500", "").Body)
		alt := filepath.Join(root, "alt.sh")
		b, _ := json.Marshal(map[string]any{"id": id, "type": "mail.received", "subject": subj, "occurred_at": ev["occurred_at"]})
		writeExec(t, alt, "#!/bin/sh\ncat <<'EOF'\n"+string(b)+"\n{\"cursor\":\"alt\"}\nEOF\n")
		l.register("src_mail_alt", "/bin/sh "+alt, "mail", "mail")
		l.settle("src_mail_alt", 2)
		if n := count(); n != 1 {
			t.Errorf("同一 Message-ID 的事件应只有 1 条，实际 %d", n)
		}
		if after := fmt.Sprint(l.Web("GET", "/v1/items?limit=500", "").Body); after != items {
			t.Error("换实现后事项列表发生变化（产生重复事项）")
		}
	})
}

// adapterRefuses 直接运行适配器：退出码 2、stdout 无任何输出（不输出新游标）、stderr 说明原因。
// 内核的 needs_input 事项只写通用文案，原因只在适配器的 stderr 里。
func (l *live) adapterRefuses(path, want string) {
	l.t.Helper()
	cmd := exec.Command(l.adapter)
	cmd.Stdin = strings.NewReader(`{"config":{},"cursor":""}`)
	cmd.Env = []string{"PATH=" + path, "HOME=" + l.home}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 2 || o.Len() != 0 || !strings.Contains(e.String(), want) {
		l.t.Errorf("适配器应退出码 2、无输出、stderr 含 %q: err=%v stdout=%q stderr=%q", want, err, o.String(), e.String())
	}
}

// settle0 只是为了在 disable 后让调度器静止一拍。
func (l *live) settle0(string) { time.Sleep(3 * time.Second) }
