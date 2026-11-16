package task

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 简报资料的五个新段（设计 §7.3"简报的资料"）：schedule、proposals、decisions、due_waiting、activity。
// 全部是账本读取，不调模型；没有内容时是空数组。
const (
	defaultScheduleDays = 3
	maxProposals        = 50
	firstBriefWindow    = 24 * time.Hour
	calendarKind        = "calendar"
	echoReasonPrefix    = "收到回音"
	eventPage           = 500
)

type window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// briefWindow 取本份简报的取材窗口：起点是同一实例上一份简报的 until，没有（实例的第一份）取 24 小时；
// 被跳过的周期不产生简报，窗口自然变长。简报事项的 origin 记所属实例（规则建事项时写入）。
func briefWindow(all []domain.Item, instance, self string, now time.Time) window {
	w := window{Since: now.Add(-firstBriefWindow), Until: now}
	var latest time.Time
	for _, it := range all {
		if it.ID == self || it.Kind != domain.KindBrief || it.Origin == nil || it.Origin.Source != domain.OriginInstancePrefix+instance {
			continue
		}
		var sp struct {
			Until time.Time `json:"until"`
		}
		if json.Unmarshal(it.Spec, &sp) == nil && sp.Until.After(latest) {
			latest = sp.Until
		}
	}
	if !latest.IsZero() && latest.Before(now) {
		w.Since = latest
	}
	return w
}

// scheduleDays 取实例参数 schedule_days：缺省 3，合法范围 1 到 14（包的参数校验已拦截越界值，这里兜底取缺省）。
func scheduleDays(params json.RawMessage) int {
	var p struct {
		Days int `json:"schedule_days"`
	}
	if json.Unmarshal(params, &p) != nil || p.Days < 1 || p.Days > 14 {
		return defaultScheduleDays
	}
	return p.Days
}

type scheduleEntry struct {
	Title    string    `json:"title"`
	Start    time.Time `json:"start"`
	End      string    `json:"end,omitempty"`
	Location string    `json:"location,omitempty"`
	Source   string    `json:"source"`
	Link     string    `json:"link,omitempty"`
}

type proposalEntry struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Summary       string   `json:"summary,omitempty"`
	Sources       string   `json:"sources,omitempty"`
	Recommend     string   `json:"recommend,omitempty"`
	Reason        string   `json:"reason,omitempty"`
	LowConfidence bool     `json:"low_confidence"`
	Events        []string `json:"-"`             // 引用的来源事件 id，用来出原文文件
	Raw           []rawRef `json:"raw,omitempty"` // 各来源事件的原文文件名与裁剪量
}

type decisionEntry struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Question string     `json:"question,omitempty"`
	Deadline *time.Time `json:"deadline,omitempty"`
	WebOnly  bool       `json:"web_only"`
}

type dueEntry struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Project  string     `json:"project,omitempty"`
	Reason   string     `json:"reason"` // due_today、due_tomorrow、overdue、echo、follow_up_due、stale
	Deadline *time.Time `json:"deadline,omitempty"`
}

type activityEntry struct {
	Item    string    `json:"item"`
	Title   string    `json:"title"`
	Kind    string    `json:"kind"` // done、progress
	Text    string    `json:"text,omitempty"`
	At      time.Time `json:"at"`
	Project string    `json:"project,omitempty"`
}

type projectActivity struct {
	Project string          `json:"project"`
	Entries []activityEntry `json:"entries"`
}

