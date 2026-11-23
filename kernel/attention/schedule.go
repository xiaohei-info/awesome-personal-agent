package attention

// 到点提醒（§7.4「主动提醒」）：周期 Tick 里按事项的计划日期、时段、每日钟点与截止批量算出已到的提醒点，
// 一次 Tick 合成一条投递，送到主 Agent 的主会话（唤醒路由，由网关插件注入），主会话不能唤醒时落 Web 未读，不走无头运行。
// 不另设定时器：用触达自己的周期 Tick；暂存、作废与"已处理"沿用 attention_held、attention_done 两张表，没有新表。
//
// 去重与节流：
//   - 幂等键 sched:<类型>:<事项>:<提醒点 UnixNano>：同一提醒点全局只推一次（与其他提醒同一个 known 判定）；
//   - 同一事项同一类型的提醒点与已推或已排队的相距不足 dedup_min，不再推（被跳过的点不算推过）；
//   - 事项一变（改期、改钟点、完成、放弃）就清掉它被拦住的提醒点，含它的、还没被领取的合并投递作废，
//     成员放回去由下一次 Tick 按事项现状重算（作废的事项按新时间重新算点，旧点不再出现）；
//   - 静默时段内拦到静默结束，结束后合并成一条；每日条数或消息数到上限就攒到下一个工作日的工作开始时间合并推一条；
//   - 注入失败由投递队列按退避重试，超限或无人领取退回 Web 未读，不静默丢弃。

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ScheduleItem 是到点提醒合并投递的占位事项：它不属于任何一个事项，成员变化由 voidScheduled 处理。
const ScheduleItem = "attention:schedule"

// 提醒类型（幂等键的第二段）。
const (
	SchedStart = "start" // 开始：只有日期、时段开始、每日钟点
	SchedEnd   = "end"   // 截止：时段结束、只到日的截止、截止前提前量
)

const (
	schedPrefix      = "sched:"
	schedBatchPrefix = "schedbatch:"
	schedInPrefix    = "schedin:" // attention_done 里记合并投递的成员：schedin:<投递 id>|<成员幂等键>
	// schedLookback：提醒点最多往回找这么久（重启、休眠后补推）；更早的不推。也不早于开启时刻（Since）。
	schedLookback = 24 * time.Hour
)

// 提醒原因（决定提醒文案）。
const (
	whyPlannedDay = "planned_day" // 只有日期：当天工作开始时间
	whySlotStart  = "slot_start"  // 日期 + 时段：时段开始
	whySlotEnd    = "slot_end"    // 时段结束
	whyDaily      = "daily"       // 只有钟点：每天
	whyDueDay     = "due_day"     // 只到日的截止：当天工作结束时间
	whyDueSoon    = "due_soon"    // 带钟点的截止：提前量
)

// SchedPoint 是一个提醒点。
type SchedPoint struct {
	Item  domain.Item
	Kind  string // SchedStart / SchedEnd
	Why   string
	At    time.Time
	Shift bool // 落在非工作日，顺延到了下一个工作日
}

// SchedKey 是提醒点的幂等键：事项 + 提醒类型 + 提醒点时间。
func SchedKey(kind, item string, at time.Time) string {
	return fmt.Sprintf("%s%s:%s:%d", schedPrefix, kind, item, at.UnixNano())
}

// parseKey 拆幂等键；不是到点提醒的键返回 false。
func parseKey(key string) (kind, item string, at time.Time, ok bool) {
	rest, ok := strings.CutPrefix(key, schedPrefix)
	if !ok {
		return "", "", time.Time{}, false
	}
	kind, rest, _ = strings.Cut(rest, ":")
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return "", "", time.Time{}, false
	}
	n, err := strconv.ParseInt(rest[i+1:], 10, 64)
	if err != nil {
		return "", "", time.Time{}, false
	}
	return kind, rest[:i], time.Unix(0, n), true
}

