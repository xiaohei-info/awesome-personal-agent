// Package srcstub 是桩来源脚本 SRC_STUB：按 case 输出标准事件 JSONL（主文档 §9.2），并支持 fetch 模式回固定字节。
// 只用于测试与验收环境，不得被 apa 二进制或内核代码导入；不连任何真实账号。
//
// 取 case：环境变量 SRC_CASE（case 名；或指向一个文件，文件内容是 case 名，或以 `{` 开头的原样 JSONL，供执行者追加）；
// 缺省时读可执行文件同目录的 `case` 文件（来源子进程环境是白名单时用）。
// fetch 行为：SRC_FETCH 或同目录 `fetch` 文件：空/ok=固定字节；fail[:码]=非零退出码加 stderr 一句原因；big=33 MiB；size:N=N 字节。
package srcstub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// 替身自己的退出码：拉取与 fetch 的内核语义见主文档 §9.2（1 临时失败、3 永久失败）。
const (
	ExitFail    = 1  // 配置的 fetch 失败缺省码
	ExitBadArgs = 3  // 输入不合法（stdin 缺 ref、未知 case）
	ExitNoCase  = 99 // 没有设置 case
)

// 固定大小，验收按这些数字断言。
const (
	BigBodyBytes   = 200 * 1024
	MidBodyChars   = 3000
	DigestChars    = 30000
	OversizeBytes  = 1536 * 1024 // oversize-mid 第 2 条的正文，约 1.5 MiB
	FetchOversize  = 33 << 20    // 超过取回上限 32 MiB
	FetchFixedSize = 4096
	TooManyMsgs    = 201
	ChatLongMsgs   = 60
)

var base = time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)

type m = map[string]any

func at(i int) string { return base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339) }

// Body 生成确定的 n 字节 ASCII 正文，tag 不同则开头不同（用于"开头改动"的更新版本）。
func Body(tag string, n int) string {
	var b strings.Builder
	for i := 0; b.Len() < n; i++ {
		fmt.Fprintf(&b, "%s line %06d: the quick brown fox jumps over the lazy dog.\n", tag, i)
	}
	return b.String()[:n]
}

// FetchBytes 是 fetch 模式对 ref 回的固定字节。
func FetchBytes(ref string) []byte {
	return bytes.Repeat([]byte("srcstub-attachment:"+ref+"\n"), FetchFixedSize)[:FetchFixedSize]
}

func ev(id, typ, subject string, extra m) m {
	e := m{"id": id, "type": typ, "subject": subject, "occurred_at": at(0)}
	for k, v := range extra {
		e[k] = v
	}
	return e
}

func chat(n int, textLen int) []m {
	out := make([]m, n)
	for i := range out {
		sender := []string{"alice", "bob"}[i%2]
		text := fmt.Sprintf("msg %03d ", i)
		text += strings.Repeat("x", textLen-len(text))
		out[i] = m{"sender": sender, "text": text, "at": at(i)}
	}
	return out
}

func joinText(items []m) string {
	var s []string
	for _, it := range items {
		s = append(s, it["sender"].(string)+": "+it["text"].(string))
	}
	return strings.Join(s, "\n")
}

