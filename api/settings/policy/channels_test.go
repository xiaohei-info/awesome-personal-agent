package policy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	apichannel "github.com/xiaohei-info/awesome-personal-agent/api/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	kchannel "github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) extKey(channel string) func(*http.Request) {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "channel." + channel})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
}

// 回答人只改这个渠道的键；只给 Web。
func TestChannelAnswerersReplaceOnlyThisChannel(t *testing.T) {
	e := setup(t)
	if rec := e.do("PUT", "/v1/settings/allowlists", `{"channel_answerers":{"a":["1"],"b":["2"]}}`, e.web); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/channels/a/answerers", `{"answerers":["7","8"]}`, e.web); rec.Code != 200 {
		t.Fatalf("put: %d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); len(p.ChannelAnswerers["a"]) != 2 || p.ChannelAnswerers["b"][0] != "2" {
		t.Fatalf("answerers: %v", p.ChannelAnswerers)
	}
	if rec := e.do("PUT", "/v1/settings/channels/a/answerers", `{"answerers":["9"]}`, e.bearer(auth.Delegate)); rec.Code != 403 || errCode(t, rec) != domain.CodeWebOnly {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
}

// T7：识别只经内存，名单要本人另外"加上"；口令错、渠道不符、过期、用过都是 404。
func TestIdentifyFlowDoesNotWriteTheList(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	id := kchannel.NewIdentifier(func() time.Time { return now })
	e := setup(t, apichannel.IdentifyRoutes(id))
	tg, other := e.extKey("tg"), e.extKey("other")

	rec := e.do("POST", "/v1/settings/channels/tg/identify", `{}`, e.web)
	var start struct{ Code string }
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &start) != nil || len(start.Code) != len("apa-7K3Q") {
		t.Fatalf("start: %d %s", rec.Code, rec.Body)
	}
	status := func() map[string]string {
		var out map[string]any
		rec := e.do("GET", "/v1/settings/channels/tg/identify", "", e.web)
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		m := map[string]string{}
		for k, v := range out {
			if s, ok := v.(string); ok {
				m[k] = s
			}
		}
		return m
	}
	if s := status(); s["state"] != "waiting" || s["code"] != "" {
		t.Fatalf("waiting: %v", s)
	}
	report := func(key func(*http.Request), code string) int {
		return e.do("POST", "/v1/channels/identify", `{"code":"`+code+`","sender":"4242","display":"@me"}`, key).Code
	}
	if c := report(tg, "apa-XXXX"); c != 404 {
		t.Fatalf("wrong code: %d", c)
	}
	if c := report(other, start.Code); c != 404 {
		t.Fatalf("other channel's key: %d", c)
	}
	if c := e.do("POST", "/v1/channels/identify", `{"code":"`+start.Code+`","sender":"1"}`, e.web).Code; c != 403 {
		t.Fatalf("web session must not report: %d", c)
	}
	if c := report(tg, start.Code); c != 200 {
		t.Fatalf("report: %d", c)
	}
	if s := status(); s["state"] != "got" || s["sender"] != "4242" || s["display"] != "@me" {
		t.Fatalf("got: %v", s)
	}
	if c := report(tg, start.Code); c != 404 {
		t.Fatalf("code reused: %d", c)
	}
	if _, p := e.current(); len(p.ChannelAnswerers["tg"]) != 0 {
		t.Fatalf("identification wrote the list: %v", p.ChannelAnswerers)
	}

	e.do("POST", "/v1/settings/channels/tg/identify", `{}`, e.web)
	now = now.Add(kchannel.IdentifyTTL)
	if s := status(); s["state"] != "expired" {
		t.Fatalf("expired: %v", s)
	}
}

// channelKey 给某个渠道签一把 ext 密钥，返回凭据登记（测试里要断言吊销状态）。
func (e *env) channelKey(channel string) auth.Credential {
	e.t.Helper()
	var c auth.Credential
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		var err error
		c, _, err = e.auth.Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "channel." + channel})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return c
}

