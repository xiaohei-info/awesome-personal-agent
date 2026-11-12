package telegram

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
)

func testLogger(t *testing.T) *log.Logger { return log.New(testWriter{t}, "", 0) }

func newBot(t *testing.T) *fakebot.Bot {
	fb, err := fakebot.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(fb.Close)
	return fb
}

func memStore() Store { return &mem{recs: map[string]Record{}} }

// mem 是测试用的内存 Store。
type mem struct{ recs map[string]Record }

func (m *mem) Lookup(id string) (Record, bool) { r, ok := m.recs[id]; return r, ok }
func (m *mem) Save(id string, r Record) error  { m.recs[id] = r; return nil }
func (m *mem) Find(c int64, msg int) (string, Record, bool) {
	for id, r := range m.recs {
		if r.Chat == c && r.Msg == msg {
			return id, r, true
		}
	}
	return "", Record{}, false
}

// ACT-TG-02、ACT-DAY-05：决策送达 Telegram，消息含问题、选项、推荐，ack 带外部消息 ref。
func TestDecisionIsSentWithQuestionOptionsRecommendationAndAcked(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	f.enqueue(decision())
	start(t, f, fb, memStore())

	eventually(t, "ack", func() bool { a, _, _ := f.snapshot(); return len(a["decision:item-1:3"]) == 1 })
	sent := fb.Sent()
	if len(sent) != 1 {
		t.Fatalf("want exactly one message, got %d", len(sent))
	}
	for _, want := range []string{"合并到哪个分支", "要合到 main 还是 release？", "main: 合到 main", "release: 合到 release", "推荐：main", "/items/item-1"} {
		if !strings.Contains(sent[0].Text, want) {
			t.Errorf("message lacks %q:\n%s", want, sent[0].Text)
		}
	}
	if len(sent[0].Buttons) != 2 {
		t.Errorf("want 2 option buttons, got %v", sent[0].Buttons)
	}
	acks, _, _ := f.snapshot()
	if want := "4242:" + strconv.Itoa(sent[0].ID); acks["decision:item-1:3"][0] != want {
		t.Errorf("ack ref = %q, want %q", acks["decision:item-1:3"][0], want)
	}
}

// ACT-TG-03、ACT-DAY-05：点选项按钮 / 回复选项，回答带回答人与 ref 转达给内核，本人得到确认。
func TestAnswerByButtonAndByReplyIsForwarded(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	f.enqueue(decision())
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	eventually(t, "ack", func() bool { a, _, _ := f.snapshot(); return len(a["decision:item-1:3"]) == 1 })
	msg := fb.Sent()[0]

	fb.Press(chat, msg.ID, owner, "1")
	eventually(t, "button answer", func() bool { _, _, a := f.snapshot(); return len(a) == 1 })
	fb.Reply(chat, msg.ID, owner, "main")
	fb.Reply(chat, msg.ID, owner, "同意")
	eventually(t, "reply answers", func() bool { _, _, a := f.snapshot(); return len(a) == 3 })

	_, _, as := f.snapshot()
	ref := "4242:" + strconv.Itoa(msg.ID)
	want := []answerCall{
		{ID: "decision:item-1:3", Ref: ref, Sender: "owner", Choice: "release"},
		{ID: "decision:item-1:3", Ref: ref, Sender: "owner", Choice: "main"},
		{ID: "decision:item-1:3", Ref: ref, Sender: "owner", Text: "同意"},
	}
	got := map[answerCall]bool{}
	for _, a := range as {
		got[a] = true
	}
	for _, w := range want {
		if !got[w] {
			t.Errorf("answer %+v was not forwarded; got %+v", w, as)
		}
	}
	eventually(t, "confirmation", func() bool {
		for _, s := range fb.Sent() {
			if strings.Contains(s.Text, "事项会继续") {
				return true
			}
		}
		return false
	})
}

