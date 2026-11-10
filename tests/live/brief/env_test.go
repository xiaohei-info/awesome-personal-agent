//go:build live

// Package brief 按验收文档 flow-brief（ACT-BRF-01..06）用真实 apa 二进制、真实 Hermes（headless 与 inject 插件）
// 和真实模型端点走完整简报流程。只能在 taiyi 的隔离环境运行：Hermes 家目录由测试在临时目录里新建，
// 只复制种子里的 config.yaml 与 .env，不读写 ~/.hermes。Web 步骤走 Web 页面调用的同一套 HTTP API
// （已登录会话 + CSRF），与 tests/live/background、tests/live/mail 一致；taiyi 上没有 ego-browser。
package brief

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const harness = "hermes"

var tokenRE = regexp.MustCompile(`apa_[A-Za-z0-9_]{12,}`)

type fx struct {
	*baseline.Env
	t        *testing.T
	hermesBN string
	seed     string
	wrapper  string // 内核拉起 Hermes 用的包装脚本
	down     string // 存在即表示 harness 不可用（BRF-05）
	hhome    string // 无头运行用的 HERMES_HOME
	evidence map[string]any
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

func setup(t *testing.T) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	e.Serve()
	e.IssueCredentials()
	e.CreateProjects()
	f := &fx{Env: e, t: t, evidence: map[string]any{}}
	f.prepareHermes()
	f.declareHermes()
	t.Cleanup(f.stopGateways)
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	// 简报是只读运行，默认档位 read 已足够；任务型读资料文件需要 file 工具集，放到 edit 上限（与 injection 一致）。
	f.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"edit","by_harness":{}}}`, 200)
	return f
}

func (f *fx) prepareHermes() {
	f.hermesBN = envOr("APA_LIVE_HERMES_BIN", "/opt/apa-live/venv/bin/hermes")
	f.seed = envOr("APA_LIVE_HERMES_SEED", "/opt/apa-live/hermes-home")
	if strings.HasSuffix(f.hermesBN, "/apa-live/bin/hermes") { // 该包装脚本把 HERMES_HOME 固定为共享目录，会绕开隔离
		f.t.Fatalf("APA_LIVE_HERMES_BIN=%s 会使用共享 HERMES_HOME；请改用 /opt/apa-live/venv/bin/hermes", f.hermesBN)
	}
	if _, err := os.Stat(f.hermesBN); err != nil {
		f.t.Fatalf("真实 Hermes 不可用（APA_LIVE_HERMES_BIN=%s）：%v", f.hermesBN, err)
	}
	f.hhome = f.newHermesHome("headless")
	f.down = filepath.Join(f.Root, "harness-down")
	f.wrapper = filepath.Join(f.Root, "hermes-run.sh")
	script := fmt.Sprintf(`#!/bin/sh
# 内核子进程只继承白名单环境：这里补上隔离的 HERMES_HOME 与 apa 所在目录，参数原样交给真实 hermes。
if [ -e %q ]; then echo "harness unavailable" >&2; exit 127; fi
export HERMES_HOME=%q
export PATH=%q:"$PATH"
export APA_BIN=%q
printf '%%s\n' "$@" > args.txt
exec %q "$@"
`, f.down, f.hhome, filepath.Dir(f.Bin["apa"]), f.Bin["apa"], f.hermesBN)
	must(f.t, os.WriteFile(f.wrapper, []byte(script), 0o755))
}

// newHermesHome 建全新的隔离 HERMES_HOME：只复制种子里的 config.yaml 与 .env（0600），挂上并启用 apa 插件。
func (f *fx) newHermesHome(name string) string {
	home := filepath.Join(f.Root, "hermes-home-"+name)
	must(f.t, os.MkdirAll(filepath.Join(home, "plugins"), 0o700))
	for _, n := range []string{"config.yaml", ".env"} {
		b, err := os.ReadFile(filepath.Join(f.seed, n))
		if err != nil {
			f.t.Fatalf("读取 Hermes 种子 %s：%v", n, err)
		}
		if n == "config.yaml" {
			if strings.Contains("\n"+string(b), "\nplugins:") {
				f.t.Fatal("种子 config.yaml 已有 plugins 段，请改为无插件的全新种子")
			}
			// plugins/hermes/tools/README.md 要求的安装项；种子已有则不重复写（只在测试代码里补，缺失才加）。
			if !strings.Contains(string(b), "client_env_passthrough") {
				if strings.Contains("\n"+string(b), "\nterminal:") {
					f.t.Fatal("种子 config.yaml 已有 terminal 段但缺 client_env_passthrough，请修正种子")
				}
				b = append(b, []byte("\nterminal:\n  client_env_passthrough:\n    - APA_RUN_CONTEXT\n")...)
			}
			b = append(b, []byte("\nplugins:\n  enabled:\n    - apa-core\n    - apa-tools\n    - apa-headless\n    - apa-inject\n")...)
		}
		must(f.t, os.WriteFile(filepath.Join(home, n), b, 0o600))
	}
	for link, dir := range map[string]string{"apa-core": "core", "apa-tools": "tools", "apa-headless": "headless", "apa-inject": "inject"} {
		must(f.t, os.Symlink(filepath.Join(repoRoot(), "plugins", "hermes", dir), filepath.Join(home, "plugins", link)))
	}
	return home
}

// declareHermes 提交 plugins/hermes/headless/declaration.json（只把可执行文件换成包装脚本）并在 Web 确认。
func (f *fx) declareHermes() {
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "plugins", "hermes", "headless", "declaration.json"))
	must(f.t, err)
	var decl struct {
		Headless map[string]any `json:"headless"`
	}
	must(f.t, json.Unmarshal(raw, &decl))
	cmd := decl.Headless["command"].([]any)
	decl.Headless["command"] = append([]any{f.wrapper}, cmd[1:]...)
	b, _ := json.Marshal(map[string]any{"harness": harness, "headless": decl.Headless})
	if r := f.Apa(f.DEL, string(b), "declare", "-", "--evidence", "真实 Hermes 的无头运行声明"); r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/"+harness+"/1/confirm", "", 200)
}

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
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, f.redact(r.Stdout), f.redact(r.Stderr))
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

func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("等待超时（%v）：%s", d, what)
		}
		time.Sleep(2 * time.Second)
	}
}

type event struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Reason  string         `json:"reason"`
	At      string         `json:"at"`
	Payload map[string]any `json:"payload"`
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

type briefItem struct {
	ID      string         `json:"id"`
	Kind    string         `json:"kind"`
	Phase   string         `json:"phase"`
	Title   string         `json:"title"`
	Summary string         `json:"summary"`
	Version int64          `json:"version"`
	Created string         `json:"created_at"`
	Spec    map[string]any `json:"spec"`
}

func (f *fx) briefs() []briefItem {
	f.t.Helper()
	var out struct {
		Items []briefItem `json:"items"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list", "--kind", "brief").Stdout), &out))
	return out.Items
}

