package purge

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/blobs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

const marker = "MARKER-PURGE"

var (
	owner    = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	worker   = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: authz.LevelWorker}
	delegate = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
)

type env struct {
	st   *store.Store
	x    *command.Executor
	blob string
	prj  string
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, materials.Module, proc.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	l := config.LayoutOf(home)
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	x := command.New(st, func() time.Time { now = now.Add(time.Minute); return now })
	for _, err := range []error{projects.Register(x), items.Register(x, items.NewKinds()), materials.Register(x), blobs.Register(x, l.Blobs), Register(x, l.Blobs)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	e := &env{st: st, x: x, blob: l.Blobs}
	e.prj = e.must(t, owner, "project.create", map[string]any{"name": "P"})["id"].(string)
	return e
}

func (e *env) do(id command.Identity, cmd string, in any) (map[string]any, error) {
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
	var out map[string]any
	if err == nil {
		_ = json.Unmarshal(r.Value, &out)
	}
	return out, err
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) map[string]any {
	t.Helper()
	out, err := e.do(id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return out
}

func (e *env) events(t *testing.T) []eventlog.Entry {
	t.Helper()
	var out []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = eventlog.Read(tx, 0, 10000); return }); err != nil {
		t.Fatal(err)
	}
	return out
}

func (e *env) blobFile(h string) bool {
	_, err := os.Stat(filepath.Join(e.blob, h))
	return err == nil
}

func (e *env) putBlob(t *testing.T, content string) string {
	t.Helper()
	return e.must(t, owner, "blob.put", map[string]any{"content": []byte(content)})["hash"].(string)
}

func (e *env) note(t *testing.T, body string) string {
	t.Helper()
	return e.must(t, worker, "material.commit", map[string]any{"project": e.prj, "kind": "minutes", "title": "n", "content": body, "reason": "r"})["material"].(string)
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

// ACT-PRG-01 / ACT-PRG-03：事项投影被删，Event 保留信封、内容成墓碑，序号连续，另记删除事件。
func TestPurgeItemTombstonesEventsKeepsSequence(t *testing.T) {
	e := setup(t)
	id := e.must(t, owner, "item.create", map[string]any{"title": marker, "kind": "task", "reason": marker, "evidence": marker})["id"].(string)
	before := e.events(t)
	cursor := before[len(before)-1].Event.Seq

	res := e.must(t, owner, Command, map[string]any{"object": id})
	if res["kind"] != KindItem || res["events"].(float64) < 1 {
		t.Fatalf("result %v", res)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error { _, err := items.Get(tx, id); return err }); exit(err) != domain.ExitNotFound {
		t.Fatalf("item projection survived purge: %v", err)
	}

	after := e.events(t)
	for i, en := range after {
		if en.Event.Seq != int64(i+1) {
			t.Fatalf("sequence gap at %d: %d", i, en.Event.Seq)
		}
	}
	for _, en := range after {
		if en.Event.Object != id || en.Event.Type == EvPurged {
			continue
		}
		b, _ := json.Marshal(en.Event)
		if !en.Tombstone || strings.Contains(string(b), marker) || en.Event.Actor.Principal != "owner" || en.Event.Type == "" {
			t.Fatalf("event not tombstoned cleanly: %+v", en)
		}
	}
	last := after[len(after)-1]
	if last.Event.Type != EvPurged || last.Event.Object != id || last.Event.Seq <= cursor || last.Tombstone {
		t.Fatalf("delete event %+v", last)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error { return items.Verify(tx) }); err != nil {
		t.Fatalf("projection no longer matches the log: %v", err)
	}
	// 游标：从删除前的序号读，拿到的第一条序号紧随其后。
	var page []eventlog.Entry
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { page, err = eventlog.Read(tx, cursor, 100); return })
	if len(page) != 1 || page[0].Event.Seq != cursor+1 {
		t.Fatalf("cursor read %+v", page)
	}
}

