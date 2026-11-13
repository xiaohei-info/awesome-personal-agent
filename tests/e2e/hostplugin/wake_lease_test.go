//go:build e2e

package hostplugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	wakePack  = "acc-wake"
	wakeHN    = "acc-stub-wake"
	headlessR = wakePack + "/event_rules/wake-headless"
	wakeR     = wakePack + "/event_rules/wake-main"
	shortR    = wakePack + "/event_rules/wake-short"
)

// wakePackFiles 是附录 C.12 写法的后台执行规则：runner=main_session 的三条规则，分别走 acc-stub（未声明 wake）、
// acc-stub-wake（运行超时 10 分钟）、acc-stub-wake（运行超时 1 分钟，对应验收里"把规则的运行超时改为 1 分钟"）。
var wakePackFiles = map[string]string{
	"pack.yaml": `name: acc-wake
version: 1.0.0
description: 验收用：唤醒真实主会话（替身）
event_rules:
  - id: wake-headless
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-wake-headless]}
    do: [{action: start_background_run, runner: main_session, harness: acc-stub, prompt: handle-item, access: read, cwd: run_dir}]
  - id: wake-main
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-wake]}
    do: [{action: start_background_run, runner: main_session, harness: acc-stub-wake, prompt: handle-item, access: read, cwd: run_dir}]
  - id: wake-short
    mode: observe
    when: {event: item.created, kind: task, tag_any: [acc-wake-short]}
    do: [{action: start_background_run, runner: main_session, harness: acc-stub-wake, prompt: handle-short, access: read, cwd: run_dir}]
`,
	"prompts/handle-item.md":  "---\nrun_as: main_session\ntimeout: 10m\n---\nSTUB_MODE=no-receipt ACCWAKE 你在处理事项 {item_id}，本人不在场。\n",
	"prompts/handle-short.md": "---\nrun_as: main_session\ntimeout: 1m\n---\nACCWAKE 你在处理事项 {item_id}，本人不在场。\n",
}

type run struct {
	ID        string `json:"id"`
	Launch    string `json:"launch"`
	Runner    string `json:"runner"`
	Target    string `json:"target"`
	State     string `json:"state"`
	StartedAt string `json:"started_at"`
	Timeout   int    `json:"timeout"`
}

func (f *fx) runs(target string) []run {
	f.t.Helper()
	var out struct {
		Runs []run `json:"runs"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "run", "list", "--target", target).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Runs
}

func (f *fx) waitRun(target string) run {
	f.t.Helper()
	var r []run
	if !f.WaitFor(30*time.Second, func() bool { r = f.runs(target); return len(r) > 0 }) {
		f.t.Fatalf("事项 %s 30 秒内没有命中规则产出 Run\n%s", target, f.ServiceLog())
	}
	return r[0]
}

// task 建 tag 事项（open，由 DEL 代本人建）。
func (f *fx) tagged(title, tag string) string {
	f.t.Helper()
	return f.add("acc-open", title, "open", "--tags", tag)
}

// newWakeKernel 装上规则包后起内核，并建好凭据、项目、替身端点与 acc-stub 声明（未声明 main_session）。
func newWakeKernel(t *testing.T) *fx {
	t.Helper()
	e := baseline.New(t)
	e.Init()
	for name, body := range wakePackFiles {
		p := filepath.Join(e.Home, "packs", wakePack, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	e.Serve()
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.DeclareStubHarness()
	return &fx{e, t}
}

func (f *fx) autoRule(id string) {
	f.t.Helper()
	f.webOK("PATCH", "/rules/"+url.PathEscape(id)+"/mode", `{"mode":"auto"}`, 200)
}

// delivery 是网关插件领到的投递。
type wakeDelivery struct {
	ID    string `json:"id"`
	Item  string `json:"item"`
	Class string `json:"class"`
	Run   string `json:"run"`
	Raw   string `json:"-"`
}

// gatewayClaim 扮演网关插件：持 token 长轮询领取一条投递，没有返回 nil。
func (f *fx) gatewayClaim(token string, wait int) *wakeDelivery {
	f.t.Helper()
	r := f.APIRaw("GET", "/v1/deliveries/next?wait="+itoa(wait), "", token)
	if r.Code == http.StatusNoContent {
		return nil
	}
	if r.Code != http.StatusOK {
		f.t.Fatalf("领取投递: HTTP %d %s", r.Code, r.Body)
	}
	var d wakeDelivery
	if err := json.Unmarshal([]byte(r.Body), &d); err != nil {
		f.t.Fatal(err)
	}
	d.Raw = r.Body
	return &d
}

func itoa(n int) string { return strconv.Itoa(n) }

// event 是 Web 审计页看到的事件（只列验收关心的字段）。
type event struct {
	Seq    int    `json:"seq"`
	Type   string `json:"type"`
	Object string `json:"object"`
	Actor  struct {
		Principal string `json:"principal"`
		Run       string `json:"run"`
	} `json:"actor"`
	Payload map[string]any `json:"payload"`
	Raw     string         `json:"-"`
}

// events 返回账本里的全部审计事件（Web 看到的，不经任何过滤）。
func (f *fx) events() []event {
	f.t.Helper()
	body := f.webOK("GET", "/audit/events?limit=500", "", 200)
	var out struct {
		Events []json.RawMessage `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		f.t.Fatal(err)
	}
	evs := make([]event, 0, len(out.Events))
	for _, raw := range out.Events {
		var e event
		if err := json.Unmarshal(raw, &e); err != nil {
			f.t.Fatal(err)
		}
		e.Raw = string(raw)
		evs = append(evs, e)
	}
	return evs
}