// 纵深防御：内核没标 web_only 却拒绝回答时，拒绝原因和 Web 链接原样发给本人，不当作已回答。
func TestKernelRejectionIsRelayedWithWebLink(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	web := "/items/item-fin"
	f.answerFn = func(answerCall) (int, *APIError) {
		return 403, &APIError{Code: "web_only", Why: "this decision can only be answered in the Web UI", WebURL: &web}
	}
	d := decision()
	d.ID, d.Content.WebURL = "decision:item-fin:1", web
	f.enqueue(d)
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	msg := fb.Sent()[0]
	if strings.Contains(msg.Text, "作答链接") || !strings.Contains(msg.Text, f.srv.URL+web) {
		t.Errorf("web_only message must carry the Web link and no form link:\n%s", msg.Text)
	}
	eventually(t, "ack", func() bool { a, _, _ := f.snapshot(); return len(a[d.ID]) == 1 })

	fb.Reply(chat, msg.ID, owner, "同意")
	eventually(t, "rejection notice", func() bool {
		for _, s := range fb.Sent()[1:] {
			if strings.Contains(s.Text, "回答未被接受") && strings.Contains(s.Text, f.srv.URL+web) {
				return true
			}
		}
		return false
	})
	for _, s := range fb.Sent() {
		if strings.Contains(s.Text, "事项会继续") {
			t.Fatalf("a rejected answer must not be confirmed: %s", s.Text)
		}
	}
}

// 内核给了一次性表单链接时，ext 原样发出（链接由内核发放，ext 不另取）。
func TestFormLinkFromKernelIsSent(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	d := decision()
	d.Content.FormURL = "/forms/tok123"
	f.enqueue(d)
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	if !strings.Contains(fb.Sent()[0].Text, f.srv.URL+"/forms/tok123") {
		t.Errorf("form link missing:\n%s", fb.Sent()[0].Text)
	}
}

// 非决策投递（提醒）只发文字和 Web 链接，没有按钮。
func TestReminderHasNoButtons(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	f.enqueue(Delivery{ID: "remind:1", Class: "reminder", Version: 1, Content: Content{Title: "截止已过", WebURL: "https://web.example/items/1"}})
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	if s := fb.Sent()[0]; len(s.Buttons) != 0 || !strings.Contains(s.Text, "https://web.example/items/1") {
		t.Errorf("unexpected reminder message: %+v", s)
	}
}

// ACT-DGA-11：规则 alert 出口的提醒没有事项（item 为空）、带 origin；ext 照常发送并回报送达，不报错、不跳过，后续投递继续领取。
func TestAlertReminderWithoutItemIsSentAndAcked(t *testing.T) {
	var alert Delivery
	wire := `{"id":"alert:sev_1","item":"","origin":{"source_event":"sev_1","rule":"p/source_rules/over","source":"src_m"},"class":"reminder","route":"channel:acc","version":1,` +
		`"state":"claimed","content":{"title":"磁盘","summary":"用量 96%","web_url":"https://mon.example/disk-a"}}`
	if err := json.Unmarshal([]byte(wire), &alert); err != nil {
		t.Fatal(err)
	}
	f, fb := newFakeAPA(t), newBot(t)
	f.enqueue(alert)
	f.enqueue(Delivery{ID: "remind:2", Class: "reminder", Version: 1, Content: Content{Title: "下一条"}})
	start(t, f, fb, memStore())
	eventually(t, "both messages", func() bool { return len(fb.Sent()) == 2 })
	s := fb.Sent()[0]
	if len(s.Buttons) != 0 || !strings.Contains(s.Text, "磁盘") || !strings.Contains(s.Text, "用量 96%") || !strings.Contains(s.Text, "https://mon.example/disk-a") {
		t.Errorf("alert message: %+v", s)
	}
	eventually(t, "acks", func() bool {
		acks, fails, _ := f.snapshot()
		return len(acks["alert:sev_1"]) == 1 && len(acks["remind:2"]) == 1 && len(fails) == 0
	})
}

