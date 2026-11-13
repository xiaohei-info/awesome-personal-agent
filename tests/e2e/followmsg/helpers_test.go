//go:build e2e

// Package followmsg 是全流程验收 flow-follow-messages、flow-triage-failure 的端到端测试
// （验收文档 ACT-MSG-*、ACT-TRI-*）。真实 apa 二进制（apa init、apa serve）+ tests/e2e/baseline 共用环境脚本，
// 来源是登记在内核里的验收脚本 ACC_SOURCE（真实拉取路径），模型端点是 MODEL_STUB。
// Web 步骤走 Web 页面调用的同一套 HTTP API（已登录会话 + CSRF）；本包不需要真实 Hermes 与浏览器。
package followmsg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 退出码（附录 B.1）。
const (
	exitInvalid  = 2
	exitDenied   = 4
	exitNotFound = 5
	exitConflict = 6
	workHost     = "work-host.cli"
	srcID        = "src_acc"
)

type kit struct {
	*baseline.Env
	t     *testing.T
	queue string // ACC_SOURCE 的待输出目录：放一个 .jsonl 文件，下个周期输出并移走
	seq   int
}

// accScript 是 ACC_SOURCE：把收到的 stdin 存盘，输出队列目录里的事件，末行给游标。
const accScript = `#!/bin/sh
Q="$1"
cat > "$Q/last-stdin.json"
for f in "$Q"/in/*.jsonl; do
  [ -e "$f" ] || continue
  cat "$f"
  mv "$f" "$Q/done/"
done
echo '{"cursor":"c'"$(ls "$Q/done" | wc -l | tr -d ' ')"'"}'
`

// accRules 是验收用的能力包（只含确定性规则；判断型沿用内置 inbound-triage 包）。
const accRules = `name: acc-inbound
version: 1.0.0
description: 验收用来源规则
source_rules:
  - id: acc-proposed
    match: {source_type: acc, event_type: [issue.opened]}
    kind: task
    project_by: "fixed:%s"
    then: proposed
  - id: fm-reply
    match: {source_type: acc, event_type: [acc.reply]}
    on_update: attach
    then: screen
  - id: acc-auto
    match: {source_type: acc, event_type: [issue.auto]}
    kind: task
    project_by: "fixed:%s"
    then: proposed
    accept: auto
    mode: observe
`

// newKit 建好环境：init → 项目 → 验收能力包 → serve → 凭据 → MODEL_STUB → 登记 ACC_SOURCE。
func newKit(t *testing.T) *kit {
	t.Helper()
	e := baseline.New(t)
	k := &kit{Env: e, t: t, queue: filepath.Join(e.Root, "acc-source")}
	e.Init()
	e.Serve()
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.DeclareStubHarness()
	pack := filepath.Join(e.Home, "packs", "acc-inbound")
	k.must(os.MkdirAll(pack, 0o755))
	k.must(os.WriteFile(filepath.Join(pack, "pack.yaml"), []byte(fmt.Sprintf(accRules, e.Projects["acc-work"], e.Projects["acc-open"])), 0o644))
	// 用户包里的规则取自己包内的提示词模板：把内置 inbound-triage 的初筛与起草模板复制进来，规则才能 then: screen。
	for _, f := range []string{"triage-screen.md", "triage-draft.md"} {
		b, err := os.ReadFile(filepath.Join(repoRoot(), "packs", "inbound-triage", "prompts", f))
		k.must(err)
		k.must(os.MkdirAll(filepath.Join(pack, "prompts"), 0o755))
		k.must(os.WriteFile(filepath.Join(pack, "prompts", f), b, 0o644))
	}
	e.StopKernel()
	e.StartKernelAgain()

	k.must(os.MkdirAll(filepath.Join(k.queue, "in"), 0o755))
	k.must(os.MkdirAll(filepath.Join(k.queue, "done"), 0o755))
	script := filepath.Join(k.queue, "acc-source.sh")
	k.must(os.WriteFile(script, []byte(accScript), 0o755))
	body, _ := json.Marshal(map[string]any{"id": srcID, "type": "script", "source_type": "acc",
		"command": script + " " + k.queue, "schedule": "1s", "timeout": 30})
	k.webOK("POST", "/v1/sources", string(body), 201)
	return k
}

func repoRoot() string {
	_, f, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(f), "..", "..", "..")
}

