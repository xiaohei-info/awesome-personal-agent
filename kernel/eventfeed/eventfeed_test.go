package eventfeed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

var human = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}

func open(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(eventlog.Module); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ev(typ, obj, payload string) domain.Event {
	return domain.Event{Type: typ, Object: obj, Actor: human, Reason: "SECRET-REASON",
		Evidence: "SECRET-EVIDENCE", Payload: json.RawMessage(payload)}
}

func appendAll(t *testing.T, s *store.Store, evs ...domain.Event) {
	t.Helper()
	err := s.Update(ctx, func(tx *store.Tx) error {
		for _, e := range evs {
			if _, err := eventlog.Append(tx, e); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, s *store.Store, after int64, limit int) Page {
	t.Helper()
	var p Page
	err := s.View(ctx, func(tx *store.Tx) error {
		var err error
		p, err = Read(tx, after, limit)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// flow-event-subscribe：订阅方按游标读取登记类型的信封，处理成功后才推进游标；
// 未推进则重读得到同样的信封（至少一次）；信封不泄露正文、reason、证据。
func TestEventSubscribeFlow(t *testing.T) {
	s := open(t)
	appendAll(t, s,
		ev(domain.EvItemCreated, "item-1", `{"title":"写周报","body":"SECRET-BODY"}`),
		ev("internal.unregistered", "x", `{"title":"hidden"}`),
		ev(domain.EvItemPhaseChanged, "item-1", `{"to":"doing"}`),
	)

	p := read(t, s, 0, 10)
	if len(p.Envelopes) != 2 {
		t.Fatalf("want 2 registered envelopes, got %+v", p.Envelopes)
	}
	first := p.Envelopes[0]
	if first.Seq != 1 || first.Type != domain.EvItemCreated || first.Object != "item-1" ||
		first.Title != "写周报" || first.Actor != human || first.At.IsZero() {
		t.Fatalf("bad envelope %+v", first)
	}
	if p.Envelopes[1].Seq != 3 || p.Envelopes[1].Title != "" || p.Next != 3 {
		t.Fatalf("bad second envelope/next: %+v next=%d", p.Envelopes[1], p.Next)
	}
	raw, _ := json.Marshal(p)
	for _, leak := range []string{"SECRET-BODY", "SECRET-REASON", "SECRET-EVIDENCE", "payload", "reason", "evidence"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("envelope leaks %q: %s", leak, raw)
		}
	}

	// 订阅方没推进游标（处理失败）：重读得到相同结果。
	again := read(t, s, 0, 10)
	if len(again.Envelopes) != 2 || again.Envelopes[0] != first {
		t.Fatalf("re-read differs: %+v", again)
	}
	// 推进游标后，只看到新事件。
	appendAll(t, s, ev(domain.EvRunFinished, "run-1", `{}`))
	next := read(t, s, p.Next, 10)
	if len(next.Envelopes) != 1 || next.Envelopes[0].Seq != 4 || next.Next != 4 {
		t.Fatalf("after cursor: %+v", next)
	}
	// 已追平：无新事件，游标不动。
	if idle := read(t, s, 4, 10); len(idle.Envelopes) != 0 || idle.Next != 4 {
		t.Fatalf("idle: %+v", idle)
	}
}

// 整段都是未登记类型时，游标仍要越过它们，否则订阅方会卡死。
func TestCursorSkipsUnregisteredRuns(t *testing.T) {
	s := open(t)
	for i := 0; i < 5; i++ {
		appendAll(t, s, ev("internal.noise", "n", `{}`))
	}
	appendAll(t, s, ev(domain.EvSourceFailed, "src", `{}`))
	p := read(t, s, 0, 1)
	if len(p.Envelopes) != 1 || p.Envelopes[0].Seq != 6 || p.Next != 6 {
		t.Fatalf("%+v", p)
	}
}

func TestLimitStopsAtLastReturned(t *testing.T) {
	s := open(t)
	appendAll(t, s, ev(domain.EvItemCreated, "a", `{}`), ev(domain.EvItemCreated, "b", `{}`),
		ev(domain.EvItemCreated, "c", `{}`))
	p := read(t, s, 0, 2)
	if len(p.Envelopes) != 2 || p.Next != 2 {
		t.Fatalf("%+v", p)
	}
	if p := read(t, s, 0, 0); len(p.Envelopes) != 0 || p.Next != 0 {
		t.Fatalf("limit 0: %+v", p)
	}
}

// 物理删除墓碑化内容后信封仍在，序号不变，标题随内容抹除。
func TestTombstonedEventKeepsEnvelope(t *testing.T) {
	s := open(t)
	appendAll(t, s, ev(domain.EvItemCreated, "item-1", `{"title":"私密"}`))
	err := s.Update(ctx, func(tx *store.Tx) error {
		_, err := eventlog.Tombstone(tx, "item-1")
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	p := read(t, s, 0, 10)
	if len(p.Envelopes) != 1 || p.Envelopes[0].Seq != 1 || p.Envelopes[0].Title != "" {
		t.Fatalf("%+v", p)
	}
}
