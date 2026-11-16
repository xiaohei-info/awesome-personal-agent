//go:build e2e

package sourcesv2

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"testing"
)

// TestFlowCalendarRaw 覆盖 ACT-OAD-04 的日历部分：经 Web 读到的日程事件，起止、地点、组织者、参与者与状态在 headers 里。
func TestFlowCalendarRaw(t *testing.T) {
	k := setup(t)
	programs(t)
	const src = "src_cal_raw"
	k.step("ACT-OAD-04 日历事件的 headers", func(t *testing.T) {
		k.SetToolCase("cal-base")
		k.webSource(map[string]any{"id": src, "name": src, "type": "script", "command": filepath.Join(progDir, "calendar"), "schedule": "1s", "timeout": 20,
			"source_type": "calendar", "dedupe_domain": "acc-cal-raw", "config": map[string]any{"account": "acc@example.com"}})
		var dentist SrcEvent
		k.until("牙医复诊入账", func() bool {
			for _, e := range k.events(src) {
				if e.Subject == "牙医复诊" {
					dentist = e
					return true
				}
			}
			return false
		})
		r := k.fullEvent(dentist)
		want := map[string]any{
			"Start": "2026-03-12T09:30:00+08:00", "End": "2026-03-12T10:30:00+08:00", "Location": "市口腔医院",
			"Organizer": "口腔医院 <clinic@example.com>", "Status": "confirmed",
			"Attendee": []any{"我 <me@example.com>", "family@example.com"},
		}
		if !reflect.DeepEqual(r.Headers, want) {
			b, _ := json.Marshal(r.Headers)
			t.Errorf("headers 不符: %s", b)
		}
	})
}
