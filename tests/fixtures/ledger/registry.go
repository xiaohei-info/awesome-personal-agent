// Package ledgerfixture 生成合成的 v0.1 结构账本夹具（OMAC-NOTES §5.1），并按账本结构版本派生同内容夹具。
// 只在测试目录里使用：不含任何真实数据，也不进入发布构建。
package ledgerfixture

import (
	"fmt"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
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
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
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

// Modules 与 cli/serve 的 registry() 装配同一份存储模块（registry_test.go 对照源码防漂移）。
var Modules = []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, runidentity.Module,
	kprojects.Module, clock.Module, kitems.Module, core.Module, kcontext.Module, delegation.Module, executors.Module,
	delivery.Module, attention.Module, proc.Module, direct.Module, task.Module, followup.Module, rules.Module,
	instances.Module, sregistry.Module, runner.Module, webhook.Module, channel.Module,
	materials.Module, hosted.Module, search.Module, guideline.Module, agentsmd.Module, background.Module, chain.Module,
	khealth.Module, kdoctor.Module, resume.Module, wake.Module, summaries.Module}

// Latest 是主干当前登记的最高账本结构版本（发布物 schema.ledger）。
func Latest() (int, error) {
	r, err := RegistryAt(1 << 30)
	if err != nil {
		return 0, err
	}
	return r.SchemaLedger(), nil
}

// RegistryAt 返回只含账本结构版本 ≤ v 的迁移的登记表：每个模块取其迁移序列里属于 ≤ v 的前缀，
// 一步都没有的模块不登记。这正是结构版本 v 的真实账本曾经应用过的那些迁移（迁移只增，按版本单调）。
func RegistryAt(v int) (*store.Registry, error) {
	reg := store.NewRegistry()
	for _, m := range Modules {
		n := 0
		for n < len(m.Migrations) && m.Migrations[n].LedgerVersion() <= v {
			n++
		}
		if n == 0 {
			continue
		}
		if err := reg.Register(store.Module{Name: m.Name, Migrations: m.Migrations[:n]}); err != nil {
			return nil, fmt.Errorf("ledgerfixture: %w", err)
		}
	}
	return reg, nil
}
