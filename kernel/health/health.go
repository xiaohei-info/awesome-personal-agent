// Package health 是健康视图的汇总（设计 §15.2）：由既有账本记录与外呼统计读出十一类信号，
// 长时与短任务两个队列分开给出排队数和最长等待时间。只读、只报不修；不新增存储，不写任何模块的数据。
// 每个信号的修复入口由展示层链接（来源页、运行详情、项目页等），本包不触发任何修复。
package health

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// RuleFallbackReason 是规则内容变化后回到 observe 时 health.alert 的 Reason（kernel/rules 写入）。
const RuleFallbackReason = "rule content changed, back to observe"

// PackHealth 由能力包加载器提供：仍未修复的加载拒绝（加载器保留上一有效版本）。
type PackHealth interface{ Health() []packs.Rejection }

// Options 配置汇总；Deliveries 必填，Packs 为空表示没有加载器（该信号恒为空）。
type Options struct {
	Deliveries *delivery.Queue
	Packs      PackHealth
	Now        func() time.Time
}

// SourceFailure 是一个来源的失败现状（来源页是修复入口；认证失败带 needs_input 事项）。
type SourceFailure struct {
	Source              string
	Enabled             bool
	ConsecutiveFailures int
	LastError           string
	NeedsInputItem      string
}

// QueueHealth 是单个外呼队列的现状。
type QueueHealth struct {
	Running, Queued int
	LongestWait     time.Duration // 排队中最长等待；没有排队为 0
}

// Outcall 把长时与短任务队列分开（互不挤占），并给出超时的 Run。
type Outcall struct {
	Long, Short QueueHealth
	Timeouts    []domain.Run
}

// FollowupKind 是跟进判断失败 health.alert 的 payload.kind（kernel/proactive/followup 写入）。
// 与初筛不同，它出健康类投递（notify.go），同时进"模型不可用"。
const FollowupKind = "followup"

// ModelIssue 是一次判断型调用失败或超时（含 web_only 初筛、跟进判断）。同一模板之后有成功调用即清除（见 recovered）。
type ModelIssue struct {
	Seq      int64
	Kind     string
	Object   string
	Reason   string
	template string // 写入点记下的提示词模板；空表示无法判定恢复
}

// OtherAlert 是没有专属信号的 health.alert（规则动作失败等）。
type OtherAlert struct {
	Seq    int64
	Object string
	Reason string
}

// RuleFallback 是一次规则回退 observe 的事件。
type RuleFallback struct {
	Seq  int64
	Rule string
	At   time.Time
}

// ReceiptFailure 是一次回执或捕获被内核拒绝（退出码 2、4、23）的告警；Credential 是被拒绝的操作者（凭据）。
type ReceiptFailure struct {
	Seq        int64
	Object     string
	Command    string
	Exit       int
	Credential string
	at         time.Time
}

// 模型端点现状（View.Endpoint）。
const (
	EndpointNotConfigured = "not_configured" // 没有配置，而有事件在等它
	EndpointFailing       = "failing"        // 最近一次调用在全部重试后仍失败，之后没有成功
)

// View 是十一类信号。空切片/零值表示该信号当前正常。
type View struct {
	SourceFailures   []SourceFailure
	Outcall          Outcall
	DeliveryBacklog  []domain.Delivery
	ChainTrips       []chain.Trip
	AgentsMDDrift    []agentsmd.OpenDrift
	PackRejections   []packs.Rejection
	ModelUnavailable []ModelIssue
	// Endpoint 是模型端点现状，空表示正常（或没有配置、也没有事件在等它）；计入"模型不可用"。
	Endpoint string
	// Unjudged 是未判定积压（disposition 为 unjudged），单独一个提醒级信号；Expired 是已过期、不计入积压的条数。
	Unjudged        []domain.SourceEvent
	Expired         int
	RuleFallbacks   []RuleFallback
	ReceiptFailures []ReceiptFailure
	OtherAlerts     []OtherAlert
}

// Healthy 为真表示异常级信号都正常。未判定积压只是待本人处理的提醒，不影响它。
func (v View) Healthy() bool {
	return len(v.SourceFailures) == 0 && len(v.Outcall.Timeouts) == 0 && len(v.DeliveryBacklog) == 0 &&
		len(v.ChainTrips) == 0 && len(v.AgentsMDDrift) == 0 && len(v.PackRejections) == 0 &&
		len(v.ModelUnavailable) == 0 && v.Endpoint == "" && len(v.RuleFallbacks) == 0 && len(v.ReceiptFailures) == 0 && len(v.OtherAlerts) == 0
}

