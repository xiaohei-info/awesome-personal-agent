//go:build e2e

package channel

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
	harness    = "acc-bg-stub"
	packName   = "acc-bg"
	rulePrefix = packName + "/event_rules/"
)

// bgPack 是验收用能力包：规则初始 observe，由 Web 切到 auto。运行参数只来自规则动作与 Web 设置。
var bgPack = map[string]string{
	"pack.yaml": `name: acc-bg
version: 1.0.0
description: 验收用：白名单全流程的后台运行
event_rules:
  - id: main-read
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-main]}
    do: [{action: start_background_run, runner: main_session, prompt: handle-item, access: read, cwd: run_dir}]
  - id: reread
    mode: observe
    when: {event: item.updated, kind: task, tag_any: [acc-reread]}
    do: [{action: start_background_run, runner: main_session, prompt: handle-item, access: read, cwd: run_dir}]
`,
	"prompts/handle-item.md": `---
run_as: main_session
timeout: 5m
---
ACCMAIN 你在处理事项 {item_id}，本人不在场。
`,
}

// preamble 是替身脚本的公共开头：ITEM 是本次运行的目标事项（提示词里的第一个事项 id）。
const preamble = `ITEM=$(printf '%s' "$*" | grep -o 'itm_[0-9a-f]*' | head -1)
rc() { echo $? > "$OUT/$1.rc"; }
`

type bg struct {
	*baseline.Env
	t    *testing.T
	conf string // 无头替身读取的开关文件（子进程不继承内核环境，开关只能走文件）
	out  string // 替身脚本写结果的目录
}

// setup 建环境：init、放入验收能力包、serve，再签发凭据、建项目并声明替身 harness。
func setupBG(t *testing.T) *bg {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	for name, body := range bgPack {
		p := filepath.Join(e.Home, "packs", packName, name)
		must(t, os.MkdirAll(filepath.Dir(p), 0o755))
		must(t, os.WriteFile(p, []byte(body), 0o644))
	}
	e.Serve()
	f := &bg{Env: e, t: t}
	e.IssueCredentials()
	e.CreateProjects()
	f.declareHarness()
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	return f
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// declareHarness 声明一个包装脚本作为 harness：按开关文件决定行为——先执行 SCRIPT（以运行者身份调 CLI，
// 环境来自内核注入的 APA_RUN、APA_TOKEN），再交给 stub-harness 按 MODE 收尾（summary-only、hang、exit1）。
func (f *bg) declareHarness() {
	f.conf = filepath.Join(f.Root, "stub.conf")
	f.out = filepath.Join(f.Root, "out")
	must(f.t, os.MkdirAll(f.out, 0o755))
	wrapper := filepath.Join(f.Root, "acc-harness.sh")
	script := fmt.Sprintf(`#!/bin/sh
. %q
printf '%%s\n' "$@" > args.txt
env > env.txt
export APA="%s" OUT=%q
# 会话 id：每次运行各自一个；带 --resume 时沿用被续接的会话 id（与真实 harness 同行为）。
SID="sess-$APA_RUN"; prev=""
for a in "$@"; do [ "$prev" = "--resume" ] && SID="$a"; prev="$a"; done
export STUB_HARNESS_SESSION_ID="$SID"
if [ -n "$LOSE_SESSION" ] && printf '%%s' "$*" | grep -q -- '--resume'; then
  echo "No session found matching $SID"; echo "No session found matching $SID" >&2; exit 1
fi
if [ -n "$SCRIPT" ]; then
  sh "$SCRIPT" "$@"
fi
export STUB_HARNESS_MODE="${MODE:-summary-only}"
[ -n "$HANG_FOR" ] && export STUB_HARNESS_HANG_FOR="$HANG_FOR"
exec %q "$@"
`, f.conf, f.Bin["apa"], f.out, f.Bin["stub-harness"])
	must(f.t, os.WriteFile(wrapper, []byte(script), 0o755))
	f.mode("summary-only", "")
	f.declare(harness, wrapper, "{prompt}", true)
	f.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"read","by_harness":{}}}`, 200)
}

func (f *bg) declare(name, wrapper, arg string, resume bool) {
	h := map[string]any{
		"command":     []string{wrapper, arg},
		"session":     map[string]any{"extract": "$.session_id"},
		"access":      map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
		"exit_codes":  map[string]any{"ok": []int{0}, "session_missing": []int{1}},
		"exit_output": map[string]any{"session_missing": []string{"No session found matching"}},
	}
	if resume {
		h["resume"] = []string{"--resume", "{session_id}"}
	}
	f.declareRaw(name, h)
}

func (f *bg) declareRaw(name string, h map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"harness": name, "headless": h})
	if r := f.Apa(f.DEL, string(b), "declare", "-", "--evidence", "验收声明替身 harness"); r.Code != 0 {
		f.t.Fatalf("apa declare %s: %d %s%s", name, r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/"+name+"/1/confirm", "", 200)
}

// mode 切换替身行为：mode 为 stub-harness 的收尾模式，script 是以运行者身份执行的 shell 脚本正文（可空）。
func (f *bg) mode(mode, script string, extra ...string) {
	f.t.Helper()
	sp := ""
	if script != "" {
		sp = filepath.Join(f.Root, "script.sh")
		must(f.t, os.WriteFile(sp, []byte(script), 0o644))
	}
	body := "MODE=" + mode + "\nSCRIPT=" + sp + "\n" + strings.Join(extra, "\n") + "\n"
	must(f.t, os.WriteFile(f.conf, []byte(body), 0o644))
}

// outFile 读替身脚本写下的结果文件（不存在返回空串）。
func (f *bg) outFile(name string) string {
	b, _ := os.ReadFile(filepath.Join(f.out, name))
	return strings.TrimSpace(string(b))
}

func (f *bg) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *bg) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *bg) auto(rules ...string) {
	f.t.Helper()
	for _, r := range rules {
		f.webOK("PATCH", "/rules/"+url.PathEscape(rulePrefix+r)+"/mode", `{"mode":"auto"}`, 200)
	}
}

func (f *bg) setMode(rule, mode string) {
	f.t.Helper()
	f.webOK("PATCH", "/rules/"+url.PathEscape(rulePrefix+rule)+"/mode", `{"mode":"`+mode+`"}`, 200)
}

// task 用 DEL 在 acc-work 建一个带 tag 的任务事项（item.created 触发验收包里对应的规则）。
func (f *bg) task(title, project string, tags ...string) string {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open",
		"--project", f.Projects[project], "--tags", strings.Join(tags, ","), "--evidence", "验收触发")
	return idOf(f.t, r.Stdout)
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

type run struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Runner   string `json:"runner"`
	Launch   string `json:"launch"`
	Target   string `json:"target"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Harness  string `json:"harness"`
	Access   string `json:"access"`
	Session  string `json:"session_id"`

	ChainDepth int `json:"chain_depth"`
	Basis      struct {
		RuleID        string `json:"rule_id"`
		PolicyVersion int    `json:"policy_version"`
	} `json:"basis"`
}

