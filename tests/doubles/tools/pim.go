package tools

import (
	"encoding/json"
	"strings"
)

// pim 邮件线程：mail-cli get --include-source 把线程头放在原始邮件 `message.source` 里（MEAS-MAIL-THREAD），内容为合成。
var pimThread = []map[string]any{
	{"dateReceived": "2026-01-02T10:00:00.000Z", "isRead": false, "messageId": "mid-p1@example.test", "sender": "A <a@example.test>", "senderAddress": "a@example.test", "senderName": "A", "subject": "报价"},
	{"dateReceived": "2026-01-03T10:00:00.000Z", "isRead": false, "messageId": "mid-p2@example.test", "sender": "B <b@example.test>", "senderAddress": "b@example.test", "senderName": "B", "subject": "Re: 报价",
		"source": "References: <mid-p1@example.test>\r\nIn-Reply-To: <mid-p1@example.test>\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n正文"},
}

func pimCall(tool, c string, args []string, root string) out {
	if len(args) == 0 {
		return failf(ExitRefused, "tool-stub: %s 缺子命令", tool)
	}
	fixture := func(name string) out {
		b, err := readFixture(root, "sources", "applepim", "testdata", name)
		if err != nil {
			return failf(ExitNoCase, "tool-stub: %v", err)
		}
		return ok(string(b))
	}
	switch tool {
	case "calendar-cli":
		if c == "pim-calendar" && args[0] == "events" {
			return fixture("events.json")
		}
	case "reminder-cli":
		if strings.HasPrefix(c, "rem-undated") && args[0] == "items" {
			return remCase(root, c, args)
		}
		if c == "pim-reminders" && args[0] == "items" {
			for _, a := range args {
				if f, ok := strings.CutPrefix(a, "--filter="); ok && (f == "upcoming" || f == "overdue") {
					return fixture("reminders-" + f + ".json")
				}
			}
		}
	case "mail-cli":
		n := 2
		switch c {
		case "mail-long-html", "mail-long-legacy": // 正文远超 16 KiB 的信：HTML 带两个附件 / 升级前就存在的同一封纯文本信
			name := strings.TrimPrefix(c, "mail-")
			if args[0] == "messages" || args[0] == "get" {
				return fixture(args[0] + "-" + name + ".json")
			}
		case "pim-mail-thread-1":
			n = 1
		case "pim-mail-thread":
		case "pim-mail":
			if args[0] == "messages" {
				return fixture("messages.json")
			}
			if args[0] == "get" {
				id := argVal(args, "--id=")
				return fixture("get-" + strings.SplitN(id, "@", 2)[0] + ".json")
			}
		default:
			return failf(ExitNoCase, "tool-stub: 未知 pim case %q", c)
		}
		msgs := pimThread[:n]
		switch args[0] {
		case "messages":
			rev := make([]map[string]any, 0, n)
			for i := n - 1; i >= 0; i-- {
				rev = append(rev, msgs[i])
			}
			b := marshal(map[string]any{"count": n, "engine": "sqlite", "mailbox": "INBOX", "success": true, "totalInMailbox": n, "messages": rev})
			return ok(string(b) + "\n")
		case "get":
			id := argVal(args, "--id=")
			for _, m := range msgs {
				if m["messageId"] == id {
					msg := map[string]any{"account": "Acct", "mailbox": "INBOX", "messageId": id, "content": "正文 " + id}
					if v, ok := m["source"]; ok {
						msg["source"] = v
					}
					b := marshal(map[string]any{"success": true, "engine": "sqlite", "message": msg})
					return ok(string(b) + "\n")
				}
			}
			return ok(`{"success":false,"error":"message not found"}` + "\n")
		}
	}
	return failf(ExitRefused, "tool-stub: %s %v 在 case %q 下没有录制", tool, args, c)
}

func argVal(args []string, prefix string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

// remCase 回放 rem-undated* case：upcoming、overdue、all 三个筛选各一份录制；--list 按列表名过滤，
// 与真实工具一致（all 是没有专门筛选时的取法，MEAS-REM-UNDATED 待真实工具证实）。
func remCase(root, c string, args []string) out {
	f := argVal(args, "--filter=")
	b, err := readFixture(root, "sources", "applepim", "testdata", "rem", c, f+".json")
	if err != nil {
		return failf(ExitNoCase, "tool-stub: %v", err)
	}
	list := argVal(args, "--list=")
	if list == "" {
		return ok(string(b))
	}
	var o struct {
		Reminders []map[string]any `json:"reminders"`
	}
	if err := json.Unmarshal(b, &o); err != nil {
		return failf(ExitNoCase, "tool-stub: %v", err)
	}
	kept := []map[string]any{}
	for _, r := range o.Reminders {
		if r["list"] == list || r["listId"] == list {
			kept = append(kept, r)
		}
	}
	return ok(string(marshal(map[string]any{"success": true, "filter": f, "count": len(kept), "reminders": kept})) + "\n")
}
