// Package conn 是外呼调用器的第三种外呼：连接插件调用（设计 §2、§3、§7.7）。
//
// 本包只有接口、类型与调用器。连接插件在 connectors/ 里实现这些接口，内核不 import 插件；
// 装配方把插件（接入声明 + 工厂）交给调用器。内核经调用器调用插件：发什么、发不发、给谁都已在调用前由内核定好，
// 接口上没有收件人参数。调用器负责超时、恢复 panic、同一连接串行、收听保活、报错用凭据值脱敏；
// 它不写账本、不发事件，结果交回调用模块（§4.1 第 3 条）。
package conn

import (
	"context"
	"errors"
)

// Contract 是进程内连接插件接口的版本（apa version 的 protocols.connector_contract）；只增不改。
const Contract = 1

// 方向。
const (
	DirectionChannel = "channel"
	DirectionOutput  = "output"
)

// Capabilities 是渠道的能力声明，与 connector.yaml 的 capabilities 一致；内核据它决定给什么内容。
// 三项都为假即「只能看」：决策提醒带一次性表单链接（§2.3）。
type Capabilities struct {
	Buttons  bool `yaml:"buttons" json:"buttons"`   // 能显示可点的选项并回传点击
	Reply    bool `yaml:"reply" json:"reply"`       // 能回传本人对那条消息的文字回复
	Identify bool `yaml:"identify" json:"identify"` // 能在收听里认出识别口令
}

// ViewOnly 报告渠道是否只能看（收不了回答）。
func (c Capabilities) ViewOnly() bool { return !c.Buttons && !c.Reply }

// Option 是决策的一个可选项。
type Option struct{ ID, Label string }

// Message 是中立消息：内容已由内核定好，插件只按平台渲染。
type Message struct {
	Key       string // 幂等键：投递 id + 版本
	Class     string // decision / reminder / brief / health
	Title     string
	Summary   string
	Options   []Option // web_only 时为空
	Recommend string
	WebURL    string // 绝对地址
	FormURL   string // 一次性表单链接；只出现在只能看的渠道上的非 web_only 决策
	WebOnly   bool
	Test      bool // 「发一条测试」：内容固定，由调用器构造
}

// Receipt 是平台给的消息标识，之后的回答按它核对。
type Receipt struct{ Ref string }

// Config 是一次调用交给插件的配置：本连接的非凭据字段与本连接声明过的凭据字段。
type Config struct {
	Settings map[string]string
	Secrets  map[string]string
}

// Channel 是渠道插件实现的接口。
type Channel interface {
	Send(ctx context.Context, m Message) (Receipt, error)
	Test(ctx context.Context) error // 鉴权与可达性自检，不打扰本人
}

// Listener 由能收回答的渠道实现：阻塞到 ctx 结束，断开就返回错误。
type Listener interface {
	Listen(ctx context.Context, in Inbound) error
}

// Inbound 是内核交给插件的回流口（PR3 接入回答命令）。
type Inbound interface {
	Answer(ctx context.Context, a Answer) Outcome
	Identify(ctx context.Context, code, sender, display string) bool
}

// Answer 是插件收到的一次回答；Sender 必须是平台分配、不可更改的用户 ID。
type Answer struct{ Ref, Sender, Choice, Text string }

// Outcome 是回流结果：Code 为 accepted / web_only / outdated / not_allowed / not_found / failed。
type Outcome struct{ Code, WebURL string }

// Plugin 是装配方交给调用器的一种连接类型：接入声明与工厂。
type Plugin struct {
	Decl Decl
	New  func(Config) (Channel, error)
}

// 问题代码（§1.6）：固定集合，插件只能从中选。
const (
	ProblemAuthFailed         = "auth_failed"
	ProblemUnreachable        = "unreachable"
	ProblemRejected           = "rejected"
	ProblemRateLimited        = "rate_limited"
	ProblemRecipientMissing   = "recipient_missing"
	ProblemConfigInvalid      = "config_invalid"
	ProblemCredentialsMissing = "credentials_missing"
	ProblemTargetNotWritable  = "target_not_writable"
	ProblemNotListening       = "not_listening"
	// ProblemTemporary 不在 §1.6 的集合里：插件没给代码的错误按临时失败处理，卡片上显示为「暂时没送出去」。
	ProblemTemporary = "temporary"
)

var problems = map[string]bool{ProblemAuthFailed: true, ProblemUnreachable: true, ProblemRejected: true, ProblemRateLimited: true,
	ProblemRecipientMissing: true, ProblemConfigInvalid: true, ProblemCredentialsMissing: true, ProblemTargetNotWritable: true,
	ProblemNotListening: true, ProblemTemporary: true}

// Problem 是带问题代码的错误；Detail 不得含凭据（调用器还会再脱敏一遍）。
type Problem struct{ Code, Detail string }

func (p *Problem) Error() string { return p.Code + ": " + p.Detail }

// Fail 返回带问题代码的错误，供插件使用。
func Fail(code, detail string) error { return &Problem{Code: code, Detail: detail} }

// ProblemOf 取错误的问题代码；没有或不在集合里的按临时失败。
func ProblemOf(err error) string {
	var p *Problem
	if errors.As(err, &p) && problems[p.Code] {
		return p.Code
	}
	return ProblemTemporary
}
