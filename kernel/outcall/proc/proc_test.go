package proc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

func openStore(t *testing.T, home string) *store.Store {
	t.Helper()
	reg := store.NewRegistry()
	if err := reg.Register(Module); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

type harness struct {
	t       *testing.T
	st      *store.Store
	d       *Dispatcher
	results chan Result
}

func newHarness(t *testing.T, long, short int, opt Options) *harness {
	t.Helper()
	h := &harness{t: t, st: openStore(t, t.TempDir()), results: make(chan Result, 64)}
	h.d = h.dispatcher(h.st, long, short, opt)
	return h
}

func (h *harness) dispatcher(st *store.Store, long, short int, opt Options) *Dispatcher {
	opt.OnResult = func(r Result) { h.results <- r }
	cfg := config.Default()
	cfg.MaxSubprocesses, cfg.MaxShortSubprocesses = long, short
	d := New(st, cfg, opt)
	h.t.Cleanup(d.Close)
	return d
}

func (h *harness) enqueue(cat domain.RunCategory, target string, explicit bool, script string, args ...string) domain.Run {
	h.t.Helper()
	var r domain.Run
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		var err error
		r, err = Enqueue(tx, Request{
			Run:      domain.Run{Category: cat, Target: target, TimeoutSec: 30},
			Spec:     Spec{Command: append([]string{"/bin/sh", "-c", script, "sh"}, args...)},
			Explicit: explicit,
		}, time.Now())
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func (h *harness) next() Result {
	h.t.Helper()
	select {
	case r := <-h.results:
		return r
	case <-time.After(20 * time.Second):
		h.t.Fatal("timed out waiting for a run result")
		return Result{}
	}
}

func (h *harness) get(id string) Record {
	h.t.Helper()
	var rec Record
	if err := h.st.View(ctx, func(tx *store.Tx) (err error) { rec, err = Get(tx, id); return }); err != nil {
		h.t.Fatal(err)
	}
	return rec
}

func (h *harness) states() map[string]domain.RunState {
	h.t.Helper()
	m := map[string]domain.RunState{}
	h.st.View(ctx, func(tx *store.Tx) error {
		recs, err := List(tx)
		for _, r := range recs {
			m[r.Run.ID] = r.Run.State
		}
		return err
	})
	return m
}

func (h *harness) dispatch() {
	h.t.Helper()
	if err := h.d.Dispatch(ctx); err != nil {
		h.t.Fatal(err)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// gated 的脚本：读完 stdin 后等 $1 文件出现才退出，用来把运行钉在 running。
const gated = `cat >/dev/null; echo started >> "$1.started"; while [ ! -e "$1" ]; do sleep 0.02; done; echo done-$2`

func release(t *testing.T, gate string) {
	t.Helper()
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
}

func pidAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	return !strings.HasPrefix(s[strings.LastIndexByte(s, ')')+2:], "Z") // 僵尸视为已死
}

// 子进程契约：命令 + stdin JSON → 退出码 + stdout。
func TestSubprocessContractStdinJSONToExitCodeAndStdout(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	var ok domain.Run
	h.st.Update(ctx, func(tx *store.Tx) (err error) {
		ok, err = Enqueue(tx, Request{Run: domain.Run{Category: domain.RunSource, Target: "src1", TimeoutSec: 30},
			Spec: Spec{Command: []string{"/bin/sh", "-c", "cat; echo; echo out-line"}, Stdin: json.RawMessage(`{"cursor":"c1"}`)}}, time.Now())
		return
	})
	_ = ok
	h.dispatch()
	r := h.next()
	if r.Run.State != domain.RunSucceeded || r.Run.ExitCode == nil || *r.Run.ExitCode != 0 {
		t.Fatalf("%+v", r.Run)
	}
	if !strings.Contains(string(r.Stdout), `{"cursor":"c1"}`) || !strings.Contains(string(r.Stdout), "out-line") {
		t.Fatalf("stdout %q", r.Stdout)
	}
	if r.Run.StartedAt == nil || r.Run.FinishedAt == nil {
		t.Fatal("stats not recorded")
	}
	bad := h.enqueue(domain.RunTransform, "t1", false, `echo oops >&2; exit 7`)
	h.dispatch()
	r = h.next()
	if r.Run.ID != bad.ID || r.Run.State != domain.RunFailed || *r.Run.ExitCode != 7 || !strings.Contains(string(r.Stderr), "oops") {
		t.Fatalf("%+v %q", r.Run, r.Stderr)
	}
	// 最近成功时间
	h.st.View(ctx, func(tx *store.Tx) error {
		if _, found, err := LastSuccess(tx, domain.RunSource, "src1"); err != nil || !found {
			t.Fatalf("last success: %v %v", found, err)
		}
		if _, found, _ := LastSuccess(tx, domain.RunTransform, "t1"); found {
			t.Fatal("failed run counted as success")
		}
		return nil
	})
}

func TestStartFailureIsFailedRunWithReason(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	var r domain.Run
	h.st.Update(ctx, func(tx *store.Tx) (err error) {
		r, err = Enqueue(tx, Request{Run: domain.Run{Category: domain.RunReader, TimeoutSec: 5},
			Spec: Spec{Command: []string{"/nonexistent/binary"}}}, time.Now())
		return
	})
	h.dispatch()
	res := h.next()
	if res.Run.ID != r.ID || res.Run.State != domain.RunFailed || res.Run.ExitCode != nil || !strings.HasPrefix(res.Reason, "start:") {
		t.Fatalf("%+v %q", res.Run, res.Reason)
	}
}

// ACT-OUT-01：长时运行超限时排队，不丢、不超额，依次执行，健康统计可见排队数与等待。
func TestLongRunsBeyondQuotaQueueAndRunInOrder(t *testing.T) {
	h := newHarness(t, 1, 2, Options{})
	dir := t.TempDir()
	var runs []domain.Run
	for i, tgt := range []string{"item-a", "item-b", "item-c"} {
		runs = append(runs, h.enqueue(domain.RunTask, tgt, false, gated, filepath.Join(dir, "g"+strconv.Itoa(i)), tgt))
	}
	h.dispatch()
	waitFor(t, "first run started", func() bool { return h.get(runs[0].ID).Run.State == domain.RunRunning })

	st := h.states()
	if st[runs[0].ID] != domain.RunRunning || st[runs[1].ID] != domain.RunQueued || st[runs[2].ID] != domain.RunQueued {
		t.Fatalf("want 1 running 2 queued: %v", st)
	}
	h.st.View(ctx, func(tx *store.Tx) error {
		s, err := Stats(tx, time.Now().Add(time.Minute))
		if err != nil {
			t.Fatal(err)
		}
		if l := s[QueueLong]; l.Running != 1 || l.Queued != 2 || l.LongestWait < time.Minute {
			t.Fatalf("long stat %+v", l)
		}
		return nil
	})
	// 额度没有被别的方式绕过：再次调度也不会起第二个。
	h.dispatch()
	time.Sleep(100 * time.Millisecond)
	if b, _ := os.ReadFile(filepath.Join(dir, "g1.started")); len(b) != 0 {
		t.Fatal("queued run started beyond the quota")
	}

	for i, run := range runs {
		release(t, filepath.Join(dir, "g"+strconv.Itoa(i)))
		res := h.next()
		if res.Run.ID != run.ID || res.Run.State != domain.RunSucceeded {
			t.Fatalf("step %d: got %s %s, want %s succeeded", i, res.Run.ID, res.Run.State, run.ID)
		}
		if !strings.Contains(string(res.Stdout), "done-"+run.Target) {
			t.Fatalf("stdout %q", res.Stdout)
		}
	}
	for id, s := range h.states() {
		if s != domain.RunSucceeded {
			t.Fatalf("run %s ended %s: a run was lost", id, s)
		}
	}
}

// 长时与短任务各有额度，互不挤占（ACT-OUT-02 的内核侧）；本人显式触发的在短队列内先于定时拉取（ACT-OUT-03）。
func TestQueuesAreIndependentAndExplicitRunsGoFirst(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	dir := t.TempDir()
	long := h.enqueue(domain.RunTask, "item-a", false, gated, filepath.Join(dir, "long"), "x")
	s1 := h.enqueue(domain.RunSource, "s1", false, gated, filepath.Join(dir, "s1"), "s1")
	s2 := h.enqueue(domain.RunSource, "s2", false, gated, filepath.Join(dir, "s2"), "s2")
	h.dispatch()
	waitFor(t, "both queues running", func() bool {
		st := h.states()
		return st[long.ID] == domain.RunRunning && st[s1.ID] == domain.RunRunning
	})
	// 本人在 s2 之后才触发重放，仍排在 s2 之前。
	replay := h.enqueue(domain.RunReader, "replay", true, gated, filepath.Join(dir, "replay"), "replay")
	h.dispatch()
	if st := h.states(); st[s2.ID] != domain.RunQueued || st[replay.ID] != domain.RunQueued {
		t.Fatalf("short queue exceeded quota: %v", st)
	}
	release(t, filepath.Join(dir, "s1"))
	if r := h.next(); r.Run.ID != s1.ID {
		t.Fatalf("got %s", r.Run.ID)
	}
	waitFor(t, "explicit replay to start", func() bool { return h.get(replay.ID).Run.State == domain.RunRunning })
	if h.get(s2.ID).Run.State != domain.RunQueued {
		t.Fatal("scheduled pull ran before the explicit replay")
	}
	release(t, filepath.Join(dir, "replay"))
	release(t, filepath.Join(dir, "s2"))
	release(t, filepath.Join(dir, "long"))
	for i := 0; i < 3; i++ {
		h.next()
	}
}

func TestSameTargetInFlightIsRejectedAndQueuedCountsAsInFlight(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	dir := t.TempDir()
	h.enqueue(domain.RunTask, "item-a", false, gated, filepath.Join(dir, "a"), "a")
	// 不 dispatch：queued 也算在途。
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		_, err := Enqueue(tx, Request{Run: domain.Run{Category: domain.RunBackground, Target: "item-a", TimeoutSec: 5},
			Spec: Spec{Command: []string{"/bin/true"}}}, time.Now())
		return err
	})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeConflict {
		t.Fatalf("want conflict, got %v", err)
	}
	h.enqueue(domain.RunSource, "src-1", false, `true`)
	err = h.st.Update(ctx, func(tx *store.Tx) error {
		_, err := Enqueue(tx, Request{Run: domain.Run{Category: domain.RunSource, Target: "src-1", TimeoutSec: 5},
			Spec: Spec{Command: []string{"/bin/true"}}}, time.Now())
		return err
	})
	if !errors.As(err, &de) || de.Code != domain.CodeConflict {
		t.Fatalf("source pull must not double-queue, got %v", err)
	}
}

func TestEnqueueRollsBackWithCallerTransaction(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	boom := errors.New("boom")
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		if _, err := Enqueue(tx, Request{Run: domain.Run{ID: "r1", Category: domain.RunTask, Target: "i", TimeoutSec: 5},
			Spec: Spec{Command: []string{"/bin/true"}}}, time.Now()); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if len(h.states()) != 0 {
		t.Fatal("run survived the caller's rollback")
	}
}

func TestEnqueueValidation(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	cases := map[string]Request{
		"http category":  {Run: domain.Run{Category: domain.RunJudge, TimeoutSec: 5}, Spec: Spec{Command: []string{"x"}}},
		"no command":     {Run: domain.Run{Category: domain.RunTask, TimeoutSec: 5}},
		"no timeout":     {Run: domain.Run{Category: domain.RunTask}, Spec: Spec{Command: []string{"x"}}},
		"bad stdin":      {Run: domain.Run{Category: domain.RunTask, TimeoutSec: 5}, Spec: Spec{Command: []string{"x"}, Stdin: json.RawMessage("{")}},
		"negative depth": {Run: domain.Run{Category: domain.RunTask, TimeoutSec: 5, ChainDepth: -1}, Spec: Spec{Command: []string{"x"}}},
	}
	for name, req := range cases {
		err := h.st.Update(ctx, func(tx *store.Tx) error { _, err := Enqueue(tx, req, time.Now()); return err })
		var de *domain.Error
		if !errors.As(err, &de) || de.Code != domain.CodeInvalidRequest {
			t.Errorf("%s: want invalid_request, got %v", name, err)
		}
	}
	if len(h.states()) != 0 {
		t.Fatal("invalid request left a run behind")
	}
}

// ACT-OUT-05：子进程不继承内核环境，只拿白名单变量加注入变量。
func TestSubprocessEnvironmentIsWhitelisted(t *testing.T) {
	h := newHarness(t, 1, 1, Options{
		Environ: func() []string {
			return []string{"ACC_SECRET=leak", "MODEL_API_KEY=k", "PATH=" + os.Getenv("PATH"), "HOME=/home/x", "XDG_CONFIG_HOME=/xdg/c", "XDG_DATA_HOME=/xdg/d"}
		},
		Env: func(r domain.Run) map[string]string {
			return map[string]string{"APA_URL": "http://127.0.0.1:1", "APA_TOKEN": "tok-" + r.ID, "APA_PROJECT": "proj-" + r.Target}
		},
	})
	run := h.enqueue(domain.RunTask, "proj", false, `env`)
	h.dispatch()
	res := h.next()
	got := map[string]string{}
	for _, l := range strings.Split(string(res.Stdout), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			got[k] = v
		}
	}
	for _, banned := range []string{"ACC_SECRET", "MODEL_API_KEY"} {
		if _, leaked := got[banned]; leaked {
			t.Fatalf("%s leaked into the subprocess", banned)
		}
	}
	for k, want := range map[string]string{"HOME": "/home/x", "XDG_CONFIG_HOME": "/xdg/c", "XDG_DATA_HOME": "/xdg/d",
		"APA_URL": "http://127.0.0.1:1", "APA_TOKEN": "tok-" + run.ID, "APA_RUN": run.ID, "APA_PROJECT": "proj-proj"} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	if got["PATH"] == "" {
		t.Error("PATH missing")
	}
	// 运行凭据不落账本。
	h.st.View(ctx, func(tx *store.Tx) error {
		rows, _ := tx.Query("SELECT spec FROM outcall_run")
		defer rows.Close()
		for rows.Next() {
			var s string
			rows.Scan(&s)
			if strings.Contains(s, "tok-") {
				t.Error("credential persisted in ledger")
			}
		}
		return nil
	})
}