// Collect 在调用方的只读事务内汇总。任一来源读取失败都原样返回，不给出残缺的"健康"。
func Collect(tx *store.Tx, o Options) (View, error) {
	if o.Deliveries == nil {
		return View{}, errors.New("health: Deliveries is required")
	}
	now := time.Now()
	if o.Now != nil {
		now = o.Now()
	}
	var (
		v   View
		err error
	)
	if v.SourceFailures, err = sourceFailures(tx); err != nil {
		return View{}, err
	}
	if v.Outcall, err = outcall(tx, now); err != nil {
		return View{}, err
	}
	if v.DeliveryBacklog, err = o.Deliveries.Backlog(tx); err != nil {
		return View{}, err
	}
	if v.ChainTrips, err = chain.Trips(tx); err != nil {
		return View{}, err
	}
	if v.AgentsMDDrift, err = agentsmd.OpenDrifts(tx); err != nil {
		return View{}, err
	}
	if o.Packs != nil {
		v.PackRejections = o.Packs.Health()
	}
	if v.Unjudged, err = runner.Unjudged(tx); err != nil {
		return View{}, err
	}
	if v.Expired, err = runner.CountExpired(tx); err != nil {
		return View{}, err
	}
	if v.Endpoint, err = endpoint(tx, v.Unjudged); err != nil {
		return View{}, err
	}
	if v.ModelUnavailable, v.RuleFallbacks, v.ReceiptFailures, v.OtherAlerts, err = alerts(tx); err != nil {
		return View{}, err
	}
	return v, nil
}

// endpoint 判定模型端点现状：调用持续失败总是异常；没有配置只在有事件因此未判定时才算异常（不用模型是允许的，§5.6）。
func endpoint(tx *store.Tx, backlog []domain.SourceEvent) (string, error) {
	h, err := direct.HealthIn(tx)
	if err != nil || h.Unavailable {
		return EndpointFailing, err
	}
	_, pol, err := policy.Current(tx)
	if err != nil || (pol.GenerateEndpoint.URL != "" && pol.GenerateEndpoint.Model != "") {
		return "", err
	}
	for _, e := range backlog {
		// 判断未定、外发范围未选择、放回积压的事件与端点有没有配置无关
		if e.UnjudgedReason != domain.UnjudgedUndecided && e.UnjudgedReason != domain.UnjudgedOutboundUnset && e.UnjudgedReason != domain.UnjudgedRestored {
			return EndpointNotConfigured, nil
		}
	}
	return "", nil
}

func sourceFailures(tx *store.Tx) ([]SourceFailure, error) {
	srcs, err := registry.List(tx)
	if err != nil {
		return nil, err
	}
	var out []SourceFailure
	for _, s := range srcs {
		st, err := runner.StatsOf(s)
		if err != nil {
			return nil, err
		}
		if st.ConsecutiveFailures == 0 && s.Enabled && st.NeedsInputItem == "" {
			continue
		}
		out = append(out, SourceFailure{Source: s.ID, Enabled: s.Enabled, ConsecutiveFailures: st.ConsecutiveFailures,
			LastError: st.LastError, NeedsInputItem: st.NeedsInputItem})
	}
	return out, nil
}

func outcall(tx *store.Tx, now time.Time) (Outcall, error) {
	stats, err := proc.Stats(tx, now)
	if err != nil {
		return Outcall{}, err
	}
	recs, err := proc.List(tx) // shortcut: 全表扫描，Run 很多时给超时加按状态的查询
	if err != nil {
		return Outcall{}, err
	}
	out := Outcall{Long: queueHealth(stats[proc.QueueLong]), Short: queueHealth(stats[proc.QueueShort])}
	for _, r := range recs {
		if r.Run.State == domain.RunTimeout {
			out.Timeouts = append(out.Timeouts, r.Run)
		}
	}
	return out, nil
}

func queueHealth(s proc.QueueStat) QueueHealth {
	return QueueHealth{Running: s.Running, Queued: s.Queued, LongestWait: s.LongestWait}
}

