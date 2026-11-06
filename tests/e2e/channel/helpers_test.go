//go:build e2e

package channel

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

// setup 为每个流程建一套独立环境（独立 APA_HOME 与 apa serve）；本人在 Web 上把决策路由到渠道 acc-chan，
// 并把 alice 登记为 acc-chan 的回答人。
func setup(t *testing.T) *fx { t.Helper(); return wrap(t, baseline.Setup(t)) }

func wrap(t *testing.T, e *baseline.Env) *fx {
	t.Helper()
	f := &fx{e, t}
	f.putPolicy("/settings/policy", map[string]any{"disturb": map[string]any{
		"routes": map[string]string{"decision": "acc-chan", "reminder": "web", "brief": "web", "health": "web", "wake": "web"}}})
	f.putPolicy("/settings/allowlists", map[string]any{"channel_answerers": map[string][]string{"acc-chan": {"alice"}}})
	return f
}

func (f *fx) putPolicy(path string, body map[string]any) {
	f.t.Helper()
	b, _ := json.Marshal(body)
	f.webOK("PUT", path, string(b), http.StatusOK)
}

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

// decision 用 DEL 建一个任务，再用 WRK 为它写入带两个选项的决策请求，返回事项 id。
func (f *fx) decision(title string) string {
	f.t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	out := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建个任务").Stdout
	if err := json.Unmarshal([]byte(out), &m); err != nil || m.ID == "" {
		f.t.Fatalf("没有返回事项 id: %v", err)
	}
	f.apa(f.WRK, 0, "ask", m.ID, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A")
	return m.ID
}

type delivery struct {
	ID      string `json:"id"`
	Item    string `json:"item"`
	Class   string `json:"class"`
	Route   string `json:"route"`
	Version int64  `json:"version"`
	State   string `json:"state"`
	Content struct {
		Title   string `json:"title"`
		FormURL string `json:"form_url"`
	} `json:"content"`
}

const settle = 45 * time.Second

// claimWith 以 token 长轮询领取一次；没有投递返回 nil，其他状态码直接返回给调用方判断。
func (f *fx) claimWith(token string, waitSec int) (*delivery, baseline.Resp) {
	f.t.Helper()
	r := f.APIRaw("GET", fmt.Sprintf("/v1/deliveries/next?wait=%d", waitSec), "", token)
	if r.Code != http.StatusOK {
		return nil, r
	}
	var d delivery
	if err := json.Unmarshal([]byte(r.Body), &d); err != nil {
		f.t.Fatal(err)
	}
	return &d, r
}

// claimWithin 用 CHAN 在 limit 内持续领取，领到第一条即返回；超时返回 nil。
func (f *fx) claimWithin(limit time.Duration) *delivery {
	f.t.Helper()
	for end := time.Now().Add(limit); time.Now().Before(end); {
		d, r := f.claimWith(f.CHAN, 5)
		if d != nil {
			return d
		}
		if r.Code != http.StatusNoContent {
			f.t.Fatalf("领取投递: HTTP %d %s", r.Code, r.Body)
		}
	}
	return nil
}

func (f *fx) mustClaim(item string) *delivery {
	f.t.Helper()
	d := f.claimWithin(settle)
	if d == nil || d.Item != item {
		f.t.Fatalf("CHAN %s 内没有领到事项 %s 的投递: %+v", settle, item, d)
	}
	return d
}

func (f *fx) ack(token string, d *delivery, ref string) baseline.Resp {
	f.t.Helper()
	return f.APIRaw("POST", "/v1/deliveries/"+d.ID+"/ack", fmt.Sprintf(`{"version":%d,"external_ref":%q}`, d.Version, ref), token)
}

func (f *fx) answer(token string, d *delivery, ref, sender, choice string) baseline.Resp {
	f.t.Helper()
	b, _ := json.Marshal(map[string]string{"ref": ref, "sender": sender, "choice": choice})
	return f.APIRaw("POST", "/v1/deliveries/"+d.ID+"/answer", string(b), token)
}

// itemJSON 以本人身份读事项的原始 JSON。
func (f *fx) itemJSON(id string) string {
	f.t.Helper()
	var w struct {
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/items/"+id, "", http.StatusOK)), &w); err != nil || len(w.Item) == 0 {
		f.t.Fatalf("读事项 %s: %v", id, err)
	}
	return string(w.Item)
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

func (f *fx) expect(r baseline.Resp, want ...int) {
	f.t.Helper()
	for _, w := range want {
		if r.Code == w {
			return
		}
	}
	f.t.Fatalf("HTTP %d，期望 %v: %s", r.Code, want, r.Body)
}
