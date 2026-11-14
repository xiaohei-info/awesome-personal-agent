// Package health 是健康视图的汇总（设计 §15.2）：由既有账本记录与外呼统计读出十三类信号，
// 长时与短任务两个队列分开给出排队数和最长等待时间。只读、只报不修；不新增存储，不写任何模块的数据。
// 每个信号的修复入口由展示层链接（来源页、运行详情、项目页等），本包不触发任何修复。
package health

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
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
	// DelegationStallAfter 是委派多久没有回执就算长时间无回执；0 取默认 24 小时（运行配置 delegation_stall_after）。
	DelegationStallAfter time.Duration
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
	EndpointPaused        = "paused"         // 本人暂停了模型：是正常状态，不计入"模型不可用"
)

// View 是十三类信号。空切片/零值表示该信号当前正常。
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
	// Judge 与 Generate 是判断端点与生成端点各自的现状（空、EndpointNotConfigured、EndpointFailing、EndpointPaused）；
	// Endpoint 取其中较差的，暂停时为空。
	Judge, Generate string
	// Unjudged 是未判定积压（disposition 为 unjudged），单独一个提醒级信号；Expired 是已过期、不计入积压的条数。
	Unjudged        []domain.SourceEvent
	Expired         int
	RuleFallbacks   []RuleFallback
	ReceiptFailures []ReceiptFailure
	OtherAlerts     []OtherAlert
	// StalledDelegations 与 ClockLags 是 v0.2.0 追加的两个提醒级信号，都不影响 Healthy。
	StalledDelegations []StalledDelegation
	ClockLags          []ClockLag
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
	now := clock.Now()
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
	if v.Judge, v.Generate, v.Endpoint, err = endpoint(tx, v.Unjudged); err != nil {
		return View{}, err
	}
	if v.ModelUnavailable, v.RuleFallbacks, v.ReceiptFailures, v.OtherAlerts, v.ClockLags, err = alerts(tx, now); err != nil {
		return View{}, err
	}
	after := o.DelegationStallAfter
	if after <= 0 {
		after = 24 * time.Hour
	}
	if v.StalledDelegations, err = stalledDelegations(tx, now, after); err != nil {
		return View{}, err
	}
	return v, nil
}

