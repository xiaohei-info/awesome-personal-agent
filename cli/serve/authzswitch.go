package serve

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var authzSwitchActor = domain.Actor{Type: domain.ActorSystem, Principal: "authz-switch", Via: domain.ViaWeb}

// SwitchPlan 是切换会做的事：preview 只读地算出它，switchAuthz 在同一份计算上写入。
type SwitchPlan struct {
	Grants        []policy.Grant    `json:"grants"`         // 将撤销的有效授权单
	Asks          []string          `json:"asks"`           // 将清掉初筛标记、风险类别或超时默认的未答问题
	LegacyPolicy  []string          `json:"legacy_policy"`  // 策略文档里将去掉的旧段
	AgentTier     map[string]string `json:"agent_tier"`     // 将显式写入的档位（现有 Agent 一律只整理）
	MatchProjects []string          `json:"match_projects"` // 还带按维度匹配规则的项目
}

// PlanSwitch 只读地算出切换会做的事。
func PlanSwitch(tx *store.Tx, now time.Time) (SwitchPlan, error) {
	var p SwitchPlan
	gs, err := policy.ListGrants(tx)
	if err != nil {
		return p, err
	}
	for _, g := range gs {
		if g.RevokedAt.IsZero() && (g.ExpiresAt.IsZero() || now.Before(g.ExpiresAt)) {
			p.Grants = append(p.Grants, g)
		}
	}
	if p.LegacyPolicy, err = policy.LegacyKeys(tx); err != nil {
		return p, err
	}
	_, pol, err := policy.Stored(tx)
	if err != nil {
		return p, err
	}
	cards, err := executors.Cards(tx)
	if err != nil {
		return p, err
	}
	p.AgentTier = map[string]string{}
	for _, c := range cards {
		if _, set := pol.AgentTier.ByAgent[c.ID]; !set {
			p.AgentTier[c.ID] = policy.AllowOrganize
		}
	}
	its, err := decisions.LegacyAskItems(tx)
	if err != nil {
		return p, err
	}
	p.Asks = its
	prs, err := projects.List(tx)
	if err != nil {
		return p, err
	}
	for _, pr := range prs {
		if len(pr.AllowedExecutorMatch) > 0 {
			p.MatchProjects = append(p.MatchProjects, pr.ID)
		}
	}
	return p, nil
}

// switchAuthz 是权限模型第二版的一次性切换（§16.5），启动时在迁移之后运行，幂等：
// 撤销全部有效授权单、写一版不含旧段且为每个现有 Agent 显式写"只整理"的策略、清掉未答问题上的旧状态，
// 最后记 authz.switched（载荷是每一步的明细）。全部在一个事务里，要么都成要么都不动。
func switchAuthz(ctx context.Context, st *store.Store, now func() time.Time) error {
	return st.Update(ctx, func(tx *store.Tx) error {
		done, err := policy.Switched(tx)
		if err != nil || done {
			return err
		}
		plan, err := PlanSwitch(tx, now())
		if err != nil {
			return err
		}
		at := now()
		revoked, err := policy.RevokeAllGrants(tx, authzSwitchActor, at, "permission model switch")
		if err != nil {
			return err
		}
		version, err := policy.UpdateWith(tx, authzSwitchActor, nil, "permission model switch", func(np *policy.Policy) error {
			if np.AgentTier.ByAgent == nil {
				np.AgentTier.ByAgent = map[string]policy.AgentAllowance{}
			}
			for id, t := range plan.AgentTier {
				np.AgentTier.ByAgent[id] = policy.AgentAllowance{Tier: t}
			}
			return nil
		})
		if err != nil {
			return err
		}
		asks, err := decisions.ClearLegacyAskState(tx, authzSwitchActor, at)
		if err != nil {
			return err
		}
		// shortcut: 带维度匹配规则的项目只记下、不自动展开成 id 名单（本机账本没有项目）；有的话由本人在项目设置里重设名单。
		payload, err := json.Marshal(map[string]any{"grants_revoked": revoked, "asks_cleared": asks, "policy_version": version,
			"legacy_policy": plan.LegacyPolicy, "agent_tier": plan.AgentTier, "match_projects": plan.MatchProjects})
		if err != nil {
			return err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvAuthzSwitched, Object: policy.SwitchObject, Actor: authzSwitchActor,
			Reason: "permission model switch", Payload: payload, At: at}); err != nil {
			return err
		}
		slog.Info("permission model switched", "grants_revoked", len(revoked), "asks_cleared", len(asks), "policy_version", version)
		return nil
	})
}