// at 把本机日期与当天分钟数换成时刻。
func at(day time.Time, min int) time.Time {
	return time.Date(day.Year(), day.Month(), day.Day(), min/60, min%60, 0, 0, time.Local)
}

func clockMin(s string) int { m, _ := policy.ParseClock(s); return m } // 策略写入时已校验

// nextWorkday 返回 day 当天或之后第一个工作日（至多找 7 天，策略保证至少有一个工作日）。
func nextWorkday(s policy.Proactive, day time.Time) time.Time {
	for i := 0; i < 7 && !s.Workday(day); i++ {
		day = day.AddDate(0, 0, 1)
	}
	return day
}

// PointsOf 返回事项落在 (from, to] 内的提醒点，按开关过滤：
//   - 只有日期（planned_for，无时段）：当天工作开始时间；非工作日且开了顺延，取下一个工作日的工作开始时间；
//   - 日期 + 时段：开始提醒在时段开始，截止提醒在时段结束减提前量；
//   - 只有钟点（daily_at，无日期）：每天这个钟点；
//   - 只到日的截止（当天 23:59:59）：当天工作结束时间；带钟点的截止：提前量大于 0 时在截止前这么多分钟（截止已过另由 remindOverdue 管）。
func PointsOf(it domain.Item, s policy.Proactive, from, to time.Time) []SchedPoint {
	var out []SchedPoint
	add := func(kind, why string, t time.Time, shift bool) {
		if t.After(from) && !t.After(to) && (kind == SchedStart && s.Start || kind == SchedEnd && s.End) {
			out = append(out, SchedPoint{Item: it, Kind: kind, Why: why, At: t, Shift: shift})
		}
	}
	if it.PlannedFor != "" {
		if day, err := time.ParseInLocation("2006-01-02", it.PlannedFor, time.Local); err == nil {
			if a, b, ok := domain.ParseSlot(it.PlannedSlot); ok {
				add(SchedStart, whySlotStart, at(day, a), false)
				add(SchedEnd, whySlotEnd, at(day, b).Add(-time.Duration(s.LeadMin)*time.Minute), false)
			} else {
				shifted := day
				if s.Shift {
					shifted = nextWorkday(s, day)
				}
				add(SchedStart, whyPlannedDay, at(shifted, clockMin(s.WorkStart)), !shifted.Equal(day))
			}
		}
	} else if m, ok := domain.ParseClock(it.DailyAt); ok && it.DailyAt != "" {
		f := from.In(time.Local)
		for day := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, time.Local); !day.After(to); day = day.AddDate(0, 0, 1) {
			add(SchedStart, whyDaily, at(day, m), false)
		}
	}
	if d := it.Deadline; d != nil {
		l := d.In(time.Local)
		if l.Hour() == 23 && l.Minute() == 59 && l.Second() == 59 { // 只到日的截止（§6.3 决定 82）
			add(SchedEnd, whyDueDay, at(l, clockMin(s.WorkEnd)), false)
		} else if s.LeadMin > 0 {
			add(SchedEnd, whyDueSoon, d.Add(-time.Duration(s.LeadMin)*time.Minute), false)
		}
	}
	return out
}

// schedPhases 是会提醒的阶段：与截止已过再提醒相同（建议不计承诺，不提醒）。
var schedPhases = []domain.Phase{domain.PhaseOpen, domain.PhaseWaiting, domain.PhaseNeedsInput}

// duePoints 返回全部会提醒的事项在 (from, to] 内的提醒点，按时间、事项排序。
func duePoints(tx *store.Tx, s policy.Proactive, from, to time.Time) ([]SchedPoint, error) {
	var out []SchedPoint
	for _, ph := range schedPhases {
		list, err := items.List(tx, items.Filter{Phase: ph})
		if err != nil {
			return nil, err
		}
		for _, it := range list {
			if it.Kind == domain.KindBrief {
				continue
			}
			out = append(out, PointsOf(it, s, from, to)...)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].At.Equal(out[j].At) {
			return out[i].At.Before(out[j].At)
		}
		return out[i].Item.ID < out[j].Item.ID
	})
	return out, nil
}

