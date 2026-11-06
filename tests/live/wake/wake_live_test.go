//go:build live

// Package wake 按验收文档 flow-wake-lease 用真实 apa 内核和真实 Hermes 走唤醒租约流程。
// 当前覆盖 ACT-WAK-01、02；ACT-WAK-03..05 依赖 02 产出的唤醒运行，02 未通过时不得开始（见 PR 说明的上游缺陷）。
// 环境（均由运行者在 taiyi 隔离环境提供，见 /opt/apa-live/README.md）：
//
//	APA_LIVE_HERMES_BIN   真实 hermes 可执行文件
//	APA_LIVE_HERMES_HOME  隔离 HERMES_HOME 模板（只复制其中 config.yaml 与 .env，权限 0600，不共用）
//
// 凭据只经文件交接，不进入输出或证据。任一依赖缺失即失败（live 节点不得跳过或用替身）。
// 本文件的步骤顺序即验收顺序：前一步未通过，后续不得开始。
package wake

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
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
	hermesHome string // 本次运行的临时 HERMES_HOME（模板的 config.yaml 与 .env 的副本）
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
	bin, tmpl := os.Getenv("APA_LIVE_HERMES_BIN"), os.Getenv("APA_LIVE_HERMES_HOME")
	if bin == "" || tmpl == "" {
		t.Fatal("live 节点需要真实 Hermes：设置 APA_LIVE_HERMES_BIN 与 APA_LIVE_HERMES_HOME（taiyi: /opt/apa-live/creds/live.env）")
	}
	e := baseline.New(t)
	e.Init()
	for name, body := range pack {
		p := filepath.Join(e.Home, "packs", packName, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	e.Serve()
	f := &fx{Env: e, t: t, hermesBin: bin, hermesHome: filepath.Join(e.Root, "hermes-home")}
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

func (f *fx) task(title string) string {
	f.t.Helper()
	r := f.apaOK(f.DEL, "item", "add", "--kind", "task", "--title", title, "--phase", "open",
		"--project", f.Projects["acc-work"], "--tags", "acc-wake", "--evidence", "验收触发")
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

type delivery struct {
	ID    string `json:"id"`
	Class string `json:"class"`
	Route string `json:"route"`
	Run   string `json:"run"`
}

// wakeDeliveries 经网关插件的 token（DEL）从内核领取队列里的唤醒投递，不长轮询。
func (f *fx) nextWake() *delivery {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/deliveries/next?wait=0", "", f.DEL)
	if r.Code == 204 {
		return nil
	}
	if r.Code != 200 {
		f.t.Fatalf("GET /deliveries/next: HTTP %d %s", r.Code, r.Body)
	}
	var d delivery
	must(f.t, json.Unmarshal([]byte(r.Body), &d))
	return &d
}

func TestWakeLease(t *testing.T) {
	f := setup(t)

	f.step("ACT-WAK-01 显式声明唤醒：未声明前走 headless，确认 launch=wake 后规则走唤醒", func(t *testing.T) {
		f.declare("") // 未声明 main_session：默认 headless
		f.autoRule()
		before := f.task("acc-wake default headless")
		var rs []run
		f.waitFor("未声明唤醒时规则拉起一次后台运行", 60*time.Second, func() bool { rs = f.runs(before); return len(rs) > 0 })
		if rs[0].Launch != "headless" {
			t.Fatalf("未声明唤醒时 Run.launch = %q，期望 headless", rs[0].Launch)
		}
		f.waitFor("无头运行结束", 5*time.Minute, func() bool { rs = f.runs(before); return rs[0].State != "queued" && rs[0].State != "running" })

		f.declare("wake")
		woken := f.task("acc-wake after declare")
		deadline := time.Now().Add(30 * time.Second)
		for len(f.runs(woken)) == 0 {
			if time.Now().After(deadline) {
				t.Fatalf("声明并确认 launch=wake 后规则 30 秒内没有产生 launch=wake 的运行；事项 %s 的审计事件：%s", woken, f.audit(woken))
			}
			time.Sleep(250 * time.Millisecond)
		}
		if got := f.runs(woken)[0].Launch; got != "wake" {
			t.Fatalf("声明并确认 launch=wake 后 Run.launch = %q，期望 wake", got)
		}
	})

	f.step("ACT-WAK-02 产出唤醒投递（class=wake，带运行 id）", func(t *testing.T) {
		var d *delivery
		f.waitFor("内核产出 class=wake 的唤醒投递", 30*time.Second, func() bool { d = f.nextWake(); return d != nil })
		if d.Class != "wake" || d.Run == "" {
			t.Fatalf("投递 = %+v，期望 class=wake 且带运行 id", *d)
		}
	})
}
