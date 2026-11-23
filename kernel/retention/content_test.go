package retention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var web = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

const rawBody = "RAW-BODY-MARKER"

func newContentEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, 30*24*time.Hour, 14*24*time.Hour)
	for _, err := range []error{registry.Register(e.x, registry.Options{Declared: func(s domain.Source) string { return "src " + s.ID }}), items.Register(e.x, items.NewKinds()), blobs.Register(e.x, e.lay.Blobs)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) do(cmd string, in any) map[string]any {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: web, Command: cmd, Input: b, Evidence: "owner said so"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	var out map[string]any
	_ = json.Unmarshal(r.Value, &out)
	return out
}

func (e *env) source(id string, days any) {
	e.t.Helper()
	e.do(registry.CmdRegisterExec, map[string]any{"id": id, "command": "echo", "schedule": "1m", "timeout": 30})
	e.do(registry.CmdSetRetention, map[string]any{"id": id, "days": days})
}

// event 落一条来源事件（落盘时刻为 e.now），可带处置、挂靠事项与取回的附件 blob；返回事件 id。
func (e *env) event(src, id string, d domain.Disposition, item, blob string) string {
	e.t.Helper()
	var sid string
	err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := runner.AppendInternal(tx, domain.Source{ID: src}, domain.SourceEvent{ID: id, Type: "mail", Subject: "subj " + id, Sender: "a@example.org", Link: "https://x.example/" + id, Detail: rawBody + id, OccurredAt: e.now}); err != nil {
			return err
		}
		evs, err := runner.Events(tx, src)
		if err != nil {
			return err
		}
		for _, ev := range evs {
			if ev.DedupeKey.EventID == id {
				sid = ev.ID
			}
		}
		if d != domain.DispositionPending {
			if err := runner.SetDisposition(tx, sid, d, item, ""); err != nil {
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
		e.t.Fatal(err)
	}
	return sid
}

func (e *env) content(sid string) (state string, parts int, subject string) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		mf, err := runner.ContentOf(tx, sid)
		if err != nil {
			return err
		}
		ev, err := runner.Event(tx, sid)
		state, parts, subject = mf.State, len(mf.Parts), ev.Subject
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) sweepContent() {
	e.t.Helper()
	if err := e.svc.ExpireContent(bg); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) putBlob(content string) string {
	e.t.Helper()
	return e.do("blob.put", map[string]any{"content": []byte(content)})["hash"].(string)
}

func (e *env) hasBlob(h string) bool {
	_, err := os.Stat(filepath.Join(e.lay.Blobs, h))
	return err == nil
}