func (f *fx) findEvents(pred func(event) bool) (got []event) {
	for _, e := range f.events() {
		if pred(e) {
			got = append(got, e)
		}
	}
	return
}

func ofRun(typ, run string) func(event) bool {
	return func(e event) bool { return e.Type == typ && e.Payload["run_id"] == run }
}

func (f *fx) waitEvent(what string, pred func(event) bool) event {
	f.t.Helper()
	var got []event
	if !f.WaitFor(30*time.Second, func() bool { got = f.findEvents(pred); return len(got) > 0 }) {
		f.t.Fatalf("30 秒内审计里没有：%s", what)
	}
	return got[0]
}

func (f *fx) phase(id string) string {
	f.t.Helper()
	var out struct {
		Item struct {
			Phase string `json:"phase"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "item", "show", id).Stdout), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Item.Phase
}

func (f *fx) runState(id string) (state string, r run) {
	f.t.Helper()
	if err := json.Unmarshal([]byte(f.apa(f.OBS, 0, "run", "show", id).Stdout), &r); err != nil {
		f.t.Fatal(err)
	}
	return r.State, r
}

func (f *fx) waitRunState(id, want string) {
	f.t.Helper()
	var got string
	if !f.WaitFor(30*time.Second, func() bool { got, _ = f.runState(id); return got == want }) {
		f.t.Fatalf("运行 %s 应转 %s，实际 %s", id, want, got)
	}
}

// exchange 扮演网关插件：用 token 换取运行 id 的运行凭据。成功时返回凭据明文。
func (f *fx) exchange(token, runID string, want int) (cred, expires string) {
	f.t.Helper()
	r := f.APIRaw("POST", "/v1/runs/"+runID+"/credential", "", token)
	if r.Code != want {
		f.t.Fatalf("换取运行凭据: HTTP %d（期望 %d）%s", r.Code, want, r.Body)
	}
	var out struct {
		Token     string `json:"token"`
		ExpiresAt string `json:"expires_at"`
	}
	if want == http.StatusOK {
		if err := json.Unmarshal([]byte(r.Body), &out); err != nil {
			f.t.Fatal(err)
		}
	}
	return out.Token, out.ExpiresAt
}

// runApaAs 以运行身份（APA_RUN + 运行凭据）运行 apa，期望退出码 want。
func (f *fx) asRun(runID, cred string, want int, env map[string]string, args ...string) (stdout, stderr string) {
	f.t.Helper()
	e := map[string]string{"APA_RUN": runID}
	for k, v := range env {
		e[k] = v
	}
	r := f.apaEnv(cred, want, e, args...)
	return r.Stdout, r.Stderr
}

// delAnswer 以 DEL 在 direct 会话里附原话代答一条决策，期望退出码 want。
func (f *fx) delAnswer(decision string, want int) {
	f.t.Helper()
	f.apaEnv(f.DEL, want, map[string]string{"APA_SESSION": "wak-other", "APA_SESSION_KIND": "direct"}, "decide", decision, "--option", "A", "--evidence", "选 A")
}

func (f *fx) grantFor(cred string) string {
	f.t.Helper()
	var creds struct {
		Credentials []struct{ ID, Label string } `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/settings/tokens", "", 200)), &creds); err != nil {
		f.t.Fatal(err)
	}
	id := ""
	for _, c := range creds.Credentials {
		if c.Label == cred {
			id = c.ID
		}
	}
	if id == "" {
		f.t.Fatalf("没有找到凭据 %s", cred)
	}
	body := `{"grantee":{"credential":"` + id + `"},"scope":{"projects":["` + f.Projects["acc-open"] + `"]},"actions":["decision.answer"],"expires_at":"` +
		time.Now().Add(time.Hour).UTC().Format(time.RFC3339) + `","reason":"away"}`
	var g struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.webOK("POST", "/settings/grants", body, 201)), &g); err != nil || g.ID == "" {
		f.t.Fatalf("签发授权单没有返回 id: %v", err)
	}
	return g.ID
}

