package items

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func day(e *env, delta int) string {
	return e.now.AddDate(0, 0, delta).In(time.Local).Format(dateLayout)
}

func (e *env) progress(t *testing.T, id string) []domain.Progress {
	t.Helper()
	var ps []domain.Progress
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ps, err = Progress(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return ps
}

// ACT-WBN-05/06：计划日期可写可清，不产生 Timer；非法写法被拒且事项不变。
func TestPlannedForSetClearAndValidate(t *testing.T) {
	e := setup(t)
	id := e.create(t, owner, map[string]any{"phase": "open", "assignee": "wrk", "planned_for": day(e, 1)})
	if got := e.get(t, id).PlannedFor; got != day(e, 1) {
		t.Fatalf("create with planned_for: %q", got)
	}
	e.wantNoDue(t, id)
	var timers int
	_ = e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query("SELECT count(*) FROM clock_timer")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			_ = r.Scan(&timers)
		}
		return nil
	})
	if timers != 0 {
		t.Fatal("planned_for must not create a timer")
	}
	e.must(t, worker, "item.update", map[string]any{"id": id, "planned_for": day(e, 2)})
	if got := e.get(t, id).PlannedFor; got != day(e, 2) {
		t.Fatalf("update planned_for: %q", got)
	}
	before := e.get(t, id)
	for _, bad := range []string{"2026-13-40", "2026-11-12T10:00:00Z", "2026-1-2", "tomorrow"} {
		if _, err := e.do(owner, "item.update", map[string]any{"id": id, "planned_for": bad}); exit(err) != domain.ExitUsage {
			t.Fatalf("%q must be rejected with the parameter error, got %v", bad, err)
		}
	}
	if after := e.get(t, id); after.PlannedFor != before.PlannedFor || after.Version != before.Version {
		t.Fatalf("rejected writes changed the item: %+v", after)
	}
	e.must(t, worker, "item.update", map[string]any{"id": id, "planned_for": nil})
	if e.get(t, id).PlannedFor != "" {
		t.Fatal("null must clear planned_for")
	}
	e.verify(t)
}

// ACT-WBN-09/10：进展追加不改阶段，记 item.progress_added，所指日期不晚于今天。
func TestProgressAppendsWithoutPhaseChange(t *testing.T) {
	e := setup(t)
	id := e.create(t, owner, map[string]any{"phase": "open", "assignee": "wrk"})
	v0 := e.get(t, id).Version
	e.must(t, worker, "item.progress", map[string]any{"id": id, "text": "联系了供应商", "on": day(e, -1)})
	e.must(t, worker, "item.progress", map[string]any{"id": id, "text": "无日期"})
	it := e.get(t, id)
	if it.Phase != domain.PhaseOpen || it.Version != v0+2 {
		t.Fatalf("progress must keep phase and bump version: %+v", it)
	}
	ps := e.progress(t, id)
	if len(ps) != 2 || ps[0].Text != "联系了供应商" || ps[0].On != day(e, -1) || ps[0].Actor != "wrk" || !ps[0].At.Equal(e.now) || ps[1].On != "" {
		t.Fatalf("progress rows: %+v", ps)
	}
	var n int
	for _, ev := range e.events(t) {
		if ev.Type == domain.EvItemProgressAdded && ev.Object == id {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("want 2 item.progress_added events, got %d", n)
	}
	e.must(t, worker, "item.progress", map[string]any{"id": id, "text": "今天", "on": day(e, 0)})
	for _, in := range []map[string]any{
		{"id": id, "text": "未来", "on": day(e, 1)},
		{"id": id, "text": "坏日期", "on": "2026-13-40"},
		{"id": id, "text": "  "},
	} {
		if _, err := e.do(worker, "item.progress", in); exit(err) != domain.ExitUsage {
			t.Fatalf("%v must be rejected: %v", in, err)
		}
	}
	if len(e.progress(t, id)) != 3 {
		t.Fatal("rejected progress must not be stored")
	}
	e.verify(t)
	// 投影坏了可由事件重建，进展一并回来。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec("DELETE FROM item_progress"); err != nil {
			return err
		}
		return Rebuild(tx)
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.progress(t, id); len(got) != 3 || got[0].Text != "联系了供应商" {
		t.Fatalf("rebuild lost progress: %+v", got)
	}
	e.verify(t)
}

