package backup

import (
	"context"
	"errors"
	"io/fs"
	"net"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// `apa secrets rekey` 与备份同属数据目录的维护：在主机上、服务停止时运行（§5.5），所以放在这里，共用迁移注册表。
func init() { client.Register("secrets", secretsUsage, secretsCmd) }

const secretsUsage = "usage: apa secrets rekey"

func secretsCmd(c *client.Ctx, args []string) error {
	if len(args) != 1 || args[0] != "rekey" {
		return usage("invalid arguments for secrets", secretsUsage)
	}
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	l := config.LayoutOf(home)
	cfg, err := config.Load(l.Config)
	if err != nil {
		return usage(err.Error(), "fix "+l.Config)
	}
	if cn, err := net.DialTimeout("tcp", cfg.Listen, time.Second); err == nil { // 换钥期间服务不能在读写凭据
		cn.Close()
		return conflict("apa serve is still listening on "+cfg.Listen, "stop the service, run apa secrets rekey, then start it again")
	}
	reg, err := registry()
	if err != nil {
		return err
	}
	if _, err := secrets.Load(home); errors.Is(err, fs.ErrNotExist) {
		return usage("there is no master key in "+home, "start apa serve once to generate it")
	}
	info, err := store.Inspect(l.Ledger, reg)
	if err != nil {
		return err
	}
	if reg.Behind(info) { // 迁移只由 apa serve 做（先留升级前快照）
		return usage("the ledger has not been migrated to this binary yet", "start apa serve once, stop it, then run apa secrets rekey")
	}
	st, err := store.Open(home, reg)
	if err != nil {
		return err
	}
	defer st.Close()
	old, _, err := secrets.Open(context.Background(), st, home) // 也走完上次中断的换钥
	if err != nil {
		return err
	}
	n, nv, err := secrets.Rekey(context.Background(), st, old)
	if err != nil {
		return err
	}
	return c.Print(map[string]any{"ok": true, "reencrypted": n, "key_id": nv.ID()})
}
