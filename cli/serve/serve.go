package serve

import (
	"context"
	"errors"
	"flag"
	"io"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/background"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	khealth "github.com/xiaohei-info/awesome-personal-agent/kernel/health"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/followup"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/proactive/guideline"
	kprojects "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/resume"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	sregistry "github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/summaries"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/wake"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
)

// registry 登记内核当前装配的全部存储模块；init 与 serve 共用，保证建库与启动看到同一份迁移。
func registry() (*store.Registry, error) {
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, runidentity.Module,
		kprojects.Module, clock.Module, kitems.Module, core.Module, kcontext.Module, delegation.Module, executors.Module,
		delivery.Module, attention.Module, proc.Module, direct.Module, task.Module, followup.Module, rules.Module,
		instances.Module, sregistry.Module, runner.Module, webhook.Module, channel.Module,
		materials.Module, hosted.Module, search.Module, guideline.Module, agentsmd.Module, background.Module, chain.Module, khealth.Module, resume.Module, wake.Module, summaries.Module} {
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
	clock.SetHome(home) // 测试构建从这里读 test-clock-offset；发布构建无操作
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

	// 先占端口再动账本：第二个 serve 在这里就失败，不会把第一个正在跑的运行按崩溃恢复。
	ln, err := core.Listen(cfg.Listen)
	if err != nil {
		return domain.NewError(domain.CodeConflict, "cannot listen on "+cfg.Listen, "free the port or change listen in "+l.Config)
	}
	defer ln.Close()
	k, err := assemble(st, l, cfg, localURL(ln.Addr()))
	if err != nil {
		return err
	}
	if err := k.start(ctx); err != nil {
		return err
	}
	ready(ln.Addr())

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stopped := k.runLoops(ctx)
	err = k.srv.Serve(ctx, ln)
	cancel()
	stopped()
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