// briefLedgerSections 组出取材窗口与五个新段：窗口写进 brief 事项的 spec.since/until（调用方负责），五段各成一份材料。
func (s *Service) briefLedgerSections(ctx context.Context, c rules.Call) (w window, mats []Material, props briefProposals, err error) {
	now := s.o.Now()
	inst, self := "", ""
	var params json.RawMessage
	if c.Instance != nil {
		inst, params = c.Instance.ID, c.Instance.Params
	}
	if c.Item != nil {
		self = c.Item.ID
	}
	var (
		schedule  = []scheduleEntry{}
		proposals = []proposalEntry{}
		pending   int
		decisions = []decisionEntry{}
		due       = []dueEntry{}
		activity  = []projectActivity{}
	)
	err = s.o.Store.View(ctx, func(tx *store.Tx) error {
		all, err := items.List(tx, items.Filter{})
		if err != nil {
			return err
		}
		w = briefWindow(all, inst, self, now)
		srcs, err := registry.List(tx)
		if err != nil {
			return err
		}
		names := map[string]string{}
		for _, src := range srcs {
			names[src.ID] = src.Name
		}
		if schedule, err = scheduleSection(tx, srcs, now, scheduleDays(params)); err != nil {
			return err
		}
		if proposals, pending, err = s.proposalsSection(tx, all, w, names); err != nil {
			return err
		}
		td, err := items.TodayItems(tx, now)
		if err != nil {
			return err
		}
		for _, it := range td.Pending {
			d := decisionEntry{ID: it.ID, Title: it.Title}
			if it.Ask != nil {
				d.Question, d.Deadline, d.WebOnly = it.Ask.Question, it.Ask.Deadline, it.Ask.RiskClass == domain.RiskClassWebOnly
			}
			decisions = append(decisions, d)
		}
		if due, err = dueSection(tx, all, td, w, now); err != nil {
			return err
		}
		activity, err = activitySection(tx, all, w)
		return err
	})
	if err != nil {
		return w, nil, props, err
	}
	for _, m := range []struct {
		name string
		v    any
	}{{"schedule", schedule}, {"decisions", decisions}, {"due_waiting", due}, {"activity", activity}} {
		b, _ := json.Marshal(map[string]any{m.name: m.v})
		mats = append(mats, Material{Name: m.name, Body: string(b)})
	}
	return w, mats, briefProposals{Entries: proposals, Pending: pending}, nil
}

// scheduleSection：语义类型为 calendar 的来源里，开始时间落在 [今天 0 点, +days 天) 的事件；同一关联键只取最新一版，
// 最新一版是取消的不列。结束时间与地点取原文 headers 部分的 end、location（来源没带就不写）。
func scheduleSection(tx *store.Tx, srcs []domain.Source, now time.Time, days int) ([]scheduleEntry, error) {
	y, m, d := now.In(time.Local).Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, time.Local)
	to := from.AddDate(0, 0, days)
	out := []scheduleEntry{}
	for _, src := range srcs {
		if src.SourceKind != calendarKind {
			continue
		}
		evs, err := runner.Events(tx, src.ID)
		if err != nil {
			return nil, err
		}
		latest := map[string]domain.SourceEvent{} // 事件按落盘顺序，后写的覆盖先写的
		var order []string
		for _, e := range evs {
			k := e.ID
			if len(e.CorrelationKeys) > 0 {
				k = e.CorrelationKeys[0]
			}
			if _, seen := latest[k]; !seen {
				order = append(order, k)
			}
			latest[k] = e
		}
		for _, k := range order {
			e := latest[k]
			if e.Type == "event.cancelled" || e.OccurredAt.Before(from) || !e.OccurredAt.Before(to) {
				continue
			}
			en := scheduleEntry{Title: e.Subject, Start: e.OccurredAt, Source: src.Name, Link: e.Link}
			if p, err := runner.ReadPart(tx, e.ID, runner.PartHeaders); err == nil {
				var h map[string]any
				if json.Unmarshal(p.Content, &h) == nil {
					en.End, _ = h["end"].(string)
					en.Location, _ = h["location"].(string)
				}
			} else if !isNotFoundErr(err) {
				return nil, err
			}
			out = append(out, en)
		}
	}
	slices.SortStableFunc(out, func(a, b scheduleEntry) int { return a.Start.Compare(b.Start) })
	return out, nil
}

func isNotFoundErr(err error) bool {
	de, ok := err.(*domain.Error)
	return ok && de.Code == domain.CodeNotFound
}

