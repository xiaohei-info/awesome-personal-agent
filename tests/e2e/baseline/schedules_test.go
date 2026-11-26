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
	// 简报与复盘建的事项 kind 都是 brief，标题以包名开头。
	briefs := func() string {
		r := e.Apa(e.OBS, "", "item", "list", "--kind", "brief")
		if r.Code != 0 {
			t.Fatalf("item list --kind brief: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		return r.Stdout
	}
	week()
	time.Sleep(5 * time.Second)
	if out := briefs(); strings.Contains(out, "itm_") {
		t.Errorf("默认安排不应在验收环境触发: %s", out)
	}
	e.WebLogin() // 拨过一周后原会话已过期
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
	if !e.WaitFor(30*time.Second, func() bool {
		out := briefs()
		return strings.Contains(out, `"title":"brief `) && strings.Contains(out, `"title":"review `)
	}) {
		t.Errorf("重新启用后拨一周应出现简报与复盘事项: %s\n%s", briefs(), e.ServiceLog())
	}
}
