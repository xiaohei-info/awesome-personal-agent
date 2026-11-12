package registry

// 外发范围（设计 §6.10、§14.14）：来源登记里的 outbound{local, external} 与按端点位置裁剪来源内容的共用函数。
// 组织每一次模型调用的模块（入站、对话捕获，以及之后的判断协议客户端）都经 Decide 取范围、经 Trim 裁剪，
// 不各写一份；裁剪后的内容才进模型请求与输入指纹。

import (
	"net/url"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 外发范围的取值。
const (
	ScopeFull    = "full"    // 全文
	ScopeExcerpt = "excerpt" // 元信息、摘要与正文前 ExcerptChars 个字符
	ScopeMeta    = "meta"    // 类型、标题、发送者、时间、链接域名
	ScopeNone    = "none"    // 不送
)

// ExcerptChars 是 excerpt 范围保留的正文字符数（按字符，不按字节）。
const ExcerptChars = 500

// 端点位置与策略里 locality 的取值一致；这里不依赖策略包，只认 local，其余（含未声明）按 external（§14.14）。
const localityLocal = "local"

// 不能把内容送给任一可用端点的原因。
const (
	BlockUnset = "outbound_unset" // 外部范围未选择而可用的端点都在外部：记未判定，选定后自动重判
	BlockNone  = "outbound_none"  // 对所有可用端点的范围都是 none：改走摘要出口
)

// ValidScope 报告 s 是否是合法的范围取值。
func ValidScope(s string) bool { return domain.ValidOutboundScope(s) }

// ScopeFor 取来源在某个位置的端点上的范围；空串表示外部范围未选择。本机范围缺省 full。
func ScopeFor(o domain.Outbound, locality string) string {
	if locality == localityLocal {
		if o.Local == "" {
			return ScopeFull
		}
		return o.Local
	}
	if o.External == nil {
		return ""
	}
	return *o.External
}

// Decision 是一次模型调用前的外发判定。
type Decision struct {
	// Scopes 与传入的端点位置一一对应；空串表示该端点对这条内容不可用（范围为 none 或外部范围未选择）。
	Scopes []string
	// Block 非空表示没有任何端点可用（BlockUnset 或 BlockNone）。没有给出端点时恒为空：端点未配置不是外发控制的事。
	Block string
}

// Decide 按各端点的位置取范围。有外部范围未选择的端点而没有任何端点可用时，Block 为 BlockUnset（选定后可能可用）；
// 其余都不可用时为 BlockNone。
func Decide(o domain.Outbound, localities ...string) Decision {
	d := Decision{Scopes: make([]string, len(localities))}
	usable, unset := false, false
	for i, l := range localities {
		switch s := ScopeFor(o, l); s {
		case "":
			unset = true
		case ScopeNone:
		default:
			d.Scopes[i], usable = s, true
		}
	}
	switch {
	case len(localities) == 0 || usable:
	case unset:
		d.Block = BlockUnset
	default:
		d.Block = BlockNone
	}
	return d
}

// Trimmed 是裁剪后允许送出的来源内容。空字段即不送。
type Trimmed struct {
	Type       string
	Subject    string
	Sender     string
	OccurredAt string // RFC 3339
	Link       string // full：原链接；excerpt、meta：只有域名
	Summary    string
	Detail     string
}

// Trim 按范围裁剪一条来源事件。范围不是 full、excerpt、meta 之一（含 none 与未选择）时返回 false：调用方不得外发。
func Trim(ev domain.SourceEvent, scope string) (Trimmed, bool) {
	switch scope {
	case ScopeFull, ScopeExcerpt, ScopeMeta:
	default:
		return Trimmed{}, false
	}
	t := Trimmed{Type: ev.Type, Subject: ev.Subject, Sender: ev.Sender, Link: linkDomain(ev.Link)}
	if !ev.OccurredAt.IsZero() {
		t.OccurredAt = ev.OccurredAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	switch scope {
	case ScopeFull:
		t.Link, t.Summary, t.Detail = ev.Link, ev.Summary, ev.Detail
	case ScopeExcerpt:
		t.Summary, t.Detail = ev.Summary, firstChars(ev.Detail, ExcerptChars)
	}
	return t, true
}

func firstChars(s string, n int) string {
	if len(s) <= n { // 字节数不超过 n，字符数必然不超过
		return s
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// linkDomain 只留链接的主机名；解析不出主机的链接整个丢弃（路径与查询里常带令牌）。
func linkDomain(link string) string {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil {
		return ""
	}
	return u.Hostname()
}
