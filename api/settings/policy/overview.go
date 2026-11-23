package policy

import (
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	krules "github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// overviewWindow 是预览里"近几天"的口径：与设计 §3.3 一致取 7 天。
const overviewWindow = 7 * 24 * time.Hour

// Overview 是 GET /settings/notify/overview（只给 Web 本人，只读）：设置页只读总览与各页单选旁预览的数据。
// 行表就是投递矩阵的行表（policy.FixedRows 加每个实例的两行），不另写一份；每行给有没有产生方（没有的 Web 不显示）
// 与近 7 天的计数。now 是按当前数据的现状，source_alert 是来源失败提醒线的依据（runner 的常量，Web 照它写说明）。
type Overview struct {
	Since       time.Time     `json:"since"`
	Rows        []overviewRow `json:"rows"`
	Now         nowCounts     `json:"now"`
	SourceAlert sourceAlert   `json:"source_alert"`
}

type overviewRow struct {
	Key      string `json:"key"`
	Produced bool   `json:"produced"`
	// Blocked 只在实例行：实例停用（instance_disabled），或它的定时规则没有一条是开（rule_off、rule_observe），所以不会产生。
	Blocked string `json:"blocked,omitempty"`
	// Rules 只在规则设的提醒、来源即时提醒两行：产生这一行的规则键。
	Rules []string `json:"rules,omitempty"`
	attention.RowCount
}

type nowCounts struct {
	DecisionsPending int        `json:"decisions_pending"` // 现在等你决定的问题
	Deadlines        int        `json:"deadlines"`         // 没结束、带截止的事项
	Overdue          int        `json:"overdue"`           // 其中截止已过的
	Waiting          int        `json:"waiting"`           // 在等别人的事项
	StaleNow         int        `json:"stale_now"`         // 现在已算久未推进的
	StaleFrom        *time.Time `json:"stale_from,omitempty"`
	ProposalsNew     int64      `json:"proposals_new"` // 近 7 天新增的建议
	// RuleObserved 是规则键 → 近 7 天试运行里"本来会做"的次数（提醒类规则在试运行时，单选旁写"本来会提醒 N 次"）。
	RuleObserved map[string]int64 `json:"rule_observed"`
}

type sourceAlert struct {
	StreakFailures      int          `json:"streak_failures"`
	LowFrequencyOverSec int          `json:"low_frequency_over_sec"`
	Sources             []sourceLine `json:"sources"`
}

type sourceLine struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Schedule   string `json:"schedule"`
	AlertAfter int    `json:"alert_after"` // 按拉取周期，连续失败到第几次提醒；来源失败时另声明的机会周期见 §9.2
}

// OverviewRoutes 返回总览路由。ps 是已加载的能力包（认出自带提醒的规则）；staleAfter 是久未推进的阈值（与今天页同一个）。
func OverviewRoutes(st *store.Store, ps krules.PackSource, staleAfter func() time.Duration) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/settings/notify/overview", Handler: func(w http.ResponseWriter, req *http.Request, p *core.Principal) error {
			if err := webOnly(p); err != nil {
				return err
			}
			var o Overview
			err := st.View(req.Context(), func(tx *store.Tx) (err error) {
				o, err = overview(tx, ps, staleAfter(), clock.Now())
				return err
			})
			if err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, o)
		}})
	}
}