// proposalsSection：窗口内新建且仍是 proposed 的建议，最新的至多 50 条；pending_total 是全部未处置的建议数。
func (s *Service) proposalsSection(tx *store.Tx, all []domain.Item, w window, names map[string]string) ([]proposalEntry, int, error) {
	var cand []domain.Item
	total := 0
	for _, it := range all {
		if it.Phase != domain.PhaseProposed {
			continue
		}
		total++
		if it.CreatedAt.After(w.Since) && !it.CreatedAt.After(w.Until) {
			cand = append(cand, it)
		}
	}
	slices.SortStableFunc(cand, func(a, b domain.Item) int { return b.CreatedAt.Compare(a.CreatedAt) })
	if len(cand) > maxProposals {
		cand = cand[:maxProposals]
	}
	out := []proposalEntry{}
	for _, it := range cand {
		e := proposalEntry{ID: it.ID, Title: it.Title, Summary: it.Summary}
		if p := it.Proposal; p != nil {
			e.Recommend, e.Reason, e.LowConfidence = p.Recommend, p.Reason, p.LowConfidence
			var ns []string
			for _, id := range p.Sources {
				ev, err := runner.Event(tx, id)
				if err != nil {
					if isNotFoundErr(err) {
						continue
					}
					return nil, 0, err
				}
				e.Events = append(e.Events, id)
				if n := names[ev.SourceID]; n != "" && !slices.Contains(ns, n) {
					ns = append(ns, n)
				}
			}
			e.Sources = strings.Join(ns, "、")
		}
		out = append(out, e)
	}
	return out, total, nil
}

// dueSection 与"今天"视图同一口径：今天与明天到期的、已过期未完成的；窗口内收到回音回到 open 的；
// 跟进到期仍在等的（等待事项的 follow-up 轮次提醒已记 followup.reminded 且之后没有进展）；久未推进的。
func dueSection(tx *store.Tx, all []domain.Item, td items.TodayList, w window, now time.Time) ([]dueEntry, error) {
	out := []dueEntry{}
	today, tomorrow := items.Today(now), items.Today(now.AddDate(0, 0, 1))
	seen := map[string]bool{}
	add := func(it domain.Item, reason string) {
		if !seen[it.ID+reason] {
			seen[it.ID+reason] = true
			out = append(out, dueEntry{ID: it.ID, Title: it.Title, Project: it.Project, Reason: reason, Deadline: it.Deadline})
		}
	}
	byID := map[string]domain.Item{}
	for _, it := range all {
		byID[it.ID] = it
		if it.Deadline == nil || !(it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting || it.Phase == domain.PhaseNeedsInput) {
			continue
		}
		switch day := items.Today(*it.Deadline); {
		case day == today:
			add(it, "due_today")
		case day == tomorrow:
			add(it, "due_tomorrow")
		case day < today:
			add(it, "overdue")
		}
	}
	// 回音：窗口内由 followmsg 把 waiting 转回 open 的规则性转移（§9.4）。
	err := eachLive(tx, domain.EvItemPhaseChanged, w, func(e domain.Event) {
		var p struct {
			Before *domain.Item `json:"before"`
			Item   domain.Item  `json:"item"`
		}
		if json.Unmarshal(e.Payload, &p) != nil || p.Before == nil || p.Before.Phase != domain.PhaseWaiting ||
			p.Item.Phase != domain.PhaseOpen || !strings.HasPrefix(e.Reason, echoReasonPrefix) {
			return
		}
		if cur, ok := byID[e.Object]; ok && cur.Phase == domain.PhaseOpen {
			add(cur, "echo")
		}
	})
	if err != nil {
		return nil, err
	}
	// 跟进已到期仍在等：仍是 waiting，且自提醒之后没有变化（离开 waiting 即清轮次）。
	err = eachLive(tx, "followup.reminded", window{Until: now}, func(e domain.Event) {
		if cur, ok := byID[e.Object]; ok && cur.Phase == domain.PhaseWaiting && !cur.UpdatedAt.After(e.At) {
			add(cur, "follow_up_due")
		}
	})
	if err != nil {
		return nil, err
	}
	for _, it := range td.Stale {
		add(it, "stale")
	}
	return out, nil
}

