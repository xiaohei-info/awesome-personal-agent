//go:build live

// Package background 按验收文档 flow-bg-main-session（ACT-BGM-01..09）与 flow-bg-resume（ACT-RES-01..05）
// 用真实 apa 二进制、真实 Hermes（headless 插件声明的 `hermes chat --oneshot`）和真实模型端点走完整流程。
// 只能在 taiyi 的隔离环境运行（见 run-on-taiyi.sh）：Hermes 家目录必须由 APA_LIVE_HERMES_HOME 显式给出，
// 不读写 ~/.hermes。Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF），与 tests/live/mail 一致。
// 运行凭据只落在测试临时目录，不进入仓库或日志。
package background

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	packRule   = "background-run/event_rules/"
	liveRule   = "live-bg/event_rules/"
	waitRun    = 12 * time.Minute // 真实模型一次运行可能多轮工具调用
	exitDenied = "4"
	exitAuth   = "3"
)

// livePack 是部署方追加的规则：noresume 声明的 harness 与 Hermes 同构但不声明 resume（BGM-09、RES-03）。
var livePack = map[string]string{
	"pack.yaml": `name: live-bg
version: 1.0.0
description: 真实 Hermes 验收：指定 harness 与不支持续接
event_rules:
  - id: noresume
    mode: observe
    when: {event: item.phase_changed, to_phase: open, kind: [task], tag_any: [live-noresume]}
    do: [{action: start_background_run, runner: main_session, prompt: handle-item, access: edit, harness: hermes-noresume, cwd: run_dir}]
  - id: task-ask
    mode: observe
    when: {event: item.phase_changed, to_phase: open, kind: [task], tag_any: [live-task]}
    do: [{action: start_background_run, runner: task, prompt: run-task, access: edit, executor: work-host.cli, harness: hermes, cwd: run_dir}]
`,
}

