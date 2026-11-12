package tools

import (
	"encoding/json"
	"strings"
)

func gogCall(c string, args []string, root string) out {
	if len(args) == 1 && args[0] == "--version" {
		if c == "cal-version-0.1" {
			return ok("v0.1.0 (commit abc1234)\n")
		}
		b, err := readFixture(root, "sources", "calendar", "testdata", "0.9.0", "version.txt")
		if err != nil {
			return failf(ExitNoCase, "tool-stub: %v", err)
		}
		return ok(string(b))
	}
	// gog 没有只读开关；替身只认 calendar events，create/delete 等一律拒绝。
	i := indexOf(args, "calendar")
	if i < 0 || i+1 >= len(args) || args[i+1] != "events" {
		return failf(ExitRefused, "tool-stub: gog %v 不是录制过的只读命令", args)
	}
	if c == "cal-auth-fail" {
		acct := "unknown"
		for _, a := range args {
			if v, ok := strings.CutPrefix(a, "--account="); ok {
				acct = v
			}
		}
		return failf(1, "No auth for calendar %s", acct)
	}
	b, err := readFixture(root, "sources", "calendar", "testdata", "0.9.0", "events.json")
	if err != nil {
		return failf(ExitNoCase, "tool-stub: %v", err)
	}
	var doc struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return failf(ExitNoCase, "tool-stub: events.json: %v", err)
	}
	find := func(id string) map[string]any {
		for _, e := range doc.Events {
			if e["id"] == id {
				return e
			}
		}
		return nil
	}
	edit := func(id, summary, updated string) {
		if e := find(id); e != nil {
			e["summary"], e["updated"] = summary, updated
		}
	}
	switch c {
	case "cal-base", "cal-version-0.1":
	case "cal-updated", "cal-updated-2", "cal-new":
		edit("a1", "保险到期日（改到周五）", "2026-03-02T08:00:00.000Z")
		edit("c1_20260310T010000Z", "周会（改线上）", "2026-03-02T08:30:00.000Z")
		if c != "cal-updated" {
			edit("a1", "保险到期日（改到下周一）", "2026-03-03T08:00:00.000Z")
		}
		if c == "cal-new" {
			doc.Events = append(doc.Events, map[string]any{"id": "e1", "iCalUID": "e1@google.com", "status": "confirmed", "summary": "新增的体检",
				"htmlLink": "https://calendar.google.com/event?eid=e1", "created": "2026-03-04T08:00:00.000Z", "updated": "2026-03-04T08:00:00.000Z",
				"start": map[string]any{"dateTime": "2026-03-25T09:00:00+08:00"}, "end": map[string]any{"dateTime": "2026-03-25T10:00:00+08:00"}})
		}
	default:
		return failf(ExitNoCase, "tool-stub: 未知日历 case %q", c)
	}
	o := marshal(doc)
	return ok(string(o) + "\n")
}

func indexOf(s []string, v string) int {
	for i, x := range s {
		if x == v {
			return i
		}
	}
	return -1
}
