package telegram

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"

	"github.com/go-telegram/bot/models"

	"github.com/xiaohei-info/awesome-personal-agent/ext/telegram/fakebot"
)

// 环境变量：
//
//	APA_URL        内核地址（必填）
//	APA_EXT_KEY    本渠道的 ext 密钥，Web 签发（必填）
//	TG_BOT         Bot token（必填；与本人 harness 用的 Bot 分开，同一 Bot 不能有两个长轮询消费者）
//	TG_CHAT        本人的会话 id（必填，整数）
//	TG_API         Bot API 地址，缺省官方
//	APA_WEB_URL    相对 Web 链接的前缀，缺省 APA_URL
//	APA_EXT_STATE  发送记录文件，缺省 $XDG_STATE_HOME/apa/ext-telegram.json（再缺省 ~/.local/state）
//	APA_EXT_CHECK  1 时进入 `apa ext check` 自测模式：用本地 Bot API 替身和临时状态，不碰真实 Telegram 与真实状态

// 自测模式下替身里的"收件人"（与 `apa ext check` 的约定一致，见 cli/ext/check.go）：
// 标题为 checkAnswerTitle 的投递，收件人以 checkSender 选第一个选项。
const (
	checkAnswerTitle = "apa-ext-check:answer"
	checkSender      = "check-user"
)

// Main 读取环境并运行到 ctx 结束。st 为 nil 时用状态文件；测试可注入。
func Main(ctx context.Context, env func(string) string, stderr io.Writer, st Store) error {
	lg := log.New(stderr, "telegram-ext: ", log.LstdFlags)
	cfg := Config{APAURL: env("APA_URL"), APAKey: env("APA_EXT_KEY"), BotToken: env("TG_BOT"), BotAPI: env("TG_API"), WebBase: env("APA_WEB_URL")}

	if env("APA_EXT_CHECK") == "1" {
		fb, err := fakebot.New()
		if err != nil {
			return err
		}
		defer fb.Close()
		fb.OnSend = func(b *fakebot.Bot, s fakebot.Sent) {
			if len(s.Buttons) > 0 && len(s.Text) >= len(checkAnswerTitle) && s.Text[:len(checkAnswerTitle)] == checkAnswerTitle {
				b.Press(s.Chat, s.ID, models.User{ID: 1, Username: checkSender}, s.Buttons[0])
			}
		}
		cfg.BotToken, cfg.BotAPI, cfg.Chat, cfg.Wait = "1:check", fb.URL, 1, 1
		if st == nil {
			dir, err := os.MkdirTemp("", "apa-ext-check-")
			if err != nil {
				return err
			}
			defer os.RemoveAll(dir)
			if st, err = OpenFileStore(filepath.Join(dir, "state.json")); err != nil {
				return err
			}
		}
	} else {
		chat, err := strconv.ParseInt(env("TG_CHAT"), 10, 64)
		if err != nil {
			return fmt.Errorf("TG_CHAT must be your chat id (an integer)")
		}
		cfg.Chat = chat
		if st == nil {
			if st, err = OpenFileStore(statePath(env)); err != nil {
				return err
			}
		}
	}
	e, err := New(cfg, st, lg)
	if err != nil {
		return err
	}
	e.Run(ctx)
	return nil
}

func statePath(env func(string) string) string {
	if p := env("APA_EXT_STATE"); p != "" {
		return p
	}
	base := env("XDG_STATE_HOME")
	if base == "" {
		base = filepath.Join(env("HOME"), ".local", "state")
	}
	return filepath.Join(base, "apa", "ext-telegram.json")
}
