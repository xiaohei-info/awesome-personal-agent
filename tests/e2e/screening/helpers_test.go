//go:build e2e

package screening

import (
	"encoding/json"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 初筛返回延迟与超时是验收文档（60 秒 / 120 秒）的等比缩小：语义（返回前处于"初筛中"、超时按规则继续）不变，
// 但整条流程可以在一次 go test 里跑完。
const (
	slow       = 6 * time.Second // 对应"返回延迟 60 秒"
	timeoutMs  = 30000           // 对应"初筛超时 120 秒"
	okEscalate = "yes"           // 对话端点回答"是"：升级
	okNormal   = "no"            // 回答"否"且资料足够：不升级
)

type fx struct {
	*baseline.Env
	t *testing.T
}

func setup(t *testing.T) *fx {
	t.Helper()
	f := &fx{baseline.Setup(t), t}
	f.endpoint(timeoutMs)
	// 决策投递走渠道 acc-chan，且不合并：这样"没有投递"与"随后投递"都能用 CHAN 直接观察。
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"inject","decision":"channel:acc-chan","health":"web","reminder":"inject","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true},"web_only":{"categories":["external_commitment","funds","irreversible","permission_change","spec_activation"],"rules":[{"tag":"acc-finance","category":"funds"}]}}`, 200)
	return f
}

func (f *fx) endpoint(ms int) {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"model_endpoint": map[string]any{
		"url": f.ModelStub.URL(), "model": "acc-model", "api_key": "stub-key", "screening_timeout_ms": ms}})
	f.webOK("PUT", "/settings/model-endpoints", string(b), 200)
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

// stub 把 MODEL_STUB 上 web_only 初筛这道题的作答换成 answer/delay/status，并清空请求记录。
// 只配了生成端点，初筛由对话端点回答（§14.4）；status 非 0 表示端点失败。
func (f *fx) stub(answer string, delay time.Duration, status int) {
	f.ModelStub.Reset()
	f.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: baseline.ScreeningPhrase}, Answer: answer,
		DelayMS: int(delay / time.Millisecond), Status: status})
}

// screeningRequests 只数初筛（web-only-screening 提示词）的请求：决策卡片文案、一句话摘要是系统发起的另类调用，不属初筛。
func (f *fx) screeningRequests() int {
	n := 0
	for _, r := range f.ModelStub.Requests() {
		if strings.Contains(r.BodyText, baseline.ScreeningPhrase) {
			n++
		}
	}
	return n
}

// task 用 DEL 代本人建一个 open 的任务事项（assignee 为 work-host.cli）。
func (f *fx) task(title string, tags ...string) string {
	f.t.Helper()
	args := []string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建个任务"}
	if len(tags) > 0 {
		args = append(args, "--tags", strings.Join(tags, ","))
	}
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.DEL, 0, args...).Stdout), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %v", err)
	}
	return m.ID
}

// ask 用 WRK 写入决策请求（问题 "选哪个方案"，选项 A=方案A / B=方案B）。
func (f *fx) ask(id string, extra ...string) {
	f.t.Helper()
	args := append([]string{"ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A", "--free-text"}, extra...)
	f.apa(f.WRK, 0, args...)
}

// webItem 以本人身份读事项（Web 看得到初筛中的完整事项）。
func (f *fx) webItem(id string) map[string]any {
	f.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	body := f.webOK("GET", "/items/"+id, "", 200)
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		f.t.Fatal(err)
	}
	if out.Item == nil { // 兼容直接返回事项对象
		_ = json.Unmarshal([]byte(body), &out.Item)
	}
	return out.Item
}

func (f *fx) askOf(id string) map[string]any { a, _ := f.webItem(id)["ask"].(map[string]any); return a }

func (f *fx) pending(id string) bool { p, _ := f.askOf(id)["screening_pending"].(bool); return p }

func (f *fx) phase(id string) string { s, _ := f.webItem(id)["phase"].(string); return s }

func (f *fx) resolution(id string) map[string]any {
	r, _ := f.askOf(id)["resolution"].(map[string]any)
	return r
}

// waitCleared 等 screening_pending 清除（最多 limit）。
func (f *fx) waitCleared(id string, limit time.Duration) {
	f.t.Helper()
	stop := time.Now().Add(limit)
	for f.pending(id) {
		if time.Now().After(stop) {
			f.t.Fatalf("%s 的 screening_pending %v 内没有清除", id, limit)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

type step struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Reason  string         `json:"reason"`
	Payload map[string]any `json:"payload"`
}

func (f *fx) events(object, typ string) []step {
	f.t.Helper()
	var out struct {
		Events []step `json:"events"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/audit/events?limit=500&object="+object+"&type="+typ, "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Events
}

// hasEvent 报告对象上是否有 reason 含 sub 的指定类型事件。
func (f *fx) hasEvent(object, typ, sub string) bool {
	for _, e := range f.events(object, typ) {
		if strings.Contains(e.Reason, sub) {
			return true
		}
	}
	return false
}

// health 是 `apa health` 的原文，用来找初筛失败信号里的事项 id。
func (f *fx) health() string { return f.apa(f.OBS, 0, "health").Stdout }

// claim 用 CHAN 领取一次渠道投递，返回状态码与正文。
func (f *fx) claim(waitSec string) (int, string) {
	r := f.APIRaw("GET", "/v1/deliveries/next?wait="+waitSec, "", f.CHAN)
	return r.Code, r.Body
}

func (f *fx) kill9() {
	f.t.Helper()
	pid := f.ServePID()
	if pid == 0 {
		f.t.Fatal("apa serve 没有在运行")
	}
	if err := syscall.Kill(pid, syscall.SIGKILL); err != nil {
		f.t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}
