package serve

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"maps"
	"os"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func init() { client.Register("authz", authzCmd) }

const authzUsage = "usage: apa authz preview [--json] [--days 30]"

// Preview 是 `apa authz preview` 的结果：切换会做什么、最近 Agent 的命令在新模型下的结局、存量对象落到新模型的样子。
// 只读：在账本的在线副本上算，数据目录不受影响。
type Preview struct {
	Switched bool       `json:"switched"`
	Plan     SwitchPlan `json:"plan"`
	Days     int        `json:"days"`
	// Rejudge：按事件类型还原出的 Agent 命令，在"现有 Agent 一律只整理"下的结局。
	Rejudge   map[string]int             `json:"rejudge"` // effective / pending_owner(23) / refused(20、4)
	Details   []RejudgedEvent            `json:"details"` // 后两类的明细
	Agents    []AgentRow                 `json:"agents"`
	Keys      []KeyRow                   `json:"keys"`
	Materials []MaterialRow              `json:"materials"`
	Settings  map[string]json.RawMessage `json:"settings"`
}

type RejudgedEvent struct {
	Seq     int64     `json:"seq"`
	At      time.Time `json:"at"`
	Type    string    `json:"type"`
	Actor   string    `json:"actor"`
	Command string    `json:"command"`
	Tier    string    `json:"tier"`
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

type MaterialRow struct {
	Kind   string `json:"kind"`
	Before string `json:"before"`
	After  string `json:"after"`
}

// previewSettings 是参与权限判定、要在切换前后对照的策略段（不含模型端点：那里有密钥）。
var previewSettings = []string{"web_only", "screening_timeout_ms", "material_write", "agent_tier", "permission_cap", "main_session_launch",
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
	pv := Preview{Days: days, Rejudge: map[string]int{"effective": 0, "pending_owner": 0, "refused": 0, "uncertain": 0}}
	err = st.View(ctx, func(tx *store.Tx) error {
		var err error
		if pv.Switched, err = policy.Switched(tx); err != nil {
			return err
		}
		if pv.Plan, err = PlanSwitch(tx, now); err != nil {
			return err
		}
		if pv.Settings, err = policy.StoredRaw(tx, previewSettings...); err != nil {
			return err
		}
		if err := rejudge(tx, &pv, now.Add(-time.Duration(days)*24*time.Hour)); err != nil {
			return err
		}
		return inventory(tx, &pv)
	})
	return pv, err
}

// rejudge：最近的 Agent 事件按事件类型还原成命令，再按新档位判（现有 Agent 切换后一律只整理，所以二档、3a 都是 23）。
// 旧事件没有记命令名，这是近似：同一类型对应的命令档位相同时才算准，不同时按载荷细分（建为建议与否、离开哪个阶段）。
func rejudge(tx *store.Tx, pv *Preview, since time.Time) error {
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
			cmd, tier, note := commandOf(ev)
			if cmd == "" {
				continue
			}
			outcome := "effective"
			switch tier {
			case "uncertain":
				outcome = "uncertain"
			case "decide", "rules":
				outcome = "pending_owner"
			case "power", "internal":
				outcome = "refused"
			}
			pv.Rejudge[outcome]++
			if outcome != "effective" {
				pv.Details = append(pv.Details, RejudgedEvent{Seq: ev.Seq, At: ev.At, Type: ev.Type, Actor: ev.Actor.Principal, Command: cmd, Tier: tier, Outcome: outcome, Note: note})
			}
		}
	}
}

// wordsTier 是旧"只接受本人原话"档里新建类命令（拒绝事件记在命令名下）在新模型的档。
var wordsTier = map[string]string{"project.create": "organize", "source.register": "power", "inbound.backlog_rules": "organize",
	"inbound.backlog_score": "organize", "inbound.backlog_rejudge": "organize", "inbound.backlog_restore": "organize", "inbound.ignore_unjudged": "organize"}

// commandOf 由事件类型（必要时看载荷）还原命令与它在新模型下的档；不是 Agent 发起的命令（健康告警、运行结束）返回空。
func commandOf(ev domain.Event) (cmd, tier, note string) {
	var p struct {
		Before *domain.Item `json:"before"`
		Item   *domain.Item `json:"item"`
		Action string       `json:"action"`
		Code   string       `json:"code"`
	}
	_ = json.Unmarshal(ev.Payload, &p)
	switch ev.Type {
	case domain.EvItemCreated:
		if p.Item != nil && p.Item.Phase != domain.PhaseProposed {
			return "item.create", "decide", "created as " + string(p.Item.Phase) + ", not as a proposal"
		}
		return "item.create", "organize", ""
	case domain.EvItemPhaseChanged:
		if p.Before != nil && (p.Before.Phase == domain.PhaseProposed || p.Before.Phase == domain.PhaseNeedsInput) {
			return "item.transition", "decide", "left " + string(p.Before.Phase)
		}
		return "item.transition", "organize", ""
	case domain.EvItemUpdated, "item.progress_added", domain.EvDecisionRequested, domain.EvDelegationCreated, domain.EvDelegationReported,
		domain.EvMaterialRevised, domain.EvMaterialRevisionProposed, "executor.declared":
		return ev.Type, "organize", ""
	case domain.EvDecisionResolved:
		return "decision.answer", "decide", ""
	case domain.EvCommandRejected: // 旧模型拒绝过的尝试：按它的授权动作重判
		switch p.Action {
		case "answer_decision":
			return "rejected:" + p.Action, "decide", "old code " + p.Code
		case "change_settings":
			return "rejected:" + p.Action, "power", "old code " + p.Code + "; settings are 3a (23) or 3b (20)"
		case "manage_source", "manage_project":
			// 新建类命令的拒绝记在命令名下：能直接认出命令；否则旧事件不足以分辨是一档（改名、重放、积压）还是 3a/3b（启停、登记、密钥）。
			if t, ok := wordsTier[ev.Object]; ok {
				return ev.Object, t, "old code " + p.Code
			}
			return "rejected:" + p.Action, "uncertain", "old code " + p.Code + " on " + ev.Object + "; tier 1 if it was rename/replay/backlog, 3a or 3b if it was enable/update/register/webhook"
		default:
			return "rejected:" + p.Action, "organize", "old code " + p.Code + "; tier 1 now goes through"
		}
	}
	return "", "", ""
}

