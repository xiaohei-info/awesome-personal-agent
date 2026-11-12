package domain

import (
	"encoding/json"
	"time"
)

// Kind 是开放字符串；以下为内置值，能力包可注册新 kind。
type Kind string

const (
	KindTask       Kind = "task"
	KindCommitment Kind = "commitment"
	KindDecision   Kind = "decision"
	KindDelegation Kind = "delegation"
	KindBrief      Kind = "brief"
)

// Priority 用于排序与投递阈值。
type Priority string

const (
	PriorityLow    Priority = "low"
	PriorityNormal Priority = "normal"
	PriorityHigh   Priority = "high"
	PriorityUrgent Priority = "urgent"
)

func (p Priority) Valid() bool {
	switch p {
	case PriorityLow, PriorityNormal, PriorityHigh, PriorityUrgent:
		return true
	}
	return false
}

// WaitingKind 是 waiting_on 的对象类别。
type WaitingKind string

const (
	WaitingHuman    WaitingKind = "human"
	WaitingExecutor WaitingKind = "executor"
	WaitingExternal WaitingKind = "external"
	WaitingTime     WaitingKind = "time"
)

func (k WaitingKind) Valid() bool {
	switch k {
	case WaitingHuman, WaitingExecutor, WaitingExternal, WaitingTime:
		return true
	}
	return false
}

// WaitingOn：waiting 阶段必填。
type WaitingOn struct {
	Kind WaitingKind `json:"kind"`
	Ref  string      `json:"ref"`
}

// Origin 记录事项从哪来。
type Origin struct {
	Source string `json:"source"`
	Actor  string `json:"actor,omitempty"`
	Ref    string `json:"ref,omitempty"`
}

// 内置关系类型；类型字符串开放。
const (
	RelParent      = "parent"
	RelBlocks      = "blocks"
	RelDerivedFrom = "derived_from"
	RelSupersedes  = "supersedes"
	RelRelates     = "relates"

	// 事项与材料之间的内置类型（§6.3）：依据、产出、纪要。边读作"事项 <type> 材料"。
	RelBasis   = "basis"
	RelOutput  = "output"
	RelMinutes = "minutes"
)

// Relation 是事项之间、或事项到材料的带类型的边。
type Relation struct {
	Type string `json:"type"`
	From string `json:"from"`
	To   string `json:"to"`
}

// AskOption 是决策选项；Next 为决定后的下一阶段，空表示 open。
type AskOption struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Next  Phase  `json:"next,omitempty"`
}

// NextPhase 返回选项决定后的阶段，默认 open。
func (o AskOption) NextPhase() Phase {
	if o.Next == "" {
		return PhaseOpen
	}
	return o.Next
}

// AskInput 是调用方可写的决策请求字段。
type AskInput struct {
	Question         string      `json:"question"`
	Options          []AskOption `json:"options,omitempty"`
	Recommend        string      `json:"recommend,omitempty"`
	FreeText         bool        `json:"free_text,omitempty"`
	Deadline         *time.Time  `json:"deadline,omitempty"`
	DefaultOnTimeout string      `json:"default_on_timeout,omitempty"`
	ResumeNote       string      `json:"resume_note,omitempty"`
}

// RiskClassWebOnly 是 web_only 初筛升级后写入 risk_class 的值（§14.4）；authz.WebOnly 认它。
const RiskClassWebOnly = "web_only"

// AskKernelState 只由内核维护，调用方不可写、API 输入类型不含这些字段。
type AskKernelState struct {
	RiskClass        string `json:"risk_class,omitempty"`        // 策略判定，只读
	ScreeningPending bool   `json:"screening_pending,omitempty"` // web_only 初筛未返回时为真
}

// ResolvedViaGrant 是依据授权单代答的决定入口（§14.5）；与渠道名共用 Resolution.ResolvedVia，渠道不能取这个名字。
const ResolvedViaGrant = "grant"

// Resolution 是决定结果。
type Resolution struct {
	Choice      string    `json:"choice,omitempty"`
	Text        string    `json:"text,omitempty"`
	ResolvedVia string    `json:"resolved_via"` // web、chat、grant、渠道名、timeout
	AnsweredBy  string    `json:"answered_by,omitempty"`
	Evidence    string    `json:"evidence,omitempty"`
	Grant       string    `json:"grant,omitempty"` // resolved_via 为 grant 时的授权单 id
	ResolvedAt  time.Time `json:"resolved_at"`
}

