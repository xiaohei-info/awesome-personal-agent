package policy

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
)

const proactiveOn = `{"on":true,"since":"2000-01-01T00:00:00Z","work_start":"09:00","work_end":"18:00","workdays":[1,2,3,4,5],"shift":true,"start":true,"end":true,"lead_min":0,"dedup_min":60,"max_items":20,"max_batches":6}`

func disturbWith(proactive string) string {
	return `{"disturb":{"routes":{"decision":"inject"},"merge_window_sec":0,"remind_on_deadline":true,"daily_cap":20,"mutes":[],"proactive":` + proactive + `}}`
}

// 开启时刻只由设置接口记：从关到开记现在（请求体里带的不认），一直开着沿用原值，关掉清空；不合法的设置 400 不生效。
func TestProactiveSinceIsStampedByTheServer(t *testing.T) {
	e := setup(t)
	before := time.Now().Add(-time.Second)
	if rec := e.do("PUT", "/v1/settings/policy", disturbWith(proactiveOn), e.web); rec.Code != 200 {
		t.Fatalf("enable: %d %s", rec.Code, rec.Body)
	}
	_, p := e.current()
	since := p.Disturb.Proactive.Since
	if since == nil || since.Before(before) {
		t.Fatalf("since = %v, want the server's now", since)
	}
	edited := strings.Replace(proactiveOn, `"lead_min":0`, `"lead_min":15`, 1)
	if rec := e.do("PUT", "/v1/settings/policy", disturbWith(edited), e.web); rec.Code != 200 {
		t.Fatalf("edit: %d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.Disturb.Proactive.LeadMin != 15 || !p.Disturb.Proactive.Since.Equal(*since) {
		t.Fatalf("staying on keeps since: %+v", p.Disturb.Proactive)
	}
	off := strings.Replace(proactiveOn, `"on":true`, `"on":false`, 1)
	if rec := e.do("PUT", "/v1/settings/policy", disturbWith(off), e.web); rec.Code != 200 {
		t.Fatalf("disable: %d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.Disturb.Proactive.On || p.Disturb.Proactive.Since != nil {
		t.Fatalf("off clears since: %+v", p.Disturb.Proactive)
	}
	for _, c := range [][2]string{{`"work_start":"09:00"`, `"work_start":"18:00"`}, {`"workdays":[1,2,3,4,5]`, `"workdays":[]`}, {`"dedup_min":60`, `"dedup_min":-1`}} {
		if rec := e.do("PUT", "/v1/settings/policy", disturbWith(strings.Replace(proactiveOn, c[0], c[1], 1)), e.web); rec.Code != 400 {
			t.Fatalf("%s: %d %s", c[1], rec.Code, rec.Body)
		}
	}
	bad := strings.Replace(proactiveOn, `"max_batches":6`, `"max_batches":6,"task_push":{"nightly":true}`, 1)
	if rec := e.do("PUT", "/v1/settings/policy", disturbWith(bad), e.web); rec.Code != 400 {
		t.Fatalf("unknown task type: %d %s", rec.Code, rec.Body)
	}
}

// 预览只读、只给 Web 本人；Agent 的钥匙不能用。
func TestProactivePreviewIsWebOnlyAndReadOnly(t *testing.T) {
	e := setup(t)
	v0, _ := e.current()
	rec := e.do("POST", "/v1/settings/proactive/preview", proactiveOn, e.web)
	if rec.Code != 200 {
		t.Fatalf("preview: %d %s", rec.Code, rec.Body)
	}
	var pv struct {
		Total   int    `json:"total"`
		Route   string `json:"route"`
		Blocked string `json:"blocked"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &pv); err != nil || pv.Total != 0 || pv.Route != "web" || pv.Blocked != "main_not_set" {
		t.Fatalf("preview = %s", rec.Body)
	}
	if v, _ := e.current(); v != v0 {
		t.Fatal("preview wrote a policy version")
	}
	if rec := e.do("POST", "/v1/settings/proactive/preview", proactiveOn, e.bearer(auth.Delegate)); rec.Code != 403 {
		t.Fatalf("delegate: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("POST", "/v1/settings/proactive/preview", strings.Replace(proactiveOn, `"09:00"`, `"9am"`, 1), e.web); rec.Code != 400 {
		t.Fatalf("invalid: %d %s", rec.Code, rec.Body)
	}
}
