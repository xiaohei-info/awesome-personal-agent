package why

import (
	"encoding/json"
	"slices"
	"sort"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 规矩改动清单（§14.3 3a 的兜底）：3a 改日常的每一次改动，带前值、后值，能撤回的给出逆操作。
// 来源是两类事件：命令管线记了 basis.tier=rules 的事件，以及改到 3a 段的 policy.changed。

// 撤不回的原因（稳定码，Web 按词表渲染）。
const (
	RevertRunStarted  = "run_started"    // 已拉起的运行撤不回
	RevertDataPurged  = "data_purged"    // 已清除的数据撤不回
	RevertSuperseded  = "superseded"     // 之后又被改过：撤回会盖掉后来的改动
	RevertNotPossible = "not_revertible" // 没有记下前值，只能去设置页改回
)

// RuleChange 是清单的一行。Before、After 是这次改动涉及的值（策略段为 段名→值）。
type RuleChange struct {
	Seq      int64           `json:"seq"`
	At       time.Time       `json:"at"`
	Actor    domain.Actor    `json:"actor"`
	Agent    string          `json:"agent,omitempty"`
	Command  string          `json:"command"` // 命令名；策略段改动为 "policy"
	Object   string          `json:"object"`
	Sections []string        `json:"sections,omitempty"`
	Before   json.RawMessage `json:"before,omitempty"`
	After    json.RawMessage `json:"after,omitempty"`
	Revert   RevertState     `json:"revert"`
}

// RevertState：OK 为真可一键撤回；否则 Why 是撤不回的原因码。
type RevertState struct {
	OK  bool   `json:"ok"`
	Why string `json:"why,omitempty"`
}

// Reversal 是一次撤回要执行的逆操作：命令（以 Web 本人身份）或把策略的若干段换回第 From 版的值。
type Reversal struct {
	Command  string
	Input    json.RawMessage
	Sections []string
	From     int
}

// PolicyCommand 是策略段改动在清单里的命令名。
const PolicyCommand = "policy"

// state 是每条可撤回命令"之后又被改过"的判据：同一对象上更晚出现这些类型的事件。
var state = map[string][]string{
	"source.set_enabled":           {"source.enabled", "source.disabled"},
	"source.rename":                {"source.renamed"},
	"source.set_content_retention": {"source.updated"},
	"instance.create":              {"instance.toggled", "instance.updated"},
	"instance.toggle":              {"instance.toggled"},
	"instance.update":              {"instance.updated"},
	"rule.set_mode":                {domain.EvRuleModeChanged},
}

// RuleChanges 列出 before（0 不限）之前最近的至多 limit 条 3a 改动，最新的在前；agent 非空时只列该 Agent 做的。
func RuleChanges(tx *store.Tx, agent string, before int64, limit int) ([]RuleChange, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	cmds, err := eventlog.QueryBefore(tx, eventlog.Filter{Tier: "rules"}, before, limit)
	if err != nil {
		return nil, err
	}
	pols, err := eventlog.QueryBefore(tx, eventlog.Filter{Type: domain.EvPolicyChanged}, before, limit)
	if err != nil {
		return nil, err
	}
	var out []RuleChange
	seen := map[int64]bool{} // 设置页写的 policy.changed 也带 basis.tier，两路都会取到
	for _, e := range append(cmds, pols...) {
		ev := e.Event
		if seen[ev.Seq] {
			continue
		}
		seen[ev.Seq] = true
		if agent != "" && (ev.Basis == nil || ev.Basis.Agent != agent) {
			continue
		}
		var rc RuleChange
		var ok bool
		if ev.Type == domain.EvPolicyChanged {
			rc, ok, err = policyChange(tx, ev)
		} else {
			rc, ok, err = commandChange(tx, ev)
		}
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, rc)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq > out[j].Seq })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// ReverseOf 给出 seq 那次改动的清单行与逆操作；撤不回时 Reversal 为空、行上写明原因。
func ReverseOf(tx *store.Tx, seq int64) (RuleChange, *Reversal, error) {
	es, err := eventlog.Read(tx, seq-1, 1)
	if err != nil {
		return RuleChange{}, nil, err
	}
	if len(es) == 0 || es[0].Event.Seq != seq {
		return RuleChange{}, nil, domain.NewError(domain.CodeNotFound, "no such change", "list rule changes and retry")
	}
	ev := es[0].Event
	var (
		rc RuleChange
		ok bool
	)
	if ev.Type == domain.EvPolicyChanged {
		rc, ok, err = policyChange(tx, ev)
	} else if ev.Basis != nil && ev.Basis.Tier == "rules" {
		rc, ok, err = commandChange(tx, ev)
	}
	if err != nil {
		return rc, nil, err
	}
	if !ok {
		return rc, nil, domain.NewError(domain.CodeNotFound, "event is not an everyday-rule change", "list rule changes and retry")
	}
	if !rc.Revert.OK {
		return rc, nil, nil
	}
	if rc.Command == PolicyCommand {
		var p struct {
			Previous int `json:"previous"`
		}
		_ = json.Unmarshal(ev.Payload, &p)
		return rc, &Reversal{Sections: rc.Sections, From: p.Previous}, nil
	}
	in, err := inverse(rc, ev)
	return rc, &Reversal{Command: inverseCommand(rc.Command), Input: in}, err
}

func base(ev domain.Event, command string) RuleChange {
	rc := RuleChange{Seq: ev.Seq, At: ev.At, Actor: ev.Actor, Command: command, Object: ev.Object}
	if ev.Basis != nil {
		rc.Agent = ev.Basis.Agent
	}
	return rc
}

