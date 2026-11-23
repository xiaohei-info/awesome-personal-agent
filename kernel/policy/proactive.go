package policy

import (
	"slices"
	"time"
)

// Proactive 是「主动提醒」（§7.4）：事项到点提醒，以及简报与复盘的产出送到哪。
// 策略里没有这一段（旧版本文档、从没保存过）时取 DefaultProactive：到点提醒关，简报与复盘注入主会话。
// 到点提醒与简报都只走一条路：注入主会话时不再按 routes 发渠道，选 route 时照旧按 routes 走、不注入。
type Proactive struct {
	// On 是到点提醒的总开关，默认关（开启前已过的提醒点不补推，见 Since）。
	On bool `json:"on"`
	// Since 是最近一次从关到开的时刻，由设置接口写入；早于它的提醒点不推，开启时不会把过去的一次灌进来。
	Since *time.Time `json:"since,omitempty"`
	// WorkStart、WorkEnd 是工作时间（本机时区 HH:MM）：只有日期的计划在当天 WorkStart 提醒，只到日的截止在当天 WorkEnd 提醒。
	WorkStart string `json:"work_start"`
	WorkEnd   string `json:"work_end"`
	// Workdays 是算工作日的星期几，1 为周一、7 为周日。
	Workdays []int `json:"workdays"`
	// Shift 为真时，落在非工作日的只有日期的计划顺延到下一个工作日的 WorkStart；截止不顺延（顺延会越过截止）。
	Shift bool `json:"shift"`
	// Start、End 分别是开始提醒与截止提醒的开关。
	Start bool `json:"start"`
	End   bool `json:"end"`
	// LeadMin 是截止提醒的提前量（分钟）：时段结束提前这么多；带钟点的截止为 0 时不额外提醒（截止已过由 remind_on_deadline 管）。
	LeadMin int `json:"lead_min"`
	// DedupMin：同一事项同一类提醒点与已推过的相距不足这么多分钟就不再推；0 不按时间去重（同一提醒点仍只推一次）。
	DedupMin int `json:"dedup_min"`
	// MaxItems、MaxBatches 是每天到点提醒的事项条数与合并后的消息条数上限，0 不限；到了就攒到下一个工作日的 WorkStart 合并推一条。
	MaxItems   int `json:"max_items"`
	MaxBatches int `json:"max_batches"`
	// TaskPush 按定时任务类型（包名，TaskTypes 之一）决定产出写回 APA 之后要不要再注入主会话；缺的类型取 TaskPushDefaults。
	// 写回 APA 始终做；不推时简报与复盘照旧按 routes.brief 走，例行任务只记在 APA。
	TaskPush map[string]bool `json:"task_push,omitempty"`
	// InstancePush 按实例覆盖 TaskPush，键是实例 id。
	InstancePush map[string]bool `json:"instance_push,omitempty"`
}

// TaskTypes 是产出能注入主会话的定时任务类型（内置包名）。
var TaskTypes = []string{"brief", "review", "routine"}

// TaskPushDefaults：简报与复盘默认注入主会话（本人要在聊天里看到），例行任务默认只记在 APA。
var TaskPushDefaults = map[string]bool{"brief": true, "review": true, "routine": false}

// PushTask 报告实例 instance（类型 pack）的产出要不要注入主会话：按实例 ?? 按类型 ?? 默认。
func (p Proactive) PushTask(pack, instance string) bool {
	if v, ok := p.InstancePush[instance]; ok && instance != "" {
		return v
	}
	if v, ok := p.TaskPush[pack]; ok {
		return v
	}
	return TaskPushDefaults[pack]
}

// DefaultProactive 是没有保存过时的主动提醒设置。
func DefaultProactive() Proactive {
	return Proactive{WorkStart: "09:00", WorkEnd: "18:00", Workdays: []int{1, 2, 3, 4, 5}, Shift: true,
		Start: true, End: true, DedupMin: 60, MaxItems: 20, MaxBatches: 6}
}

// ProactiveSettings 返回生效的主动提醒设置。
func (d Disturb) ProactiveSettings() Proactive {
	if d.Proactive == nil {
		return DefaultProactive()
	}
	return *d.Proactive
}

// Workday 报告 t 所在的那天（按 t 的时区）是不是工作日。
func (p Proactive) Workday(t time.Time) bool {
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return slices.Contains(p.Workdays, wd)
}

// check 校验并规整主动提醒设置；Briefs 为空取 main。
func (p *Proactive) check() error {
	a, errA := ParseClock(p.WorkStart)
	b, errB := ParseClock(p.WorkEnd)
	if errA != nil || errB != nil || a >= b {
		return invalid("disturb proactive work_start %q must be earlier than work_end %q (HH:MM)", p.WorkStart, p.WorkEnd)
	}
	days := slices.Clone(p.Workdays)
	slices.Sort(days)
	days = slices.Compact(days)
	if len(days) == 0 || days[0] < 1 || days[len(days)-1] > 7 {
		return invalid("disturb proactive workdays must be 1 (Monday) to 7 (Sunday), at least one")
	}
	p.Workdays = days
	switch {
	case p.LeadMin < 0 || p.LeadMin > 1440:
		return invalid("disturb proactive lead_min %d must be 0..1440", p.LeadMin)
	case p.DedupMin < 0 || p.DedupMin > 1440:
		return invalid("disturb proactive dedup_min %d must be 0..1440", p.DedupMin)
	case p.MaxItems < 0 || p.MaxBatches < 0:
		return invalid("disturb proactive max_items and max_batches must not be negative")
	}
	for k := range p.TaskPush {
		if !slices.Contains(TaskTypes, k) {
			return invalid("disturb proactive task_push %q: unknown task type", k)
		}
	}
	for k := range p.InstancePush {
		if k == "" {
			return invalid("disturb proactive instance_push has an empty instance id")
		}
	}
	if !p.On {
		p.Since = nil
	}
	return nil
}

// CheckProactive 按保存时的同一套规则校验并规整一份主动提醒设置（设置页预览用，不写入）。
func CheckProactive(p Proactive) (Proactive, error) {
	p.Workdays = slices.Clone(p.Workdays)
	err := p.check()
	return p, err
}
