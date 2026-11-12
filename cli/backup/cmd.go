package backup

import (
	"context"
	"flag"
	"io"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func init() { client.Register("backup", backupCmd) }

// registryFn 供出内核当前装配的全部迁移；由 cli/serve 在 init 里登记（它是唯一装配内核的地方），避免本包反向依赖 serve。
var registryFn func() (*store.Registry, error)

// UseRegistry 登记取迁移注册表的函数。
func UseRegistry(f func() (*store.Registry, error)) { registryFn = f }

func registry() (*store.Registry, error) {
	if registryFn == nil {
		return nil, usage("the ledger migrations are not wired into this binary", "use the apa binary")
	}
	return registryFn()
}

const usageText = "usage: apa backup create [--out <dir>] [--no-secrets] | verify <backup> | restore <backup> --home <dir>"

// parse 解析 flag 与位置参数，二者可任意交错（`restore <备份> --home <目录>`）。
func parse(name string, args []string, define func(*flag.FlagSet)) ([]string, error) {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	define(fs)
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usage("invalid arguments for backup "+name, usageText)
		}
		if fs.NArg() == 0 {
			return pos, nil
		}
		pos = append(pos, fs.Arg(0))
		args = fs.Args()[1:]
	}
}

func backupCmd(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no backup subcommand given", usageText)
	}
	sub, rest := args[0], args[1:]
	reg, err := registry()
	if err != nil {
		return err
	}
	switch sub {
	case "create":
		var out string
		var noSecrets bool
		pos, err := parse(sub, rest, func(fs *flag.FlagSet) {
			fs.StringVar(&out, "out", "", "")
			fs.BoolVar(&noSecrets, "no-secrets", false, "")
		})
		if err != nil || len(pos) != 0 {
			return usage("invalid arguments for backup create", usageText)
		}
		home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
		if err != nil {
			return usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
		}
		m, err := Create(context.Background(), home, Options{Out: out, NoSecrets: noSecrets}, reg, clock.Now())
		if err != nil {
			return err
		}
		return c.Print(map[string]any{"ok": true, "backup": m.Dir, "no_secrets": m.NoSecrets, "schema_ledger": m.Schema.Ledger,
			"event_max_seq": m.EventMaxSeq, "files": len(m.Files)})
	case "verify":
		pos, err := parse(sub, rest, func(*flag.FlagSet) {})
		if err != nil || len(pos) != 1 {
			return usage("invalid arguments for backup verify", usageText)
		}
		rep, err := Verify(pos[0], reg)
		if err != nil {
			return err
		}
		return c.Print(map[string]any{"ok": true, "files": rep.Files, "hashes": "match", "integrity": "ok", "events": "continuous",
			"event_max_seq": rep.EventMaxSeq, "schema_ledger": rep.Manifest.Schema.Ledger, "no_secrets": rep.Manifest.NoSecrets})
	case "restore":
		var home string
		pos, err := parse(sub, rest, func(fs *flag.FlagSet) { fs.StringVar(&home, "home", "", "") })
		if err != nil || len(pos) != 1 || home == "" {
			return usage("invalid arguments for backup restore", usageText)
		}
		res, err := Restore(pos[0], home, reg)
		if err != nil {
			return err
		}
		return c.Print(map[string]any{"ok": true, "home": res.Home, "schema_ledger": res.SchemaLedger, "no_secrets": res.NoSecrets,
			"notices": res.Notices, "next": "point `apa serve` at the restored directory with APA_HOME"})
	}
	return usage("unknown backup subcommand", usageText)
}
