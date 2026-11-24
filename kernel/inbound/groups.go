package inbound

import (
	"cmp"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// ---- 建议页的分组（§7.2） ----
// 纯读视图：每次读建议时按已有字段现算，不落账、不存分组。输入只有合并键（proposal.merge_key）、来源类别
// （第一条来源事件的类型、是否属于上游对象、origin.source）与标题；不读正文，不调模型。每条建议恰好在一组里。

// 组的种类。
const (
	GroupPinned  = "pinned"  // 需要本人拍板的：置顶、不折叠，不进任何批量
	GroupTopic   = "topic"   // 标题类别（Topic 给出是哪一类）
	GroupObject  = "object"  // 合并键相同、又不属于任何标题类别的几条
	GroupHosted  = "hosted"  // 其余属于上游对象的（代码托管通知）
	GroupHistory = "history" // 从待办库迁进来的历史清单
	GroupRest    = "rest"    // 其余各不相同的具体事，逐条列出
)

// 标题类别，顺序即建议页上的顺序。
const (
	TopicCIFailure       = "ci_failure"
	TopicReviewRequest   = "review_request"
	TopicAlert           = "alert"
	TopicFinance         = "finance"
	TopicAccountSecurity = "account_security"
	TopicSubscription    = "subscription"
)

// HistorySourcePrefixes：origin.source 以其中任一前缀开头的建议是从待办库迁进来的调研、学习类清单（agent:vault-*）。
// shortcut: 前缀表在代码里，要让本人在设置页自己改时升级成策略项。
var HistorySourcePrefixes = []string{"agent:vault-"}

// topicRule 是一类的标题规则。只认写明的写法，认不出的不归类（拿不准就不折进去）。
type topicRule struct {
	topic string
	re    *regexp.Regexp
}

var topicRules = []topicRule{
	{TopicCIFailure, regexp.MustCompile(`(?i)((\bci\b|\bbuild\b|\bworkflow\b|\bpipeline\b|\be2e\b|构建|流水线|集成测试).*(失败|\bfail(ed|ure|ing)?\b)|\brun failed\b)`)},
	{TopicReviewRequest, regexp.MustCompile(`(?i)(请求评审|评审请求|待评审|请.{0,6}评审|(审阅|评审|review)\s*\S{0,20}\s*(PR|MR)\b|\breview[ _]requested\b)`)},
	{TopicAccountSecurity, regexp.MustCompile(`(?i)(登录提醒|新登录|新设备|新\s*IP|异地登录|验证码|密码已(更改|修改|重置)|密钥创建|轮换密钥|账[号户]安全|安全提醒|\bnew sign-?in\b|\bpassword (was )?(changed|reset)\b|\bsecurity alert\b|\bverification code\b)`)},
	{TopicAlert, regexp.MustCompile(`(?i)(告警|预警|故障|漏洞|\balert\b)`)},
	{TopicFinance, regexp.MustCompile(`(?i)(提现|提币|充值|到账|扣款|账单|资金费|结算账户|计费|支付档案|付款|转账|退款|\binvoice\b|\bpayment\b|\bbilling\b|\bwithdraw)`)},
	{TopicSubscription, regexp.MustCompile(`(?i)(营销|订阅|\bnewsletter\b|\bunsubscribe\b|\bearly access\b|\bhighlights\b|\bunread notifications\b|\bdigest\b)`)},
}

// 来源类别能认的标题类别：代码托管（属于上游对象）的标题多是 PR 自己的标题，只认 CI 失败与评审请求；
// 即时通讯另认告警；邮件六类都认；其余来源（提醒、Agent 自建、迁移）不按标题归类。
var topicsBySource = map[string][]string{
	"hosted": {TopicCIFailure, TopicReviewRequest},
	"chat":   {TopicCIFailure, TopicReviewRequest, TopicAlert},
	"mail":   {TopicCIFailure, TopicReviewRequest, TopicAccountSecurity, TopicAlert, TopicFinance, TopicSubscription},
}

var topicOrder = []string{TopicCIFailure, TopicReviewRequest, TopicAlert, TopicFinance, TopicAccountSecurity, TopicSubscription}

// Group 是建议页的一组。Items 按建立时刻先后；Bundles 是组里合并键相同、接受时合成一个事项的几条（每组至少两条，
// 第一条是留下的那条），本人拆开过、跨项目的不在同一个里；First、Last 是组里最早与最新一条的建立时刻。
type Group struct {
	Key     string     `json:"key"`
	Kind    string     `json:"kind"`
	Topic   string     `json:"topic,omitempty"`
	Items   []string   `json:"items"`
	Bundles [][]string `json:"bundles,omitempty"`
	First   time.Time  `json:"first"`
	Last    time.Time  `json:"last"`
}

// NeedsYou：决定与承诺、起草判为紧急、优先级高或紧急的建议需要本人拍板，不折叠、不进批量。
func NeedsYou(it domain.Item) bool {
	return it.Kind == domain.KindDecision || it.Kind == domain.KindCommitment || (it.Proposal != nil && it.Proposal.Urgent != nil && *it.Proposal.Urgent) ||
		it.Priority == domain.PriorityHigh || it.Priority == domain.PriorityUrgent
}

func isHistory(it domain.Item) bool {
	return it.Origin != nil && slices.ContainsFunc(HistorySourcePrefixes, func(p string) bool { return strings.HasPrefix(it.Origin.Source, p) })
}

// sourceClass 是建议的来源类别：属于上游对象的为 hosted，否则取第一条来源事件类型的对象部分（mail、chat…）；没有来源事件为空。
func sourceClass(o Origin) string {
	if o.Object != "" {
		return "hosted"
	}
	fam, _, _ := strings.Cut(o.Type, ".")
	return fam
}

// TopicOf 是建议的标题类别，空为不归类。评审请求另认结构化的事件类型（<对象>.review_requested）。
func TopicOf(it domain.Item, o Origin) string {
	if strings.HasSuffix(o.Type, ".review_requested") {
		return TopicReviewRequest
	}
	allowed := topicsBySource[sourceClass(o)]
	for _, r := range topicRules {
		if slices.Contains(allowed, r.topic) && r.re.MatchString(it.Title) {
			return r.topic
		}
	}
	return ""
}

func byCreated(a, b domain.Item) int {
	return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), cmp.Compare(a.ID, b.ID))
}

