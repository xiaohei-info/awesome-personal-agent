package task

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var kernel = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "kernel:inbound", Via: domain.ViaSource, OnBehalfOf: "owner"}, Level: authz.LevelDelegate}

func TestBriefWindowStartsFromPreviousBriefOfSameInstance(t *testing.T) {
	now := t0.Add(48 * time.Hour)
	prev := t0.Add(30 * time.Hour)
	mk := func(id, origin string, until time.Time) domain.Item {
		sp, _ := json.Marshal(map[string]any{"until": until})
		return domain.Item{ID: id, Kind: domain.KindBrief, Origin: &domain.Origin{Source: origin}, Spec: sp}
	}
	all := []domain.Item{mk("a", "instance:ins_1", t0), mk("b", "instance:ins_1", prev), mk("c", "instance:other", now.Add(-time.Hour)), mk("self", "instance:ins_1", now)}
	if w := briefWindow(all, "ins_1", "self", now); !w.Since.Equal(prev) || !w.Until.Equal(now) {
		t.Fatalf("window must start at the latest earlier brief of this instance only: %+v", w)
	}
	if w := briefWindow(nil, "ins_1", "self", now); !w.Since.Equal(now.Add(-24 * time.Hour)) {
		t.Fatalf("first brief of an instance takes 24h: %+v", w)
	}
}

func TestScheduleDaysDefaultsAndBounds(t *testing.T) {
	for in, want := range map[string]int{`{}`: 3, `{"schedule_days":7}`: 7, `{"schedule_days":0}`: 3, `{"schedule_days":15}`: 3} {
		if got := scheduleDays(json.RawMessage(in)); got != want {
			t.Fatalf("%s: %d want %d", in, got, want)
		}
	}
}

