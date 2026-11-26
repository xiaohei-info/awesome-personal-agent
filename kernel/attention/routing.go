package attention

// 去处与节流的唯一一处（§7.4 投递矩阵）：每条提醒按投递 id 认出它属于矩阵的哪一行，
// 节流属性（静默、合并方式）由下面的属性表声明，去处由 policy.Delivery.Lookup 给出两层目标，在这里解析成路由并入队。
// 日上限与暂停仍由投递队列按投递类别判（CreateGated：健康类不计日上限、不受暂停）。各模块只产出提醒（Notify 或 offer），
// 不自己判静默、不自己选路由。

import (
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// AgentSuffix 是两层都推时 Agent 层那条投递的 id 后缀；只推一层时 id 不带后缀。
const AgentSuffix = "@agent"

// Notice 是其他模块交给触达的一条提醒（健康推送、健康告警）：与触达自己产出的提醒走同一条流水线。
// ID 是幂等键：同一 ID 只提醒一次。
type Notice struct {
	ID, Item string
	Class    domain.DeliveryClass
	Priority domain.Priority
	Content  domain.DeliveryContent
	Origin   *domain.DeliveryOrigin
}

// Notify 让 n 走触达：按它所在的行判静默与合并，放行时按矩阵选去处。在调用方的事务里运行。
func (a *Attention) Notify(tx *store.Tx, n Notice) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	return a.offer(tx, pol, held{id: n.ID, item: n.Item, class: n.Class, priority: n.Priority, version: 1, content: n.Content, origin: n.Origin})
}

// 合并方式。
type merge int

const (
	mergeNone  merge = iota
	mergeLow         // 低优先级提醒在合并窗口内合成一条摘要
	mergeQuiet       // 静默期间攒下的，静默结束时一起放行的合成一条
	mergeBatch       // 到点提醒：每个 Tick 到期的合成一条，另受每日件数与条数上限（proactive.max_items/max_batches）
)

type throttle struct {
	quiet bool // 受静默时段约束（临近截止的决策可豁免）
	merge merge
}

// throttles 是各行的节流属性，按投递 id 前缀认行；没列出的（各类提醒、来源即时提醒、Agent 替你答了）取 reminderThrottle。
// 定时任务的产出（brief:、task:）不合并：它们按自己的行送，混进低优先级摘要会丢掉去处（§1.4 的缺陷）。
var throttles = []struct {
	prefix string
	t      throttle
}{
	{"decision:", throttle{quiet: true}},
	{"brief:", throttle{quiet: true}},
	{"task:", throttle{quiet: true}},
	{schedPrefix, throttle{quiet: true, merge: mergeBatch}},
	{"push:", throttle{quiet: true, merge: mergeQuiet}}, // 来源失败、运行失败、没出来、新建议攒多了
	{"health:", throttle{}},                             // 系统告警：不判静默
}

var reminderThrottle = throttle{quiet: true, merge: mergeLow}

func throttleOf(id string) throttle {
	for _, t := range throttles {
		if strings.HasPrefix(id, t.prefix) {
			return t.t
		}
	}
	return reminderThrottle
}

// row 是一条提醒在矩阵里的位置：按 keys 依次找行；floor 为真时两层都不推仍留一条 Web 未读，为假时只记在 APA。
type row struct {
	keys  []string
	floor bool
	pack  string       // 定时任务类型（推主会话时内容换成正文）；不是定时任务产出时为空
	task  *domain.Item // 定时任务的产出事项
	inst  string
}

// templateKinds 是模板型运行的事项：失败归"没出来"（与健康推送的判定一致）。
var templateKinds = map[domain.Kind]bool{domain.KindBrief: true, "routine": true}

// rowOf 认出一条提醒所在的行。
func rowOf(tx *store.Tx, h held) (row, error) {
	id := h.id
	one := func(k string) (row, error) { return row{keys: []string{k}, floor: true}, nil }
	switch {
	case strings.HasPrefix(id, "decision:"):
		return one(policy.RowDecision)
	case strings.HasPrefix(id, "agent-answer:"):
		return one(policy.RowAgentAnswered)
	case strings.HasPrefix(id, "remind:due:"), strings.HasPrefix(id, "remind:ask:"):
		return one(policy.RowOverdue)
	case strings.HasPrefix(id, "remind:followup:"):
		return one(policy.RowFollowup)
	case strings.HasPrefix(id, "remind:stale:"):
		return one(policy.RowStale)
	case strings.HasPrefix(id, "remind:"):
		return one(policy.RowRuleReminder)
	case strings.HasPrefix(id, schedPrefix), strings.HasPrefix(id, schedBatchPrefix):
		return one(policy.RowScheduled)
	case strings.HasPrefix(id, "task:push:"), strings.HasPrefix(id, "batch-result:"):
		return one(policy.RowSourceResult)
	case strings.HasPrefix(id, "push:proposals:"):
		return one(policy.RowNewProposals)
	case strings.HasPrefix(id, "push:source:"):
		return one(policy.RowSourceFailing)
	case strings.HasPrefix(id, "health:"):
		return one(policy.RowHealth)
	case strings.HasPrefix(id, "push:run:"):
		it, err := items.Get(tx, h.item)
		if isNotFound(err) || err == nil && !templateKinds[it.Kind] {
			return one(policy.RowRunFailed)
		}
		if err != nil {
			return row{}, err
		}
		return taskRow(tx, it, true)
	case strings.HasPrefix(id, "brief:"), strings.HasPrefix(id, "task:"):
		it, err := items.Get(tx, h.item)
		if isNotFound(err) {
			return row{floor: strings.HasPrefix(id, "brief:")}, nil
		}
		if err != nil {
			return row{}, err
		}
		return taskRow(tx, it, false)
	}
	return one(policy.RowSourceAlert) // 规则 alert 出口与紧急建议（带出处、没有事项）
}

