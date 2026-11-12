package domain

import (
	"encoding/json"
	"time"
)

// ActorType 区分操作者类别。
type ActorType string

const (
	ActorHuman  ActorType = "human"
	ActorAgent  ActorType = "agent"
	ActorSystem ActorType = "system"
)

// Via 是操作入口。
type Via string

const (
	ViaWeb    Via = "web"
	ViaCLI    Via = "cli"
	ViaClock  Via = "clock"
	ViaRule   Via = "rule"
	ViaSource Via = "source"
	ViaExt    Via = "ext"
)

// 会话类型（§14.5）：只有 SessionDirect 时，内核接受凭本人原话的代理写入。
const (
	SessionDirect  = "direct"
	SessionGroup   = "group"
	SessionUnknown = "unknown"
)

// Actor 是变更的操作者（§6.7）。
type Actor struct {
	Type      ActorType `json:"type"`
	Principal string    `json:"principal"`
	Via       Via       `json:"via"`
	Harness   string    `json:"harness,omitempty"`
	Session   string    `json:"session,omitempty"`
	// SessionKind 是接入方自报的会话类型（SessionDirect、SessionGroup、SessionUnknown；§14.5）；空表示没有上报。
	SessionKind string `json:"session_kind,omitempty"`
	Run         string `json:"run,omitempty"`
	OnBehalfOf  string `json:"on_behalf_of,omitempty"`
}

// Basis 是授权依据：策略版本与规则 id（自动动作必填）。
type Basis struct {
	PolicyVersion int    `json:"policy_version"`
	RuleID        string `json:"rule_id,omitempty"`
}

// Event 是账本的追加日志，也是对外事件流（§6.7）。
type Event struct {
	Seq         int64           `json:"seq"` // 单调递增，也是订阅游标
	Type        string          `json:"type"`
	Object      string          `json:"object"`
	Actor       Actor           `json:"actor"`
	Reason      string          `json:"reason,omitempty"`
	Payload     json.RawMessage `json:"payload"` // 足以重放这次变更
	ChainDepth  int             `json:"chain_depth"`
	Basis       *Basis          `json:"basis,omitempty"`
	ResolvedVia string          `json:"resolved_via,omitempty"`
	Evidence    string          `json:"evidence,omitempty"`
	Grant       string          `json:"grant,omitempty"` // 依据授权单写入时的授权单 id（§6.7、§14.5）
	At          time.Time       `json:"at"`
}

// 标准事件类型（附录 B.4）；对外事件类型只增不改。
const (
	EvItemCreated                    = "item.created"
	EvItemUpdated                    = "item.updated"
	EvItemPhaseChanged               = "item.phase_changed"
	EvItemDeadlinePassed             = "item.deadline_passed"
	EvItemProgressAdded              = "item.progress_added"
	EvDecisionRequested              = "decision.requested"
	EvDecisionResolved               = "decision.resolved"
	EvDelegationCreated              = "delegation.created"
	EvDelegationReported             = "delegation.reported"
	EvCommandRejected                = "command.rejected"
	EvMaterialRevised                = "material.revised"
	EvMaterialRevisionProposed       = "material.revision_proposed"
	EvGuidelineActivated             = "guideline.activated"
	EvProjectUpdated                 = "project.updated"
	EvProjectAllowedExecutorsChanged = "project.allowed_executors_changed"
	EvRuleModeChanged                = "rule.mode_changed"
	EvAllowlistChanged               = "allowlist.changed"
	EvPolicyChanged                  = "policy.changed"
	EvGrantIssued                    = "grant.issued"
	EvGrantRevoked                   = "grant.revoked"
	EvRunStarted                     = "run.started"
	EvRunFinished                    = "run.finished"
	EvSourceFailed                   = "source.failed"
	EvHealthAlert                    = "health.alert"
)

// RegisteredEventTypes 是对外开放的稳定事件类型表，顺序同附录 B.4。
var RegisteredEventTypes = []string{
	EvItemCreated, EvItemUpdated, EvItemPhaseChanged, EvItemDeadlinePassed,
	EvDecisionRequested, EvDecisionResolved,
	EvDelegationCreated, EvDelegationReported,
	EvCommandRejected,
	EvMaterialRevised, EvMaterialRevisionProposed, EvGuidelineActivated,
	EvProjectUpdated, EvProjectAllowedExecutorsChanged,
	EvRuleModeChanged, EvAllowlistChanged, EvPolicyChanged, EvGrantIssued, EvGrantRevoked,
	EvRunStarted, EvRunFinished, EvSourceFailed, EvHealthAlert,
	EvItemProgressAdded,
}

var registered = func() map[string]bool {
	m := make(map[string]bool, len(RegisteredEventTypes))
	for _, t := range RegisteredEventTypes {
		m[t] = true
	}
	return m
}()

// IsRegisteredEventType 判断事件类型是否对外开放（订阅只可见登记类型）。
func IsRegisteredEventType(t string) bool { return registered[t] }
