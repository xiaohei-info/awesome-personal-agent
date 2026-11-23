// Package content 是来源程序共用的内容包（设计 §9.2、附录 D.8）：HTML 转文本、信头规范化、
// 按字节分批。只在 sources/ 一侧使用，内核不 import。
//
// HTML 转文本用 github.com/JohannesKaufmann/html-to-markdown/v2（MIT，纯 Go），选型理由见设计 §18。
// 换库只改 HTMLToText。
package content

import (
	"errors"
	"fmt"
	"mime"
	"net/textproto"
	"regexp"
	"strings"

	"github.com/JohannesKaufmann/html-to-markdown/v2/converter"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/base"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/commonmark"
	"github.com/JohannesKaufmann/html-to-markdown/v2/plugin/table"
)

// ErrConvert 表示 HTML 转文本失败。调用方应让 detail 留空、原文照常保存，并以 partial 自报（D.8）。
var ErrConvert = errors.New("html to text conversion failed")

// 不带 WithDomain：相对地址原样保留，转换不访问网络；script/style 由库默认丢弃。
var htmlConv = converter.NewConverter(converter.WithPlugins(base.NewBasePlugin(), commonmark.NewCommonmarkPlugin(), table.NewTablePlugin()))

// HTMLToText 把 HTML 转成保留链接地址、列表、引用与表格结构的文本。只做纯文本变换：
// 不执行脚本、不取远程资源。
func HTMLToText(s string) (out string, err error) {
	if strings.TrimSpace(s) == "" {
		return "", nil
	}
	defer func() {
		if r := recover(); r != nil {
			out, err = "", fmt.Errorf("%w: %v", ErrConvert, r)
		}
	}()
	out, err = htmlConv.ConvertString(s)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrConvert, err)
	}
	// 库在相邻两个列表间插入的 Markdown 分隔注释，对文本没有意义。
	out = strings.ReplaceAll(out, "<!--THE END-->\n\n", "")
	return strings.TrimSpace(out), nil
}

// Header 是一个未规范化的信头（名与原始取值，顺序即邮件里的顺序）。
type Header struct{ Name, Value string }

// NormalizeHeaders 产出事件 headers 字段：名规范成 MIME 标准写法，取值解开折行并解 RFC 2047
// 编码字，去掉空值；同名多值按出现顺序成字符串数组，单值是字符串。
func NormalizeHeaders(in []Header) map[string]any {
	multi := map[string][]string{}
	var dec mime.WordDecoder
	for _, h := range in {
		v := strings.Join(strings.Fields(h.Value), " ")
		if d, err := dec.DecodeHeader(v); err == nil {
			v = d
		}
		name := textproto.CanonicalMIMEHeaderKey(strings.TrimSpace(h.Name))
		if name == "" || v == "" {
			continue
		}
		multi[name] = append(multi[name], v)
	}
	out := make(map[string]any, len(multi))
	for k, vs := range multi {
		if len(vs) == 1 {
			out[k] = vs[0]
		} else {
			out[k] = vs
		}
	}
	return out
}

// Batcher 控制一次拉取的输出字节预算：累计接近 max_source_output_mb 的四分之三时停下，
// 调用方把游标停在已输出的最后一条（设计 §9.2）。
type Batcher struct{ budget, used int }

func NewBatcher(maxOutputMB int) *Batcher { return &Batcher{budget: maxOutputMB << 20 / 4 * 3} }

func (b *Batcher) Budget() int { return b.budget }
func (b *Batcher) Used() int   { return b.used }

// Full 表示预算已用尽，不应再取新条目。
func (b *Batcher) Full() bool { return b.used >= b.budget }

// Add 登记一条将要输出的事件（字节数含换行）。放得下返回 true；放不下返回 false，该条不输出。
// 第一条总被接收：否则单条超过预算时游标永远不前进（单条上限由内核按 max_source_event_mb 另行拒收）。
func (b *Batcher) Add(n int) bool {
	if b.used > 0 && b.used+n > b.budget || b.used >= b.budget {
		return false
	}
	b.used += n
	return true
}

// githubThreadRe 是代码托管平台通知邮件的 Message-ID 写法：<owner/repo/pull|issues/编号[/…]@github.com>，
// 原信、评审、评论、状态变更都以同一个 PR 或 issue 开头（附录 D.2）。
var githubThreadRe = regexp.MustCompile(`(?i)^<?([a-z0-9_.-]+)/([a-z0-9_.-]+)/(pull|issues)/(\d+)(?:/[^@<>\s]*)?@github\.com>?$`)

// NotificationLink 给代码托管平台的通知邮件补上它所说对象的规范链接（https://github.com/owner/repo/pull/编号），
// 与 github 来源同一对象的事件链接逐字相同，内核据此按"同一链接"把邮件与平台事件并成一条建议（§9.3 合并键）。
// 发件人不是该平台的通知地址、或 ids（自己的 Message-ID、References、In-Reply-To）里没有这种写法时返回空：宁可不补，不猜。
func NotificationLink(sender string, ids ...string) string {
	if !fromGitHub(sender) {
		return ""
	}
	for _, s := range ids {
		for _, id := range strings.Fields(s) {
			if m := githubThreadRe.FindStringSubmatch(id); m != nil {
				return "https://github.com/" + m[1] + "/" + m[2] + "/" + strings.ToLower(m[3]) + "/" + m[4]
			}
		}
	}
	return ""
}

func fromGitHub(sender string) bool {
	if i := strings.LastIndex(sender, "<"); i >= 0 {
		sender = strings.TrimSuffix(sender[i+1:], ">")
	}
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(sender)), "@github.com")
}

// githubFooterRe 是通知邮件纯文本正文的固定页脚「… view it on GitHub: <链接>」；githubObjectRe 是正文里任何一处 PR 或 issue 的链接。
var (
	githubFooterRe = regexp.MustCompile(`(?i)view it on GitHub:?\s*<?(https://github\.com/[^\s<>]+)`)
	githubObjectRe = regexp.MustCompile(`https://github\.com/([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+)/(pull|issues)/(\d+)`)
)

// NotificationBodyLink 从通知邮件的正文里取它所说对象的规范链接，给没有线程头的旧邮件回填用（附录 D.2）。
// 只在发件人是该平台的通知地址、正文有「view it on GitHub」页脚且页脚指向一个 PR 或 issue、
// 并且正文里出现的每一个 PR 或 issue 链接都是同一个对象时才给；否则返回空。不看标题，不猜。
func NotificationBodyLink(sender, body string) string {
	if !fromGitHub(sender) {
		return ""
	}
	f := githubFooterRe.FindStringSubmatch(body)
	if f == nil {
		return ""
	}
	m := githubObjectRe.FindStringSubmatch(f[1])
	if m == nil || !strings.HasPrefix(f[1], m[0]) || (len(f[1]) > len(m[0]) && !strings.ContainsRune("/#?", rune(f[1][len(m[0])]))) {
		return "" // 页脚指向别处（运行记录、仓库首页），或编号后还连着别的字符
	}
	link := m[0]
	for _, o := range githubObjectRe.FindAllString(body, -1) {
		if o != link { // 正文还说到别的 PR 或 issue：拿不准是哪一个
			return ""
		}
	}
	return link
}
