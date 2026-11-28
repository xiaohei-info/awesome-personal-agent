package serve

import (
	"context"
	"encoding/json"
	"log/slog"
	"maps"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ExecutorsModel 是执行者统一切换在 authz.switched 载荷里的标记（§16.5）。
const ExecutorsModel = "executors"

var executorSwitchActor = domain.Actor{Type: domain.ActorSystem, Principal: "executor-switch", Via: domain.ViaWeb}

// switchedTo 报告账本是否已做过 model 为 m 的切换。
func switchedTo(tx *store.Tx, m string) (bool, error) {
	es, err := eventlog.Query(tx, eventlog.Filter{Object: policy.SwitchObject, Type: domain.EvAuthzSwitched}, 0, 100)
	for _, e := range es {
		var p struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(e.Event.Payload, &p) == nil && p.Model == m {
			return true, nil
		}
	}
	return false, err
}

// switchExecutors 是"谁来干活"统一为执行者的一次性切换（§16.5），启动时在权限卡切换之后运行，幂等、单事务：
// 旧 Agent 卡换成隔离另写的声明与执行者、被选用的兼容卡换成执行者（executors.Unify）；策略里按卡选人的旧字段换算成
// 执行者 id 后清空，权限卡的旧键 legacy:<程序> 改成 <程序>，按卡的拉起方式并入按声明的一份；事项与安排上的旧选择同样换算。
// 最后记 authz.switched（model 为 executors，载荷是每一步的明细）。切换后拉起的命令、权限档与人格设置与切换前一致。
func switchExecutors(ctx context.Context, st *store.Store, now func() time.Time) error {
	return st.Update(ctx, func(tx *store.Tx) error {
		done, err := switchedTo(tx, ExecutorsModel)
		if err != nil || done {
			return err
		}
		_, pol, err := policy.Stored(tx)
		if err != nil {
			return err
		}
		def, main := "", ""
		if pol.DefaultHarness != "" {
			def, main = "legacy:"+pol.DefaultHarness, "legacy:"+pol.DefaultHarness
		}
		if pol.DefaultAgentID != nil {
			def = *pol.DefaultAgentID
		}
		if pol.MainAgentID != nil {
			main = *pol.MainAgentID
		}
		referenced := []string{def, main, pol.SourcePipeline.Agent}
		for _, id := range pol.RuleAgent {
			referenced = append(referenced, id)
		}
		u, err := executors.Unify(tx, referenced)
		if err != nil {
			return err
		}
		at := now()
		dropped := map[string]string{} // 换算不成执行者的选择位置 → 旧值
		conv := func(where, old string) string {
			if old == "" {
				return ""
			}
			if id := u.Map[old]; id != "" {
				return id
			}
			dropped[where] = old
			return ""
		}
		version, err := policy.UpdateWith(tx, executorSwitchActor, nil, "executor switch", func(np *policy.Policy) error {
			np.DefaultExecutor = conv("default_agent_id", def)
			np.MainExecutor = conv("main_agent_id", main)
			np.SourcePipeline.Executor = conv("source_pipeline.agent", np.SourcePipeline.Agent)
			for rule, old := range np.RuleAgent {
				if id := conv("rule_agent."+rule, old); id != "" {
					if np.RuleExecutor == nil {
						np.RuleExecutor = map[string]string{}
					}
					np.RuleExecutor[rule] = id
				}
			}
			for k, c := range maps.Clone(np.AgentCards) { // 兼容卡的权限卡就是那份声明的权限卡
				if key, ok := strings.CutPrefix(k, "legacy:"); ok {
					if _, has := np.AgentCards[key]; !has {
						np.AgentCards[key] = c
					}
					delete(np.AgentCards, k)
				}
			}
			for k, v := range np.MainSessionLaunch.ByAgent { // 旧卡已成为同 id 的声明
				if np.MainSessionLaunch.ByHarness == nil {
					np.MainSessionLaunch.ByHarness = map[string]string{}
				}
				np.MainSessionLaunch.ByHarness[k] = v
			}
			np.DefaultHarness, np.DefaultAgentID, np.MainAgentID, np.RuleAgent = "", nil, nil, nil
			np.SourcePipeline.Agent, np.MainSessionLaunch.ByAgent = "", nil
			return nil
		})
		if err != nil {
			return err
		}
		its, err := items.RemapPreferred(tx, u.Map, executorSwitchActor, at)
		if err != nil {
			return err
		}
		ins, err := instances.RemapExecutor(tx, u.Map, executorSwitchActor, at)
		if err != nil {
			return err
		}
		payload, err := json.Marshal(map[string]any{"model": ExecutorsModel, "policy_version": version, "unified": u,
			"dropped": dropped, "items": its, "instances": ins})
		if err != nil {
			return err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvAuthzSwitched, Object: policy.SwitchObject, Actor: executorSwitchActor,
			Reason: "executor switch: agent cards → executors", Payload: payload, At: at}); err != nil {
			return err
		}
		slog.Info("switched to executors", "declarations", len(u.Declarations), "executors", len(u.Executors), "skipped", len(u.Skipped),
			"dropped", len(dropped)+len(u.Dropped), "policy_version", version)
		return nil
	})
}
