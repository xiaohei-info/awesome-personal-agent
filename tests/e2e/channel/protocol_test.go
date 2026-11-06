//go:build e2e

// Package channel 按验收文档 flow-channel-protocol（ACT-CHP-01..11）用真实 apa 二进制与真实 HTTP 请求
// 独立执行"投递领取协议的幂等、版本和回答人校验由内核保证"。环境来自 flow-env-baseline 的共用脚本；
// 渠道 ext 由测试直接扮演（HTTP 调用），ACT-CHP-11 用真实 Telegram ext 二进制与一个故意不幂等的坏 ext。
// Web 设置页的操作用已登录的本人会话调用页面背后的同一组 HTTP API。
//
// 只改测试，不改产品代码。
package channel

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// resolution 读事项的决策结果：未回答返回 nil。
func (f *fx) resolution(id string) *struct {
	Choice      string `json:"choice"`
	ResolvedVia string `json:"resolved_via"`
	AnsweredBy  string `json:"answered_by"`
} {
	f.t.Helper()
	var it struct {
		Phase string `json:"phase"`
		Ask   struct {
			Resolution *struct {
				Choice      string `json:"choice"`
				ResolvedVia string `json:"resolved_via"`
				AnsweredBy  string `json:"answered_by"`
			} `json:"resolution"`
		} `json:"ask"`
	}
	if err := json.Unmarshal([]byte(f.itemJSON(id)), &it); err != nil {
		f.t.Fatal(err)
	}
	return it.Ask.Resolution
}

func (f *fx) phase(id string) string {
	f.t.Helper()
	var it struct {
		Phase string `json:"phase"`
	}
	_ = json.Unmarshal([]byte(f.itemJSON(id)), &it)
	return it.Phase
}

