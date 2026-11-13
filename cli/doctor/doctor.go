// Package doctor 实现 `apa doctor [--json] [--upgrade-check]`（设计 §15.5、§16.5，附录 B.1）。
//
// 在 APA 主机上读数据目录，不需要内核在运行：账本先在线导出到临时目录的一份副本，再以只读方式查询，
// 因此服务运行与否都一样，数据目录里不新增、不改动任何文件。内核一侧的检查（运行形态、模型端点、来源、能力包、渠道与导出）
// 与 GET /doctor 共用 kernel/doctor；这里补上只能在本机做的：数据目录与版本、运行配置、宿主插件、重复提醒，
// 以及工具适配来源的工具探测。
package doctor

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	kpacks "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

func init() { client.Register("doctor", doctorCmd) }

var registryFn func() (*store.Registry, error)

// UseRegistry 登记取迁移注册表的函数；由 cli/serve 在 init 里登记（它是唯一装配内核的地方）。
func UseRegistry(f func() (*store.Registry, error)) { registryFn = f }

const usageText = "usage: apa doctor [--json] [--upgrade-check]"

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageText) }

func doctorCmd(c *client.Ctx, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var asJSON, upgrade bool
	fs.BoolVar(&asJSON, "json", false, "")
	fs.BoolVar(&upgrade, "upgrade-check", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for doctor")
	}
	if registryFn == nil {
		return usage("the ledger migrations are not wired into this binary")
	}
	reg, err := registryFn()
	if err != nil {
		return err
	}
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	in := Input{Home: home, Reg: reg, Model: &model.Client{}, Now: clock.Now()}
	// 升级预检不执行任何来源适配器、插件自检命令，也不探测模型端点：它只读文件与账本副本。
	var rep kdoctor.Report
	if upgrade {
		rep = UpgradeCheck(context.Background(), in)
	} else {
		rep = Run(context.Background(), in)
	}
	if asJSON {
		err = c.Print(rep)
	} else {
		_, err = fmt.Fprint(c.Env.Stdout, Render(rep))
	}
	if err != nil {
		return err
	}
	if !rep.OK {
		return client.ExitStatus(1)
	}
	return nil
}

// Input 是一次自检的外部事实。
type Input struct {
	Home  string
	Reg   *store.Registry
	Model *model.Client
	Now   time.Time
	// Env 非空时替换子进程环境（测试用）；为空继承当前进程。
	Env []string
}

// ledger 是账本副本上的读取结果。
type ledger struct {
	st      *store.Store
	info    store.Info
	infoErr error
	current bool // 结构版本等于本二进制的版本
	cleanup func()
}

// openLedger 在线导出账本副本并检视；副本在临时目录里，数据目录不受影响。账本不存在时 st 为空。
func openLedger(ctx context.Context, in Input) (*ledger, error) {
	l := &ledger{cleanup: func() {}}
	layout := config.LayoutOf(in.Home)
	if _, err := os.Stat(layout.Ledger); err != nil {
		return l, err
	}
	dir, err := os.MkdirTemp("", "apa-doctor-")
	if err != nil {
		return l, err
	}
	l.cleanup = func() { os.RemoveAll(dir) }
	snap := dir + "/ledger.db"
	if err := store.BackupFile(ctx, in.Home, snap); err != nil {
		return l, err
	}
	l.info, l.infoErr = store.Inspect(snap, in.Reg)
	if l.infoErr != nil {
		return l, nil
	}
	l.current = l.info.Ledger == in.Reg.SchemaLedger()
	if l.st, err = store.OpenSnapshot(snap); err != nil {
		return l, err
	}
	prev := l.cleanup
	l.cleanup = func() { l.st.Close(); prev() }
	return l, nil
}

// Run 是 `apa doctor`：九类检查，每类一项结论。
func Run(ctx context.Context, in Input) kdoctor.Report {
	layout := config.LayoutOf(in.Home)
	cfgCheck, _ := configCheck(layout)
	l, openErr := openLedger(ctx, in)
	defer l.cleanup()
	checks := []kdoctor.Check{cfgCheck, dataDirCheck(layout, in, l, openErr)}
	if l.st == nil || !l.current {
		why := "the ledger is not readable"
		if l.st != nil {
			why = "the ledger structure differs from this binary; an older one migrates when apa serve starts"
		}
		for _, n := range []string{kdoctor.Tier, kdoctor.ModelEndpoint, kdoctor.Sources, kdoctor.Packs, kdoctor.HostPlugin, kdoctor.Overlaps, kdoctor.Channels} {
			checks = append(checks, kdoctor.Check{Name: n, Status: kdoctor.Warning, Why: "skipped: " + why,
				Next: "fix the data directory check first; run apa doctor --upgrade-check before upgrading"})
		}
		return kdoctor.Assemble(checks...)
	}
	facts, err := kdoctor.ReadFacts(ctx, l.st)
	if err != nil {
		checks = append(checks, kdoctor.Check{Name: kdoctor.Tier, Status: kdoctor.Error, Why: "the ledger facts cannot be read: " + err.Error(), Next: "restore from a backup (apa backup restore)"})
		return kdoctor.Assemble(checks...)
	}
	ld := offlinePacks(layout)
	kernel := kdoctor.Kernel(ctx, facts, kdoctor.Env{Model: in.Model, Packs: ld, Now: func() time.Time { return clock.Now() }})
	for i := range kernel {
		if kernel[i].Name == kdoctor.Sources {
			probeTools(ctx, in, facts, &kernel[i])
			restoredSecrets(layout, &kernel[i])
		}
	}
	host, overlaps := hostChecks(ctx, in, facts)
	return kdoctor.Assemble(append(append(checks, kernel...), host, overlaps)...)
}

// offlinePacks 以与 apa serve 相同的方式读取内置包与数据目录包（只读文件）。
func offlinePacks(l config.Layout) *kpacks.Loader {
	ld := kpacks.New(kpacks.Options{Builtin: rootpacks.FS, DataDir: l.Packs, Kinds: kitems.NewKinds()})
	_, _ = ld.Load() // 拒绝记在加载器的 Health 里，由能力包检查呈现
	return ld
}

// Render 是默认的人读输出：每项一行结论，原因与下一步缩进其后。
func Render(r kdoctor.Report) string {
	var b strings.Builder
	tag := map[kdoctor.Status]string{kdoctor.OK: "ok   ", kdoctor.Warning: "WARN ", kdoctor.Error: "ERROR"}
	for _, c := range r.Checks {
		fmt.Fprintf(&b, "%s %s\n      why:  %s\n", tag[c.Status], c.Name, c.Why)
		if c.Next != "" {
			fmt.Fprintf(&b, "      next: %s\n", c.Next)
		}
		for _, d := range c.Details {
			fmt.Fprintf(&b, "      - %s [%s] %s\n", d.Subject, d.Status, d.Why)
		}
	}
	if r.OK {
		b.WriteString("no error-level findings\n")
	} else {
		b.WriteString("error-level findings present\n")
	}
	return b.String()
}
