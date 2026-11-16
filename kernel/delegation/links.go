package delegation

import (
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// vaultLinkRule 是回执库内链接自动登记的内置规则 id，记在事件的 Basis 里（§7.5）。
const vaultLinkRule = "delegate.report.vault_links"

// vaultActor 是自动登记的操作者：system。本人可在 Web 移除这条关系，材料保留。
var vaultActor = domain.Actor{Type: domain.ActorSystem, Principal: "kernel:delegation", Via: domain.ViaRule}

// registerVaultLinks 在 delegate.report 报 done 的同一事务里，把落在已登记笔记库内的链接登记为链接型材料
// （项目取父事项的项目，标题取文件名，kind reference；同一位置已登记的复用），并以 output 关联到父事项（§7.5）。
// 父事项没有项目、链接不在任何已登记的库内或是普通网址、位置写法不合规时不登记，照旧只记在回执的证据里。
// 未登记任何笔记库的部署没有自动登记。
func registerVaultLinks(c *command.Ctx, child domain.Item, links []string) error {
	var vault []string
	for _, l := range links {
		if strings.HasPrefix(l, materials.VaultPrefix) {
			vault = append(vault, l)
		}
	}
	if len(vault) == 0 {
		return nil
	}
	parentID, ok, err := items.ParentOf(c.Tx, child.ID)
	if err != nil || !ok {
		return err
	}
	parent, err := items.Get(c.Tx, parentID)
	if err != nil || parent.Project == "" {
		return err
	}
	pv, pol, err := policy.Current(c.Tx)
	if err != nil {
		return err
	}
	basis := &domain.Basis{PolicyVersion: pv, RuleID: vaultLinkRule}
	for _, l := range vault {
		loc, err := materials.NormalizeLocation(l, pol.NoteVaults)
		if err != nil { // 库未登记或写法不合规：不是自动登记的对象
			continue
		}
		title := loc[strings.LastIndex(loc, "/")+1:]
		m, created, err := materials.LinkTx(c.Tx, parent.Project, materials.KindReference, title, nil, loc)
		if err != nil {
			return err
		}
		if created {
			if _, err := c.EmitAs(vaultActor, basis, domain.Event{Type: domain.EvMaterialRevised, Object: m.ID,
				Reason: "vault link in a done report: " + loc, Payload: materials.LinkedPayload(m)}); err != nil {
				return err
			}
		}
		snap, added, err := items.Relate(c.Tx, parent.ID, m.ID, domain.RelOutput, c.Now())
		if err != nil || !added {
			if err != nil {
				return err
			}
			continue
		}
		if _, err := command.Bump(c.Tx, parent.ID); err != nil { // 父事项不是本命令的授权对象
			return err
		}
		if _, err := c.EmitAs(vaultActor, basis, domain.Event{Type: domain.EvItemUpdated, Object: parent.ID,
			Reason: "output of the delegation: " + loc, Payload: snap}); err != nil {
			return err
		}
	}
	return nil
}
