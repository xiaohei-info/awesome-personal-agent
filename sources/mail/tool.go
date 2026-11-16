package mail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/sources/internal/content"
)

var (
	versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)
	// 认证失败只能从 stderr 识别（himalaya 无专用退出码，§9.7 待实测项）。
	authRe = regexp.MustCompile(`(?i)authenticat|login failed|invalid credentials|auth(orization)? failed|password|\b401\b|\b403\b|keyring`)
)

// checkVersion 运行 `himalaya --version` 并与 Supported 比对，返回匹配的范围。
func checkVersion(ctx context.Context, r Runner) (VersionRange, error) {
	out, errOut, err := r(ctx, []string{"--version"})
	if err != nil {
		return VersionRange{}, classify(ctx, err, errOut)
	}
	m := versionRe.FindStringSubmatch(string(out))
	if m == nil {
		return VersionRange{}, fail(ExitConfig, "无法从 --version 输出识别版本: %q", strings.TrimSpace(string(out)))
	}
	var v ver
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	for _, vr := range Supported {
		if !v.less(vr.Min) && v.less(vr.Max) {
			return vr, nil
		}
	}
	return VersionRange{}, fail(ExitConfig, "%s 版本 %s 不在声明范围内（%s）", tool, m[0], supportedText())
}

func supportedText() string {
	var p []string
	for _, vr := range Supported {
		p = append(p, fmt.Sprintf("[%d.%d.%d, %d.%d.%d)", vr.Min[0], vr.Min[1], vr.Min[2], vr.Max[0], vr.Max[1], vr.Max[2]))
	}
	return strings.Join(p, " ")
}

func call(ctx context.Context, r Runner, args []string) ([]byte, error) {
	out, errOut, err := r(ctx, args)
	if err != nil {
		return nil, classify(ctx, err, errOut)
	}
	return out, nil
}

// classify 把工具失败映射到 §9.7 退出码：未安装、认证失败 → 2；超时、网络等其余 → 1。
func classify(ctx context.Context, err error, stderr []byte) error {
	msg := strings.TrimSpace(string(stderr))
	var nf *exec.Error
	switch {
	case errors.As(err, &nf) && errors.Is(nf.Err, exec.ErrNotFound):
		return fail(ExitConfig, "未找到 %s，请先安装", tool)
	case ctx.Err() != nil:
		return fail(ExitTemporary, "%s 超时: %v", tool, ctx.Err())
	case authRe.MatchString(msg):
		return fail(ExitConfig, "%s 认证失败: %s", tool, msg)
	}
	return fail(ExitTemporary, "%s 失败: %v: %s", tool, err, msg)
}

// envelope 是 `envelope list` 输出的一项；需要 message_id 的版本范围里缺失视为输出无法解析。
type envelope struct {
	ID        flexString `json:"id"`
	MessageID string     `json:"message_id"`
	Subject   string     `json:"subject"`
	// HasAttachment 只用来在 content.missing 里写明"有附件、未取清单"。
	HasAttachment bool   `json:"has_attachment"`
	Date          string `json:"date"`
	From          struct {
		Name string `json:"name"`
		Addr string `json:"addr"`
	} `json:"from"`
}

// flexString 接受 JSON 字符串或数字（各版本信件编号类型不同）。
type flexString string

func (f *flexString) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*f = flexString(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return err
	}
	*f = flexString(n)
	return nil
}

func (f flexString) String() string { return string(f) }

var dateLayouts = []string{time.RFC3339Nano, "2006-01-02 15:04Z07:00", "2006-01-02 15:04:05Z07:00"}

func (e envelope) toEvent(needID bool) (event, time.Time, error) {
	id := strings.TrimSpace(e.MessageID)
	if needID && id == "" {
		return event{}, time.Time{}, errors.New("缺少 Message-ID")
	}
	if e.ID == "" {
		return event{}, time.Time{}, errors.New("缺少信件编号")
	}
	var at time.Time
	var err error
	for _, l := range dateLayouts {
		if at, err = time.Parse(l, e.Date); err == nil {
			break
		}
	}
	if err != nil {
		return event{}, time.Time{}, fmt.Errorf("日期无法解析: %q", e.Date)
	}
	sender := e.From.Addr
	if sender == "" {
		sender = e.From.Name
	}
	subj := strings.TrimSpace(e.Subject)
	if subj == "" {
		subj = "(无主题)" // subject 是标准事件必填字段
	}
	return event{ID: id, Type: "mail.received", Subject: subj, OccurredAt: at.UTC().Format(time.RFC3339Nano), Sender: sender}, at, nil
}

// parseRead 拆开 `message read -H … ` 的输出："头: 值\n…\n\n正文"（头可折行）。返回自己的 Message-ID、
// 线程根（设计 §9.7：References 首项，没有取 In-Reply-To，都没有就是自己）、按出现顺序的信头与不截断的正文。
// Message-ID 缺失视为输出无法解析；其余头缺失不是错误。
func parseRead(s string) (id, root string, hs []content.Header, body string, err error) {
	head, body, _ := strings.Cut(s, "\n\n")
	h := map[string]string{}
	for _, ln := range strings.Split(head, "\n") {
		if ln != "" && (ln[0] == ' ' || ln[0] == '\t') && len(hs) > 0 { // 折行续上一个头
			hs[len(hs)-1].Value += " " + ln
		} else if k, v, ok := strings.Cut(ln, ":"); ok {
			hs = append(hs, content.Header{Name: k, Value: v})
		}
	}
	for _, x := range hs {
		k := strings.ToLower(strings.TrimSpace(x.Name))
		h[k] += " " + x.Value
	}
	if id = strings.Join(strings.Fields(h["message-id"]), ""); id == "" {
		return "", "", nil, "", errors.New("缺少 Message-ID")
	}
	root = firstMsgID(h["references"])
	if root == "" {
		root = firstMsgID(h["in-reply-to"])
	}
	if root == "" {
		root = id
	}
	return id, root, hs, body, nil
}

var msgIDRe = regexp.MustCompile(`<[^<>\s]+>`)

// firstMsgID 取头值里的第一个 <Message-ID>。
func firstMsgID(v string) string { return msgIDRe.FindString(v) }
