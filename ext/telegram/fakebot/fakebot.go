// Package fakebot 是本地的 Telegram Bot API 替身：只实现渠道 ext 用到的方法，供节点测试和 `apa ext check` 的
// 自测模式使用，不连任何真实平台。
package fakebot

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-telegram/bot/models"
)

// Sent 是 ext 发出的一条消息。
type Sent struct {
	ID      int
	Chat    int64
	Text    string
	Buttons []string // inline 按钮的 callback_data，按行展平
}

// Bot 是一个监听本机回环地址的 Bot API 替身。
type Bot struct {
	srv *http.Server
	URL string

	mu      sync.Mutex
	sent    []Sent
	updates []models.Update
	nextMsg int
	nextUpd int64
	// OnSend 在每条消息发出后同步调用（锁外）；自测模式用它让"收件人"作答。
	OnSend func(*Bot, Sent)
	// SendErr 非空时 sendMessage 返回该描述的 Bot API 错误，用来模拟发送失败。
	SendErr string
}

// New 启动替身。
func New() (*Bot, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	b := &Bot{URL: "http://" + ln.Addr().String(), nextMsg: 100}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /{bot}/{method}", b.handle)
	b.srv = &http.Server{Handler: mux}
	go b.srv.Serve(ln)
	return b, nil
}

func (b *Bot) Close() { b.srv.Close() }

// Sent 返回目前发出的全部消息。
func (b *Bot) Sent() []Sent {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Sent(nil), b.sent...)
}

func (b *Bot) push(u models.Update) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextUpd++
	u.ID = b.nextUpd
	b.updates = append(b.updates, u)
}

// Press 模拟收件人点 chat 里消息 msg 的 inline 按钮。
func (b *Bot) Press(chat int64, msg int, from models.User, data string) {
	b.push(models.Update{CallbackQuery: &models.CallbackQuery{ID: "cb" + strconv.Itoa(msg) + data, From: from, Data: data,
		Message: models.MaybeInaccessibleMessage{Type: models.MaybeInaccessibleMessageTypeMessage,
			Message: &models.Message{ID: msg, Date: 1, Chat: models.Chat{ID: chat}}}}})
}

// Reply 模拟收件人回复消息 to。
func (b *Bot) Reply(chat int64, to int, from models.User, text string) {
	b.push(models.Update{Message: &models.Message{ID: b.id(), Chat: models.Chat{ID: chat}, From: &from, Text: text,
		ReplyToMessage: &models.Message{ID: to, Chat: models.Chat{ID: chat}}}})
}

func (b *Bot) id() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.nextMsg++
	return b.nextMsg
}

func reply(w http.ResponseWriter, result any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "result": result})
}

func (b *Bot) handle(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(1 << 20); err != nil && r.ContentLength > 0 {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	switch r.PathValue("method") {
	case "getMe":
		reply(w, models.User{ID: 1, IsBot: true, Username: "fake_bot"})
	case "getUpdates":
		b.getUpdates(w, r)
	case "sendMessage":
		b.sendMessage(w, r)
	default: // answerCallbackQuery 等：成功即可
		reply(w, true)
	}
}

func (b *Bot) getUpdates(w http.ResponseWriter, r *http.Request) {
	offset, _ := strconv.ParseInt(r.FormValue("offset"), 10, 64)
	deadline := time.Now().Add(300 * time.Millisecond) // 够短，ext 退出时不用久等
	for {
		b.mu.Lock()
		var out []models.Update
		for _, u := range b.updates {
			if u.ID >= offset {
				out = append(out, u)
			}
		}
		b.mu.Unlock()
		if len(out) > 0 || time.Now().After(deadline) {
			if out == nil {
				out = []models.Update{}
			}
			reply(w, out)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func (b *Bot) sendMessage(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	failMsg := b.SendErr
	b.mu.Unlock()
	if failMsg != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": false, "error_code": 400, "description": failMsg})
		return
	}
	chat, _ := strconv.ParseInt(r.FormValue("chat_id"), 10, 64)
	s := Sent{Chat: chat, Text: r.FormValue("text")}
	if rm := r.FormValue("reply_markup"); rm != "" {
		var kb models.InlineKeyboardMarkup
		if err := json.Unmarshal([]byte(rm), &kb); err != nil {
			http.Error(w, fmt.Sprint("bad reply_markup: ", err), http.StatusBadRequest)
			return
		}
		for _, row := range kb.InlineKeyboard {
			for _, btn := range row {
				s.Buttons = append(s.Buttons, btn.CallbackData)
			}
		}
	}
	s.ID = b.id()
	b.mu.Lock()
	b.sent = append(b.sent, s)
	hook := b.OnSend
	b.mu.Unlock()
	reply(w, models.Message{ID: s.ID, Chat: models.Chat{ID: chat}, Text: s.Text})
	if hook != nil {
		go hook(b, s)
	}
}