func (e *env) liveKeys(channel string) int {
	e.t.Helper()
	var n int
	if err := e.st.View(bg, func(tx *store.Tx) error {
		all, err := e.auth.List(tx)
		for _, c := range all {
			if c.Kind == auth.Ext && c.Scope == "channel."+channel && c.RevokedAt.IsZero() {
				n++
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// 删除自写程序渠道：一个动作吊销它的钥匙并从策略两处删掉这个名字，可重复执行；别的渠道与路由不动。
func TestRemoveOwnChannelRevokesKeysAndDropsPolicy(t *testing.T) {
	e := setup(t)
	if rec := e.do("PUT", "/v1/settings/policy", `{"channels":{"slack":{"text_only":true},"keep":{"text_only":true}},"disturb":{"routes":{"decision":"channel:slack","reminder":"inject","brief":"inject","health":"web"}}}`, e.web); rec.Code != 200 {
		t.Fatalf("seed policy: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/allowlists", `{"channel_answerers":{"slack":["1"],"keep":["2"]}}`, e.web); rec.Code != 200 {
		t.Fatalf("seed allowlists: %d %s", rec.Code, rec.Body)
	}
	e.channelKey("slack")
	e.channelKey("slack")
	gone := e.channelKey("slack") // 已经吊销过的钥匙不影响结果
	if err := e.st.Update(bg, func(tx *store.Tx) error { return e.auth.Revoke(tx, gone.ID) }); err != nil {
		t.Fatal(err)
	}
	e.channelKey("keep")
	before, _ := e.current()

	rec := e.do("DELETE", "/v1/settings/channels/slack", "", e.web)
	if rec.Code != 200 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Version int `json:"version"`
		Revoked int `json:"revoked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || out.Revoked != 2 || out.Version != before+1 {
		t.Fatalf("delete result: %+v %s", out, rec.Body)
	}
	if n := e.liveKeys("slack"); n != 0 {
		t.Fatalf("slack still has %d live keys", n)
	}
	if n := e.liveKeys("keep"); n != 1 {
		t.Fatalf("keep lost its key: %d", n)
	}
	_, p := e.current()
	if _, ok := p.Channels["slack"]; ok {
		t.Fatalf("channels still has slack: %v", p.Channels)
	}
	if _, ok := p.ChannelAnswerers["slack"]; ok {
		t.Fatalf("channel_answerers still has slack: %v", p.ChannelAnswerers)
	}
	if _, ok := p.Channels["keep"]; !ok || p.ChannelAnswerers["keep"][0] != "2" {
		t.Fatalf("other channel changed: %v %v", p.Channels, p.ChannelAnswerers)
	}
	// 路由不动：指向已删除渠道的条目由「提醒与打扰」照样列出并标明。
	if p.Disturb.Routes["decision"] != "channel:slack" {
		t.Fatalf("route should stay: %v", p.Disturb.Routes)
	}
	ev := strings.Join(e.eventTypes(), ",")
	if !strings.Contains(ev, domain.EvPolicyChanged) || !strings.Contains(ev, domain.EvAllowlistChanged) {
		t.Fatalf("audit events: %s", ev)
	}

	// 幂等：再删一次不报错、不再改策略。
	rec = e.do("DELETE", "/v1/settings/channels/slack", "", e.web)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); rec.Code != 200 || err != nil || out.Revoked != 0 || out.Version != before+1 {
		t.Fatalf("second delete: %d %s", rec.Code, rec.Body)
	}
}

// 删除只给 Web；内置渠道的名字前缀不许从这个入口删。
func TestRemoveOwnChannelOwnerOnly(t *testing.T) {
	e := setup(t)
	if rec := e.do("PUT", "/v1/settings/allowlists", `{"channel_answerers":{"keep":["2"]}}`, e.web); rec.Code != 200 {
		t.Fatalf("seed: %d %s", rec.Code, rec.Body)
	}
	e.channelKey("keep")
	if rec := e.do("DELETE", "/v1/settings/channels/keep", "", e.bearer(auth.Delegate)); rec.Code != 403 || errCode(t, rec) != domain.CodeTierClosed {
		t.Fatalf("token: %d %s", rec.Code, rec.Body)
	}
	if n := e.liveKeys("keep"); n != 1 {
		t.Fatalf("rejected call revoked a key: %d", n)
	}
	if rec := e.do("DELETE", "/v1/settings/channels/ch-a1", "", e.web); rec.Code != 400 || errCode(t, rec) != domain.CodeInvalidRequest {
		t.Fatalf("built-in prefix: %d %s", rec.Code, rec.Body)
	}
}

// 吊销成功但策略写入失败时整体回滚：不留半状态，钥匙仍然有效。
func TestRemoveOwnChannelRollsBackWhenPolicyFails(t *testing.T) {
	e := setup(t)
	e.channelKey("slack")
	// 写一条内核读不出来的策略版本：删除动作里的策略写入必然失败。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO policy_version (version, doc, at) VALUES (?, ?, ?)`, 99, `{"disturb":{"routes":{"bogus":"web"}}}`, time.Now().UnixNano())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rec := e.do("DELETE", "/v1/settings/channels/slack", "", e.web)
	if rec.Code != 400 || errCode(t, rec) != domain.CodeInvalidRequest {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body)
	}
	if n := e.liveKeys("slack"); n != 1 {
		t.Fatalf("half state: the key was revoked but the policy write failed (%d live)", n)
	}
}
