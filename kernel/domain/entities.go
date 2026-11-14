package domain

import (
	"encoding/json"
	"time"
)

// ---- Project（§6.5） ----

type ProjectStatus string

const (
	ProjectActive   ProjectStatus = "active"
	ProjectPaused   ProjectStatus = "paused"
	ProjectArchived ProjectStatus = "archived"
)

// Checkout 是 repo 的一个本地检出；Host 必须等于该机主机名。
type Checkout struct {
	Host string `json:"host"`
	Path string `json:"path"`
}

// Repo 是项目的普通列表项，不是独立实体。
type Repo struct {
	Remote    string     `json:"remote"`
	Checkouts []Checkout `json:"checkouts,omitempty"`
}

type Project struct {
	ID     string        `json:"id"`
	Name   string        `json:"name"`
	Intro  string        `json:"intro,omitempty"`
	Tags   []string      `json:"tags,omitempty"`
	Status ProjectStatus `json:"status"`
	Repos  []Repo        `json:"repos,omitempty"`
	// nil 不限制；空切片只能指派给本人（§14.7）。只在 Web 设置。
	AllowedExecutors []string `json:"allowed_executors"`
	Capture          bool     `json:"capture,omitempty"`
	// CaptureOutbound 是对话增量送外部端点的范围（full、excerpt、meta、none）；nil 未选择，没有缺省值（§6.5、§14.14）。
	CaptureOutbound *string `json:"capture_outbound"`
}

// LocalCheckout 返回项目在 host 上的第一个检出（§12.6：Host 等于该机主机名才算本机有检出）；没有时返回 nil。
func (p Project) LocalCheckout(host string) *Checkout {
	for _, r := range p.Repos {
		for i := range r.Checkouts {
			if r.Checkouts[i].Host == host {
				return &r.Checkouts[i]
			}
		}
	}
	return nil
}

// ExecutorAllowed 按 §6.14 第 5 条判断指派是否在项目允许范围内。
func (p Project) ExecutorAllowed(executor, self string) bool {
	if p.AllowedExecutors == nil || executor == self {
		return true
	}
	for _, id := range p.AllowedExecutors {
		if id == executor {
			return true
		}
	}
	return false
}

// ---- Timer（§6.8） ----

type TimerKind string

const (
	TimerDue       TimerKind = "due"
	TimerRemind    TimerKind = "remind"
	TimerFollowUp  TimerKind = "follow_up"
	TimerWaitUntil TimerKind = "wait_until"
	TimerRecur     TimerKind = "recur"
)

type Timer struct {
	ID         string    `json:"id"`
	Target     string    `json:"target"` // 事项或能力包实例
	Kind       TimerKind `json:"kind"`
	At         time.Time `json:"at"`
	Recurrence string    `json:"recurrence,omitempty"`
}

// ---- Delivery（§6.9） ----

type DeliveryClass string

const (
	DeliveryDecision DeliveryClass = "decision"
	DeliveryBrief    DeliveryClass = "brief"
	DeliveryReminder DeliveryClass = "reminder"
	DeliveryHealth   DeliveryClass = "health"
	DeliveryWake     DeliveryClass = "wake"
)

type DeliveryState string

const (
	DeliveryPending      DeliveryState = "pending"
	DeliveryClaimed      DeliveryState = "claimed"
	DeliveryDelivered    DeliveryState = "delivered"
	DeliveryAcknowledged DeliveryState = "acknowledged"
	DeliveryFallbackWeb  DeliveryState = "fallback_web"
	DeliverySuperseded   DeliveryState = "superseded"
)

var deliveryNext = map[DeliveryState][]DeliveryState{
	DeliveryPending:   {DeliveryClaimed, DeliveryFallbackWeb, DeliverySuperseded},
	DeliveryClaimed:   {DeliveryDelivered, DeliveryPending, DeliveryFallbackWeb, DeliverySuperseded}, // 回 pending：租约到期重试
	DeliveryDelivered: {DeliveryAcknowledged, DeliverySuperseded},
	// 退回 Web 只是渠道没送到，不挡本人确认
	DeliveryFallbackWeb: {DeliveryAcknowledged, DeliverySuperseded},
}

// CanTransition 判断投递状态能否由 s 转到 to；acknowledged、superseded 为终态；fallback_web 只剩确认或作废。
func (s DeliveryState) CanTransition(to DeliveryState) bool {
	for _, n := range deliveryNext[s] {
		if n == to {
			return true
		}
	}
	return false
}

// DeliveryContent 不含来源正文全文。
type DeliveryContent struct {
	Title     string   `json:"title"`
	Summary   string   `json:"summary,omitempty"`
	Options   []string `json:"options,omitempty"`
	Recommend string   `json:"recommend,omitempty"`
	WebURL    string   `json:"web_url,omitempty"`
	FormURL   string   `json:"form_url,omitempty"` // 一次性表单链接（§10.3）
	// WebOnly 由触达在写入决策投递时按事项现状填写，只读。为真时本内容不含 options、recommend、form_url，
	// 只给标题、摘要与 Web 链接，渠道不得渲染可点选项（§14.4）。
	WebOnly bool `json:"web_only,omitempty"`
}