// ACT-OUT-06：超时杀整个进程组（含孙进程），Run 记为 timeout。
func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	pidfile := filepath.Join(t.TempDir(), "child.pid")
	var run domain.Run
	h.st.Update(ctx, func(tx *store.Tx) (err error) {
		run, err = Enqueue(tx, Request{Run: domain.Run{Category: domain.RunTask, Target: "item", TimeoutSec: 1},
			Spec: Spec{Command: []string{"/bin/sh", "-c", `sleep 300 & echo $! > "$1"; wait`, "sh", pidfile}}}, time.Now())
		return
	})
	start := time.Now()
	h.dispatch()
	res := h.next()
	if res.Run.ID != run.ID || res.Run.State != domain.RunTimeout {
		t.Fatalf("%+v", res.Run)
	}
	if el := time.Since(start); el < time.Second || el > 10*time.Second {
		t.Fatalf("timed out after %v", el)
	}
	b, err := os.ReadFile(pidfile)
	if err != nil {
		t.Fatal(err)
	}
	child, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	waitFor(t, "grandchild to die", func() bool { return !pidAlive(child) })
	if rec := h.get(run.ID); rec.Run.State != domain.RunTimeout || rec.Reason == "" || rec.PGID != 0 {
		t.Fatalf("%+v", rec)
	}
}

