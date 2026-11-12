//go:build e2e

package agents

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// accPack 是验收用能力包：按 tag 在事项创建时触发专属 agent。规则初始 observe，由 Web 切到 auto。
var accPack = map[string]string{
	"pack.yaml": `name: acc-agents
version: 1.0.0
description: 验收用：按 tag 触发专属 agent
event_rules:
  - id: run-task
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-task]}
    do: [{action: call_agent, prompt: acc-task}]
  - id: run-gen
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-gen]}
    do: [{action: call_agent, prompt: acc-gen}]
  - id: run-timeout
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-timeout]}
    do: [{action: call_agent, prompt: acc-timeout}]
  - id: auto-accept-note
    mode: observe
    when: {event: item.created, kind: note, phase: proposed}
    do: [{action: set_phase, phase: open}]
`,
	"prompts/acc-task.md": `---
agent: task
access: read
timeout: 5m
---
ACCTASK 事项 {item_id}：查证后用 apa report {item_id} --outcome done --summary ok 回执。
`,
	"prompts/acc-timeout.md": `---
agent: task
access: read
timeout: 1m
---
ACCTIMEOUT 事项 {item_id}：本模板超时 1 分钟。
`,
	"prompts/acc-gen.md": `---
agent: generate
timeout: 30s
output_schema:
  type: object
  required: [insufficient_info]
  properties:
    insufficient_info: {type: boolean}
    note: {type: string}
---
ACCGEN 事项 {item_id}：写出一句说明；资料不足时把 insufficient_info 设为 true。
`,
}

const rulePrefix = "acc-agents/event_rules/"

type fx struct {
	*baseline.Env
	t       *testing.T
	conf    string // wrapper 读取的开关文件
	wrapper string
	stub    string // 声明的 harness 名
}

const harness = "acc-agents-stub"

// setup 建环境：先 init、放入验收能力包，再 serve；cfg 追加到 config.yaml（改额度需要重启）。
func setup(t *testing.T, cfg string) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	for name, body := range accPack {
		p := filepath.Join(e.Home, "packs", "acc-agents", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.Serve()
	f := &fx{Env: e, t: t}
	if cfg != "" {
		f.restartWithConfig(cfg)
	}
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	f.declareHarness()
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	return f
}

func (f *fx) restartWithConfig(cfg string) {
	f.t.Helper()
	f.StopKernel()
	p := filepath.Join(f.Home, "config.yaml")
	b, err := os.ReadFile(p)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, append(b, []byte(cfg)...), 0o644); err != nil {
		f.t.Fatal(err)
	}
	f.StartKernelAgain()
}

