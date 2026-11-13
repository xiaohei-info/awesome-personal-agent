//go:build e2e

package judge

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// accRelay 是 flow-agent-cascade 的测试能力包（ACT-CAS-03 起）：relay-gen（生成型，relay_to: relay-task）、
// relay-task（任务型，result_fields: [result]）、ss-judge（single_shot 的判断型），分别由 tag 触发。
var accRelay = map[string]string{
	"pack.yaml": `name: acc-relay
version: 1.0.0
description: 验收用：生成型接力任务型、single_shot 判断型
event_rules:
  - id: relay
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-relay]}
    do: [{action: call_agent, prompt: relay-gen}]
  - id: relay-open
    mode: observe
    when: {event: item.phase_changed, to_phase: open, tag_any: [acc-relay-open]}
    do: [{action: call_agent, prompt: relay-gen}]
  - id: sum
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-sum]}
    do: [{action: call_agent, prompt: sum-task}]
  - id: ss
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-ss]}
    do: [{action: call_agent, prompt: ss-judge}]
  - id: auto-accept-note
    mode: observe
    when: {event: item.created, kind: note, phase: proposed}
    do: [{action: set_phase, phase: open}]
`,
	"prompts/relay-gen.md": `---
agent: generate
relay_to: relay-task
timeout: 30s
output_schema:
  type: object
  required: [insufficient_info]
  properties:
    insufficient_info: {type: boolean}
    note: {type: string}
---
ACCRELAYGEN 事项 {item_id}：写出一句说明；资料不足时把 insufficient_info 设为 true。
`,
	"prompts/relay-task.md": `---
agent: task
access: read
timeout: 1m
result_fields: [result]
---
ACCRELAYTASK 事项 {item_id}：查证后写回结果。
`,
	"prompts/sum-task.md": `---
agent: task
access: read
timeout: 1m
---
ACCSUMTASK 事项 {item_id}：自己回执（apa report），不声明 result_fields。
`,
	"prompts/ss-judge.md": `---
agent: judge
question_type: yes_no
answer_by: [classifier, chat]
band: {low: 0.3, high: 0.7}
on_undecided: no
single_shot: true
criteria: 验收用 single_shot 判断。
---
ACCSS 这条事项值得处理吗？
`,
}

const relayRule = "acc-relay/event_rules/"
const harnessName = "acc-cas-stub"

// installRelay 放入验收包、声明包装 harness、让规则生效。
func (f *fx) installRelay() {
	f.t.Helper()
	for name, body := range accRelay {
		p := filepath.Join(f.Home, "packs", "acc-relay", name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			f.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			f.t.Fatal(err)
		}
	}
	f.StopKernel()
	f.StartKernelAgain()
	f.conf = filepath.Join(f.Root, "stub.conf")
	wrapper := filepath.Join(f.Root, "acc-harness.sh")
	receipt := filepath.Join(f.Root, "acc-receipt.sh")
	must(f.t, os.WriteFile(receipt, []byte(fmt.Sprintf("#!/bin/sh\nexec %q report \"$1\" --outcome done --summary cas-receipt --link https://example.invalid/pr/1\n", f.Bin["apa"])), 0o755))
	must(f.t, os.WriteFile(wrapper, []byte(fmt.Sprintf(`#!/bin/sh
. %q
item=$(printf '%%s' "$*" | grep -o 'itm_[0-9a-f]*' | head -1)
export STUB_HARNESS_MODE="${MODE:-receipt}"
export STUB_HARNESS_RECEIPT_CMD="%s $item"
exec %q "$@"
`, f.conf, receipt, f.Bin["stub-harness"])), 0o755))
	f.mode("receipt")
	decl, _ := json.Marshal(map[string]any{"harness": harnessName, "headless": map[string]any{
		"command": []string{wrapper, "{prompt}"},
		"session": map[string]any{"extract": "$.session_id"},
		"access":  map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}},
	}})
	if r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "验收声明替身 harness"); r.Code != 0 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/"+harnessName+"/1/confirm", "", 200)
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harnessName+`"}`, 200)
	for _, r := range []string{"relay", "relay-open", "sum", "ss", "auto-accept-note"} {
		f.webOK("PATCH", "/rules/"+url.PathEscape(relayRule+r)+"/mode", `{"mode":"auto"}`, 200)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// mode 切换 STUB_HARNESS 的行为：receipt、no-receipt、exit1、summary-only。
func (f *fx) mode(mode string) {
	f.t.Helper()
	must(f.t, os.WriteFile(f.conf, []byte("MODE="+mode+"\n"), 0o644))
}

type run struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Target   string `json:"target"`
	State    string `json:"state"`
}

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

type auditEvent struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Reason  string         `json:"reason"`
	Payload map[string]any `json:"payload"`
}

func (f *fx) audit(object, typ string) []auditEvent {
	f.t.Helper()
	var out struct {
		Events []auditEvent `json:"events"`
	}
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + url.QueryEscape(object)
	}
	if typ != "" {
		path += "&type=" + typ
	}
	must(f.t, json.Unmarshal([]byte(f.webOK("GET", path, "", 200)), &out))
	return out.Events
}

// runFinished 是 category 类别的调用记录（run.finished，payload 带 category）。
func (f *fx) runFinished(category string) (out []auditEvent) {
	for _, e := range f.audit("", "run.finished") {
		if c, _ := e.Payload["category"].(string); c == category {
			out = append(out, e)
		}
	}
	return
}

func (f *fx) item(id string) map[string]any {
	f.t.Helper()
	m := jsonMap(f.t, f.apa(f.OBS, 0, "item", "show", id).Stdout)
	if it := sub(m, "item"); it != nil {
		return it
	}
	return m
}

func (f *fx) phase(id string) string { s, _ := f.item(id)["phase"].(string); return s }

// triggerItem 建一个带 tag 的任务事项，触发验收包里对应的规则。
func (f *fx) triggerItem(title, tag string) string {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-open"], "--tags", tag, "--evidence", "验收触发")
	return jsonMap(f.t, r.Stdout)["id"].(string)
}

func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	if !f.WaitFor(d, cond) {
		f.t.Fatalf("等待超时（%v）：%s\n%s", d, what, f.ServiceLog())
	}
}

// proposals 是建议流里（proposed 阶段）的事项 id → 标题。
func (f *fx) proposals() map[string]string {
	f.t.Helper()
	var out struct {
		Items []struct{ ID, Title string } `json:"items"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list", "--phase", "proposed").Stdout), &out))
	m := map[string]string{}
	for _, it := range out.Items {
		m[it.ID] = it.Title
	}
	return m
}

func (f *fx) rawJudgment(id string) map[string]any {
	f.t.Helper()
	return jsonMap(f.t, f.webOK("GET", "/judgments/"+id, "", 200))
}

func contains(s string, subs ...string) bool {
	for _, x := range subs {
		if !strings.Contains(s, x) {
			return false
		}
	}
	return true
}

// hasPhase 报告是否有标题为 title 的事项处在 phase（接力建出的目标事项与触发事项同名）。
func (f *fx) hasPhase(title, phase string) bool {
	f.t.Helper()
	var out struct {
		Items []struct{ Title, Phase string } `json:"items"`
	}
	must(f.t, json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "list").Stdout), &out))
	for _, it := range out.Items {
		if it.Title == title && it.Phase == phase {
			return true
		}
	}
	return false
}
