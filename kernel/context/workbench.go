package kcontext

// 工作台概览与增量负载（§7.6、§11.6）：
//
//   - 不属于任何项目的会话第一次读（从头读）给工作台概览：等你决定、在途派活、今天与逾期、各项目未结事项、本会话在跟，
//     每组至多 OverviewGroupRows 行，多出的给条数与查询命令；之后的增量覆盖所有项目，只收事项级变化，
//     普通字段改动只对"本会话在跟"的事项给出。
//   - 增量里"渲染出来与游标那一刻相同"的事项不带：事件变了而注入里能看见的那一行没变，不算变化；
//     挂靠消息只有这一次真的转交了才算变化。
//   - 整份注入有 InjectBudget 字的上限：事项行按组截断并写明另有几条；投递与跟进消息在读取时已记为送达，不截。
//   - 未挂事项的派活在记下它的会话里每轮都提示，直到挂上或标为不跟踪（§6.17）。

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// OverviewGroupRows 是工作台概览每组的行数上限。
	OverviewGroupRows = 5
	// InjectBudget 是一次注入的字数上限（按字符计）：内核自己守住，不靠宿主截断。讨论区的一页读取（apa item thread）用同一个上限。
	InjectBudget = 4000
	// trackingLimit 是"本会话在跟"最多查多少个事项。
	trackingLimit = 50
)

// 概览的分组。
const (
	GroupDecide     = "decide"     // 等你决定：待回答的问题与待处理的建议
	GroupDispatches = "dispatches" // 在途派活
	GroupToday      = "today"      // 今天与逾期
	GroupProject    = "project"    // 某个项目的未结事项
	GroupTracking   = "tracking"   // 本会话在跟
)

// OverviewGroup 是概览的一组。More 是没列出的条数，Query 是读全的命令。
type OverviewGroup struct {
	Key     string  `json:"key"`
	Project string  `json:"project,omitempty"`
	Name    string  `json:"name,omitempty"` // 项目组的项目名
	Items   []Entry `json:"items"`
	More    int     `json:"more,omitempty"`
	Query   string  `json:"query,omitempty"`
}

// UnattachedDispatch 是本会话记下、还没挂到事项上的派活。
type UnattachedDispatch struct {
	ID       string `json:"id"`
	Executor string `json:"executor"`
	Session  string `json:"session,omitempty"`
	About    string `json:"about,omitempty"`
}

func live(it domain.Item) bool { return !closed(it) }