func overview(tx *store.Tx, ps krules.PackSource, staleAfter time.Duration, now time.Time) (Overview, error) {
	o := Overview{Since: now.Add(-overviewWindow).UTC(), Now: nowCounts{RuleObserved: map[string]int64{}}}
	counts, err := attention.RowCounts(tx, o.Since)
	if err != nil {
		return o, err
	}
	timers, alerts := krules.ReminderProducers(ps)
	states, err := krules.List(tx)
	if err != nil {
		return o, err
	}
	ins, err := instances.List(tx)
	if err != nil {
		return o, err
	}
	custom := slices.ContainsFunc(ins, func(in instances.Instance) bool { return !slices.Contains(kpolicy.TaskTypes, in.Pack) })
	for _, k := range kpolicy.FixedRows {
		row := overviewRow{Key: k, RowCount: counts[k]}
		switch {
		case k == kpolicy.RowRuleReminder:
			row.Rules, row.Produced = timers, len(timers) > 0
		case k == kpolicy.RowSourceAlert:
			row.Rules, row.Produced = alerts, len(alerts) > 0
		case strings.HasSuffix(k, ":"+kpolicy.TaskOther):
			row.Produced = custom
		case strings.HasPrefix(k, kpolicy.RowTaskResult), strings.HasPrefix(k, kpolicy.RowTaskFailed):
			row.Produced = false // 内置类型的产出归各自实例行（定时安排卡上设）；类型行只作新建安排的初值
		default:
			row.Produced = true
		}
		row.Produced = row.Produced || row.Events > 0
		o.Rows = append(o.Rows, row)
	}
	for _, in := range ins {
		blocked := instanceBlocked(in, states)
		for _, prefix := range []string{kpolicy.RowInstance, kpolicy.RowInstanceFail} {
			k := prefix + in.ID
			o.Rows = append(o.Rows, overviewRow{Key: k, Produced: true, Blocked: blocked, RowCount: counts[k]})
		}
	}
	if err := nowOf(tx, &o.Now, states, staleAfter, now, o.Since); err != nil {
		return o, err
	}
	srcs, err := registry.List(tx)
	if err != nil {
		return o, err
	}
	o.SourceAlert = sourceAlert{StreakFailures: runner.StreakFailures, LowFrequencyOverSec: int(runner.LowFrequencyOver / time.Second), Sources: []sourceLine{}}
	for _, s := range srcs {
		if s.Schedule == "" { // 推送来源没有拉取，不会连续失败
			continue
		}
		d, _ := time.ParseDuration(s.Schedule)
		o.SourceAlert.Sources = append(o.SourceAlert.Sources, sourceLine{ID: s.ID, Name: s.Name, Schedule: s.Schedule, AlertAfter: runner.AlertAfter(d)})
	}
	return o, nil
}

// instanceBlocked：实例停用，或它所属能力包的定时规则没有一条是开，到点就不会产出结果。
func instanceBlocked(in instances.Instance, states []krules.State) string {
	if !in.Enabled {
		return "instance_disabled"
	}
	why := ""
	for _, s := range states {
		if s.Pack != in.Pack || s.Group != "clock_rules" {
			continue
		}
		switch s.Mode {
		case krules.ModeAuto:
			return ""
		case krules.ModeObserve:
			why = "rule_observe"
		default:
			if why == "" {
				why = "rule_off"
			}
		}
	}
	return why
}

func nowOf(tx *store.Tx, n *nowCounts, states []krules.State, staleAfter time.Duration, now, since time.Time) error {
	all, err := items.List(tx, items.Filter{})
	if err != nil {
		return err
	}
	for _, it := range all {
		switch it.Phase {
		case domain.PhaseNeedsInput:
			n.DecisionsPending++
		case domain.PhaseWaiting:
			n.Waiting++
		}
		if it.Phase != domain.PhaseOpen && it.Phase != domain.PhaseWaiting {
			continue
		}
		if it.Deadline != nil {
			n.Deadlines++
			if it.Deadline.Before(now) {
				n.Overdue++
			}
		}
		if it.Phase == domain.PhaseOpen { // 口径同今天页的久未推进：open 事项按最近一次更新
			at := it.UpdatedAt.Add(staleAfter)
			if !at.After(now) {
				n.StaleNow++
			} else if n.StaleFrom == nil || at.Before(*n.StaleFrom) {
				n.StaleFrom = &at
			}
		}
	}
	if n.ProposalsNew, err = eventlog.CountSince(tx, domain.EvItemCreated, since, "$.item.phase", string(domain.PhaseProposed)); err != nil {
		return err
	}
	for _, s := range states {
		if s.Mode != krules.ModeObserve {
			continue
		}
		c, err := eventlog.CountSince(tx, krules.EvObserved, since, "$.rule", s.Key)
		if err != nil {
			return err
		}
		n.RuleObserved[s.Key] = c
	}
	return nil
}
