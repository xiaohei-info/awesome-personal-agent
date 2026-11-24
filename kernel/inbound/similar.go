package inbound

import (
	"regexp"
	"slices"
	"strings"
	"unicode"

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

// ---- 仓库 + PR 号（§9.3 强标识） ----
// 代码托管的变更请求只认通用写法：链接里的 <owner>/<repo>/pull(s)|merge_requests/<n>，或「[owner/repo] … PR #n」「owner/repo 的 PR #n」。

var (
	changeLink = regexp.MustCompile(`(?i)https?://[^/\s]+/([\w.-]+/[\w.-]+)/(?:pulls?|merge_requests)/(\d+)`)
	repoTag    = regexp.MustCompile(`\[([\w.-]+/[\w.-]+)\]`)
	repoBefore = regexp.MustCompile(`(?i)([\w.-]+/[\w.-]+)\s*(?:的\s*)?(?:PR|MR)\s*[#!]\d+`)
	changeNum  = regexp.MustCompile(`(?i)\b(?:PR|MR)\s*[#!](\d+)`)
)

// changeRef 是一条记录说的是哪个仓库的哪个 PR（「owner/repo#n」，小写）；说不出仓库或号时为空。
func changeRef(it domain.Item) string {
	ref := ""
	if it.Origin != nil {
		ref = it.Origin.Ref
	}
	texts := []string{ref, mergeKeyOfItem(it), it.Title, it.Summary}
	for _, s := range texts {
		if m := changeLink.FindStringSubmatch(s); m != nil {
			return strings.ToLower(m[1] + "#" + m[2])
		}
	}
	repo := ""
	for _, s := range texts[1:] {
		if m := repoTag.FindStringSubmatch(s); m != nil {
			repo = m[1]
			break
		}
		if m := repoBefore.FindStringSubmatch(s); m != nil {
			repo = m[1]
			break
		}
	}
	for _, s := range texts[1:] {
		if m := changeNum.FindStringSubmatch(s); m != nil && repo != "" {
			return strings.ToLower(repo + "#" + m[1])
		}
	}
	return ""
}

// otherChange 为真：两条说的是同一仓库的不同 PR——永远是不同的事，不合并、不互相并入。
func otherChange(a, b string) bool {
	ra, _, oka := strings.Cut(a, "#")
	rb, _, okb := strings.Cut(b, "#")
	return oka && okb && ra == rb && a != b
}

// ---- 关键事实（§9.3 整合校验） ----

var factNumber = regexp.MustCompile(`^\d+(?:[.:/-]\d+)*$`)

// keyFacts 是一条建议标题与摘要里必须原样出现在整合文本里的片段：强标识（链接、#编号、带数字的编号）与至少两位的独立数字（金额、日期、时刻）。
func keyFacts(title, summary string) []string {
	s := title + " " + summary
	var out []string
	for t := range tokens(s) {
		if i := strings.IndexFunc(t, func(r rune) bool { return r > unicode.MaxASCII }); i > 0 {
			t = t[:i] // 链接后面紧跟的中文标点不算链接
		}
		out = append(out, t)
	}
	// 独立数字：按字母数字与 . : / - 以外的字符切开，整段是数字（可带这几个连接符）才算，编号里嵌的数字不单算。
	for _, f := range strings.FieldsFunc(s, func(r rune) bool {
		return r > unicode.MaxASCII || !(unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune(".:/-", r))
	}) {
		f = strings.Trim(f, ".:/-")
		if factNumber.MatchString(f) && len(strings.Map(func(r rune) rune {
			if unicode.IsDigit(r) {
				return r
			}
			return -1
		}, f)) >= 2 {
			out = append(out, f)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// missingFacts 是 facts 里在 text 中找不到的（小写、空白合一后按子串比）。
func missingFacts(text string, facts []string) (out []string) {
	text = normalizeText(text)
	for _, f := range facts {
		if !strings.Contains(text, normalizeText(f)) {
			out = append(out, f)
		}
	}
	return
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
