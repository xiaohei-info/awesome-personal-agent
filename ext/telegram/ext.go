// Package telegram 是 Telegram 渠道 ext（设计 §10.3、§5.7）：长轮询领取投递，以投递 id 为幂等键发到 Telegram，
// 回报送达，把本人在 Telegram 里的回答转达给内核。
//
// 它是 APA 自己的独立进程：只经 HTTP 接入内核，不直接写存储，不复用也不依赖任何 harness 的网关。
// Bot SDK 是 github.com/go-telegram/bot，换掉它的办法见 README.md。
package telegram

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-telegram/bot"
	"github.com/go-telegram/bot/models"
)

// Config 是 ext 的全部配置。外部系统凭据（Bot token）由 ext 自持，内核不读。
type Config struct {
	APAURL   string // 内核地址
	APAKey   string // 本渠道的 ext 密钥
	WebBase  string // 相对 Web 链接的前缀；空则用 APAURL
	BotToken string
	BotAPI   string // Bot API 地址；空为 Telegram 官方
	Chat     int64  // 本人的会话；只向它发送，只接受它的回复
	Wait     int    // 领取长轮询秒数，缺省 50
	// PollTimeout 是 Telegram getUpdates 的长轮询时长，缺省 SDK 值。
	PollTimeout time.Duration
}

// Ext 是一个运行中的渠道 ext。
type Ext struct {
	cfg Config
	apa *apa
	bot *bot.Bot
	st  Store
	log *log.Logger
	// 同一条投递的发送必须串行：领取循环是唯一发送者，这里只防御重入。
	sendMu sync.Mutex
}

// New 创建 ext；getMe 失败（token 无效、Bot API 不可达）直接返回错误。
func New(cfg Config, st Store, lg *log.Logger) (*Ext, error) {
	if cfg.APAURL == "" || cfg.APAKey == "" || cfg.BotToken == "" || cfg.Chat == 0 {
		return nil, fmt.Errorf("APA_URL, APA_EXT_KEY, TG_BOT and TG_CHAT are all required")
	}
	if cfg.Wait <= 0 {
		cfg.Wait = 50
	}
	if cfg.WebBase == "" {
		cfg.WebBase = cfg.APAURL
	}
	e := &Ext{cfg: cfg, apa: newAPA(cfg.APAURL, cfg.APAKey), st: st, log: lg}
	opts := []bot.Option{
		bot.WithDefaultHandler(e.onUpdate),
		bot.WithAllowedUpdates(bot.AllowedUpdates{"message", "callback_query"}),
		bot.WithErrorsHandler(func(err error) { lg.Printf("telegram: %v", err) }),
	}
	if cfg.BotAPI != "" {
		opts = append(opts, bot.WithServerURL(cfg.BotAPI))
	}
	if cfg.PollTimeout > 0 {
		opts = append(opts, bot.WithHTTPClient(cfg.PollTimeout, &http.Client{Timeout: cfg.PollTimeout + 5*time.Second}))
	}
	b, err := bot.New(cfg.BotToken, opts...)
	if err != nil {
		return nil, err
	}
	e.bot = b
	return e, nil
}

// Run 同时跑投递领取和 Telegram 长轮询，直到 ctx 结束。
func (e *Ext) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); e.claimLoop(ctx) }()
	e.bot.Start(ctx)
	wg.Wait()
}

