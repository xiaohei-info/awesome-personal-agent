package policy

import (
	"encoding/json"
	"net/http"
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
