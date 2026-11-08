package eventlog

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

func open(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(Module); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var human = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}

func ev(typ, obj string) domain.Event {
	return domain.Event{Type: typ, Object: obj, Actor: human, Payload: json.RawMessage(`{"k":1}`)}
}

func appendAll(t *testing.T, s *store.Store, evs ...domain.Event) []int64 {
	t.Helper()
	var seqs []int64
	err := s.Update(ctx, func(tx *store.Tx) error {
		for _, e := range evs {
			seq, err := Append(tx, e)
			if err != nil {
				return err
			}
			seqs = append(seqs, seq)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return seqs
}

func read(t *testing.T, s *store.Store, after int64, limit int) []Entry {
	t.Helper()
	var out []Entry
	err := s.View(ctx, func(tx *store.Tx) error {
		var err error
		out, err = Read(tx, after, limit)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestAppendAssignsMonotonicSeqAndRoundTripsAllFields(t *testing.T) {
	s := open(t)
	at := time.Date(2026, 1, 2, 3, 4, 5, 6, time.UTC)
	full := domain.Event{
		Type: domain.EvDecisionResolved, Object: "dec-1",
		Actor: domain.Actor{Type: domain.ActorAgent, Principal: "a1", Via: domain.ViaRule,
			Harness: "h", Session: "s", Run: "r", OnBehalfOf: "me"},
		Reason: "auto", Payload: json.RawMessage(`{"answer":"yes"}`), ChainDepth: 3,
		Basis: &domain.Basis{PolicyVersion: 7, RuleID: "rule-9"}, ResolvedVia: "grant", Evidence: "ev", Grant: "grt_1", At: at,
	}
	seqs := appendAll(t, s, ev(domain.EvItemCreated, "i1"), full, ev(domain.EvItemUpdated, "i1"))
	if seqs[0] != 1 || seqs[1] != 2 || seqs[2] != 3 {
		t.Fatalf("seqs = %v, want 1,2,3", seqs)
	}
	got := read(t, s, 0, 10)
	if len(got) != 3 {
		t.Fatalf("got %d events", len(got))
	}
	g := got[1].Event
	full.Seq = 2
	if g.Seq != 2 || g.Type != full.Type || g.Object != full.Object || g.Actor != full.Actor ||
		g.Reason != full.Reason || string(g.Payload) != string(full.Payload) || g.ChainDepth != 3 ||
		*g.Basis != *full.Basis || g.ResolvedVia != "grant" || g.Grant != "grt_1" || g.Evidence != "ev" || !g.At.Equal(at) {
		t.Fatalf("round trip mismatch: %+v", g)
	}
	if got[0].Event.Basis != nil || got[0].Tombstone {
		t.Fatalf("plain event should have nil basis, no tombstone: %+v", got[0])
	}
	if got[0].Event.At.IsZero() {
		t.Fatal("At should default to now")
	}
}

func TestAppendRollsBackWithCallerTransaction(t *testing.T) {
	s := open(t)
	boom := errors.New("boom")
	err := s.Update(ctx, func(tx *store.Tx) error {
		if _, err := Append(tx, ev(domain.EvItemCreated, "i1")); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n := len(read(t, s, 0, 10)); n != 0 {
		t.Fatalf("rolled-back event visible: %d", n)
	}
	// 回滚后序号不应留下空洞
	if seqs := appendAll(t, s, ev(domain.EvItemCreated, "i2")); seqs[0] != 1 {
		t.Fatalf("seq after rollback = %d, want 1", seqs[0])
	}
}

func TestAppendRejectsInvalidEvents(t *testing.T) {
	s := open(t)
	bad := map[string]func(*domain.Event){
		"no type":        func(e *domain.Event) { e.Type = "" },
		"no object":      func(e *domain.Event) { e.Object = "" },
		"bad actor type": func(e *domain.Event) { e.Actor.Type = "robot" },
		"no principal":   func(e *domain.Event) { e.Actor.Principal = "" },
		"bad via":        func(e *domain.Event) { e.Actor.Via = "fax" },
		"negative depth": func(e *domain.Event) { e.ChainDepth = -1 },
		"empty payload":  func(e *domain.Event) { e.Payload = nil },
		"invalid json":   func(e *domain.Event) { e.Payload = json.RawMessage(`{`) },
		"rule w/o basis": func(e *domain.Event) { e.Actor.Via = domain.ViaRule },
		"rule w/o id": func(e *domain.Event) {
			e.Actor.Via = domain.ViaRule
			e.Basis = &domain.Basis{PolicyVersion: 1}
		},
	}
	for name, mut := range bad {
		e := ev(domain.EvItemCreated, "i1")
		mut(&e)
		err := s.Update(ctx, func(tx *store.Tx) error { _, err := Append(tx, e); return err })
		if !errors.Is(err, ErrInvalidEvent) {
			t.Errorf("%s: err = %v, want ErrInvalidEvent", name, err)
		}
	}
	if n := len(read(t, s, 0, 10)); n != 0 {
		t.Fatalf("invalid events persisted: %d", n)
	}
}

func TestAppendRejectedInReadOnlyTx(t *testing.T) {
	s := open(t)
	err := s.View(ctx, func(tx *store.Tx) error { _, err := Append(tx, ev(domain.EvItemCreated, "i")); return err })
	if !errors.Is(err, store.ErrReadOnly) {
		t.Fatalf("err = %v", err)
	}
}

func TestReadByCursorIsGaplessAndResumable(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		appendAll(t, s, ev(domain.EvItemUpdated, "i1"))
	}
	p1 := read(t, s, 0, 2)
	p2 := read(t, s, p1[len(p1)-1].Event.Seq, 2)
	p3 := read(t, s, p2[len(p2)-1].Event.Seq, 2)
	var seqs []int64
	for _, p := range [][]Entry{p1, p2, p3} {
		for _, e := range p {
			seqs = append(seqs, e.Event.Seq)
		}
	}
	if len(seqs) != 5 {
		t.Fatalf("seqs = %v", seqs)
	}
	for i, q := range seqs {
		if q != int64(i+1) {
			t.Fatalf("seqs = %v, want 1..5", seqs)
		}
	}
	if n := len(read(t, s, 5, 10)); n != 0 {
		t.Fatalf("past head returned %d", n)
	}
	if n := len(read(t, s, 0, 0)); n != 0 {
		t.Fatalf("limit 0 returned %d", n)
	}
	var head int64
	s.View(ctx, func(tx *store.Tx) error { var err error; head, err = Head(tx); return err })
	if head != 5 {
		t.Fatalf("head = %d", head)
	}
}

func TestReadObjectReplaysOneObjectInOrder(t *testing.T) {
	s := open(t)
	appendAll(t, s, ev(domain.EvItemCreated, "a"), ev(domain.EvItemCreated, "b"),
		ev(domain.EvItemPhaseChanged, "a"), ev(domain.EvDecisionRequested, "a"))
	var got []Entry
	if err := s.View(ctx, func(tx *store.Tx) error {
		var err error
		got, err = ReadObject(tx, "a")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Event.Seq != 1 || got[1].Event.Seq != 3 || got[2].Event.Seq != 4 {
		t.Fatalf("got %+v", got)
	}
}

func TestSeqNeverReusedAfterTombstoneAndSurvivesReopen(t *testing.T) {
	home := t.TempDir()
	reg := store.NewRegistry()
	reg.Register(Module)
	s, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	appendAll(t, s, ev(domain.EvItemCreated, "i1"), ev(domain.EvItemCreated, "i2"))
	s.Close()
	s, err = store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if seq := appendAll(t, s, ev(domain.EvItemCreated, "i3"))[0]; seq != 3 {
		t.Fatalf("seq after reopen = %d, want 3", seq)
	}
}

func TestTombstoneErasesContentKeepsEnvelopeAndCursor(t *testing.T) {
	s := open(t)
	secret := "MARKER-PURGE"
	e := domain.Event{
		Type: domain.EvItemCreated, Object: "victim", Actor: human,
		Reason: secret, Payload: json.RawMessage(`{"body":"` + secret + `"}`), Evidence: secret,
		ChainDepth: 2, At: time.Unix(1700000000, 0).UTC(),
	}
	appendAll(t, s, ev(domain.EvItemCreated, "other"), e, ev(domain.EvItemUpdated, "other"))
	before := read(t, s, 0, 10)

	var n int64
	err := s.Update(ctx, func(tx *store.Tx) error {
		var err error
		n, err = Tombstone(tx, "victim")
		return err
	})
	if err != nil || n != 1 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	after := read(t, s, 0, 10)
	if len(after) != 3 {
		t.Fatalf("len = %d", len(after))
	}
	for i := range after {
		if after[i].Event.Seq != before[i].Event.Seq {
			t.Fatal("seq changed")
		}
	}
	// 未涉及对象的事件不变
	if after[0].Tombstone || string(after[0].Event.Payload) != `{"k":1}` {
		t.Fatalf("other event altered: %+v", after[0])
	}
	v := after[1]
	if !v.Tombstone || v.Event.Reason != "" || v.Event.Evidence != "" ||
		strings.Contains(string(v.Event.Payload), secret) || !json.Valid(v.Event.Payload) {
		t.Fatalf("content not erased: %+v", v)
	}
	if v.Event.Type != e.Type || v.Event.Object != "victim" || v.Event.Actor != human || !v.Event.At.Equal(e.At) {
		t.Fatalf("envelope lost: %+v", v.Event)
	}
	// 幂等，且删除后追加的序号继续递增
	s.Update(ctx, func(tx *store.Tx) error { n, err = Tombstone(tx, "victim"); return err })
	if err != nil || n != 0 {
		t.Fatalf("second tombstone n=%d err=%v", n, err)
	}
	if seq := appendAll(t, s, ev("purge.done", "victim"))[0]; seq != 4 {
		t.Fatalf("seq = %d, want 4", seq)
	}
	// 墓碑后事件日志文件里确实没有内容
	raw, _ := rawText(t, s)
	if strings.Contains(raw, secret) {
		t.Fatal("secret still stored in events table")
	}
}

func rawText(t *testing.T, s *store.Store) (string, error) {
	t.Helper()
	var sb strings.Builder
	err := s.View(ctx, func(tx *store.Tx) error {
		r, err := tx.Query("SELECT reason, payload, evidence, actor FROM eventlog_event")
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var a, b, c, d string
			if err := r.Scan(&a, &b, &c, &d); err != nil {
				return err
			}
			sb.WriteString(a + b + c + d)
		}
		return r.Err()
	})
	return sb.String(), err
}

func TestTombstoneRejectsEmptyObject(t *testing.T) {
	s := open(t)
	err := s.Update(ctx, func(tx *store.Tx) error { _, err := Tombstone(tx, ""); return err })
	if !errors.Is(err, ErrInvalidEvent) {
		t.Fatalf("err = %v", err)
	}
}

func TestQueryFiltersByActorObjectType(t *testing.T) {
	s := open(t)
	a := domain.Actor{Type: domain.ActorAgent, Principal: "bot", Via: domain.ViaCLI}
	h := domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}
	err := s.Update(context.Background(), func(tx *store.Tx) error {
		for _, e := range []domain.Event{
			{Type: "t1", Object: "o1", Actor: a, Payload: json.RawMessage(`null`)},
			{Type: "t2", Object: "o1", Actor: h, Payload: json.RawMessage(`null`)},
			{Type: "t1", Object: "o2", Actor: h, Payload: json.RawMessage(`null`)},
		} {
			if _, err := Append(tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	q := func(f Filter, after int64, limit int) []int64 {
		var seqs []int64
		_ = s.View(context.Background(), func(tx *store.Tx) error {
			es, err := Query(tx, f, after, limit)
			for _, e := range es {
				seqs = append(seqs, e.Event.Seq)
			}
			return err
		})
		return seqs
	}
	for _, c := range []struct {
		f     Filter
		after int64
		limit int
		want  int
	}{{Filter{}, 0, 10, 3}, {Filter{Actor: "me"}, 0, 10, 2}, {Filter{Object: "o1"}, 0, 10, 2},
		{Filter{Type: "t1"}, 0, 10, 2}, {Filter{Actor: "me", Type: "t1", Object: "o2"}, 0, 10, 1},
		{Filter{}, 1, 10, 2}, {Filter{}, 0, 1, 1}, {Filter{}, 0, 0, 0}, {Filter{Actor: "x"}, 0, 10, 0}} {
		if got := q(c.f, c.after, c.limit); len(got) != c.want {
			t.Errorf("%+v after=%d limit=%d: got %v want %d", c.f, c.after, c.limit, got, c.want)
		}
	}
}
