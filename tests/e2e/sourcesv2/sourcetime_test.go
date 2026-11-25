//go:build e2e

package sourcesv2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 验收：只写配置、不写代码，外部来源就能带上到期与起止时间（§9.2、§9.3）。
// 用随发行附带的 http-json 与一条声明式的 mapping（due_at、starts_at、ends_at 各指一个字段），
// 断言建议事项直接带上截止、计划日期与时段，并在 ext 记下来源给的原值；接受它之后日期照旧。
func TestFlowSourceTimeFromConfigOnly(t *testing.T) {
	k := setup(t)
	buildHTTPJSON(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"items":[{"uid":"cfg-time-1","kind":"reminder.due","title":"交材料","at":"2026-11-30T01:00:00Z",
			"due":"2026-12-02","start":"2026-12-03T09:00:00+08:00","end":"2026-12-03T10:30:00+08:00"}]}`)
	}))
	defer srv.Close()
	k.addPull("src_time_cfg", "http-json", "reminder", nil, map[string]any{
		"url": srv.URL, "items_path": "items", "event_type": "reminder.due",
		"mapping": map[string]any{"id": "uid", "subject": "title", "occurred_at": "at",
			"due_at": "due", "starts_at": "start", "ends_at": "end"},
	})

	ev := k.settled("src_time_cfg", "cfg-time-1")
	if ev.Disposition != "proposed" || ev.Item == "" {
		t.Fatalf("事件 %+v", ev)
	}
	it := k.item(ev.Item)
	deadline, _ := it.Raw["deadline"].(string)
	d, err := time.Parse(time.RFC3339, deadline)
	if err != nil || d.Format(time.DateOnly) != "2026-12-02" || d.Hour() != 23 || d.Minute() != 59 || d.Second() != 59 {
		t.Fatalf("截止应落成 2026-12-02 当天 23:59:59（按主机时区）: %q", deadline)
	}
	// 计划日期与时段按同一主机时区算：从截止的偏移取时区，独立算期望值，不依赖测试机时区。
	_, off := d.Zone()
	host := time.FixedZone("host", off)
	start := time.Date(2026, 12, 3, 1, 0, 0, 0, time.UTC).In(host)
	if it.Raw["planned_for"] != start.Format(time.DateOnly) {
		t.Fatalf("计划日期 %v, 想要 %s", it.Raw["planned_for"], start.Format(time.DateOnly))
	}
	if want := start.Format("15:04") + " - " + start.Add(90*time.Minute).Format("15:04"); it.Raw["planned_slot"] != want {
		t.Fatalf("时段 %v, 想要 %s", it.Raw["planned_slot"], want)
	}
	var ext map[string]string
	_ = json.Unmarshal([]byte(mustJSON(it.Raw["ext"])), &ext)
	if ext["source_deadline"] == "" || ext["source_planned_for"] == "" || ext["source_planned_slot"] == "" {
		t.Fatalf("ext 应记下来源给的原值: %+v", ext)
	}

	// 接受只做阶段转移：日期原样保留（§9.3 方案一）。
	k.webOK("POST", "/items/"+ev.Item+"/accept", `{}`, 200)
	after := k.item(ev.Item)
	if after.Phase != "open" || after.Raw["planned_for"] != it.Raw["planned_for"] || after.Raw["planned_slot"] != it.Raw["planned_slot"] ||
		mustJSON(after.Raw["deadline"]) != mustJSON(it.Raw["deadline"]) {
		t.Fatalf("接受后日期应原样保留: %+v", after.Raw)
	}
}

func mustJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}
