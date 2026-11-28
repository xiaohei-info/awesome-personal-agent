package policy

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"
)

// 权限卡的十组（§14.3）。组键是稳定的契约：存在策略里、随拒绝返回、记在事件的依据里。
const (
	GroupView         = "view"
	GroupOrganize     = "organize"
	GroupSourceEvents = "source_events"
	GroupDecide       = "decide"
	GroupRules        = "rules"
	GroupConnect      = "connect"
	GroupOutbound     = "outbound"
	GroupChannels     = "channels"
	GroupAgents       = "agents"
	GroupPurge        = "purge"
)

// Groups 是十组，按权限卡上的顺序。
var Groups = []string{GroupView, GroupOrganize, GroupSourceEvents, GroupDecide, GroupRules, GroupConnect, GroupOutbound, GroupChannels, GroupAgents, GroupPurge}

// GroupGrant 是一组的开放；Until 为空表示一直允许。
type GroupGrant struct {
	Until *time.Time `json:"until,omitempty"`
}

// Card 是一个 Agent 的权限卡（§14.5）。不在 Groups 里的组即"不允许"。只有本人能改。
type Card struct {
	Groups map[string]GroupGrant `json:"groups"`
	// DecideProjects 是"替我拍板"的项目限定；空表示全部项目。
	DecideProjects []string `json:"decide_projects,omitempty"`
	// Tool 是它在主机上能用的工具（read/edit/full），主会话与全部后台运行同一档。
	Tool string `json:"tool"`
	// RunsAllMessages 为假（缺省）时后台运行只看信任发件人的挂靠消息（§14.6）。
	RunsAllMessages bool `json:"runs_all_messages,omitempty"`
}

// DefaultCard 是新 Agent 与没有归属的调用者的卡：前三组允许，工具只读，后台运行只看信任发件人的消息。
func DefaultCard() Card {
	return Card{Groups: map[string]GroupGrant{GroupView: {}, GroupOrganize: {}, GroupSourceEvents: {}}, Tool: PermRead}
}

// With 返回在 c 上再一直开放 groups 的副本（不改 c）。
func (c Card) With(groups ...string) Card {
	out := c
	out.Groups = maps.Clone(c.Groups)
	if out.Groups == nil {
		out.Groups = map[string]GroupGrant{}
	}
	for _, g := range groups {
		out.Groups[g] = GroupGrant{}
	}
	return out
}

// WithTool 返回把工具档位换成 tool 的副本。
func (c Card) WithTool(tool string) Card {
	c.Groups = maps.Clone(c.Groups)
	c.Tool = tool
	return c
}

// Allows 报告 now 时组 g 是否开放；到期即回到不允许，不需要后台任务。
func (c Card) Allows(g string, now time.Time) bool {
	gr, ok := c.Groups[g]
	return ok && (gr.Until == nil || now.Before(*gr.Until))
}

// Card 返回 agent（声明）的权限卡；没设过的 Agent 与没有归属的调用者取 DefaultCard。
func (p Policy) Card(agent string) Card {
	agent = AgentKey(agent)
	if c, ok := p.AgentCards[agent]; ok && agent != "" {
		return c
	}
	if c, ok := p.AgentCards[legacyPrefix+agent]; ok && agent != "" { // 执行者统一切换前的旧键
		return c
	}
	return DefaultCard()
}

const legacyPrefix = "legacy:"

// AgentKey 把历史记录里的 Agent id 认成声明：执行者统一切换前的兼容卡 legacy:<程序> 就是声明 <程序>（§16.5）；
// 运行快照、审计依据与规矩改动的"按 Agent 撤回"读旧 id 时都经它。
func AgentKey(id string) string { return strings.TrimPrefix(id, legacyPrefix) }

// Limits 是三项明示的全局上限（§14.5、§14.9）；0 表示没设过，取运行配置的初值。
type Limits struct {
	LongRuns   int `json:"long_runs,omitempty"`
	ShortRuns  int `json:"short_runs,omitempty"`
	ChainDepth int `json:"chain_depth,omitempty"`
}

// Or 返回设置里的值，没设过（0）时取运行配置的初值 fallback。
func limitOr(v, fallback int) int {
	if v > 0 {
		return v
	}
	return fallback
}

// Long、Short、Chain 是三项上限的生效值；fallback 是运行配置（config.yaml）的初值。
func (l Limits) Long(fallback int) int  { return limitOr(l.LongRuns, fallback) }
func (l Limits) Short(fallback int) int { return limitOr(l.ShortRuns, fallback) }
func (l Limits) Chain(fallback int) int { return limitOr(l.ChainDepth, fallback) }

// 三项上限的可调范围。
const (
	MaxRunsLimit       = 16
	MaxChainDepthLimit = 32
)

