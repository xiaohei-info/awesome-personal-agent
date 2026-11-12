package serve

import (
	"github.com/xiaohei-info/awesome-personal-agent/api/version"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
)

func init() { client.Register("version", versionCmd) }

// schemaLedger 是 registry() 登记的全部迁移里最高的账本结构版本，即发布物的 `schema.ledger`。
func schemaLedger() (int, error) {
	reg, err := registry()
	if err != nil {
		return 0, err
	}
	return reg.SchemaLedger(), nil
}

// versionCmd 是 `apa version`（附录 B.5）：读本二进制自身，不连内核、不需要凭据；内容与 GET /version 同源。
func versionCmd(c *client.Ctx, args []string) error {
	if len(args) != 0 {
		return usage("apa version takes no arguments", "usage: apa version")
	}
	n, err := schemaLedger()
	if err != nil {
		return err
	}
	return c.Print(version.Current(n))
}
