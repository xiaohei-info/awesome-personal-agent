// Package ext 是 apa 的 ext 命令组（附录 B.1，设计 §10.3）：ext list、ext run、ext check。
//
// ext 是独立进程，只经 HTTP 协议与内核交互，不嵌入内核，也不依赖任何 harness。本包只做本机的发现、启动和协议自测：
// 不含业务逻辑，不直接访问存储，也不读 ext 密钥（密钥由 ext 自己读取）。
//
// ext 目录：$APA_EXT_DIR，缺省为当前目录下的 ext/。每个 ext 是其下一个子目录，带 ext.yaml：
//
//	kind: channel          # channel | export | webhook
//	description: 一句话说明
//	command: [./run.sh]    # 在该目录下执行；run 原样传递当前进程环境（含 APA_URL 与 ext 自己的密钥）
package ext

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("ext", cmd) }

// EnvDir 指定 ext 目录。
const EnvDir = "APA_EXT_DIR"

const usageHint = "usage: apa ext list | run <name> | check [<name>...] [--timeout <seconds>]"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

// Kind 是 ext 的种类（§10.3）。
type Kind string

const (
	KindChannel Kind = "channel"
	KindExport  Kind = "export"
	KindWebhook Kind = "webhook"
)

// Info 是 ext list 的一项。
type Info struct {
	Name        string   `json:"name"`
	Kind        Kind     `json:"kind"`
	Description string   `json:"description,omitempty"`
	Command     []string `json:"command"`
	dir         string
}

type manifest struct {
	Kind        Kind     `yaml:"kind"`
	Description string   `yaml:"description"`
	Command     []string `yaml:"command"`
}

func cmd(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no ext subcommand given")
	}
	root := c.Env.Getenv(EnvDir)
	if root == "" {
		root = "ext"
	}
	switch args[0] {
	case "list":
		return list(c, root, args[1:])
	case "run":
		return run(c, root, args[1:])
	case "check":
		return check(c, root, args[1:])
	}
	return usage("unknown ext subcommand " + args[0])
}

// discover 读取 root 下所有带 ext.yaml 的子目录；目录不存在等于没有 ext。无法解析的清单直接报错，不静默跳过。
func discover(root string) ([]Info, error) {
	ents, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, e := range ents {
		dir := filepath.Join(root, e.Name())
		data, err := os.ReadFile(filepath.Join(dir, "ext.yaml"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var m manifest
		if err := yaml.Unmarshal(data, &m); err != nil {
			return nil, domain.NewError(domain.CodeInvalidRequest, "ext "+e.Name()+": ext.yaml is not valid YAML", "fix "+filepath.ToSlash(filepath.Join(dir, "ext.yaml")))
		}
		switch m.Kind {
		case KindChannel, KindExport, KindWebhook:
		default:
			return nil, domain.NewError(domain.CodeInvalidRequest, "ext "+e.Name()+": kind must be channel, export or webhook", "fix ext.yaml")
		}
		if len(m.Command) == 0 || m.Command[0] == "" {
			return nil, domain.NewError(domain.CodeInvalidRequest, "ext "+e.Name()+": command is required", "fix ext.yaml")
		}
		out = append(out, Info{Name: e.Name(), Kind: m.Kind, Description: m.Description, Command: m.Command, dir: dir})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func find(root, name string) (Info, error) {
	if name == "" || name != filepath.Base(name) || strings.HasPrefix(name, ".") {
		return Info{}, usage("invalid ext name")
	}
	all, err := discover(root)
	if err != nil {
		return Info{}, err
	}
	for _, x := range all {
		if x.Name == name {
			return x, nil
		}
	}
	return Info{}, domain.NewError(domain.CodeNotFound, "ext "+name+" not found", "run `apa ext list`")
}

func list(c *client.Ctx, root string, args []string) error {
	if len(args) != 0 {
		return usage("apa ext list takes no arguments")
	}
	all, err := discover(root)
	if err != nil {
		return err
	}
	if all == nil {
		all = []Info{}
	}
	if !c.Pretty {
		return c.Print(map[string]any{"exts": all})
	}
	if len(all) == 0 {
		_, err = fmt.Fprintln(c.Env.Stdout, "no exts")
		return err
	}
	for _, x := range all {
		fmt.Fprintf(c.Env.Stdout, "%-16s %-8s %s\n", x.Name, x.Kind, x.Description)
	}
	return nil
}

// run 在 ext 目录下前台启动 ext，直到它退出或收到 SIGINT/SIGTERM（转给 ext）。ext 非零退出按本地失败返回。
func run(c *client.Ctx, root string, args []string) error {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") {
		return usage("apa ext run takes exactly one ext name")
	}
	x, err := find(root, args[0])
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	p := exec.CommandContext(ctx, x.Command[0], x.Command[1:]...)
	p.Dir = x.dir
	p.Stdin, p.Stdout, p.Stderr = c.Env.Stdin, c.Env.Stdout, c.Env.Stderr
	p.Cancel = func() error { return p.Process.Signal(syscall.SIGTERM) }
	p.WaitDelay = 5 * time.Second
	if err := p.Run(); err != nil {
		if ctx.Err() != nil { // 被中断，ext 已收到 SIGTERM
			return nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return domain.NewError(domain.CodeInternal, fmt.Sprintf("ext %s exited with status %d", x.Name, ee.ExitCode()), "check the ext's own output")
		}
		return domain.NewError(domain.CodeInternal, "ext "+x.Name+" could not be started", "check command in ext.yaml")
	}
	return nil
}

func check(c *client.Ctx, root string, args []string) error {
	fs := flag.NewFlagSet("ext check", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	timeout := fs.Int("timeout", 20, "")
	// flag 包遇到第一个非 flag 参数即停；把名字与 flag 分开再解析，允许任意顺序。
	var names, flags []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "-") {
			flags = append(flags, args[i])
			if !strings.Contains(args[i], "=") && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
		} else {
			names = append(names, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil || *timeout < 1 {
		return usage("invalid arguments")
	}
	var targets []Info
	if len(names) == 0 {
		all, err := discover(root)
		if err != nil {
			return err
		}
		for _, x := range all {
			if x.Kind == KindChannel {
				targets = append(targets, x)
			}
		}
		if len(targets) == 0 {
			return usage("no channel ext found to check")
		}
	}
	for _, n := range names {
		x, err := find(root, n)
		if err != nil {
			return err
		}
		if x.Kind != KindChannel {
			return usage("ext " + n + " is not a channel ext; only channel exts use the delivery protocol")
		}
		targets = append(targets, x)
	}
	var reports []Report
	failed := false
	for _, x := range targets {
		r := Check(context.Background(), x, time.Duration(*timeout)*time.Second, c.Env.Stderr)
		failed = failed || !r.OK
		reports = append(reports, r)
	}
	if err := c.Print(map[string]any{"ok": !failed, "reports": reports}); err != nil {
		return err
	}
	if failed {
		return domain.NewError(domain.CodeInternal, "ext protocol check failed", "see the failed checks in the report")
	}
	return nil
}