type run struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Runner   string `json:"runner"`
	Target   string `json:"target"`
	State    string `json:"state"`
	Harness  string `json:"harness"`
	Access   string `json:"access"`
}

func (f *fx) runs(args ...string) []run {
	f.t.Helper()
	var out struct {
		Runs []run `json:"runs"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, append([]string{"run", "list"}, args...)...).Stdout), &out))
	return out.Runs
}

func (f *fx) finishedRun(target string, d time.Duration) run {
	f.t.Helper()
	var r run
	defer func() {
		if f.t.Failed() {
			f.t.Logf("事项 %s 的运行: %+v", target, f.runs("--target", target))
		}
	}()
	f.waitFor("事项 "+target+" 的运行结束", d, func() bool {
		for _, x := range f.runs("--target", target) {
			if x.State != "queued" && x.State != "running" {
				r = x
				return true
			}
		}
		return false
	})
	return r
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

func (f *fx) redact(s string) string {
	for _, tok := range []string{f.OBS, f.WRK, f.WRK2, f.DEL, f.CHAN} {
		if tok != "" {
			s = strings.ReplaceAll(s, tok, "<redacted>")
		}
	}
	return tokenRE.ReplaceAllString(s, "<redacted>")
}

// saveEvidence 把一份文本证据写入 APA_LIVE_EVIDENCE_DIR（未设置则忽略）。
func (f *fx) saveEvidence(name, content string) {
	dir := os.Getenv("APA_LIVE_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	must(f.t, os.MkdirAll(dir, 0o755))
	must(f.t, os.WriteFile(filepath.Join(dir, name), []byte(f.redact(content)), 0o644))
}

func (f *fx) flushEvidence() {
	b, _ := json.MarshalIndent(f.evidence, "", "  ")
	f.saveEvidence("brief-evidence.json", string(b))
}

// hermesChat 以主会话方式（新开会话、独立 HERMES_HOME）问一句话，返回脱敏后的输出。
func (f *fx) hermesChat(prompt string) string {
	f.t.Helper()
	work := filepath.Join(f.Root, "main-session")
	must(f.t, os.MkdirAll(work, 0o755))
	cmd := exec.Command(f.hermesBN, "chat", "--oneshot", "-Q", "-q", prompt)
	cmd.Dir = work
	cmd.Env = []string{"HOME=" + f.Root, "PATH=" + filepath.Dir(f.Bin["apa"]) + ":" + os.Getenv("PATH"),
		"HERMES_HOME=" + f.newHermesHome(fmt.Sprintf("main-%d", time.Now().UnixNano())), "APA_BIN=" + f.Bin["apa"],
		"APA_URL=" + f.URL, "APA_TOKEN=" + f.DEL, "APA_HOME=" + f.Home}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	must(f.t, cmd.Start())
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			f.t.Fatalf("hermes chat 失败：%v\n%s", err, f.redact(out.String()))
		}
	case <-time.After(10 * time.Minute):
		_ = cmd.Process.Kill()
		f.t.Fatalf("hermes chat 10 分钟未返回\n%s", f.redact(out.String()))
	}
	return f.redact(out.String())
}

// stopGateways 结束 Hermes oneshot 顺带拉起的网关：只认本测试 Root 下各隔离家目录里的 gateway.pid。
func (f *fx) stopGateways() {
	pids, _ := filepath.Glob(filepath.Join(f.Root, "hermes-home-*", "gateway.pid"))
	for _, p := range pids {
		var g struct {
			PID int `json:"pid"`
		}
		if b, err := os.ReadFile(p); err == nil && json.Unmarshal(b, &g) == nil && g.PID > 1 {
			_ = syscall.Kill(g.PID, syscall.SIGTERM)
		}
	}
}