// ACT-BRF-02/03/04/07、ACT-INA-05：简报资料带五个新段并把取材窗口写进事项；导入的事件不计入当天活动。
func TestBriefMaterialsCarryLedgerSectionsAndWindow(t *testing.T) {
	e := setup(t)
	e.harness("hx", "summary")
	e.setDefaultHarness("hx")
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec(`INSERT INTO sources_registry_source
			(id, name, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, default_project, outbound_local, outbound_external, config, cursor, enabled, stats)
			VALUES ('src_cal', '工作日历', 'script', 'calendar', '', '', 0, '', '', '', 'full', '', '', '', 1, '')`); err != nil {
			return err
		}
		src := domain.Source{ID: "src_cal"}
		for _, ev := range []domain.SourceEvent{
			{ID: "m1", Type: "event.created", Subject: "周会", OccurredAt: t0.Add(5 * time.Hour), CorrelationKeys: []string{"k1"}},
			{ID: "m2", Type: "event.created", Subject: "取消的会", OccurredAt: t0.Add(6 * time.Hour), CorrelationKeys: []string{"k2"}},
			{ID: "m2c", Type: "event.cancelled", Subject: "取消的会", OccurredAt: t0.Add(6 * time.Hour), CorrelationKeys: []string{"k2"}},
			{ID: "m3", Type: "event.created", Subject: "太远的会", OccurredAt: t0.Add(10 * 24 * time.Hour), CorrelationKeys: []string{"k3"}},
		} {
			if err := runner.AppendInternal(tx, src, ev); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	prop := func(title string) {
		e.must(kernel, "item.create", map[string]any{"kind": "task", "title": title, "phase": "proposed",
			"proposal": map[string]any{"recommend": "accept", "reason": "r", "low_confidence": true}})
	}
	for i := 0; i < 52; i++ {
		prop("建议-" + string(rune('A'+i%26)) + string(rune('a'+i/26)))
	}
	e.must(owner, "item.create", map[string]any{"kind": "task", "title": "待决事", "phase": "needs_input", "ask": map[string]any{"question": "要不要？"}})
	e.must(owner, "item.create", map[string]any{"kind": "task", "title": "今天到期", "phase": "open", "deadline": t0})
	var done struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", map[string]any{"kind": "task", "title": "现场完成", "phase": "open"}).Value, &done)
	e.must(owner, "item.transition", map[string]any{"id": done.ID, "to": "done", "reason": "ok", "evidence": "e"})
	e.must(owner, "item.progress", map[string]any{"id": done.ID, "text": "LIVE-NOTE"})
	if err := e.st.Update(bg, func(tx *store.Tx) error { // 导入事件：At 在窗口内，但不是现场发生的
		b, _ := json.Marshal(map[string]any{"progress": domain.Progress{ID: "x", Text: "IMPORTED-NOTE", At: t0}})
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemProgressAdded, Object: done.ID, Actor: owner.Actor, Reason: "import",
			Payload: b, At: t0.Add(-time.Hour), Imported: "imp_1", RecordedAt: t0})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	id := e.openItemWithOrigin("instance:ins_1")
	if err := e.svc.CallAgent(AgentOptions{})(bg, e.call("brief", id)); err != nil {
		t.Fatal(err)
	}
	m := materialsOf(e, e.waitResult().Run.ID)

	if !strings.Contains(m["schedule"], "周会") || strings.Contains(m["schedule"], "取消的会") || strings.Contains(m["schedule"], "太远的会") || !strings.Contains(m["schedule"], "工作日历") {
		t.Fatalf("schedule: %s", m["schedule"])
	}
	var pr struct {
		Proposals []struct{ Title string } `json:"proposals"`
		Total     int                      `json:"pending_total"`
	}
	if err := json.Unmarshal([]byte(m["proposals"]), &pr); err != nil || len(pr.Proposals) != 50 || pr.Total != 52 {
		t.Fatalf("proposals must cap at 50 and report all 52 pending: %v %d/%d", err, len(pr.Proposals), pr.Total)
	}
	if !strings.Contains(m["decisions"], "要不要？") || !strings.Contains(m["due_waiting"], "今天到期") || !strings.Contains(m["due_waiting"], "due_today") {
		t.Fatalf("decisions %s due_waiting %s", m["decisions"], m["due_waiting"])
	}
	if !strings.Contains(m["activity"], "LIVE-NOTE") || !strings.Contains(m["activity"], "现场完成") || strings.Contains(m["activity"], "IMPORTED-NOTE") {
		t.Fatalf("activity must count live events only: %s", m["activity"])
	}
	var sp struct{ Since, Until time.Time }
	if err := json.Unmarshal(e.item(id).Spec, &sp); err != nil || !sp.Until.Equal(t0) || !sp.Since.Equal(t0.Add(-24*time.Hour)) {
		t.Fatalf("window must be written to spec: %v %s", err, e.item(id).Spec)
	}
	if o := e.item(id).Origin; o == nil || o.Source != "instance:ins_1" {
		t.Fatalf("origin: %+v", o)
	}
}

func (e *env) openItemWithOrigin(source string) string {
	e.t.Helper()
	var ref struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", map[string]any{"kind": "brief", "title": "tk 2026-03-01", "phase": "open", "origin": map[string]string{"source": source}}).Value, &ref)
	return ref.ID
}

func materialsOf(e *env, run string) map[string]string {
	e.t.Helper()
	var f struct {
		Materials []Material `json:"materials"`
	}
	if err := json.Unmarshal([]byte(read(e.t, e.runs+"/"+run+"/materials.json")), &f); err != nil {
		e.t.Fatal(err)
	}
	out := map[string]string{}
	for _, m := range f.Materials {
		out[m.Name] = m.Body
	}
	return out
}

// 评审阻断项：Agent 写回 {period, sections} 会整体替换 spec；窗口仍要留在 spec 里，下一份简报以它的 until 为起点。
func TestBriefWindowSurvivesAgentWriteBackAndSeedsNextBrief(t *testing.T) {
	e := setup(t)
	e.harness("hx", "noOutcome")
	e.setDefaultHarness("hx")
	run := func(id string) {
		if err := e.svc.CallAgent(AgentOptions{})(bg, e.call("write-brief", id)); err != nil {
			t.Fatal(err)
		}
		runDir := e.runs + "/" + func() string { // 运行目录里只有这一次运行
			ents, _ := os.ReadDir(e.runs)
			return ents[len(ents)-1].Name()
		}()
		agent := runidentity.Identity{Secret: waitFile(t, runDir+"/token")}
		e.must(agent, "item.update", map[string]any{"id": id, "spec": json.RawMessage(fullSpec), "reason": "write"}) // 只写 period 与 sections
		e.must(agent, "delegate.report", reportIn(id, "progress"))
		if err := os.WriteFile(runDir+"/go", nil, 0o600); err != nil {
			t.Fatal(err)
		}
		e.waitResult()
	}
	first := e.openItemWithOrigin("instance:ins_1")
	run(first)
	var sp struct {
		Period       string
		Since, Until time.Time
	}
	if err := json.Unmarshal(e.item(first).Spec, &sp); err != nil || sp.Period == "" || !sp.Until.Equal(t0) || !sp.Since.Equal(t0.Add(-24*time.Hour)) {
		t.Fatalf("window lost after the agent replaced spec: %v %s", err, e.item(first).Spec)
	}
	if e.item(first).Phase != domain.PhaseDone {
		t.Fatalf("brief must still settle done: %s %+v", e.item(first).Phase, e.item(first).Ask)
	}
	var all []domain.Item
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { all, err = items.List(tx, items.Filter{}); return })
	if w := briefWindow(all, "ins_1", "next", t0.Add(24*time.Hour)); !w.Since.Equal(t0) {
		t.Fatalf("next brief must start at the first one's until: %+v", w)
	}
}
