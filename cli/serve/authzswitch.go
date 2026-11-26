package serve

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
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
	Grants        []policy.Grant         `json:"grants"`         // 将撤销的有效授权单
	Asks          []string               `json:"asks"`           // 将清掉初筛标记、风险类别或超时默认的未答问题
	LegacyPolicy  []string               `json:"legacy_policy"`  // 策略文档里将去掉的旧段
	Cards         map[string]policy.Card `json:"cards"`          // 将写入的权限卡（按 §16.5 映射表由旧档位与工具上限换算）
	MatchProjects []string               `json:"match_projects"` // 还带按维度匹配规则的项目
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
	if p.Cards, err = PlanCards(tx); err != nil {
		return p, err
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

// CardsModel 是权限卡切换在 authz.switched 载荷里的标记（§16.5）。
const CardsModel = "cards"

// PlanCards 只读地算出切换要写的权限卡：执行者名录里的 Agent、没有 Agent 归属而记了程序的钥匙的兼容卡、
// 旧 agent_tier 与 permission_cap 段里出现过的键，按 §16.5 的映射表换算；已经有权限卡的不动。
func PlanCards(tx *store.Tx) (map[string]policy.Card, error) {
	raw, err := policy.StoredRaw(tx, "agent_tier", "permission_cap")
	if err != nil {
		return nil, err
	}
	agents, err := executors.Cards(tx)
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, c := range agents {
		ids = append(ids, c.ID)
	}
	creds, err := auth.New(nil).List(tx)
	if err != nil {
		return nil, err
	}
	for _, c := range creds {
		if c.AgentID == "" && c.Harness != "" {
			ids = append(ids, "legacy:"+c.Harness)
		}
	}
	out, err := policy.CardsFromLegacy(raw["agent_tier"], raw["permission_cap"], ids)
	if err != nil {
		return nil, err
	}
	_, pol, err := policy.Stored(tx)
	if err != nil {
		return nil, err
	}
	for id := range pol.AgentCards {
		delete(out, id)
	}
	return out, nil
}

// cardsSwitched 报告账本是否已切换到权限卡：有一条载荷 model 为 cards 的 authz.switched。
func cardsSwitched(tx *store.Tx) (bool, error) {
	es, err := eventlog.Query(tx, eventlog.Filter{Object: policy.SwitchObject, Type: domain.EvAuthzSwitched}, 0, 100)
	for _, e := range es {
		var p struct {
			Model string `json:"model"`
		}
		if json.Unmarshal(e.Event.Payload, &p) == nil && p.Model == CardsModel {
			return true, nil
		}
	}
	return false, err
}

// switchAuthz 是权限模型的一次性切换（§16.5），启动时在迁移之后运行，幂等。还没做过第二版切换的账本先
// 撤销全部有效授权单、清掉未答问题上的旧状态；然后按映射表为每个 Agent 写权限卡、去掉旧段，写一版策略，
// 最后记 authz.switched（载荷是每一步的明细，model 为 cards）。全部在一个事务里，要么都成要么都不动。
func switchAuthz(ctx context.Context, st *store.Store, now func() time.Time) error {
	return st.Update(ctx, func(tx *store.Tx) error {
		v2, err := policy.Switched(tx)
		if err != nil {
			return err
		}
		done, err := cardsSwitched(tx)
		if err != nil || done {
			return err
		}
		plan, err := PlanSwitch(tx, now())
		if err != nil {
			return err
		}
		at := now()
		var revoked []string
		var asks []string
		if !v2 {
			if revoked, err = policy.RevokeAllGrants(tx, authzSwitchActor, at, "permission model switch"); err != nil {
				return err
			}
		}
		version, err := policy.UpdateWith(tx, authzSwitchActor, nil, "permission model switch", func(np *policy.Policy) error {
			if np.AgentCards == nil {
				np.AgentCards = map[string]policy.Card{}
			}
			for id, c := range plan.Cards {
				np.AgentCards[id] = c
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !v2 {
			if asks, err = decisions.ClearLegacyAskState(tx, authzSwitchActor, at); err != nil {
				return err
			}
		}
		// shortcut: 带维度匹配规则的项目只记下、不自动展开成 id 名单（本机账本没有项目）；有的话由本人在项目设置里重设名单。
		payload, err := json.Marshal(map[string]any{"model": CardsModel, "grants_revoked": revoked, "asks_cleared": asks, "policy_version": version,
			"legacy_policy": plan.LegacyPolicy, "cards": plan.Cards, "match_projects": plan.MatchProjects})
		if err != nil {
			return err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvAuthzSwitched, Object: policy.SwitchObject, Actor: authzSwitchActor,
			Reason: "permission model switch", Payload: payload, At: at}); err != nil {
			return err
		}
		slog.Info("permission model switched", "model", CardsModel, "cards", len(plan.Cards), "grants_revoked", len(revoked), "asks_cleared", len(asks), "policy_version", version)
		return nil
	})
}