func (e *Ext) claimLoop(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		d, err := e.apa.next(ctx, e.cfg.Wait)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			e.log.Printf("claim: %v", err)
			sleep(ctx, backoff)
			backoff = min(backoff*2, 30*time.Second)
			continue
		}
		backoff = time.Second
		if d != nil {
			e.deliver(ctx, d)
		}
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// deliver 发送一条投递并回报。幂等：已有发送记录就不再发，直接补报 ack（回执丢失、崩溃重启、租约到期重领都走这里）。
func (e *Ext) deliver(ctx context.Context, d *Delivery) {
	e.sendMu.Lock()
	defer e.sendMu.Unlock()
	rec, sent := e.st.Lookup(d.ID)
	if sent && rec.Stale {
		return
	}
	if !sent {
		var err error
		if rec, err = e.send(ctx, d); err != nil {
			e.log.Printf("send %s: %v", d.ID, err)
			if ferr := e.apa.fail(ctx, d.ID, d.Version, err.Error()); ferr != nil {
				e.log.Printf("fail %s: %v", d.ID, ferr)
			}
			return
		}
		// 消息已发出：存盘失败也不能报 fail（会让内核重发）。最坏是此刻崩溃后重发一次，见 README 的已知窗口。
		if err := e.st.Save(d.ID, rec); err != nil {
			e.log.Printf("save %s: %v (a crash before this delivery is acked may re-send it)", d.ID, err)
		}
	}
	if err := e.apa.ack(ctx, d.ID, d.Version, rec.Ref); err != nil {
		if ae := asAPIError(err); ae != nil && ae.IsConflict() {
			e.log.Printf("delivery %s is outdated, dropped", d.ID) // 版本已作废：不再报告
			rec.Stale = true
			if err := e.st.Save(d.ID, rec); err != nil {
				e.log.Printf("save %s: %v", d.ID, err)
			}
			return
		}
		e.log.Printf("ack %s: %v (will be re-claimed and acked with the same ref)", d.ID, err)
	}
}

func (e *Ext) webLink(u string) string {
	if strings.HasPrefix(u, "/") {
		return strings.TrimRight(e.cfg.WebBase, "/") + u
	}
	return u
}

// optionID 取 "<id>: <标签>" 的 id 部分。
func optionID(o string) string {
	id, _, _ := strings.Cut(o, ": ")
	return id
}

func (e *Ext) render(d *Delivery) string {
	c := d.Content
	var b strings.Builder
	b.WriteString(c.Title)
	if c.Summary != "" {
		b.WriteString("\n" + c.Summary)
	}
	if !c.WebOnly && len(c.Options) > 0 { // 有选项就可作答；是否接受由内核按投递类别判定
		b.WriteString("\n\n选项：")
		for _, o := range c.Options {
			b.WriteString("\n· " + o)
		}
		if c.Recommend != "" {
			b.WriteString("\n推荐：" + c.Recommend)
		}
	}
	if c.FormURL != "" && !c.WebOnly {
		b.WriteString("\n\n作答链接（一次性）：" + e.webLink(c.FormURL))
	}
	if c.WebURL != "" {
		b.WriteString("\n\n在 Web 查看：" + e.webLink(c.WebURL))
	}
	return b.String()
}

func (e *Ext) send(ctx context.Context, d *Delivery) (Record, error) {
	p := &bot.SendMessageParams{ChatID: e.cfg.Chat, Text: e.render(d)}
	var opts []string
	if !d.Content.WebOnly { // web_only 只给 Web 链接：没有选项，没有按钮
		opts = d.Content.Options
	}
	var rows [][]models.InlineKeyboardButton
	for i, o := range opts {
		rows = append(rows, []models.InlineKeyboardButton{{Text: o, CallbackData: strconv.Itoa(i)}})
	}
	if len(rows) > 0 {
		p.ReplyMarkup = &models.InlineKeyboardMarkup{InlineKeyboard: rows}
	}
	m, err := e.bot.SendMessage(ctx, p)
	if err != nil {
		return Record{}, err
	}
	return Record{Ref: fmt.Sprintf("%d:%d", e.cfg.Chat, m.ID), Chat: e.cfg.Chat, Msg: m.ID, Options: opts, WebOnly: d.Content.WebOnly, WebURL: d.Content.WebURL, At: time.Now()}, nil
}

// ---- 回答回流 ----

func (e *Ext) onUpdate(ctx context.Context, _ *bot.Bot, u *models.Update) {
	switch {
	case u.CallbackQuery != nil:
		e.onButton(ctx, u.CallbackQuery)
	case u.Message != nil:
		e.onReply(ctx, u.Message)
	}
}

func sender(u models.User) string {
	if u.Username != "" {
		return u.Username
	}
	return strconv.FormatInt(u.ID, 10)
}

