//go:build live

// Package mail 用真实邮箱（AgentMail 测试账号）和真实 himalaya 验证邮件适配器（验收文档 flow-source-mail）。
// 凭据只经 APA_LIVE_* 环境变量指向的文件读取；himalaya 配置放临时 HOME，不碰用户配置。
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
)

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("缺少环境变量 %s（先 source APA_LIVE_ENV_FILE）", k)
	}
	return v
}

// TestLiveMailAdapterReadsRealMessageID 对应 ACT-MAIL-02：真实邮件经真实 himalaya 进入标准事件，id 取 Message-ID。
func TestLiveMailAdapterReadsRealMessageID(t *testing.T) {
	addr := need(t, "APA_LIVE_MAIL_ADDRESS")
	imap := strings.SplitN(need(t, "APA_LIVE_MAIL_IMAP"), ":", 2)
	smtpHP := need(t, "APA_LIVE_MAIL_SMTP")
	pwFile := need(t, "APA_LIVE_MAIL_PASSWORD_FILE")
	pw, err := os.ReadFile(pwFile)
	if err != nil {
		t.Fatal(err)
	}

	home := t.TempDir()
	cfgDir := filepath.Join(home, "Library", "Application Support", "himalaya") // macOS 上 himalaya 的配置位置
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf("[accounts.live]\ndefault = true\nemail = %q\nbackend.type = \"imap\"\nbackend.host = %q\nbackend.port = %s\nbackend.encryption.type = \"tls\"\nbackend.login = %q\nbackend.auth.type = \"password\"\nbackend.auth.cmd = %q\n",
		addr, imap[0], imap[1], addr, "cat "+pwFile)
	if err := os.WriteFile(filepath.Join(cfgDir, "config.toml"), []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	bin := filepath.Join(t.TempDir(), "mail-himalaya")
	build := exec.Command("go", "build", "-o", bin, "./sources/mail/cmd/mail-himalaya")
	build.Dir = filepath.Join("..", "..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建适配器: %v\n%s", err, out)
	}

	// 发一封带已知 Message-ID 的测试邮件到测试邮箱自身。
	mid := fmt.Sprintf("<apa-live-%d@apa.test>", time.Now().UnixNano())
	host := strings.SplitN(smtpHP, ":", 2)[0]
	c, err := tls.Dial("tcp", smtpHP, &tls.Config{ServerName: host})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := smtp.NewClient(c, host)
	if err != nil {
		t.Fatal(err)
	}
	if err := sc.Auth(smtp.PlainAuth("", addr, strings.TrimSpace(string(pw)), host)); err != nil {
		t.Fatal(err)
	}
	msg := fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: APA-LIVE-TEST mail\r\nMessage-ID: %s\r\n\r\nbody\r\n", addr, addr, mid)
	if err := sc.Mail(addr); err != nil {
		t.Fatal(err)
	}
	if err := sc.Rcpt(addr); err != nil {
		t.Fatal(err)
	}
	w, _ := sc.Data()
	_, _ = w.Write([]byte(msg))
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	_ = sc.Quit()

	deadline := time.Now().Add(90 * time.Second)
	for {
		cmd := exec.Command(bin)
		cmd.Stdin = strings.NewReader(`{"config":{},"cursor":""}`)
		cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home}
		var o, e bytes.Buffer
		cmd.Stdout, cmd.Stderr = &o, &e
		runErr := cmd.Run()
		code := 0
		if ee, ok := runErr.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		if code != 0 {
			t.Fatalf("适配器对真实 himalaya 退出码 %d（期望 0）: %s", code, strings.TrimSpace(e.String()))
		}
		for _, l := range strings.Split(o.String(), "\n") {
			var ev struct {
				ID string `json:"id"`
			}
			if json.Unmarshal([]byte(l), &ev) == nil && ev.ID == mid {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("90s 内没有拉到 Message-ID=%s 的事件，输出:\n%s", mid, o.String())
		}
		time.Sleep(3 * time.Second)
	}
}