// stamp 是一行前面的提醒点时间：放行时与当天比，不是当天的带上日期（攒到次日或补推的点）。
func stamp(t, now time.Time) string {
	t = t.In(time.Local)
	if n := now.In(time.Local); t.Year() != n.Year() || t.YearDay() != n.YearDay() {
		return "· [" + t.Format("01-02 15:04") + "] "
	}
	return "· [" + t.Format("15:04") + "] "
}

// line 是提醒点在合并消息里的一行（不含时间，放行时由 stamp 补上）：类型、标题与事项 id（主会话据 id 接着处理）。
func line(p SchedPoint) string {
	var why string
	switch p.Why {
	case whyPlannedDay:
		why = "开始｜计划 " + p.Item.PlannedFor + " 做"
		if p.Shift {
			why += "（非工作日，顺延到今天）"
		}
	case whySlotStart:
		why = "开始｜" + p.Item.PlannedSlot
	case whySlotEnd:
		why = "截止｜时段 " + p.Item.PlannedSlot + " 结束"
	case whyDaily:
		why = "开始｜每天 " + p.Item.DailyAt
	case whyDueDay:
		why = "截止｜今天截止"
	case whyDueSoon:
		why = "截止｜" + p.Item.Deadline.In(time.Local).Format("01-02 15:04") + " 截止"
	}
	return fmt.Sprintf("%s %s（%s）", why, p.Item.Title, p.Item.ID)
}

// MainRoute 是送到主会话的路由：主 Agent 能唤醒时为它的唤醒路由，否则 Web 未读，并给出原因（executors.Main*）。
// 从不选无头运行：无头运行在本人看不到的会话里。
func MainRoute(tx *store.Tx, pol policy.Policy) (route, reason string, err error) {
	agent, reason, err := executors.MainWake(tx, pol)
	if err != nil || reason != "" {
		return delivery.RouteWeb, reason, err
	}
	return delivery.WakeRoute(agent), "", nil
}

// remindScheduled 是 Tick 里的到点提醒一步：找出新到的提醒点并暂存，再放行到期的暂存成一条合并投递。
func (a *Attention) remindScheduled(tx *store.Tx, pol policy.Policy) error {
	s := pol.Disturb.ProactiveSettings()
	if !s.On { // 关掉后还没放行的提醒点一并丢掉：关就是不要再来
		_, err := tx.Exec(`DELETE FROM attention_held WHERE id LIKE 'sched:%'`)
		return err
	}
	now := a.o.Now()
	from := now.Add(-schedLookback)
	if s.Since != nil && s.Since.After(from) {
		from = *s.Since
	}
	pts, err := duePoints(tx, s, from, now)
	if err != nil {
		return err
	}
	for _, p := range pts {
		key := SchedKey(p.Kind, p.Item.ID, p.At)
		if known, err := a.known(tx, key); err != nil || known {
			if err != nil {
				return err
			}
			continue
		}
		dup, err := a.schedDup(tx, p, time.Duration(s.DedupMin)*time.Minute)
		if err != nil {
			return err
		}
		if dup { // 不记下来：跳过的点不算"推过"，否则会挡住之后真正该推的点
			continue
		}
		ver, err := command.Version(tx, p.Item.ID)
		if err != nil {
			return err
		}
		if err := a.offer(tx, pol, held{id: key, item: p.Item.ID, class: domain.DeliveryReminder, priority: p.Item.Priority, version: ver,
			content: domain.DeliveryContent{Title: p.Item.Title, Summary: line(p), WebURL: a.o.WebURL(p.Item.ID)}}); err != nil {
			return err
		}
	}
	return nil // 放行在 flush：静默与暂停与其他提醒同一条流水线，成批与每日上限是这一行声明的合并方式
}

