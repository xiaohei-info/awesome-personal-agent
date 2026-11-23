package task

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/budget"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/summaries"
)

// 简报与复盘的资料（设计 §7.3"简报的资料"）：全部是账本读取，不调模型；没有内容时是空数组。
// 模板的 materials 决定组哪些段：没声明的段不查询、不写原文文件，digest 也不汇入（附录 C.7）。
const (
	defaultScheduleDays = 3
	maxScheduleDays     = 14
	proposalsListed     = 20             // 建议段给全的条数
	maxProposalsBrief   = 40             // 其余必须本人看的建议精简列出的上限
	firstBriefWindow    = 24 * time.Hour // 定时读不出上一个节拍时，第一份的窗口
	calendarKind        = "calendar"
	echoReasonPrefix    = "收到回音"
	eventPage           = 500
	followShown         = 3   // 后续事件给最新几条
	followSummaryChars  = 300 // 每条后续事件摘要的字符预算
)

// legacyMaterials 是模板没写 materials 时组的段，与引入 materials 之前一致。
var legacyMaterials = []string{"instance_params", "ledger_snapshot", "spot_check", "digest", "planned", "schedule", "decisions", "due_waiting", "activity", "proposals"}

func wants(declared []string) func(string) bool {
	if len(declared) == 0 {
		declared = legacyMaterials
	}
	return func(name string) bool { return slices.Contains(declared, name) }
}

type window struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// briefPlan 是一份简报取材所需的实例信息。时区以实例定时的 CRON_TZ 为准，没有时取本机时区（与调度器解释 cron 同源）；
// first 是本次到期之前的上一个调度时刻（实例第一份的起点），period 是定时的一个周期（上一个节拍到其后一拍）。
type briefPlan struct {
	want   func(string) bool
	loc    *time.Location
	first  time.Time
	period time.Duration
	days   int             // 日程前瞻天数
	scope  map[string]bool // 实例的项目范围；空表示全部
	base   string          // Web 地址，拼条目的 web_url；空时不给链接
	fields []string        // 模板声明的结果字段：上一份简报是否有效按它判（validResult）
}

func planFor(c rules.Call, want func(string) bool, base string, now time.Time) briefPlan {
	p := briefPlan{want: want, loc: time.Local, first: now.Add(-firstBriefWindow), period: firstBriefWindow, base: strings.TrimRight(base, "/")}
	var params struct {
		Schedule string   `json:"schedule"`
		Projects []string `json:"projects"`
		Days     int      `json:"schedule_days"`
	}
	due := now
	if c.Instance != nil {
		_ = json.Unmarshal(c.Instance.Params, &params) // 参数已按包的 instance_params 校验
		if !c.Instance.Due.IsZero() {
			due = c.Instance.Due // 立即跑一次的到期时刻就是当时
		}
	}
	if sc, err := clock.ParseSchedule(params.Schedule); err == nil {
		p.loc = sc.Location()
		if prev := sc.Prev(due); !prev.IsZero() {
			p.first = prev
			if next := sc.Next(prev); next.After(prev) {
				p.period = next.Sub(prev)
			}
		}
	}
	p.days = scheduleDays(params.Days, p.period)
	if len(params.Projects) > 0 {
		p.scope = map[string]bool{}
		for _, id := range params.Projects {
			p.scope[id] = true
		}
	}
	return p
}

// scheduleDays：实例参数 schedule_days 合法（1 到 14）时用它；否则取一个周期的天数，至少 3 天，至多 14 天。
func scheduleDays(param int, period time.Duration) int {
	if param >= 1 && param <= maxScheduleDays {
		return param
	}
	d := int((period + 24*time.Hour - 1) / (24 * time.Hour))
	return min(max(d, defaultScheduleDays), maxScheduleDays)
}

func (p briefPlan) inScope(it domain.Item) bool { return p.scope == nil || p.scope[it.Project] }

func (p briefPlan) itemURL(id string) string { return p.pageURL("/items/" + id) }

func (p briefPlan) pageURL(path string) string {
	if p.base == "" {
		return ""
	}
	return p.base + path
}

func (p briefPlan) at(t time.Time) time.Time { return t.In(p.loc) }

