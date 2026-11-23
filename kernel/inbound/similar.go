package inbound

import (
	"regexp"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
)

// ---- 跨来源合并的规则初筛：相似度（§9.3） ----

// strongToken 是强标识：链接，或含数字、长度不少于 4 的编号（#226、B18630823、PR-1234）。
var strongToken = regexp.MustCompile(`https?://\S+|#\d{2,}|[A-Za-z]*\d[A-Za-z0-9_-]{2,}`)

func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strongToken.FindAllString(strings.ToLower(s), -1) {
		if len([]rune(t)) >= 4 || strings.HasPrefix(t, "#") {
			out[t] = true
		}
	}
	return out
}

// similarScore = 标题的字符二元组 Dice 系数 + 共享强标识 0.3 + 摘要 Dice × 0.2，上限 1。
// 两个标题共有的开头（「起草：」「Re: [仓库]」「会议：」这类模板前缀）不说明是同一件事：两边去掉它之后都还有内容时只比剩下的部分。
func similarScore(aTitle, aSummary, bTitle, bSummary string) float64 {
	s := similarity(stripCommonPrefix(normalizeText(aTitle), normalizeText(bTitle)))
	ta, tb := tokens(aTitle+" "+aSummary), tokens(bTitle+" "+bSummary)
	for t := range ta {
		if tb[t] {
			s += 0.3
			break
		}
	}
	if aSummary != "" && bSummary != "" {
		s += 0.2 * similarity(normalizeText(aSummary), normalizeText(bSummary))
	}
	return min(s, 1)
}

// originSource 是事项的来源 id；本人或 Agent 直接建的事项没有来源（Origin 为空），记为空串。
func originSource(it domain.Item) string {
	if it.Origin == nil {
		return ""
	}
	return it.Origin.Source
}

func stripCommonPrefix(a, b string) (string, string) {
	ra, rb := []rune(a), []rune(b)
	n := 0
	for n < len(ra) && n < len(rb) && ra[n] == rb[n] {
		n++
	}
	if n < 2 || n == len(ra) || n == len(rb) {
		return a, b
	}
	return string(ra[n:]), string(rb[n:])
}

func round2(f float64) float64 { return float64(int(f*100+0.5)) / 100 }

// sendable：标题与摘要属于来源内容，外发范围未选择或为 none 的位置不送。
func sendable(o domain.Outbound, locality string) bool {
	sc := registry.ScopeFor(o, locality)
	return sc != "" && sc != registry.ScopeNone
}