// rejections 数事项上的 command.rejected 事件（本人用 OBS 读事件流）。
func (f *fx) rejections(item string) int {
	f.t.Helper()
	out := f.apa(f.OBS, 0, "events", "--limit", "500").Stdout
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") { // 事件流是逐行 JSON
		var e struct {
			Type   string `json:"type"`
			Object string `json:"object"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			f.t.Fatalf("事件行不是 JSON: %v\n%s", err, line)
		}
		if e.Type == "command.rejected" && e.Object == item {
			n++
		}
	}
	return n
}

// ACT-CHP-01：领取带租约，期间别人领不到；到期后可再次领取，投递 id 不变。
func TestCHP01ClaimAndLease(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-01")

	d := f.mustClaim(id)
	if d.State != "claimed" || d.Class != "decision" || d.Route != "channel:acc-chan" {
		t.Fatalf("领到的投递状态不对: %+v", d)
	}
	if again, r := f.claimWith(f.CHAN, 2); again != nil {
		t.Fatalf("租约期内又被领取了: %+v", again)
	} else if r.Code != http.StatusNoContent {
		t.Fatalf("租约期内再次领取应是 204，实际 %d %s", r.Code, r.Body)
	}
	// 默认租约 60 秒：不回报，等到期后重新可领。
	var re *delivery
	for end := time.Now().Add(100 * time.Second); re == nil && time.Now().Before(end); {
		re, _ = f.claimWith(f.CHAN, 5)
	}
	if re == nil || re.ID != d.ID || re.State != "claimed" {
		t.Fatalf("租约到期后没有重新领到同一条投递: %+v（原 %s）", re, d.ID)
	}
}

// ACT-CHP-02：渠道由密钥确定，别的渠道的密钥领不到，也报告不了这条投递。
func TestCHP02ChannelFromKey(t *testing.T) {
	t.Parallel()
	f := setup(t)
	var out struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(f.webOK("POST", "/settings/ext-keys", `{"label":"CHAN2","scope":"channel.acc-chan2"}`, http.StatusCreated)), &out); err != nil || out.Secret == "" {
		t.Fatalf("签发 acc-chan2 密钥: %v", err)
	}
	id := f.decision("chp-02")
	// 等投递进入队列：acc-chan 的密钥领得到，acc-chan2 的领不到。
	d := f.mustClaim(id)
	if other, r := f.claimWith(out.Secret, 2); other != nil {
		t.Fatalf("跨渠道领取成功: %+v", other)
	} else if r.Code != http.StatusNoContent {
		t.Fatalf("acc-chan2 领取应是 204，实际 %d %s", r.Code, r.Body)
	}
	f.expect(f.ack(out.Secret, d, "x-1"), http.StatusNotFound, http.StatusForbidden)
	f.expect(f.answer(out.Secret, d, "x-1", "alice", "A"), http.StatusForbidden, http.StatusNotFound)
	if f.resolution(id) != nil {
		t.Fatal("跨渠道密钥改变了决策")
	}
}

// ACT-CHP-03：版本变化后用旧版本号 ack 得到 409，投递转 superseded，不再可确认。
func TestCHP03StaleVersionAck(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-03")
	d := f.mustClaim(id)

	f.webOK("PATCH", "/items/"+id, `{"title":"chp-03 改过","reason":"改动使版本变化"}`, http.StatusOK)

	r := f.ack(f.CHAN, d, "tg-1")
	f.expect(r, http.StatusConflict)
	f.expect(f.ack(f.CHAN, d, "tg-1"), http.StatusConflict) // 作废后仍不能确认
	if f.unreadHas(d.ID) {
		t.Errorf("作废的投递不应进 Web 未读")
	}
}

func (f *fx) unreadHas(id string) bool {
	for _, d := range f.unread() {
		if d.ID == id {
			return true
		}
	}
	return false
}

// ACT-CHP-04：失败回报触发退避重试。默认第 1 次约 1 分钟后可领；调小间隔后跑完 5 次，超限转 fallback_web。
func TestCHP04BackoffDefault(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-04-default")
	d := f.mustClaim(id)
	failed := time.Now()
	f.expect(f.APIRaw("POST", "/v1/deliveries/"+d.ID+"/fail", `{"version":`+itoa(d.Version)+`,"reason":"网络抖动"}`, f.CHAN), http.StatusOK)

	if got, _ := f.claimWith(f.CHAN, 20); got != nil {
		t.Fatalf("失败后 %s 内就可再领，默认第 1 次退避应约 1 分钟: %+v", time.Since(failed).Round(time.Second), got)
	}
	var re *delivery
	for end := failed.Add(100 * time.Second); re == nil && time.Now().Before(end); {
		re, _ = f.claimWith(f.CHAN, 5)
	}
	if re == nil || re.ID != d.ID {
		t.Fatal("退避期满后投递没有重新可领")
	}
	if wait := time.Since(failed); wait < 55*time.Second || wait > 95*time.Second {
		t.Errorf("第 1 次退避约 1 分钟，实际 %s", wait.Round(time.Second))
	}
}

func TestCHP04BackoffExhaustedFallsBackToWeb(t *testing.T) {
	t.Parallel()
	e := baselineNew(t)
	e.Init()
	// 默认节奏 1、3、7、15、30 分钟共 5 次；验收时调小间隔，次数不变。
	e.ServeWith("delivery_backoff: 1s,1s,1s,1s,1s\n")
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	e.DeclareStubHarness()
	f := wrap(t, e)

	id := f.decision("chp-04-exhaust")
	d := f.mustClaim(id)
	for n := 1; n <= 5; n++ {
		f.expect(f.APIRaw("POST", "/v1/deliveries/"+d.ID+"/fail", `{"version":`+itoa(d.Version)+`,"reason":"失败"}`, f.CHAN), http.StatusOK)
		re := f.claimWithin(30 * time.Second)
		if re == nil || re.ID != d.ID {
			t.Fatalf("第 %d 次失败后投递没有重新可领: %+v", n, re)
		}
		d = re
	}
	// 第 6 次失败：超出 5 次重试。
	f.expect(f.APIRaw("POST", "/v1/deliveries/"+d.ID+"/fail", `{"version":`+itoa(d.Version)+`,"reason":"失败"}`, f.CHAN), http.StatusOK)
	if re, _ := f.claimWith(f.CHAN, 3); re != nil {
		t.Fatalf("超限后仍可领取: %+v", re)
	}
	var got *delivery
	for _, u := range f.unread() {
		if u.ID == d.ID {
			u := u
			got = &u
		}
	}
	if got == nil || got.State != "fallback_web" {
		t.Fatalf("超限后投递应转 fallback_web 并在 Web 未读可见: %+v", got)
	}
}

func itoa(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ACT-CHP-05：回答人不在白名单、或缺失，都被拒绝，事项仍是 needs_input，并留有拒绝记录。
func TestCHP05AnswererWhitelist(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-05")
	d := f.mustClaim(id)
	f.expect(f.ack(f.CHAN, d, "tg-5"), http.StatusOK)

	before := f.rejections(id)
	f.expect(f.answer(f.CHAN, d, "tg-5", "mallory", "A"), http.StatusForbidden)
	f.expect(f.answer(f.CHAN, d, "tg-5", "", "A"), http.StatusForbidden)
	if f.resolution(id) != nil || f.phase(id) != "needs_input" {
		t.Fatalf("被拒绝的回答生效了: phase=%s", f.phase(id))
	}
	if got := f.rejections(id); got-before != 2 {
		t.Errorf("两次被拒绝应各留一条 command.rejected，实际新增 %d", got-before)
	}
}

// ACT-CHP-06：回答人正确但 ref 与送达时不同，被拒绝。
func TestCHP06RefMismatch(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-06")
	d := f.mustClaim(id)
	f.expect(f.ack(f.CHAN, d, "tg-6"), http.StatusOK)

	f.expect(f.answer(f.CHAN, d, "tg-other", "alice", "A"), http.StatusForbidden)
	if f.resolution(id) != nil {
		t.Fatal("ref 不符的回答生效了")
	}
	f.expect(f.answer(f.CHAN, d, "tg-6", "alice", "A"), http.StatusOK) // 对照：ref 正确则生效
}

// ACT-CHP-07：只认第一次回答，第二次 409，结果不被覆盖。
func TestCHP07FirstAnswerWins(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-07")
	d := f.mustClaim(id)
	f.expect(f.ack(f.CHAN, d, "tg-7"), http.StatusOK)

	f.expect(f.answer(f.CHAN, d, "tg-7", "alice", "A"), http.StatusOK)
	f.expect(f.answer(f.CHAN, d, "tg-7", "alice", "B"), http.StatusConflict)
	res := f.resolution(id)
	if res == nil || res.Choice != "A" || res.ResolvedVia != "acc-chan" || res.AnsweredBy != "alice" {
		t.Fatalf("结果应保持第一次回答 A / resolved_via=acc-chan / alice: %+v", res)
	}
}

// ACT-CHP-08：吊销密钥立即生效（401），重新签发后可用。
func TestCHP08RevokeTakesEffectImmediately(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-08")
	d := f.mustClaim(id)

	var list struct {
		Credentials []struct {
			ID    string `json:"id"`
			Label string `json:"label"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/settings/ext-keys", "", http.StatusOK)), &list); err != nil {
		t.Fatal(err)
	}
	keyID := ""
	for _, c := range list.Credentials {
		if c.Label == "CHAN" {
			keyID = c.ID
		}
	}
	if keyID == "" {
		t.Fatalf("设置里找不到 CHAN: %+v", list)
	}
	f.webOK("DELETE", "/settings/ext-keys/"+keyID, "", http.StatusOK)

	f.expect(f.APIRaw("GET", "/v1/deliveries/next?wait=0", "", f.CHAN), http.StatusUnauthorized)
	f.expect(f.ack(f.CHAN, d, "tg-8"), http.StatusUnauthorized)

	var out struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal([]byte(f.webOK("POST", "/settings/ext-keys", `{"label":"CHAN","scope":"channel.acc-chan"}`, http.StatusCreated)), &out); err != nil || out.Secret == "" {
		t.Fatalf("重新签发: %v", err)
	}
	f.expect(f.ack(out.Secret, d, "tg-8"), http.StatusOK)
}

