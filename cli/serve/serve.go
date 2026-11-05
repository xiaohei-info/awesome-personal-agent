package serve

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apidecisions "github.com/xiaohei-info/awesome-personal-agent/api/decisions"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	apiprojects "github.com/xiaohei-info/awesome-personal-agent/api/projects"
	"github.com/xiaohei-info/awesome-personal-agent/api/settings/credentials"
	apipolicy "github.com/xiaohei-info/awesome-personal-agent/api/settings/policy"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	kdecisions "github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	kprojects "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const clockInterval = time.Second

// registry 登记内核当前装配的全部存储模块；init 与 serve 共用，保证建库与启动看到同一份迁移。
func registry() (*store.Registry, error) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module,
		kprojects.Module, clock.Module, kitems.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			return nil, err
		}
	}
	return reg, nil
}

func serveCmd(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for serve", "usage: apa serve")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, c, func(a net.Addr) {
		_ = c.Print(map[string]any{"ok": true, "listen": a.String()})
	})
}

// run 装配内核并服务，直到 ctx 取消。ready 在监听建立后、开始服务前调用一次。
// 监听地址只取自 config.yaml（默认回环）；未 `apa init` 过的目录拒绝启动，不隐式建库。
func run(ctx context.Context, c *client.Ctx, ready func(net.Addr)) error {
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	l := config.LayoutOf(home)
	if _, err := os.Stat(l.Ledger); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return usage("the APA home has not been initialized", "run `apa init` first")
		}
		return err
	}
	cfg, err := config.Load(l.Config)
	if err != nil {
		return usage(err.Error(), "fix "+l.Config)
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
	set, err := ownerPasswordSet(ctx, st)
	if err != nil {
		return err
	}
	if !set {
		return usage("no Web login password is set", "run `apa init` first")
	}

	srv, clk, err := assemble(st)
	if err != nil {
		return err
	}
	ln, err := core.Listen(cfg.Listen)
	if err != nil {
		return domain.NewError(domain.CodeConflict, "cannot listen on "+cfg.Listen, "free the port or change listen in "+l.Config)
	}
	ready(ln.Addr())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	clockDone := make(chan struct{})
	go func() {
		defer close(clockDone)
		_ = clk.Run(ctx, clockInterval, func(err error) { slog.Error("clock tick failed", "err", err) })
	}()
	err = srv.Serve(ctx, ln)
	cancel()
	<-clockDone
	return err
}

func ownerPasswordSet(ctx context.Context, st *store.Store) (set bool, err error) {
	err = st.View(ctx, func(tx *store.Tx) error {
		r, err := tx.Query("SELECT 1 FROM apicore_owner WHERE id = 1")
		if err != nil {
			return err
		}
		defer r.Close()
		set = r.Next()
		return r.Err()
	})
	return
}

// assemble 登记内核命令并挂载 HTTP 路由分组与时钟。
func assemble(st *store.Store) (*core.Server, *clock.Clock, error) {
	x := command.New(st, nil)
	if err := kitems.Register(x, kitems.NewKinds()); err != nil {
		return nil, nil, err
	}
	if err := kdecisions.Register(x); err != nil {
		return nil, nil, err
	}
	if err := kprojects.Register(x); err != nil {
		return nil, nil, err
	}
	a := auth.New(nil)
	creds, err := credentials.Mount(x, a)
	if err != nil {
		return nil, nil, err
	}
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{
		apiitems.Routes(x, st), apidecisions.Routes(x, ""), apiprojects.Routes(st, x), apipolicy.Routes(st), creds}})
	if err != nil {
		return nil, nil, err
	}
	clk, err := clock.New(st, func(tx *store.Tx, c clock.Command) error {
		if kdecisions.Owns(c) {
			return kdecisions.HandleTimer(tx, c)
		}
		return kitems.HandleTimer(tx, c)
	}, nil)
	return srv, clk, err
}