// Events 返回 case 对应的事件；ok 为假表示未知 case。env 取 SRC_IMG_URL、SRC_OVERSIZE_BYTES 两个可选覆盖。
func Events(name string, env map[string]string) (evs []m, ok bool) {
	switch name {
	case "big-body", "big-body-reply", "big-body-update":
		e := ev("big-"+name, "mail.received", "Big body", m{"detail": Body(name, BigBodyBytes), "sender": "alice@example.com",
			"correlation_keys": []string{"thread-big"}, "occurred_at": at(0)})
		if name == "big-body-reply" {
			e["subject"], e["occurred_at"], e["sender"] = "Re: Big body", at(5), "bob@example.com"
		}
		if name == "big-body-update" {
			e["occurred_at"] = at(10)
		}
		return []m{e}, true
	case "ten-big":
		for i := 0; i < 10; i++ {
			evs = append(evs, ev(fmt.Sprintf("ten-big-%d", i), "mail.received", fmt.Sprintf("Big %d", i), m{"detail": Body(fmt.Sprint(i), BigBodyBytes), "occurred_at": at(i)}))
		}
		return evs, true
	case "mail-like":
		img := env["SRC_IMG_URL"]
		if img == "" {
			img = "http://tracker.invalid/pixel.png"
		}
		return []m{ev("mail-like-1", "mail.received", "Quarterly report", m{
			"summary": "Please review the attached report.",
			"detail":  "Hi,\n\nPlease review the attached report.\n\n[portal](https://example.com/portal)\n- item one\n- item two\n",
			"sender":  "alice@example.com", "correlation_keys": []string{"<mid-1@example.com>"},
			"headers": m{"From": "alice@example.com", "To": []string{"me@example.com", "team@example.com"}, "Cc": []string{"bob@example.com"},
				"Subject": "Quarterly report", "Message-ID": "<mid-1@example.com>", "Date": "Tue, 10 Nov 2026 09:00:00 +0000",
				"List-Id": "<reports.example.com>"},
			"attachments": []m{
				{"name": "report.pdf", "media_type": "application/pdf", "size": FetchFixedSize, "ref": "att-0"},
				{"name": "logo.png", "media_type": "image/png", "size": FetchFixedSize, "inline": true, "ref": "att-1"},
			},
			"original": m{"media_type": "text/html", "text": "<html><body><p>Hi,</p><p>Please review the attached report.</p>" +
				"<img src=\"" + img + "\"><script>alert(1)</script></body></html>"},
		})}, true
	case "chat-two-things":
		items := []m{
			{"sender": "alice", "text": "Can you publish the v2 docs by Friday?", "at": at(0)},
			{"sender": "bob", "text": "Sure, I will draft the changelog first.", "at": at(1)},
			{"sender": "alice", "text": "Also the invoice for the venue needs paying this week.", "at": at(2)},
			{"sender": "bob", "text": "I will ask finance about the invoice.", "at": at(3)},
		}
		return []m{ev("chat-two-things-1", "chat.message", "Team chat", m{"items": items, "detail": joinText(items), "sender": "alice", "correlation_keys": []string{"chat-room-1"}})}, true
	case "chat-long":
		items := chat(ChatLongMsgs, 100)
		return []m{ev("chat-long-1", "chat.message", "Long chat", m{"items": items, "detail": joinText(items), "sender": "alice", "correlation_keys": []string{"chat-room-2"}})}, true
	case "too-many-msgs":
		items := chat(TooManyMsgs, 20)
		return []m{ev("too-many-msgs-1", "chat.message", "Too many messages", m{"items": items, "detail": joinText(items)})}, true
	case "oversize-mid":
		n := OversizeBytes
		if v, err := strconv.Atoi(env["SRC_OVERSIZE_BYTES"]); err == nil && v > 0 {
			n = v
		}
		return []m{
			ev("oversize-1", "mail.received", "Normal one", m{"detail": "first normal event", "occurred_at": at(0)}),
			ev("oversize-2", "mail.received", "Oversize", m{"detail": Body("oversize", n), "occurred_at": at(1)}),
			ev("oversize-3", "mail.received", "Normal three", m{"detail": "third normal event", "occurred_at": at(2)}),
		}, true
	case "partial":
		return []m{ev("partial-1", "mail.received", "Partial mail", m{"detail": "Only the body could be read.", "sender": "alice@example.com",
			"content": m{"state": "partial", "missing": []m{
				{"part": "original", "reason": "tool cannot export raw HTML without marking the mail read"},
				{"part": "attachments", "reason": "attachment list needs the full export"},
			}}})}, true
	case "mid-body":
		return []m{ev("mid-body-1", "mail.received", "Mid body", m{"detail": Body("mid", MidBodyChars)})}, true
	case "digest-30k":
		for i := 0; i < 2; i++ {
			evs = append(evs, ev(fmt.Sprintf("digest-%d", i), "digest.note", fmt.Sprintf("Digest %d", i), m{"detail": Body(fmt.Sprint("d", i), DigestChars), "occurred_at": at(i)}))
		}
		return evs, true
	}
	return nil, false
}

