//go:build e2e

package baseline

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// TestDefaultSchedulesPaused 验证验收环境与墙钟时刻无关：拨过一整周（含工作日 08:30 与周五 17:30）也不出现默认安排
// 建的简报、复盘事项；重新启用后同样拨一周则出现，证明前一半不是空过（没有默认宿主时内核本来就跳过简报）。
func TestDefaultSchedulesPaused(t *testing.T) {
	e := Setup(t)
	e.webOK("PUT", "/v1/settings/policy", `{"default_harness":"acc-stub"}`, 200)
	week := func() { e.TCAdvance(8 * 24 * 60 * 60) }
	has := func(kind string) bool {
		r := e.Apa(e.OBS, "", "item", "list", "--kind", kind)
		if r.Code != 0 {
			t.Fatalf("item list --kind %s: %d %s%s", kind, r.Code, r.Stdout, r.Stderr)
		}
		return strings.Contains(r.Stdout, "itm_")
	}
	week()
	time.Sleep(5 * time.Second)
	for _, kind := range []string{"brief", "review"} {
		if has(kind) {
			t.Errorf("默认安排不应在验收环境触发 %s", kind)
		}
	}
	var l struct {
		Instances []struct {
			ID string `json:"id"`
		} `json:"instances"`
	}
	if err := json.Unmarshal([]byte(e.webOK("GET", "/v1/instances", "", 200)), &l); err != nil {
		t.Fatal(err)
	}
	for _, in := range l.Instances {
		e.webOK("PATCH", "/v1/instances/"+in.ID, `{"enabled":true}`, 200)
	}
	week()
	if !e.WaitFor(30*time.Second, func() bool { return has("brief") && has("review") }) {
		t.Errorf("重新启用后拨一周应出现简报与复盘事项\n%s", e.ServiceLog())
	}
}
