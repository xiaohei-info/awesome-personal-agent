package tools

import (
	"strings"
)

// pim 邮件线程：mail-cli 的 References/In-Reply-To 取法尚未实测（MEAS-MAIL-THREAD），这里按
// `message` 对象带 `references`、`inReplyTo` 字符串字段合成；真实输出形态以后由实测结论修正。
var pimThread = []map[string]any{
	{"dateReceived": "2026-01-02T10:00:00.000Z", "isRead": false, "messageId": "mid-p1@example.test", "sender": "A <a@example.test>", "senderAddress": "a@example.test", "senderName": "A", "subject": "报价"},
	{"dateReceived": "2026-01-03T10:00:00.000Z", "isRead": false, "messageId": "mid-p2@example.test", "sender": "B <b@example.test>", "senderAddress": "b@example.test", "senderName": "B", "subject": "Re: 报价",
		"references": "<mid-p1@example.test>", "inReplyTo": "<mid-p1@example.test>"},
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
					for _, k := range []string{"references", "inReplyTo"} {
						if v, ok := m[k]; ok {
							msg[k] = v
						}
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
