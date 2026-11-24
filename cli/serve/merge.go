package serve

import (
	"context"
	"flag"
	"io"
	"os"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	kpacks "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

func init() { client.Register("merge", mergeCmd) }

const mergeUsage = "usage: apa merge run [--dry-run|--apply] [--source <id|prefix*>] [--project <id|->] [--since <time>] [--until <time>] [--limit <n>]" +
	" | mark-pending [--dry-run|--apply] [--by <name>] [--source <id|prefix*>] [--project <id|->] [--since <time>] [--until <time>] [--limit <n>]"

// hostCLI 是主机终端上的本人：同 apa serve 读主机配置文件的 hostOwner，与 Web 上的本人同级（§14.12）。
var hostCLI = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: owner, Via: domain.ViaWeb}, Level: authz.LevelWeb}

// mergeCmd 是 `apa merge`（§9.3）：
//   - run：按维护循环同一套规则跑一批待归类的建议（含本人补打的）；不等攒批条件，可按来源、项目、时间窗、条数筛。
//   - mark-pending：给还没有归类标记的建议补记待归类（标 backfill，维护循环不自动归类它们，等 run --apply）。
//
// 缺省与 --dry-run 一样只演练：在账本的在线副本上算规则层，不调模型、不写账本，服务开着也能跑；
// --apply 才调写作模型（run）、经内核命令落账；离线跑，apa serve 占着监听端口时拒绝。
func mergeCmd(c *client.Ctx, args []string) error {
	if len(args) == 0 || (args[0] != "run" && args[0] != "mark-pending") {
		return usage("apa merge needs run or mark-pending", mergeUsage)
	}
	sub := args[0]
	fs := flag.NewFlagSet("merge "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	dry := fs.Bool("dry-run", false, "")
	apply := fs.Bool("apply", false, "")
	by := fs.String("by", "", "")
	var f inbound.ManualFilter
	fs.StringVar(&f.Source, "source", "", "")
	fs.StringVar(&f.Project, "project", "", "")
	since, until := fs.String("since", "", ""), fs.String("until", "", "")
	fs.IntVar(&f.Limit, "limit", 0, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || (*dry && *apply) || f.Limit < 0 || (*by != "" && sub != "mark-pending") {
		return usage("invalid arguments for apa merge "+sub, mergeUsage)
	}
	var err error
	if f.Since, err = parseWhen(*since); err != nil {
		return usage("--since must be RFC 3339 or YYYY-MM-DD", mergeUsage)
	}
	if f.Until, err = parseWhen(*until); err != nil {
		return usage("--until must be RFC 3339 or YYYY-MM-DD", mergeUsage)
	}
	if *by == "" {
		if *by = c.Env.Getenv("USER"); *by == "" {
			*by = owner
		}
	}
	ctx := context.Background()
	st, l, done, err := openOffline(ctx, c, !*apply)
	if err != nil {
		return err
	}
	defer done()
	x := command.New(st, nil)
	ld := kpacks.New(kpacks.Options{Builtin: rootpacks.FS, DataDir: l.Packs, Kinds: kitems.NewKinds()})
	if _, err := ld.Load(); err != nil {
		return err
	}
	judge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}})
	if err != nil {
		return err
	}
	in, err := inbound.New(inbound.Options{Store: st, Exec: x, Packs: ld, Judge: judge, Owner: owner})
	if err != nil {
		return err
	}
	if err := inbound.RegisterManual(x); err != nil {
		return err
	}
	if sub == "mark-pending" {
		rep, err := in.MarkPending(ctx, hostCLI, *by, f, *apply)
		if err != nil {
			return err
		}
		return c.Print(rep)
	}
	var rep inbound.ManualReport
	if *apply {
		rep, err = in.RunManual(ctx, hostCLI, f)
	} else {
		rep, err = in.PlanManual(ctx, f)
	}
	if err != nil {
		return err
	}
	return c.Print(rep)
}

func parseWhen(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	if t, err := time.ParseInLocation("2006-01-02", s, time.Local); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

// openOffline 打开账本：dry 时在在线副本上（服务开着也行，不动账本），否则占住 serve 的监听端口后打开真账本（同 apa repair-merges）。
func openOffline(ctx context.Context, c *client.Ctx, dry bool) (*store.Store, config.Layout, func(), error) {
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return nil, config.Layout{}, nil, usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	l := config.LayoutOf(home)
	if _, err := os.Stat(l.Ledger); err != nil {
		return nil, l, nil, usage("the APA home has not been initialized", "run `apa init` first")
	}
	reg, err := registry()
	if err != nil {
		return nil, l, nil, err
	}
	if pending, err := store.Pending(home, reg); err != nil {
		return nil, l, nil, tooNew(err)
	} else if pending { // 迁移只由 apa serve 做（留升级前快照）
		return nil, l, nil, domain.NewError(domain.CodeConflict, "the ledger has migrations this build has not applied", "start `apa serve` of this build once, stop it, then run again")
	}
	var closers []func()
	done := func() {
		for i := len(closers) - 1; i >= 0; i-- {
			closers[i]()
		}
	}
	dir := home
	if dry {
		if dir, err = os.MkdirTemp("", "apa-merge-dry-"); err != nil {
			return nil, l, nil, err
		}
		closers = append(closers, func() { os.RemoveAll(dir) })
		if err := store.BackupFile(ctx, home, dir+"/"+store.LedgerFile); err != nil {
			done()
			return nil, l, nil, err
		}
	} else {
		cfg, err := config.Load(l.Config)
		if err != nil {
			return nil, l, nil, usage(err.Error(), "fix "+l.Config)
		}
		ln, err := core.Listen(cfg.Listen) // 占住 serve 的端口：服务开着时拒绝，执行期间服务也起不来
		if err != nil {
			return nil, l, nil, domain.NewError(domain.CodeConflict, "apa serve is running (listen address "+cfg.Listen+" is taken)", "stop the service first, or run without --apply")
		}
		closers = append(closers, func() { ln.Close() })
	}
	st, err := store.Open(dir, reg)
	if err != nil {
		done()
		return nil, l, nil, tooNew(err)
	}
	closers = append(closers, func() { st.Close() })
	return st, l, done, nil
}