// endpoint 判定两个端点的现状：生成端点调用持续失败总是异常；某个端点没有配置只在有事件因此未判定时才算异常（不用模型是允许的，§5.6）。
// 判断端点的故障由 direct 记在 direct_judge_health：分类端点整批失败后即使对话端点定案也要报（§8.8）。
// 本人暂停了模型时两者都是 EndpointPaused，不算异常：暂停前留下的故障标记要等恢复后的调用来清除。
func endpoint(tx *store.Tx, backlog []domain.SourceEvent) (judge, generate, worst string, err error) {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return "", EndpointFailing, EndpointFailing, err
	}
	if pol.ModelsPaused {
		return EndpointPaused, EndpointPaused, "", nil
	}
	h, err := direct.HealthIn(tx)
	if err != nil {
		return "", EndpointFailing, EndpointFailing, err
	}
	if h.Unavailable {
		generate = EndpointFailing
	}
	jh, err := direct.JudgeHealthIn(tx)
	if err != nil {
		return EndpointFailing, generate, EndpointFailing, err
	}
	// 判断端点可选：撤掉之后旧故障记录不再报，否则没有调用能把它清掉。
	if jh.Unavailable && pol.JudgeEndpoint != nil && pol.JudgeEndpoint.URL != "" {
		judge = EndpointFailing
	}
	waiting := false
	for _, e := range backlog {
		// 判断未定、外发范围未选择、放回积压的事件与端点有没有配置无关
		if e.UnjudgedReason != domain.UnjudgedUndecided && e.UnjudgedReason != domain.UnjudgedOutboundUnset && e.UnjudgedReason != domain.UnjudgedRestored {
			waiting = true
			break
		}
	}
	if waiting && (pol.JudgeEndpoint == nil || pol.JudgeEndpoint.URL == "") {
		judge = EndpointNotConfigured
	}
	if waiting && generate == "" && (pol.GenerateEndpoint.URL == "" || pol.GenerateEndpoint.Model == "") {
		generate = EndpointNotConfigured
	}
	worst = generate
	if judge == EndpointFailing || (judge != "" && worst == "") {
		worst = judge
	}
	return judge, generate, worst, nil
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
func alerts(tx *store.Tx, now time.Time) (model []ModelIssue, rules []RuleFallback, receipts []ReceiptFailure, other []OtherAlert, lags []ClockLag, err error) {
	var after int64
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, after, 500)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		if len(es) == 0 {
			if model, err = recovered(tx, model); err == nil {
				receipts, err = receiptsOpen(tx, receipts)
			}
			return model, rules, receipts, other, lags, err
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
			case p.Kind == ClockLagKind: // 迟到只在最近一小时内算数：之后连续一小时没有新的迟到即清除
				if e.At.After(now.Add(-ClockLagClearAfter)) {
					lags = append(lags, ClockLag{Seq: e.Seq, Timer: e.Object, At: e.At})
				}
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

// StalledDelegation 是一个超过 delegation_stall_after 没有回执或进展的委派子事项（修复入口：任务委派页）。
type StalledDelegation struct {
	Item     string
	Assignee string
	Since    time.Time // 最近一次回执或进展，没有则为委派时刻
}

// stalledDelegations 读出仍开着、执行者没变、最近一次动静（委派、回执、进展）距今不短于 after 的委派子事项。
// 有回执（动静被刷新）、被改派（assignee 与委派时不同）、终结（离开 open）都使其不再出现。
// shortcut: 逐个委派读事项事件，委派很多时按事件类型一次查询。
func stalledDelegations(tx *store.Tx, now time.Time, after time.Duration) ([]StalledDelegation, error) {
	its, err := items.List(tx, items.Filter{Kind: domain.KindDelegation, Phase: domain.PhaseOpen})
	if err != nil {
		return nil, err
	}
	var out []StalledDelegation
	for _, it := range its {
		if it.Assignee == "" {
			continue
		}
		es, err := eventlog.ReadObject(tx, it.ID)
		if err != nil {
			return nil, err
		}
		var last time.Time
		reassigned := false
		for _, en := range es {
			switch en.Event.Type {
			case domain.EvDelegationCreated:
				var p struct {
					Item struct {
						Assignee string `json:"assignee"`
					} `json:"item"`
				}
				if !en.Tombstone && json.Unmarshal(en.Event.Payload, &p) == nil && p.Item.Assignee != "" && p.Item.Assignee != it.Assignee {
					reassigned = true
				}
				fallthrough
			case domain.EvDelegationReported, domain.EvItemProgressAdded:
				if en.Event.At.After(last) {
					last = en.Event.At
				}
			}
		}
		if reassigned || last.IsZero() || now.Sub(last) < after {
			continue
		}
		out = append(out, StalledDelegation{Item: it.ID, Assignee: it.Assignee, Since: last})
	}
	return out, nil
}

// ClockLagKind 是时钟迟到 health.alert 的 payload.kind；ClockLagThreshold 是算迟到的下限，ClockLagClearAfter 是连续多久没有迟到才清除。
const (
	ClockLagKind       = "clock_lag"
	ClockLagThreshold  = 5 * time.Minute
	ClockLagClearAfter = time.Hour
)

// ClockLag 是一次迟到触发；At 是实际触发时刻，Timer 是 Timer 的目标对象。
type ClockLag struct {
	Seq   int64
	Timer string
	At    time.Time
}

// RecordClockLag 在时钟处理事务内调用：定时实际触发比应触发时刻晚 ClockLagThreshold 以上（含停机错过的）时，
// 记一条 health.alert（只报不修；投递仍走同一处，§15.2）。c.Late 由时钟给出；未迟到什么也不写。
func RecordClockLag(tx *store.Tx, c clock.Command, at time.Time) error {
	if c.Late < ClockLagThreshold {
		return nil
	}
	p, _ := json.Marshal(map[string]any{"kind": ClockLagKind, "timer": c.TimerID, "timer_kind": c.Kind, "late_seconds": int64(c.Late.Seconds())})
	_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: c.Target, Payload: p, At: at,
		Actor:  domain.Actor{Type: domain.ActorSystem, Principal: "kernel:clock", Via: domain.ViaClock},
		Reason: fmt.Sprintf("timer %s fired %s after its scheduled time", c.TimerID, c.Late.Round(time.Second))})
	return err
}
