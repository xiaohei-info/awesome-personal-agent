package runner

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	t0    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
)

type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	clk  *clock.Clock
	r    *Runner
	disp *proc.Dispatcher
	dir  string
	done chan proc.Result
	errs chan error

	mu  sync.Mutex
	now time.Time
}

func (e *env) Now() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

func setup(t *testing.T, cfg config.Config) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, proc.Module, registry.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, dir: t.TempDir(), now: t0, done: make(chan proc.Result, 32), errs: make(chan error, 32)}
	e.x = command.New(st, e.Now)
	lay := config.LayoutOf(filepath.Join(e.dir, "home"))
	if err := lay.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{projects.Register(e.x), items.Register(e.x, items.NewKinds()),
		registry.Register(e.x, registry.Options{Layout: lay})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	e.r, err = New(Options{Store: st, Exec: e.x, Layout: lay, Now: e.Now,
		OnError: func(err error) { e.errs <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.r.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.disp = proc.New(st, cfg, proc.Options{Now: e.Now, OnResult: func(res proc.Result) {
		if err := e.r.Handle(bg, res); err != nil {
			e.errs <- err
		}
		e.done <- res
	}})
	t.Cleanup(e.disp.Close)
	e.r.Attach(e.disp)
	e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error { return e.r.HandleTimer(tx, c) }, e.Now)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func defaults() config.Config { return config.Config{MaxSubprocesses: 2, MaxShortSubprocesses: 2} }

// script 写一个可执行的来源脚本；$1 前后的 stdin 存到 <name>.stdin.<n>。
func (e *env) script(name, body string) string {
	e.t.Helper()
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

type m = map[string]any

func (e *env) register(id, cmd string, extra m) {
	e.t.Helper()
	in := m{"id": id, "type": "script", "source_type": "acc", "command": cmd, "schedule": "1m", "timeout": 30}
	for k, v := range extra {
		in[k] = v
	}
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: registry.CmdRegisterExec, Input: js(in)}); err != nil {
		e.t.Fatal(err)
	}
}

// tick 触发一次时钟并等 n 个来源 Run 结束（含结果入账）。
func (e *env) tick(n int) []proc.Result {
	e.t.Helper()
	if err := e.r.Tick(bg, e.clk); err != nil {
		e.t.Fatal(err)
	}
	return e.wait(n)
}

func (e *env) wait(n int) []proc.Result {
	e.t.Helper()
	var out []proc.Result
	for i := 0; i < n; i++ {
		select {
		case r := <-e.done:
			out = append(out, r)
		case err := <-e.errs:
			e.t.Fatalf("runner error: %v", err)
		case <-time.After(20 * time.Second):
			e.t.Fatalf("timed out waiting for run %d/%d", i+1, n)
		}
	}
	select {
	case err := <-e.errs:
		e.t.Fatalf("runner error: %v", err)
	default:
	}
	return out
}

func (e *env) source(id string) (s domain.Source, st Stats) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { s, err = registry.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	st, err := StatsOf(s)
	if err != nil {
		e.t.Fatal(err)
	}
	return s, st
}

func (e *env) events(id string) (out []domain.SourceEvent) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = Events(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) rejects(id string) (out []Reject) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = Rejects(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

const goodEvents = `echo '{"id":"e1","type":"issue.opened","subject":"s1","occurred_at":"2026-03-01T00:00:00Z","sender":"alice","sender_verified":true,"correlation_keys":["repo#1"]}'
echo '{"id":"e2","type":"issue.opened","subject":"s2","occurred_at":"2026-03-01T00:00:01Z"}'
echo '{"cursor":"c1"}'
`

// ACT-SRC-02：两条标准事件落盘、游标推进、stdin 含约定字段、来源页统计更新。
func TestNormalPullAdvancesCursorAndPassesContract(t *testing.T) {
	e := setup(t, defaults())
	stdin := filepath.Join(e.dir, "stdin")
	e.register("src_a", e.script("a.sh", `cat > `+stdin+`.$$; cat `+stdin+`.$$ > `+stdin+`.last
`+goodEvents), m{"config": m{"repo": "x/y"}})
	if err := e.r.Sync(bg); err != nil {
		t.Fatal(err)
	}
	e.tick(1)

	s, st := e.source("src_a")
	if s.Cursor != "c1" || st.ConsecutiveFailures != 0 || st.LastSuccessAt.IsZero() {
		t.Fatalf("cursor/stats after success: %q %+v", s.Cursor, st)
	}
	evs := e.events("src_a")
	if len(evs) != 2 || evs[0].DedupeKey != (domain.DedupeKey{Domain: "src_a", EventID: "e1"}) ||
		!evs[0].TrustedSender() || evs[0].Disposition != domain.DispositionPending || evs[0].CorrelationKeys[0] != "repo#1" {
		t.Fatalf("events: %+v", evs)
	}

	e.advance(time.Minute)
	e.tick(1)
	b, err := os.ReadFile(stdin + ".last")
	if err != nil {
		t.Fatal(err)
	}
	var in struct {
		SourceID string          `json:"source_id"`
		Config   json.RawMessage `json:"config"`
		Cursor   string          `json:"cursor"`
		RunID    string          `json:"run_id"`
		Window   struct{ From, To string }
	}
	if err := json.Unmarshal(b, &in); err != nil {
		t.Fatalf("stdin %s: %v", b, err)
	}
	if in.SourceID != "src_a" || in.Cursor != "c1" || !strings.HasPrefix(in.RunID, "run_") || string(in.Config) != `{"repo":"x/y"}` ||
		in.Window.From != t0.Format(time.RFC3339) || in.Window.To != t0.Add(time.Minute).Format(time.RFC3339) {
		t.Fatalf("stdin contract: %s", b)
	}
}

// ACT-SRC-03：同一事件 id 重复、两个来源共用去重域，都只留一条。
func TestDedupeByDomainAndEventID(t *testing.T) {
	e := setup(t, defaults())
	sh := e.script("a.sh", goodEvents)
	e.register("src_a", sh, m{"dedupe_domain": "shared"})
	e.register("src_b", sh, m{"dedupe_domain": "shared"})
	if err := e.r.Sync(bg); err != nil {
		t.Fatal(err)
	}
	e.tick(2)
	e.advance(time.Minute)
	e.tick(2)
	total := e.count(`SELECT COUNT(*) FROM sources_runner_event`)
	if total != 2 {
		t.Fatalf("want 2 events (e1,e2 once in shared domain), got %d", total)
	}
	if len(e.events("src_a"))+len(e.events("src_b")) != 2 {
		t.Fatal("source attribution lost")
	}
}

// ACT-SRC-04：缺必填字段的事件不入账并留原因；正常事件照常入账，游标仍推进。
func TestInvalidEventRejectedWithReasonOthersKept(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", `echo '{"id":"bad","type":"t","occurred_at":"2026-03-01T00:00:00Z"}'
echo 'not json'
echo '{"id":"ok","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'
echo '{"cursor":"c9"}'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	evs := e.events("src_a")
	if len(evs) != 1 || evs[0].DedupeKey.EventID != "ok" {
		t.Fatalf("events: %+v", evs)
	}
	rj := e.rejects("src_a")
	if len(rj) != 2 || !strings.Contains(rj[0].Reason, "subject") || rj[0].Line != 1 || rj[1].Line != 2 {
		t.Fatalf("rejects: %+v", rj)
	}
	if s, st := e.source("src_a"); s.Cursor != "c9" || st.ConsecutiveFailures != 0 {
		t.Fatalf("bad events must not fail the run: %q %+v", s.Cursor, st)
	}
}

// ACT-SRC-05：末行不是合法游标时，事件留下、游标不动、按临时失败计；重复输出不产生重复。
func TestBadCursorLineKeepsEventsHoldsCursor(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", `echo '{"id":"e1","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'
echo 'oops'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	s, st := e.source("src_a")
	if s.Cursor != "" || st.ConsecutiveFailures != 1 || len(e.events("src_a")) != 1 {
		t.Fatalf("cursor=%q stats=%+v events=%d", s.Cursor, st, len(e.events("src_a")))
	}
	e.advance(time.Minute)
	e.tick(1)
	if n := len(e.events("src_a")); n != 1 {
		t.Fatalf("repeat output duplicated: %d", n)
	}
}

// ACT-SRC-06：退出码 1 退避重试：游标不动、失败数可见、不停用、不打扰；连续失败时跳过中间节拍。
func TestExit1BacksOffAndKeepsSourceEnabled(t *testing.T) {
	e := setup(t, defaults())
	count := filepath.Join(e.dir, "count")
	e.register("src_a", e.script("a.sh", `echo x >> `+count+`
exit 1
`), nil)
	e.r.Sync(bg)
	runs := func() int { b, _ := os.ReadFile(count); return strings.Count(string(b), "x") }

	e.tick(1) // 第 1 次失败：下一节拍照常重试
	e.advance(time.Minute)
	e.tick(1) // 第 2 次失败：之后跳过一个节拍
	e.advance(time.Minute)
	if err := e.r.Tick(bg, e.clk); err != nil { // 退避中：不应拉起
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if runs() != 2 {
		t.Fatalf("backoff did not skip a beat: %d runs", runs())
	}
	e.advance(time.Minute)
	e.tick(1)
	s, st := e.source("src_a")
	if runs() != 3 || st.ConsecutiveFailures != 3 || s.Cursor != "" || !s.Enabled {
		t.Fatalf("runs=%d cursor=%q enabled=%v stats=%+v", runs(), s.Cursor, s.Enabled, st)
	}
	if n := e.itemCount(); n != 0 {
		t.Fatalf("exit 1 must not bother the owner, got %d items", n)
	}
	if n := e.count(`SELECT COUNT(*) FROM eventlog_event WHERE type = ?`, domain.EvHealthAlert); n != 0 {
		t.Fatalf("exit 1 must not raise health alerts, got %d", n)
	}
}

func (e *env) count(sql string, args ...any) (n int64) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(sql, args...)
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Scan(&n)
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) itemCount() int64 { return e.count(`SELECT COUNT(*) FROM items`) }

// ACT-SRC-07：退出码 2 生成一个 needs_input 事项；重复失败不重复建；其他来源不受影响。
func TestExit2CreatesOneNeedsInputItemOthersUnaffected(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_bad", e.script("bad.sh", `echo 'token expired' >&2; exit 2`), nil)
	e.register("src_ok", e.script("ok.sh", goodEvents), nil)
	e.r.Sync(bg)
	e.tick(2)
	e.advance(time.Minute)
	e.tick(2)

	if n := e.itemCount(); n != 1 {
		t.Fatalf("want exactly one needs_input item, got %d", n)
	}
	s, st := e.source("src_bad")
	if s.Cursor != "" || st.NeedsInputItem == "" || st.ConsecutiveFailures != 2 || !s.Enabled {
		t.Fatalf("bad source: %q %+v", s.Cursor, st)
	}
	var it domain.Item
	e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, st.NeedsInputItem); return })
	if it.Phase != domain.PhaseNeedsInput || it.Origin == nil || it.Origin.Source != "src_bad" || !strings.Contains(it.Ask.Question, "src_bad") {
		t.Fatalf("item: %+v", it)
	}
	if s, st := e.source("src_ok"); s.Cursor != "c1" || st.ConsecutiveFailures != 0 {
		t.Fatalf("healthy source affected: %q %+v", s.Cursor, st)
	}
}