// ACT-WBN-11：worker 只能写自己的事项；delegate 写非绑定事项要依据。
func TestProgressPermissions(t *testing.T) {
	e := setup(t)
	mine := e.create(t, owner, map[string]any{"phase": "open", "assignee": "wrk"})
	other := e.create(t, owner, map[string]any{"phase": "open"})
	if _, err := e.do(agent("wrk2", "worker"), "item.progress", map[string]any{"id": mine, "text": "x"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker on another's item: %v", err)
	}
	if _, err := e.x.Do(bg, newReq(delegate, other, "")); exit(err) != domain.ExitBasisRequired {
		t.Fatalf("delegate without basis: %v", err)
	}
	if len(e.progress(t, other)) != 0 {
		t.Fatal("rejected progress was stored")
	}
	if _, err := e.x.Do(bg, newReq(delegate, other, "已订好了")); err != nil {
		t.Fatalf("delegate with evidence: %v", err)
	}
	if len(e.progress(t, other)) != 1 {
		t.Fatal("authorized progress not stored")
	}
	if rejections(e.events(t)) != 1 { // 退出码 23 的拒绝由内核记 command.rejected；4 沿用既有行为，不记
		t.Fatal("the missing-basis refusal must leave command.rejected")
	}
}

// ACT-WBN-07：四组取材；过期的计划留在 planned，不改期。
func TestTodayGroups(t *testing.T) {
	e := setup(t)
	e.create(t, owner, map[string]any{"title": "A", "phase": "open", "planned_for": day(e, -1)})
	e.create(t, owner, map[string]any{"title": "B", "phase": "open", "planned_for": day(e, 0)})
	e.create(t, owner, map[string]any{"title": "C", "phase": "open", "planned_for": day(e, 1)})
	e.create(t, owner, map[string]any{"title": "E", "phase": "done", "planned_for": day(e, 0), "reason": "r"})
	pend := e.create(t, owner, map[string]any{"title": "G", "phase": "needs_input", "ask": map[string]any{"question": "q?", "free_text": true}})
	stale := e.create(t, owner, map[string]any{"title": "S", "phase": "open"})
	fresh := e.create(t, owner, map[string]any{"title": "F", "phase": "open"})
	e.now = e.now.Add(StaleAfter + time.Hour)
	e.must(t, owner, "item.progress", map[string]any{"id": fresh, "text": "动了"})
	var l TodayList
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { l, err = TodayItems(tx, e.now); return }); err != nil {
		t.Fatal(err)
	}
	ids := func(its []domain.Item) (out []string) {
		for _, it := range its {
			out = append(out, it.Title)
		}
		return
	}
	// 时钟已推进 14 天多：所有计划日期都已过期，仍留在 planned；done 不在。
	if got := ids(l.Planned); len(got) != 3 {
		t.Fatalf("planned: %v", got)
	}
	if got := ids(l.Pending); len(got) != 1 || l.Pending[0].ID != pend {
		t.Fatalf("pending: %v", got)
	}
	foundStale, foundFresh := false, false
	for _, it := range l.Stale {
		foundStale = foundStale || it.ID == stale
		foundFresh = foundFresh || it.ID == fresh
	}
	if !foundStale || foundFresh {
		t.Fatalf("stale must hold untouched items only: %v", ids(l.Stale))
	}
}

// 今天当天的取材：昨天、今天进 planned，明天与 done 不进；今天到期进 due。
func TestTodayPlannedAndDue(t *testing.T) {
	e := setup(t)
	a := e.create(t, owner, map[string]any{"title": "A", "phase": "open", "planned_for": day(e, -1)})
	b := e.create(t, owner, map[string]any{"title": "B", "phase": "open", "planned_for": day(e, 0)})
	e.create(t, owner, map[string]any{"title": "C", "phase": "open", "planned_for": day(e, 1)})
	e.create(t, owner, map[string]any{"title": "E", "phase": "done", "planned_for": day(e, 0), "reason": "r"})
	d := e.create(t, owner, map[string]any{"title": "D", "phase": "open", "deadline": e.now.Add(time.Minute)})
	var l TodayList
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { l, err = TodayItems(tx, e.now); return }); err != nil {
		t.Fatal(err)
	}
	if len(l.Planned) != 2 || l.Planned[0].ID != a || l.Planned[1].ID != b || l.Planned[0].PlannedFor != day(e, -1) {
		t.Fatalf("planned: %+v", l.Planned)
	}
	if len(l.Due) != 1 || l.Due[0].ID != d || len(l.Pending) != 0 || len(l.Stale) != 0 {
		t.Fatalf("groups: %+v", l)
	}
}

