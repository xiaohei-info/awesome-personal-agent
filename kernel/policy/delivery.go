package policy

import (
	"maps"
	"slices"
	"sort"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 投递矩阵（§7.4 "投递矩阵"）：每一行是一类提醒，各有两层去处——第一层推给 Agent（off、main 马上唤醒主会话、
// next_chat 下次对话时带上），第二层推给本人（off、default 默认渠道、channel:<连接 id>）。
// 存在 disturb.delivery；没有这个键时按旧键（routes、push.*.route、proactive.task_push/instance_push）换算（DeriveDelivery），
// 行为与换算前完全一样。写入之后旧键原样冻结、内核不再读它们，旧二进制回滚时读到的就是迁移那一刻的设置。
// 本段只管"送到哪"；产不产生（push.*.on、proactive.on、remind_on_deadline、规则模式）与节流（静默、日上限、暂停、合并）仍在原处。

// 第一层（推给 Agent）的取值。
const (
	AgentOff      = "off"
	AgentMain     = "main"      // 马上：唤醒主会话，Agent 在对话里转告
	AgentNextChat = "next_chat" // 下次对话时带上（注入）
)

// 第二层（推给本人）的取值；另可为 channel:<连接 id>。
const (
	UserOff     = "off"
	UserDefault = "default" // disturb.delivery.default_channel
)

// 行键。task_result、task_failed 后接定时任务类型（TaskTypes）或 other（不出自内置类型的简报与任务）；
// 按实例覆盖的行是 instance:<实例 id>（结果）与 instance_failed:<实例 id>（没出来）。
const (
	RowDecision      = "decision"
	RowAgentAnswered = "agent_answered"
	RowScheduled     = "scheduled"
	RowOverdue       = "overdue"
	RowFollowup      = "followup"
	RowStale         = "stale"
	RowRuleReminder  = "rule_reminder"
	RowTaskResult    = "task_result:"
	RowTaskFailed    = "task_failed:"
	RowInstance      = "instance:"
	RowInstanceFail  = "instance_failed:"
	RowSourceFailing = "source_failing"
	RowRunFailed     = "run_failed"
	RowHealth        = "health"
	RowNewProposals  = "new_proposals"
	RowSourceAlert   = "source_alert"
	// TaskOther 是 task_result:/task_failed: 后的"其他"：不是由内置定时任务类型产出的简报或任务。
	TaskOther = "other"
)

// reminderRows 是旧版本里同走 routes.reminder 的行。
var reminderRows = []string{RowAgentAnswered, RowOverdue, RowFollowup, RowStale, RowRuleReminder, RowSourceAlert}

// FixedRows 是矩阵的固定行（不含按实例覆盖的行），顺序即设置页的顺序。
var FixedRows = func() []string {
	out := []string{RowDecision, RowAgentAnswered, RowScheduled, RowOverdue, RowFollowup, RowStale, RowRuleReminder}
	for _, t := range append(slices.Clone(TaskTypes), TaskOther) {
		out = append(out, RowTaskResult+t)
	}
	for _, t := range append(slices.Clone(TaskTypes), TaskOther) {
		out = append(out, RowTaskFailed+t)
	}
	return append(out, RowSourceFailing, RowRunFailed, RowHealth, RowNewProposals, RowSourceAlert)
}()

// Target 是一行的两层去处。Else 只在 Agent 为 main 时有意义：主会话唤醒不了时改送哪（next_chat、default、channel:<id>），
// 空表示落 Web 未读。ByPriority 按事项优先级覆盖整行（承接旧的「类别:优先级」路由），不再嵌套。
type Target struct {
	Agent      string            `json:"agent"`
	User       string            `json:"user"`
	Else       string            `json:"else,omitempty"`
	ByPriority map[string]Target `json:"by_priority,omitempty"`
}

// Delivery 是 disturb.delivery：默认渠道与各行去处。缺的行取出厂值（Default 换算的结果）。
type Delivery struct {
	DefaultChannel string            `json:"default_channel"`
	Rows           map[string]Target `json:"rows"`
}

// Lookup 依次找 keys 里第一条有的行（先在本矩阵，再在出厂值），再按优先级取覆盖；都没有时 ok 为假（只记录，不出投递）。
func (m Delivery) Lookup(prio domain.Priority, keys ...string) (Target, bool) {
	for _, rows := range []map[string]Target{m.Rows, factory.Rows} {
		for _, k := range keys {
			if t, ok := rows[k]; ok {
				if p, ok := t.ByPriority[string(prio)]; ok && prio != "" {
					return p, true
				}
				t.ByPriority = nil
				return t, true
			}
		}
	}
	return Target{}, false
}

// Matrix 返回生效的投递矩阵：存过就是存的那份，没存过按旧键换算。packs 给出实例 id → 类型，用于换算按实例覆盖的行
// （只换算 packs 里有的实例；存过的矩阵不用它）。
func (d Disturb) Matrix(packs map[string]string) Delivery {
	if d.Delivery != nil {
		return *d.Delivery
	}
	return DeriveDelivery(d, packs)
}

// legacyRoute 与旧版本触达选路由时的规整一致：空为 web；inject、wake、web 与 channel: 原样；其他当作渠道名。
func legacyRoute(r string) string {
	switch {
	case r == "":
		return "web"
	case r == "inject" || r == "wake" || r == "web" || strings.HasPrefix(r, "channel:"):
		return r
	}
	return "channel:" + r
}

// DeriveDelivery 按旧键换算矩阵（纯函数，§16.5 读取时兼容）：路由 inject → (next_chat, off)；web → (off, off)；
// channel:X → (off, X 是默认渠道记 default，否则记 X)。默认渠道取所有渠道取值里出现最多的那个（并列取字典序最小）。
func DeriveDelivery(d Disturb, packs map[string]string) Delivery {
	def := mostUsedChannel(d)
	user := func(r string) string {
		switch r = legacyRoute(r); {
		case r == "web" || r == "inject":
			return UserOff
		case r == def:
			return UserDefault
		}
		return r
	}
	conv := func(r string) Target {
		if legacyRoute(r) == "inject" {
			return Target{Agent: AgentNextChat, User: UserOff}
		}
		return Target{Agent: AgentOff, User: user(r)}
	}
	orElse := func(r string) string { // 主会话唤醒不了时的退路
		switch t := conv(r); {
		case t.Agent == AgentNextChat:
			return AgentNextChat
		case t.User == UserOff:
			return ""
		default:
			return t.User
		}
	}
	// fromRoutes 换算一个类别及其「类别:优先级」覆盖；wrap 改写每一条（推主会话时把它变成退路）。
	fromRoutes := func(class string, wrap func(string) Target) Target {
		t := wrap(d.Routes[class])
		for k, r := range d.Routes {
			if c, p, ok := strings.Cut(k, ":"); ok && c == class {
				if t.ByPriority == nil {
					t.ByPriority = map[string]Target{}
				}
				t.ByPriority[p] = wrap(r)
			}
		}
		return t
	}
	main := func(r string) Target { return Target{Agent: AgentMain, User: UserOff, Else: orElse(r)} }
	mainNoElse := func(string) Target { return Target{Agent: AgentMain, User: UserOff} }
	none := Target{Agent: AgentOff, User: UserOff}
	brief := string(domain.DeliveryBrief)
	decision := d.Routes[string(domain.DeliveryDecision)]
	pushRoute := func(cat string) Target { // 推送类：选了渠道就用它，否则跟"需要你决定"
		if r := d.PushRuleFor(cat).Route; r != "" {
			return conv(r)
		}
		return conv(decision)
	}
	pr := d.ProactiveSettings()
	tmpl := d.PushRuleFor(PushTemplateRun).Route
	result := func(pack string, push bool) Target {
		switch {
		case push && briefPack(pack):
			return fromRoutes(brief, main)
		case push:
			return mainNoElse("")
		case briefPack(pack):
			return fromRoutes(brief, conv)
		}
		return none // 例行任务不推：只记在 APA
	}
	failed := func(push bool) Target {
		switch {
		case tmpl != "":
			return conv(tmpl)
		case push:
			return main(decision)
		}
		return conv(decision)
	}

	rows := map[string]Target{
		RowDecision:               fromRoutes(string(domain.DeliveryDecision), conv),
		RowScheduled:              mainNoElse(""),
		RowTaskResult + TaskOther: fromRoutes(brief, conv),
		RowTaskFailed + TaskOther: failed(false),
		RowSourceFailing:          pushRoute(PushSourceFailing),
		RowRunFailed:              pushRoute(PushRunFailed),
		RowNewProposals:           pushRoute(PushNewProposals),
		RowHealth:                 conv(d.Routes[string(domain.DeliveryHealth)]),
	}
	for _, r := range reminderRows {
		rows[r] = fromRoutes(string(domain.DeliveryReminder), conv)
	}
	for _, t := range TaskTypes {
		rows[RowTaskResult+t] = result(t, pr.PushTask(t, ""))
		rows[RowTaskFailed+t] = failed(pr.PushTask(t, ""))
	}
	for id, push := range pr.InstancePush {
		if pack, ok := packs[id]; ok && id != "" {
			rows[RowInstance+id] = result(pack, push)
			rows[RowInstanceFail+id] = failed(push)
		}
	}
	return Delivery{DefaultChannel: def, Rows: rows}
}

// briefPack：产出是简报类事项的定时任务类型（简报、复盘）；其余（例行任务、自建类型）的产出是普通事项。
func briefPack(pack string) bool { return pack == "brief" || pack == "review" }

// mostUsedChannel 是旧键里出现最多的渠道路由；没有渠道时为空。
func mostUsedChannel(d Disturb) string {
	n := map[string]int{}
	for _, r := range d.Routes {
		if r = legacyRoute(r); strings.HasPrefix(r, "channel:") {
			n[r]++
		}
	}
	for _, r := range d.Push {
		if r.Route != "" && strings.HasPrefix(legacyRoute(r.Route), "channel:") {
			n[legacyRoute(r.Route)]++
		}
	}
	keys := slices.Sorted(maps.Keys(n))
	best := ""
	for _, k := range keys {
		if best == "" || n[k] > n[best] {
			best = k
		}
	}
	return best
}

// factory 是出厂矩阵：存过的矩阵缺行时取它。
var factory = DeriveDelivery(Default().Disturb, nil)

// FactoryDelivery 返回出厂矩阵（新装或从没保存过时的值）。
func FactoryDelivery() Delivery { return DeriveDelivery(Default().Disturb, nil) }

func validRowKey(k string) bool {
	if slices.Contains(FixedRows, k) {
		return true
	}
	for _, p := range []string{RowInstance, RowInstanceFail} {
		if id, ok := strings.CutPrefix(k, p); ok && strings.TrimSpace(id) != "" {
			return true
		}
	}
	return false
}

// userValue：off、default、channel:<id>；wake 只为承接旧路由（旧键里写过唤醒路由时换算出来的值）。
func userValue(v string) bool {
	id, ok := strings.CutPrefix(v, "channel:")
	return v == UserOff || v == UserDefault || v == "wake" || ok && strings.TrimSpace(id) != ""
}

func (t Target) check(k string, nested bool) error {
	if !oneOf(t.Agent, AgentOff, AgentMain, AgentNextChat) {
		return invalid("disturb delivery row %q: agent %q, use off, main or next_chat", k, t.Agent)
	}
	if !userValue(t.User) {
		return invalid("disturb delivery row %q: user %q, use off, default or channel:<id>", k, t.User)
	}
	if t.Else != "" && (t.Agent != AgentMain || t.Else != AgentNextChat && (t.Else == UserOff || !userValue(t.Else))) {
		return invalid("disturb delivery row %q: else %q is only for agent main and must be next_chat, default or channel:<id>", k, t.Else)
	}
	if nested && len(t.ByPriority) > 0 {
		return invalid("disturb delivery row %q: by_priority cannot nest", k)
	}
	for p, bt := range t.ByPriority {
		if !domain.Priority(p).Valid() {
			return invalid("disturb delivery row %q: unknown priority %q", k, p)
		}
		if err := bt.check(k+":"+p, true); err != nil {
			return err
		}
	}
	return nil
}

func (m *Delivery) check() error {
	if id, ok := strings.CutPrefix(m.DefaultChannel, "channel:"); m.DefaultChannel != "" && (!ok || strings.TrimSpace(id) == "") {
		return invalid("disturb delivery default_channel %q must be empty or channel:<id>", m.DefaultChannel)
	}
	if m.Rows == nil {
		m.Rows = map[string]Target{}
	}
	keys := slices.Collect(maps.Keys(m.Rows))
	sort.Strings(keys)
	for _, k := range keys {
		if !validRowKey(k) {
			return invalid("disturb delivery row %q: unknown row", k)
		}
		if err := m.Rows[k].check(k, false); err != nil {
			return err
		}
	}
	return nil
}

// FrozenEqual 报告 a、b 的冻结键（routes、push.*.route、proactive.task_push 与 instance_push）是否相同。
// 矩阵写入之后这几个键原样冻结，设置接口据此拒绝改动它们（改 disturb.delivery）。
func FrozenEqual(a, b Disturb) bool {
	if !maps.Equal(a.Routes, b.Routes) {
		return false
	}
	for _, c := range PushCategories {
		if a.PushRuleFor(c).Route != b.PushRuleFor(c).Route {
			return false
		}
	}
	pa, pb := a.ProactiveSettings(), b.ProactiveSettings()
	return maps.Equal(pa.TaskPush, pb.TaskPush) && maps.Equal(pa.InstancePush, pb.InstancePush)
}
