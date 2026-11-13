package doctor

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 升级预检的检查名（不在 §15.5 的九类里，排在报告末尾）。
const (
	checkSchema     = "schema_version"
	checkPackCompat = "pack_compat"
	checkOutbound   = "source_outbound"
	checkPublicURL  = "public_url"
	checkFollowUps  = "after_upgrade"
)

// UpgradeCheck 是 `apa doctor --upgrade-check`（§16.5 升级第 1 步）：升级前用新版本二进制只读地报告
// 数据结构版本差异、会被拒绝的用户能力包（逐字段）、未选外发范围的来源、监听非本机地址时缺少的 public_url，以及升级后需要本人处理的事项。
// 账本只在临时目录的副本上读，数据目录不新增、不改动任何文件，服务停止或运行都一样。
func UpgradeCheck(ctx context.Context, in Input) kdoctor.Report {
	layout := config.LayoutOf(in.Home)
	l, openErr := openLedger(ctx, in)
	defer l.cleanup()

	schema := kdoctor.Check{Name: checkSchema, Status: kdoctor.OK}
	target := in.Reg.SchemaLedger()
	switch {
	case openErr != nil:
		schema.Status, schema.Why, schema.Next = kdoctor.Error, "the ledger cannot be read: "+openErr.Error(), "set APA_HOME to the data directory to upgrade"
	case l.infoErr != nil && errors.Is(l.infoErr, store.ErrSchemaTooNew):
		schema.Status, schema.Why, schema.Next = kdoctor.Error, fmt.Sprintf("the ledger is newer than this binary (ledger structure up to %d); an older binary never opens newer data", target), "use the binary that matches the data"
	case l.infoErr != nil:
		schema.Status, schema.Why, schema.Next = kdoctor.Error, "the ledger is damaged: "+l.infoErr.Error(), "restore a backup before upgrading"
	default:
		schema.Data = map[string]any{"current": l.info.Ledger, "target": target}
		if l.info.Ledger < target {
			schema.Why = fmt.Sprintf("ledger structure %d → %d: apa serve takes a pre-upgrade snapshot under backups/ and then migrates (additive only)", l.info.Ledger, target)
		} else {
			schema.Why = fmt.Sprintf("ledger structure %d already matches this binary; there is nothing to migrate", l.info.Ledger)
		}
		schema.Next = "stop the service, replace the release, start apa serve"
	}

	cfgCheck, cfg := configCheck(layout)
	urlCheck := kdoctor.Check{Name: checkPublicURL, Status: kdoctor.OK, Why: "listen address and public_url are compatible with the new version", Next: "nothing to do"}
	if cfgCheck.Status == kdoctor.Error {
		urlCheck.Status = kdoctor.Error
		urlCheck.Why, urlCheck.Next = cfgCheck.Why, cfgCheck.Next
		if cfg.Listen != "" && cfg.PublicURL == "" && cfg.CheckExposure() != nil {
			urlCheck.Why = "the new version refuses to start: " + cfgCheck.Why
		}
	}

	pc := kdoctor.Check{Name: checkPackCompat, Status: kdoctor.OK, Why: "no user pack would be rejected", Next: "nothing to do"}
	ld := offlinePacks(layout)
	for _, r := range ld.Health() {
		pc.Add(kdoctor.Detail{Subject: r.Pack, Status: kdoctor.Warning, Why: "would be rejected: " + r.Reason + "; the builtin pack of the same name takes effect",
			Next: "fix the pack under packs/" + r.Pack + " as appendix C.7 and C.11 describe"})
	}
	if pc.Status != kdoctor.OK {
		pc.Why, pc.Next = "user packs that override builtin packs would be rejected after the upgrade", "fix them before or after upgrading; the builtin packs keep working meanwhile"
	}

	oc := kdoctor.Check{Name: checkOutbound, Status: kdoctor.OK, Why: "every source has chosen its external outbound scope", Next: "nothing to do"}
	if l.st != nil {
		srcs, err := kdoctor.LegacySources(ctx, l.st)
		if err != nil {
			oc.Status, oc.Why, oc.Next = kdoctor.Error, "the source registrations cannot be read: "+err.Error(), "restore a backup before upgrading"
		}
		for _, s := range srcs {
			if s.Outbound.External == nil {
				oc.Add(kdoctor.Detail{Subject: s.ID, Status: kdoctor.Warning, Why: "the external outbound scope is not chosen; it stays unset until you choose it",
					Next: "choose the external outbound scope of this source in Web before using an external model endpoint"})
			}
		}
		if oc.Status == kdoctor.Warning {
			oc.Why, oc.Next = fmt.Sprintf("%d source(s) have no external outbound scope; until chosen, events that need an external endpoint are held as outbound_unset", len(oc.Details)), "choose the scope per source in Web after the upgrade"
		}
	}

	fu := kdoctor.Check{Name: checkFollowUps, Status: kdoctor.OK, Why: "things for you to handle after the upgrade", Next: "see details"}
	todo := map[string]string{
		"web-login":  "Web login sessions made before the upgrade are invalid; log in once again",
		"plugins":    "upgrade the host plugins together with the kernel; old plugins do not report the session type and answers on your own words are rejected",
		"rule-modes": "builtin reminder rules still on their initial observe mode switch to auto and are recorded as rule.mode_changed",
		"event-subs": "subscribers and channel ext should skip event types they do not know and handle deliveries without an item",
	}
	if oc.Status == kdoctor.Warning {
		todo["outbound"] = "choose the external outbound scope of each source listed above"
	}
	if pc.Status == kdoctor.Warning {
		todo["packs"] = "fix the user packs listed above"
	}
	ids := make([]string, 0, len(todo))
	for id := range todo {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		fu.Add(kdoctor.Detail{Subject: id, Status: kdoctor.OK, Why: todo[id]})
	}
	fu.Why = strings.TrimSpace(fu.Why)
	return kdoctor.Assemble(schema, urlCheck, pc, oc, fu)
}