// Ask 是任何 kind 的事项都可能带的决策请求（§6.3）。
type Ask struct {
	AskInput
	AskKernelState
	Resolution *Resolution `json:"resolution,omitempty"`
}

// ItemInput 是调用方可写的事项字段；不含内核维护字段、phase 与 version（phase 只经转阶段命令改变）。
type ItemInput struct {
	Kind        Kind            `json:"kind"`
	Title       string          `json:"title"`
	Summary     string          `json:"summary,omitempty"`
	Body        string          `json:"body,omitempty"` // 对 agent 是资料，不是指令
	StatusLabel string          `json:"status_label,omitempty"`
	Priority    Priority        `json:"priority,omitempty"`
	Project     string          `json:"project,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	Assignee    string          `json:"assignee,omitempty"`
	WaitingOn   *WaitingOn      `json:"waiting_on,omitempty"`
	Deadline    *time.Time      `json:"deadline,omitempty"`
	PlannedFor  string          `json:"planned_for,omitempty"` // 计划日期 YYYY-MM-DD（§6.3），不带定时
	Ask         *AskInput       `json:"ask,omitempty"`
	Origin      *Origin         `json:"origin,omitempty"`
	Spec        json.RawMessage `json:"spec,omitempty"`
	Ext         json.RawMessage `json:"ext,omitempty"` // 内核永不读取
}

// ItemKernelState 只由内核维护；子事项的 Unattended 继承自后台运行。
// CreatedAt、UpdatedAt 是该事项首条与最后一条事项事件的时间，由事项中心维护，不进事件快照。
type ItemKernelState struct {
	ChainDepth int       `json:"chain_depth"`
	Unattended bool      `json:"unattended,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
}

// Item 是一切事务的统一表示（§6.3）。
type Item struct {
	ID    string `json:"id"`
	Phase Phase  `json:"phase"`
	ItemKernelState
	Version     int64           `json:"version"` // 乐观并发
	Kind        Kind            `json:"kind"`
	Title       string          `json:"title"`
	Summary     string          `json:"summary,omitempty"`
	Body        string          `json:"body,omitempty"`
	StatusLabel string          `json:"status_label,omitempty"`
	Priority    Priority        `json:"priority,omitempty"`
	Project     string          `json:"project,omitempty"`
	Tags        []string        `json:"tags,omitempty"`
	Assignee    string          `json:"assignee,omitempty"`
	WaitingOn   *WaitingOn      `json:"waiting_on,omitempty"`
	Deadline    *time.Time      `json:"deadline,omitempty"`    // 事项自身的截止，与 ask.deadline（决策超时）无关
	PlannedFor  string          `json:"planned_for,omitempty"` // 计划日期 YYYY-MM-DD（§6.3），不带定时、不产生提醒
	Ask         *Ask            `json:"ask,omitempty"`
	Origin      *Origin         `json:"origin,omitempty"`
	Spec        json.RawMessage `json:"spec,omitempty"`
	Ext         json.RawMessage `json:"ext,omitempty"`
}

// CheckEntry 校验 item 以 to 阶段（附 reason、证据）的进入约束（§6.4、§6.14 第 3 条）。
// 阶段间允许任意转移，这里只校验进入约束；"允许的转移"配置属于命令层。
func CheckEntry(it Item, to Phase, reason string, hasEvidence bool) *Error {
	switch {
	case !to.Valid():
		return NewError(CodeInvalidRequest, "未知阶段", "阶段只能是 proposed、open、waiting、needs_input、done、dropped")
	case reason == "":
		return NewError(CodeInvalidRequest, "阶段转移必须带 reason", "补充 reason 后重试")
	case to == PhaseWaiting && it.WaitingOn == nil:
		return NewError(CodeInvalidRequest, "waiting 阶段必须有 waiting_on", "先设置 waiting_on")
	case to == PhaseNeedsInput && it.Ask == nil:
		return NewError(CodeInvalidRequest, "needs_input 阶段必须有 ask", "先写入决策请求")
	case to == PhaseDone && it.Kind == KindDelegation && !hasEvidence:
		return NewError(CodeInvalidRequest, "delegation 进入 done 必须带证据", "随回执提交证据")
	}
	return nil
}

// Progress 是事项上一条追加不改的进展（§6.3）：文字、所指日期（可空，不晚于记录当天）、记录时刻与操作者。
type Progress struct {
	ID    string    `json:"id"`
	Text  string    `json:"text"`
	On    string    `json:"on,omitempty"`
	Actor string    `json:"actor"`
	At    time.Time `json:"at"`
}