// alerts 一遍读完 health.alert 并按来源分到各信号，分类只在这里（熔断由 chain.Trips 读，来源与能力包各有专属信号）：
// 模型调用失败（kind=screening、followup、decision-card）→ 模型不可用，之后恢复的清除；规则回退 observe；回执与捕获被拒（kind=receipt）→ 回执与捕获失败，同凭据同命令成功即清除；其余 → 通用告警。
// shortcut: 每次读全部健康事件，事件很多时加游标或时间窗。
func alerts(tx *store.Tx) (model []ModelIssue, rules []RuleFallback, receipts []ReceiptFailure, other []OtherAlert, err error) {
	var after int64
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, after, 500)
		if err != nil {
			return nil, nil, nil, nil, err
		}
		if len(es) == 0 {
			if model, err = recovered(tx, model); err == nil {
				receipts, err = receiptsOpen(tx, receipts)
			}
			return model, rules, receipts, other, err
		}
		for _, en := range es {
			e := en.Event
			after = e.Seq
			if en.Tombstone { // 对象已物理删除，告警内容没了，不再算现状
				continue
			}
			var p struct {
				Kind     string `json:"kind"`
				Rule     string `json:"rule"`
				Template string `json:"template"`
				Command  string `json:"command"`
				Exit     int    `json:"exit"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			switch {
			case e.Reason == chain.ExceededReason || p.Kind == "source" || p.Kind == "pack":
			case p.Kind == ScreeningKind || p.Kind == FollowupKind || p.Kind == DraftKind:
				model = append(model, ModelIssue{Seq: e.Seq, Kind: p.Kind, Object: e.Object, Reason: e.Reason, template: p.Template})
			case p.Kind == command.ReceiptKind:
				receipts = append(receipts, ReceiptFailure{Seq: e.Seq, Object: e.Object, Command: p.Command, Exit: p.Exit, Credential: e.Actor.Principal, at: e.At})
			case e.Reason == RuleFallbackReason:
				rules = append(rules, RuleFallback{Seq: e.Seq, Rule: p.Rule, At: e.At})
			default:
				other = append(other, OtherAlert{Seq: e.Seq, Object: e.Object, Reason: e.Reason})
			}
		}
	}
}

// receiptsOpen 去掉已清除的回执与捕获告警：告警之后同一凭据又成功执行过同一命令（一次即清除）。
// 另一凭据或另一命令的成功不算。成功以命令管线为 Receipt 命令留下的最近成功标记为凭，调用方带不带幂等键都一样。
// shortcut: 每条告警一次查询，告警很多时按凭据与命令合并查询。
func receiptsOpen(tx *store.Tx, rs []ReceiptFailure) ([]ReceiptFailure, error) {
	var open []ReceiptFailure
	for _, r := range rs {
		ok, err := command.SucceededSince(tx, r.Credential, r.Command, r.at)
		if err != nil {
			return nil, err
		}
		if !ok {
			open = append(open, r)
		}
	}
	return open, nil
}

// recovered 去掉已恢复的模型告警：告警之后同一模板有成功的真实调用（run.finished，非复用）即清除。
// 初筛与起草逐事项只做一次、不重试，所以任一事项的成功即算恢复；其余（跟进判断会对同一事项重试）要求同一事项成功，
// 否则别的事项成功会盖住仍在失败的事项。model 按序号升序。
// shortcut: 扫告警之后全部 run.finished，Run 很多时加游标或按模板查询。
func recovered(tx *store.Tx, model []ModelIssue) ([]ModelIssue, error) {
	if len(model) == 0 {
		return nil, nil
	}
	byTemplate, byObject := map[string]int64{}, map[[2]string]int64{}
	after := model[0].Seq
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvRunFinished}, after, 500)
		if err != nil {
			return nil, err
		}
		if len(es) == 0 {
			break
		}
		for _, en := range es {
			e := en.Event
			after = e.Seq
			var p struct {
				Template string `json:"template"`
				State    string `json:"state"`
				Reused   bool   `json:"reused"`
			}
			if !en.Tombstone && json.Unmarshal(e.Payload, &p) == nil && p.Template != "" && p.State == string(domain.RunSucceeded) && !p.Reused {
				byTemplate[p.Template], byObject[[2]string{p.Template, e.Object}] = e.Seq, e.Seq
			}
		}
	}
	var open []ModelIssue
	for _, m := range model {
		ok := byObject[[2]string{m.template, m.Object}]
		if m.Kind == ScreeningKind || m.Kind == DraftKind {
			ok = byTemplate[m.template]
		}
		if m.template == "" || ok < m.Seq {
			open = append(open, m)
		}
	}
	return open, nil
}
