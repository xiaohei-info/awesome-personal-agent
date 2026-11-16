package purge

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var srcA = domain.Source{ID: "src_a", DedupeDomain: "dom"}

// addEvent 落一条来源事件，可选挂在事项上；带 blob 时在附件清单里记上已取回的附件（取回由 attachment-fetch 节点实现，这里只造夹具）。
func (e *env) addEvent(t *testing.T, id, item, blob string) string {
	t.Helper()
	var sid string
	err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, srcA, domain.SourceEvent{ID: id, Type: "mail", Subject: "s " + id, Detail: marker + id, OccurredAt: time.Now()}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, srcA.ID)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.DedupeKey.EventID == id {
				sid = ev.ID
			}
		}
		if item != "" {
			if err := runner.SetDisposition(tx, sid, domain.DispositionAttached, item, ""); err != nil {
				return err
			}
		}
		if blob != "" {
			list, _ := json.Marshal([]map[string]any{{"name": "a.pdf", "size": 4, "blob": blob}})
			_, err = tx.Exec(`INSERT INTO sources_runner_event_part (event_id, part, media_type, bytes, sha256, content) VALUES (?, 'attachments', 'application/json', ?, 'x', ?)`, sid, len(list), list)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sid
}

func (e *env) eventGone(t *testing.T, sid string) bool {
	t.Helper()
	var gone bool
	if err := e.st.View(bg, func(tx *store.Tx) error {
		_, err := runner.Event(tx, sid)
		gone = err != nil
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return gone
}

func (e *env) readdOnce(t *testing.T, id string) {
	t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return runner.AppendInternal(tx, srcA, domain.SourceEvent{ID: id, Type: "mail", Subject: "again", Detail: "again", OccurredAt: time.Now()})
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) eventCount(t *testing.T) (n int) {
	t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		evs, err := runner.Events(tx, srcA.ID)
		n = len(evs)
		return err
	})
	return
}

func (e *env) judge(t *testing.T, rid, subject string) {
	t.Helper()
	p := 0.5
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return record.Put(tx, record.Record{ID: rid, Use: "inbound_screen", Template: "t@h", Subject: subject, Fingerprint: "f", Band: record.Band{Low: .1, High: .6},
			DecidedBy: record.DecidedClassifier, Answer: "yes", CreatedAt: time.Now(),
			Levels: []record.Level{{Backend: record.BackendClassifier, PYes: &p, Outcome: record.OutcomeDecided}}})
	}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) judgments(t *testing.T) (n int) {
	t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		rs, _, err := record.List(tx, record.Filter{})
		n = len(rs)
		return err
	})
	return
}

// ACT-PRG-01：删一条来源事件后，事件行、原文、无引用的取回附件与判断记录都不在，另记删除事件；其他事件不受影响。
func TestPurgeSourceEventRemovesEverythingItOwns(t *testing.T) {
	e := setup(t)
	h := e.putBlob(t, "attachment-bytes")
	sid := e.addEvent(t, "m1", "", h)
	other := e.addEvent(t, "m2", "", "")
	e.judge(t, "j1", sid)
	e.judge(t, "j2", other)

	res := e.must(t, owner, Command, map[string]any{"object": sid})
	if res["kind"] != KindSourceEvent || n(res["blobs_removed"]) != 1 {
		t.Fatalf("result %v", res)
	}
	if !e.eventGone(t, sid) || e.eventGone(t, other) {
		t.Fatal("purge must delete exactly the named event")
	}
	if e.blobFile(h) {
		t.Fatal("unreferenced fetched attachment survived")
	}
	if e.judgments(t) != 1 {
		t.Fatalf("judgment records left = %d, want only the other event's", e.judgments(t))
	}
	var purged bool
	for _, en := range e.events(t) {
		purged = purged || en.Event.Type == EvPurged && en.Event.Object == sid
	}
	if !purged {
		t.Fatal("no purge.executed event")
	}
	if _, err := e.do(owner, Command, map[string]any{"object": sid}); exit(err) != domain.ExitNotFound {
		t.Fatalf("second purge: %v", err)
	}
}

