package serve

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func init() { client.Register("authz", authzCmd) }

const authzUsage = "usage: apa authz preview [--json] [--days 30]"

// Preview 是 `apa authz preview` 的结果（§16.5）：切换会写出的权限卡、最近 Agent 的命令在权限卡下结局有变的清单、
// 存量 Agent 与钥匙落到新模型的样子。只读：在账本的在线副本上算，数据目录不受影响。
type Preview struct {
	Switched bool       `json:"switched"` // 已切换到权限卡
	Plan     SwitchPlan `json:"plan"`
	Days     int        `json:"days"`
	// Rejudge：按事件类型还原出的 Agent 命令，在它所属 Agent 的权限卡下与当时相比：unchanged、now_allowed、now_refused、uncertain。
	Rejudge  map[string]int             `json:"rejudge"`
	Details  []RejudgedEvent            `json:"details"` // 结局有变或无法判定的明细
	Agents   []AgentRow                 `json:"agents"`
	Keys     []KeyRow                   `json:"keys"`
	Settings map[string]json.RawMessage `json:"settings"`
}

type RejudgedEvent struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Actor   string    `json:"actor"`
	Agent   string    `json:"agent"`
	Command string    `json:"command"`
	Group   string    `json:"group"`
	Before  string    `json:"before"` // effective / refused
	Outcome string    `json:"outcome"`
	Note    string    `json:"note,omitempty"`
}

type AgentRow struct {
	ID, Name, Harness string
	Enabled           bool
	Card              policy.Card `json:"card"` // 切换后的权限卡
}

type KeyRow struct {
	ID      string `json:"id"`
	Kind    string `json:"kind"`
	Label   string `json:"label,omitempty"`
	Harness string `json:"harness,omitempty"`
	AgentID string `json:"agent_id,omitempty"`
	Revoked bool   `json:"revoked"`
	After   string `json:"after"`
}

// previewSettings 是参与权限判定、要在切换前后对照的策略段（不含模型端点：那里有密钥）。
var previewSettings = []string{"agent_tier", "permission_cap", "agent_cards", "limits", "main_session_launch",
	"rule_agent", "default_agent_id", "main_agent_id", "channel_answerers", "forward_allowlist"}

