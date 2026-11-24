package inbound

import (
	"cmp"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ---- 关键事实（§9.3 整合校验） ----
//
// 整合后的文本必须保留成员的四类事实：金额、日期与时间、单据类编号、明确的数量。其余（版本号、CI job 与 workflow 名、注释条数、
// 耗时秒数、文件数与行数、孤立的无单位数字、提交哈希、地址片段）整合时可以归纳，不查。
// 拿不准算不算强制的，按不强制处理：每一类只认下面写明的形态，认不出的一律放过。

// 事实的类别。
const (
	factAmount   = "金额"
	factDate     = "日期时间"
	factDocument = "单据编号"
	factQuantity = "数量"
)

// fact 是必须保留的一个片段。Text 是原文里的写法，Kind 是被判成哪一类，Why 是为什么算强制（命中的是哪条形态）。
// need 是整合文本里都要出现的片段：多数就是 Text；「11月16日」这类中文写法只要求月和日的数字在，不要求写法一字不差。
type fact struct {
	Text, Kind, Why string
	need            []string
}

type factRule struct {
	kind string
	re   *regexp.Regexp // 第 1 个子表达式是命中的片段
	// accept 看命中片段 t[a:b] 与它前后的字符；返回 false 即不算（拿不准的放过），否则给出 why 与 need（nil 即 [Text]）。
	accept func(t string, a, b int) (why string, need []string, ok bool)
}

const (
	curCode = `USDT|USDC|USD|CNY|RMB|EUR|GBP|JPY|HKD|BTC|ETH`
	curCJK  = `美元|美金|人民币|港币|欧元|英镑|日元|万元|亿元|元`
	numAmt  = `\d[\d,]*(?:\.\d+)?`
	gap     = `[ \x{00a0}]{0,2}` // 数字与币种之间至多两个空格（含不换行空格）
)

var (
	amountNum    = regexp.MustCompile(numAmt)
	docWord      = regexp.MustCompile(`编号|号|ID|id`)
	nonDigitEdge = regexp.MustCompile(`[^\d]`)
)

// 按优先级排：先被前面的类别认走的片段，后面的不再认（「PR #217」里的 217 不会再被当成数量）。
var factRules = []factRule{
	{factAmount, regexp.MustCompile(`(?i)((?:[$¥￥€£]|\b(?:` + curCode + `)\b)` + gap + numAmt + `)`), func(t string, a, b int) (string, []string, bool) {
		return "数字前紧跟币种，按金额算", []string{amountNum.FindString(t[a:b])}, true
	}},
	{factAmount, regexp.MustCompile(`(?i)(` + numAmt + gap + `(?:(?:` + curCode + `)\b|` + curCJK + `))`), func(t string, a, b int) (string, []string, bool) {
		// 前面紧挨着数字或 : . / - 的是时刻、版本号的一截，不是金额。
		return "数字后紧跟币种，按金额算", []string{amountNum.FindString(t[a:b])}, !hasByte(t, a-1, func(c byte) bool { return isDigit(c) || strings.IndexByte(":./-", c) >= 0 })
	}},
	{factDocument, regexp.MustCompile(`(https?://[^\s<>"'）】」》，。；：！？、]+)`), func(t string, a, b int) (string, []string, bool) {
		return "链接路径带 PR、issue 或 run 的编号", nil, changeLinkPath.MatchString(t[a:b]) // 普通链接不在四类里
	}},
	{factDocument, regexp.MustCompile(`(?i)\b((?:PR|MR)\s*[#!-]?\s*\d+)`), func(t string, a, b int) (string, []string, bool) {
		// 「PR0」「PR-7」这类一位数的是阶段名，不是 PR 号；带 # 或 ! 的才不看位数。
		return "带 PR/MR 前缀的编号", []string{digitsOf(t[a:b])}, strings.ContainsAny(t[a:b], "#!") || len(digitsOf(t[a:b])) >= 2
	}},
	{factDocument, regexp.MustCompile(`(#\d{2,})`), func(t string, a, b int) (string, []string, bool) {
		return "带 # 前缀的编号", nil, !hasByte(t, b, isDigit)
	}},
	{factDocument, regexp.MustCompile(`((?:工单|订单|单据|运单|快递单|发票|退款单|申请单|报销单|审批单|交易)(?:编号|号|ID|id)?\s*[:：#]?\s*[A-Za-z]*\d[\w-]*)`), func(t string, a, b int) (string, []string, bool) {
		g := t[a:b]
		i := strings.IndexAny(g, "0123456789")
		// 「订单 3 笔」这类不是编号：名字后没有「号」「编号」「ID」的，至少要 4 位数字。
		if !docWord.MatchString(g[:i]) && len(digitsOf(g)) < 4 {
			return "", nil, false
		}
		return "单据名后的编号", []string{strings.TrimRight(g[i:], "-")}, true
	}},
	{factDocument, regexp.MustCompile(`(?i)\b((?:order|ticket|issue|invoice|incident|run)(?:\s*(?:id|no\.?|number))?\s*[:#]?\s*[A-Za-z]{0,6}[-_]?\d{4,}[\w-]*)`), func(t string, a, b int) (string, []string, bool) {
		g := t[a:b]
		return "单据名后的编号", []string{strings.TrimRight(g[strings.IndexAny(g, "0123456789"):], "-")}, true
	}},
	{factDocument, regexp.MustCompile(`\b([A-Z][A-Z0-9]{1,9}-\d{2,}(?:-\d+)*)\b`), func(t string, a, b int) (string, []string, bool) {
		g := t[a:b]
		return "大写前缀加连字符加数字的工单号（形如 GT-395）", nil, !nonKey[strings.ToLower(g[:strings.IndexByte(g, '-')])]
	}},
	{factDocument, regexp.MustCompile(`\b([A-Za-z]{1,3}\d{6,})\b`), func(t string, a, b int) (string, []string, bool) {
		return "字母前缀加至少 6 位数字的单号", nil, true
	}},
	{factDate, regexp.MustCompile(`(\d{4}[-/.]\d{1,2}[-/.]\d{1,2})`), func(t string, a, b int) (string, []string, bool) {
		p := nonDigitEdge.Split(t[a:b], 3)
		return "完整的年月日", nil, inRange(p[1], 1, 12) && inRange(p[2], 1, 31) && !hasByte(t, b, isDigit)
	}},
	{factDate, regexp.MustCompile(`(\d{1,4}年\d{1,2}月(?:\d{1,2}[日号])?|\d{1,2}月\d{1,2}[日号])`), func(t string, a, b int) (string, []string, bool) {
		return "含年月日字样的日期", numbersOf(t[a:b]), true
	}},
	{factDate, regexp.MustCompile(`(\d{1,2}:\d{2}(?::\d{2})?)`), func(t string, a, b int) (string, []string, bool) {
		p := strings.Split(t[a:b], ":")
		edge := func(i int) bool { return i >= 0 && i < len(t) && (isDigit(t[i]) || t[i] == ':') }
		return "时:分的时刻", nil, inRange(p[0], 0, 23) && inRange(p[1], 0, 59) && !edge(a-1) && !edge(b)
	}},
	{factDate, regexp.MustCompile(`(\d{1,2}点(?:\d{1,2}分|半)?)`), func(t string, a, b int) (string, []string, bool) {
		return "含「点」字样的时刻", numbersOf(t[a:b]), true
	}},
	{factDate, regexp.MustCompile(`((?:1[0-2]|0?[1-9])/(?:[12]\d|3[01]|0?[1-9]))`), func(t string, a, b int) (string, []string, bool) {
		before := func(c byte) bool { return isDigit(c) || strings.IndexByte("/.:-", c) >= 0 }
		return "月/日", nil, len(digitsOf(t[a:b])) >= 3 && !hasByte(t, a-1, before) && !hasByte(t, b, func(c byte) bool { return isDigit(c) || c == '/' }) // 「3/4」这类像分数的不算
	}},
	{factDate, regexp.MustCompile(`((?:0[1-9]|1[0-2])-(?:0[1-9]|[12]\d|3[01]))`), func(t string, a, b int) (string, []string, bool) {
		bad := func(c byte) bool { return isDigit(c) || strings.IndexByte("/.:-", c) >= 0 }
		return "两位的月-日", nil, !hasByte(t, a-1, bad) && !hasByte(t, b, bad) // 前后连着数字或分隔符的是编号、范围的一截
	}},
	{factQuantity, regexp.MustCompile(`(\d+\s*(?:件|个|笔|份|张|台|套|批|(?i:pcs|pieces|units)))`), func(t string, a, b int) (string, []string, bool) {
		g := t[a:b]
		num := g[:len(g)-len(strings.TrimLeft(g, "0123456789"))]
		why := "数字后带量词「" + strings.TrimSpace(g[len(num):]) + "」"
		// 序数、小数点后的、文件数、行数、条数、时长之类不是明确的数量。
		return why, []string{num}, !(strings.HasSuffix(strings.TrimRight(t[:a], " "), "第") || hasByte(t, a-1, func(c byte) bool { return c == '.' || c == '/' }) || notCount.MatchString(t[b:]))
	}},
}

var (
	changeLinkPath = regexp.MustCompile(`(?i)/(?:pulls?|issues|merge_requests|actions/runs|runs)/\d+`)
	// 大写前缀加连字符加数字，但不是工单号的常见写法。
	nonKey = map[string]bool{"sha": true, "md": true, "utf": true, "iso": true, "tls": true, "ssl": true, "aes": true, "rsa": true, "http": true, "gb": true, "ipv": true}
	// 量词后面跟着这些的是文件数、行数、条数、时长等，不是明确的数量。
	notCount = regexp.MustCompile(`(?i)^\s*(?:文件|行|注释|评论|字符|字段|函数|测试|用例|检查|提交|分支|步骤|阶段|章节|文档|页|依赖|参数|模块|目录|秒|分钟|小时|月|周|天|工作流|job|workflow)`)
)

func hasByte(t string, i int, f func(byte) bool) bool { return i >= 0 && i < len(t) && f(t[i]) }

func digitsOf(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

// numbersOf 是 s 里每一段连续数字。
func numbersOf(s string) []string { return regexp.MustCompile(`\d+`).FindAllString(s, -1) }

func inRange(s string, lo, hi int) bool {
	n, err := strconv.Atoi(s)
	return err == nil && n >= lo && n <= hi
}

func trimNum(s string) string { return strings.TrimRight(s, "0123456789,. \t") }

// keyFacts 是一条建议标题与摘要里必须出现在整合文本里的片段，按类别判定，每个都带类别与原因。
func keyFacts(title, summary string) []fact {
	text := title + " " + summary
	var out []fact
	var taken [][2]int
	for _, r := range factRules {
		for _, m := range r.re.FindAllStringSubmatchIndex(text, -1) {
			a, b := m[2], m[3]
			if slices.ContainsFunc(taken, func(s [2]int) bool { return a < s[1] && s[0] < b }) {
				continue
			}
			why, need, ok := r.accept(text, a, b)
			if !ok {
				continue
			}
			taken = append(taken, [2]int{a, b})
			if need == nil {
				need = []string{text[a:b]}
			}
			out = append(out, fact{Text: text[a:b], Kind: r.kind, Why: why, need: need})
		}
	}
	slices.SortFunc(out, func(a, b fact) int { return cmp.Or(cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Text, b.Text)) })
	return slices.CompactFunc(out, func(a, b fact) bool { return a.Kind == b.Kind && a.Text == b.Text })
}

// missingFacts 是 facts 里在 text 中找不到的（小写、空白合一后比）：纯数字类的片段按数字边界比，「840」不会在「1840」里算找到。
func missingFacts(text string, facts []fact) (out []fact) {
	text = normalizeText(text)
	for _, f := range facts {
		if !slices.ContainsFunc(f.need, func(n string) bool { return !hasFact(text, n) }) {
			continue
		}
		out = append(out, f)
	}
	return
}

var numericNeed = regexp.MustCompile(`^[\d,.:/-]+$`)

func hasFact(text, need string) bool {
	need = normalizeText(need)
	if !numericNeed.MatchString(need) {
		return strings.Contains(text, need)
	}
	for i := 0; ; {
		j := strings.Index(text[i:], need)
		if j < 0 {
			return false
		}
		a, b := i+j, i+j+len(need)
		if (a == 0 || !isDigit(text[a-1])) && (b == len(text) || !isDigit(text[b])) {
			return true
		}
		i = a + 1
	}
}

func isDigit(c byte) bool { return c >= '0' && c <= '9' }

// String 是报告里写的一句：缺的是哪一项、判成哪一类、为什么算强制。
func (f fact) String() string { return fmt.Sprintf("「%s」（%s：%s）", f.Text, f.Kind, f.Why) }