func (p briefPlan) atp(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	v := t.In(p.loc)
	return &v
}

// zoneName 是给 Agent 的时区：有 IANA 名时用名字，本机时区读不出名字时写成 UTC 偏移。
func zoneName(loc *time.Location, now time.Time) string {
	if n := loc.String(); n != "Local" && n != "" {
		return n
	}
	return "UTC" + now.In(loc).Format("-07:00")
}

// briefWindow 取本份的取材窗口与上一份：二者是同一份简报，即同一实例里窗口最晚的一份有效结果（validResult）。
// 失败、受阻、只报了完成却没写结果的那份都不作起点也不当 prior；没有有效的取 first。简报事项的 origin 记所属实例。
func briefWindow(all []domain.Item, instance, self string, now, first time.Time, fields []string) (window, *domain.Item) {
	w := window{Since: first, Until: now}
	var latest time.Time
	var prior *domain.Item
	for i, it := range all {
		if it.ID == self || it.Kind != domain.KindBrief || it.Origin == nil || it.Origin.Source != domain.OriginInstancePrefix+instance || !validResult(it, fields) {
			continue
		}
		var sp struct {
			Until time.Time `json:"until"`
		}
		_ = json.Unmarshal(it.Spec, &sp)
		if prior == nil || sp.Until.After(latest) || sp.Until.Equal(latest) && it.CreatedAt.After(prior.CreatedAt) {
			prior, latest = &all[i], sp.Until
		}
	}
	if !latest.IsZero() && latest.Before(now) {
		w.Since = latest
	}
	if !w.Since.Before(now) {
		w.Since = now.Add(-firstBriefWindow)
	}
	return w, prior
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
	ID            string     `json:"id"`
	Title         string     `json:"title"`
	Summary       string     `json:"summary,omitempty"`
	Sources       string     `json:"sources,omitempty"`
	Why           []string   `json:"why,omitempty"` // 排在前面的原因：priority_urgent/high、overdue、due_soon、urgent、low_confidence、new_in_window
	CreatedAt     time.Time  `json:"created_at"`
	Deadline      *time.Time `json:"deadline,omitempty"`
	Recommend     string     `json:"recommend,omitempty"`
	Reason        string     `json:"reason,omitempty"`
	LowConfidence bool       `json:"low_confidence"`
	WebURL        string     `json:"web_url,omitempty"`
	FollowedBy    *followUp  `json:"followed_by,omitempty"`
	Events        []string   `json:"-"`             // 引用的来源事件 id，用来出原文文件
	Raw           []rawRef   `json:"raw,omitempty"` // 各来源事件的原文文件名与裁剪量
}

// proposalBrief 是超出完整条数、但必须让本人看到的建议：只给能检索与打开它的字段。
type proposalBrief struct {
	ID        string     `json:"id"`
	Title     string     `json:"title"`
	Sources   string     `json:"sources,omitempty"`
	Why       []string   `json:"why"`
	CreatedAt time.Time  `json:"created_at"`
	Deadline  *time.Time `json:"deadline,omitempty"`
	WebURL    string     `json:"web_url,omitempty"`
}

// followUp 是建议引用的来源事件之后，同一来源里共用关联键（同一对象）的后续事件：数目，加最新几条按该来源外发范围裁剪的内容。
// 后续只说明对象后来有过变化，不代表已经解决；外发范围不允许时只给数目。
type followUp struct {
	Count  int          `json:"count"`
	Latest []laterEvent `json:"latest,omitempty"`
}

type laterEvent struct {
	Type    string `json:"type"`
	At      string `json:"at,omitempty"`
	Subject string `json:"subject,omitempty"`
	Summary string `json:"summary,omitempty"`
	Link    string `json:"link,omitempty"`
}

type progressNote struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

type decisionEntry struct {
	ID       string     `json:"id"`
	Title    string     `json:"title"`
	Project  string     `json:"project,omitempty"`
	Question string     `json:"question,omitempty"`
	Deadline *time.Time `json:"deadline,omitempty"`
	WebURL   string     `json:"web_url,omitempty"`
}