// taskRow 是定时任务产出（failed 为假）或"没出来"（failed 为真）所在的行：按实例覆盖 → 按类型 → 其他。
// 简报类的产出两层都不推时仍留 Web 未读；例行任务等普通产出只记在 APA。
func taskRow(tx *store.Tx, it domain.Item, failed bool) (row, error) {
	inst, pack, found, err := instances.TaskOf(tx, it)
	if err != nil {
		return row{}, err
	}
	r := row{task: &it, floor: failed || it.Kind == domain.KindBrief}
	if found {
		r.pack, r.inst = pack, inst
		r.keys = append(r.keys, map[bool]string{false: policy.RowInstance, true: policy.RowInstanceFail}[failed]+inst)
	}
	prefix := map[bool]string{false: policy.RowTaskResult, true: policy.RowTaskFailed}[failed]
	known := found && slices.Contains(policy.TaskTypes, pack)
	if !failed && it.Kind == domain.KindBrief {
		known = found && (pack == "brief" || pack == "review") // 简报类产出只归简报、复盘两类，其余归"其他"
	}
	if known {
		r.keys = append(r.keys, prefix+pack)
	}
	if failed || it.Kind == domain.KindBrief {
		r.keys = append(r.keys, prefix+policy.TaskOther)
	}
	return r, nil
}

// layers 是一行解析出的两层路由；空串表示这一层不推。main 层已到主会话时 toMain 为真（定时任务产出据此换正文）。
type layers struct {
	user, agent string
	toMain      bool
}

// resolve 把 r 所在行的两层目标解析成路由。主会话唤醒不了时 Agent 层改走 Else（空即 Web 未读）；
// 那时本人层已经在推就不再另落 Web 未读。
func resolve(tx *store.Tx, pol policy.Policy, r row, prio domain.Priority) (layers, error) {
	var packs map[string]string
	if r.inst != "" {
		packs = map[string]string{r.inst: r.pack}
	}
	m := pol.Disturb.Matrix(packs)
	t, _ := m.Lookup(prio, r.keys...)
	user := func(v string) string {
		switch {
		case v == "" || v == policy.UserOff:
			return ""
		case v == policy.AgentNextChat:
			return delivery.RouteInject
		case v == policy.UserDefault:
			if m.DefaultChannel == "" {
				return delivery.RouteWeb
			}
			return m.DefaultChannel
		}
		return v
	}
	l := layers{user: user(t.User)}
	switch t.Agent {
	case policy.AgentNextChat:
		l.agent = delivery.RouteInject
	case policy.AgentMain:
		route, blocked, err := MainRoute(tx, pol)
		if err != nil {
			return l, err
		}
		switch {
		case blocked == "":
			l.agent, l.toMain = route, true
		case t.Else != "":
			l.agent = user(t.Else)
		case l.user == "":
			l.agent = delivery.RouteWeb
		}
	}
	if l.agent == l.user {
		l.agent = ""
	}
	return l, nil
}

// emit 按 r 所在行把 d（基础 id，路由待定）写成一层或两层投递；两层都不推时按 floor 留 Web 未读或什么都不出。
// 返回是否写了投递。
func (a *Attention) emit(tx *store.Tx, pol policy.Policy, r row, prio domain.Priority, d domain.Delivery, t delivery.Target) (bool, error) {
	l, err := resolve(tx, pol, r, prio)
	if err != nil {
		return false, err
	}
	if l.user == "" && l.agent == "" {
		if !r.floor {
			return false, nil
		}
		l.user = delivery.RouteWeb
	}
	var out []domain.Delivery
	if l.user != "" {
		u := d
		u.Route = l.user
		out = append(out, u)
	}
	if l.agent != "" {
		g := d
		g.Route = l.agent
		if l.user != "" {
			g.ID += AgentSuffix
		}
		if l.toMain && r.task != nil { // 定时任务的产出进主会话：内容换成带类别的正文
			g.Content = TaskContent(*r.task, r.pack, a.o.WebURL(r.task.ID))
		}
		out = append(out, g)
	}
	for _, x := range out {
		if _, _, err := a.o.Queue.CreateGated(tx, pol, x, t); err != nil {
			return false, err
		}
	}
	return true, nil
}
