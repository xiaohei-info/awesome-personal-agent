//go:build e2e

package sourcesv2

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestFlowOtherAdaptersRawOAD04 统一验收 ACT-OAD-04：同一个环境里登记日历、提醒事项、http-json、dir-watch 四个
// 来源程序，各等一轮后经 Web 读事件。各来源节点的单项测试（*_raw_test.go）不替代本测试。
// 工具替身的 case 是全局的：先日历，再提醒事项；日历来源取到事件后停用，免得被后一个 case 干扰。
func TestFlowOtherAdaptersRawOAD04(t *testing.T) {
	dir := programs(t)
	for name, pkg := range map[string]string{"http-json": "./sources/httpjson/cmd/http-json", "dir-watch": "./sources/dirwatch/cmd/dir-watch"} {
		b := exec.Command("go", "build", "-o", filepath.Join(dir, name), pkg)
		b.Dir = repoRoot()
		if out, err := b.CombinedOutput(); err != nil {
			t.Fatalf("go build %s: %v\n%s", name, err, out)
		}
	}
	k := setup(t)

	k.step("ACT-OAD-04 日程 headers 含起止、地点、组织者、参与者与状态", func(t *testing.T) {
		k.SetToolCase("cal-base")
		k.webSource(map[string]any{"id": "src_oad_cal", "name": "cal", "type": "script", "command": filepath.Join(dir, "calendar"), "schedule": "1s", "timeout": 20,
			"source_type": "calendar", "dedupe_domain": "acc-oad-cal", "config": map[string]any{"account": "acc@example.com"}})
		var ev SrcEvent
		k.until("牙医复诊入账", func() bool {
			for _, e := range k.events("src_oad_cal") {
				if e.Subject == "牙医复诊" {
					ev = e
					return true
				}
			}
			return false
		})
		want := map[string]any{
			"Start": "2026-03-12T09:30:00+08:00", "End": "2026-03-12T10:30:00+08:00", "Location": "市口腔医院",
			"Organizer": "口腔医院 <clinic@example.com>", "Status": "confirmed",
			"Attendee": []any{"我 <me@example.com>", "family@example.com"},
		}
		if got := k.fullEvent(ev).Headers; !reflect.DeepEqual(got, want) {
			t.Errorf("日程 headers = %v，期望 %v", got, want)
		}
		k.patchSource("src_oad_cal", map[string]any{"enabled": false})
	})

	k.step("ACT-OAD-04 提醒 headers 含列表、到期日与优先级", func(t *testing.T) {
		k.SetToolCase("rem-undated")
		k.applepimSource("src_oad_rem", "reminder", map[string]any{"kind": "reminders", "undated_lists": []string{"随手记"}})
		var due SrcEvent
		k.until("到期提醒 RD1 入账", func() bool {
			for _, e := range k.events("src_oad_rem") {
				if len(e.CorrelationKeys) == 1 && e.CorrelationKeys[0] == "RD1" {
					due = e
					return true
				}
			}
			return false
		})
		h := k.fullEvent(due).Headers
		if due.Type != "reminder.due" || h["Due"] != "2026-01-10" || h["List"] == nil || h["Priority"] == nil {
			t.Errorf("提醒 headers 应含列表、到期日与优先级: type=%s %v", due.Type, h)
		}
	})

	k.step("ACT-OAD-04 http-json 的 original 是被映射的那一条 JSON 原样", func(t *testing.T) {
		const row = `{"uid":"o1","kind":"ticket","headline":"工单 O1","at":"2030-01-01T00:00:00Z","extra":{"n":1.50,"tags":["a",  "b"]}}`
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, `[`+row+`,{"uid":"o2","kind":"ticket","headline":"工单 O2","at":"2030-01-02T00:00:00Z"}]`)
		}))
		defer srv.Close()
		k.addPull("src_oad_json", "http-json", "ticket", nil, map[string]any{"url": srv.URL,
			"mapping": map[string]any{"id": "uid", "type": "kind", "subject": "headline", "occurred_at": "at"}})
		ev := k.settled("src_oad_json", "o1")
		if r := k.fullEvent(ev); r.Original.MediaType != "application/json" || r.Original.Text != row {
			t.Errorf("original 应是被映射的那一条 JSON 原样: %q %q", r.Original.MediaType, r.Original.Text)
		}
		if ev.Subject != "工单 O1" {
			t.Errorf("字段仍按映射取: %+v", ev)
		}
	})

	k.step("ACT-OAD-04 dir-watch 事件只有路径、修改时间与大小，账本里没有文件内容", func(t *testing.T) {
		const marker = "OAD04-FILE-CONTENT-MARKER"
		watch := filepath.Join(t.TempDir(), "watch")
		if err := os.MkdirAll(watch, 0o755); err != nil {
			t.Fatal(err)
		}
		file := filepath.Join(watch, "note.md")
		if err := os.WriteFile(file, []byte(marker), 0o644); err != nil {
			t.Fatal(err)
		}
		at := time.Now().Add(-time.Hour)
		if err := os.Chtimes(file, at, at); err != nil {
			t.Fatal(err)
		}
		k.webSource(map[string]any{"id": "src_oad_dir", "name": "dir", "type": "script", "command": filepath.Join(dir, "dir-watch"), "schedule": "1s", "timeout": 20,
			"source_type": "dir", "dedupe_domain": "acc-oad-dir", "config": map[string]any{"dir": watch}})
		k.until("文件事件入账", func() bool { return len(k.events("src_oad_dir")) == 1 })
		ev := k.events("src_oad_dir")[0]
		if ev.Type != "file.changed" || ev.Subject != "note.md" || !strings.HasPrefix(ev.DedupeKey.EventID, "changed:note.md:") ||
			!strings.HasSuffix(ev.DedupeKey.EventID, ":"+fmt.Sprint(len(marker))) {
			t.Errorf("事件应只带路径、mtime、大小: %+v", ev)
		}
		for name, body := range map[string]string{"事件列表": k.get("/v1/sources/src_oad_dir/events?limit=500"), "原文": k.webOK("GET", "/source-events/"+ev.ID, "", 200)} {
			if strings.Contains(body, marker) {
				t.Errorf("文件内容不应进账本（%s）: %s", name, body)
			}
			if name == "原文" && !strings.Contains(body, "file://"+file) {
				t.Errorf("link 应指向文件本身: %s", body)
			}
		}
	})
}

