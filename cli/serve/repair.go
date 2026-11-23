package serve

import (
	"context"
	"flag"
	"io"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func init() { client.Register("repair-merges", repairCmd) }

const repairUsage = "usage: apa repair-merges [--dry-run]"

// repairCmd 是 `apa repair-merges`（§9.3，2026-11-23 一次性修复）：把归类折叠掉的建议名下漏搬的消息搬到去处，
// 把仍是建议的名下挂着、没记进清单的消息补记进去（inbound.CmdRepair）。可重复执行，第二遍什么都不写。
// 离线跑：apa serve 占着监听端口时拒绝，先停服务；只登记修复命令，不起流水线、不拉来源、不调模型。
// --dry-run 在账本的在线副本上照做一遍、报告结果后删掉副本，服务开着也能跑，不动账本。
func repairCmd(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("repair-merges", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for repair-merges", repairUsage)
	}
	ctx := context.Background()
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	l := config.LayoutOf(home)
	if _, err := os.Stat(l.Ledger); err != nil {
		return usage("the APA home has not been initialized", "run `apa init` first")
	}
	reg, err := registry()
	if err != nil {
		return err
	}
	if pending, err := store.Pending(home, reg); err != nil {
		return tooNew(err)
	} else if pending { // 迁移只由 apa serve 做（留升级前快照）
		return domain.NewError(domain.CodeConflict, "the ledger has migrations this build has not applied", "start `apa serve` of this build once, stop it, then run again")
	}
	dir := home
	if *dry {
		if dir, err = os.MkdirTemp("", "apa-repair-merges-"); err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		if err := store.BackupFile(ctx, home, dir+"/"+store.LedgerFile); err != nil {
			return err
		}
	} else {
		cfg, err := config.Load(l.Config)
		if err != nil {
			return usage(err.Error(), "fix "+l.Config)
		}
		ln, err := core.Listen(cfg.Listen) // 占住 serve 的端口：服务开着时拒绝，修复期间服务也起不来
		if err != nil {
			return domain.NewError(domain.CodeConflict, "apa serve is running (listen address "+cfg.Listen+" is taken)", "stop the service first, or run with --dry-run")
		}
		defer ln.Close()
	}
	st, err := store.Open(dir, reg)
	if err != nil {
		return tooNew(err)
	}
	defer st.Close()
	x := command.New(st, nil)
	if err := inbound.RegisterRepair(x); err != nil {
		return err
	}
	done, err := inbound.Repair(ctx, x)
	if err != nil {
		return err
	}
	moved := 0
	for _, r := range done {
		moved += len(r.Events)
	}
	return c.Print(map[string]any{"ok": true, "dry_run": *dry, "items": len(done), "messages": moved, "repaired": done})
}