// ACT-SRC-08：退出码 3 停用来源、记 health.alert、事件流有 source.failed，不再被调度。
func TestExit3DisablesEmitsHealthDeliveryAndEvent(t *testing.T) {
	e := setup(t, defaults())
	count := filepath.Join(e.dir, "count")
	e.register("src_a", e.script("a.sh", `echo x >> `+count+`; exit 3`), nil)
	e.r.Sync(bg)
	e.tick(1)

	s, st := e.source("src_a")
	if s.Enabled || st.ConsecutiveFailures != 1 {
		t.Fatalf("enabled=%v stats=%+v", s.Enabled, st)
	}
	// 健康类投递由 kernel/health 据 health.alert 统一产出，这里只验证告警事件。
	if n := e.count(`SELECT COUNT(*) FROM eventlog_event WHERE type = ? AND object = ?`, domain.EvHealthAlert, "src_a"); n != 1 {
		t.Fatalf("health.alert events: %d", n)
	}
	failed := e.count(`SELECT COUNT(*) FROM eventlog_event WHERE type = ?`, domain.EvSourceFailed)
	if failed != 1 {
		t.Fatalf("source.failed events: %d", failed)
	}
	e.advance(5 * time.Minute)
	if err := e.r.Tick(bg, e.clk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 1 {
		t.Fatalf("disabled source kept being scheduled: %q", b)
	}
}

// ACT-SRC-09：超时杀进程，计超时统计，游标不动，下个节拍仍重试。
func TestTimeoutIsTransientFailure(t *testing.T) {
	e := setup(t, defaults())
	count := filepath.Join(e.dir, "count")
	e.register("src_a", e.script("a.sh", `echo x >> `+count+`; sleep 30`), m{"timeout": 1})
	e.r.Sync(bg)
	res := e.tick(1)
	if res[0].Run.State != domain.RunTimeout {
		t.Fatalf("run state %s", res[0].Run.State)
	}
	s, st := e.source("src_a")
	if st.Timeouts != 1 || st.ConsecutiveFailures != 1 || s.Cursor != "" || !s.Enabled {
		t.Fatalf("%q %+v", s.Cursor, st)
	}
	e.advance(time.Minute)
	e.tick(1)
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 2 {
		t.Fatalf("not retried: %q", b)
	}
}

// ACT-OUT-04：同一来源已有排队或运行中的 Run 时，到期不重复入队；结束后恢复。
func TestNoDuplicateRunWhileSourceInFlight(t *testing.T) {
	e := setup(t, defaults())
	release := filepath.Join(e.dir, "release")
	count := filepath.Join(e.dir, "count")
	e.register("src_a", e.script("a.sh", `echo x >> `+count+`
while [ ! -f `+release+` ]; do sleep 0.05; done
echo '{"cursor":"c1"}'
`), m{"timeout": 60})
	e.r.Sync(bg)
	for i := 0; i < 4; i++ { // 每分钟到期一次，而一次拉取耗时远超一分钟
		if err := e.r.Tick(bg, e.clk); err != nil {
			t.Fatal(err)
		}
		e.advance(time.Minute)
		time.Sleep(50 * time.Millisecond)
	}
	runs := e.count(`SELECT COUNT(*) FROM outcall_run`)
	if runs != 1 {
		t.Fatalf("source piled up %d runs", runs)
	}
	os.WriteFile(release, nil, 0o600)
	e.wait(1)
	if s, _ := e.source("src_a"); s.Cursor != "c1" {
		t.Fatalf("cursor %q", s.Cursor)
	}
	e.tick(1) // 结束后下一次到期照常入队
}

// ACT-OUT-02：长时队列占满时，来源拉取在短任务队列照常执行。
func TestSourcePullNotBlockedByFullLongQueue(t *testing.T) {
	e := setup(t, config.Config{MaxSubprocesses: 1, MaxShortSubprocesses: 1})
	hang := e.script("hang.sh", `sleep 60`)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunTask, Target: "itm_1", TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{hang}}}, e.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.disp.Dispatch(bg); err != nil {
		t.Fatal(err)
	}
	e.register("src_a", e.script("a.sh", goodEvents), nil)
	e.r.Sync(bg)
	e.tick(1)
	if s, _ := e.source("src_a"); s.Cursor != "c1" {
		t.Fatalf("source blocked by long-running task: cursor %q", s.Cursor)
	}
}

