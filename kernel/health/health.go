// Package health 是健康视图的汇总（设计 §15.2）：由既有账本记录与外呼统计读出十三类信号，
// 长时与短任务两个队列分开给出排队数和最长等待时间。只读、只报不修；不新增存储，不写任何模块的数据。
// 视图只反映现状：每个信号要么由当前状态直接算出，要么有一个正常使用中一定会被满足的恢复条件；已恢复的告警不再计入，
// 原事件仍留在事件日志里（操作记录、运行日志照常可见）。
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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
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
// 同时实现 List 的（加载器本身）还用来把模型告警的模板归到判断端点或生成端点。
type PackHealth interface{ Health() []packs.Rejection }

// StaleAfter 是只能按时间收尾的告警（没有对象可看现状的熔断、其他健康告警、外呼超时）在没有新的同类告警时保留多久。
const StaleAfter = 24 * time.Hour

// Options 配置汇总；Deliveries 必填，Packs 为空表示没有加载器（该信号恒为空）。
type Options struct {
	Deliveries *delivery.Queue
	Packs      PackHealth
	Now        func() time.Time
	// DelegationStallAfter 是委派多久没有回执就算长时间无回执；0 取默认 24 小时（运行配置 delegation_stall_after）。
	DelegationStallAfter time.Duration
}

// SourceFailure 是一个来源的失败现状：当前连续失败或已停用（来源页是修复入口；认证失败时带它建的 needs_input 事项）。
// 最近一次拉取成功即不再出现：runner 在恢复时清掉 NeedsInputItem 并结束它建的事项。
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

// ModelIssue 是一次模型调用失败或超时（含 web_only 初筛、跟进判断、起草）。Endpoint 是它所属的端点（EndpointJudge 或
// EndpointGenerate）；该端点此后有任一次成功的真实调用或探测通过即清除（见 recovered）。
type ModelIssue struct {
	Seq      int64
	Kind     string
	Object   string
	Reason   string
	Endpoint string
	template string // 写入点记下的提示词模板，用来归端点
	at       time.Time
}

// 模型告警所属的端点（ModelIssue.Endpoint），与 Run 的 category 同值。
const (
	EndpointJudge    = string(domain.RunJudge)
	EndpointGenerate = string(domain.RunGenerate)
)

// OtherAlert 是没有专属信号的 health.alert（规则动作失败等）。
type OtherAlert struct {
	Seq    int64
	Object string
	Reason string
	at     time.Time
}

