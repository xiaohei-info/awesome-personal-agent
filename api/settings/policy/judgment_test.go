package policy

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const judgmentRoute = "/v1/settings/judgment"

const goodBands = `{"bands":[{"template":"triage-screen","low":0.25,"high":0.5},{"template":"triage-screen","source":"src_a","low":0.1,"high":0.3}],"shadow":["triage-screen"]}`

// ACT-JRC-12：下限大于上限返回 400，不产生新版本与 policy.changed，已有的带不变。
func TestJudgmentBandLowAboveHighRejected(t *testing.T) {
	e := setup(t)
	if rec := e.do("PUT", judgmentRoute, goodBands, e.web); rec.Code != 200 {
		t.Fatalf("PUT good: %d %s", rec.Code, rec.Body)
	}
	v0, _ := e.current()
	events := len(e.eventTypes())
	for _, bad := range []string{
		`{"bands":[{"template":"triage-screen","source":"src_a","low":0.70,"high":0.40}]}`,
		`{"bands":[{"template":"triage-screen","low":-0.1,"high":0.4}]}`,
		`{"bands":[{"template":"triage-screen","low":0.1,"high":1.2}]}`,
		`{"bands":[{"template":"","low":0.1,"high":0.2}]}`,
		`{"bands":[{"template":"t","low":0.1,"high":0.2},{"template":"t","low":0.2,"high":0.3}]}`,
		`{"spot_check":{"per_brief":-1}}`,
	} {
		rec := e.do("PUT", judgmentRoute, bad, e.web)
		if rec.Code != 400 || errCode(t, rec) != domain.CodeInvalidRequest {
			t.Fatalf("%s: %d %s", bad, rec.Code, rec.Body)
		}
	}
	v1, p := e.current()
	if v1 != v0 || len(e.eventTypes()) != events {
		t.Fatalf("rejected writes changed state: v%d->v%d events %d->%d", v0, v1, events, len(e.eventTypes()))
	}
	if len(p.Judgment.Bands) != 2 || p.Judgment.Bands[0].Low != 0.25 || p.Judgment.Bands[0].High != 0.5 {
		t.Fatalf("bands %+v", p.Judgment.Bands)
	}
}

// ACT-JRC-13：任何凭据读写都是 403（web_only），不确定带、影子列表不变，不记 policy.changed。
func TestJudgmentSettingsWebOnly(t *testing.T) {
	e := setup(t)
	e.do("PUT", judgmentRoute, goodBands, e.web)
	v0, _ := e.current()
	events := len(e.eventTypes())
	for _, kind := range []auth.Kind{auth.Observer, auth.Worker, auth.Delegate, auth.Ext, auth.Webhook} {
		mod := e.bearer(kind)
		for _, m := range []string{"GET", "PUT"} {
			rec := e.do(m, judgmentRoute, `{"bands":[],"shadow":["x"]}`, mod)
			if rec.Code != 403 || errCode(t, rec) != domain.CodeWebOnly {
				t.Fatalf("%s as %s: %d %s", m, kind, rec.Code, rec.Body)
			}
		}
	}
	v1, p := e.current()
	if v1 != v0 || len(p.Judgment.Bands) != 2 || len(p.Judgment.Shadow) != 1 {
		t.Fatalf("credential write leaked: v%d->v%d %+v", v0, v1, p.Judgment)
	}
	for _, ty := range e.eventTypes()[events:] {
		if ty == string(domain.EvPolicyChanged) {
			t.Fatal("credential attempt recorded policy.changed")
		}
	}
}

// ACT-JRC-14 的设置一半：Web 打开影子开关记 policy.changed；读回 bands/shadow/spot_check（缺省 5）。
func TestJudgmentShadowToggleRecordsEvent(t *testing.T) {
	e := setup(t)
	rec := e.do("GET", judgmentRoute, "", e.web)
	var got struct {
		Bands     []any    `json:"bands"`
		Shadow    []string `json:"shadow"`
		SpotCheck struct {
			PerBrief int `json:"per_brief"`
		} `json:"spot_check"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != 200 || got.Bands == nil || got.Shadow == nil || got.SpotCheck.PerBrief != 5 {
		t.Fatalf("defaults: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", judgmentRoute, `{"shadow":["triage-screen","triage-screen"]}`, e.web); rec.Code != 200 {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body)
	}
	v, p := e.current()
	if v != 1 || len(p.Judgment.Shadow) != 1 || p.Judgment.Shadow[0] != "triage-screen" {
		t.Fatalf("v%d %+v", v, p.Judgment)
	}
	if ev := e.eventTypes(); len(ev) != 1 || ev[0] != string(domain.EvPolicyChanged) {
		t.Fatalf("events %v", ev)
	}
}
