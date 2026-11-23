package retention

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/webhook"
)

var (
	bg = context.Background()
	t0 = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

type noPacks struct{}

func (noPacks) List() []*packs.Pack { return nil }

type noSecrets struct{}

func (noSecrets) Secrets(*store.Tx, string) ([][]byte, error) { return nil, nil }
func (noSecrets) Keep(*store.Tx, string, string) error        { return nil }

type env struct {
	t   *testing.T
	st  *store.Store
	lay config.Layout
	wh  *webhook.Service
	x   *command.Executor
	svc *Service
	now time.Time
}

func newEnv(t *testing.T, ingestKeep, runKeep time.Duration) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{proc.Module, webhook.Module, eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, materials.Module, registry.Module, runner.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, lay: config.LayoutOf(filepath.Join(t.TempDir(), "home")), now: t0}
	if err := e.lay.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := func() time.Time { return e.now }
	e.x = command.New(st, now)
	if e.wh, err = webhook.New(webhook.Options{Store: st, Exec: e.x, Packs: noPacks{}, Auth: auth.New(now),
		Secrets: noSecrets{}, Now: now, Retention: ingestKeep}); err != nil {
		t.Fatal(err)
	}
	if e.svc, err = New(Options{Store: st, Ingest: e.wh, Layout: e.lay, Exec: e.x, RunRetention: runKeep, IngestRetention: ingestKeep, Now: now}); err != nil {
		t.Fatal(err)
	}
	if err := Register(e.x, e.lay.Blobs, Defaults(runKeep, ingestKeep, 0)); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) addIngest(id string, at time.Time, state string) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO webhook_ingest (id, source_id, delivery_key, received_at, state, signed, gen, run_id, events, reason, headers, body, body_size, purged)
			VALUES (?, 's', ?, ?, ?, 1, 0, '', 1, '', '{"X-A":"b"}', ?, 7, 0)`, id, id, at.UnixNano(), state, []byte(`{"a":1}`))
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) ingest(id string) webhook.Ingest {
	e.t.Helper()
	var g webhook.Ingest
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { g, err = webhook.IngestByID(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return g
}

// addRun 建一条 Run 记录和它的运行目录；非终态不设结束时间。
func (e *env) addRun(id string, state domain.RunState, finished time.Time) string {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{ID: id, Category: domain.RunBackground, Target: "t-" + id, TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{"/bin/true"}}}, e.now); err != nil {
			return err
		}
		switch {
		case state.Terminal():
			if _, err := tx.Exec(`UPDATE outcall_run SET state = 'running' WHERE id = ?`, id); err != nil {
				return err
			}
			return proc.Settle(tx, id, state, nil, "", finished)
		case state == domain.RunRunning:
			_, err := tx.Exec(`UPDATE outcall_run SET state = 'running' WHERE id = ?`, id)
			return err
		}
		return nil
	})
	if err != nil {
		e.t.Fatal(err)
	}
	dir := e.lay.RunDir(id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		e.t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "out.txt"), []byte("x"), 0o600); err != nil {
		e.t.Fatal(err)
	}
	return dir
}

func exists(p string) bool { _, err := os.Stat(p); return err == nil }

// ACT-RET-01：过期请求体消失、元数据还在、未到期不受影响、此时 replay 明确报错。
func TestSweepExpiresIngestBodiesKeepingMetadata(t *testing.T) {
	e := newEnv(t, 30*24*time.Hour, 0)
	e.addIngest("old", t0, webhook.StateProcessed)
	e.addIngest("fresh", t0.Add(29*24*time.Hour), webhook.StateProcessed)
	e.now = t0.Add(31 * 24 * time.Hour)
	if _, err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if g := e.ingest("old"); !g.Purged || len(g.Body) != 0 || g.BodySize != 7 || g.State != webhook.StateProcessed || g.SourceID != "s" || g.Events != 1 {
		t.Fatalf("expired ingest = %+v", g)
	}
	if g := e.ingest("fresh"); g.Purged || string(g.Body) != `{"a":1}` {
		t.Fatalf("unexpired ingest touched: %+v", g)
	}
	if err := e.wh.Replay(bg, "old"); err == nil {
		t.Fatal("replaying an expired payload must fail")
	}
}

// ACT-RET-01：默认保留期 30 天来自配置默认值，调短后即刻生效。
func TestDefaultsAreThirtyDays(t *testing.T) {
	d := config.Default()
	if d.IngestRetention != 30*24*time.Hour || d.RunRetention != 30*24*time.Hour {
		t.Fatalf("defaults = %v / %v", d.IngestRetention, d.RunRetention)
	}
	e := newEnv(t, time.Minute, 0) // RunRetention 缺省走 30 天
	if e.svc.o.RunRetention != 30*24*time.Hour {
		t.Fatalf("run retention default = %v", e.svc.o.RunRetention)
	}
	e.addIngest("i", t0, webhook.StateProcessed)
	e.now = t0.Add(2 * time.Minute)
	if _, err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if !e.ingest("i").Purged {
		t.Fatal("1-minute retention must purge after 2 minutes")
	}
}

// ACT-RET-02：过期目录被清理，Run 记录还在，在途与未到期的目录不动。
func TestSweepRemovesExpiredRunDirsOnly(t *testing.T) {
	e := newEnv(t, time.Hour, 14*24*time.Hour)
	expired := e.addRun("run_old", domain.RunSucceeded, t0)
	failed := e.addRun("run_oldfail", domain.RunFailed, t0)
	fresh := e.addRun("run_fresh", domain.RunSucceeded, t0.Add(13*24*time.Hour))
	running := e.addRun("run_running", domain.RunRunning, time.Time{})
	queued := e.addRun("run_queued", domain.RunQueued, time.Time{})
	e.now = t0.Add(15 * 24 * time.Hour)
	if _, err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{expired, failed} {
		if exists(d) {
			t.Fatalf("%s should be removed", d)
		}
	}
	for _, d := range []string{fresh, running, queued} {
		if !exists(d) {
			t.Fatalf("%s must be kept", d)
		}
	}
	var n int
	if err := e.st.View(bg, func(tx *store.Tx) error { rs, err := proc.List(tx); n = len(rs); return err }); err != nil || n != 5 {
		t.Fatalf("run records = %d, err = %v", n, err)
	}
	if _, err := e.svc.Sweep(bg); err != nil { // 幂等
		t.Fatal(err)
	}
}

func TestSweepRefusesRunIDThatEscapesRunsDir(t *testing.T) {
	e := newEnv(t, time.Hour, time.Hour)
	victim := e.addRun("../keep", domain.RunSucceeded, t0) // 解析到 runs/ 之外的 <home>/keep
	e.now = t0.Add(2 * time.Hour)
	if _, err := e.svc.Sweep(bg); err == nil {
		t.Fatal("path-like run id must be reported")
	}
	if !exists(victim) {
		t.Fatal("file outside runs/ was deleted")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("missing dependencies must be rejected")
	}
}

func (e *env) putJudgment(id string, at time.Time) {
	e.t.Helper()
	p := 0.9
	err := e.st.Update(bg, func(tx *store.Tx) error {
		return record.Put(tx, record.Record{ID: id, Use: "inbound_screen", Template: "t@h", Subject: "evt_" + id, Fingerprint: "f", Band: record.Band{Low: .15, High: .6},
			DecidedBy: record.DecidedClassifier, Answer: "yes", CreatedAt: at,
			Levels: []record.Level{{Backend: record.BackendClassifier, PYes: &p, Outcome: record.OutcomeDecided}}})
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) judgments() (ids []string) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		rs, _, err := record.List(tx, record.Filter{})
		for _, r := range rs {
			ids = append(ids, r.ID)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return ids
}

// 判断记录默认保留 180 天；配置改小后按新值清理，Sweep 幂等。
func TestSweepExpiresJudgmentsByRetention(t *testing.T) {
	e := newEnv(t, 30*24*time.Hour, 14*24*time.Hour)
	e.putJudgment("old", t0.Add(-181*24*time.Hour))
	e.putJudgment("edge", t0.Add(-179*24*time.Hour))
	e.putJudgment("new", t0.Add(-time.Hour))
	if _, err := e.svc.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.judgments(); len(got) != 2 || got[0] != "edge" || got[1] != "new" {
		t.Fatalf("default retention (180d) kept %v", got)
	}
	short, err := New(Options{Store: e.st, Ingest: e.wh, Layout: e.lay, Exec: e.x, JudgmentRetention: 24 * time.Hour, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := short.Sweep(bg); err != nil {
			t.Fatal(err)
		}
		if got := e.judgments(); len(got) != 1 || got[0] != "new" {
			t.Fatalf("1-day retention kept %v", got)
		}
	}
}

func TestKeepJudgmentsDisablesExpiry(t *testing.T) {
	e := newEnv(t, 30*24*time.Hour, 14*24*time.Hour)
	e.putJudgment("ancient", t0.Add(-1000*24*time.Hour))
	keep, err := New(Options{Store: e.st, Ingest: e.wh, Layout: e.lay, Exec: e.x, KeepJudgments: true, Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := keep.Sweep(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.judgments(); len(got) != 1 {
		t.Fatalf("judgment_retention_days: 0 must keep everything, got %v", got)
	}
}

// 报数：删了多少目录、多少字节、多少请求体、多少判断记录，在途的列出来；连跑第二次全是 0。
func TestSweepReportsCountsAndIsIdempotent(t *testing.T) {
	e := newEnv(t, 30*24*time.Hour, 30*24*time.Hour)
	e.addRun("run_old", domain.RunSucceeded, t0)
	e.addRun("run_live", domain.RunRunning, time.Time{})
	e.addIngest("old", t0, webhook.StateProcessed)
	e.putJudgment("ancient", t0.Add(-200*24*time.Hour))
	e.now = t0.Add(40 * 24 * time.Hour)
	r, err := e.svc.Sweep(bg)
	if err != nil {
		t.Fatal(err)
	}
	if r.RunDirs != 1 || r.RunDirBytes != 1 || r.IngestBodies != 1 || r.Judgments != 1 || len(r.SkippedInFlight) != 1 || r.SkippedInFlight[0] != "run_live" {
		t.Fatalf("first sweep = %+v", r)
	}
	r, err = e.svc.Sweep(bg)
	if err != nil || r.RunDirs != 0 || r.RunDirBytes != 0 || r.IngestBodies != 0 || r.Judgments != 0 {
		t.Fatalf("second sweep must delete nothing: %+v, %v", r, err)
	}
	var last *Report
	var evs []eventlog.Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		if last, err = Last(tx); err != nil {
			return err
		}
		evs, err = eventlog.Query(tx, eventlog.Filter{Type: EvSwept}, 0, 10)
		return err
	}); err != nil || last == nil || last.At != e.now || len(evs) != 2 {
		t.Fatalf("last report = %+v, events = %d, err = %v", last, len(evs), err)
	}
}

// 设置里改成 7 天，不重启下一次 Sweep 就按 7 天判断；越界的值被拒绝且不落账。
func TestSetRetentionTakesEffectWithoutRestart(t *testing.T) {
	e := newEnv(t, 30*24*time.Hour, 30*24*time.Hour)
	dir := e.addRun("run_10d", domain.RunSucceeded, t0)
	e.now = t0.Add(10 * 24 * time.Hour)
	if _, err := e.svc.Sweep(bg); err != nil || !exists(dir) {
		t.Fatalf("30 days must keep a 10-day-old run dir: %v", err)
	}
	for _, bad := range []string{`{"run_days":-1}`, `{"run_days":3651}`, `{"judgment_days":7}`, `{}`, `{"run_days":7,"table":1}`} {
		if _, err := e.x.Do(bg, command.Request{Identity: web, Command: CmdSet, Input: []byte(bad)}); err == nil {
			t.Fatalf("%s must be rejected", bad)
		}
	}
	if _, err := e.x.Do(bg, command.Request{Identity: web, Command: CmdSet, Input: []byte(`{"run_days":7}`)}); err != nil {
		t.Fatal(err)
	}
	r, err := e.svc.Sweep(bg)
	if err != nil || r.RunDirs != 1 || exists(dir) {
		t.Fatalf("7 days must remove the 10-day-old run dir: %+v, %v", r, err)
	}
	var got Settings
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		got, err = Effective(tx, Defaults(30*24*time.Hour, 30*24*time.Hour, 180*24*time.Hour))
		return
	}); err != nil || got != (Settings{7, 30, 180}) {
		t.Fatalf("effective = %+v, %v", got, err)
	}
}

// 0 = 不清理这一项。
func TestZeroDaysDisablesThatItem(t *testing.T) {
	e := newEnv(t, 30*24*time.Hour, 30*24*time.Hour)
	dir := e.addRun("run_old", domain.RunSucceeded, t0)
	if _, err := e.x.Do(bg, command.Request{Identity: web, Command: CmdSet, Input: []byte(`{"run_days":0}`)}); err != nil {
		t.Fatal(err)
	}
	e.now = t0.Add(1000 * 24 * time.Hour)
	if r, err := e.svc.Sweep(bg); err != nil || r.RunDirs != 0 || !exists(dir) {
		t.Fatalf("run_days 0 must keep run dirs: %+v, %v", r, err)
	}
}

func TestNextIsTheFollowingTenPastFour(t *testing.T) {
	loc := time.UTC
	at := func(d, h, m int) time.Time { return time.Date(2026, 11, d, h, m, 0, 0, loc) }
	if got := Next(at(23, 3, 0), 4, 10); !got.Equal(at(23, 4, 10)) {
		t.Fatalf("03:00 → %v", got)
	}
	if got := Next(at(23, 4, 10), 4, 10); !got.Equal(at(24, 4, 10)) {
		t.Fatalf("04:10 exactly must wait for tomorrow: %v", got)
	}
}

// 启动时清一次；时钟走到 04:10 再清一次，不到点不清；ctx 结束即退出。
func TestRunSweepsAtStartAndAtScheduledTime(t *testing.T) {
	e := newEnv(t, 30*24*time.Hour, 30*24*time.Hour)
	e.now = time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)
	tick := make(chan struct{})
	reports := make(chan Report, 4)
	ctx, cancel := context.WithCancel(bg)
	done := make(chan struct{})
	go func() {
		e.svc.Run(ctx, 4, 10, func() <-chan struct{} { return tick }, func(r Report, err error) {
			if err != nil {
				t.Error(err)
			}
			reports <- r
		})
		close(done)
	}()
	<-reports // 启动一次
	tick <- struct{}{}
	select {
	case <-reports:
		t.Fatal("swept before 04:10")
	case <-time.After(50 * time.Millisecond):
	}
	e.now = time.Date(2026, 11, 23, 4, 10, 0, 0, time.UTC)
	tick <- struct{}{}
	if r := <-reports; !r.At.Equal(e.now) {
		t.Fatalf("scheduled sweep at %v", r.At)
	}
	cancel()
	<-done
}
