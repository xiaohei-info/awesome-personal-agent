// Package budget 是调用模型前的资料预算与裁剪（设计 §8.4、§9.3）。账本里的原文不截断：
// 裁剪只发生在组织一次模型调用时，结果只用于这一次调用，不回写任何来源事件、事项或账本字段。
// 入站流水线、判断级联与简报的来源原文文件都经这里裁剪，裁剪量用 Trace 留痕。
package budget

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// 裁剪方式（Trace.Mode）。
const (
	ModeNone          = "none"            // 未超预算，原样送出
	ModeHeadTail      = "head_tail"       // 单段正文：留开头 3/4 与结尾 1/4，中间写明省略的字符数
	ModeNewest        = "newest_messages" // 聚合消息：从最新一条往前整条保留，更早的整条省略
	ProposalBodyChars = 4000              // 事项正文的字符上限（§9.3）
)

// Source 是一次调用的资料：Body 是正文（含摘要与链接），Messages 是聚合事件的消息（每条一个整体，旧在前），
// Suffix 是随资料附上的未关闭事项清单，不被裁剪，但计入预算。
type Source struct {
	Body     string
	Messages []string
	Suffix   string
}

// Trace 是一次裁剪的留痕：实际送出的字符数、裁剪前的字符数与方式。
type Trace struct {
	Sent     int    `json:"sent_chars"`
	Original int    `json:"original_chars"`
	Mode     string `json:"mode"`
}

// Result 是裁剪后要送出的资料及其留痕。
type Result struct {
	Text string
	Trace
}

func count(s string) int { return utf8.RuneCountInString(s) }

func headTailMarker(n int) string { return fmt.Sprintf("\n[省略 %d 个字符]\n", n) }

func messagesMarker(n int) string { return fmt.Sprintf("[省略更早的 %d 条消息]", n) }

// join 按送出时的顺序拼出完整资料：正文、各条消息各占一行、清单另起一段。
func join(body string, msgs []string, suffix string) string {
	parts := make([]string, 0, len(msgs)+1)
	if body != "" {
		parts = append(parts, body)
	}
	parts = append(parts, msgs...)
	out := strings.Join(parts, "\n")
	if suffix != "" {
		out = strings.TrimSpace(out + "\n\n" + suffix)
	}
	return strings.TrimSpace(out)
}

// Fit 把资料裁到 max 个字符以内（max 不大于 0 表示不设预算）。先于它的外发范围裁剪由调用方完成。
func Fit(s Source, max int) Result {
	s.Body, s.Suffix = strings.TrimSpace(s.Body), strings.TrimSpace(s.Suffix)
	full := join(s.Body, s.Messages, s.Suffix)
	orig := count(full)
	if max <= 0 || orig <= max {
		return Result{full, Trace{orig, orig, ModeNone}}
	}
	// 清单不裁，先从预算里扣掉；留出至少四分之一给资料本身，免得清单把资料挤没。
	room := max
	if s.Suffix != "" {
		room = max - count(s.Suffix) - 2
	}
	room = max4(room, max/4)
	var text, mode string
	if len(s.Messages) > 0 {
		text, mode = fitMessages(s.Body, s.Messages, room), ModeNewest
	} else {
		text, mode = headTail(s.Body, room), ModeHeadTail
	}
	if s.Suffix != "" {
		text = strings.TrimSpace(text + "\n\n" + s.Suffix)
	}
	return Result{text, Trace{count(text), orig, mode}}
}

func max4(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// headTail 单段裁剪：保留的字符里开头占 3/4、结尾占 1/4，省略行本身也计入预算，所以总长不超过 max。
func headTail(s string, max int) string {
	r := []rune(s)
	n := len(r)
	if n <= max {
		return s
	}
	keep := max - count(headTailMarker(n)) // 省略数不会比 n 多一位，这个上界保证总长不超
	if keep < 0 {
		keep = 0
	}
	head := keep * 3 / 4
	tail := keep - head
	return string(r[:head]) + headTailMarker(n-keep) + string(r[n-tail:])
}

// fitMessages 聚合消息：正文头部（至多占一半）照单段规则裁，消息从最新一条往前整条保留，放不下的更早的整条省略并写明条数。
// 最新一条单条就放不下时仍要送它：按单段规则裁这一条，不能一条都不送。
func fitMessages(body string, msgs []string, max int) string {
	if count(body) > max/2 {
		body = headTail(body, max/2)
	}
	left := max - count(body)
	if body != "" {
		left-- // 与后面内容之间的换行
	}
	// 为省略行预留位置：条数至多 len(msgs) 位数。
	left -= count(messagesMarker(len(msgs))) + 1
	var keep []string
	for i := len(msgs) - 1; i >= 0; i-- {
		need := count(msgs[i]) + 1
		if need > left {
			if len(keep) == 0 {
				keep = append(keep, headTail(msgs[i], max4(left-1, 1)))
				return compose(body, msgs[:i], keep)
			}
			return compose(body, msgs[:i+1], keep)
		}
		left -= need
		keep = append(keep, msgs[i])
	}
	return compose(body, nil, keep)
}

// compose 把正文、省略行与保留的消息（keep 是新到旧）按原顺序拼起来；dropped 是被省略的更早的消息。
func compose(body string, dropped, keep []string) string {
	var lines []string
	if body != "" {
		lines = append(lines, body)
	}
	if len(dropped) > 0 {
		lines = append(lines, messagesMarker(len(dropped)))
	}
	for i := len(keep) - 1; i >= 0; i-- {
		lines = append(lines, keep[i])
	}
	return strings.Join(lines, "\n")
}

// ProposalBody 是建议事项的正文（§9.3）：来源原文不超过 4000 个字符时原样写入，超过时只写前 4000 个字符，
// 末尾注明"原文较长"并链到来源原文页。来源对象更新刷新事项正文时用同一条规则。账本里的原文不受影响。
func ProposalBody(detail, event string) string {
	if len(detail) <= ProposalBodyChars || count(detail) <= ProposalBodyChars {
		return detail
	}
	r := []rune(detail)
	return string(r[:ProposalBodyChars]) + fmt.Sprintf("\n\n（原文较长，这里只写前 %d 个字符，完整原文见[来源原文页](/sources/events/%s)）", ProposalBodyChars, event)
}