// ACT-OUT-07：重启后无存活进程的 running 按失败处理，queued 重新执行，存活进程继续占额度。
func TestRestartRecoveryFailsDeadRunningAndReexecutesQueued(t *testing.T) {
	home := t.TempDir()
	st1 := openStore(t, home)
	h1 := &harness{t: t, st: st1, results: make(chan Result, 16)}
	h1.d = h1.dispatcher(st1, 1, 1, Options{})
	dir := t.TempDir()
	dead := h1.enqueue(domain.RunTask, "item-dead", false, gated, filepath.Join(dir, "x"), "x")
	queued := h1.enqueue(domain.RunTask, "item-queued", false, `cat >/dev/null; echo reran`)
	// 内核崩溃的现场：第一条已是 running，进程组已不存在（被 kill -9）。
	corpse := exec.Command("/bin/sh", "-c", "exit 0")
	corpse.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := corpse.Run(); err != nil {
		t.Fatal(err)
	}
	if _, ok, err := h1.d.claim(ctx, QueueLong); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	st1.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE outcall_run SET pgid = ? WHERE id = ?", int64(corpse.Process.Pid), dead.ID)
		return err
	})
	h1.d.cancel()
	st1.Close()

	st2 := openStore(t, home)
	h2 := &harness{t: t, st: st2, results: make(chan Result, 16)}
	h2.d = h2.dispatcher(st2, 1, 1, Options{})
	failed, alive, err := h2.d.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ID != dead.ID || failed[0].State != domain.RunFailed || len(alive) != 0 {
		t.Fatalf("failed=%+v alive=%+v", failed, alive)
	}
	if h2.get(dead.ID).Reason == "" {
		t.Fatal("recovery reason missing")
	}
	h2.dispatch()
	res := h2.next()
	if res.Run.ID != queued.ID || res.Run.State != domain.RunSucceeded || !strings.Contains(string(res.Stdout), "reran") {
		t.Fatalf("%+v %q", res.Run, res.Stdout)
	}
}