// ACT-CHP-09：渠道密钥只能做渠道动作，读事项与替本人决定都被拒绝。
func TestCHP09KeyScopedToChannelActions(t *testing.T) {
	t.Parallel()
	f := setup(t)
	id := f.decision("chp-09")
	f.expect(f.APIRaw("GET", "/v1/items", "", f.CHAN), http.StatusForbidden, http.StatusUnauthorized)
	f.expect(f.APIRaw("POST", "/v1/items/"+id+"/decide", `{"choice":"A"}`, f.CHAN), http.StatusForbidden, http.StatusUnauthorized)
	if f.resolution(id) != nil {
		t.Fatal("渠道密钥越权决定成功")
	}
}

// formID 取投递里的一次性表单令牌（form_url 形如 /forms/<令牌>）。
func formToken(t *testing.T, d *delivery) string {
	t.Helper()
	tok, ok := strings.CutPrefix(d.Content.FormURL, "/forms/")
	if !ok || tok == "" {
		t.Fatalf("投递没有一次性表单链接: %+v", d)
	}
	return tok
}

// ACT-CHP-10：只能发文字的渠道得到一次性表单链接：可答一次、只对该决策、过期失效。
func TestCHP10OneShotForm(t *testing.T) {
	t.Parallel()
	f := setup(t)
	f.putPolicy("/settings/policy", map[string]any{"channels": map[string]any{"acc-chan": map[string]bool{"text_only": true}}})

	idA, idOther := f.decision("chp-10-a"), f.decision("chp-10-other")
	got := map[string]*delivery{}
	for len(got) < 2 {
		d := f.claimWithin(settle)
		if d == nil {
			t.Fatalf("没有领到全部决策投递: %v", got)
		}
		got[d.Item] = d
	}
	tokA := formToken(t, got[idA])

	// 令牌不是 Bearer：拿它读账本被拒绝，也读不到别的决策。
	f.expect(f.APIRaw("GET", "/v1/items", "", tokA), http.StatusUnauthorized, http.StatusForbidden)
	f.expect(f.APIRaw("GET", "/v1/items/"+idOther, "", tokA), http.StatusUnauthorized, http.StatusForbidden)
	if r := f.WebRaw("GET", "/forms/"+tokA, "", false); r.Code != http.StatusOK || strings.Contains(r.Body, "chp-10-other") {
		t.Fatalf("打开表单: HTTP %d %s", r.Code, r.Body)
	}
	if f.resolution(idA) != nil {
		t.Fatal("只是打开表单就兑现了令牌")
	}

	f.expect(f.WebRaw("POST", "/forms/"+tokA, `{"choice":"B"}`, false), http.StatusOK)
	if res := f.resolution(idA); res == nil || res.Choice != "B" || res.ResolvedVia != "acc-chan" {
		t.Fatalf("表单作答没有写入决策: %+v", res)
	}
	if f.resolution(idOther) != nil {
		t.Fatal("令牌作用到了别的决策")
	}
	f.expect(f.WebRaw("POST", "/forms/"+tokA, `{"choice":"A"}`, false), http.StatusConflict)
	f.expect(f.WebRaw("GET", "/forms/"+tokA, "", false), http.StatusConflict)

	// 过期：真实二进制没有可调的时钟，把账本里这条令牌的 expires_at 改到过去（等同 24 小时后）。
	tokOther := formToken(t, got[idOther])
	db, err := sql.Open("sqlite", "file:"+filepath.Join(f.Home, "ledger.db")+"?_pragma=busy_timeout(10000)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if res, err := db.Exec(`UPDATE channel_form SET expires_at = ? WHERE item = ?`, time.Now().Add(-time.Minute).UnixNano(), idOther); err != nil {
		t.Fatal(err)
	} else if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("应改到 1 条令牌，实际 %d", n)
	}
	f.expect(f.WebRaw("POST", "/forms/"+tokOther, `{"choice":"A"}`, false), http.StatusConflict)
	if f.resolution(idOther) != nil {
		t.Fatal("过期链接仍能作答")
	}
}