// policyChange：只看改到的 3a 段；前值取 payload.previous 那一版，后值取这一版。没有 3a 段的不进清单。
func policyChange(tx *store.Tx, ev domain.Event) (RuleChange, bool, error) {
	var p struct {
		Version  int      `json:"version"`
		Previous int      `json:"previous"`
		Sections []string `json:"sections"`
	}
	if json.Unmarshal(ev.Payload, &p) != nil {
		return RuleChange{}, false, nil
	}
	var keys []string
	for _, k := range p.Sections {
		if slices.Contains(policy.RulesSections, k) {
			keys = append(keys, k)
		}
	}
	if len(keys) == 0 {
		return RuleChange{}, false, nil
	}
	pick := func(v int) (map[string]json.RawMessage, error) {
		pol, err := policy.At(tx, v)
		if err != nil {
			return nil, err
		}
		all, err := policy.Sections(pol)
		out := map[string]json.RawMessage{}
		for _, k := range keys {
			out[k] = all[k]
		}
		return out, err
	}
	before, err := pick(p.Previous)
	if err != nil {
		return RuleChange{}, false, err
	}
	after, err := pick(p.Version)
	if err != nil {
		return RuleChange{}, false, err
	}
	_, cur, err := policy.Stored(tx)
	if err != nil {
		return RuleChange{}, false, err
	}
	now, err := policy.Sections(cur)
	if err != nil {
		return RuleChange{}, false, err
	}
	rc := base(ev, PolicyCommand)
	rc.Sections = keys
	rc.Before, _ = json.Marshal(before)
	rc.After, _ = json.Marshal(after)
	rc.Revert = RevertState{OK: true}
	for _, k := range keys {
		if string(now[k]) != string(after[k]) {
			rc.Revert = RevertState{Why: RevertSuperseded}
		}
	}
	return rc, true, nil
}

func commandChange(tx *store.Tx, ev domain.Event) (RuleChange, bool, error) {
	if ev.Basis == nil || ev.Basis.Command == "" {
		return RuleChange{}, false, nil
	}
	rc := base(ev, ev.Basis.Command)
	rc.After = ev.Payload
	switch rc.Command {
	case "instance.run", "item.start":
		rc.Revert = RevertState{Why: RevertRunStarted}
		return rc, true, nil
	}
	types, revertible := state[rc.Command]
	if !revertible {
		rc.Revert = RevertState{Why: RevertNotPossible}
		return rc, true, nil
	}
	later, err := laterOn(tx, ev, types)
	if err != nil {
		return rc, false, err
	}
	if later {
		rc.Revert = RevertState{Why: RevertSuperseded}
		return rc, true, nil
	}
	prev, err := previousSnapshot(tx, ev)
	if err != nil {
		return rc, false, err
	}
	rc.Before = prev
	rc.Revert = RevertState{OK: true}
	if rc.Command == "source.set_content_retention" || rc.Command == "instance.update" {
		if prev == nil {
			rc.Revert = RevertState{Why: RevertNotPossible}
		}
	}
	return rc, true, nil
}

// laterOn：同一对象上 ev 之后是否又有 types 里的事件。
func laterOn(tx *store.Tx, ev domain.Event, types []string) (bool, error) {
	es, err := eventlog.ReadObject(tx, ev.Object)
	if err != nil {
		return false, err
	}
	for _, e := range es {
		if e.Event.Seq > ev.Seq && slices.Contains(types, e.Event.Type) {
			return true, nil
		}
	}
	return false, nil
}

// previousSnapshot：同一对象上 ev 之前最近一条与 ev 同族（同为来源快照或实例快照）的事件载荷，作为前值。
func previousSnapshot(tx *store.Tx, ev domain.Event) (json.RawMessage, error) {
	family := []string{"source.registered", "source.updated", "source.enabled", "source.disabled"}
	switch ev.Type {
	case "instance.created", "instance.updated", "instance.toggled":
		family = []string{"instance.created", "instance.updated", "instance.toggled"}
	case "source.renamed", domain.EvRuleModeChanged:
		return nil, nil // 载荷自己带前值（previous / from）
	}
	es, err := eventlog.ReadObject(tx, ev.Object)
	if err != nil {
		return nil, err
	}
	var prev json.RawMessage
	for _, e := range es {
		if e.Event.Seq < ev.Seq && slices.Contains(family, e.Event.Type) {
			prev = e.Event.Payload
		}
	}
	return prev, nil
}

func inverseCommand(c string) string {
	if c == "instance.create" {
		return "instance.toggle" // 撤回新建 = 停用
	}
	return c
}

// inverse 是逆操作的命令输入。
func inverse(rc RuleChange, ev domain.Event) (json.RawMessage, error) {
	var after, before map[string]json.RawMessage
	_ = json.Unmarshal(ev.Payload, &after)
	_ = json.Unmarshal(rc.Before, &before)
	id := ev.Object
	var in any
	switch rc.Command {
	case "source.set_enabled":
		var on bool
		_ = json.Unmarshal(after["enabled"], &on)
		in = map[string]any{"id": id, "enabled": !on}
	case "instance.toggle":
		var on bool
		_ = json.Unmarshal(after["enabled"], &on)
		in = map[string]any{"id": id, "enabled": !on}
	case "instance.create":
		in = map[string]any{"id": id, "enabled": false}
	case "instance.update":
		project := ""
		_ = json.Unmarshal(before["project"], &project)
		in = map[string]any{"id": id, "params": before["params"], "project": project}
	case "source.set_content_retention":
		days := before["content_retention_days"]
		if days == nil {
			days = json.RawMessage("null")
		}
		in = map[string]any{"id": id, "days": days}
	case "source.rename":
		in = map[string]any{"id": id, "name": after["previous"]}
	case "rule.set_mode":
		in = map[string]any{"rule": after["rule"], "mode": after["from"]}
	}
	return json.Marshal(in)
}
