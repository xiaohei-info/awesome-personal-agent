package summaries

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func digestStore(t *testing.T) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, runner.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seed 落三条事件：两条 digest、一条 ignored；返回按落盘顺序的事件 id。
func seedDigest(t *testing.T, st *store.Store) []string {
	t.Helper()
	var ids []string
	if err := st.Update(bg, func(tx *store.Tx) error {
		for _, n := range []string{"news-1", "news-2", "noise"} {
			ev := domain.SourceEvent{ID: n, Type: "news", Subject: "标题 " + n, Summary: "摘要 " + n, Link: "https://example.com/" + n, OccurredAt: time.Now()}
			if err := runner.AppendInternal(tx, domain.Source{ID: "src_news"}, ev); err != nil {
				return err
			}
		}
		evs, err := runner.Events(tx, "src_news")
		if err != nil {
			return err
		}
		for i, ev := range evs {
			d := domain.DispositionDigest
			if i == 2 {
				d = domain.DispositionIgnored
			}
			if err := runner.SetDisposition(tx, ev.ID, d, "", ""); err != nil {
				return err
			}
			ids = append(ids, ev.ID)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return ids
}

func pendingDigest(t *testing.T, st *store.Store) (es []DigestEntry) {
	t.Helper()
	if err := st.View(bg, func(tx *store.Tx) (err error) { es, err = PendingDigest(tx); return }); err != nil {
		t.Fatal(err)
	}
	return es
}

// ACT-DGA-15/16：简报取走 digest 事件（标题与链接），只取 digest；标记后下一份简报不再带，没标记的下次仍在。
func TestDigestEntriesGoIntoOneBriefOnly(t *testing.T) {
	st := digestStore(t)
	ids := seedDigest(t, st)

	es := pendingDigest(t, st)
	if len(es) != 2 || es[0].Event != ids[0] || es[0].Title != "标题 news-1" || es[0].Link != "https://example.com/news-1" || es[1].Event != ids[1] {
		t.Fatalf("first brief must list exactly the two digest events in order: %+v", es)
	}
	if err := st.Update(bg, func(tx *store.Tx) error { return MarkDigested(tx, "item_brief1", es[:1], time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if es = pendingDigest(t, st); len(es) != 1 || es[0].Event != ids[1] {
		t.Fatalf("an event already merged into a brief must not come back: %+v", es)
	}
	// 重复标记是幂等的：运行重试不报错。
	if err := st.Update(bg, func(tx *store.Tx) error { return MarkDigested(tx, "item_brief1", pendingDigestOf(t, tx), time.Now()) }); err != nil {
		t.Fatal(err)
	}
	if es = pendingDigest(t, st); len(es) != 0 {
		t.Fatalf("all merged: %+v", es)
	}
}

func pendingDigestOf(t *testing.T, tx *store.Tx) []DigestEntry {
	es, err := PendingDigest(tx)
	if err != nil {
		t.Fatal(err)
	}
	return es
}

// ACT-DGA-18：被转为建议的 digest 事件不再汇入后续简报；另一条仍在。
func TestProposedDigestEventLeavesTheNextBrief(t *testing.T) {
	st := digestStore(t)
	ids := seedDigest(t, st)
	if err := st.Update(bg, func(tx *store.Tx) error {
		return runner.Resolve(tx, ids[0], domain.DispositionProposed, "item_x")
	}); err != nil {
		t.Fatal(err)
	}
	if es := pendingDigest(t, st); len(es) != 1 || es[0].Event != ids[1] {
		t.Fatalf("only the still-digest event remains: %+v", es)
	}
}