func need(t *testing.T, k string) string {
	t.Helper()
	v := os.Getenv(k)
	if v == "" {
		t.Fatalf("缺少环境变量 %s（见 tests/live/background/run-on-taiyi.sh）", k)
	}
	return v
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

type live struct {
	*baseline.Env
	t      *testing.T
	out    string // 包装脚本为每次运行留下的 $out/<run id>/{args.txt,token}
	apaDir string
	hbin   string
	hhome  string
}

func setup(t *testing.T) *live {
	t.Helper()
	hbin, hhome := need(t, "APA_LIVE_HERMES_BIN"), need(t, "APA_LIVE_HERMES_HOME")
	if home, _ := os.UserHomeDir(); hhome == filepath.Join(home, ".hermes") || !strings.HasPrefix(hhome, "/tmp/apa-live.") {
		t.Fatalf("APA_LIVE_HERMES_HOME=%s 必须是 /tmp/apa-live.* 下的隔离目录", hhome)
	}
	e := baseline.New(t)
	e.Init()
	for name, body := range livePack {
		p := filepath.Join(e.Home, "packs", "live-bg", name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	prompt, err := os.ReadFile(filepath.Join(repoRoot(), "packs", "background-run", "prompts", "handle-item.md"))
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(e.Home, "packs", "live-bg", "prompts"), 0o755))
	must(t, os.WriteFile(filepath.Join(e.Home, "packs", "live-bg", "prompts", "handle-item.md"), prompt, 0o644))
	task, err := os.ReadFile(filepath.Join(repoRoot(), "packs", "background-run", "prompts", "run-task.md"))
	must(t, err)
	must(t, os.WriteFile(filepath.Join(e.Home, "packs", "live-bg", "prompts", "run-task.md"), task, 0o644))
	e.Serve()
	l := &live{Env: e, t: t, out: filepath.Join(e.Root, "out"), apaDir: filepath.Dir(e.Bin["apa"]), hbin: hbin, hhome: hhome}
	must(t, os.MkdirAll(l.out, 0o755))
	e.IssueCredentials()
	e.CreateProjects()
	l.installPlugin()
	l.declareHermes()
	// 执行者 work-host.cli 在本 harness 的调用方式（apa-tools 的 apa-executors.json）；账本只看到 id 与 kind。
	must(t, os.WriteFile(filepath.Join(hhome, "apa-executors.json"),
		[]byte(`[{"id":"work-host.cli","kind":"external-cli","scenarios":["coding"],"host":"work-host","command":["echo","{prompt}"]}]`), 0o600))
	t.Cleanup(l.stopGateway)
	return l
}

// installPlugin 对应 ACT-HCTX-01：在隔离 Hermes 家目录安装并启用 apa-tools 插件，新开会话不报错。
func (l *live) installPlugin() {
	pdir := filepath.Join(l.hhome, "plugins")
	must(l.t, os.MkdirAll(pdir, 0o755))
	link := filepath.Join(pdir, "apa-tools")
	_ = os.Remove(link)
	must(l.t, os.Symlink(filepath.Join(repoRoot(), "plugins", "hermes", "tools"), link))
	// `hermes plugins enable` 在无终端环境会卡在能力授权确认上，所以直接写 plugins.enabled（它做的就是这一步）。
	cfg := filepath.Join(l.hhome, "config.yaml")
	if cur, err := os.ReadFile(cfg); err != nil || !strings.Contains(string(cur), "- apa-tools") { // 同一家目录里多个测试各装一次，不能重复写 plugins 键
		must(l.t, err)
		f, err := os.OpenFile(cfg, os.O_APPEND|os.O_WRONLY, 0o600)
		must(l.t, err)
		_, err = f.WriteString("\nplugins:\n  enabled:\n    - apa-tools\n")
		must(l.t, err)
		must(l.t, f.Close())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, l.hbin, "plugins", "list")
	cmd.Env = append(os.Environ(), "HERMES_HOME="+l.hhome)
	out, err := cmd.CombinedOutput()
	if err != nil || !regexp.MustCompile(`apa-tools\s+│\s+enabled`).Match(out) {
		l.t.Fatalf("apa-tools 插件未加载: %v\n%s", err, out)
	}
}

// declareHermes 提交仓库里 plugins/hermes/headless/declaration.json（只把可执行文件换成设置 HERMES_HOME 的包装脚本，
// 内核按白名单不传 HERMES_HOME），并在 Web 确认；再声明一个去掉 resume 的同构 harness。
func (l *live) declareHermes() {
	wrapper := filepath.Join(l.Root, "hermes-wrapper.sh")
	script := fmt.Sprintf(`#!/bin/sh
export HERMES_HOME=%q
export PATH=%q:"$PATH"
d=%q/"$APA_RUN"
mkdir -p "$d"
printf '%%s\n' "$@" > "$d/args.txt"
printf '%%s' "$APA_TOKEN" > "$d/token"
exec %q "$@"
`, l.hhome, l.apaDir, l.out, l.hbin)
	must(l.t, os.WriteFile(wrapper, []byte(script), 0o755))
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "plugins", "hermes", "headless", "declaration.json"))
	must(l.t, err)
	for _, name := range []string{"hermes", "hermes-noresume"} {
		var d map[string]any
		must(l.t, json.Unmarshal(raw, &d))
		d["harness"] = name
		h := d["headless"].(map[string]any)
		cmd := h["command"].([]any)
		cmd[0] = wrapper
		if name == "hermes-noresume" {
			delete(h, "resume")
		}
		b, _ := json.Marshal(d)
		if r := l.Apa(l.DEL, string(b), "declare", "-", "--evidence", "真实 Hermes 无头运行声明"); r.Code != 0 {
			l.t.Fatalf("apa declare %s: %d %s%s", name, r.Code, r.Stdout, r.Stderr)
		}
		l.webOK("POST", "/settings/declarations/"+name+"/1/confirm", "", 200)
	}
}

// stopGateway 结束 Hermes 因 oneshot 顺带拉起的网关（只认本测试隔离家目录里的 pid 文件）。
func (l *live) stopGateway() {
	b, err := os.ReadFile(filepath.Join(l.hhome, "gateway.pid"))
	if err != nil {
		return
	}
	var g struct {
		PID  int    `json:"pid"`
		Home string `json:"hermes_home"`
	}
	if json.Unmarshal(b, &g) == nil && g.PID > 1 && g.Home == l.hhome {
		_ = syscall.Kill(g.PID, syscall.SIGTERM)
	}
}

