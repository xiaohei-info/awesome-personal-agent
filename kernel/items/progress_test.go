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
	pend := e.create(t, owner, map[string]any{"title": "G", "phase": "needs_input", "ask": map[string]any{"question": "q?"}})
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