func readEnv(env []string) map[string]string {
	out := map[string]string{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}

// setting 取配置：环境变量优先，其次可执行文件同目录的同名文件。
func setting(env map[string]string, key, file, binDir string) string {
	if v := env[key]; v != "" {
		return v
	}
	b, _ := os.ReadFile(filepath.Join(binDir, file))
	return strings.TrimSpace(string(b))
}

// Run 执行一次调用；args 为空是拉取，["fetch"] 是取回附件。
func Run(args, env []string, binDir string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := readEnv(env)
	if len(args) == 1 && args[0] == "fetch" {
		return fetch(setting(e, "SRC_FETCH", "fetch", binDir), stdin, stdout, stderr)
	}
	if len(args) != 0 {
		fmt.Fprintln(stderr, "src-stub: usage: src-stub [fetch]")
		return ExitBadArgs
	}
	_, _ = io.Copy(io.Discard, stdin) // 拉取输入（游标、窗口）不影响输出：去重按事件 id
	name := setting(e, "SRC_CASE", "case", binDir)
	if name == "" {
		fmt.Fprintln(stderr, "src-stub: SRC_CASE is not set")
		return ExitNoCase
	}
	if b, err := os.ReadFile(name); err == nil { // SRC_CASE 指向文件：case 名，或执行者追加的原样 JSONL
		if s := strings.TrimSpace(string(b)); strings.HasPrefix(s, "{") {
			return custom(s, stdout, stderr)
		} else {
			name = s
		}
	}
	evs, ok := Events(name, e)
	if !ok {
		fmt.Fprintf(stderr, "src-stub: unknown case %q\n", name)
		return ExitBadArgs
	}
	enc := json.NewEncoder(stdout)
	enc.SetEscapeHTML(false)
	for _, x := range evs {
		if err := enc.Encode(x); err != nil {
			fmt.Fprintln(stderr, "src-stub:", err)
			return ExitFail
		}
	}
	_ = enc.Encode(m{"cursor": name})
	return 0
}

// custom 原样输出执行者给的 JSONL；末行不是游标时补一个。
func custom(s string, stdout, stderr io.Writer) int {
	lines := strings.Split(s, "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	fmt.Fprintln(stdout, s)
	if !strings.HasPrefix(last, `{"cursor"`) {
		fmt.Fprintln(stdout, `{"cursor":"custom"}`)
	}
	return 0
}

func fetch(mode string, stdin io.Reader, stdout, stderr io.Writer) int {
	var in struct {
		Attachment struct {
			Ref  string `json:"ref"`
			Name string `json:"name"`
		} `json:"attachment"`
	}
	if err := json.NewDecoder(stdin).Decode(&in); err != nil || in.Attachment.Ref == "" {
		fmt.Fprintln(stderr, "src-stub: fetch needs stdin JSON with attachment.ref")
		return ExitBadArgs
	}
	switch {
	case mode == "" || mode == "ok":
		_, _ = stdout.Write(FetchBytes(in.Attachment.Ref))
		return 0
	case mode == "fail" || strings.HasPrefix(mode, "fail:"):
		code := ExitFail
		if c, err := strconv.Atoi(strings.TrimPrefix(mode, "fail:")); err == nil && c > 0 {
			code = c
		}
		fmt.Fprintln(stderr, "src-stub: attachment store unavailable (configured failure)")
		return code
	case mode == "big":
		return writeN(stdout, FetchOversize)
	case strings.HasPrefix(mode, "size:"):
		n, err := strconv.Atoi(strings.TrimPrefix(mode, "size:"))
		if err != nil || n < 0 {
			fmt.Fprintf(stderr, "src-stub: bad fetch mode %q\n", mode)
			return ExitBadArgs
		}
		return writeN(stdout, n)
	}
	fmt.Fprintf(stderr, "src-stub: bad fetch mode %q\n", mode)
	return ExitBadArgs
}

// writeN 流式写 n 字节固定内容（不整块分配 33 MiB）。
func writeN(w io.Writer, n int) int {
	chunk := bytes.Repeat([]byte{0xA5}, 64*1024)
	for n > 0 {
		k := min(n, len(chunk))
		if _, err := w.Write(chunk[:k]); err != nil {
			return ExitFail
		}
		n -= k
	}
	return 0
}

// SHA256 供测试与验收脚本对照。
func SHA256(b []byte) string { return fmt.Sprintf("%x", sha256.Sum256(b)) }