func (e *env) audits() (out []eventlog.Entry) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		all, err := eventlog.Read(tx, 0, 10000)
		for _, en := range all {
			if en.Event.Type == EvContentPurged {
				out = append(out, en)
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) newItem() string {
	e.t.Helper()
	return e.do("item.create", map[string]any{"title": "t", "kind": "task", "phase": "open"})["id"].(string)
}

// ACT-RET-02：超过天数且不挂未关闭事项的原文被清，事件行留元信息并标 purged；取回的附件随之删除；
// 挂在未关闭事项上的、还没处置的、没到期的保留；每轮只记一条审计事件，不逐条记，也不出现在对外事件类型里。
func TestRetentionPurgesDueContentOnly(t *testing.T) {
	e := newContentEnv(t)
	e.source("src_a", 1)
	open := e.newItem()
	closed := e.newItem()
	e.do("item.transition", map[string]any{"id": closed, "to": "dropped", "reason": "no"})
	h := e.putBlob("fetched attachment")

	free := e.event("src_a", "free", domain.DispositionIgnored, "", h)
	free2 := e.event("src_a", "free2", domain.DispositionDigest, "", "")
	onClosed := e.event("src_a", "on-closed", domain.DispositionProposed, closed, "")
	onOpen := e.event("src_a", "on-open", domain.DispositionProposed, open, "")
	pending := e.event("src_a", "pending", domain.DispositionPending, "", "")
	e.now = e.now.Add(36 * time.Hour)
	fresh := e.event("src_a", "fresh", domain.DispositionIgnored, "", "")
	e.now = e.now.Add(36 * time.Hour) // 前五条落盘 72 小时，fresh 36 小时

	e.sweepContent()

	for _, sid := range []string{free, free2, onClosed} {
		if st, parts, subj := e.content(sid); st != runner.StatePurged || parts != 0 || subj == "" {
			t.Errorf("%s: state=%s parts=%d subject=%q, want purged with metadata kept", sid, st, parts, subj)
		}
	}
	for name, sid := range map[string]string{"open item": onOpen, "pending": pending} {
		if st, parts, _ := e.content(sid); st != runner.StateFull || parts == 0 {
			t.Errorf("%s: state=%s parts=%d, want content kept", name, st, parts)
		}
	}
	// 事件 fresh 落盘 36 小时，保留天数 1 天时同样到期
	if st, _, _ := e.content(fresh); st != runner.StatePurged {
		t.Errorf("fresh: state=%s", st)
	}
	if e.hasBlob(h) {
		t.Error("fetched attachment of a purged event survived")
	}

	as := e.audits()
	if len(as) != 1 {
		t.Fatalf("audit events = %d, want one per source per round", len(as))
	}
	var p ContentResult
	if err := json.Unmarshal(as[0].Event.Payload, &p); err != nil || p.SourceID != "src_a" || p.Events != 4 || p.Bytes <= 0 || len(p.BlobsRemoved) != 1 {
		t.Fatalf("audit payload %s (%v)", as[0].Event.Payload, err)
	}
	if domain.IsRegisteredEventType(EvContentPurged) {
		t.Fatal("audit event must not be in the public event type table")
	}

	e.sweepContent()
	if len(e.audits()) != 1 {
		t.Fatal("a round with nothing due recorded another audit event")
	}
}

// 事项关闭后，它挂着的来源事件下一轮就被清。
func TestRetentionFollowsItemClose(t *testing.T) {
	e := newContentEnv(t)
	e.source("src_a", 1)
	it := e.newItem()
	sid := e.event("src_a", "m", domain.DispositionAttached, it, "")
	e.now = e.now.Add(48 * time.Hour)
	e.sweepContent()
	if st, _, _ := e.content(sid); st != runner.StateFull {
		t.Fatalf("event of an open item purged: %s", st)
	}
	e.do("item.transition", map[string]any{"id": it, "to": "done", "reason": "shipped"})
	e.sweepContent()
	if st, _, _ := e.content(sid); st != runner.StatePurged {
		t.Fatalf("event of a closed item kept: %s", st)
	}
}

// ACT-RET-03：没设天数的来源不清理；null 把已设的改回不清理。
func TestRetentionUnsetMeansKeep(t *testing.T) {
	e := newContentEnv(t)
	e.source("src_none", nil)
	e.source("src_set", 1)
	a := e.event("src_none", "a", domain.DispositionIgnored, "", "")
	b := e.event("src_set", "b", domain.DispositionIgnored, "", "")
	e.now = e.now.Add(1000 * 24 * time.Hour)
	e.do(registry.CmdSetRetention, map[string]any{"id": "src_set", "days": nil})
	e.sweepContent()
	for _, sid := range []string{a, b} {
		if st, _, _ := e.content(sid); st != runner.StateFull {
			t.Fatalf("%s purged without a retention setting", sid)
		}
	}
	if len(e.audits()) != 0 {
		t.Fatal("nothing was purged but an audit event was recorded")
	}
}

// 取回的附件仍被保留下来的事件（或材料）引用时不删。
func TestRetentionKeepsSharedAttachmentBlob(t *testing.T) {
	e := newContentEnv(t)
	e.source("src_a", 1)
	it := e.newItem()
	h := e.putBlob("shared")
	e.event("src_a", "gone", domain.DispositionIgnored, "", h)
	kept := e.event("src_a", "kept", domain.DispositionProposed, it, h)
	e.now = e.now.Add(48 * time.Hour)
	e.sweepContent()
	if !e.hasBlob(h) {
		t.Fatal("blob still referenced by a kept event was deleted")
	}
	if st, _, _ := e.content(kept); st != runner.StateFull {
		t.Fatal("kept event purged")
	}
	var p ContentResult
	_ = json.Unmarshal(e.audits()[0].Event.Payload, &p)
	if len(p.BlobsRemoved) != 0 {
		t.Fatalf("audit says blobs removed: %v", p.BlobsRemoved)
	}
}

// 升级前落盘的事件（legacy）正文在旧列里：清掉旧列，字节数按旧列现算；落盘时刻缺失时按发生时刻算。
func TestRetentionPurgesLegacyEvent(t *testing.T) {
	e := newContentEnv(t)
	e.source("src_a", 1)
	sid := e.event("src_a", "old", domain.DispositionIgnored, "", "")
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM sources_runner_event_part WHERE event_id = ?`, sid); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE sources_runner_event SET content_state = 'legacy', ingested_at = NULL, detail = ?, content_bytes = NULL WHERE id = ?`, rawBody, sid)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(48 * time.Hour)
	e.sweepContent()
	if st, _, _ := e.content(sid); st != runner.StatePurged {
		t.Fatalf("state = %s", st)
	}
	var p ContentResult
	_ = json.Unmarshal(e.audits()[0].Event.Payload, &p)
	if p.Bytes != int64(len(rawBody)) {
		t.Fatalf("bytes = %d, want %d", p.Bytes, len(rawBody))
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		_, err := runner.ReadPart(tx, sid, runner.PartBody)
		if err == nil {
			t.Error("purged content still readable")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 维护循环的入口 Sweep 也清来源原文。
func TestSweepIncludesSourceContent(t *testing.T) {
	e := newContentEnv(t)
	e.source("src_a", 1)
	sid := e.event("src_a", "m", domain.DispositionIgnored, "", "")
	e.now = e.now.Add(48 * time.Hour)
	if _, err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if st, _, _ := e.content(sid); st != runner.StatePurged {
		t.Fatalf("state = %s", st)
	}
}
