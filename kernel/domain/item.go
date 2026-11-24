package domain

import (
	"encoding/json"
	"strings"
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

// OriginInstancePrefix 是例行任务实例建的事项的 origin.source 前缀（instance:<实例 id>，与对话捕获的 capture:<项目> 同为内部来源）；
// 同一实例建的事项算同一序列（§6.17）。
const OriginInstancePrefix = "instance:"

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

// AskInput 是调用方可写的决策请求字段。Code、Params 只由内核身份写入（items、decisions 拒绝其他调用方带它们，§6.3）。
type AskInput struct {
	Question string `json:"question"`
	// Code 是内核生成的问题的稳定类别码（AskCode），Params 是它的参数；Web 按词表渲染，Question 是给 CLI、渠道与旧版界面的原文兜底。
	Code             AskCode           `json:"code,omitempty"`
	Params           map[string]string `json:"params,omitempty"`
	Options          []AskOption       `json:"options,omitempty"`
	Recommend        string            `json:"recommend,omitempty"`
	FreeText         bool              `json:"free_text,omitempty"`
	Deadline         *time.Time        `json:"deadline,omitempty"`
	DefaultOnTimeout string            `json:"default_on_timeout,omitempty"`
	ResumeNote       string            `json:"resume_note,omitempty"`
}

// CheckAsk 是问题的最小语义（§6.14 第 20 条）：问题非空，选项 id 与名字非空且不重复，并且本人有真实的选择——
// 至少两个选项，或允许写别的想法。只有一个选项又不能写别的想法的「问题」不需要回答，不能构造。
func CheckAsk(a AskInput) *Error {
	if strings.TrimSpace(a.Question) == "" {
		return NewError(CodeInvalidRequest, "ask.question is required", "fix the request and retry")
	}
	ids := map[string]bool{}
	for _, o := range a.Options {
		if strings.TrimSpace(o.ID) == "" || strings.TrimSpace(o.Label) == "" {
			return NewError(CodeInvalidRequest, "every option needs an id and a label", "fix the request and retry")
		}
		if ids[o.ID] {
			return NewError(CodeInvalidRequest, "duplicate option id "+o.ID, "fix the request and retry")
		}
		ids[o.ID] = true
	}
	if len(a.Options) < 2 && !a.FreeText {
		return NewError(CodeInvalidRequest, "ask needs at least two options or free_text: a question with one possible answer is not a decision",
			"offer at least two real choices, allow free_text, or record an event instead of asking")
	}
	return nil
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
	PlannedFor  string          `json:"planned_for,omitempty"`  // 计划日期 YYYY-MM-DD（§6.3），不带定时
	PlannedSlot string          `json:"planned_slot,omitempty"` // 时段 HH:MM - HH:MM（§6.3），落在计划日期那天
	DailyAt     string          `json:"daily_at,omitempty"`     // 每天到点提醒的钟点 HH:MM（§6.3），与计划日期互斥
	Ask         *AskInput       `json:"ask,omitempty"`
	Origin      *Origin         `json:"origin,omitempty"`
	Proposal    *Proposal       `json:"proposal,omitempty"` // 建议信息，由入站流水线写入（§6.3）
	Spec        json.RawMessage `json:"spec,omitempty"`
	Ext         json.RawMessage `json:"ext,omitempty"` // 内核永不读取
}

// Proposal 是 proposed 事项上的建议信息（§6.3、附录 B.2）：起草给出的推荐处置、是否把握低、来自哪些来源事件。
// 推荐只是建议流里的默认选项，不触发任何处置。
type Proposal struct {
	Recommend     string   `json:"recommend,omitempty"` // accept、attach、dismiss；空表示没有推荐
	AttachTo      string   `json:"attach_to,omitempty"` // Recommend 为 attach 时的目标事项
	Reason        string   `json:"reason,omitempty"`
	LowConfidence bool     `json:"low_confidence"`
	Sources       []string `json:"sources,omitempty"`   // 来源事件 id
	MergeKey      string   `json:"merge_key,omitempty"` // 合并键（§9.3）：同一件事的后续事件凭它找到这条建议
	Urgent        *bool    `json:"urgent,omitempty"`    // 起草或抽取对"今天不处理是否有后果"的判断；缺失表示模型没给，只供紧急提醒使用（§9.3）
	// Merges 是并入这条建议的来源事件各自的合并留痕（§9.3 跨来源合并）：并入时的候选内容与依据，拆分时据此另建一条。
	Merges []ProposalMerge `json:"merges,omitempty"`
	// Similar 是只提示、没有自动合并的相似建议（判断方拿不准或不可用）。
	Similar []ProposalSimilar `json:"similar,omitempty"`
	// NotSame 是本人拆开过的建议：与它们不再自动合并、也不再提示。
	NotSame []string `json:"not_same,omitempty"`
	// SplitFrom 是这条建议从哪条建议里拆出来的。
	SplitFrom string `json:"split_from,omitempty"`
	// Sweep 是跨来源合并的批量归类（§9.3）：新建议记 pending，归类后记结论与依据。升级前的建议没有，不回填。
	Sweep *ProposalSweep `json:"sweep,omitempty"`
	// Absorbed 是归类折叠进这一条的建议本身的溯源（§9.3）：折叠时不论那条名下有没有消息（迁移来的建议常没有）都记一条，撤销时删掉。
	Absorbed []ProposalAbsorbed `json:"absorbed,omitempty"`
	// Integrated 只在批内合并的载体上：几条同一件事的建议整合成这一条时，整合前它自己的标题与摘要、整合后的文本与一起整合的成员（§9.3），撤销时据此还原。
	Integrated *ProposalIntegrated `json:"integrated,omitempty"`
}

// ProposalIntegrated 是一次批内整合：Title、Summary 是载体整合前的原文，ToTitle、ToSummary 是写入的整合文本，Members 含载体自己。
type ProposalIntegrated struct {
	Title     string    `json:"title"`
	Summary   string    `json:"summary,omitempty"`
	ToTitle   string    `json:"to_title"`
	ToSummary string    `json:"to_summary,omitempty"`
	Body      string    `json:"body,omitempty"`    // 整合前载体的正文（撤销时还原）
	ToBody    string    `json:"to_body,omitempty"` // 整合后的正文：要点清单 + 按成员附的来源原文
	Members   []string  `json:"members"`
	Batch     string    `json:"batch,omitempty"`
	Run       string    `json:"run,omitempty"`
	At        time.Time `json:"at"`
}

// ProposalAbsorbed 是一条被归类折叠进来的建议：它的 id、原始标题、摘要与正文、origin 与项目，折叠的结论、批次与时刻。
type ProposalAbsorbed struct {
	Item    string    `json:"item"`
	Title   string    `json:"title"`
	Source  string    `json:"source,omitempty"` // 它的 origin.source
	Ref     string    `json:"ref,omitempty"`    // 它的 origin.ref
	Project string    `json:"project,omitempty"`
	Summary string    `json:"summary,omitempty"` // 它的原始摘要
	Body    string    `json:"body,omitempty"`    // 它折叠时的原始正文（全文，不截断；它自己身上也还留着，折叠只改阶段）
	Events  []string  `json:"events,omitempty"`  // 随折叠搬到去处的、它名下的来源事件
	Created time.Time `json:"created_at"`        // 它自己建立的时刻
	Action  string    `json:"action"`
	Batch   string    `json:"batch,omitempty"`
	At      time.Time `json:"at"` // 折叠的时刻
}

// 跨来源合并的归类结论（ProposalSweep.Action）。
const (
	SweepKeep      = "keep"      // 留着：照常作为建议
	SweepMerge     = "merge"     // 合并成一条建议：来源并入去处，这条折叠
	SweepAttach    = "attach"    // 并入已有事项：去处追加补充进展，这条折叠
	SweepDuplicate = "duplicate" // 判定重复：这条折叠
	SweepUndone    = "undone"    // 本人撤销了折叠
)

// 第 6 层校验（落账前）拒绝一条折叠结论的类别码（ProposalSweep.Rejected）：被拒的一律留着。
const (
	RejectCrossProject   = "cross_project"   // 去处（或整合的另一条）属于另一个项目
	RejectOtherChange    = "other_change"    // 同一仓库的不同 PR 号
	RejectNotSame        = "not_same"        // 本人拆开过
	RejectNotCandidate   = "not_candidate"   // 去处不在这一条当时的候选里，或合并项不存在、没列这一条
	RejectFactsMissing   = "facts_missing"   // 整合后的文本或正文缺成员必须保留的事实（金额、日期与时间、单据类编号、明确的数量）
	RejectActionMismatch = "action_mismatch" // 结论与去处的状态不相配（attach 要进行中，duplicate 要已完成）
	RejectCombine        = "combine_invalid" // 整合不成立：只剩一条、跨组、标题摘要或正文为空
	RejectTargetChanged  = "target_changed"  // 落账时去处已经变了
	RejectNotLanded      = "not_landed"      // 落账时整合没有落成
)

// ProposalSweep 是一条建议在跨来源合并里的归类记录。State 为 pending（待归类）或 done。
// By 是判断方：generate（写作模型）或 rule（规则，没有调模型）。Run 是本批那次模型调用的 run id，Size 与 Chars 是同批条数与送出字数；
// Deferred 是本批因资料超出预算、砍半后留到下一轮的条数（0 为没砍半）。
type ProposalSweep struct {
	State    string     `json:"state"`
	Batch    string     `json:"batch,omitempty"`
	Action   string     `json:"action,omitempty"`
	Target   string     `json:"target,omitempty"`
	Why      string     `json:"why,omitempty"`
	By       string     `json:"by,omitempty"`
	Run      string     `json:"run,omitempty"`
	Size     int        `json:"size,omitempty"`
	Chars    int        `json:"chars,omitempty"`
	Deferred int        `json:"deferred,omitempty"`
	At       *time.Time `json:"at,omitempty"`
	// Rejected 是第 6 层校验拒绝模型结论的类别码（domain.Reject*），被拒后这条留着；空为没有被拒。
	Rejected string `json:"rejected,omitempty"`
	// Group 与 Topic 是归类时模型给的分组（批号/G<k>）与主题：建议页按它把同组的建议放在一起，可整组处理。
	Group string `json:"group,omitempty"`
	Topic string `json:"topic,omitempty"`
	// Backfill 是本人补打的待归类标记（apa merge mark-pending）：维护循环不自动归类它，只由 apa merge run --apply 处理；归类后保留。
	Backfill *SweepBackfill `json:"backfill,omitempty"`
	// Retry 记这条建议被 apa merge run --retry-rejected 重新纳入过：上一次判定的批号、拒绝码与原因。有它说明重判过一次，不再重复纳入。
	Retry *SweepRetry `json:"retry,omitempty"`
}

// SweepRetry 是一条被退回的建议重新纳入待归类时留下的来源：Of 是上一次判定的批号，Rejected 与 Why 是那次的拒绝码与原因，At 是纳入时刻。
type SweepRetry struct {
	Of       string    `json:"of"`
	Rejected string    `json:"rejected"`
	Why      string    `json:"why,omitempty"`
	At       time.Time `json:"at"`
}

// SweepBackfill 记谁、何时补打的待归类标记。
type SweepBackfill struct {
	By string    `json:"by"`
	At time.Time `json:"at"`
}

// Folded 为真表示这条建议被归类折叠进了别处（并入、合并或判定重复），且没有被撤销。
func (s *ProposalSweep) Folded() bool {
	return s != nil && (s.Action == SweepMerge || s.Action == SweepAttach || s.Action == SweepDuplicate)
}

// ProposalMerge 是一次合并的留痕。By 取 merge_key（合并键相同）、generate（批量归类里写作模型判断）；
// 第一版逐条合并的旧留痕还有 classifier、chat。Item 是批量归类时被折叠的那条建议（撤销时据此恢复），旧留痕没有。
type ProposalMerge struct {
	Event    string    `json:"event"`
	Source   string    `json:"source"`
	Title    string    `json:"title"`
	Summary  string    `json:"summary,omitempty"`
	Kind     Kind      `json:"kind,omitempty"`
	Project  string    `json:"project,omitempty"`
	By       string    `json:"by"`
	Score    float64   `json:"score,omitempty"`    // 相似度（0 到 1）；合并键相同时为 0
	Judgment string    `json:"judgment,omitempty"` // 判断记录 id
	Why      string    `json:"why,omitempty"`      // 判定理由
	Batch    string    `json:"batch,omitempty"`
	Run      string    `json:"run,omitempty"`
	Item     string    `json:"item,omitempty"`
	At       time.Time `json:"at"`
}

// ProposalSimilar 是一条只提示的相似建议。
type ProposalSimilar struct {
	Item     string  `json:"item"`
	Score    float64 `json:"score"`
	Judgment string  `json:"judgment,omitempty"`
}

// ItemKernelState 只由内核维护；子事项的 Unattended 继承自后台运行。
// CreatedAt、UpdatedAt 是该事项首条与最后一条事项事件的时间，由事项中心维护，不进事件快照。
type ItemKernelState struct {
	ChainDepth int       `json:"chain_depth"`
	Unattended bool      `json:"unattended,omitempty"`
	CreatedAt  time.Time `json:"created_at,omitzero"`
	UpdatedAt  time.Time `json:"updated_at,omitzero"`
	// Imported 只有导入写下的事项才有（导入已取消，只读的历史值）：导入批次与实际写入时刻。跟进与久未推进以它和最后一次非导入变更中较晚的一个起算（§7.1、§7.3）。
	Imported *ImportInfo `json:"imported,omitempty"`
}

// ImportInfo 是事项的导入信息（§6.3）：批次号与实际写入账本的时刻。
type ImportInfo struct {
	Batch string    `json:"batch"`
	At    time.Time `json:"at"`
}

// Item 是一切事务的统一表示（§6.3）。
type Item struct {
	PreferredAgentID string `json:"preferred_agent_id,omitempty"`
	ID               string `json:"id"`
	Phase            Phase  `json:"phase"`
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
	Deadline    *time.Time      `json:"deadline,omitempty"`     // 事项自身的截止，与 ask.deadline（决策超时）无关
	PlannedFor  string          `json:"planned_for,omitempty"`  // 计划日期 YYYY-MM-DD（§6.3），不带定时、不产生提醒
	PlannedSlot string          `json:"planned_slot,omitempty"` // 时段 HH:MM - HH:MM（§6.3），有它必须有计划日期；开启主动提醒时按它提醒开始与截止（§7.4）
	DailyAt     string          `json:"daily_at,omitempty"`     // 每天到点提醒的钟点 HH:MM（§6.3），与计划日期互斥；只在开启主动提醒时产生提醒
	Ask         *Ask            `json:"ask,omitempty"`
	Origin      *Origin         `json:"origin,omitempty"`
	Proposal    *Proposal       `json:"proposal,omitempty"`
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
	// Precision 为 day 表示只知道日期（导入时给了当天的一个时刻），界面只显示到日（§7.1）；空为精确时刻。
	Precision string `json:"precision,omitempty"`
}

// ParseSlot 解析时段 `HH:MM - HH:MM`（§6.3），返回起止分钟数。只认这一种写法：两位补零、空格连字符空格；
// 开始 00:00–23:59，结束 00:01–24:00 且晚于开始（不跨午夜）。
func ParseSlot(s string) (start, end int, ok bool) {
	if len(s) != 13 || s[5:8] != " - " {
		return 0, 0, false
	}
	hm := func(v string) (int, bool) {
		if v[2] != ':' || !digits(v[:2]) || !digits(v[3:]) {
			return 0, false
		}
		h, m := int(v[0]-'0')*10+int(v[1]-'0'), int(v[3]-'0')*10+int(v[4]-'0')
		return h*60 + m, m < 60 && (h < 24 || h == 24 && m == 0)
	}
	start, ok1 := hm(s[:5])
	end, ok2 := hm(s[8:])
	return start, end, ok1 && ok2 && start < 24*60 && end > start
}

// ParseClock 解析钟点 `HH:MM`（§6.3 daily_at），两位补零，00:00–23:59，返回当天的分钟数。
func ParseClock(s string) (int, bool) {
	if len(s) != 5 || s[2] != ':' || !digits(s[:2]) || !digits(s[3:]) {
		return 0, false
	}
	h, m := int(s[0]-'0')*10+int(s[1]-'0'), int(s[3]-'0')*10+int(s[4]-'0')
	return h*60 + m, h < 24 && m < 60
}

func digits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}
