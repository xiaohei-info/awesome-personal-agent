//go:build e2e

package delivery

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

type fx struct {
	*baseline.Env
	t *testing.T
}

// setup 为每个流程建一套独立环境（独立 APA_HOME 与 apa serve），流程之间可并行。
func setup(t *testing.T) *fx { t.Helper(); return &fx{baseline.Setup(t), t} }

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

// disturb 是 PUT /settings/policy 的 disturb 段。
type disturb struct {
	Routes           map[string]string `json:"routes"`
	Quiet            *quiet            `json:"quiet,omitempty"`
	QuietExempt      bool              `json:"quiet_exempt,omitempty"`
	MergeWindowSec   int               `json:"merge_window_sec"`
	RemindOnDeadline bool              `json:"remind_on_deadline"`
}

type quiet struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Zone  string `json:"zone"`
}

// quietAround 返回覆盖当前时间、在 end 结束的静默时段（UTC，HH:MM）。
func quietAround(end time.Time) *quiet {
	return &quiet{Start: time.Now().Add(-time.Hour).UTC().Format("15:04"), End: end.UTC().Format("15:04"), Zone: "UTC"}
}

// setDisturb 以本人身份在 Web 设置页保存打扰策略。
func (f *fx) setDisturb(d disturb) {
	f.t.Helper()
	b, _ := json.Marshal(map[string]any{"disturb": d})
	f.webOK("PUT", "/settings/policy", string(b), http.StatusOK)
}

// route 表示"决策走渠道 acc-chan，其余类别只进 Web 未读"，是大多数流程的基线。
func route(extra map[string]string) map[string]string {
	r := map[string]string{"decision": "acc-chan", "reminder": "web", "brief": "web", "health": "web", "wake": "web"}
	for k, v := range extra {
		r[k] = v
	}
	return r
}

// task 用 DEL 代本人建一个 open 的任务事项（assignee 为 work-host.cli）。
func (f *fx) task(title string, extra ...string) string {
	f.t.Helper()
	args := append([]string{"item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建个任务"}, extra...)
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.apa(f.DEL, 0, args...).Stdout), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %v", err)
	}
	return m.ID
}

// ask 用 WRK 为事项写入带两个选项的决策请求。
func (f *fx) ask(id string, extra ...string) {
	f.t.Helper()
	f.apa(f.WRK, 0, append([]string{"ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A"}, extra...)...)
}

// delivery 是渠道 ext 领到的投递（只列验收关心的字段）。
type delivery struct {
	ID      string `json:"id"`
	Item    string `json:"item"`
	Class   string `json:"class"`
	Route   string `json:"route"`
	Version int64  `json:"version"`
	State   string `json:"state"`
	Content struct {
		Title     string   `json:"title"`
		Summary   string   `json:"summary"`
		Options   []string `json:"options"`
		Recommend string   `json:"recommend"`
		WebURL    string   `json:"web_url"`
	} `json:"content"`
	Raw string `json:"-"`
}

// claim 用 CHAN 长轮询领取一次；没有投递返回 nil。
func (f *fx) claim(waitSec int) *delivery {
	f.t.Helper()
	r := f.APIRaw("GET", fmt.Sprintf("/v1/deliveries/next?wait=%d", waitSec), "", f.CHAN)
	switch r.Code {
	case http.StatusNoContent:
		return nil
	case http.StatusOK:
		var d delivery
		if err := json.Unmarshal([]byte(r.Body), &d); err != nil {
			f.t.Fatal(err)
		}
		d.Raw = r.Body
		return &d
	}
	f.t.Fatalf("领取投递: HTTP %d %s", r.Code, r.Body)
	return nil
}

// claimUntil 持续领取直到 stop 为真或超时，返回途中领到的全部投递。
func (f *fx) claimUntil(timeout time.Duration, stop func([]*delivery) bool) []*delivery {
	f.t.Helper()
	var got []*delivery
	for end := time.Now().Add(timeout); time.Now().Before(end) && !stop(got); {
		if d := f.claim(5); d != nil {
			got = append(got, d)
		}
	}
	return got
}

func (f *fx) unread() []delivery {
	f.t.Helper()
	var out struct {
		Deliveries []delivery `json:"deliveries"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/deliveries/unread", "", http.StatusOK)), &out); err != nil {
		f.t.Fatal(err)
	}
	return out.Deliveries
}

func has(ds []*delivery, pred func(*delivery) bool) *delivery {
	for _, d := range ds {
		if pred(d) {
			return d
		}
	}
	return nil
}

// settle 是"一次提交之后投递应当已经就绪"的等待上限：web_only 初筛结束后由触达的周期 Tick（默认 30 秒）
// 接手，所以验收文档的 wait=5 在真实二进制上不够，这里取一个 Tick 加余量。
const settle = 45 * time.Second

// claimWithin 在 d 内持续领取，领到第一条即返回；超时返回 nil。
func (f *fx) claimWithin(d time.Duration) *delivery {
	f.t.Helper()
	got := f.claimUntil(d, func(g []*delivery) bool { return len(g) > 0 })
	if len(got) == 0 {
		return nil
	}
	return got[0]
}

// waitBacklog 等健康视图的投递积压里出现以 prefix 开头的投递 id 并返回它。
func (f *fx) waitBacklog(prefix string) string {
	f.t.Helper()
	for end := time.Now().Add(settle); time.Now().Before(end); time.Sleep(time.Second) {
		out := f.apa(f.OBS, 0, "health").Stdout
		if i := strings.Index(out, `"`+prefix); i >= 0 {
			rest := out[i+1:]
			return rest[:strings.Index(rest, `"`)]
		}
	}
	f.t.Fatalf("%s 内投递积压里没有 %s*", settle, prefix)
	return ""
}

func baselineNew(t *testing.T) *baseline.Env { t.Helper(); return baseline.New(t) }