type dueEntry struct {
	ID           string        `json:"id"`
	Title        string        `json:"title"`
	Project      string        `json:"project,omitempty"`
	Phase        domain.Phase  `json:"phase"`
	Reason       string        `json:"reason"` // due_today、due_tomorrow、due_next、overdue、echo、follow_up_due、stale
	Deadline     *time.Time    `json:"deadline,omitempty"`
	LastProgress *progressNote `json:"last_progress,omitempty"`
	WebURL       string        `json:"web_url,omitempty"`
}

type activityEntry struct {
	Item  string    `json:"item"`
	Title string    `json:"title"`
	Kind  string    `json:"kind"` // done、progress
	Text  string    `json:"text,omitempty"`
	On    string    `json:"on,omitempty"` // 进展所指的业务日期（YYYY-MM-DD）
	At    time.Time `json:"at"`           // 写入时刻
	// DateSource 说明这条凭什么进了窗口：on_date 是按业务日期，at 是没有业务日期、按写入时刻兜底（完成一律按 at）。
	DateSource string `json:"date_source"`
	Project    string `json:"project,omitempty"`
	WebURL     string `json:"web_url,omitempty"`
}

type projectActivity struct {
	Project string          `json:"project"`
	Entries []activityEntry `json:"entries"`
}

type plannedEntry struct {
	ID         string       `json:"id"`
	Title      string       `json:"title"`
	Project    string       `json:"project,omitempty"`
	Phase      domain.Phase `json:"phase"`
	PlannedFor string       `json:"planned_for"`
	WebURL     string       `json:"web_url,omitempty"`
	Slot       string       `json:"planned_slot,omitempty"`
}

type openEntry struct {
	Title        string        `json:"title"`
	Project      string        `json:"project,omitempty"`
	Phase        domain.Phase  `json:"phase"`
	Deadline     *time.Time    `json:"deadline,omitempty"`
	PlannedFor   string        `json:"planned_for,omitempty"`
	PlannedSlot  string        `json:"planned_slot,omitempty"`
	UpdatedAt    time.Time     `json:"updated_at"`
	Question     string        `json:"question,omitempty"`
	LastProgress *progressNote `json:"last_progress,omitempty"`
	WebURL       string        `json:"web_url,omitempty"`
}

// sourceCoverage 是一个已登记来源在本期的覆盖：拉取状况与 doctor 的来源检查同一口径（runner.StatsOf），
// 另给窗口内落盘的条数与还在积压里（未判定、过期）没有进初筛结果的条数。三者是不同的事实，不能互相代替。
type sourceCoverage struct {
	Name        string     `json:"name"`
	Type        string     `json:"type,omitempty"` // 语义类型（mail、calendar……）
	Via         string     `json:"via"`            // 接入方式：pull、script（定时拉取）或 webhook（对方推送）
	Fetch       string     `json:"fetch"`          // ok、failing、never、disabled、unknown；推送来源为 push
	Failures    int        `json:"consecutive_failures,omitempty"`
	LastSuccess *time.Time `json:"last_success_at,omitempty"`
	Ingested    int        `json:"ingested_in_window"`
	Backlog     int        `json:"not_screened"`
}

// briefOut 是一份简报的账本资料：窗口、上一份、各段，以及本次选取的 digest 条目（运行成功后才汇入）。
type briefOut struct {
	w      window
	prior  *domain.Item
	mats   []Material
	props  briefProposals
	digest []summaries.DigestEntry
}