// ACT-OUT-03：短任务额度为 1 时三个慢来源不并行；本人显式触发的短任务排在定时拉取之前。
func TestShortQuotaSerialisesSourcesAndExplicitRunsFirst(t *testing.T) {
	e := setup(t, config.Config{MaxSubprocesses: 1, MaxShortSubprocesses: 1})
	order := filepath.Join(e.dir, "order")
	overlap := filepath.Join(e.dir, "overlap")
	lock := filepath.Join(e.dir, "lock")
	body := func(name string) string {
		return `if [ -e ` + lock + ` ]; then echo overlap >> ` + overlap + `; fi
touch ` + lock + `
echo ` + name + ` >> ` + order + `
sleep 0.4
rm -f ` + lock + `
echo '{"cursor":"c"}'
`
	}
	for _, n := range []string{"src_a", "src_b", "src_c"} {
		e.register(n, e.script(n+".sh", body(n)), nil)
	}
	e.r.Sync(bg)
	if err := e.r.Tick(bg, e.clk); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond) // 第一个已在运行，另两个在排队
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := proc.Enqueue(tx, proc.Request{Explicit: true, Run: domain.Run{Category: domain.RunTransform, Target: "replay_1", TimeoutSec: 30},
			Spec: proc.Spec{Command: []string{e.script("replay.sh", body("replay"))}}}, e.Now())
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.wait(4)
	b, _ := os.ReadFile(order)
	lines := strings.Fields(string(b))
	if len(lines) != 4 || lines[1] != "replay" {
		t.Fatalf("explicit run must go before queued pulls: %v", lines)
	}
	if _, err := os.Stat(overlap); err == nil {
		t.Fatal("short runs overlapped despite quota 1")
	}
}

