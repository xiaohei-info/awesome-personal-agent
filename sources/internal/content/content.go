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