func (e *Ext) onButton(ctx context.Context, q *models.CallbackQuery) {
	var chat int64
	var msgID int
	switch { // 太旧的消息 Telegram 只给 id，不给内容；回答只需要 id
	case q.Message.Message != nil:
		chat, msgID = q.Message.Message.Chat.ID, q.Message.Message.ID
	case q.Message.InaccessibleMessage != nil:
		chat, msgID = q.Message.InaccessibleMessage.Chat.ID, q.Message.InaccessibleMessage.MessageID
	}
	if chat != e.cfg.Chat { // 只接受本人会话里的操作
		return
	}
	note := e.reply(ctx, chat, msgID, q.From, func(rec Record) (string, string, bool) {
		i, err := strconv.Atoi(q.Data)
		if err != nil || i < 0 || i >= len(rec.Options) {
			return "", "", false
		}
		return optionID(rec.Options[i]), "", true
	})
	if _, err := e.bot.AnswerCallbackQuery(ctx, &bot.AnswerCallbackQueryParams{CallbackQueryID: q.ID, Text: firstLine(note)}); err != nil {
		e.log.Printf("answerCallbackQuery: %v", err)
	}
	if note != "" && !strings.HasPrefix(note, okNote) {
		e.say(ctx, note)
	}
}

func (e *Ext) onReply(ctx context.Context, m *models.Message) {
	if m.Chat.ID != e.cfg.Chat || m.From == nil || m.Text == "" {
		return
	}
	if m.ReplyToMessage == nil {
		e.say(ctx, "请直接回复要回答的那条消息，或点它下面的选项按钮。")
		return
	}
	note := e.reply(ctx, m.Chat.ID, m.ReplyToMessage.ID, *m.From, func(rec Record) (string, string, bool) {
		text := strings.TrimSpace(m.Text)
		for _, o := range rec.Options {
			if strings.EqualFold(text, optionID(o)) {
				return optionID(o), "", true
			}
		}
		return "", text, true // 不是选项：作为文字回答交给内核判定
	})
	e.say(ctx, note)
}

const okNote = "已记录"

// reply 按消息找到投递并转达回答，返回要告诉本人的话。内核的拒绝（web_only、白名单、已回答等）原样告知并带 Web 链接。
func (e *Ext) reply(ctx context.Context, chat int64, msg int, from models.User,
	pick func(Record) (choice, text string, ok bool)) string {
	id, rec, found := e.st.Find(chat, msg)
	if !found {
		return "找不到这条消息对应的投递。"
	}
	if rec.WebOnly { // 内核已声明这是 web_only：不转达，直接指向 Web
		note := "这是需要在 Web 确认的决策，不能在 Telegram 回答。"
		if rec.WebURL != "" {
			note += "\n请在 Web 处理：" + e.webLink(rec.WebURL)
		}
		return note
	}
	choice, text, ok := pick(rec)
	if !ok {
		return "这个选项已失效。"
	}
	err := e.apa.answer(ctx, id, rec.Ref, sender(from), choice, text)
	if err == nil {
		return okNote + "，事项会继续。"
	}
	ae := asAPIError(err)
	if ae == nil {
		e.log.Printf("answer %s: %v", id, err)
		return "暂时无法转达回答，请稍后重试。"
	}
	note := "回答未被接受：" + ae.Why
	if ae.IsConflict() {
		note = "这条已经有结果了：" + ae.Why
	}
	if ae.WebURL != nil && *ae.WebURL != "" {
		note += "\n请在 Web 处理：" + e.webLink(*ae.WebURL)
	}
	return note
}

func (e *Ext) say(ctx context.Context, text string) {
	if text == "" {
		return
	}
	if _, err := e.bot.SendMessage(ctx, &bot.SendMessageParams{ChatID: e.cfg.Chat, Text: text}); err != nil {
		e.log.Printf("sendMessage: %v", err)
	}
}

func firstLine(s string) string {
	l, _, _ := strings.Cut(s, "\n")
	return l
}