// ACT-TG-06：发送后、回报 ack 前进程被杀。新进程（同一状态文件）在租约到期重领后只补报 ack，不再发第二条。
func TestRestartAfterSendBeforeAckDoesNotResend(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	path := filepath.Join(t.TempDir(), "state.json")
	f.ackErr = http.StatusInternalServerError // 第一个进程的 ack 没有成功
	f.enqueue(decision())

	st1, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	stop := start(t, f, fb, st1)
	eventually(t, "first ack attempt", func() bool { a, _, _ := f.snapshot(); return len(a["decision:item-1:3"]) >= 1 })
	stop() // 进程终止；发送记录已落盘
	if n := len(fb.Sent()); n != 1 {
		t.Fatalf("want 1 message before restart, got %d", n)
	}

	f.mu.Lock()
	f.ackErr = 0
	f.queue, f.acks = nil, map[string][]string{}
	f.requeue("decision:item-1:3") // 租约到期，重新可领
	f.mu.Unlock()
	st2, err := OpenFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	start(t, f, fb, st2)
	eventually(t, "re-ack", func() bool { a, _, _ := f.snapshot(); return len(a["decision:item-1:3"]) == 1 })

	if n := len(fb.Sent()); n != 1 {
		t.Fatalf("restart re-sent the message: %d messages", n)
	}
	acks, _, _ := f.snapshot()
	if want := "4242:" + strconv.Itoa(fb.Sent()[0].ID); acks["decision:item-1:3"][0] != want {
		t.Errorf("re-ack ref = %q, want the original %q", acks["decision:item-1:3"][0], want)
	}
	// 重启后点旧消息的按钮仍能回答（选项随记录落盘）。
	fb.Press(chat, fb.Sent()[0].ID, owner, "0")
	eventually(t, "answer after restart", func() bool { _, _, a := f.snapshot(); return len(a) == 1 })
}

// 发送失败如实回报 fail（由内核退避重试），不 ack、不留发送记录。
func TestSendFailureIsReportedAsFail(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	fb.SendErr = "Bad Request: chat not found"
	st := memStore()
	f.enqueue(decision())
	start(t, f, fb, st)
	eventually(t, "fail report", func() bool { _, fl, _ := f.snapshot(); return len(fl["decision:item-1:3"]) == 1 })
	acks, fails, _ := f.snapshot()
	if len(acks) != 0 || !strings.Contains(fails["decision:item-1:3"][0], "chat not found") {
		t.Errorf("acks=%v fails=%v", acks, fails)
	}
	if _, ok := st.Lookup("decision:item-1:3"); ok {
		t.Error("a failed send must leave no sent-record")
	}
}

// 版本已作废（ack 回 409）：ext 不再对该投递报告任何东西。
func TestStaleVersionIsDroppedWithoutFail(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	f.stale["decision:item-1:3"] = true
	f.enqueue(decision())
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	f.enqueue(Delivery{ID: "n:2", Class: "reminder", Version: 1, Content: Content{Title: "next"}})
	eventually(t, "next delivery handled", func() bool { a, _, _ := f.snapshot(); return len(a["n:2"]) == 1 })
	if _, fl, _ := f.snapshot(); len(fl) != 0 {
		t.Errorf("ext reported fail for a stale delivery: %v", fl)
	}
}

// 只接受本人会话：别的会话里的回复和按钮不转达。
func TestOtherChatsAreIgnored(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	f.enqueue(decision())
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	msg := fb.Sent()[0]
	fb.Press(999, msg.ID, owner, "0")
	fb.Reply(999, msg.ID, owner, "main")
	fb.Reply(chat, msg.ID, owner, "release") // 哨兵：本人会话的回答到了，前面的也该处理完了
	eventually(t, "sentinel", func() bool { _, _, a := f.snapshot(); return len(a) >= 1 })
	if _, _, a := f.snapshot(); len(a) != 1 || a[0].Choice != "release" {
		t.Errorf("answers from other chats leaked: %+v", a)
	}
}

// 内核回 409（已有结果）：告知本人，不重复生效。
func TestSecondAnswerConflictIsTold(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	f.answerFn = func(answerCall) (int, *APIError) {
		return 409, &APIError{Code: "conflict", Why: "already resolved"}
	}
	f.enqueue(decision())
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	fb.Reply(chat, fb.Sent()[0].ID, owner, "main")
	eventually(t, "conflict notice", func() bool {
		s := fb.Sent()
		return len(s) > 1 && strings.Contains(s[len(s)-1].Text, "已经有结果")
	})
}

