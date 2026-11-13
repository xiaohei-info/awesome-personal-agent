//go:build e2e

package workbench

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 退出码（附录 B.1）。
const (
	exitUsage  = 2
	exitDenied = 4
	exitBasis  = 23 // 代答缺依据
)

type kit struct {
	*baseline.Env
	t *testing.T
}

// step 运行一个验收动作；前一步失败后续步骤不得开始。
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
	r := k.Web(method, "/v1"+path, body)
	if r.Code != want {
		k.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (k *kit) apa(token string, want int, args ...string) baseline.Result {
	k.t.Helper()
	r := k.Apa(token, "", args...)
	if r.Code != want {
		k.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (k *kit) must(err error) {
	k.t.Helper()
	if err != nil {
		k.t.Fatal(err)
	}
}

func jsonOf(t *testing.T, s string, into any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), into); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

// ---- 事项 ----

// webItem 以本人（Web 登录态）新建事项，fields 是 POST /v1/items 的体（与"事项"页新建表单一致），返回 id。
func (k *kit) webItem(fields map[string]any) string {
	k.t.Helper()
	if _, ok := fields["phase"]; !ok {
		fields["phase"] = "open"
	}
	b, _ := json.Marshal(fields)
	var out struct {
		ID string `json:"id"`
	}
	jsonOf(k.t, k.webOK("POST", "/items", string(b), 201), &out)
	if out.ID == "" {
		k.t.Fatalf("新建事项没有返回 id: %s", b)
	}
	return out.ID
}

// item 读事项对象（OBS 的 `apa item show`）。
func (k *kit) item(id string) map[string]any {
	k.t.Helper()
	var out struct {
		Item map[string]any `json:"item"`
	}
	jsonOf(k.t, k.apa(k.OBS, 0, "item", "show", id).Stdout, &out)
	if out.Item == nil {
		k.t.Fatalf("item show %s 输出不是事项", id)
	}
	return out.Item
}

func (k *kit) phase(id string) string    { s, _ := k.item(id)["phase"].(string); return s }
func (k *kit) version(id string) float64 { v, _ := k.item(id)["version"].(float64); return v }
func (k *kit) planned(id string) any     { return k.item(id)["planned_for"] }

func (k *kit) progress(id string) []map[string]any {
	k.t.Helper()
	var out struct {
		Progress []map[string]any `json:"progress"`
	}
	jsonOf(k.t, k.apa(k.OBS, 0, "item", "show", id).Stdout, &out)
	return out.Progress
}

// webDone 在 Web 上把事项转 done（"今天"页的"完成"按钮调的接口）。
func (k *kit) webDone(id string) {
	k.t.Helper()
	k.webOK("POST", "/items/"+id+"/transition", `{"to":"done","reason":"在今天页完成"}`, 200)
}

// ---- 今天 ----

type todayList struct {
	Planned, Due, Pending, Stale []map[string]any
}

func (td todayList) ids(group string) []string {
	var l []map[string]any
	switch group {
	case "planned":
		l = td.Planned
	case "due":
		l = td.Due
	case "pending":
		l = td.Pending
	case "stale":
		l = td.Stale
	}
	var out []string
	for _, it := range l {
		s, _ := it["id"].(string)
		out = append(out, s)
	}
	return out
}

func has(l []string, id string) bool {
	for _, x := range l {
		if x == id {
			return true
		}
	}
	return false
}

// today 取"今天"清单：CLI（OBS）与 HTTP（OBS 凭据）两路，要求一致。
func (k *kit) today() todayList {
	k.t.Helper()
	var cli, http todayList
	jsonOf(k.t, k.apa(k.OBS, 0, "today").Stdout, &cli)
	jsonOf(k.t, k.APIRaw("GET", "/v1/today", "", k.OBS).Body, &http)
	for _, g := range []string{"planned", "due", "pending", "stale"} {
		a, b := strings.Join(sortedCopy(cli.ids(g)), ","), strings.Join(sortedCopy(http.ids(g)), ",")
		if a != b {
			k.t.Fatalf("今天清单 %s 组 CLI 与 HTTP 不一致: %s / %s", g, a, b)
		}
	}
	return cli
}

func sortedCopy(l []string) []string {
	out := append([]string(nil), l...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// webToday 是 Web"今天"页的数据（本人会话）。
func (k *kit) webToday() todayList {
	k.t.Helper()
	var td todayList
	jsonOf(k.t, k.webOK("GET", "/today", "", 200), &td)
	return td
}

// ---- 审计事件 ----

type event struct {
	Seq     int64          `json:"seq"`
	At      string         `json:"at"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Actor   actor          `json:"actor"`
	Payload map[string]any `json:"payload"`
}

type actor struct {
	Type      string `json:"type"`
	Principal string `json:"principal"`
	Via       string `json:"via"`
}

// events 取某对象（空表示全部）某类型（空表示全部）的审计事件，即 Web 审计页的数据源。
func (k *kit) events(object, typ string) []event {
	k.t.Helper()
	var out struct {
		Events []event `json:"events"`
	}
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + object
	}
	if typ != "" {
		path += "&type=" + typ
	}
	jsonOf(k.t, k.webOK("GET", path, "", 200), &out)
	return out.Events
}

// byOwner 要求这条 Event 的操作者是本人（Web）且带时间。
func byOwner(t *testing.T, what string, e event) {
	t.Helper()
	eq(t, what+" 操作者类型", e.Actor.Type, "human")
	eq(t, what+" via", e.Actor.Via, "web")
	if e.At == "" {
		t.Errorf("%s 没有时间", what)
	}
}

func (k *kit) lastEvent(object, typ string) event {
	k.t.Helper()
	es := k.events(object, typ)
	if len(es) == 0 {
		k.t.Fatalf("%s 上没有 %s 事件", object, typ)
	}
	return es[len(es)-1]
}

func (k *kit) countEvents(object, typ string) int { return len(k.events(object, typ)) }

// ---- 未读提醒 ----

func (k *kit) unreadRaw() string { return k.webOK("GET", "/deliveries/unread", "", 200) }

// findItem 按标题在全部事项里找 id（本人会话的全文搜索）。
func (k *kit) findItem(title string) string {
	k.t.Helper()
	var out struct {
		Items []map[string]any `json:"items"`
	}
	jsonOf(k.t, k.webOK("GET", "/items?limit=500&q="+url.QueryEscape(title), "", 200), &out)
	for _, it := range out.Items {
		if it["title"] == title {
			return it["id"].(string)
		}
	}
	k.t.Fatalf("没有找到事项 %q", title)
	return ""
}

// probeStubs 向两个替身各发一次测试请求（ACT-ENV-04、05 的 curl）。
func (k *kit) probeStubs() {
	k.t.Helper()
	post := func(u, body string) {
		resp, err := http.Post(u, "application/json", strings.NewReader(body))
		if err != nil {
			k.t.Fatalf("POST %s: %v", u, err)
		}
		resp.Body.Close()
	}
	post(k.Cls.URL(), `{"protocol":"probability-judgment/1","questions":[{"id":"q1","question":"是吗","criteria":"c","input":{"k":"v"}}]}`)
	post(k.ModelStub.URL()+"/chat/completions", `{"model":"acc-model","messages":[{"role":"user","content":"ping"}]}`)
}
