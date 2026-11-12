// Package metrics 是条件监视的数值一侧（设计 §3.12、§9.2、§9.3）：
//   - Series：变化类字段条件（changed、crosses_above、crosses_below）取同一序列上一次的取值。取值由连接模块在来源事件入账时
//     连同事件一起保存（sources_runner_event.prev_metrics），重启、重判和重放都得到同一结果，这里只读不写。
//   - Explain：把命中的数值条件连同上一次与本次取值整理成回放记录，`apa why` 据此回放。
//   - Alerter：规则 alert 出口的投递一侧——为来源事件产出一条没有事项、带 origin 的提醒类投递，经触达受静默、日上限和静音约束。
//
// 本包不判断规则命中（kernel/rules），不决定处置（kernel/inbound），也不写来源事件。
package metrics

import (
	"fmt"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Series 满足 rules.Series：上一次取值就是事件入账时记下的 PrevMetrics。序列第一次出现的指标没有条目，变化类条件因此不成立。
type Series struct{}

// Previous 返回事件 ev 入账时同一序列上一次的指标取值。
func (Series) Previous(ev domain.SourceEvent, metric string) (float64, bool, error) {
	v, ok := ev.PrevMetrics[metric]
	return v, ok, nil
}

// Condition 是回放记录里的一条数值条件：条件文本、上一次取值（序列第一次出现时为空）与本次取值。
type Condition struct {
	Condition string   `json:"condition"`
	Previous  *float64 `json:"previous,omitempty"`
	Current   float64  `json:"current"`
}

// Explain 列出规则 where[] 里作用于 metrics 的条件及其两次取值；事件没带该指标的条件不列。
func Explain(r packs.SourceRule, ev domain.SourceEvent) []Condition {
	var out []Condition
	for _, c := range r.Where {
		name, ok := packs.MetricName(c.Field)
		if !ok {
			continue
		}
		cur, present := ev.Metrics[name]
		if !present {
			continue
		}
		text := c.Field + " " + c.Op
		if c.Value != nil {
			text += fmt.Sprintf(" %v", c.Value)
		}
		x := Condition{Condition: text, Current: cur}
		if p, ok := ev.PrevMetrics[name]; ok {
			x.Previous = &p
		}
		out = append(out, x)
	}
	return out
}

// Alerter 产出 alert 出口的提醒。
type Alerter struct{ Attention *attention.Attention }

// Deliver 在调用方（处置命令）的事务里为命中规则 rule 的来源事件 ev 产出提醒，返回投递 id。
// 内容取来源事件的标题、摘要和链接；没有事项，出处记在 origin。同一来源事件重复调用不会重复提醒。
func (a Alerter) Deliver(tx *store.Tx, ev domain.SourceEvent, rule string) (string, error) {
	title := ev.Subject
	if title == "" {
		title = ev.Type
	}
	err := a.Attention.Alert(tx, domain.DeliveryOrigin{SourceEvent: ev.ID, Rule: rule, Source: ev.SourceID},
		domain.DeliveryContent{Title: title, Summary: ev.Summary, WebURL: ev.Link})
	return attention.AlertID(ev.ID), err
}