// declareHarness 声明一个包装脚本作为 harness：脚本按开关文件决定 stub-harness 的行为，
// 并把回执命令（apa report）指向本次运行的目标事项。子进程不继承内核环境，开关只能走文件。
func (f *fx) declareHarness() {
	f.conf = filepath.Join(f.Root, "stub.conf")
	f.wrapper = filepath.Join(f.Root, "acc-harness.sh")
	receipt := filepath.Join(f.Root, "acc-receipt.sh")
	if err := os.WriteFile(receipt, []byte(fmt.Sprintf("#!/bin/sh\nexec %q item transition \"$1\" --to done --reason acc-receipt --evidence acc-receipt 2>>%q\n", f.Bin["apa"], filepath.Join(f.Root, "receipt.err"))), 0o755); err != nil {
		f.t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
. %q
printf '%%s\n' "$@" > args.txt
item=$(printf '%%s' "$*" | grep -o 'itm_[0-9a-f]*' | head -1)
export STUB_HARNESS_MODE="${MODE:-receipt}"
[ -n "$HANG_FOR" ] && export STUB_HARNESS_HANG_FOR="$HANG_FOR"
[ -n "$SPAWN" ] && export STUB_HARNESS_SPAWN_CHILD=1
export STUB_HARNESS_RECEIPT_CMD="%s $item"
exec %q "$@"
`, f.conf, receipt, f.Bin["stub-harness"])
	if err := os.WriteFile(f.wrapper, []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	f.mode("receipt")
	decl, _ := json.Marshal(map[string]any{"harness": harness, "headless": map[string]any{
		"command": []string{f.wrapper, "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "验收声明替身 harness"); r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/"+harness+"/1/confirm", "", 200)
}

// mode 切换 STUB_HARNESS 的行为：receipt、no-receipt、exit1、hang、summary-only；extra 是附加的 shell 赋值（HANG_FOR、SPAWN）。
func (f *fx) mode(mode string, extra ...string) {
	f.t.Helper()
	body := "MODE=" + mode + "\n" + strings.Join(extra, "\n") + "\n"
	if err := os.WriteFile(f.conf, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
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

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// auto 在 Web 把验收包的规则切到 auto。
func (f *fx) auto(rules ...string) {
	f.t.Helper()
	for _, r := range rules {
		f.webOK("PATCH", "/rules/"+url.PathEscape(rulePrefix+r)+"/mode", `{"mode":"auto"}`, 200)
	}
}

// task 用 DEL 建一个带 tag 的任务事项（触发验收包里对应的规则）。
func (f *fx) task(title, tag string) string {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-open"], "--tags", tag, "--evidence", "验收触发")
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
	Target   string `json:"target"`
	State    string `json:"state"`
	ExitCode *int   `json:"exit_code"`
	Harness  string `json:"harness"`
}

// runs 是 `apa run list` 的结果（可带筛选参数）。
func (f *fx) runs(args ...string) []run {
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

type event struct {
	Seq    int64  `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Reason string `json:"reason"`
	Actor  struct {
		Type, Principal, Via string
	} `json:"actor"`
	Payload map[string]any `json:"payload"`
}

// events 取审计事件（Web 审计页的数据源）。
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
		path += "&type=" + typ
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", path, "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

// agentCalls 是专属 agent 的调用记录（run.finished，payload 带 category）；category 为空表示不限。
func (f *fx) agentCalls(object, category string) []event {
	f.t.Helper()
	var out []event
	for _, e := range f.events(object, "run.finished") {
		if c, _ := e.Payload["category"].(string); category == "" || c == category {
			out = append(out, e)
		}
	}
	return out
}

// templateCalls 只留指定提示词模板的调用：系统发起的摘要、卡片文案同属 generate，但模板不同。
func (f *fx) templateCalls(object, category, template string) []event {
	f.t.Helper()
	var out []event
	for _, e := range f.agentCalls(object, category) {
		if e.Payload["template"] == template {
			out = append(out, e)
		}
	}
	return out
}

// modelRequests 只留正文含 mark 的模型请求：系统发起的摘要、卡片文案也走同一个端点，不属被验收的调用。
func (f *fx) modelRequests(mark string) []modelstub.Request {
	var out []modelstub.Request
	for _, r := range f.ModelStub.Requests() {
		if strings.Contains(r.BodyText, mark) {
			out = append(out, r)
		}
	}
	return out
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

func (f *fx) phase(id string) string { s, _ := f.item(id)["phase"].(string); return s }

// waitFor 轮询到条件成立，超时则失败。
func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("等待超时（%v）：%s", d, what)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// taskRunOf 返回事项触发出的任务型 Run（目标事项由 call_agent 新建，标题与触发事项相同）。
func (f *fx) taskRuns() []run { return f.runs("--category", "task") }

// targetOf 取任务型运行的目标事项。
func (f *fx) titled(title string) []string {
	f.t.Helper()
	var out struct {
		Items []struct {
			ID    string `json:"id"`
			Title string `json:"title"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list").Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	var ids []string
	for _, it := range out.Items {
		if it.Title == title {
			ids = append(ids, it.ID)
		}
	}
	return ids
}

// capture 以 WRK 提交一段对话增量；session 区分同一内容的不同会话。
func (f *fx) capture(project, text, session string) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]string{"text": text, "project": f.Projects[project]})
	req, _ := http.NewRequest("POST", f.URL+"/v1/capture", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+f.WRK)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-APA-Session", session)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"captured":true`) {
		f.t.Fatalf("capture: %d %s", resp.StatusCode, b)
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

func reply(content string) modelstub.Reply { return modelstub.Reply{Content: content} }