func normalizeCards(p *Policy) error {
	if p.AgentCards == nil {
		p.AgentCards = map[string]Card{}
	}
	for a, c := range p.AgentCards {
		if strings.TrimSpace(a) == "" {
			return invalid("agent_cards: agent id is empty")
		}
		if c.Groups == nil {
			c.Groups = map[string]GroupGrant{}
		}
		for g, gr := range c.Groups {
			if !slices.Contains(Groups, g) {
				return invalid("agent_cards %q: unknown group %q", a, g)
			}
			if g == GroupPurge && gr.Until == nil {
				return invalid("agent_cards %q: purge can only be allowed until a time", a)
			}
		}
		if c.Tool == "" {
			c.Tool = PermRead
		}
		if !oneOf(c.Tool, PermRead, PermEdit, PermFull) {
			return invalid("agent_cards %q: tool %q", a, c.Tool)
		}
		if c.DecideProjects = dedupSorted(c.DecideProjects); len(c.DecideProjects) == 0 {
			c.DecideProjects = nil
		}
		p.AgentCards[a] = c
	}
	for name, v := range map[string]struct{ n, max int }{"long_runs": {p.Limits.LongRuns, MaxRunsLimit}, "short_runs": {p.Limits.ShortRuns, MaxRunsLimit}, "chain_depth": {p.Limits.ChainDepth, MaxChainDepthLimit}} {
		if v.n < 0 || v.n > v.max {
			return invalid("limits %s %d: use 1 to %d, or 0 for the default", name, v.n, v.max)
		}
	}
	return nil
}

// 旧档位（决定 84）的取值，只用于切换时换算（§16.5）。
const (
	legacyOrganize = "organize"
	legacyDecide   = "decide"
	legacyRules    = "rules"
)

type legacyAllowance struct {
	Tier     string   `json:"tier"`
	Projects []string `json:"projects,omitempty"`
}

type legacyCap struct {
	ByAgent   map[string]string `json:"by_agent"`
	Default   string            `json:"default"`
	ByHarness map[string]string `json:"by_harness"`
}

// CardFromTier 按 §16.5 的映射表把一个旧档位换成权限卡：没设过与只整理同为前三组；
// 也替我拍板加第 4 组（项目限定照搬）；连日常规矩也替我改再加第 5 组。第 6–10 组一律不允许，开放都不带期限。
func CardFromTier(tier string, projects []string, tool string) (Card, error) {
	c := DefaultCard()
	c.Tool = tool
	switch tier {
	case "", legacyOrganize:
	case legacyDecide:
		c.Groups[GroupDecide] = GroupGrant{}
		c.DecideProjects = slices.Clone(projects)
	case legacyRules:
		c.Groups[GroupDecide] = GroupGrant{}
		c.Groups[GroupRules] = GroupGrant{}
		c.DecideProjects = slices.Clone(projects)
	default:
		return Card{}, fmt.Errorf("%w: agent_tier %q", ErrInvalid, tier)
	}
	return c, nil
}

// CardsFromLegacy 把旧文档的 agent_tier 与 permission_cap 两段（原文，可为空）换算成权限卡（§16.5）。
// 换算对象是 ids（执行者名录里的 Agent、旧钥匙的兼容卡）与两段里出现过的全部键；工具档位按旧口径：
// by_agent，兼容卡（legacy:<程序>）再取 by_harness 与 default，都没有为只读。
func CardsFromLegacy(tierRaw, capRaw json.RawMessage, ids []string) (map[string]Card, error) {
	var tiers struct {
		ByAgent map[string]legacyAllowance `json:"by_agent"`
	}
	var caps legacyCap
	for _, x := range []struct {
		raw json.RawMessage
		v   any
	}{{tierRaw, &tiers}, {capRaw, &caps}} {
		if len(x.raw) > 0 {
			if err := json.Unmarshal(x.raw, x.v); err != nil {
				return nil, fmt.Errorf("policy: legacy section: %w", err)
			}
		}
	}
	all := slices.Clone(ids)
	for id := range tiers.ByAgent {
		all = append(all, id)
	}
	for id := range caps.ByAgent {
		all = append(all, id)
	}
	for h := range caps.ByHarness {
		all = append(all, "legacy:"+h)
	}
	out := map[string]Card{}
	for _, id := range all {
		if id == "" {
			continue
		}
		tool := caps.ByAgent[id]
		if h, legacy := strings.CutPrefix(id, "legacy:"); legacy && tool == "" {
			tool = cmpOr(caps.ByHarness[h], caps.Default)
		}
		al := tiers.ByAgent[id]
		c, err := CardFromTier(al.Tier, al.Projects, cmpOr(tool, PermRead))
		if err != nil {
			return nil, err
		}
		out[id] = c
	}
	return out, nil
}

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