// units 把建议按合并键连成单元：合并键相同的必在同一单元（同组），其余各自一个。单元内按建立先后，单元按首条先后。
func units(props []domain.Item) [][]domain.Item {
	props = slices.Clone(props)
	slices.SortFunc(props, byCreated)
	var out [][]domain.Item
	at := map[string]int{}
	for _, it := range props {
		k := mergeKeyOfItem(it)
		if i, ok := at[k]; ok && k != "" {
			out[i] = append(out[i], it)
			continue
		}
		if k != "" {
			at[k] = len(out)
		}
		out = append(out, []domain.Item{it})
	}
	return out
}

// bundles 是一个合并键单元里接受时合成一个事项的几份（至少两条的，见 split）。
func bundles(unit []domain.Item) [][]domain.Item {
	return slices.DeleteFunc(split(unit), func(p []domain.Item) bool { return len(p) < 2 })
}

// Groups 把建议分组：合并键相同的连成一个单元整体归组；单元里有需要拍板的归置顶，迁移来的归历史清单，
// 其次取单元里最早一条认得出的标题类别，再次多于一条的自成一组（同一对象），属于上游对象的归代码托管通知，其余逐条。
func Groups(props []domain.Item, origins map[string]Origin) []Group {
	type acc struct {
		g   Group
		its []domain.Item
	}
	var (
		order []string
		gs    = map[string]*acc{}
	)
	put := func(key, kind, topic string, unit []domain.Item) {
		a := gs[key]
		if a == nil {
			a = &acc{g: Group{Key: key, Kind: kind, Topic: topic}}
			gs[key] = a
			order = append(order, key)
		}
		a.its = append(a.its, unit...)
		if kind == GroupPinned {
			return
		}
		for _, b := range bundles(unit) {
			ids := make([]string, len(b))
			for i, it := range b {
				ids[i] = it.ID
			}
			a.g.Bundles = append(a.g.Bundles, ids)
		}
	}
	for _, u := range units(props) {
		topic := ""
		for _, it := range u {
			if topic = TopicOf(it, origins[it.ID]); topic != "" {
				break
			}
		}
		switch {
		case slices.ContainsFunc(u, NeedsYou):
			put(GroupPinned, GroupPinned, "", u)
		case slices.ContainsFunc(u, isHistory):
			put(GroupHistory, GroupHistory, "", u)
		case topic != "":
			put(GroupTopic+":"+topic, GroupTopic, topic, u)
		case len(u) > 1:
			put(GroupObject+":"+u[0].ID, GroupObject, "", u)
		case origins[u[0].ID].Object != "":
			put(GroupHosted, GroupHosted, "", u)
		default:
			put(GroupRest, GroupRest, "", u)
		}
	}
	rank := func(a *acc) int {
		switch a.g.Kind {
		case GroupPinned:
			return 0
		case GroupTopic:
			return 1 + slices.Index(topicOrder, a.g.Topic)
		case GroupObject:
			return 10
		case GroupHosted:
			return 11
		case GroupHistory:
			return 12
		}
		return 13
	}
	out := make([]Group, 0, len(order))
	for _, k := range order {
		a := gs[k]
		slices.SortFunc(a.its, byCreated)
		for _, it := range a.its {
			a.g.Items = append(a.g.Items, it.ID)
		}
		a.g.First, a.g.Last = a.its[0].CreatedAt, a.its[len(a.its)-1].CreatedAt
		out = append(out, a.g)
	}
	slices.SortStableFunc(out, func(x, y Group) int { return cmp.Compare(rank(gs[x.Key]), rank(gs[y.Key])) })
	return out
}