// 新建 Ext 的配置错误直接暴露，不静默降级。
func TestNewRejectsIncompleteConfig(t *testing.T) {
	if _, err := New(Config{APAURL: "http://x"}, memStore(), log.Default()); err == nil {
		t.Fatal("want error for missing key/token/chat")
	}
}

// TG_CHAT 不是整数时 Main 返回错误。
func TestMainRequiresNumericChat(t *testing.T) {
	err := Main(context.Background(), func(k string) string { return map[string]string{"TG_CHAT": "abc"}[k] }, testWriter{t}, memStore())
	if err == nil || !strings.Contains(err.Error(), "TG_CHAT") {
		t.Fatalf("err = %v", err)
	}
}

// webOnlyDelivery 是内核对 web_only 决策实际产出的形态（kernel/attention：只有标题、摘要、Web 链接和 web_only 标记）。
func webOnlyDelivery() Delivery {
	return Delivery{ID: "decision:item-fin:1", Class: "decision", Version: 1, Content: Content{
		Title: "确认续保付款", Summary: "是否支付 3,200 元？", WebURL: "/items/item-fin", WebOnly: true}}
}

// ACT-TG-04、ACT-LIF-05：web_only 决策的消息只有标题、摘要和 Web 链接——没有选项、推荐、按钮、表单链接；
// 本人在 Telegram 回复"同意"，ext 不转达（内核不会收到回答），回给本人 Web 链接。
func TestWebOnlyMessageIsLinkOnlyAndReplyIsPointedToWeb(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	d := webOnlyDelivery()
	f.enqueue(d)
	start(t, f, fb, memStore())
	eventually(t, "ack", func() bool { a, _, _ := f.snapshot(); return len(a[d.ID]) == 1 })

	msg := fb.Sent()[0]
	link := f.srv.URL + "/items/item-fin"
	for _, want := range []string{"确认续保付款", "是否支付 3,200 元？", link} {
		if !strings.Contains(msg.Text, want) {
			t.Errorf("web_only message lacks %q:\n%s", want, msg.Text)
		}
	}
	for _, banned := range []string{"选项", "推荐", "作答链接"} {
		if strings.Contains(msg.Text, banned) {
			t.Errorf("web_only message must not contain %q:\n%s", banned, msg.Text)
		}
	}
	if len(msg.Buttons) != 0 {
		t.Errorf("web_only message must have no buttons: %v", msg.Buttons)
	}

	fb.Reply(chat, msg.ID, owner, "同意")
	eventually(t, "web pointer", func() bool {
		s := fb.Sent()
		return len(s) == 2 && strings.Contains(s[1].Text, "不能在 Telegram 回答") && strings.Contains(s[1].Text, link)
	})
	if _, _, a := f.snapshot(); len(a) != 0 {
		t.Errorf("a web_only reply must not be forwarded as an answer: %+v", a)
	}
}

// 标了 web_only 的内容即使（被改动或出错而）带着选项和表单链接，ext 也只渲染 Web 链接。
func TestWebOnlyFlagWinsOverStrayOptions(t *testing.T) {
	f, fb := newFakeAPA(t), newBot(t)
	d := webOnlyDelivery()
	d.Content.Options, d.Content.Recommend, d.Content.FormURL = []string{"a: 同意", "b: 拒绝"}, "a", "/forms/tok"
	f.enqueue(d)
	start(t, f, fb, memStore())
	eventually(t, "message", func() bool { return len(fb.Sent()) == 1 })
	m := fb.Sent()[0]
	if len(m.Buttons) != 0 || strings.Contains(m.Text, "同意") || strings.Contains(m.Text, "/forms/") || strings.Contains(m.Text, "推荐") {
		t.Errorf("stray options leaked into a web_only message: %+v", m)
	}
}
