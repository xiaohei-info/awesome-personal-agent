package budget

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

func runes(s string) []rune { return []rune(s) }

// ACT-BUD-02：超预算的单段正文留首 3/4、尾 1/4，中间一行"省略 N 个字符"，N 等于被省略的字符数，总长不超预算。
func TestFitSingleSegmentHeadTail(t *testing.T) {
	body := strings.Repeat("甲乙丙丁戊己庚辛壬癸", 2000) // 20000 字符
	r := Fit(Source{Body: body}, 3000)
	if n := utf8.RuneCountInString(r.Text); n > 3000 || n != r.Sent {
		t.Fatalf("送出 %d 字符（记 %d），应不超过 3000 且与留痕一致", n, r.Sent)
	}
	if r.Original != 20000 || r.Mode != ModeHeadTail {
		t.Fatalf("留痕 = %+v", r.Trace)
	}
	var omitted int
	parts := strings.Split(r.Text, "\n")
	if len(parts) != 3 {
		t.Fatalf("应是 头/省略行/尾 三段，实际 %d 段", len(parts))
	}
	if _, err := fmt.Sscanf(parts[1], "[省略 %d 个字符]", &omitted); err != nil {
		t.Fatalf("省略行 %q: %v", parts[1], err)
	}
	head, tail := runes(parts[0]), runes(parts[2])
	if got := len(head) + len(tail) + omitted; got != 20000 {
		t.Fatalf("头 %d + 尾 %d + 省略 %d = %d，应等于原文 20000", len(head), len(tail), omitted, got)
	}
	if len(head) < 2200 || len(head) > 2250 || len(tail) < 700 || len(tail) > 750 {
		t.Fatalf("头尾长度 %d/%d 应约为 2250/750", len(head), len(tail))
	}
	all := runes(body)
	if string(all[:len(head)]) != parts[0] || string(all[len(all)-len(tail):]) != parts[2] {
		t.Fatal("头尾必须逐字等于原文开头与结尾")
	}
}

func TestFitUnderBudgetUntouched(t *testing.T) {
	r := Fit(Source{Body: "  短正文  "}, 3000)
	if r.Text != "短正文" || r.Mode != ModeNone || r.Sent != 3 || r.Original != 3 {
		t.Fatalf("%+v", r)
	}
	if r := Fit(Source{Body: strings.Repeat("a", 50000)}, 0); r.Mode != ModeNone || r.Sent != 50000 {
		t.Fatalf("预算为 0 不裁剪：%+v", r.Trace)
	}
}

// ACT-BUD-03：聚合消息从最新往前整条保留，更早的整条省略并写明条数。
func TestFitMessagesNewestWhole(t *testing.T) {
	var msgs []string
	for i := 1; i <= 60; i++ {
		msgs = append(msgs, fmt.Sprintf("u%02d: %s", i, strings.Repeat("字", 100)))
	}
	r := Fit(Source{Body: "会话标题", Messages: msgs}, 1000)
	if r.Mode != ModeNewest || utf8.RuneCountInString(r.Text) > 1000 {
		t.Fatalf("mode=%s len=%d", r.Mode, utf8.RuneCountInString(r.Text))
	}
	lines := strings.Split(r.Text, "\n")
	var skipped, kept int
	for _, l := range lines {
		if _, err := fmt.Sscanf(l, "[省略更早的 %d 条消息]", &skipped); err == nil {
			continue
		}
		if strings.HasPrefix(l, "u") {
			kept++
			if utf8.RuneCountInString(l) != 105 { // "uNN: " + 100 字：没有被切开
				t.Fatalf("消息被切断：%q", l)
			}
		}
	}
	if kept == 0 || skipped == 0 || kept+skipped != 60 {
		t.Fatalf("保留 %d 条、省略 %d 条，合计应为 60", kept, skipped)
	}
	if !strings.Contains(r.Text, "u60: ") || strings.Contains(r.Text, "u01: ") {
		t.Fatal("应保留最新的、丢掉最旧的")
	}
	if !strings.HasPrefix(r.Text, "会话标题") {
		t.Fatal("正文头部应保留")
	}
	if r.Original <= 6000 {
		t.Fatalf("裁剪前字符数 %d 应覆盖全部消息", r.Original)
	}
}

func TestFitMessagesFitIsUntouched(t *testing.T) {
	r := Fit(Source{Body: "t", Messages: []string{"a: 1", "b: 2"}}, 1000)
	if r.Mode != ModeNone || r.Text != "t\na: 1\nb: 2" {
		t.Fatalf("%+v %q", r.Trace, r.Text)
	}
}

// 最新一条消息单条就超出预算时不能一条都不送：按单段规则裁这一条。
func TestFitMessagesNewestAloneTooLong(t *testing.T) {
	r := Fit(Source{Messages: []string{"old: x", "new: " + strings.Repeat("长", 5000)}}, 1000)
	if n := utf8.RuneCountInString(r.Text); n > 1000 || !strings.Contains(r.Text, "new: ") || !strings.Contains(r.Text, "[省略更早的 1 条消息]") {
		t.Fatalf("len=%d %q", n, r.Text[:40])
	}
}

// 清单（Suffix）随资料一起计入预算，且不被从中间切开。
func TestFitSuffixReservedAndIntact(t *testing.T) {
	suffix := "## 未关闭事项\n- i1 买牛奶\n- i2 交电费"
	r := Fit(Source{Body: strings.Repeat("字", 5000), Suffix: suffix}, 1000)
	if !strings.HasSuffix(r.Text, suffix) || utf8.RuneCountInString(r.Text) > 1000 || r.Mode != ModeHeadTail {
		t.Fatalf("mode=%s len=%d", r.Mode, utf8.RuneCountInString(r.Text))
	}
	if r.Original != 5000+utf8.RuneCountInString("\n\n"+suffix) {
		t.Fatalf("original=%d", r.Original)
	}
}

func TestFitTinyBudgetStillBounded(t *testing.T) {
	r := Fit(Source{Body: strings.Repeat("字", 100)}, 5)
	if r.Sent < 0 || r.Mode != ModeHeadTail {
		t.Fatalf("%+v", r.Trace)
	}
}

// REQ-WB-38：事项正文超过 4000 字符只写前 4000，注明"原文较长"并链到来源原文页。
func TestProposalBody(t *testing.T) {
	short := strings.Repeat("字", 4000)
	if got := ProposalBody(short, "ev1"); got != short {
		t.Fatal("不超过 4000 字符原样写入")
	}
	long := strings.Repeat("乙", 4001)
	got := ProposalBody(long, "ev_9")
	if !strings.HasPrefix(got, strings.Repeat("乙", 4000)+"\n") || strings.Contains(got[len(strings.Repeat("乙", 4000)):], "乙") {
		t.Fatal("应恰好保留前 4000 字符")
	}
	if !strings.Contains(got, "原文较长") || !strings.Contains(got, "/sources/events/ev_9") {
		t.Fatalf("缺少注明或链接：%q", got[4000:])
	}
	if ProposalBody("", "e") != "" {
		t.Fatal("空正文保持为空")
	}
}