func inventory(tx *store.Tx, pv *Preview) error {
	_, pol, err := policy.Stored(tx)
	if err != nil {
		return err
	}
	cards, err := executors.Cards(tx)
	if err != nil {
		return err
	}
	for _, c := range cards {
		card := pol.Card(c.ID)
		if pc, ok := pv.Plan.Cards[c.ID]; ok {
			card = pc
		}
		pv.Agents = append(pv.Agents, AgentRow{ID: c.ID, Name: c.Name, Harness: c.Harness, Enabled: c.Enabled, Card: card})
	}
	creds, err := auth.New(nil).List(tx)
	if err != nil {
		return err
	}
	for _, k := range creds {
		after := "unchanged: scope by key kind; tier from its Agent"
		switch {
		case !k.RevokedAt.IsZero():
			after = "revoked; stays revoked"
		case k.Kind == auth.Delegate && k.AgentID == "":
			after = "not bound to an Agent: organize only (assign it to an Agent to follow that Agent's tier)"
		case k.Kind == auth.Delegate || k.Kind == auth.Worker:
			after = "follows Agent " + k.AgentID + " (organize after the switch)"
		}
		pv.Keys = append(pv.Keys, KeyRow{ID: k.ID, Kind: string(k.Kind), Label: k.Label, Harness: k.Harness, AgentID: k.AgentID, Revoked: !k.RevokedAt.IsZero(), After: after})
	}
	var mw struct {
		Default string            `json:"default"`
		Kinds   map[string]string `json:"kinds"`
	}
	if raw, ok := pv.Settings["material_write"]; ok {
		_ = json.Unmarshal(raw, &mw)
	}
	kinds := make([]string, 0, len(mw.Kinds))
	for k := range mw.Kinds {
		kinds = append(kinds, k)
	}
	sort.Strings(kinds)
	for _, k := range append(kinds, "(default)") {
		before := mw.Kinds[k]
		if k == "(default)" {
			before = mw.Default
		}
		after := "new: applies at once; edit by an Agent: waits for you unless the Agent may decide; edit by you: applies at once"
		if k == "guideline" {
			after = "always waits for confirmation (activating a guideline is 3a)"
		}
		pv.Materials = append(pv.Materials, MaterialRow{Kind: k, Before: before, After: after})
	}
	return nil
}

func renderPreview(pv Preview) string {
	var b strings.Builder
	fmt.Fprintf(&b, "switched: %v\n", pv.Switched)
	fmt.Fprintf(&b, "switch plan: revoke %d grants, clear %d questions, drop policy sections %v, write permission cards for %d agents, %d projects with match rules\n",
		len(pv.Plan.Grants), len(pv.Plan.Asks), pv.Plan.LegacyPolicy, len(pv.Plan.Cards), len(pv.Plan.MatchProjects))
	for _, g := range pv.Plan.Grants {
		fmt.Fprintf(&b, "  grant %s agent=%s actions=%v background=%v\n", g.ID, g.Grantee.Agent, g.Actions, g.Grantee.Background)
	}
	fmt.Fprintf(&b, "last %d days, Agent commands under the new tiers: effective %d, pending owner (23) %d, refused (20/4) %d, uncertain %d\n",
		pv.Days, pv.Rejudge["effective"], pv.Rejudge["pending_owner"], pv.Rejudge["refused"], pv.Rejudge["uncertain"])
	for _, d := range pv.Details {
		fmt.Fprintf(&b, "  #%d %s %s %s → %s (%s) %s\n", d.Seq, d.At.Format(time.RFC3339), d.Actor, d.Command, d.Outcome, d.Tier, d.Note)
	}
	for _, a := range pv.Agents {
		fmt.Fprintf(&b, "agent %s %q harness=%s tool=%s groups=%v decide_projects=%v\n", a.ID, a.Name, a.Harness, a.Card.Tool, slices.Sorted(maps.Keys(a.Card.Groups)), a.Card.DecideProjects)
	}
	for _, k := range pv.Keys {
		fmt.Fprintf(&b, "key %s kind=%s label=%q agent=%s revoked=%v → %s\n", k.ID, k.Kind, k.Label, k.AgentID, k.Revoked, k.After)
	}
	for _, m := range pv.Materials {
		fmt.Fprintf(&b, "material %s: %s → %s\n", m.Kind, m.Before, m.After)
	}
	return b.String()
}