func (f *bg) runs(args ...string) []run {
	f.t.Helper()
	var out struct {
		Runs []run `json:"runs"`
	}
	r := f.apa(f.OBS, 0, append([]string{"run", "list"}, args...)...)
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		f.t.Fatalf("apa run list 输出不是 JSON: %v\n%s", err, r.Stdout)
	}
	return out.Runs
}

func (f *bg) runsOf(target string) []run {
	f.t.Helper()
	return f.runs("--target", target)
}

type event struct {
	Seq        int64          `json:"seq"`
	Type       string         `json:"type"`
	Object     string         `json:"object"`
	Reason     string         `json:"reason"`
	ChainDepth int            `json:"chain_depth"`
	Payload    map[string]any `json:"payload"`
	Actor      struct {
		Type, Principal, Via string
	} `json:"actor"`
}

// events 取审计事件（Web 审计页的数据源）。
func (f *bg) events(object, typ string) []event {
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

func (f *bg) item(id string) map[string]any {
	f.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out))
	return out.Item
}

func (f *bg) phase(id string) string { s, _ := f.item(id)["phase"].(string); return s }

func (f *bg) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("等待超时（%v）：%s", d, what)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// settled 等待目标事项上的运行全部结束并返回它们（至少 n 个）。
func (f *bg) settled(target string, n int) []run {
	f.t.Helper()
	var rs []run
	f.waitFor("事项 "+target+" 的运行结束", 40*time.Second, func() bool {
		rs = f.runsOf(target)
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

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// step 运行一个验收动作；子测试期间 bg 与环境脚本的致命错误记在该子测试上。
func (f *bg) step(name string, fn func(t *testing.T)) {
	parent := f.t
	ok := parent.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn(t)
	})
	// 默认遵守验收顺序：前一步未通过，后续不得开始。APA_E2E_KEEPGOING=1 只用于一次列全所有失败项。
	if !ok && os.Getenv("APA_E2E_KEEPGOING") == "" {
		parent.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}
func toJSON(v any) string { b, _ := json.Marshal(v); return string(b) }

// idFrom 取文本里第一个事项 id。
func idFrom(s string) string {
	i := strings.Index(s, "itm_")
	if i < 0 {
		return ""
	}
	j := i + 4
	for j < len(s) && strings.IndexByte("0123456789abcdef", s[j]) >= 0 {
		j++
	}
	return s[i:j]
}

// listAssignee 列出指派给某执行者的事项 id。
func (f *bg) listAssignee(assignee string) []string {
	f.t.Helper()
	var out struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list", "--assignee", assignee).Stdout), &out))
	var ids []string
	for _, i := range out.Items {
		ids = append(ids, i.ID)
	}
	return ids
}

func (f *bg) projectNamed(name string) bool {
	f.t.Helper()
	return strings.Contains(f.apa(f.OBS, 0, "project", "list").Stdout, `"`+name+`"`)
}

// sessionOf 是内核记在事项上的该运行的会话 id：运行结束时按 stdout 末段摘要写进 run.finished 事件（apa why 以 DEL 读取）。
func (f *bg) sessionOf(item, runID string) string {
	f.t.Helper()
	var out struct {
		Steps []struct {
			Type  string `json:"type"`
			Actor struct {
				Run string `json:"run"`
			} `json:"actor"`
			Payload struct {
				Session string `json:"session_id"`
			} `json:"payload"`
		} `json:"steps"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.DEL, 0, "why", item).Stdout), &out))
	for _, s := range out.Steps {
		if s.Type == "run.finished" && s.Actor.Run == runID {
			return s.Payload.Session
		}
	}
	return ""
}

// draftRequests 是 MODEL_STUB 收到的、含 mark 的生成请求（起草）。
func (f *bg) draftRequests(mark string) []string {
	var out []string
	for _, r := range f.ModelStub.Requests() {
		if !baseline.IsJudgeRequest(r) && strings.Contains(r.BodyText, mark) {
			out = append(out, r.BodyText)
		}
	}
	return out
}
