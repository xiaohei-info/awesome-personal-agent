//go:build e2e

package followup

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
	ID       string          `json:"id"`
	Severity string          `json:"severity"`
	OK       bool            `json:"ok"`
	Count    int             `json:"count"`
	Why      string          `json:"why"`
	Next     string          `json:"next"`
	Detail   json.RawMessage `json:"detail"`
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

// script 在 Root/scripts 下写一个可执行脚本，返回路径。
func (f *fx) script(name, body string) string {
	f.t.Helper()
	dir := filepath.Join(f.Root, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	return p
}

// addScriptSource 在 Web 登记周期脚本来源（脚本来源只能在 Web 登记）。
func (f *fx) addScriptSource(id, command, schedule string, timeoutSec int) {
	f.t.Helper()
	body, _ := json.Marshal(map[string]any{"id": id, "type": "script", "source_type": "acc", "command": command, "schedule": schedule, "timeout": timeoutSec})
	f.webOK("POST", "/sources", string(body), http.StatusCreated)
}

// webHealth 是 Web 健康页读的接口（GET /v1/health）。
func (f *fx) webHealth() healthView {
	f.t.Helper()
	return decode[healthView](f.t, f.webOK("GET", "/health", "", http.StatusOK))
}

func (s signal) outcall(t *testing.T) outcallDetail {
	t.Helper()
	return decode[outcallDetail](t, string(s.Detail))
}

// userSteps 去掉 apa why 回放里内核后台（system 操作者）的步骤：模型替身故障下的卡片摘要起草会在安静期内
// 异步追加 run.finished 与 health.alert，它们不是健康视图发起的（与下方审计事件检查同一口径）。
func userSteps(t *testing.T, out string) string {
	t.Helper()
	var r struct {
		Steps []struct {
			Actor struct{ Type string } `json:"actor"`
			Seq   int64                 `json:"seq"`
			Type  string                `json:"type"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(out), &r); err != nil {
		t.Fatalf("apa why 输出不是 JSON: %v\n%s", err, out)
	}
	var kept []string
	for _, s := range r.Steps {
		if s.Actor.Type != "system" {
			kept = append(kept, fmt.Sprintf("%d:%s", s.Seq, s.Type))
		}
	}
	return strings.Join(kept, ",")
}

func rulePath(rule string) string { return strings.ReplaceAll(rulePrefix+rule, "/", "%2F") }

func (f *fx) rulesText() string {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/rules", "", f.OBS)
	if r.Code != http.StatusOK {
		f.t.Fatalf("GET /v1/rules: %d %s", r.Code, r.Body)
	}
	return r.Body
}

// eventsAfter 取 seq 之后的全部审计事件（Web 审计页的数据源，按序号分页读完）。
func (f *fx) eventsAfter(seq int64) []auditEvent {
	f.t.Helper()
	var out []auditEvent
	for {
		page := decode[struct {
			Events []auditEvent `json:"events"`
		}](f.t, f.webOK("GET", "/audit/events?limit=500&after="+itoa(seq), "", http.StatusOK)).Events
		out = append(out, page...)
		if len(page) < 500 {
			return out
		}
		seq = page[len(page)-1].Seq
	}
}

func (f *fx) lastSeq() int64 {
	f.t.Helper()
	var m int64
	for _, e := range f.eventsAfter(0) {
		m = max(m, e.Seq)
	}
	return m
}

// outcallDetail 是 outcall 信号的 detail。
type outcallDetail struct {
	Long, Short struct {
		Running int     `json:"running"`
		Queued  int     `json:"queued"`
		Longest float64 `json:"longest_wait_seconds"`
	}
	Timeouts []string `json:"timeouts"`
}

// the ten signal ids, in design §15.2 order.
var signalIDs = []string{"source_failures", "outcall", "delivery_backlog", "chain_trips", "agentsmd_drift", "pack_rejections", "model_unavailable", "unjudged_backlog", "rule_fallbacks", "receipt_failures", "other_alerts"}

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

// waitFor 轮询直到 cond 为真。
func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	for end := time.Now().Add(d); ; time.Sleep(200 * time.Millisecond) {
		if cond() {
			return
		}
		if time.Now().After(end) {
			f.t.Fatalf("%s（等待 %s 仍未出现）", what, d)
		}
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// newBare 是只有内核、凭据与项目的环境（不装验收包、不声明 harness）；cfg 追加到 config.yaml。
func newBare(t *testing.T, cfg string) *baseline.Env {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	e.ServeWith(cfg)
	e.IssueCredentials()
	e.CreateProjects()
	return e
}
