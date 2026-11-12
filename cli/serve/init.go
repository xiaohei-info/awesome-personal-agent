// Package serve 实现主机本地的 `apa init` 与 `apa serve`（设计 §16.2、§16.4、§14.13，附录 B.1）。
// 两条命令是 CLI 里仅有的、必须在内核所在主机上直接装配内核的入口；其余命令都只经 HTTP 调内核。
package serve

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"os/exec"
	"strings"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/cli/backup"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func init() {
	client.Register("init", initCmd)
	client.Register("serve", serveCmd)
	backup.UseRegistry(registry) // `apa backup` 与启动时的升级前快照认同一份迁移链
}

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

// defaultConfig 是 apa init 写出的 config.yaml：把默认值显式落盘，本人改监听地址时有处可改。
func defaultConfig() string {
	d := config.Default()
	return "# APA 运行配置。listen 默认只绑本机回环；跨设备访问需改成非回环地址，并经 HTTPS 暴露（设计 §14.13）。\n" +
		"listen: " + d.Listen + "\n" +
		"# 监听非本机地址时必须再配置 https 的 public_url，否则 apa serve 拒绝启动：\n# public_url: https://apa.example.com\n"
}

// initCmd 建立数据目录、账本与 config.yaml，并设置 Web 登录口令。
// 口令取 --password-stdin 的第一行；缺省时在终端提示输入并确认。数据只写到 APA_HOME 之内。
func initCmd(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fromStdin := fs.Bool("password-stdin", false, "")
	reset := fs.Bool("reset-password", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for init", "usage: apa init [--reset-password] [--password-stdin]")
	}
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	l := config.LayoutOf(home)
	if *reset { // 只重设口令：没有账本就没有可重设的，不隐式建库
		if _, err := os.Stat(l.Ledger); err != nil {
			return usage("the APA home has not been initialized", "run `apa init` first")
		}
	}
	pw, err := readPassword(c, *fromStdin)
	if err != nil {
		return err
	}
	if utf8.RuneCountInString(pw) < core.MinPasswordLen { // 先于建目录与开库：被拒时不写入任何东西
		return usage("password is too short", "use at least 12 characters")
	}
	if err := l.Ensure(); err != nil {
		return err
	}
	if err := writeIfAbsent(l.Config, defaultConfig()); err != nil {
		return err
	}
	reg, err := registry()
	if err != nil {
		return err
	}
	st, err := store.Open(home, reg)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Update(context.Background(), func(tx *store.Tx) error {
		if err := core.SetOwnerPassword(tx, pw); err != nil {
			return err
		}
		if !*reset {
			return nil
		}
		// 重设是追加一条审计 Event，账本其余内容不动；载荷不含口令。
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvAuthPasswordChanged, Object: "owner", Reason: "login password reset on the host; sessions revoked",
			Payload: []byte(`{"via":"apa init --reset-password"}`), At: clock.Now(),
			Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaCLI}})
		return err
	}); err != nil {
		return err
	}
	return c.Print(map[string]any{"ok": true, "home": home})
}

// writeIfAbsent 只在文件不存在时写入（O_EXCL），重复 init 不覆盖本人改过的配置。
func writeIfAbsent(path, content string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, os.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func readPassword(c *client.Ctx, fromStdin bool) (string, error) {
	r := bufio.NewReader(c.Env.Stdin)
	line := func() (string, error) {
		s, err := r.ReadString('\n')
		if err != nil && !(errors.Is(err, io.EOF) && s != "") {
			return "", usage("no password given", "type a password (at least 12 characters), or pipe it to `apa init --password-stdin`")
		}
		return strings.TrimRight(s, "\r\n"), nil
	}
	if fromStdin {
		return line()
	}
	if f, ok := c.Env.Stdin.(*os.File); ok {
		if st, err := f.Stat(); err == nil && st.Mode()&os.ModeCharDevice != 0 {
			off := exec.Command("stty", "-echo")
			off.Stdin = f
			if off.Run() == nil { // 取不到 stty 时退回回显，不阻断 init
				defer func() { on := exec.Command("stty", "echo"); on.Stdin = f; _ = on.Run() }()
			}
		}
	}
	io.WriteString(c.Env.Stderr, "Web login password (at least 12 characters): ")
	a, err := line()
	if err != nil {
		return "", err
	}
	io.WriteString(c.Env.Stderr, "\nRepeat password: ")
	b, err := line()
	io.WriteString(c.Env.Stderr, "\n")
	if err != nil {
		return "", err
	}
	if a != b {
		return "", usage("the two passwords differ", "run `apa init` again")
	}
	return a, nil
}