// Tracking 返回本会话在跟的事项：会话（不计插件加的 ~序号后缀）写过、还没结束的事项，最近写过的在前。
func Tracking(tx *store.Tx, session string, limit int) ([]domain.Item, error) {
	ts, err := eventlog.SessionTouches(tx, conversation(session), 0, 0, trackingLimit)
	if err != nil {
		return nil, err
	}
	var out []domain.Item
	for _, t := range ts {
		it, err := items.Get(tx, t.Object)
		if isNotFound(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if live(it) {
			out = append(out, it)
		}
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

// overview 组装工作台概览。一件事只出现在它命中的第一组里。
func overview(tx *store.Tx, req Request, pol policy.Policy) ([]OverviewGroup, error) {
	shown := map[string]bool{}
	var groups []OverviewGroup
	add := func(g OverviewGroup, list []domain.Item) error {
		for _, it := range list {
			if shown[it.ID] {
				continue
			}
			shown[it.ID] = true
			if len(g.Items) == OverviewGroupRows {
				g.More++
				continue
			}
			e, _, err := entry(tx, it, pol, req.Reader)
			if err != nil {
				return err
			}
			g.Items = append(g.Items, e)
		}
		if len(g.Items) > 0 {
			groups = append(groups, g)
		}
		return nil
	}
	all, err := items.List(tx, items.Filter{})
	if err != nil {
		return nil, err
	}
	byNewest := func(a, b domain.Item) int { return b.UpdatedAt.Compare(a.UpdatedAt) }

	var decide []domain.Item
	for _, it := range all {
		if it.Phase == domain.PhaseNeedsInput || it.Phase == domain.PhaseProposed {
			decide = append(decide, it)
		}
	}
	slices.SortStableFunc(decide, func(a, b domain.Item) int { return a.CreatedAt.Compare(b.CreatedAt) })
	if err := add(OverviewGroup{Key: GroupDecide, Query: "apa inbox"}, decide); err != nil {
		return nil, err
	}

	var inflight []domain.Item
	for _, it := range all {
		if it.Kind != domain.KindDelegation {
			continue
		}
		// 已结束、父事项还没结束的委派子事项是待接手的回收结果，与在途的一起列（§7.6）。
		if stale, err := settled(tx, it); err != nil {
			return nil, err
		} else if !stale {
			inflight = append(inflight, it)
		}
	}
	slices.SortStableFunc(inflight, byNewest)
	if err := add(OverviewGroup{Key: GroupDispatches, Query: "apa item list --kind delegation --phase open"}, inflight); err != nil {
		return nil, err
	}

	today, err := items.TodayItems(tx, clock.Now())
	if err != nil {
		return nil, err
	}
	if err := add(OverviewGroup{Key: GroupToday, Query: "apa today"}, slices.Concat(today.Overdue, today.Due, today.Planned)); err != nil {
		return nil, err
	}

	tracking, err := Tracking(tx, req.Session, 0)
	if err != nil {
		return nil, err
	}
	// 本会话在跟排在各项目之前：它最可能是这段对话要接着做的事。
	if err := add(OverviewGroup{Key: GroupTracking}, tracking); err != nil {
		return nil, err
	}

	ps, err := projects.List(tx)
	if err != nil {
		return nil, err
	}
	open := map[string][]domain.Item{}
	for _, it := range all {
		if it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting {
			open[it.Project] = append(open[it.Project], it)
		}
	}
	slices.SortStableFunc(ps, func(a, b domain.Project) int {
		return latest(open[b.ID]).Compare(latest(open[a.ID]))
	})
	for _, p := range ps {
		list := open[p.ID]
		slices.SortStableFunc(list, byNewest)
		if err := add(OverviewGroup{Key: GroupProject, Project: p.ID, Name: p.Name, Query: "apa item list --phase open --project " + p.ID}, list); err != nil {
			return nil, err
		}
	}
	slices.SortStableFunc(open[""], byNewest)
	if err := add(OverviewGroup{Key: GroupProject, Query: "apa item list --phase open"}, open[""]); err != nil {
		return nil, err
	}
	return groups, nil
}

func latest(list []domain.Item) (t time.Time) {
	for _, it := range list {
		if it.UpdatedAt.After(t) {
			t = it.UpdatedAt
		}
	}
	return t
}

// unattached 返回本会话记下、还没挂到事项上的派活（§6.17）。
func unattached(tx *store.Tx, session string) ([]UnattachedDispatch, error) {
	ds, err := delegation.Unattached(tx)
	if err != nil {
		return nil, err
	}
	var out []UnattachedDispatch
	for _, d := range ds {
		if conversation(d.Origin) == conversation(session) {
			out = append(out, UnattachedDispatch{ID: d.ID, Executor: d.Executor, Session: d.Session, About: d.About})
		}
	}
	return out, nil
}

// InFlightDispatch 是本会话派出、已挂到事项上、还没有结果回执的派活（插件进程重启后的第一轮带上，§6.17）。
type InFlightDispatch struct {
	ID       string `json:"id"`
	Item     string `json:"item"`
	Executor string `json:"executor"`
	Session  string `json:"session,omitempty"`
	Minutes  int    `json:"age_min"` // 派出至今的分钟数
}

// inFlight 返回本会话派出、子事项还没结束的外部派活（APA 拉起的有运行与结果通知，不在此列）。
func inFlight(tx *store.Tx, session string) ([]InFlightDispatch, error) {
	ds, err := delegation.InFlight(tx)
	if err != nil {
		return nil, err
	}
	now := clock.Now()
	var out []InFlightDispatch
	for _, d := range ds {
		if conversation(d.Origin) != conversation(session) {
			continue
		}
		it, err := items.Get(tx, d.Item)
		if isNotFound(err) || err == nil && closed(it) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, InFlightDispatch{ID: d.ID, Item: d.Item, Executor: d.Executor, Session: d.Session, Minutes: int(now.Sub(d.StartedAt).Minutes())})
	}
	return out, nil
}

// ---- 增量去重 ----

// rowKey 是注入里一个事项行能看见的东西（标题、阶段、截止、状态标签）加最近一条进展：两者相同就是没有可见变化。
func rowKey(it domain.Item, progress string) string {
	d := ""
	if it.Deadline != nil {
		d = it.Deadline.UTC().String()
	}
	return strings.Join([]string{it.Title, string(it.Phase), d, it.StatusLabel, progress}, "\x00")
}

// unchanged 报告事项在游标那一刻渲染出的行与现在相同：游标之前最后一个快照与最后一条进展，对比现在的。
// 游标之后才建的事项没有之前的快照，算变化。
func unchanged(tx *store.Tx, it domain.Item, after int64) (bool, error) {
	es, err := eventlog.ReadObject(tx, it.ID)
	if err != nil {
		return false, err
	}
	var before *domain.Item
	var progBefore, progNow string
	for _, en := range es {
		if en.Tombstone {
			continue
		}
		var p struct {
			Item     *domain.Item     `json:"item"`
			Progress *domain.Progress `json:"progress"`
		}
		if json.Unmarshal(en.Event.Payload, &p) != nil {
			continue
		}
		if p.Progress != nil {
			progNow = p.Progress.ID
		}
		if en.Event.Seq > after {
			continue
		}
		if p.Item != nil && p.Item.ID == it.ID {
			before = p.Item
		}
		if p.Progress != nil {
			progBefore = p.Progress.ID
		}
	}
	return before != nil && rowKey(*before, progBefore) == rowKey(it, progNow), nil
}

// ---- 渲染与字数上限 ----

var groupTitles = map[string]string{GroupDecide: "等你决定", GroupDispatches: "在途派活", GroupToday: "今天与逾期", GroupTracking: "本会话在跟"}

// budget 记着还剩多少字。
type budget struct{ left int }

func runes(s string) int { return len([]rune(s)) }

// take 能放下 s 就扣掉并返回真。
func (b *budget) take(s string) bool {
	n := runes(s)
	if n > b.left {
		return false
	}
	b.left -= n
	return true
}

func itemLines(e Entry) string {
	deadline := "无"
	if e.Deadline != nil {
		deadline = e.Deadline.UTC().Format(time.RFC3339)
	}
	var b strings.Builder
	b.WriteString("- " + e.ID + " | " + oneLine(e.Title) + " | 阶段 " + string(e.Phase) + " | 截止 " + deadline + " | 建议：" + e.Action + "\n")
	if e.Progress != "" {
		b.WriteString("  进展：" + oneLine(e.Progress) + "\n")
	}
	if e.Running {
		b.WriteString("  " + runningNotice + "\n")
	}
	if d := e.Last; d != nil {
		b.WriteString("  " + lastDispatchLine(*d, e.ID) + "\n")
	}
	return b.String()
}

func moreLine(n int, query string) string {
	s := "  另有 " + strconv.Itoa(n) + " 条未列出"
	if query != "" {
		s += "：" + query
	}
	return s + "\n"
}

// renderGroups 按上限渲染概览：放不下的行计入该组的"另有"。
func renderGroups(groups []OverviewGroup, b *budget) string {
	var out strings.Builder
	for gi, g := range groups {
		title := groupTitles[g.Key]
		switch {
		case g.Key == GroupProject && g.Project == "":
			title = "不属于任何项目的未结"
		case g.Key == GroupProject:
			title = "项目 " + oneLine(g.Name) + "（" + g.Project + "）未结"
		}
		head := "## " + title + "\n"
		if !b.take(head) {
			rest := 0
			for _, r := range groups[gi:] {
				rest += len(r.Items) + r.More
			}
			out.WriteString(moreLine(rest, "apa inbox、apa today、apa item list --phase open --project <id>"))
			break
		}
		out.WriteString(head)
		more := g.More
		for i, e := range g.Items {
			l := itemLines(e)
			if !b.take(l) {
				more += len(g.Items) - i
				break
			}
			out.WriteString(l)
		}
		if more > 0 {
			l := moreLine(more, g.Query)
			b.left -= runes(l) // 截断说明必须带上，哪怕略超
			out.WriteString(l)
		}
	}
	return out.String()
}
