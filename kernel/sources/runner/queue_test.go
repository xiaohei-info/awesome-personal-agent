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

// FIX-2：运行成功（ok=true）但遗漏了部分事件（仍为 pending）时，绝不能标为 done 成为永久孤儿；
// 应按未处理路径重排计次，达重试上限转 unjudged/process_failed，已处置的事件才转 done。
func TestFinishSuccessRequeuesUnhandledEvents(t *testing.T) {
	st := queueStore(t, 2)
	// 第 1 次成功退出：ev_00 已处置（ignored），ev_01 仍为 pending（模型遗漏）。
	if err := st.Update(bg, func(tx *store.Tx) error {
		if _, err := Claim(tx, "run_ok_1", 10); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE sources_runner_event SET disposition = 'ignored' WHERE id = 'ev_00'`); err != nil {
			return err
		}
		f, err := Finish(tx, "run_ok_1", true, 1)
		if err != nil {
			return err
		}
		if len(f.Done) != 1 || f.Done[0] != "ev_00" {
			t.Fatalf("expected only handled ev_00 in Done, got %+v", f)
		}
		if len(f.Requeued) != 1 || f.Requeued[0] != "ev_01" {
			t.Fatalf("expected unhandled pending ev_01 to be Requeued even on ok=true, got %+v", f)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 验证 ev_00 为 done，ev_01 回到 queued 仍可被下一批取到。
	_ = st.View(bg, func(tx *store.Tx) error {
		e0, err := Event(tx, "ev_00")
		if err != nil || e0.Queue != QueueDone {
			t.Fatalf("ev_00 should be done: %+v %v", e0, err)
		}
		e1, err := Event(tx, "ev_01")
		if err != nil || e1.Queue != QueueQueued || e1.Disposition != domain.DispositionPending {
			t.Fatalf("ev_01 should be requeued pending: %+v %v", e1, err)
		}
		return nil
	})
	// 第 2 次成功退出仍遗漏 ev_01：超过 maxTries=1，转 unjudged/process_failed 进积压。
	if err := st.Update(bg, func(tx *store.Tx) error {
		if _, err := Claim(tx, "run_ok_2", 10); err != nil {
			return err
		}
		f, err := Finish(tx, "run_ok_2", true, 1)
		if err != nil {
			return err
		}
		if len(f.Dropped) != 1 || f.Dropped[0] != "ev_01" {
			t.Fatalf("second unhandled success should drop ev_01 to backlog, got %+v", f)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = st.View(bg, func(tx *store.Tx) error {
		e1, err := Event(tx, "ev_01")
		if err != nil || e1.Queue != "" || e1.Disposition != domain.DispositionUnjudged || e1.UnjudgedReason != domain.UnjudgedProcessFailed {
			t.Fatalf("dropped ev_01 state mismatch: %+v %v", e1, err)
		}
		return nil
	})
}

// 运行中途的处置命令已把事件标为 done：收尾仍计入本次处置数。
func TestFinishCountsEventsDisposedDuringTheRun(t *testing.T) {
	st := queueStore(t, 2)
	if err := st.Update(bg, func(tx *store.Tx) error {
		if _, err := Claim(tx, "run_d", 10); err != nil {
			return err
		}
		if err := Resolve(tx, "ev_00", domain.DispositionIgnored, ""); err != nil {
			return err
		}
		f, err := Finish(tx, "run_d", true, 1)
		if err == nil && (len(f.Done) != 1 || f.Done[0] != "ev_00" || len(f.Requeued) != 1) {
			t.Fatalf("finished = %+v", f)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