func newReq(id command.Identity, item, evidence string) command.Request {
	b, _ := json.Marshal(map[string]any{"id": item, "text": "x"})
	return command.Request{Identity: id, Command: "item.progress", Input: b, Owner: "owner", Evidence: evidence}
}

// 回归：设了 planned_for 不能让阶段不变量（waiting 要 waiting_on、needs_input 要 ask）失效。
func TestPlannedForKeepsPhaseInvariants(t *testing.T) {
	e := setup(t)
	w := e.create(t, owner, map[string]any{"phase": "waiting", "assignee": "wrk", "planned_for": day(e, 1),
		"waiting_on": map[string]any{"kind": "external", "ref": "x"}})
	n := e.create(t, owner, map[string]any{"phase": "needs_input", "assignee": "wrk", "planned_for": day(e, 1), "ask": map[string]any{"question": "q?", "free_text": true}})
	if _, err := e.do(worker, "item.update", map[string]any{"id": w, "waiting_on": nil}); exit(err) != domain.ExitUsage {
		t.Fatalf("clearing waiting_on on a waiting item must be rejected: %v", err)
	}
	if _, err := e.do(worker, "item.update", map[string]any{"id": n, "ask": nil}); exit(err) != domain.ExitUsage {
		t.Fatalf("clearing ask on a needs_input item must be rejected: %v", err)
	}
	if e.get(t, w).WaitingOn == nil || e.get(t, n).Ask == nil {
		t.Fatal("rejected updates changed the items")
	}
}

