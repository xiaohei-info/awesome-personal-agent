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

// 会话类型：旧模型里凭本人原话的代理写入要求 SessionDirect；现在不参与任何判定，只为读旧事件保留。
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
	// SessionKind 是旧模型里接入方自报的会话类型；新事件不写，只为解码旧事件保留。
	SessionKind string `json:"session_kind,omitempty"`
	Run         string `json:"run,omitempty"`
	OnBehalfOf  string `json:"on_behalf_of,omitempty"`
}

// Basis 是授权依据：策略版本与规则 id（自动动作必填）。
type Basis struct {
	PolicyVersion int    `json:"policy_version"`
	RuleID        string `json:"rule_id,omitempty"`
	// Via 是"凭什么"（§14.3 审计）：owner_web、owner_channel、organize（一档）、allowance（Agent 档位放开）、rule。
	Via string `json:"via,omitempty"`
	// Command、Tier、Agent 由命令管线填：哪条命令、它这次的档位、调用者所属的 Agent。规矩改动清单（3a）与按 Agent 撤回据此取。
	Command string `json:"command,omitempty"`
	Tier    string `json:"tier,omitempty"`
	Agent   string `json:"agent,omitempty"`
}

// Basis.Via 的取值。
const (
	BasisOwnerWeb     = "owner_web"
	BasisOwnerChannel = "owner_channel"
	BasisOrganize     = "organize"
	BasisAllowance    = "allowance"
	BasisRule         = "rule"
)

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
	// Imported 是导入批次 id，只有导入写下的事件才有；RecordedAt 是实际写入账本的时刻，只在与 At 不同时记录，即导入事件（§6.7）。
	// 导入已取消，不再产生新的导入事件；旧账本里带 Imported 的事件仍只进账本与折叠，自动化一律走 eventlog 的"现场"读法。
	Imported   string    `json:"imported,omitempty"`
	RecordedAt time.Time `json:"recorded_at,omitzero"`
}

// 标准事件类型（附录 B.4）；对外事件类型只增不改。
const (
	EvItemCreated                    = "item.created"
	EvItemUpdated                    = "item.updated"
	EvItemPhaseChanged               = "item.phase_changed"
	EvItemDeadlinePassed             = "item.deadline_passed"
	EvItemProgressAdded              = "item.progress_added"
	EvProposalAttached               = "proposal.attached"
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
	EvAgentConfigured                = "agent.configured"
	EvAgentQueued                    = "agent.queued"
	EvRunStarted                     = "run.started"
	EvRunFinished                    = "run.finished"
	EvSourceFailed                   = "source.failed"
	EvImportCompleted                = "import.completed" // 只读的历史值：导入已取消（§7.1），不再写入；旧账本里的这类事件照常读出
	EvHealthAlert                    = "health.alert"
	EvCaptureFailed                  = "inbound.capture_failed" // 内部事件，不在 B.4 的对外稳定集合里：一次对话增量没能捕获，类别与时间，不含内容（诊断页 capture_failures 读它）
	// EvAuthzSwitched 记权限模型第二版的一次性切换（§16.5）：撤了哪些授权单、清了哪些问题、写了哪一版策略。
	// 内部事件，不在 B.4 的对外稳定集合里。
	EvAuthzSwitched = "authz.switched"

	// 登录审计（§14.13）：载荷不含口令。
	EvAuthLogin           = "auth.login"
	EvAuthLoginLocked     = "auth.login_locked"
	EvAuthPasswordChanged = "auth.password_changed"
	EvAuthSessionRevoked  = "auth.session_revoked"
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
	EvAgentConfigured, EvAgentQueued, EvRunStarted, EvRunFinished, EvSourceFailed, EvHealthAlert,
	EvAuthLogin, EvAuthLoginLocked, EvAuthPasswordChanged, EvAuthSessionRevoked,
	EvItemProgressAdded, EvProposalAttached, EvImportCompleted,
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