// ACT-PRG-02：去重键留作墓碑，同一事件再被来源带来时不重新入账。
func TestPurgedSourceEventIsNotIngestedAgain(t *testing.T) {
	e := setup(t)
	sid := e.addEvent(t, "m1", "", "")
	e.must(t, owner, Command, map[string]any{"object": sid})
	e.readdOnce(t, "m1")
	if n := e.eventCount(t); n != 0 {
		t.Fatalf("%d events re-ingested after purge", n)
	}
	e.addEvent(t, "m3", "", "") // 别的事件照常入账
	if n := e.eventCount(t); n != 1 {
		t.Fatalf("events = %d", n)
	}
}

// 取回的附件被别的来源事件或材料引用时保留。
func TestPurgeSourceEventKeepsSharedAttachmentBlob(t *testing.T) {
	e := setup(t)
	h := e.putBlob(t, "shared")
	a := e.addEvent(t, "m1", "", h)
	e.addEvent(t, "m2", "", h)
	res := e.must(t, owner, Command, map[string]any{"object": a})
	if n(res["blobs_retained"]) != 1 || !e.blobFile(h) {
		t.Fatalf("blob shared with another event was removed: %v", res)
	}

	h2 := e.putBlob(t, "also used by a material")
	b := e.addEvent(t, "m4", "", h2)
	e.note(t, "see "+"apa://blob/"+h2)
	e.must(t, owner, Command, map[string]any{"object": b})
	if !e.blobFile(h2) {
		t.Fatal("blob referenced by a material was removed")
	}
}

// 删材料时，仍被来源事件附件引用的 blob 也保留。
func TestPurgeMaterialKeepsBlobUsedBySourceEvent(t *testing.T) {
	e := setup(t)
	h := e.putBlob(t, "x")
	e.addEvent(t, "m1", "", h)
	m := e.note(t, "apa://blob/"+h)
	e.must(t, owner, Command, map[string]any{"object": m})
	if !e.blobFile(h) {
		t.Fatal("blob referenced by a source event attachment was removed")
	}
}

// ACT-PRG-03：with_source_events 才连同事项的来源事件一起删；不带不删。
func TestPurgeItemWithSourceEvents(t *testing.T) {
	e := setup(t)
	mk := func() string {
		return e.must(t, owner, "item.create", map[string]any{"title": "t", "kind": "task"})["id"].(string)
	}
	withIt, withoutIt := mk(), mk()
	a1, a2 := e.addEvent(t, "a1", withIt, ""), e.addEvent(t, "a2", withIt, "")
	b1 := e.addEvent(t, "b1", withoutIt, "")
	loose := e.addEvent(t, "c1", "", "")

	res := e.must(t, owner, Command, map[string]any{"object": withIt, "with_source_events": true})
	if n(res["source_events"]) != 2 || !e.eventGone(t, a1) || !e.eventGone(t, a2) {
		t.Fatalf("source events of the item survive: %v", res)
	}
	e.must(t, owner, Command, map[string]any{"object": withoutIt})
	if e.eventGone(t, b1) || e.eventGone(t, loose) {
		t.Fatal("source events must stay unless with_source_events is set")
	}
	e.readdOnce(t, "a1")
	if e.eventCount(t) != 2 {
		t.Fatalf("tombstoned event came back (events = %d)", e.eventCount(t))
	}
}

func TestPurgeWithSourceEventsOnlyForItems(t *testing.T) {
	e := setup(t)
	sid := e.addEvent(t, "m1", "", "")
	m := e.note(t, "n")
	for _, id := range []string{sid, m} {
		if _, err := e.do(owner, Command, map[string]any{"object": id, "with_source_events": true}); exit(err) != domain.ExitUsage {
			t.Fatalf("%s: exit %d (%v)", id, exit(err), err)
		}
	}
	if e.eventGone(t, sid) {
		t.Fatal("rejected purge deleted the event")
	}
}

// 授权同事项：delegate 是 web_only，内容不变。
func TestPurgeSourceEventOnlyOwnerOnWeb(t *testing.T) {
	e := setup(t)
	sid := e.addEvent(t, "m1", "", "")
	if _, err := e.do(delegate, Command, map[string]any{"object": sid}); exit(err) != domain.ExitWebOnly {
		t.Fatalf("delegate: %v", err)
	}
	if e.eventGone(t, sid) {
		t.Fatal("rejected purge deleted the event")
	}
}
