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
	Config          json.RawMessage `json:"config,omitempty"`
	Cursor          string          `json:"cursor,omitempty"`
	Enabled         bool            `json:"enabled"`
	Stats           json.RawMessage `json:"stats,omitempty"`
}

type Disposition string

const (
	DispositionPending  Disposition = "pending"
	DispositionAttached Disposition = "attached"
	DispositionProposed Disposition = "proposed"
	DispositionIgnored  Disposition = "ignored"
	DispositionUnjudged Disposition = "unjudged"
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
}

// TrustedSender：缺失或未核实按不在白名单处理（§6.10）。
func (e SourceEvent) TrustedSender() bool { return e.Sender != "" && e.SenderVerified }
