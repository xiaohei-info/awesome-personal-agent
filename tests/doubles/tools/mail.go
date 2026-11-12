package tools

import (
	"encoding/json"
	"fmt"
	"strings"
)

// mailMsg 是合成邮件（线程等录制样本里没有的场景）；形态与 himalaya v1.1.0 的 envelope list / message read 一致。
type mailMsg struct {
	id, mid, subject, from, date string
	refs, inReplyTo              string
}

func (m mailMsg) envelope() map[string]any {
	name, addr := "AgentMail", m.from
	if i := strings.Index(m.from, "<"); i > 0 {
		name, addr = strings.TrimSpace(m.from[:i]), strings.Trim(m.from[i:], "<>")
	}
	return map[string]any{"id": m.id, "flags": []string{}, "subject": m.subject,
		"from": map[string]any{"name": name, "addr": addr}, "to": map[string]any{"name": nil, "addr": "agent@example.com"},
		"date": m.date, "has_attachment": false}
}

// read 按 -H 顺序输出请求的头，空行后接正文，作为一个 JSON 字符串（himalaya 的 --output json）。
func (m mailMsg) read(args []string) string {
	hv := map[string]string{"message-id": m.mid, "references": m.refs, "in-reply-to": m.inReplyTo,
		"subject": m.subject, "from": m.from, "date": m.date}
	var sb strings.Builder
	for i, a := range args {
		if a == "-H" && i+1 < len(args) {
			if v := hv[strings.ToLower(args[i+1])]; v != "" {
				fmt.Fprintf(&sb, "%s: %s\n", args[i+1], v)
			}
		}
	}
	sb.WriteString("\nBODY-PLACEHOLDER\n")
	b := marshal(sb.String())
	return string(b)
}

var (
	threadMsgs = []mailMsg{
		{"11", "<mid-t1@example.com>", "[APA-LIVE-TEST] thread root", "agent@example.com", "2026-11-06 06:00+00:00", "", ""},
		{"12", "<mid-t2@example.com>", "Re: [APA-LIVE-TEST] thread root", "agent@example.com", "2026-11-06 07:00+00:00", "<mid-t1@example.com>", "<mid-t1@example.com>"},
		{"13", "<mid-t3@example.com>", "Re: [APA-LIVE-TEST] thread root", "agent@example.com", "2026-11-06 08:00+00:00", "", "<mid-t1@example.com>"},
	}
	mid7      = mailMsg{"7", "<mid-7@example.com>", "[APA-LIVE-TEST] mid probe 7", "agent@example.com", "2026-11-06 09:00+00:00", "", ""}
	oldReply2 = mailMsg{"21", "<mid-o2@example.com>", "Re: 报价", "王经理 <wang@example.com>", "2026-11-06 06:00+00:00", "<mid-o1@example.com>", ""}
	oldReply3 = mailMsg{"22", "<mid-o3@example.com>", "Re: 报价", "王经理 <wang@example.com>", "2026-11-06 07:00+00:00", "<mid-o1@example.com>", ""}
)

// 合成 case：版本 1.1.0 的输出形态，邮件集合如下。
var synthMail = map[string][]mailMsg{
	"mail-thread-1":    threadMsgs[:1],
	"mail-thread-2":    threadMsgs[:2],
	"mail-thread":      threadMsgs,
	"mail-old-reply":   {oldReply2},
	"mail-old-reply-2": {oldReply2, oldReply3},
}

func mailCall(c string, args []string, root string) out {
	const ver = "1.1.0"
	dir := []string{"sources", "mail", "testdata", ver}
	versionTxt := func(d []string) out {
		b, err := readFixture(root, append(append([]string{}, d...), "version.txt")...)
		if err != nil {
			return failf(ExitNoCase, "tool-stub: %v", err)
		}
		return ok(string(b))
	}
	if len(args) == 1 && args[0] == "--version" {
		switch c {
		case "mail-version-0.1":
			return ok("himalaya 0.1.0\n")
		case "mail-2.0.0":
			return versionTxt([]string{"sources", "mail", "testdata", "2.0.0"})
		}
		return versionTxt(dir)
	}
	if c == "mail-version-0.1" {
		return failf(1, "tool-stub: unexpected call after out-of-range version")
	}
	if len(args) < 2 || (args[0] != "envelope" || args[1] != "list") && (args[0] != "message" || args[1] != "read") {
		return failf(ExitRefused, "tool-stub: himalaya %v 不是录制过的只读命令", args)
	}
	if c == "mail-auth-fail" {
		return failf(1, "Error: cannot authenticate to IMAP server: Invalid credentials")
	}
	isList := args[0] == "envelope"
	if msgs, synth := synthMail[c]; synth {
		return synthMailOut(msgs, isList, args)
	}
	d := dir
	switch c {
	case "mail-1.1.0", "mail-1.1.0-new", "mail-garbled", "mail-garbled-json":
	case "mail-2.0.0":
		d = []string{"sources", "mail", "testdata", "2.0.0"}
	default:
		return failf(ExitNoCase, "tool-stub: 未知邮件 case %q", c)
	}
	if isList {
		if pageOf(args) > 1 {
			return ok("[]\n")
		}
		b, err := readFixture(root, append(append([]string{}, d...), "list.json")...)
		if err != nil {
			return failf(ExitNoCase, "tool-stub: %v", err)
		}
		if c == "mail-1.1.0-new" { // 追加 mid-7
			var l []any
			json.Unmarshal(b, &l)
			b = marshal(append(l, mid7.envelope()))
		}
		return ok(string(b))
	}
	ref := args2ref(args)
	switch {
	case c == "mail-garbled":
		return ok(`"BODY-ONLY-NO-HEADER"` + "\n")
	case c == "mail-garbled-json":
		return ok("<<<not json>>>\n")
	case c == "mail-1.1.0-new" && ref == mid7.id:
		return ok(mid7.read(args) + "\n")
	}
	b, err := readFixture(root, append(append([]string{}, d...), "read-"+ref+".json")...)
	if err != nil {
		return failf(1, "Error: message %s not found", ref)
	}
	return ok(string(b))
}

func synthMailOut(msgs []mailMsg, isList bool, args []string) out {
	if isList {
		if pageOf(args) > 1 {
			return ok("[]\n")
		}
		env := make([]map[string]any, 0, len(msgs))
		for i := len(msgs) - 1; i >= 0; i-- { // 新到旧
			env = append(env, msgs[i].envelope())
		}
		b := marshal(env)
		return ok(string(b) + "\n")
	}
	ref := args2ref(args)
	for _, m := range msgs {
		if m.id == ref {
			return ok(m.read(args) + "\n")
		}
	}
	return failf(1, "Error: message %s not found", ref)
}

func args2ref(args []string) string {
	a := trimJSONFlag(args)
	return a[len(a)-1]
}

func pageOf(args []string) int {
	for i, a := range args {
		if a == "--page" && i+1 < len(args) {
			var n int
			fmt.Sscan(args[i+1], &n)
			return n
		}
	}
	return 1
}