func TestRecoverKeepsLiveProcessRunningAndCountingAgainstQuota(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	live := exec.Command("/bin/sleep", "60")
	live.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := live.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { syscall.Kill(-live.Process.Pid, syscall.SIGKILL); live.Wait() })
	orphan := h.enqueue(domain.RunTask, "item-live", false, `true`)
	next := h.enqueue(domain.RunTask, "item-next", false, `true`)
	if _, ok, _ := h.d.claim(ctx, QueueLong); !ok {
		t.Fatal("claim")
	}
	h.st.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE outcall_run SET pgid = ? WHERE id = ?", int64(live.Process.Pid), orphan.ID)
		return err
	})
	failed, alive, err := h.d.Recover(ctx)
	if err != nil || len(failed) != 0 || len(alive) != 1 || alive[0].ID != orphan.ID {
		t.Fatalf("%v %v %v", failed, alive, err)
	}
	h.dispatch()
	time.Sleep(100 * time.Millisecond)
	if h.get(next.ID).Run.State != domain.RunQueued {
		t.Fatal("orphan did not hold its quota slot")
	}
}

func TestSettleRejectsIllegalTransition(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	r := h.enqueue(domain.RunTask, "i", false, `true`)
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		return Settle(tx, r.ID, domain.RunSucceeded, nil, "", time.Now()) // queued 不能直接成功
	})
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeConflict {
		t.Fatalf("%v", err)
	}
	err = h.st.Update(ctx, func(tx *store.Tx) error { return Settle(tx, "nope", domain.RunFailed, nil, "", time.Now()) })
	if !errors.As(err, &de) || de.Code != domain.CodeNotFound {
		t.Fatalf("%v", err)
	}
}