// TestFlowReminderOpenRule 覆盖 ACT-REM-02：reminder.open 命中内置规则 reminder-open，直接进建议流，不调用模型；
// 其后改标题、补到期日（ACT-REM-03）仍是同一条事项。
func TestFlowReminderOpenRule(t *testing.T) {
	k := setup(t)
	programs(t)
	const src = "src_rem_rule"
	var open SrcEvent
	k.step("ACT-REM-02 reminder.open 直接进建议流，没有调用模型", func(t *testing.T) {
		k.SetToolCase("rem-undated")
		k.applepimSource(src, "reminder", map[string]any{"kind": "reminders", "undated_lists": []string{"随手记"}})
		k.until("RU1 入账并处置", func() bool {
			for _, e := range k.events(src) {
				if len(e.CorrelationKeys) == 1 && e.CorrelationKeys[0] == "RU1" && e.Disposition != "pending" {
					open = e
					return true
				}
			}
			return false
		})
		if open.Type != "reminder.open" || open.Disposition != "proposed" {
			t.Fatalf("reminder.open 应为 proposed: type=%s disposition=%s", open.Type, open.Disposition)
		}
		if !k.eventRule(open, "reminder-open") {
			t.Errorf("应命中规则 reminder-open: %+v", k.why(open.ID))
		}
		if open.Item == "" {
			t.Fatal("proposed 事件应关联一条事项")
		}
		found := false
		for _, id := range k.suggestions() {
			found = found || id == open.Item
		}
		if !found {
			t.Errorf("事项 %s 不在建议流 %v", open.Item, k.suggestions())
		}
		if n := k.modelLines(); n != 0 {
			t.Errorf("规则直接放行，模型替身不应有请求: %d", n)
		}
	})
	k.step("ACT-REM-03 改标题、补到期日后仍是同一条事项", func(t *testing.T) {
		k.SetToolCase("rem-undated-renamed")
		k.until("改标题后的新版本入账", func() bool { return countKey(k.events(src), "RU1") == 2 })
		k.SetToolCase("rem-undated-dated")
		k.until("补到期日后的新版本入账", func() bool { return countKey(k.events(src), "RU1") == 3 })
		items := map[string]bool{}
		for _, e := range k.events(src) {
			if len(e.CorrelationKeys) == 1 && e.CorrelationKeys[0] == "RU1" && e.Item != "" {
				items[e.Item] = true
			}
		}
		if len(items) != 1 || !items[open.Item] {
			t.Errorf("RU1 的各版本应都落在原事项 %s: %v", open.Item, items)
		}
	})
}

func countKey(evs []SrcEvent, key string) int {
	n := 0
	for _, e := range evs {
		if len(e.CorrelationKeys) == 1 && e.CorrelationKeys[0] == key {
			n++
		}
	}
	return n
}