func authzCmd(c *client.Ctx, args []string) error {
	if len(args) == 0 || args[0] != "preview" {
		return domain.NewError(domain.CodeInvalidRequest, "unknown authz subcommand", authzUsage)
	}
	fs := flag.NewFlagSet("authz preview", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := fs.Bool("json", false, "")
	days := fs.Int("days", 30, "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 || *days < 1 {
		return domain.NewError(domain.CodeInvalidRequest, "invalid arguments for authz preview", authzUsage)
	}
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	pv, err := RunPreview(context.Background(), home, *days, clock.Now())
	if err != nil {
		return err
	}
	if *asJSON {
		return c.Print(pv)
	}
	_, err = fmt.Fprint(c.Env.Stdout, renderPreview(pv))
	return err
}

// RunPreview 在 home 的账本在线副本上算出 Preview；副本放在临时目录，用完删除。
func RunPreview(ctx context.Context, home string, days int, now time.Time) (Preview, error) {
	dir, err := os.MkdirTemp("", "apa-authz-preview-")
	if err != nil {
		return Preview{}, err
	}
	defer os.RemoveAll(dir)
	snap := dir + "/ledger.db"
	if err := store.BackupFile(ctx, home, snap); err != nil {
		return Preview{}, err
	}
	st, err := store.OpenSnapshot(snap)
	if err != nil {
		return Preview{}, err
	}
	defer st.Close()
	pv := Preview{Days: days, Rejudge: map[string]int{"unchanged": 0, "now_allowed": 0, "now_refused": 0, "uncertain": 0}, Details: []RejudgedEvent{}}
	err = st.View(ctx, func(tx *store.Tx) error {
		var err error
		if pv.Switched, err = cardsSwitched(tx); err != nil {
			return err
		}
		if pv.Plan, err = PlanSwitch(tx, now); err != nil {
			return err
		}
		if pv.Settings, err = policy.StoredRaw(tx, previewSettings...); err != nil {
			return err
		}
		_, pol, err := policy.Stored(tx)
		if err != nil {
			return err
		}
		card := func(agent string) policy.Card {
			if c, ok := pv.Plan.Cards[agent]; ok {
				return c
			}
			return pol.Card(agent)
		}
		owners, err := keyOwners(tx)
		if err != nil {
			return err
		}
		if err := rejudge(tx, &pv, card, owners, now.Add(-time.Duration(days)*24*time.Hour), now); err != nil {
			return err
		}
		return inventory(tx, &pv, card)
	})
	return pv, err
}

// rejudge：最近的 Agent 事件按事件类型还原成命令与组，在所属 Agent 换算后的权限卡下重判，与当时的结局比较。
// 旧事件没有记命令名，这是近似：同一类型对应的命令组相同时才算准，不同时按载荷细分（建为建议与否、离开哪个阶段）。
// owners 是钥匙 id 到所属 Agent（与 command.Caller.Agent 同口径），用于旧事件没记 Agent 时按发起的钥匙归属。
func rejudge(tx *store.Tx, pv *Preview, card func(string) policy.Card, owners map[string]string, since, now time.Time) error {
	var after int64
	for {
		es, err := eventlog.Read(tx, after, 1000)
		if err != nil {
			return err
		}
		if len(es) == 0 {
			return nil
		}
		for _, e := range es {
			after = e.Event.Seq
			ev := e.Event
			if ev.At.Before(since) || ev.Actor.Type != domain.ActorAgent || strings.HasPrefix(ev.Actor.Principal, "kernel") {
				continue
			}
			cmd, group, agent, note := commandOf(ev)
			if cmd == "" {
				continue
			}
			if agent == "" {
				if agent, err = agentOf(tx, owners, ev.Actor.Principal); err != nil {
					return err
				}
			}
			before := "effective"
			if ev.Type == domain.EvCommandRejected {
				before = "refused"
			}
			outcome := "uncertain"
			switch {
			case group == "" || group == "uncertain":
			case group == string(authz.GroupInternal) || group == string(authz.GroupOwner):
				outcome = map[string]string{"effective": "now_refused", "refused": "unchanged"}[before]
			default:
				allowed := card(agent).Allows(group, now)
				switch {
				case allowed == (before == "effective"):
					outcome = "unchanged"
				case allowed:
					outcome = "now_allowed"
				default:
					outcome = "now_refused"
				}
			}
			pv.Rejudge[outcome]++
			if outcome != "unchanged" {
				pv.Details = append(pv.Details, RejudgedEvent{Seq: ev.Seq, At: ev.At, Type: ev.Type, Actor: ev.Actor.Principal, Agent: agent,
					Command: cmd, Group: group, Before: before, Outcome: outcome, Note: note})
			}
		}
	}
}

// oldTierGroup 是旧档位到组：3b 拆到了五组和只有本人，旧记录里分不出是哪一条，按 uncertain 处理（除非对象就是命令名）。
var oldTierGroup = map[string]string{"read": policy.GroupView, "organize": policy.GroupOrganize, "decide": policy.GroupDecide, "rules": policy.GroupRules,
	"internal": string(authz.GroupInternal), "power": "uncertain"}

// powerCommands 是旧 3b 新建类命令（拒绝事件记在命令名下）在权限卡下的组。
var powerCommands = map[string]string{"source.register": policy.GroupConnect, "source.register_script": policy.GroupConnect,
	"connection.create": policy.GroupChannels, "executor.register": policy.GroupAgents, "credential.issue": string(authz.GroupOwner)}

// commandOf 由事件类型（必要时看载荷）还原命令、组与所属 Agent；不是 Agent 发起的命令（健康告警、运行结束）返回空。
func commandOf(ev domain.Event) (cmd, group, agent, note string) {
	var p struct {
		Before *domain.Item `json:"before"`
		Item   *domain.Item `json:"item"`
		Action string       `json:"action"`
		Code   string       `json:"code"`
		Tier   string       `json:"tier"`
		Agent  string       `json:"agent"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	if ev.Basis != nil {
		agent = ev.Basis.Agent
	}
	switch ev.Type {
	case domain.EvItemCreated:
		if p.Item != nil && p.Item.Phase != domain.PhaseProposed {
			return "item.create", policy.GroupDecide, agent, "created as " + string(p.Item.Phase) + ", not as a proposal"
		}
		return "item.create", policy.GroupOrganize, agent, ""
	case domain.EvItemPhaseChanged:
		if p.Before != nil && (p.Before.Phase == domain.PhaseProposed || p.Before.Phase == domain.PhaseNeedsInput) {
			return "item.transition", policy.GroupDecide, agent, "left " + string(p.Before.Phase)
		}
		return "item.transition", policy.GroupOrganize, agent, ""
	case domain.EvItemUpdated, "item.progress_added", domain.EvDecisionRequested, domain.EvDelegationCreated, domain.EvDelegationReported,
		domain.EvMaterialRevised, domain.EvMaterialRevisionProposed, "executor.declared":
		return ev.Type, policy.GroupOrganize, agent, ""
	case domain.EvDecisionResolved:
		return "decision.answer", policy.GroupDecide, agent, ""
	case domain.EvCommandRejected:
		agent = p.Agent
		g, known := oldTierGroup[p.Tier]
		switch {
		case !known && p.Tier != "": // 切换后的记录：tier 键已是组键
			g = p.Tier
		case g == "uncertain":
			if c, ok := powerCommands[ev.Object]; ok {
				return ev.Object, c, agent, "old code " + p.Code
			}
		case p.Code == string(domain.CodePermissionDenied):
			return "rejected:" + p.Action, "", agent, "key scope (" + p.Code + "); the card does not change it"
		}
		return "rejected:" + p.Action, g, agent, "old code " + p.Code + " on " + ev.Object
	}
	return "", "", "", ""
}

// agentOf 与 command.Caller.Agent 同口径：运行凭据（run:<id>）取运行记录上的 Agent，钥匙取 owners 里的归属。
func agentOf(tx *store.Tx, owners map[string]string, principal string) (string, error) {
	run, ok := strings.CutPrefix(principal, "run:")
	if !ok {
		return owners[principal], nil
	}
	rec, err := proc.Get(tx, run)
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeNotFound {
		return "", nil // 运行记录已不在：按没有归属
	}
	return rec.Run.InstanceID(), err
}

func keyOwners(tx *store.Tx) (map[string]string, error) {
	creds, err := auth.New(nil).List(tx)
	if err != nil {
		return nil, err
	}
	owners := map[string]string{}
	for _, k := range creds {
		if k.AgentID == "" && k.Harness != "" {
			owners[k.ID] = "legacy:" + k.Harness
		} else {
			owners[k.ID] = k.AgentID
		}
	}
	return owners, nil
}

func inventory(tx *store.Tx, pv *Preview, card func(string) policy.Card) error {
	cards, err := executors.Cards(tx)
	if err != nil {
		return err
	}
	seen := map[string]bool{}
	for _, c := range cards {
		seen[c.ID] = true
		pv.Agents = append(pv.Agents, AgentRow{ID: c.ID, Name: c.Name, Harness: c.Harness, Enabled: c.Enabled, Card: card(c.ID)})
	}
	for _, id := range slices.Sorted(maps.Keys(pv.Plan.Cards)) { // 兼容卡与只在旧段里出现过的 Agent
		if !seen[id] {
			pv.Agents = append(pv.Agents, AgentRow{ID: id, Name: id, Card: pv.Plan.Cards[id]})
		}
	}
	creds, err := auth.New(nil).List(tx)
	if err != nil {
		return err
	}
	for _, k := range creds {
		after := "unchanged: scope by key kind; groups from its Agent's permission card"
		switch {
		case !k.RevokedAt.IsZero():
			after = "revoked; stays revoked"
		case (k.Kind == auth.Delegate || k.Kind == auth.Worker) && k.AgentID == "" && k.Harness != "":
			after = "follows the compatibility card legacy:" + k.Harness
		case (k.Kind == auth.Delegate || k.Kind == auth.Worker) && k.AgentID == "":
			after = "not bound to an Agent: default card (view, organize items, handle source events)"
		case k.Kind == auth.Delegate || k.Kind == auth.Worker:
			after = "follows Agent " + k.AgentID + "'s permission card"
		}
		pv.Keys = append(pv.Keys, KeyRow{ID: k.ID, Kind: string(k.Kind), Label: k.Label, Harness: k.Harness, AgentID: k.AgentID, Revoked: !k.RevokedAt.IsZero(), After: after})
	}
	return nil
}

func renderPreview(pv Preview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "switched to permission cards: %v\n", pv.Switched)
	fmt.Fprintf(&b, "switch plan: revoke %d grants, clear %d questions, drop policy sections %v, write permission cards for %d agents, %d projects with match rules\n",
		len(pv.Plan.Grants), len(pv.Plan.Asks), pv.Plan.LegacyPolicy, len(pv.Plan.Cards), len(pv.Plan.MatchProjects))
	for _, a := range pv.Agents {
		fmt.Fprintf(&b, "agent %s %q harness=%s tool=%s groups=%v decide_projects=%v runs_all_messages=%v\n", a.ID, a.Name, a.Harness, a.Card.Tool,
			slices.Sorted(maps.Keys(a.Card.Groups)), a.Card.DecideProjects, a.Card.RunsAllMessages)
	}
	fmt.Fprintf(&b, "last %d days, Agent commands under the permission cards: unchanged %d, now allowed %d, now refused %d, uncertain %d\n",
		pv.Days, pv.Rejudge["unchanged"], pv.Rejudge["now_allowed"], pv.Rejudge["now_refused"], pv.Rejudge["uncertain"])
	for _, d := range pv.Details {
		fmt.Fprintf(&b, "  #%d %s %s agent=%s %s group=%s %s → %s %s\n", d.Seq, d.At.Format(time.RFC3339), d.Actor, d.Agent, d.Command, d.Group, d.Before, d.Outcome, d.Note)
	}
	for _, k := range pv.Keys {
		fmt.Fprintf(&b, "key %s kind=%s label=%q agent=%s revoked=%v → %s\n", k.ID, k.Kind, k.Label, k.AgentID, k.Revoked, k.After)
	}
	return b.String()
}
