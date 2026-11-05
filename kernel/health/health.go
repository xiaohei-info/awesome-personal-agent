// Package health 是健康视图的汇总（设计 §15.2）：由既有账本记录与外呼统计读出八类信号，
// 长时与短任务两个队列分开给出排队数和最长等待时间。只读、只报不修；不新增存储，不写任何模块的数据。
// 每个信号的修复入口由展示层链接（来源页、运行详情、项目页等），本包不触发任何修复。
package health

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
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

// ModelIssue 是一次判断型 Run 失败或超时（含 web_only 初筛），事件序号与原因。
type ModelIssue struct {
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

// View 是八类信号。空切片/零值表示该信号当前正常。
type View struct {
	SourceFailures   []SourceFailure
	Outcall          Outcall
	DeliveryBacklog  []domain.Delivery
	ChainTrips       []chain.Trip
	AgentsMDDrift    []agentsmd.OpenDrift
	PackRejections   []packs.Rejection
	ModelUnavailable []ModelIssue
	Unjudged         []domain.SourceEvent
	RuleFallbacks    []RuleFallback
}

// Healthy 为真表示八类信号都正常。
func (v View) Healthy() bool {
	return len(v.SourceFailures) == 0 && len(v.Outcall.Timeouts) == 0 && len(v.DeliveryBacklog) == 0 &&
		len(v.ChainTrips) == 0 && len(v.AgentsMDDrift) == 0 && len(v.PackRejections) == 0 &&
		len(v.ModelUnavailable) == 0 && len(v.Unjudged) == 0 && len(v.RuleFallbacks) == 0
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
	if v.ModelUnavailable, v.RuleFallbacks, err = alerts(tx); err != nil {
		return View{}, err
	}
	return v, nil
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

// alerts 一遍读完 health.alert：初筛失败与规则回退 observe（熔断由 chain.Trips 读）。
// shortcut: 每次读全部健康事件，事件很多时加游标或时间窗。
func alerts(tx *store.Tx) (model []ModelIssue, rules []RuleFallback, err error) {
	var after int64
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, after, 500)
		if err != nil || len(es) == 0 {
			return model, rules, err
		}
		for _, en := range es {
			e := en.Event
			after = e.Seq
			var p struct {
				Kind string `json:"kind"`
				Rule string `json:"rule"`
			}
			if len(e.Payload) > 0 {
				_ = json.Unmarshal(e.Payload, &p) // 墓碑化后没有内容：只按 Reason 判断
			}
			switch {
			case p.Kind == "screening":
				model = append(model, ModelIssue{Seq: e.Seq, Object: e.Object, Reason: e.Reason})
			case e.Reason == RuleFallbackReason:
				rules = append(rules, RuleFallback{Seq: e.Seq, Rule: p.Rule, At: e.At})
			}
		}
	}
}