// briefLedger 按 plan 组出各段。窗口写进 brief 事项的 spec.since/until 由调用方负责。
func (s *Service) briefLedger(ctx context.Context, c rules.Call, p briefPlan) (out briefOut, err error) {
	now := s.o.Now()
	inst, self := "", ""
	if c.Instance != nil {
		inst = c.Instance.ID
	}
	if c.Item != nil {
		self = c.Item.ID
	}
	want := p.want
	seg := map[string]any{}
	err = s.o.Store.View(ctx, func(tx *store.Tx) error {
		all, err := items.List(tx, items.Filter{})
		if err != nil {
			return err
		}
		out.w, out.prior = briefWindow(all, inst, self, now, p.first, p.fields)
		srcs, err := registry.List(tx)
		if err != nil {
			return err
		}
		names := map[string]string{}
		for _, src := range srcs {
			names[src.ID] = src.Name
		}
		pnames, err := projectNames(tx)
		if err != nil {
			return err
		}
		pname := func(id string) string { return pnames[id] }
		td, err := items.TodayItems(tx, now)
		if err != nil {
			return err
		}
		if want("context") {
			if seg["context"], err = contextSection(tx, srcs, p, out.w, now); err != nil {
				return err
			}
		}
		if want("prior") {
			seg["prior"] = priorSection(out.prior, p)
		}
		if want("schedule") {
			if seg["schedule"], err = scheduleSection(tx, srcs, now, p); err != nil {
				return err
			}
		}
		if want("proposals") {
			if out.props, err = proposalsSection(tx, all, out.w, names, p, now); err != nil {
				return err
			}
		}
		if want("decisions") {
			ds := []decisionEntry{}
			for _, it := range td.Pending {
				if !p.inScope(it) {
					continue
				}
				d := decisionEntry{ID: it.ID, Title: it.Title, Project: pname(it.Project), WebURL: p.itemURL(it.ID)}
				if it.Ask != nil {
					d.Question, d.Deadline = it.Ask.Question, p.atp(it.Ask.Deadline)
				}
				ds = append(ds, d)
			}
			seg["decisions"] = ds
		}
		if want("due_waiting") {
			if seg["due_waiting"], err = dueSection(tx, all, td, out.w, now, p, pname); err != nil {
				return err
			}
		}
		if want("planned") {
			pl := []plannedEntry{}
			for _, it := range td.Planned {
				if p.inScope(it) {
					pl = append(pl, plannedEntry{it.ID, it.Title, pname(it.Project), it.Phase, it.PlannedFor, p.itemURL(it.ID), it.PlannedSlot})
				}
			}
			seg["planned"] = pl
		}
		if want("activity") {
			if seg["activity"], err = activitySection(tx, all, out.w, p, pname); err != nil {
				return err
			}
		}
		if want("open_items") {
			if seg["open_items"], err = openItemsSection(tx, all, now, p, pname); err != nil {
				return err
			}
		}
		if want("digest") {
			if out.digest, err = summaries.PendingDigest(tx); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return out, err
	}
	for _, name := range []string{"context", "prior", "schedule", "decisions", "due_waiting", "planned", "activity", "open_items"} {
		if v, ok := seg[name]; ok {
			b, _ := json.Marshal(map[string]any{name: v})
			out.mats = append(out.mats, Material{Name: name, Body: string(b)})
		}
	}
	return out, nil
}

func projectNames(tx *store.Tx) (map[string]string, error) {
	ps, err := projects.List(tx)
	out := map[string]string{}
	for _, p := range ps {
		out[p.ID] = p.Name
	}
	return out, err
}

// contextSection：时刻与时区、窗口、范围内的项目名、各已登记来源的覆盖，以及抽检与 Web 的入口。
func contextSection(tx *store.Tx, srcs []domain.Source, p briefPlan, w window, now time.Time) (map[string]any, error) {
	ps, err := projects.List(tx)
	if err != nil {
		return nil, err
	}
	scoped := []string{}
	for _, pr := range ps {
		if p.scope != nil && p.scope[pr.ID] || p.scope == nil && pr.Status != domain.ProjectArchived {
			scoped = append(scoped, pr.Name)
		}
	}
	cov := []sourceCoverage{}
	for _, src := range srcs {
		c := sourceCoverage{Name: src.Name, Type: src.SourceKind, Via: string(src.Type), Fetch: "ok"}
		if c.Via == "" { // 历史登记行没写类型，登记模块按脚本来源对待（registry.go 的 exec 判定）
			c.Via = string(domain.SourceScript)
		}
		st, err := runner.StatsOf(src)
		switch {
		case !src.Enabled:
			c.Fetch = "disabled"
		case src.Type == domain.SourceWebhook: // 推送来源没有拉取记录：只看本期收到多少，不当成"从未拉取"
			c.Fetch = "push"
		case err != nil:
			c.Fetch = "unknown"
		case st.Runs == 0 && st.LastSuccessAt.IsZero() && st.LastFailureAt.IsZero():
			c.Fetch = "never"
		case st.ConsecutiveFailures > 0:
			c.Fetch, c.Failures = "failing", st.ConsecutiveFailures
		}
		if err == nil && !st.LastSuccessAt.IsZero() {
			c.LastSuccess = p.atp(&st.LastSuccessAt)
		}
		if c.Ingested, c.Backlog, err = runner.Counts(tx, src.ID, w.Since, w.Until); err != nil {
			return nil, err
		}
		cov = append(cov, c)
	}
	ctx := map[string]any{"now": p.at(now), "timezone": zoneName(p.loc, now), "window": window{p.at(w.Since), p.at(w.Until)},
		"period_start": p.at(p.first), "projects": scoped, "sources": cov}
	_, off := now.In(p.loc).Zone()
	if _, host := now.In(time.Local).Zone(); off != host {
		ctx["host_timezone"] = zoneName(time.Local, now) // "今天""明天"与计划日期按主机日界（与今天视图一致）
	}
	if p.base != "" {
		ctx["web_base"], ctx["spot_check_url"] = p.base, p.base+"/spot-check"
	}
	return ctx, nil
}

// priorSection：同一实例上一份有效的简报（与窗口起点是同一份）（正文原样），用来避免重复播报没有变化的普通信息；没有时为 null。
func priorSection(it *domain.Item, p briefPlan) any {
	if it == nil {
		return nil
	}
	var sp struct {
		Period   string          `json:"period"`
		Sections json.RawMessage `json:"sections"`
	}
	_ = json.Unmarshal(it.Spec, &sp)
	return map[string]any{"period": sp.Period, "created_at": p.at(it.CreatedAt), "sections": sp.Sections}
}

// scheduleSection：语义类型为 calendar 的来源里，开始时间落在 [今天 0 点, +days 天) 的事件（日界按实例时区）；
// 同一关联键只取最新一版，最新一版是取消的不列。结束时间与地点取原文 headers 部分的 end、location（来源没带就不写）。
func scheduleSection(tx *store.Tx, srcs []domain.Source, now time.Time, p briefPlan) ([]scheduleEntry, error) {
	y, m, d := now.In(p.loc).Date()
	from := time.Date(y, m, d, 0, 0, 0, 0, p.loc)
	to := from.AddDate(0, 0, p.days)
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
			en := scheduleEntry{Title: e.Subject, Start: p.at(e.OccurredAt), Source: src.Name, Link: e.Link}
			if part, err := runner.ReadPart(tx, e.ID, runner.PartHeaders); err == nil {
				var h map[string]any
				if json.Unmarshal(part.Content, &h) == nil {
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

// proposalsSection：范围内全部未处置的建议排好序，前 proposalsListed 条给全（摘要、推荐、后续、原文），其余必须本人看的
// （高优先级、已过期或今明到期、起草判今天不处理有后果、把握低）给精简条目，至多 maxProposalsBrief 条，再多的只计数；
// 其余只计数，另给窗口内新增的条数与按来源的分布。顺序：优先级高或到期 → 今天不处理有后果 → 本期新增（把握低的在前）→
// 更早的把握低 → 其余，同档新建的在前：简报讲的是本期新增，更早的必须本人看的仍一条不漏地列出（§7.3"简报的资料"）。
func proposalsSection(tx *store.Tx, all []domain.Item, w window, names map[string]string, p briefPlan, now time.Time) (briefProposals, error) {
	tomorrow := items.Today(now.AddDate(0, 0, 1))
	why := func(it domain.Item) []string {
		var ws []string
		if it.Priority == domain.PriorityUrgent || it.Priority == domain.PriorityHigh {
			ws = append(ws, "priority_"+string(it.Priority))
		}
		if it.Deadline != nil {
			switch day := items.Today(*it.Deadline); {
			case day < items.Today(now):
				ws = append(ws, "overdue")
			case day <= tomorrow:
				ws = append(ws, "due_soon")
			}
		}
		if pr := it.Proposal; pr != nil {
			if pr.Urgent != nil && *pr.Urgent {
				ws = append(ws, "urgent")
			}
			if pr.LowConfidence {
				ws = append(ws, "low_confidence")
			}
		}
		if it.CreatedAt.After(w.Since) && !it.CreatedAt.After(w.Until) {
			ws = append(ws, "new_in_window")
		}
		return ws
	}
	// 档：0 高优先级或到期，1 今天不处理有后果，2 新增且把握低，3 新增，4 把握低，5 其余；0、1、2、4 是必须本人看的。
	tier := func(ws []string) int {
		has := func(w string) bool { return slices.Contains(ws, w) }
		switch {
		case slices.ContainsFunc(ws, func(s string) bool { return strings.HasPrefix(s, "priority_") || s == "overdue" || s == "due_soon" }):
			return 0
		case has("urgent"):
			return 1
		case has("new_in_window") && has("low_confidence"):
			return 2
		case has("new_in_window"):
			return 3
		case has("low_confidence"):
			return 4
		}
		return 5
	}
	must := func(tier int) bool { return tier != 3 && tier != 5 }
	type cand struct {
		it   domain.Item
		why  []string
		tier int
	}
	var cs []cand
	out := briefProposals{BySource: map[string]int{}, MoreURL: p.pageURL("/proposals")}
	for _, it := range all {
		if it.Phase != domain.PhaseProposed || !p.inScope(it) {
			continue
		}
		ws := why(it)
		cs = append(cs, cand{it, ws, tier(ws)})
		if slices.Contains(ws, "new_in_window") {
			out.New++
		}
	}
	out.Pending = len(cs)
	slices.SortStableFunc(cs, func(a, b cand) int {
		if a.tier != b.tier {
			return a.tier - b.tier
		}
		return b.it.CreatedAt.Compare(a.it.CreatedAt)
	})
	for i, c := range cs {
		it := c.it
		var ns []string
		var evs []string
		var later []domain.SourceEvent
		if pr := it.Proposal; pr != nil {
			for _, id := range pr.Sources {
				ev, err := runner.Event(tx, id)
				if err != nil {
					if isNotFoundErr(err) {
						continue
					}
					return briefProposals{}, err
				}
				evs = append(evs, id)
				if n := names[ev.SourceID]; n != "" && !slices.Contains(ns, n) {
					ns = append(ns, n)
				}
				if i >= proposalsListed {
					continue // 后续只给完整条目
				}
				ls, err := runner.Later(tx, id)
				if err != nil {
					return briefProposals{}, err
				}
				for _, l := range ls {
					if !slices.Contains(pr.Sources, l.ID) && !slices.ContainsFunc(later, func(x domain.SourceEvent) bool { return x.ID == l.ID }) {
						later = append(later, l)
					}
				}
			}
		}
		src := strings.Join(ns, "、")
		if slices.Contains(c.why, "new_in_window") {
			k := src
			if k == "" {
				k = "未记来源"
			}
			out.BySource[k]++
		}
		switch {
		case i < proposalsListed:
			e := proposalEntry{ID: it.ID, Title: it.Title, Summary: it.Summary, Sources: src, Why: c.why, CreatedAt: p.at(it.CreatedAt),
				Deadline: p.atp(it.Deadline), WebURL: p.itemURL(it.ID), Events: evs}
			if pr := it.Proposal; pr != nil {
				e.Recommend, e.Reason, e.LowConfidence = pr.Recommend, pr.Reason, pr.LowConfidence
				e.FollowedBy = followOf(tx, later, p)
			}
			out.Entries = append(out.Entries, e)
		case must(c.tier) && len(out.Also) < maxProposalsBrief:
			out.Also = append(out.Also, proposalBrief{ID: it.ID, Title: it.Title, Sources: src, Why: c.why, CreatedAt: p.at(it.CreatedAt),
				Deadline: p.atp(it.Deadline), WebURL: p.itemURL(it.ID)})
		case must(c.tier):
			out.MustNotListed++
		}
	}
	return out, nil
}

// followOf 把后续事件按发生时刻排好，给数目与最新几条；每条按该来源对外部端点的外发范围裁剪（§14.14），摘要另按字符预算裁。
func followOf(tx *store.Tx, later []domain.SourceEvent, p briefPlan) *followUp {
	if len(later) == 0 {
		return nil
	}
	slices.SortStableFunc(later, func(a, b domain.SourceEvent) int { return b.OccurredAt.Compare(a.OccurredAt) })
	f := &followUp{Count: len(later)}
	for _, ev := range later[:min(followShown, len(later))] {
		tr, ok := registry.Trim(ev, rawScope(tx, ev))
		if !ok {
			continue // 外发范围不允许：只计数
		}
		l := laterEvent{Type: tr.Type, Subject: tr.Subject, Link: tr.Link, Summary: budget.Fit(budget.Source{Body: tr.Summary}, followSummaryChars).Text}
		if !ev.OccurredAt.IsZero() {
			l.At = p.at(ev.OccurredAt).Format(time.RFC3339)
		}
		f.Latest = append(f.Latest, l)
	}
	return f
}

func lastProgress(tx *store.Tx, id string, p briefPlan) (*progressNote, error) {
	ps, err := items.Progress(tx, id)
	if err != nil || len(ps) == 0 {
		return nil, err
	}
	last := ps[len(ps)-1]
	return &progressNote{Text: last.Text, At: p.at(last.At)}, nil
}

// dueSection 与"今天"视图同一口径：今天与明天到期的、已过期未完成的；窗口内收到回音回到 open 的；
// 跟进到期仍在等的（等待事项的 follow-up 轮次提醒已记 followup.reminded 且之后没有进展）；久未推进的。
// 周期超过两天的实例（复盘）另给下一个周期内到期的（due_next）。每条带当前阶段与最近一条进展。
func dueSection(tx *store.Tx, all []domain.Item, td items.TodayList, w window, now time.Time, p briefPlan, pname func(string) string) ([]dueEntry, error) {
	out := []dueEntry{}
	today, tomorrow := items.Today(now), items.Today(now.AddDate(0, 0, 1))
	horizon := now.Add(p.period)
	seen := map[string]bool{}
	var adds []domain.Item
	var reasons []string
	add := func(it domain.Item, reason string) {
		if p.inScope(it) && !seen[it.ID+reason] {
			seen[it.ID+reason] = true
			adds, reasons = append(adds, it), append(reasons, reason)
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
		case p.period > 48*time.Hour && !it.Deadline.After(horizon):
			add(it, "due_next")
		}
	}
	// 回音：窗口内由 followmsg 把 waiting 转回 open 的规则性转移（§9.4）。
	err := eachLive(tx, domain.EvItemPhaseChanged, w, func(e domain.Event) {
		var pl struct {
			Before *domain.Item `json:"before"`
			Item   domain.Item  `json:"item"`
		}
		if json.Unmarshal(e.Payload, &pl) != nil || pl.Before == nil || pl.Before.Phase != domain.PhaseWaiting ||
			pl.Item.Phase != domain.PhaseOpen || !strings.HasPrefix(e.Reason, echoReasonPrefix) {
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
	for i, it := range adds {
		lp, err := lastProgress(tx, it.ID, p)
		if err != nil {
			return nil, err
		}
		out = append(out, dueEntry{ID: it.ID, Title: it.Title, Project: pname(it.Project), Phase: it.Phase, Reason: reasons[i],
			Deadline: p.atp(it.Deadline), LastProgress: lp, WebURL: p.itemURL(it.ID)})
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

// activitySection：窗口内完成的事项与写下的进展条目，按项目分组；导入事件不计（"现场"读法），简报与复盘自己（brief 事项）不算活动。
// 进展按业务日期 on 取：on 落在 [since 那天, until 那天)（主机日界，与 on 的"今天"同源），相邻窗口不重不漏；
// 没有 on 的进展与完成按事件的 at 取（since, until]，条目标 date_source。写入时刻不代表发生时刻（迁移把历史进展都写在导入那晚）。
func activitySection(tx *store.Tx, all []domain.Item, w window, p briefPlan, pname func(string) string) ([]projectActivity, error) {
	byID := map[string]domain.Item{}
	for _, it := range all {
		byID[it.ID] = it
	}
	var got []activityEntry
	add := func(e domain.Event, en activityEntry) {
		it, ok := byID[e.Object]
		if !ok || it.Kind == domain.KindBrief || !p.inScope(it) {
			return
		}
		en.Item, en.Title, en.Project, en.At, en.WebURL = it.ID, it.Title, pname(it.Project), p.at(e.At), p.itemURL(it.ID)
		got = append(got, en)
	}
	inAt := func(e domain.Event) bool { return e.At.After(w.Since) && !e.At.After(w.Until) }
	fromDay, toDay := items.Today(w.Since), items.Today(w.Until)
	if err := eachLive(tx, domain.EvItemPhaseChanged, w, func(e domain.Event) {
		var pl struct {
			Item domain.Item `json:"item"`
		}
		if json.Unmarshal(e.Payload, &pl) == nil && pl.Item.Phase == domain.PhaseDone {
			add(e, activityEntry{Kind: "done", DateSource: "at"})
		}
	}); err != nil {
		return nil, err
	}
	// on 不晚于写入那天，所以 on 落在窗口里的进展都写于 since 那天零点之后：从那里扫起即可。
	dayStart, err := time.ParseInLocation("2006-01-02", fromDay, time.Local)
	if err != nil {
		return nil, err
	}
	if err := eachLive(tx, domain.EvItemProgressAdded, window{Since: dayStart.Add(-time.Nanosecond), Until: w.Until}, func(e domain.Event) {
		var pl struct {
			Progress *domain.Progress `json:"progress"`
		}
		if json.Unmarshal(e.Payload, &pl) != nil || pl.Progress == nil {
			return
		}
		if on := pl.Progress.On; on != "" {
			if on >= fromDay && on < toDay {
				add(e, activityEntry{Kind: "progress", Text: pl.Progress.Text, On: on, DateSource: "on_date"})
			}
		} else if inAt(e) {
			add(e, activityEntry{Kind: "progress", Text: pl.Progress.Text, DateSource: "at"})
		}
	}); err != nil {
		return nil, err
	}
	key := func(a activityEntry) string { // 按业务日期排，没有的按写入时刻所在的日子
		if a.On != "" {
			return a.On
		}
		return items.Today(a.At)
	}
	slices.SortStableFunc(got, func(a, b activityEntry) int {
		if c := strings.Compare(key(a), key(b)); c != 0 {
			return c
		}
		return a.At.Compare(b.At)
	})
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

// openItemsSection：范围内全部在途事项（open、waiting、needs_input；不含建议与简报，初筛未返回的待决不列），不截断，带 total。
// 排序让该先看的在前：等本人决定、已逾期或今明到期、在等别人、久未推进、其余；同档按更新时间从旧到新，卡得最久的不被挤掉。
// shortcut: 与账本快照同样不分页；在途事项多到撑爆资料文件时再按档位裁剪并在条目里写明裁了多少。
func openItemsSection(tx *store.Tx, all []domain.Item, now time.Time, p briefPlan, pname func(string) string) (map[string]any, error) {
	tomorrow := items.Today(now.AddDate(0, 0, 1))
	rank := func(it domain.Item) int {
		switch {
		case it.Phase == domain.PhaseNeedsInput:
			return 0
		case it.Deadline != nil && items.Today(*it.Deadline) <= tomorrow:
			return 1
		case it.Phase == domain.PhaseWaiting:
			return 2
		case now.Sub(it.UpdatedAt) > items.StaleAfter:
			return 3
		}
		return 4
	}
	var sel []domain.Item
	for _, it := range all {
		if it.Kind == domain.KindBrief || !p.inScope(it) || !(it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting || it.Phase == domain.PhaseNeedsInput) {
			continue
		}
		sel = append(sel, it)
	}
	slices.SortStableFunc(sel, func(a, b domain.Item) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return a.UpdatedAt.Compare(b.UpdatedAt)
	})
	es := []openEntry{}
	for _, it := range sel {
		lp, err := lastProgress(tx, it.ID, p)
		if err != nil {
			return nil, err
		}
		e := openEntry{Title: it.Title, Project: pname(it.Project), Phase: it.Phase, Deadline: p.atp(it.Deadline), PlannedFor: it.PlannedFor, PlannedSlot: it.PlannedSlot,
			UpdatedAt: p.at(it.UpdatedAt), LastProgress: lp, WebURL: p.itemURL(it.ID)}
		if it.Ask != nil {
			e.Question = it.Ask.Question
		}
		es = append(es, e)
	}
	return map[string]any{"total": len(es), "items": es}, nil
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