const (
	srcID  = "src_wake"
	mAlice = "WAKE-ALICE-OK" // alice 在转发白名单内
	mBob   = "WAKE-BOB-OUT"  // bob 不在白名单内
)

// accScript 是一个脚本来源：输出队列目录里的事件，末行给游标。
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

// attachedChild 造一个已委派并带 PR 的子事项，并让白名单内外各一条跟进消息挂靠到它上面（alice 在转发白名单内，bob 不在）；返回子事项 id。
// 委派子事项上的消息保持待转发，由负责它的运行取走（§9.4）。
func (f *fx) attachedChild(parent string) string {
	f.t.Helper()
	child := idOf(f.t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", "wake-child", "--evidence", "委派带 PR 的子事项").Stdout)
	f.attachMessages(child)
	return child
}

// attachMessages 给已委派的子事项报 PR，脚本来源的两条消息按 PR 关联键挂靠上去。
func (f *fx) attachMessages(item string) {
	f.t.Helper()
	q := filepath.Join(f.Root, "wake-source")
	for _, d := range []string{"in", "done"} {
		if err := os.MkdirAll(filepath.Join(q, d), 0o755); err != nil {
			f.t.Fatal(err)
		}
	}
	script := filepath.Join(q, "src.sh")
	if err := os.WriteFile(script, []byte(accScript), 0o755); err != nil {
		f.t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"id": srcID, "type": "script", "source_type": "acc", "command": script + " " + q, "schedule": "1s", "timeout": 30})
	f.webOK("POST", "/sources", string(body), 201)
	f.webOK("PUT", "/settings/allowlists", `{"forward_allowlist":{"`+srcID+`":["alice"]}}`, 200)

	const pr = "https://example.invalid/acc/test-repo/pull/201"
	f.apa(f.WRK, 0, "report", item, "--outcome", "progress", "--summary", "已开 PR", "--pr", pr)
	ids := map[string]bool{}
	for i, m := range []struct{ subject, sender string }{{mAlice, "alice"}, {mBob, "bob"}} {
		id := fmt.Sprintf("ev-%d", i+1)
		ids[id] = true
		b, _ := json.Marshal(map[string]any{"id": id, "type": "comment.created", "subject": m.subject, "summary": "摘要 " + m.subject,
			"correlation_keys": []string{pr}, "sender": m.sender, "sender_verified": true, "occurred_at": time.Now().UTC().Format(time.RFC3339)})
		tmp := filepath.Join(q, fmt.Sprintf("tmp-%d", i))
		if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
			f.t.Fatal(err)
		}
		if err := os.Rename(tmp, filepath.Join(q, "in", fmt.Sprintf("%04d.jsonl", i+1))); err != nil {
			f.t.Fatal(err)
		}
	}
	if !f.WaitFor(30*time.Second, func() bool {
		var out struct {
			Events []struct {
				DedupeKey struct {
					EventID string `json:"event_id"`
				} `json:"dedupe_key"`
				Disposition string `json:"disposition"`
				Item        string `json:"item"`
			} `json:"events"`
		}
		r := f.APIRaw("GET", "/v1/sources/"+srcID+"/events?limit=100", "", f.OBS)
		if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil {
			return false
		}
		n := 0
		for _, e := range out.Events {
			if ids[e.DedupeKey.EventID] && e.Disposition == "attached" && e.Item == item {
				n++
			}
		}
		return n == len(ids)
	}) {
		f.t.Fatalf("两条消息没有在 30 秒内挂靠到事项上: %s", f.APIRaw("GET", "/v1/sources/"+srcID+"/events?limit=100", "", f.OBS).Body)
	}
}