func TestCloseKillsRunningAndLeavesQueued(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	dir := t.TempDir()
	a := h.enqueue(domain.RunTask, "a", false, gated, filepath.Join(dir, "a"), "a")
	b := h.enqueue(domain.RunTask, "b", false, gated, filepath.Join(dir, "b"), "b")
	h.dispatch()
	waitFor(t, "a running", func() bool { return h.get(a.ID).Run.State == domain.RunRunning })
	h.d.Close()
	if r := h.get(a.ID); r.Run.State != domain.RunFailed || r.Reason != "shutdown" {
		t.Fatalf("%+v", r)
	}
	if h.get(b.ID).Run.State != domain.RunQueued {
		t.Fatal("queued run must survive shutdown for re-execution")
	}
}

func TestOutputKeepsOnlyTail(t *testing.T) {
	h := newHarness(t, 1, 1, Options{})
	h.enqueue(domain.RunReader, "r", false, `head -c 200000 /dev/zero | tr '\0' a; echo END`)
	h.dispatch()
	res := h.next()
	if len(res.Stdout) != tailLimit || !strings.HasSuffix(string(res.Stdout), "END\n") {
		t.Fatalf("len=%d", len(res.Stdout))
	}
}

// 来源与转换脚本的 stdout 是事件流：超过末段大小也全部交回。
func TestEventStreamKeepsWholeOutput(t *testing.T) {
	h := newHarness(t, 1, 2, Options{})
	script := `echo FIRST; head -c 200000 /dev/zero | tr '\0' a; echo; echo END`
	h.enqueue(domain.RunSource, "s", false, script)
	h.enqueue(domain.RunTransform, "t", false, script)
	h.dispatch()
	for range 2 {
		res := h.next()
		if res.Overflow || !strings.HasPrefix(string(res.Stdout), "FIRST\n") || !strings.HasSuffix(string(res.Stdout), "END\n") {
			t.Fatalf("%s overflow=%v len=%d", res.Run.Category, res.Overflow, len(res.Stdout))
		}
	}
}

