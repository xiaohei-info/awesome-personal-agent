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
	ext                          *mailExt // 长邮件样本才有：更多信头、长正文、附件标记
}

// mailExt 是长邮件样本（himalaya-long、mail-many-long）的附加内容：固定信头的其余取值、正文长度（字节）与 has_attachment。
type mailExt struct {
	hdr     map[string]string // 小写信头名 → 取值
	bodyLen int
	attach  bool
}

func (m mailMsg) envelope() map[string]any {
	name, addr := "AgentMail", m.from
	if i := strings.Index(m.from, "<"); i > 0 {
		name, addr = strings.TrimSpace(m.from[:i]), strings.Trim(m.from[i:], "<>")
	}
	return map[string]any{"id": m.id, "flags": []string{}, "subject": m.subject,
		"from": map[string]any{"name": name, "addr": addr}, "to": map[string]any{"name": nil, "addr": "agent@example.com"},
		"date": m.date, "has_attachment": m.ext != nil && m.ext.attach}
}

// read 按 -H 顺序输出请求的头，空行后接正文，作为一个 JSON 字符串（himalaya 的 --output json）。
func (m mailMsg) read(args []string) string {
	hv := map[string]string{"message-id": m.mid, "references": m.refs, "in-reply-to": m.inReplyTo,
		"subject": m.subject, "from": m.from, "date": m.date}
	body := "BODY-PLACEHOLDER"
	if m.ext != nil {
		for k, v := range m.ext.hdr {
			hv[k] = v
		}
		body = longBody(m.mid, m.ext.bodyLen)
	}
	var sb strings.Builder
	for i, a := range args {
		if a == "-H" && i+1 < len(args) {
			if v := hv[strings.ToLower(args[i+1])]; v != "" {
				fmt.Fprintf(&sb, "%s: %s\n", args[i+1], v)
			}
		}
	}
	sb.WriteString("\n" + body + "\n")
	b := marshal(sb.String())
	return string(b)
}

var (
	// 合成邮件的时间要晚于录制样本里最晚的一封（mid-7，09:00）并按验收出场顺序递增：适配器的游标是已读最晚一封的时间，
	// 更早的信读不到（验收 ACT-MAIL-06 之后才放线程与旧回信）。
	threadMsgs = []mailMsg{
		{"11", "<mid-t1@example.com>", "[APA-LIVE-TEST] thread root", "agent@example.com", "2026-11-06 12:00+00:00", "", "", nil},
		{"12", "<mid-t2@example.com>", "Re: [APA-LIVE-TEST] thread root", "agent@example.com", "2026-11-06 13:00+00:00", "<mid-t1@example.com>", "<mid-t1@example.com>", nil},
		{"13", "<mid-t3@example.com>", "Re: [APA-LIVE-TEST] thread root", "agent@example.com", "2026-11-06 14:00+00:00", "", "<mid-t1@example.com>", nil},
	}
	mid7      = mailMsg{"7", "<mid-7@example.com>", "[APA-LIVE-TEST] mid probe 7", "agent@example.com", "2026-11-06 09:00+00:00", "", "", nil}
	oldReply2 = mailMsg{"21", "<mid-o2@example.com>", "Re: 报价", "王经理 <wang@example.com>", "2026-11-06 15:00+00:00", "<mid-o1@example.com>", "", nil}
	oldReply3 = mailMsg{"22", "<mid-o3@example.com>", "Re: 报价", "王经理 <wang@example.com>", "2026-11-06 16:00+00:00", "<mid-o1@example.com>", "", nil}
)

// longBody 是确定性的合成正文：恰好 n 字节，按行带 Message-ID 与行号，测试能逐字比对"没有被截断"。
func longBody(mid string, n int) string {
	var sb strings.Builder
	for i := 0; sb.Len() < n; i++ {
		fmt.Fprintf(&sb, "%s line %05d lorem ipsum dolor sit amet\n", mid, i)
	}
	return sb.String()[:n]
}

// LongBody 供测试读取 himalaya-long / mail-many-long 的正文期望值。
func LongBody(mid string, n int) string { return longBody(mid, n) }

var (
	// himalaya-long：一封正文 40 KiB（大于旧的 16 KiB 截断点）、带附件、固定信头齐全的邮件。
	longMsg = mailMsg{"31", "<mid-long@example.com>", "[APA-LIVE-TEST] long mail", "AgentMail <agent@example.com>", "2026-11-06 17:00+00:00", "<mid-t1@example.com>", "<mid-t2@example.com>",
		&mailExt{bodyLen: LongBodyBytes, attach: true, hdr: map[string]string{
			"to": "me@example.com", "cc": "team@example.com", "reply-to": "noreply@example.com",
			"list-id": "<news.example.com>", "list-unsubscribe": "<mailto:unsub@example.com>",
			"list-unsubscribe-post": "List-Unsubscribe=One-Click", "content-type": "text/plain; charset=utf-8"}}}
	// mail-many-long：20 封各 110 KiB，每小时一封，供按字节分批（总量略大于 2 MiB：max_source_output_mb=2 时一次放不下，每轮至多 13 封）。
	manyLong = func() []mailMsg {
		var ms []mailMsg
		for i := 0; i < ManyLongCount; i++ {
			ms = append(ms, mailMsg{fmt.Sprint(40 + i), fmt.Sprintf("<mid-ml%02d@example.com>", i), fmt.Sprintf("[APA-LIVE-TEST] many long %02d", i), "agent@example.com",
				fmt.Sprintf("2026-11-07 %02d:00+00:00", i), "", "", &mailExt{bodyLen: ManyLongBytes, hdr: map[string]string{"to": "me@example.com", "content-type": "text/plain; charset=utf-8"}}})
		}
		return ms
	}()
)

// 长邮件样本的规格，供测试断言。
const (
	LongBodyBytes = 40 << 10
	ManyLongCount = 20
	ManyLongBytes = 110 << 10
)

// 合成 case：版本 1.1.0 的输出形态，邮件集合如下。
var synthMail = map[string][]mailMsg{
	"mail-thread-1":    threadMsgs[:1],
	"mail-thread-2":    threadMsgs[:2],
	"mail-thread":      threadMsgs,
	"mail-old-reply":   {oldReply2},
	"mail-old-reply-2": {oldReply2, oldReply3},
	"himalaya-long":    {longMsg},
	"mail-many-long":   manyLong,
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
		// 追加 mid-7。garbled 两个 case 紧接在 mid-7 入账之后（验收 ACT-MAIL-06、07），邮箱里已有它：
		// 否则列表里的信全都早于游标，适配器一封也不会去读，"输出无法解析"就永远触发不了。
		if c == "mail-1.1.0-new" || c == "mail-garbled" || c == "mail-garbled-json" {
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