// RuleFallback 是一次规则回退 observe 的事件。
type RuleFallback struct {
	Seq    int64
	Rule   string
	At     time.Time
	object string
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
	trips, err := chain.Trips(tx)
	if err != nil {
		return View{}, err
	}
	if v.ChainTrips, err = openTrips(tx, trips, now); err != nil {
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
	if v.ModelUnavailable, v.RuleFallbacks, v.ReceiptFailures, v.OtherAlerts, v.ClockLags, err = alerts(tx, now, o.Packs); err != nil {
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
		// 外发范围未选择、放回积压的事件与端点有没有配置无关；v0.1 的 undecided 按 model_unavailable 对待
		if e.UnjudgedReason != domain.UnjudgedOutboundUnset && e.UnjudgedReason != domain.UnjudgedRestored {
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
		// 只看现状：最近一次拉取成功（连续失败归零）且仍启用即正常，留下的 needs_input 事项由 runner 在恢复时结束。
		if st.ConsecutiveFailures == 0 && s.Enabled {
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
	// 超时只在它仍是该目标最近一次运行、且结束不到 StaleAfter 时算现状：之后同一目标再跑一次（不论成败）即被取代。
	key := func(r domain.Run) string { return orElse(r.Target, r.ID) }
	latest := map[string]string{}
	for _, r := range recs { // 按序号升序
		latest[key(r.Run)] = r.Run.ID
	}
	for _, r := range recs {
		if r.Run.State == domain.RunTimeout && latest[key(r.Run)] == r.Run.ID && (r.Run.FinishedAt == nil || now.Sub(*r.Run.FinishedAt) < StaleAfter) {
			out.Timeouts = append(out.Timeouts, r.Run)
		}
	}
	return out, nil
}

func queueHealth(s proc.QueueStat) QueueHealth {
	return QueueHealth{Running: s.Running, Queued: s.Queued, LongestWait: s.LongestWait}
}

// alerts 一遍读完 health.alert 并按来源分到各信号，分类只在这里（熔断由 chain.Trips 读，来源与能力包各有专属信号）：
// 模型调用失败（kind=screening、followup、decision-card）→ 模型不可用，所属端点之后成功即清除；规则回退 observe，之后该规则切过模式即清除；
// 回执与捕获被拒（kind=receipt）→ 回执与捕获失败，同凭据同命令成功或事项已终结即清除；其余 → 通用告警，对象已处理或过了 StaleAfter 即清除。
// shortcut: 每次读全部健康事件，事件很多时加游标或时间窗。
func alerts(tx *store.Tx, now time.Time, ph PackHealth) (model []ModelIssue, rules []RuleFallback, receipts []ReceiptFailure, other []OtherAlert, lags []ClockLag, err error) {
	var after int64
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, after, 500)
		if err != nil {
			return nil, nil, nil, nil, nil, err
		}
		if len(es) == 0 {
			if model, err = recovered(tx, model, ph); err == nil {
				if receipts, err = receiptsOpen(tx, receipts); err == nil {
					if rules, err = fallbacksOpen(tx, rules); err == nil {
						other, err = othersOpen(tx, other, now)
					}
				}
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
				model = append(model, ModelIssue{Seq: e.Seq, Kind: p.Kind, Object: e.Object, Reason: e.Reason, template: p.Template, at: e.At})
			case p.Kind == command.ReceiptKind:
				receipts = append(receipts, ReceiptFailure{Seq: e.Seq, Object: e.Object, Command: p.Command, Exit: p.Exit, Credential: e.Actor.Principal, at: e.At})
			case e.Reason == RuleFallbackReason:
				rules = append(rules, RuleFallback{Seq: e.Seq, Rule: p.Rule, At: e.At, object: e.Object})
			default:
				other = append(other, OtherAlert{Seq: e.Seq, Object: e.Object, Reason: e.Reason, at: e.At})
			}
		}
	}
}

// receiptsOpen 去掉已清除的回执与捕获告警：告警之后同一凭据又成功执行过同一命令（一次即清除），或被拒回执所指的事项已终结
// （回执已无处可交，事后再成功的机会不会出现）。另一凭据或另一命令的成功不算。成功以命令管线为 Receipt 命令留下的最近成功标记为凭，
// 调用方带不带幂等键都一样。
// shortcut: 每条告警一次查询，告警很多时按凭据与命令合并查询。
func receiptsOpen(tx *store.Tx, rs []ReceiptFailure) ([]ReceiptFailure, error) {
	var open []ReceiptFailure
	for _, r := range rs {
		ok, err := command.SucceededSince(tx, r.Credential, r.Command, r.at)
		if err == nil && !ok {
			ok, err = terminal(tx, r.Object)
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			open = append(open, r)
		}
	}
	return open, nil
}

// terminal 报告 obj 是已终结（done、dropped）的事项；不是事项为假。
func terminal(tx *store.Tx, obj string) (bool, error) {
	it, err := items.Get(tx, obj)
	if err != nil {
		if isNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return it.Phase.Terminal(), nil
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

// handled 报告告警（序号 seq）所指的对象此后已被处理：是已终结的事项，或本人之后对它做过操作（告警与拒绝不算）。
// shortcut: 逐条告警读对象的后续事件，告警很多时按对象合并查询。
func handled(tx *store.Tx, obj string, seq int64) (bool, error) {
	if ok, err := terminal(tx, obj); ok || err != nil {
		return ok, err
	}
	after := seq
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Object: obj}, after, 500)
		if err != nil || len(es) == 0 {
			return false, err
		}
		for _, en := range es {
			after = en.Event.Seq
			if t := en.Event.Type; !en.Tombstone && en.Event.Actor.Type == domain.ActorHuman && t != domain.EvHealthAlert && t != domain.EvCommandRejected {
				return true, nil
			}
		}
	}
}

// openTrips 只留仍是现状的熔断：被拦下的对象之后既没有终结、本人也没处理过；没有对象的命令（Object 是命令名）
// 无从看现状，过了 StaleAfter 即清除。
func openTrips(tx *store.Tx, ts []chain.Trip, now time.Time) ([]chain.Trip, error) {
	var open []chain.Trip
	for _, t := range ts {
		if t.Object == t.Command {
			if now.Sub(t.At) < StaleAfter {
				open = append(open, t)
			}
			continue
		}
		ok, err := handled(tx, t.Object, t.Seq)
		if err != nil {
			return nil, err
		}
		if !ok {
			open = append(open, t)
		}
	}
	return open, nil
}

// fallbacksOpen 只留之后没有再切过模式的规则回退：本人重新审阅（切回 auto、改成 off，或其他任何切换）即清除。
func fallbacksOpen(tx *store.Tx, rs []RuleFallback) ([]RuleFallback, error) {
	var open []RuleFallback
	for _, r := range rs {
		es, err := eventlog.Query(tx, eventlog.Filter{Object: r.object, Type: domain.EvRuleModeChanged}, r.Seq, 1)
		if err != nil {
			return nil, err
		}
		if len(es) == 0 {
			open = append(open, r)
		}
	}
	return open, nil
}

// othersOpen 只留仍是现状的通用告警（规则动作失败、升级说明等）：对象之后已处理（见 handled）或告警已过 StaleAfter 即清除。
// 这类告警没有能代表现状的状态可读，时间窗保证它们不会永远挂着；同一问题再发生会记新的告警。
func othersOpen(tx *store.Tx, as []OtherAlert, now time.Time) ([]OtherAlert, error) {
	var open []OtherAlert
	for _, a := range as {
		if now.Sub(a.at) >= StaleAfter {
			continue
		}
		ok, err := handled(tx, a.Object, a.Seq)
		if err != nil {
			return nil, err
		}
		if !ok {
			open = append(open, a)
		}
	}
	return open, nil
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// recovered 去掉已恢复的模型告警：每条告警先归到端点（模板的 agent 类型：judge → 判断端点，generate → 生成端点；
// 没有配判断端点时判断型由对话端点即生成端点作答），该端点在告警之后有任一次成功的真实调用（复用缓存不算）或探测通过即清除。
// 不再要求"同一模板再成功一次"：起草、初筛逐事项只做一次，那件事未必会再发生。model 按序号升序。
func recovered(tx *store.Tx, model []ModelIssue, ph PackHealth) ([]ModelIssue, error) {
	if len(model) == 0 {
		return nil, nil
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return nil, err
	}
	judgeOn := pol.JudgeEndpoint != nil && pol.JudgeEndpoint.URL != ""
	agents := templateAgents(ph)
	seqs, ats, err := lastSuccess(tx, model[0].Seq)
	if err != nil {
		return nil, err
	}
	var open []ModelIssue
	for _, m := range model {
		m.Endpoint = attribute(m, agents, judgeOn)
		if seqs[m.Endpoint] <= m.Seq && !ats[m.Endpoint].After(m.at) {
			open = append(open, m)
		}
	}
	return open, nil
}

// templateAgents 取已加载包里各提示词模板的 agent 类型；加载器不可用时为空，由 attribute 按告警的 kind 兜底。
func templateAgents(ph PackHealth) map[string]string {
	l, ok := ph.(interface{ List() []*packs.Pack })
	if !ok {
		return nil
	}
	out := map[string]string{}
	for _, p := range l.List() {
		for name, pr := range p.Prompts {
			if _, dup := out[name]; !dup {
				out[name] = pr.Meta.Agent
			}
		}
	}
	return out
}

func attribute(m ModelIssue, agents map[string]string, judgeOn bool) string {
	agent := agents[m.template]
	if agent == "" { // 模板不在已加载的包里：初筛与跟进推迟是判断型，其余起草是生成型
		agent = EndpointGenerate
		if m.Kind == ScreeningKind || m.Kind == FollowupKind {
			agent = EndpointJudge
		}
	}
	if agent == EndpointJudge && judgeOn {
		return EndpointJudge
	}
	return EndpointGenerate
}

// lastSuccess 取两个端点各自最近一次成功：序号 after 之后成功的 run.finished（生成型调用、探测通过、
// 写明由哪一级定案的判断）的最大序号，以及判断记录里由分类端点（判断端点）或对话端点（生成端点）定案的最近时刻（记录没有序号）。
// shortcut: 扫告警之后全部 run.finished，Run 很多时加游标或按类别查询。
func lastSuccess(tx *store.Tx, after int64) (seqs map[string]int64, ats map[string]time.Time, err error) {
	seqs, ats = map[string]int64{}, map[string]time.Time{}
	note := func(ep string, seq int64) { seqs[ep] = max(seqs[ep], seq) }
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvRunFinished}, after, 500)
		if err != nil {
			return nil, nil, err
		}
		if len(es) == 0 {
			break
		}
		for _, en := range es {
			e := en.Event
			after = e.Seq
			var p struct {
				Category  string `json:"category"`
				State     string `json:"state"`
				Reused    bool   `json:"reused"`
				Probe     bool   `json:"probe"`
				DecidedBy string `json:"decided_by"`
			}
			if en.Tombstone || json.Unmarshal(e.Payload, &p) != nil || p.State != string(domain.RunSucceeded) || p.Reused {
				continue
			}
			switch {
			case p.Category == EndpointGenerate, p.Category == EndpointJudge && p.DecidedBy == record.DecidedChat:
				note(EndpointGenerate, e.Seq)
			case p.Category == EndpointJudge && (p.Probe || p.DecidedBy == record.DecidedClassifier):
				note(EndpointJudge, e.Seq)
			}
		}
	}
	for ep, by := range map[string]string{EndpointJudge: record.DecidedClassifier, EndpointGenerate: record.DecidedChat} {
		if ats[ep], err = record.LatestDecided(tx, by); err != nil {
			return nil, nil, err
		}
	}
	return seqs, ats, nil
}

// ProbeClears 报告端点 ep（EndpointJudge 或 EndpointGenerate）有没有要由探测通过来清除的故障：端点故障标记，或归它的未恢复告警。
func ProbeClears(tx *store.Tx, ph PackHealth, ep string) (bool, error) {
	judge, generate, _, err := endpoint(tx, nil)
	if err != nil {
		return false, err
	}
	if (ep == EndpointJudge && judge == EndpointFailing) || (ep == EndpointGenerate && generate == EndpointFailing) {
		return true, nil
	}
	model, _, _, _, _, err := alerts(tx, clock.Now(), ph)
	if err != nil {
		return false, err
	}
	for _, m := range model {
		if m.Endpoint == ep {
			return true, nil
		}
	}
	return false, nil
}

// RecordProbe 在本人测试已保存的端点或自检通过后调用：有要清除的故障时经 direct 记下探测通过（清端点故障标记、留一条 run.finished），
// 没有时什么也不写，查看自检不会每次都留痕。
func RecordProbe(tx *store.Tx, ph PackHealth, ep string, actor domain.Actor, at time.Time) error {
	ok, err := ProbeClears(tx, ph, ep)
	if err != nil || !ok {
		return err
	}
	return direct.RecordProbe(tx, ep, actor, at)
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