// schedDup：同一事项同一类型已有推过或排队中的提醒点，与 p 相距不足 window。
func (a *Attention) schedDup(tx *store.Tx, p SchedPoint, window time.Duration) (bool, error) {
	if window <= 0 {
		return false, nil
	}
	prefix := schedPrefix + p.Kind + ":" + p.Item.ID + ":"
	dup := false
	err := scan(tx, func(r *store.Rows) error {
		var id string
		if err := r.Scan(&id); err != nil {
			return err
		}
		if _, item, t, ok := parseKey(id); ok && item == p.Item.ID && strings.HasPrefix(id, prefix) {
			if d := p.At.Sub(t); d < window && d > -window {
				dup = true
			}
		}
		return nil
	}, `SELECT id FROM attention_held WHERE id LIKE ? UNION ALL SELECT id FROM attention_done WHERE id LIKE ?`, prefix+"%", prefix+"%")
	return dup, err
}

// releaseScheduled 是到点提醒这一行的成批放行（flush 已按共享流水线判过静默）：到了每日件数或条数上限的
// 攒到下一个工作日的工作开始时间；其余合成一条投递，按矩阵 scheduled 行的去处入队（日上限与暂停由投递队列判）。
// 被静音的事项各自只留 Web 未读，不进合并消息。
func (a *Attention) releaseScheduled(tx *store.Tx, pol policy.Policy, due []held, now time.Time) error {
	if len(due) == 0 {
		return nil
	}
	s := pol.Disturb.ProactiveSettings()
	at := func(h held) time.Time { _, _, t, _ := parseKey(h.id); return t }
	sort.SliceStable(due, func(i, j int) bool { return at(due[i]).Before(at(due[j])) })
	hold := func(hs []held, until time.Time) error {
		for _, h := range hs {
			if _, err := tx.Exec(`UPDATE attention_held SET release_at = ? WHERE id = ?`, until.UnixNano(), h.id); err != nil {
				return err
			}
		}
		return nil
	}
	batches, members, err := a.schedToday(tx, now)
	if err != nil {
		return err
	}
	room := len(due)
	if s.MaxItems > 0 {
		room = min(room, s.MaxItems-int(members))
	}
	if s.MaxBatches > 0 && batches >= int64(s.MaxBatches) {
		room = 0
	}
	if room <= 0 {
		return hold(due, a.nextWorkStart(s, now))
	}
	if err := hold(due[room:], a.nextWorkStart(s, now)); err != nil {
		return err
	}
	sched := row{keys: []string{policy.RowScheduled}, floor: true}
	var batch []held
	for _, h := range due[:room] {
		if _, err := tx.Exec(`DELETE FROM attention_held WHERE id = ?`, h.id); err != nil {
			return err
		}
		t, err := a.target(tx, held{item: h.item})
		if err != nil {
			return err
		}
		if a.o.Queue.Muted(pol, h.item, t) {
			if _, err := a.emit(tx, pol, sched, h.priority, domain.Delivery{ID: h.id, Item: h.item, Class: domain.DeliveryReminder,
				Version: h.version, Content: h.content}, t); err != nil {
				return err
			}
			continue
		}
		batch = append(batch, h)
	}
	if len(batch) == 0 {
		return nil
	}
	keys := make([]string, len(batch))
	lines := make([]string, len(batch))
	for i, h := range batch {
		keys[i], lines[i] = h.id, stamp(at(h), now)+h.content.Summary
	}
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\n")))
	id := schedBatchPrefix + hex.EncodeToString(sum[:8])
	if _, err := a.emit(tx, pol, sched, "", domain.Delivery{ID: id, Item: ScheduleItem, Class: domain.DeliveryReminder, Version: 1,
		Content: domain.DeliveryContent{Title: fmt.Sprintf("到点提醒：%d 件事", len(batch)), Summary: strings.Join(lines, "\n"), WebURL: "/today"}}, delivery.Target{}); err != nil {
		return err
	}
	for _, k := range keys {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO attention_done (id) VALUES (?), (?)`, k, schedInPrefix+id+"|"+k); err != nil {
			return err
		}
	}
	return nil
}

// nextWorkStart 是明天起第一个工作日的工作开始时间（每日上限到了，攒到那时合并推）。
func (a *Attention) nextWorkStart(s policy.Proactive, now time.Time) time.Time {
	n := now.In(time.Local)
	day := nextWorkday(s, time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1))
	return at(day, clockMin(s.WorkStart))
}

// schedToday 是本机今天已产出的到点提醒合并消息条数与其中的事项条数（作废的不算）。
func (a *Attention) schedToday(tx *store.Tx, now time.Time) (batches, members int64, err error) {
	n := now.In(time.Local)
	from := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.Local)
	var ids []string
	if err = scan(tx, func(r *store.Rows) error {
		var id string
		if err := r.Scan(&id); err != nil {
			return err
		}
		ids = append(ids, id)
		return nil
	}, `SELECT id FROM delivery_delivery WHERE id LIKE 'schedbatch:%' AND id NOT LIKE '%`+AgentSuffix+`' AND state != 'superseded' AND created_at >= ? AND created_at < ?`,
		from.UnixNano(), from.AddDate(0, 0, 1).UnixNano()); err != nil {
		return 0, 0, err
	}
	for _, id := range ids {
		var k int64
		if err = scan(tx, func(r *store.Rows) error { return r.Scan(&k) },
			`SELECT COUNT(*) FROM attention_done WHERE id LIKE ?`, schedInPrefix+id+"|%"); err != nil {
			return 0, 0, err
		}
		members += k
	}
	return int64(len(ids)), members, nil
}

// voidScheduled：事项变了，含它的、还没被领取的合并投递作废，成员的提醒点放回去，由下一次 Tick 按各事项现状重算。
// 已被领取（正在注入）或已送达的不动：那条消息已经或正在出去，再发一次就是重复。
func (a *Attention) voidScheduled(tx *store.Tx, item string) error {
	var batches []string
	err := scan(tx, func(r *store.Rows) error {
		var id string
		if err := r.Scan(&id); err != nil {
			return err
		}
		rest := strings.TrimPrefix(id, schedInPrefix)
		batch, key, _ := strings.Cut(rest, "|")
		if _, it, _, ok := parseKey(key); ok && it == item {
			batches = append(batches, batch)
		}
		return nil
	}, `SELECT id FROM attention_done WHERE id LIKE ?`, schedInPrefix+"%|"+schedPrefix+"%:"+item+":%")
	if err != nil {
		return err
	}
	for _, b := range batches {
		n, err := a.o.Queue.SupersedeID(tx, b)
		if err != nil {
			return err
		}
		m, err := a.o.Queue.SupersedeID(tx, b+AgentSuffix) // 两层都推时的另一条
		if err != nil {
			return err
		}
		if n+m == 0 {
			continue
		}
		var keys []string
		if err := scan(tx, func(r *store.Rows) error {
			var id string
			if err := r.Scan(&id); err != nil {
				return err
			}
			keys = append(keys, id)
			return nil
		}, `SELECT id FROM attention_done WHERE id LIKE ?`, schedInPrefix+b+"|%"); err != nil {
			return err
		}
		for _, k := range keys {
			_, member, _ := strings.Cut(k, "|")
			if _, err := tx.Exec(`DELETE FROM attention_done WHERE id IN (?, ?)`, k, member); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- 预览（设置页「开启后会提醒多少条」） ----

// Preview 是按当前数据、假设现在开启时接下来 24 小时的到点提醒：总条数、合并后的批数、下一批的时刻与条数，以及送到哪。
type Preview struct {
	Total     int        `json:"total"`
	Batches   int        `json:"batches"`
	NextAt    *time.Time `json:"next_at,omitempty"`
	NextCount int        `json:"next_count"`
	// Route 是送往的路由（矩阵 scheduled 行，两层都推时取 Agent 层）；Blocked 非空时主会话不能唤醒（executors.Main*），
	// 提醒按该行的退路走（默认落 Web 未读）。
	Route   string         `json:"route"`
	Blocked string         `json:"blocked,omitempty"`
	Entries []PreviewEntry `json:"entries"`
}

// PreviewEntry 是预览里的一条（至多 20 条）。
type PreviewEntry struct {
	Item  string    `json:"item"`
	Title string    `json:"title"`
	Kind  string    `json:"kind"`
	Why   string    `json:"why"`
	At    time.Time `json:"at"`
}

// SchedulePreview 按设置 s 算预览：提醒点按开关取 (now, now+24h]；同一事项同一类型相距不足去重窗口的只算一次；
// 静默时段内的点并到静默结束那一刻；同一时刻（到分钟）的点是一批；每日上限按本机自然日截断（截断的次日合并，算作一批）。只读。
func SchedulePreview(tx *store.Tx, pol policy.Policy, s policy.Proactive, now time.Time) (Preview, error) {
	pv := Preview{Entries: []PreviewEntry{}}
	sched := row{keys: []string{policy.RowScheduled}, floor: true}
	l, err := resolve(tx, pol, sched, "")
	if err != nil {
		return pv, err
	}
	pv.Route = orElse(orElse(l.agent, l.user), delivery.RouteWeb)
	if t, _ := pol.Disturb.Matrix(nil).Lookup("", policy.RowScheduled); t.Agent == policy.AgentMain {
		if _, pv.Blocked, err = MainRoute(tx, pol); err != nil {
			return pv, err
		}
	}
	pts, err := duePoints(tx, s, now, now.Add(24*time.Hour))
	if err != nil {
		return pv, err
	}
	last := map[string]time.Time{}
	type slot struct {
		at time.Time
		n  int
	}
	var slots []slot
	window := time.Duration(s.DedupMin) * time.Minute
	for _, p := range pts {
		k := p.Kind + ":" + p.Item.ID
		if t, ok := last[k]; ok && window > 0 && p.At.Sub(t) < window {
			continue
		}
		last[k] = p.At
		rel := p.At.Truncate(time.Minute)
		if end, ok := quietEnd(pol, p.At); ok {
			rel = end
		}
		if len(slots) > 0 && slots[len(slots)-1].at.Equal(rel) {
			slots[len(slots)-1].n++
		} else {
			slots = append(slots, slot{rel, 1})
		}
		if len(pv.Entries) < 20 {
			pv.Entries = append(pv.Entries, PreviewEntry{Item: p.Item.ID, Title: p.Item.Title, Kind: p.Kind, Why: p.Why, At: p.At})
		}
	}
	sort.SliceStable(slots, func(i, j int) bool { return slots[i].at.Before(slots[j].at) })
	var items, batches int
	day := ""
	deferred := 0
	for _, sl := range slots {
		if d := sl.at.In(time.Local).Format("2006-01-02"); d != day {
			day, items, batches = d, 0, 0
		}
		n := sl.n
		if s.MaxBatches > 0 && batches >= s.MaxBatches {
			n = 0
		}
		if s.MaxItems > 0 {
			n = min(n, s.MaxItems-items)
		}
		deferred += sl.n - max(n, 0)
		if n <= 0 {
			continue
		}
		items, batches = items+n, batches+1
		pv.Total += n
		pv.Batches++
		if pv.NextAt == nil {
			t := sl.at
			pv.NextAt, pv.NextCount = &t, n
		}
	}
	if deferred > 0 { // 超上限的攒到下一个工作日合并成一条，也算在预览里
		pv.Total += deferred
		pv.Batches++
	}
	return pv, nil
}