func (k *kit) must(err error) {
	k.t.Helper()
	if err != nil {
		k.t.Fatal(err)
	}
}

// step 运行一个验收动作；子测试期间 kit 与环境脚本的致命错误记在该子测试上。
func (k *kit) step(name string, f func(t *testing.T)) {
	k.t.Helper()
	parent := k.t
	ok := parent.Run(name, func(t *testing.T) {
		k.t, k.Env.T = t, t
		defer func() { k.t, k.Env.T = parent, parent }()
		f(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续步骤不得开始", name)
	}
}

func (k *kit) webOK(method, path, body string, want int) string {
	k.t.Helper()
	r := k.Web(method, path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (k *kit) expect(label string, r baseline.Result, code int) {
	k.t.Helper()
	if r.Code != code {
		k.t.Fatalf("%s: 退出码 %d，期望 %d\nstdout=%s\nstderr=%s", label, r.Code, code, r.Stdout, r.Stderr)
	}
}

func (k *kit) idOf(label string, r baseline.Result) string {
	k.t.Helper()
	k.expect(label, r, 0)
	var o struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &o); err != nil || o.ID == "" {
		k.t.Fatalf("%s 没有返回 id: %q", label, r.Stdout)
	}
	return o.ID
}

// Ev 是 ACC_SOURCE 要输出的一条标准事件。
type Ev struct {
	ID       string   `json:"id"`
	Type     string   `json:"type"`
	Subject  string   `json:"subject"`
	Summary  string   `json:"summary,omitempty"`
	Detail   string   `json:"detail,omitempty"`
	Keys     []string `json:"correlation_keys,omitempty"`
	Sender   string   `json:"sender,omitempty"`
	Verified bool     `json:"sender_verified"`
	At       string   `json:"occurred_at"`
}

// emit 让 ACC_SOURCE 在下一个周期输出这条事件，返回事件 id。
func (k *kit) emit(typ, subject string, keys ...string) string {
	k.t.Helper()
	k.seq++
	e := Ev{ID: fmt.Sprintf("ev-%d", k.seq), Type: typ, Subject: subject, Summary: "摘要 " + subject, Keys: keys,
		Sender: "alice", Verified: true, At: time.Now().UTC().Format(time.RFC3339)}
	b, _ := json.Marshal(e)
	tmp := filepath.Join(k.queue, fmt.Sprintf("tmp-%d", k.seq))
	k.must(os.WriteFile(tmp, append(b, '\n'), 0o644))
	k.must(os.Rename(tmp, filepath.Join(k.queue, "in", fmt.Sprintf("%04d.jsonl", k.seq))))
	return e.ID
}

// SrcEvent 是来源页的一条事件（GET /sources/{id}/events）。
type SrcEvent struct {
	ID        string `json:"id"`
	DedupeKey struct {
		Domain  string `json:"domain"`
		EventID string `json:"event_id"`
	} `json:"dedupe_key"`
	Disposition string `json:"disposition"`
	Item        string `json:"item"`
	AttachState string `json:"attach_state"`
	Subject     string `json:"subject"`
}

func (k *kit) events() []SrcEvent {
	k.t.Helper()
	r := k.APIRaw("GET", "/v1/sources/"+srcID+"/events?limit=500", "", k.OBS)
	if r.Code != 200 {
		k.t.Fatalf("来源事件: %d %s", r.Code, r.Body)
	}
	var out struct {
		Events []SrcEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
		k.t.Fatal(err)
	}
	return out.Events
}

// event 取某条来源事件（按 ACC_SOURCE 给的事件 id，即去重键的末段）。
func (k *kit) event(id string) (SrcEvent, bool) {
	for _, e := range k.events() {
		if e.DedupeKey.EventID == id {
			return e, true
		}
	}
	return SrcEvent{}, false
}

func (k *kit) waitFor(what string, d time.Duration, cond func() bool) {
	k.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			k.t.Fatalf("%s: %v 内未发生", what, d)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// settled 等 ACC_SOURCE 的事件被拉取并过完流水线（disposition 不再是 pending），返回该事件。
func (k *kit) settled(id string) SrcEvent {
	k.t.Helper()
	var ev SrcEvent
	k.waitFor("事件 "+id+" 完成处置", 20*time.Second, func() bool {
		var ok bool
		ev, ok = k.event(id)
		return ok && ev.Disposition != "pending"
	})
	return ev
}
