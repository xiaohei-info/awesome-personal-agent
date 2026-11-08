//go:build e2e

package ops

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// opsPack 是验收用能力包（放在数据目录，不属于发行）：按 tag 在事项创建时触发任务型 agent，
// STUB_MODE 写在提示词里，由无头运行替身按提示词决定行为。规则初始 observe，由 Web 切到 auto。
var opsPack = map[string]string{
	"pack.yaml": `name: acc-ops
version: 1.0.0
description: 验收用：按 tag 触发任务型 agent，并可循环（健康视图造数）
event_rules:
  - id: run-hang
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-hang]}
    do: [{action: call_agent, prompt: acc-hang}]
  - id: once
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-once]}
    do: [{action: call_agent, prompt: acc-loop}]
  - id: run-hold
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-hold]}
    do: [{action: call_agent, prompt: acc-hold}]
  - id: probe
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-probe]}
    do: [{action: call_agent, prompt: acc-loop}]
  - id: bad-action
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-bad]}
    do: [{action: create_timer, after: 0s}]
  - id: loop
    mode: observe
    when: {event: item.created, kind: task}
    do: [{action: call_agent, prompt: acc-loop}]
webhook_schemes:
  - id: acc-token
    signature_header: X-Acc-Token
    algorithm: token
webhook_mappings:
  - id: acc-generic
    match: {header: {X-Acc-Event: created}}
    fields:
      id: "{$.id}"
      type: "acc.created"
      subject: "{$.title}"
`,
	"prompts/acc-hang.md": `---
agent: task
access: read
timeout: 4s
---
ACCHANG STUB_MODE=hang 事项 {item_id}：挂起直到超时。
`,
	"prompts/acc-hold.md": `---
agent: task
access: read
timeout: 2h
---
ACCHOLD STUB_MODE=hang 事项 {item_id}：长时间占用长时运行额度。
`,
	"prompts/acc-loop.md": `---
agent: task
access: read
timeout: 1m
---
ACCLOOP STUB_MODE=receipt 事项 {item_id}。
`,
}

const (
	harness    = "acc-ops-stub"
	rulePrefix = "acc-ops/event_rules/"
)

// opsFx 是装有验收包与替身 harness 的环境。cfg 追加到 config.yaml（须在 serve 之前，额度改动需要重启）。
func opsFx(t *testing.T, cfg string) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	for name, body := range opsPack {
		p := filepath.Join(e.Home, "packs", "acc-ops", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.ServeWith(cfg)
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	f := &fx{e, t}
	f.declareHarness()
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	return f
}

// declareHarness 声明一个包装脚本作为 harness：回执命令把触发事项转 done。
func (f *fx) declareHarness() {
	wrapper := filepath.Join(f.Root, "acc-harness.sh")
	receipt := filepath.Join(f.Root, "acc-receipt.sh")
	if err := os.WriteFile(receipt, []byte(fmt.Sprintf("#!/bin/sh\nexec %q item transition \"$1\" --to done --reason acc-receipt --evidence acc-receipt\n", f.Bin["apa"])), 0o755); err != nil {
		f.t.Fatal(err)
	}
	script := fmt.Sprintf(`#!/bin/sh
item=$(printf '%%s' "$*" | grep -o 'itm_[0-9a-f]*' | head -1)
export STUB_HARNESS_RECEIPT_CMD="%s $item"
exec %q "$@"
`, receipt, f.Bin["stub-harness"])
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		f.t.Fatal(err)
	}
	decl, _ := json.Marshal(map[string]any{"harness": harness, "headless": map[string]any{
		"command": []string{wrapper, "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "验收声明替身 harness"); r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/"+harness+"/1/confirm", "", 200)
}

// auto 在 Web 把验收包的规则切到 auto。
func (f *fx) auto(rules ...string) {
	f.t.Helper()
	for _, r := range rules {
		f.webOK("PATCH", "/rules/"+url.PathEscape(rulePrefix+r)+"/mode", `{"mode":"auto"}`, 200)
	}
}

// task 用 DEL 建一个带 tag 的任务事项（触发验收包里对应的规则），返回事项 id。
func (f *fx) task(title, tag string) string {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-open"], "--tags", tag, "--evidence", "验收触发")
	return decode[struct {
		ID string `json:"id"`
	}](f.t, r.Stdout).ID
}

// signal 读 `apa health` 里一个信号。
type signal struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Count  int             `json:"count"`
	Why    string          `json:"why"`
	Next   string          `json:"next"`
	Detail json.RawMessage `json:"detail"`
}

type healthView struct {
	Healthy bool     `json:"healthy"`
	Signals []signal `json:"signals"`
}

func (f *fx) health() healthView {
	f.t.Helper()
	return decode[healthView](f.t, f.apa(f.OBS, 0, "health").Stdout)
}

func (h healthView) sig(id string) signal {
	for _, s := range h.Signals {
		if s.ID == id {
			return s
		}
	}
	return signal{ID: id, OK: true}
}