// 超过总上限：只交回上限内的完整行并标明 overflow；后面的输出照常读走，子进程能正常退出。
func TestEventStreamOverLimitKeepsCompleteLinesAndDrains(t *testing.T) {
	h := &harness{t: t, st: openStore(t, t.TempDir()), results: make(chan Result, 64)}
	cfg := config.Default()
	cfg.MaxSourceOutput = 100
	h.d = New(h.st, cfg, Options{OnResult: func(r Result) { h.results <- r }})
	t.Cleanup(h.d.Close)
	h.enqueue(domain.RunSource, "s", false, `i=0; while [ $i -lt 20 ]; do echo "line-$i-xxxxxxxxxx"; i=$((i+1)); done; head -c 200000 /dev/zero | tr '\0' a`)
	h.dispatch()
	res := h.next()
	got := string(res.Stdout)
	if !res.Overflow || res.Run.State != domain.RunSucceeded || len(got) > 100 || !strings.HasPrefix(got, "line-0-") || !strings.HasSuffix(got, "\n") {
		t.Fatalf("overflow=%v state=%s stdout=%q", res.Overflow, res.Run.State, got)
	}
}

// 外部运行（唤醒，§13.6）：不需要命令，写入即 running，不占额度、调度器不拉起，重启恢复不判失败；
// 到运行超时由 Dispatch 按超时落账并交回调用模块，只交一次。
func TestExternalRunIsNeverStartedAndTimesOutOnDispatch(t *testing.T) {
	now := time.Now()
	h := newHarness(t, 1, 1, Options{Now: func() time.Time { return now }})
	var ext domain.Run
	if err := h.st.Update(ctx, func(tx *store.Tx) (err error) {
		ext, err = Enqueue(tx, Request{Run: domain.Run{Category: domain.RunBackground, Launch: "wake", Target: "item_w", TimeoutSec: 60}, External: true}, now)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if rec := h.get(ext.ID); rec.Run.State != domain.RunRunning || rec.Queue != QueueExternal || rec.Run.StartedAt == nil {
		t.Fatalf("external run: %+v", rec)
	}
	if err := h.st.Update(ctx, func(tx *store.Tx) error {
		_, err := Enqueue(tx, Request{Run: domain.Run{Category: domain.RunBackground, Target: "item_w", TimeoutSec: 60}, External: true}, now)
		if de := (*domain.Error)(nil); !errors.As(err, &de) || de.Code != domain.CodeConflict {
			t.Fatalf("same target in flight: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.enqueue(domain.RunTask, "item_t", false, "exit 0") // 唯一的长时额度仍可用
	if err := h.d.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if r := h.next(); r.Run.Target != "item_t" || r.Run.State != domain.RunSucceeded {
		t.Fatalf("long run: %+v", r.Run)
	}
	if failed, _, err := h.d.Recover(ctx); err != nil || len(failed) != 0 {
		t.Fatalf("recover must leave external runs alone: %v %+v", err, failed)
	}

	now = now.Add(61 * time.Second)
	if err := h.d.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	if r := h.next(); r.Run.ID != ext.ID || r.Run.State != domain.RunTimeout || r.Reason == "" {
		t.Fatalf("timeout result: %+v", r)
	}
	if err := h.d.Dispatch(ctx); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-h.results:
		t.Fatalf("settled twice: %+v", r)
	default:
	}
}

// enqueueSpec 写一条带规格的 agent 运行。
func (h *harness) enqueueSpec(spec Spec) domain.Run {
	h.t.Helper()
	var r domain.Run
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		var err error
		r, err = Enqueue(tx, Request{Run: domain.Run{Category: domain.RunTask, Target: "it", TimeoutSec: 30}, Spec: spec}, time.Now())
		return err
	})
	if err != nil {
		h.t.Fatal(err)
	}
	return r
}

func agentEnv(domain.Run) map[string]string {
	return map[string]string{"APA_URL": "http://k", "APA_TOKEN": "secret", "APA_PROJECT": "p1"}
}

// 运行上下文文件（§7.7）：只有本次运行能看到路径，0600，内容来自入队时定下的目录，运行结束即删；
// 指定文件名时用它（续接同一会话沿用原路径）。
func TestRunContextFileLivesAsLongAsTheRun(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	h := newHarness(t, 1, 1, Options{ContextDir: dir, Env: agentEnv})
	script := `stat -f %Lp "$APA_RUN_CONTEXT" > "$1/mode" 2>/dev/null || stat -c %a "$APA_RUN_CONTEXT" > "$1/mode"; printf '%s' "$APA_RUN_CONTEXT" > "$1/path"; cat "$APA_RUN_CONTEXT" > "$1/body"`
	run := func(name string) (domain.Run, string) {
		r := h.enqueueSpec(Spec{Command: []string{"/bin/sh", "-c", script, "sh", out}, Dir: out, RunDir: "/runs/x", ContextName: name})
		h.dispatch()
		if res := h.next(); res.Run.State != domain.RunSucceeded {
			t.Fatalf("%+v", res)
		}
		b, _ := os.ReadFile(filepath.Join(out, "path"))
		return r, string(b)
	}

	r, path := run("")
	if want := filepath.Join(dir, r.ID); path != want {
		t.Fatalf("path = %q, want %q", path, want)
	}
	if mode, _ := os.ReadFile(filepath.Join(out, "mode")); strings.TrimSpace(string(mode)) != "600" {
		t.Fatalf("mode = %q", mode)
	}
	var got runContext
	body, _ := os.ReadFile(filepath.Join(out, "body"))
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	want := runContext{Run: r.ID, URL: "http://k", Token: "secret", Project: "p1", RunDir: "/runs/x", WorkDir: out}
	if got != want {
		t.Fatalf("context = %+v, want %+v", got, want)
	}
	if left, _ := os.ReadDir(dir); len(left) != 0 {
		t.Fatalf("left behind: %v", left)
	}

	if r, path = run("run_first"); path != filepath.Join(dir, "run_first") {
		t.Fatalf("named context path = %q", path)
	}
	body, _ = os.ReadFile(filepath.Join(out, "body"))
	if err := json.Unmarshal(body, &got); err != nil || got.Run != r.ID {
		t.Fatalf("named context holds the current run: %+v %v", got, err)
	}
}

// 不是 agent 运行、没签出凭据、没配置目录：都不写文件，也不给路径。
func TestRunContextOnlyForAgentRunsWithACredential(t *testing.T) {
	check := func(name string, opt Options, runDir string) {
		t.Helper()
		out := t.TempDir()
		h := newHarness(t, 1, 1, opt)
		h.enqueueSpec(Spec{Command: []string{"/bin/sh", "-c", `printf '%s' "$APA_RUN_CONTEXT" > "$1/path"`, "sh", out}, Dir: out, RunDir: runDir})
		h.dispatch()
		if res := h.next(); res.Run.State != domain.RunSucceeded {
			t.Fatalf("%s: %+v", name, res)
		}
		if b, _ := os.ReadFile(filepath.Join(out, "path")); len(b) != 0 {
			t.Fatalf("%s: got a context path %q", name, b)
		}
		if opt.ContextDir != "" {
			if left, _ := os.ReadDir(opt.ContextDir); len(left) != 0 {
				t.Fatalf("%s: wrote %v", name, left)
			}
		}
	}
	check("not an agent run", Options{ContextDir: t.TempDir(), Env: agentEnv}, "")
	check("no credential issued", Options{ContextDir: t.TempDir()}, "/runs/x")
	check("no directory configured", Options{Env: agentEnv}, "/runs/x")
}

// 写不出上下文文件时不拉起：工具侧取不到身份，起了也做不成事，原因要落在 Run 上。
func TestRunContextWriteFailureFailsTheRun(t *testing.T) {
	out := t.TempDir()
	h := newHarness(t, 1, 1, Options{ContextDir: filepath.Join(out, "missing"), Env: agentEnv})
	h.enqueueSpec(Spec{Command: []string{"/bin/sh", "-c", `touch "$1/ran"`, "sh", out}, Dir: out, RunDir: "/runs/x"})
	h.dispatch()
	res := h.next()
	if res.Run.State != domain.RunFailed || !strings.Contains(res.Reason, "run context") {
		t.Fatalf("%+v", res)
	}
	if _, err := os.Stat(filepath.Join(out, "ran")); !os.IsNotExist(err) {
		t.Fatal("the process started anyway")
	}
}

// 重启时进程已不在的运行：按失败落账时一并清掉它留下的文件（里面的凭据已随之失效）。
func TestRecoverRemovesTheContextFileOfADeadRun(t *testing.T) {
	dir, out := t.TempDir(), t.TempDir()
	h := newHarness(t, 1, 1, Options{ContextDir: dir, Env: agentEnv})
	r := h.enqueueSpec(Spec{Command: []string{"/bin/sh", "-c", "exit 0"}, Dir: out, RunDir: "/runs/x"})
	if err := h.st.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE outcall_run SET state = 'running', started_at = 1 WHERE id = ?", r.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	left := filepath.Join(dir, r.ID)
	if err := os.WriteFile(left, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if failed, _, err := h.d.Recover(ctx); err != nil || len(failed) != 1 {
		t.Fatalf("failed=%v err=%v", failed, err)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("context file survived recovery: %v", err)
	}
}