// 启停：停用的来源不再被调度；重新启用后恢复。
func TestDisableStopsSchedulingEnableResumes(t *testing.T) {
	e := setup(t, defaults())
	count := filepath.Join(e.dir, "count")
	e.register("src_a", e.script("a.sh", `echo x >> `+count+`
echo '{"cursor":"c"}'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	set := func(on bool) {
		if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: registry.CmdSetEnabled, Input: js(m{"id": "src_a", "enabled": on})}); err != nil {
			t.Fatal(err)
		}
	}
	set(false)
	e.advance(3 * time.Minute)
	e.r.Tick(bg, e.clk)
	time.Sleep(200 * time.Millisecond)
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 1 {
		t.Fatalf("disabled source ran: %q", b)
	}
	set(true)
	e.tick(1)
	if b, _ := os.ReadFile(count); strings.Count(string(b), "x") != 2 {
		t.Fatalf("re-enabled source did not run: %q", b)
	}
}

const bigEvents = `i=0
while [ $i -lt 700 ]; do
  echo "{\"id\":\"e$i\",\"type\":\"t\",\"subject\":\"s\",\"detail\":\"` + "xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx" + `\",\"occurred_at\":\"2026-03-01T00:00:00Z\"}"
  i=$((i+1))
done
`

// 来源输出超过 64KB（约 110KB）时开头的事件也要落盘，游标照常推进。
func TestOutputBeyondOldTailLimitPersistsEveryEvent(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", bigEvents+`echo '{"cursor":"big"}'
`), nil)
	e.r.Sync(bg)
	res := e.tick(1)
	s, st := e.source("src_a")
	if len(res[0].Stdout) <= 64<<10 || s.Cursor != "big" || st.ConsecutiveFailures != 0 || len(e.events("src_a")) != 700 {
		t.Fatalf("stdout=%d cursor=%q stats=%+v events=%d", len(res[0].Stdout), s.Cursor, st, len(e.events("src_a")))
	}
}

// 超过总上限：上限内已解析的事件照常落盘，游标不动，按临时失败计并写原因；下次重试不产生重复。
func TestOutputOverLimitKeepsEventsHoldsCursor(t *testing.T) {
	cfg := defaults()
	cfg.MaxSourceOutput = 32 << 10
	e := setup(t, cfg)
	e.register("src_a", e.script("a.sh", bigEvents+`echo '{"cursor":"big"}'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	s, st := e.source("src_a")
	n := len(e.events("src_a"))
	if s.Cursor != "" || st.ConsecutiveFailures != 1 || !strings.Contains(st.LastError, "max_source_output_mb") || n == 0 || n >= 700 {
		t.Fatalf("cursor=%q stats=%+v events=%d", s.Cursor, st, n)
	}
	if rj := e.rejects("src_a"); len(rj) != 0 { // 上限切在行中间的半行不算坏行
		t.Fatalf("rejects: %+v", rj)
	}
	e.advance(time.Minute)
	e.tick(1)
	if got := len(e.events("src_a")); got != n {
		t.Fatalf("retry duplicated events: %d -> %d", n, got)
	}
}

// 来源登记的 credentials_path 经 stdin 交给来源进程；未登记时不出现这个字段。
func TestCredentialsPathPassedOnStdin(t *testing.T) {
	e := setup(t, defaults())
	cred := filepath.Join(e.dir, "home", "sources", "src_a", "gh")
	e.register("src_a", e.script("a.sh", `cat > `+filepath.Join(e.dir, "a.stdin")+`
`+goodEvents), m{"credentials_path": cred})
	e.register("src_b", e.script("b.sh", `cat > `+filepath.Join(e.dir, "b.stdin")+`
`+goodEvents), nil)
	e.r.Sync(bg)
	e.tick(2)
	for name, want := range map[string]string{"a.stdin": cred, "b.stdin": ""} {
		b, err := os.ReadFile(filepath.Join(e.dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var in map[string]any
		if err := json.Unmarshal(b, &in); err != nil {
			t.Fatal(err)
		}
		got, present := in["credentials_path"]
		if (want != "" && got != want) || (want == "" && present) {
			t.Fatalf("%s: %s", name, b)
		}
	}
}

func TestParseCapsRejectsPerRun(t *testing.T) {
	p := parse([]byte(strings.Repeat("not json\n", maxRejects+5) + `{"cursor":"c"}`))
	if len(p.rejects) != maxRejects || !p.cursorOK {
		t.Fatalf("rejects=%d cursorOK=%v", len(p.rejects), p.cursorOK)
	}
}

// 非来源类别的 Run 不归本模块处置；启动恢复判失败（无退出码）的来源 Run 按临时失败计。
func TestHandleIgnoresOtherCategoriesAndTreatsMissingExitAsTransient(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", goodEvents), nil)
	if err := e.r.Handle(bg, proc.Result{Run: domain.Run{ID: "run_x", Category: domain.RunTask, Target: "src_a", State: domain.RunFailed}}); err != nil {
		t.Fatal(err)
	}
	if _, st := e.source("src_a"); st.Runs != 0 {
		t.Fatal("task run was handled as a source run")
	}
	if err := e.r.Handle(bg, proc.Result{Run: domain.Run{ID: "run_y", Category: domain.RunSource, Target: "src_a", State: domain.RunFailed},
		Reason: "no live process after restart"}); err != nil {
		t.Fatal(err)
	}
	s, st := e.source("src_a")
	if st.ConsecutiveFailures != 1 || st.LastError != "no live process after restart" || s.Cursor != "" {
		t.Fatalf("%q %+v", s.Cursor, st)
	}
}

// 同一个 Run 的结果被重复交付（重启后重放）只生效一次。
func TestHandleIsIdempotentPerRun(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", goodEvents), nil)
	res := proc.Result{Run: domain.Run{ID: "run_z", Category: domain.RunSource, Target: "src_a", State: domain.RunFailed,
		ExitCode: ptr(1)}}
	for i := 0; i < 2; i++ {
		if err := e.r.Handle(bg, res); err != nil {
			t.Fatal(err)
		}
	}
	if _, st := e.source("src_a"); st.ConsecutiveFailures != 1 {
		t.Fatalf("replayed result counted twice: %+v", st)
	}
}

func ptr[T any](v T) *T { return &v }

// ACT-IRL-20（来源侧）：标准事件的 metrics、merge_key、items[] 随事件落盘并可读回；items 列里的事项 id 与判断记录 id 一并读出。
func TestV2EventFieldsPersistAndReadBack(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", `echo '{"id":"k1","type":"acc.proposed","subject":"带新字段","occurred_at":"2026-03-01T00:00:00Z","metrics":{"size":3},"merge_key":"mk-k1","items":[{"sender":"bob","text":"hi","at":"2026-03-01T00:00:00Z"}]}'
echo '{"cursor":"c1"}'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	evs := e.events("src_a")
	if len(evs) != 1 || evs[0].Metrics["size"] != 3 || evs[0].MergeKey != "mk-k1" || len(evs[0].Messages) != 1 || evs[0].Messages[0].Text != "hi" {
		t.Fatalf("v2 fields lost: %+v", evs)
	}
	if n := len(e.rejects("src_a")); n != 0 {
		t.Fatalf("valid v2 event rejected: %d", n)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE sources_runner_event SET items = ?, judgment = 'jdg_1'`, `["itm_1","itm_2"]`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := e.events("src_a")[0]; len(got.Items) != 2 || got.Items[1] != "itm_2" || got.Judgment != "jdg_1" {
		t.Fatalf("items/judgment: %+v", got)
	}
}

// ACT-IRL-21：只含 v0.1 字段的旧来源程序照常入账、不被拒收，也没有 v2 字段。
func TestV01EventStillAcceptedWithoutNewFields(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", `echo '{"id":"old1","type":"acc.proposed","subject":"旧格式","occurred_at":"2026-03-01T00:00:00Z","correlation_keys":["old-1"],"sender":"alice"}'
echo '{"cursor":"c1"}'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	evs := e.events("src_a")
	if len(evs) != 1 || evs[0].Metrics != nil || evs[0].MergeKey != "" || evs[0].Items != nil || evs[0].Messages != nil || evs[0].Disposition != domain.DispositionPending {
		t.Fatalf("old-format event: %+v", evs)
	}
	if n := len(e.rejects("src_a")); n != 0 {
		t.Fatalf("old-format event rejected: %d", n)
	}
}

func TestInvalidV2FieldsAreRejectedWithReason(t *testing.T) {
	base := `{"id":"x","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z",`
	many := strings.Repeat(`{"sender":"a","text":"t","at":"2026-03-01T00:00:00Z"},`, maxItems) + `{"sender":"a","text":"t","at":"2026-03-01T00:00:00Z"}`
	for name, tc := range map[string]struct{ line, want string }{
		"metric not a number":  {base + `"metrics":{"size":"big"}}`, "bad field"},
		"metric name too long": {base + `"metrics":{"` + strings.Repeat("n", 65) + `":1}}`, "metrics name"},
		"metric name empty":    {base + `"metrics":{"":1}}`, "metrics name"},
		"items over 200":       {base + `"items":[` + many + `]}`, "at most 200"},
		"item without text":    {base + `"items":[{"sender":"a","text":" ","at":"2026-03-01T00:00:00Z"}]}`, "items[0].text"},
	} {
		p := parse([]byte(tc.line + "\n"))
		if len(p.events) != 0 || len(p.rejects) != 1 || !strings.Contains(p.rejects[0].Reason, tc.want) {
			t.Errorf("%s: events=%d rejects=%+v", name, len(p.events), p.rejects)
		}
	}
	ok := parse([]byte(base + `"metrics":{"` + strings.Repeat("n", 64) + `":1.5}}` + "\n"))
	if len(ok.events) != 1 {
		t.Fatalf("64-char metric name must pass: %+v", ok.rejects)
	}
}

// 新处置 digest、alerted 与新的未判定原因 outbound_unset、restored 可写入并读回。
func TestNewDispositionsAndUnjudgedReasons(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", `for i in 1 2 3 4; do echo '{"id":"e'$i'","type":"t","subject":"s","occurred_at":"2026-03-01T00:00:00Z"}'; done
echo '{"cursor":"c1"}'
`), nil)
	e.r.Sync(bg)
	e.tick(1)
	evs := e.events("src_a")
	if len(evs) != 4 {
		t.Fatalf("events: %d", len(evs))
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for i, s := range []struct {
			d      domain.Disposition
			reason string
		}{{domain.DispositionDigest, ""}, {domain.DispositionAlerted, ""},
			{domain.DispositionUnjudged, domain.UnjudgedOutboundUnset}, {domain.DispositionUnjudged, domain.UnjudgedRestored}} {
			if err := SetDisposition(tx, evs[i].ID, s.d, "", s.reason); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got := e.events("src_a")
	want := []struct {
		d      domain.Disposition
		reason string
	}{{domain.DispositionDigest, ""}, {domain.DispositionAlerted, ""},
		{domain.DispositionUnjudged, "outbound_unset"}, {domain.DispositionUnjudged, "restored"}}
	for i, w := range want {
		if got[i].Disposition != w.d || got[i].UnjudgedReason != w.reason {
			t.Errorf("event %d: %s/%q, want %s/%q", i, got[i].Disposition, got[i].UnjudgedReason, w.d, w.reason)
		}
	}
	// restored 与 outbound_unset 不参与后台自动重判（restored 只经积压放开，outbound_unset 等外发范围选定）。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, id := range []string{got[2].ID, got[3].ID} {
			if _, err := Requeue(tx, id); err == nil {
				t.Errorf("%s must not be auto-requeued", id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 列里的 JSON 被写坏时读取暴露真实错误，不返回残缺事件；不可设置的处置被拒。
func TestCorruptV2ColumnSurfacesErrorAndBadDispositionRejected(t *testing.T) {
	e := setup(t, defaults())
	e.register("src_a", e.script("a.sh", goodEvents), nil)
	e.r.Sync(bg)
	e.tick(1)
	id := e.events("src_a")[0].ID
	err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := SetDisposition(tx, id, domain.DispositionExpired, "", ""); err == nil {
			t.Error("expired must not be settable via SetDisposition")
		}
		_, err := tx.Exec(`UPDATE sources_runner_event SET metrics = 'not json' WHERE id = ?`, id)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error { _, err := Events(tx, "src_a"); return err }); err == nil || !strings.Contains(err.Error(), "metrics") {
		t.Fatalf("corrupt metrics must surface: %v", err)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error { _, _, err := EventPage(tx, "src_a", nil, 0, 10); return err }); err == nil {
		t.Fatal("EventPage must surface corrupt column")
	}
}