// Injection 记录注入到哪个会话；注入不等于送达。
type Injection struct {
	Session string    `json:"injected_to_session"`
	At      time.Time `json:"at"`
}

type Delivery struct {
	ID            string          `json:"id"` // 发送方以此为幂等键
	Item          string          `json:"item"`
	Run           string          `json:"run,omitempty"` // 唤醒类投递的运行 id；不带凭据明文（§13.6）
	Class         DeliveryClass   `json:"class"`
	Route         string          `json:"route"`
	Content       DeliveryContent `json:"content"`
	Version       int64           `json:"version"` // 事项变化后旧版本作废
	State         DeliveryState   `json:"state"`
	Attempts      int             `json:"attempts"`
	NextAttemptAt *time.Time      `json:"next_attempt_at,omitempty"`
	ExternalRef   string          `json:"external_ref,omitempty"`
	Injections    []Injection     `json:"injections,omitempty"`
	// Suppressed 记被日上限（daily_cap）或静音（muted）压成"只留 Web 未读"的原因；为空表示没被压。
	Suppressed string `json:"suppressed,omitempty"`
	// Origin 只在没有事项（Item 为空）时出现：规则 alert 出口的提醒记触发它的来源事件与规则（§6.9）。
	Origin *DeliveryOrigin `json:"origin,omitempty"`
}

// DeliveryOrigin 是没有事项的提醒的出处：来源事件 id、规则 id（包/组/id）与来源 id（静音按来源对照）。
type DeliveryOrigin struct {
	SourceEvent string `json:"source_event"`
	Rule        string `json:"rule"`
	Source      string `json:"source,omitempty"`
}

// ---- Run（§6.11） ----

type RunCategory string

const (
	RunJudge      RunCategory = "judge"
	RunGenerate   RunCategory = "generate"
	RunTask       RunCategory = "task"
	RunBackground RunCategory = "background"
	RunSource     RunCategory = "source"
	RunTransform  RunCategory = "transform"
	RunReader     RunCategory = "reader"
)

type RunState string

const (
	RunQueued    RunState = "queued"
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
	RunTimeout   RunState = "timeout"
)

// Terminal 为运行终态。
func (s RunState) Terminal() bool {
	return s == RunSucceeded || s == RunFailed || s == RunTimeout
}

// CanTransition：queued → running → 终态；排队中被取消或拉起失败直接 failed。
func (s RunState) CanTransition(to RunState) bool {
	switch s {
	case RunQueued:
		return to == RunRunning || to == RunFailed
	case RunRunning:
		return to == RunSucceeded || to == RunFailed || to == RunTimeout
	}
	return false
}

type Run struct {
	ID          string      `json:"id"`
	Category    RunCategory `json:"category"`
	Runner      string      `json:"runner,omitempty"` // 后台运行：task 或 main_session
	Launch      string      `json:"launch,omitempty"` // 后台运行：headless 或 wake
	Target      string      `json:"target"`           // 事项 id；来源拉取记来源 id
	Harness     string      `json:"harness,omitempty"`
	SessionID   string      `json:"session_id,omitempty"`
	Access      string      `json:"access,omitempty"`
	Basis       *Basis      `json:"basis,omitempty"`
	ChainDepth  int         `json:"chain_depth"`
	State       RunState    `json:"state"`
	ExitCode    *int        `json:"exit_code,omitempty"`
	StartedAt   *time.Time  `json:"started_at,omitempty"`
	FinishedAt  *time.Time  `json:"finished_at,omitempty"`
	TimeoutSec  int         `json:"timeout,omitempty"`
	Fingerprint string      `json:"fingerprint,omitempty"` // 判断型与生成型的输入指纹
}

// ---- Source 登记、来源事件（§6.10） ----

type SourceType string

const (
	SourcePull    SourceType = "pull"
	SourceScript  SourceType = "script"
	SourceWebhook SourceType = "webhook"
)

// Source 是来源登记。SourceKind 是能力包规则匹配用的语义类型，内核只当字符串比较。
type Source struct {
	ID              string          `json:"id"`
	Type            SourceType      `json:"type"`
	SourceKind      string          `json:"source_type,omitempty"`
	Command         string          `json:"command,omitempty"`
	Schedule        string          `json:"schedule,omitempty"`
	Timeout         int             `json:"timeout,omitempty"`
	CredentialsPath string          `json:"credentials_path,omitempty"`
	DedupeDomain    string          `json:"dedupe_domain,omitempty"`
	DefaultProject  string          `json:"default_project"`
	Outbound        Outbound        `json:"outbound"`
	Config          json.RawMessage `json:"config,omitempty"`
	Cursor          string          `json:"cursor,omitempty"`
	Enabled         bool            `json:"enabled"`
	Stats           json.RawMessage `json:"stats,omitempty"`
}

