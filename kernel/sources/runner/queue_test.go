package runner

import (
	"fmt"
	"sync"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func queueStore(t *testing.T, n int) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(Module); err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Update(bg, func(tx *store.Tx) error {
		for i := range n {
			id := fmt.Sprintf("ev_%02d", i)
			if _, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
				occurred_at, correlation_keys, sender, sender_verified, ingested_at) VALUES ('d', ?, ?, 'src', 'r', 'mail.received', '', '', '', '', 1, '[]', '', 0, ?)`,
				id, id, int64(i+1)); err != nil {
				return err
			}
			level := LevelNormal
			if i == n-1 {
				level = LevelUrgent
			}
			if err := Enqueue(tx, id, level); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return st
}

// 并发取批：每条事件只被一批取到，立即的排最前。
func TestClaimTakesEachEventOnce(t *testing.T) {
	st := queueStore(t, 12)
	var mu sync.Mutex
	seen := map[string]string{}
	var wg sync.WaitGroup
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			run := fmt.Sprintf("run_%d", w)
			if err := st.Update(bg, func(tx *store.Tx) error {
				evs, err := Claim(tx, run, 5)
				mu.Lock()
				defer mu.Unlock()
				for _, e := range evs {
					if prev, dup := seen[e.ID]; dup {
						t.Errorf("%s claimed by %s and %s", e.ID, prev, run)
					}
					seen[e.ID] = run
				}
				return err
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if len(seen) != 12 {
		t.Fatalf("claimed %d of 12", len(seen))
	}
	st2 := queueStore(t, 3)
	_ = st2.Update(bg, func(tx *store.Tx) error {
		evs, err := Claim(tx, "run_x", 1)
		if err != nil || len(evs) != 1 || evs[0].ID != "ev_02" {
			t.Fatalf("urgent first: %+v %v", evs, err)
		}
		return nil
	})
}

// 失败放回待处理并计次；超过上限的离开队列转入积压；已有去向的直接算已处理。
func TestFinishRequeuesThenDrops(t *testing.T) {
	st := queueStore(t, 2)
	claimFail := func(run string) (f Finished) {
		if err := st.Update(bg, func(tx *store.Tx) error {
			if _, err := Claim(tx, run, 10); err != nil {
				return err
			}
			var err error
			f, err = Finish(tx, run, false, 1)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	// ev_00 在第一批里被 Agent 忽略：不再重排。
	if err := st.Update(bg, func(tx *store.Tx) error {
		if _, err := Claim(tx, "run_1", 10); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sources_runner_event SET disposition = 'ignored' WHERE id = 'ev_00'`); err != nil {
			return err
		}
		f, err := Finish(tx, "run_1", false, 1)
		if len(f.Done) != 1 || len(f.Requeued) != 1 || f.Requeued[0] != "ev_01" {
			t.Fatalf("first failure: %+v", f)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if f := claimFail("run_2"); len(f.Dropped) != 1 || f.Dropped[0] != "ev_01" {
		t.Fatalf("second failure should drop: %+v", f)
	}
	_ = st.View(bg, func(tx *store.Tx) error {
		ev, err := Event(tx, "ev_01")
		if err != nil || ev.Queue != "" || ev.Disposition != domain.DispositionUnjudged || ev.UnjudgedReason != domain.UnjudgedProcessFailed {
			t.Fatalf("dropped event: %+v %v", ev, err)
		}
		q, err := Queued(tx)
		if err != nil || q.Normal+q.Urgent != 0 {
			t.Fatalf("queue not empty: %+v %v", q, err)
		}
		return nil
	})
}