// 久未推进对导入事项从导入时刻起算（§7.1、§7.3）：事项的 updated_at 是原始时刻（很久以前），
// 但导入时刻在阈值之内就不算久未推进；导入时刻也过了阈值才列入；之后的普通变更照常刷新。
func TestStaleCountsFromImportTimeForImportedItems(t *testing.T) {
	e := setup(t)
	old := e.create(t, owner, map[string]any{"title": "旧", "phase": "open"})
	imported := e.create(t, owner, map[string]any{"title": "导入", "phase": "open"})
	e.now = e.now.Add(StaleAfter + time.Hour)
	mark := func(id string, at time.Time) {
		t.Helper()
		b, _ := json.Marshal(map[string]any{"batch": "imp_1", "at": at})
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			_, err := tx.Exec(`UPDATE items SET imported = ? WHERE id = ?`, string(b), id)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	stale := func() map[string]bool {
		t.Helper()
		var l TodayList
		if err := e.st.View(bg, func(tx *store.Tx) (err error) { l, err = TodayItems(tx, e.now); return }); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for _, it := range l.Stale {
			out[it.ID] = true
		}
		return out
	}
	mark(imported, e.now.Add(-time.Hour)) // 一小时前才导入
	if got := stale(); !got[old] || got[imported] {
		t.Fatalf("imported item counts from import time, plain old item is stale: %v", got)
	}
	mark(imported, e.now.Add(-StaleAfter-time.Minute)) // 导入也已超过阈值
	if got := stale(); !got[imported] {
		t.Fatalf("import time past the threshold must list it: %v", got)
	}
}

// W26：时段的写法与边界、有时段必须有日期、改日期时段不动、清日期一起清时段。
func TestPlannedSlotRules(t *testing.T) {
	e := setup(t)
	id := e.create(t, owner, map[string]any{"phase": "open", "planned_for": day(e, 1), "planned_slot": "00:00 - 00:30"})
	for _, ok := range []string{"23:30 - 24:00", "09:05 - 09:06"} {
		e.must(t, owner, "item.update", map[string]any{"id": id, "planned_slot": ok})
		if got := e.get(t, id).PlannedSlot; got != ok {
			t.Fatalf("slot %q stored as %q", ok, got)
		}
	}
	before := e.get(t, id)
	for _, bad := range []string{"9:00 - 10:00", "09:00-10:00", "09:00 – 10:00", "10:00 - 09:00", "10:00 - 10:00", "23:00 - 01:00", "24:00 - 24:30", "09:60 - 10:00", "09:00 - 24:01"} {
		if _, err := e.do(owner, "item.update", map[string]any{"id": id, "planned_slot": bad}); exit(err) != domain.ExitUsage {
			t.Fatalf("%q must be refused as a usage error, got %v", bad, err)
		}
	}
	if after := e.get(t, id); after.PlannedSlot != before.PlannedSlot || after.Version != before.Version {
		t.Fatalf("refused writes changed the item: %+v", after)
	}
	// 有时段必须有日期：新建不给日期、更新时同时清日期都被拒。
	if _, err := e.do(owner, "item.create", map[string]any{"kind": "task", "title": "x", "phase": "open", "planned_slot": "09:00 - 10:00"}); exit(err) != domain.ExitUsage {
		t.Fatalf("slot without a date must be refused, got %v", err)
	}
	if _, err := e.do(owner, "item.update", map[string]any{"id": id, "planned_for": nil, "planned_slot": "09:00 - 10:00"}); exit(err) != domain.ExitUsage {
		t.Fatalf("clearing the date while writing a slot must be refused, got %v", err)
	}
	e.must(t, owner, "item.update", map[string]any{"id": id, "planned_for": day(e, 3)})
	if it := e.get(t, id); it.PlannedFor != day(e, 3) || it.PlannedSlot != "09:05 - 09:06" {
		t.Fatalf("changing the date must keep the slot: %+v", it)
	}
	e.must(t, owner, "item.update", map[string]any{"id": id, "planned_for": nil})
	if it := e.get(t, id); it.PlannedFor != "" || it.PlannedSlot != "" {
		t.Fatalf("clearing the date must clear the slot in the same command: %+v", it)
	}
	e.verify(t) // 折叠回放后时段与投影一致
}

// W26：同一幂等键重复提交只建一件；带旧版本号的改期返回冲突且不改数据。
func TestScheduleWritesAreIdempotentAndVersioned(t *testing.T) {
	e := setup(t)
	in, _ := json.Marshal(map[string]any{"kind": "task", "title": "会", "phase": "open", "planned_for": day(e, 1), "planned_slot": "14:00 - 15:00"})
	req := command.Request{Identity: owner, Command: "item.create", Input: in, Owner: "owner", IdemKey: "new-page-1"}
	r1, err1 := e.x.Do(bg, req)
	r2, err2 := e.x.Do(bg, req)
	if err1 != nil || err2 != nil || string(r1.Value) != string(r2.Value) {
		t.Fatalf("replay must return the first result: %s / %s (%v %v)", r1.Value, r2.Value, err1, err2)
	}
	var all []domain.Item
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { all, err = List(tx, Filter{}); return })
	if len(all) != 1 {
		t.Fatalf("two submits made %d items", len(all))
	}
	var out struct{ ID string }
	_ = json.Unmarshal(r1.Value, &out)
	stale := e.get(t, out.ID).Version
	e.must(t, owner, "item.update", map[string]any{"id": out.ID, "planned_for": day(e, 2)}) // 别处先改了
	up, _ := json.Marshal(map[string]any{"id": out.ID, "planned_for": day(e, 5), "reason": "改期"})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "item.update", Input: up, Owner: "owner", ExpectVersion: &stale}); exit(err) != domain.ExitConflict {
		t.Fatalf("a stale version must be a conflict, got %v", err)
	}
	if got := e.get(t, out.ID).PlannedFor; got != day(e, 2) {
		t.Fatalf("the conflicting write must not apply: %s", got)
	}
}

// W26："今天"的计划组同一天里有时段的在前、按开始时间排，没时段的在后。
func TestTodayPlannedOrdersBySlot(t *testing.T) {
	e := setup(t)
	e.create(t, owner, map[string]any{"title": "无时段", "phase": "open", "planned_for": day(e, 0)})
	e.create(t, owner, map[string]any{"title": "下午", "phase": "open", "planned_for": day(e, 0), "planned_slot": "14:00 - 15:00"})
	e.create(t, owner, map[string]any{"title": "上午", "phase": "open", "planned_for": day(e, 0), "planned_slot": "09:00 - 10:00"})
	var l TodayList
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { l, err = TodayItems(tx, e.now); return }); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, it := range l.Planned {
		got = append(got, it.Title)
	}
	if len(got) != 3 || got[0] != "上午" || got[1] != "下午" || got[2] != "无时段" {
		t.Fatalf("planned order: %v", got)
	}
}