func (l *live) webOK(method, path, body string, want int) string {
	l.t.Helper()
	r := l.Web(method, "/v1"+path, body)
	if r.Code != want {
		l.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (l *live) apa(token string, want int, args ...string) baseline.Result {
	l.t.Helper()
	r := l.Apa(token, "", args...)
	if r.Code != want {
		l.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (l *live) setMode(rule, mode string) {
	l.t.Helper()
	l.webOK("PATCH", "/rules/"+url.PathEscape(rule)+"/mode", `{"mode":"`+mode+`"}`, 200)
}

func (l *live) waitFor(what string, d time.Duration, cond func() bool) {
	l.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			l.t.Fatalf("等待超时（%v）：%s", d, what)
		}
		time.Sleep(time.Second)
	}
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

// openItem 在 acc-work 建 tag 事项（proposed）再转 open，触发 item.phase_changed→open。
func (l *live) openItem(title, body string, tags ...string) string {
	l.t.Helper()
	id := idOf(l.t, l.apa(l.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--body", body, "--phase", "proposed",
		"--project", l.Projects["acc-work"], "--tags", strings.Join(tags, ","), "--evidence", "live 触发").Stdout)
	l.apa(l.DEL, 0, "item", "transition", id, "--to", "open", "--reason", "live 进入 open", "--evidence", "live 进入 open")
	return id
}

type run struct {
	ID         string `json:"id"`
	Category   string `json:"category"`
	Runner     string `json:"runner"`
	Launch     string `json:"launch"`
	Target     string `json:"target"`
	State      string `json:"state"`
	Harness    string `json:"harness"`
	Access     string `json:"access"`
	Session    string `json:"session_id"`
	ChainDepth int    `json:"chain_depth"`
	Basis      struct {
		RuleID        string `json:"rule_id"`
		PolicyVersion int    `json:"policy_version"`
	} `json:"basis"`
}

func (l *live) runs(target string) []run {
	l.t.Helper()
	var out struct {
		Runs []run `json:"runs"`
	}
	must(l.t, json.Unmarshal([]byte(l.apa(l.OBS, 0, "run", "list", "--target", target).Stdout), &out))
	return out.Runs
}

// settled 等待目标事项上至少 n 个运行全部结束。
func (l *live) settled(target string, n int) []run {
	l.t.Helper()
	var rs []run
	l.waitFor("事项 "+target+" 的运行结束", waitRun, func() bool {
		rs = l.runs(target)
		if len(rs) < n {
			return false
		}
		for _, r := range rs {
			if r.State == "queued" || r.State == "running" {
				return false
			}
		}
		return true
	})
	return rs
}

func (l *live) show(id string) map[string]any {
	l.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	must(l.t, json.Unmarshal([]byte(l.apa(l.OBS, 0, "item", "show", id).Stdout), &out))
	return out.Item
}

func (l *live) phase(id string) string { s, _ := l.show(id)["phase"].(string); return s }

type event struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Reason  string         `json:"reason"`
	Payload map[string]any `json:"payload"`
}

func (l *live) events(object string) []event {
	l.t.Helper()
	var out struct {
		Events []event `json:"events"`
	}
	must(l.t, json.Unmarshal([]byte(l.webOK("GET", "/audit/events?limit=500&object="+url.QueryEscape(object), "", 200)), &out))
	return out.Events
}

func (l *live) children(parent string) []string {
	l.t.Helper()
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	must(l.t, json.Unmarshal([]byte(l.apa(l.OBS, 0, "item", "list", "--assignee", "work-host.cli").Stdout), &out))
	var ids []string
	for _, i := range out.Items {
		ids = append(ids, i.ID)
	}
	return ids
}

func (l *live) runFile(runID, name string) string {
	b, _ := os.ReadFile(filepath.Join(l.out, runID, name))
	return string(b)
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// step 运行一个验收动作；前一步未通过，后续不得开始。
func (l *live) step(name string, fn func(t *testing.T)) {
	parent := l.t
	ok := parent.Run(name, func(t *testing.T) {
		l.t, l.Env.T = t, t
		defer func() { l.t, l.Env.T = parent, parent }()
		fn(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}

func logJSON(t *testing.T, what string, v any) {
	b, _ := json.MarshalIndent(v, "", "  ")
	t.Logf("%s:\n%s", what, b)
}