// Outbound 是来源内容可以送给模型端点的范围，按端点位置分别设置（§6.10、§14.14）。
// 取值 full、excerpt、meta、none；External 为 nil 表示未选择（外部范围没有缺省值）。
type Outbound struct {
	Local    string  `json:"local"`
	External *string `json:"external"`
}

// ValidOutboundScope 报告 s 是否是外发范围的合法取值（full、excerpt、meta、none）。
func ValidOutboundScope(s string) bool {
	switch s {
	case "full", "excerpt", "meta", "none":
		return true
	}
	return false
}

type Disposition string

const (
	DispositionPending  Disposition = "pending"
	DispositionAttached Disposition = "attached"
	DispositionProposed Disposition = "proposed"
	DispositionIgnored  Disposition = "ignored"
	DispositionUnjudged Disposition = "unjudged"
	// DispositionExpired 是超出自动重判时限的未判定事件：不再自动重判、不计入积压，本人仍可转建议或忽略（§9.3）。
	DispositionExpired Disposition = "expired"
	// DispositionDigest 汇入简报的"知会"一节，不建事项、不提醒；DispositionAlerted 已直接提醒本人（§6.10、§9.3）。
	DispositionDigest  Disposition = "digest"
	DispositionAlerted Disposition = "alerted"
)

// 未判定的原因（§6.10 unjudged_reason）。
const (
	UnjudgedModelUnavailable = "model_unavailable" // 模型未配置或持续不可用：模型恢复后自动重判
	UnjudgedUndecided        = "undecided"         // 判断型级联后仍未定：不自动重判，等本人处理
	UnjudgedOutboundUnset    = "outbound_unset"    // 来源未选外发范围而端点都在外部：选定后自动重判
	UnjudgedRestored         = "restored"          // 本人把旧级联忽略的事件放回积压：只经积压放开重判
	UnjudgedModelsPaused     = "models_paused"     // 本人暂停了模型：同 model_unavailable 参与自动重判与到期，恢复本身不触发重判
)

// DedupeKey 是 (去重域, 事件 id)。
type DedupeKey struct {
	Domain  string `json:"domain"`
	EventID string `json:"event_id"`
}

// DedupeKeyFor 在 Source 未登记去重域时以 source id 作去重域。
func (s Source) DedupeKeyFor(eventID string) DedupeKey {
	d := s.DedupeDomain
	if d == "" {
		d = s.ID
	}
	return DedupeKey{Domain: d, EventID: eventID}
}

type SourceEvent struct {
	ID              string      `json:"id"`
	SourceID        string      `json:"source_id"`
	DedupeKey       DedupeKey   `json:"dedupe_key"`
	Type            string      `json:"type"`
	Subject         string      `json:"subject,omitempty"`
	Summary         string      `json:"summary,omitempty"`
	Detail          string      `json:"detail,omitempty"`
	Link            string      `json:"link,omitempty"`
	OccurredAt      time.Time   `json:"occurred_at"`
	CorrelationKeys []string    `json:"correlation_keys,omitempty"`
	Sender          string      `json:"sender,omitempty"`
	SenderVerified  bool        `json:"sender_verified"`
	Disposition     Disposition `json:"disposition"`
	Item            string      `json:"item,omitempty"`
	AttachState     string      `json:"attach_state,omitempty"`
	UnjudgedReason  string      `json:"unjudged_reason,omitempty"`
	Rejudges        int         `json:"rejudges,omitempty"`
	// v0.2.0：标准事件的可选字段与判断关联（§6.10）。Items 是挂靠或生成的事项 id；来源程序给的聚合消息在 Messages，不进 API。
	Metrics  map[string]float64 `json:"metrics,omitempty"`
	MergeKey string             `json:"merge_key,omitempty"`
	Items    []string           `json:"items,omitempty"`
	Judgment string             `json:"judgment,omitempty"`
	Messages []EventMessage     `json:"-"`
	// PrevMetrics 是入账那一刻同一序列（来源，第一个关联键，指标名）上一次的取值；序列第一次出现的指标没有条目。
	// 变化类字段条件（changed、crosses_*）据此判断，重启、重判和重放都得到同一结果（§9.2）。
	PrevMetrics map[string]float64 `json:"-"`
}

// EventMessage 是聚合事件 items[] 里的一条消息（§9.2）。
type EventMessage struct {
	Sender string    `json:"sender"`
	Text   string    `json:"text"`
	At     time.Time `json:"at"`
}

// TrustedSender：缺失或未核实按不在白名单处理（§6.10）。
func (e SourceEvent) TrustedSender() bool { return e.Sender != "" && e.SenderVerified }