// eachLive 用"现场"读法按类型枚举 At 落在窗口内的事件（Since 为零表示不设下限）；导入事件不计。
// shortcut: 从最新往回翻页，遇到早于 Since 的事件即停；事件日志极大且回溯很久时再加按时间的索引。
func eachLive(tx *store.Tx, typ string, w window, fn func(domain.Event)) error {
	var before int64
	for {
		page, err := eventlog.QueryBefore(tx, eventlog.Filter{Type: typ, Live: true}, before, eventPage)
		if err != nil {
			return err
		}
		for _, en := range page {
			e := en.Event
			before = e.Seq
			if en.Tombstone || e.At.After(w.Until) {
				continue
			}
			if !w.Since.IsZero() && !e.At.After(w.Since) {
				return nil
			}
			fn(e)
		}
		if len(page) < eventPage {
			return nil
		}
	}
}

// activitySection：窗口内完成的事项与写下的进展条目，按项目分组；按事件的 at 取，导入事件不计（"现场"读法）。
func activitySection(tx *store.Tx, all []domain.Item, w window) ([]projectActivity, error) {
	byID := map[string]domain.Item{}
	for _, it := range all {
		byID[it.ID] = it
	}
	var got []activityEntry
	add := func(e domain.Event, en activityEntry) {
		it, ok := byID[e.Object]
		if !ok {
			return
		}
		en.Item, en.Title, en.Project, en.At = it.ID, it.Title, it.Project, e.At
		got = append(got, en)
	}
	if err := eachLive(tx, domain.EvItemPhaseChanged, w, func(e domain.Event) {
		var p struct {
			Item domain.Item `json:"item"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Item.Phase == domain.PhaseDone {
			add(e, activityEntry{Kind: "done"})
		}
	}); err != nil {
		return nil, err
	}
	if err := eachLive(tx, domain.EvItemProgressAdded, w, func(e domain.Event) {
		var p struct {
			Progress *domain.Progress `json:"progress"`
		}
		if json.Unmarshal(e.Payload, &p) == nil && p.Progress != nil {
			add(e, activityEntry{Kind: "progress", Text: p.Progress.Text})
		}
	}); err != nil {
		return nil, err
	}
	slices.SortStableFunc(got, func(a, b activityEntry) int { return a.At.Compare(b.At) })
	out := []projectActivity{}
	idx := map[string]int{}
	for _, en := range got {
		i, ok := idx[en.Project]
		if !ok {
			i = len(out)
			idx[en.Project] = i
			out = append(out, projectActivity{Project: en.Project, Entries: []activityEntry{}})
		}
		out[i].Entries = append(out[i].Entries, en)
	}
	return out, nil
}

// keepWindow 把取材窗口并回事项的 spec.since/until：Agent 写回 {period, sections} 会整体替换 spec，窗口随之丢失，
// 而下一份简报要取它作起点。窗口以拉起时记下的为准，其余键不动。
func (s *Service) keepWindow(ctx context.Context, it domain.Item, w window) (domain.Item, error) {
	m := map[string]json.RawMessage{}
	if len(it.Spec) > 0 {
		if err := json.Unmarshal(it.Spec, &m); err != nil {
			m = map[string]json.RawMessage{} // 不是对象的 spec 由结果判定按缺字段处理
		}
	}
	m["since"], _ = json.Marshal(w.Since)
	m["until"], _ = json.Marshal(w.Until)
	spec, _ := json.Marshal(m)
	in, _ := json.Marshal(map[string]any{"id": it.ID, "spec": json.RawMessage(spec), "reason": "brief window kept"})
	if _, err := s.o.Exec.Do(ctx, command.Request{Identity: system, Command: "item.update", Input: in}); err != nil {
		return it, err
	}
	it.Spec = spec
	return it, nil
}