// 物理删除事项时，指向它的判断记录一并删除，别的对象的记录不动（§6.16）。
func TestPurgeItemDeletesItsJudgmentRecords(t *testing.T) {
	e := setup(t)
	id := e.must(t, owner, "item.create", map[string]any{"title": "x", "kind": "task", "reason": "r", "evidence": "e"})["id"].(string)
	put := func(rid, subject string) {
		p := 0.9
		err := e.st.Update(bg, func(tx *store.Tx) error {
			return record.Put(tx, record.Record{ID: rid, Use: "followup_defer", Template: "t@h", Subject: subject, Fingerprint: "f", Band: record.Band{Low: .1, High: .6},
				DecidedBy: record.DecidedClassifier, Answer: "yes", CreatedAt: time.Now(),
				Levels: []record.Level{{Backend: record.BackendClassifier, PYes: &p, Outcome: record.OutcomeDecided}}})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	put("jd_mine", id)
	put("jd_other", "item_other")
	e.must(t, owner, Command, map[string]any{"object": id})
	var left []record.Record
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { left, _, err = record.List(tx, record.Filter{}); return })
	if len(left) != 1 || left[0].ID != "jd_other" {
		t.Fatalf("records left: %+v", left)
	}
}

// 无其他引用的 blob 随材料删除；材料投影和修订一并消失。
func TestPurgeMaterialRemovesUnsharedBlob(t *testing.T) {
	e := setup(t)
	h := e.putBlob(t, marker+" attachment")
	m := e.note(t, "正文 "+marker+" "+blobs.Ref(h))

	res := e.must(t, owner, Command, map[string]any{"object": m})
	if res["kind"] != KindMaterial || n(res["blobs_removed"]) != 1 {
		t.Fatalf("result %v", res)
	}
	if e.blobFile(h) {
		t.Fatal("unshared blob file survived")
	}
	var n int
	_ = e.st.View(bg, func(tx *store.Tx) error {
		_, err := materials.Get(tx, m)
		if err == nil {
			n++
		}
		revs, _ := materials.History(tx, m)
		n += len(revs)
		return nil
	})
	if n != 0 {
		t.Fatal("material or revisions survived")
	}
	for _, en := range e.events(t) {
		b, _ := json.Marshal(en.Event)
		if en.Event.Object == m && en.Event.Type != EvPurged && (!en.Tombstone || strings.Contains(string(b), marker)) {
			t.Fatalf("event leaks content: %s", b)
		}
	}
}

// ACT-PRG-02：两个材料引用同一 blob，删一个，blob 与另一材料保持可读；全删后才清除文件。
func TestPurgeKeepsBlobStillReferencedByAnotherMaterial(t *testing.T) {
	e := setup(t)
	h := e.putBlob(t, "shared")
	a := e.note(t, "A "+blobs.Ref(h))
	b := e.note(t, "B "+blobs.Ref(h))

	res := e.must(t, owner, Command, map[string]any{"object": a})
	if n(res["blobs_removed"]) != 0 || n(res["blobs_retained"]) != 1 {
		t.Fatalf("result %v", res)
	}
	if !e.blobFile(h) {
		t.Fatal("shared blob deleted")
	}
	if got := e.must(t, owner, "blob.get", map[string]any{"hash": h}); got["size"].(float64) != 6 {
		t.Fatalf("blob unreadable: %v", got)
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		_, v, err := materials.Show(tx, b, "")
		if err != nil || !strings.Contains(v.Content, blobs.Ref(h)) {
			t.Fatalf("other material unreadable: %v %+v", err, v)
		}
		return nil
	})

	e.must(t, owner, Command, map[string]any{"object": b})
	if e.blobFile(h) {
		t.Fatal("blob survived after last reference was purged")
	}
}

// ACT-PRG-04 的内核侧：只有 Web 本人能执行；delegate 退出码 20，worker 被拒，内容不变。
func TestPurgeOnlyOwnerOnWeb(t *testing.T) {
	e := setup(t)
	id := e.must(t, owner, "item.create", map[string]any{"title": marker, "kind": "task"})["id"].(string)
	for _, c := range []struct {
		id   command.Identity
		want int
	}{{delegate, domain.ExitWebOnly}, {worker, domain.ExitPermissionDenied}} {
		if _, err := e.do(c.id, Command, map[string]any{"object": id}); exit(err) != c.want {
			t.Fatalf("exit %d want %d (%v)", exit(err), c.want, err)
		}
	}
	for _, en := range e.events(t) {
		if en.Tombstone && en.Event.Type != domain.EvCommandRejected || en.Event.Type == EvPurged {
			t.Fatalf("rejected purge changed the log: %+v", en)
		}
	}
	if err := e.st.View(bg, func(tx *store.Tx) error { _, err := items.Get(tx, id); return err }); err != nil {
		t.Fatalf("item gone after rejected purge: %v", err)
	}
}

func TestPurgeRejectsBadInputAndUnknownObject(t *testing.T) {
	e := setup(t)
	id := e.must(t, owner, "item.create", map[string]any{"title": "t", "kind": "task"})["id"].(string)
	for _, in := range []any{map[string]any{}, map[string]any{"object": ""}, map[string]any{"object": id, "x": 1}} {
		if _, err := e.do(owner, Command, in); exit(err) != domain.ExitUsage {
			t.Fatalf("%v: exit %d (%v)", in, exit(err), err)
		}
	}
	if _, err := e.do(owner, Command, map[string]any{"object": "nope"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("unknown object: %v", err)
	}
	e.must(t, owner, Command, map[string]any{"object": id})
	if _, err := e.do(owner, Command, map[string]any{"object": id}); exit(err) != domain.ExitNotFound {
		t.Fatalf("second purge: %v", err)
	}
}

func n(v any) int {
	l, _ := v.([]any)
	return len(l)
}

// 正文里形如 blob 引用但哈希不合法的普通文本不能让材料删不掉；其后的合法引用照常处理。
func TestPurgeIgnoresMalformedBlobRefText(t *testing.T) {
	e := setup(t)
	h := e.putBlob(t, "real")
	bad := blobs.RefPrefix + strings.Repeat("G", 64)
	m := e.note(t, bad+" "+blobs.RefPrefix+strings.Repeat("A", 64)+" "+blobs.Ref(h))
	res := e.must(t, owner, Command, map[string]any{"object": m})
	if n(res["blobs_removed"]) != 1 || e.blobFile(h) {
		t.Fatalf("result %v", res)
	}
}
