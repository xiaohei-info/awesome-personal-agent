package inbound

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type spyBatchQueue struct {
	dispatched int
}

func (q *spyBatchQueue) Dispatch(_ context.Context) error {
	q.dispatched++
	return nil
}

func registerBatchAgent(t *testing.T, e *env, sp policy.SourcePipeline) (*spyBatchQueue, string) {
	t.Helper()
	if err := executors.Register(e.x, executors.Options{Owner: "owner"}); err != nil {
		// 可能在 newEnv 里未注册；若已注册则忽略
		if !strings.Contains(err.Error(), "already registered") {
			t.Fatal(err)
		}
	}
	// 登记一份已确认的无头运行声明与默认 harness
	decl := executors.Declaration{
		Harness: "stub-harness",
		Headless: &executors.Headless{
			Command: []string{"/bin/echo", "{prompt}"},
			Session: executors.Session{Extract: "--session-id"},
			Access:  map[string][]string{"read": {}, "edit": {}, "full": {}},
		},
	}
	e.must("executor.register", decl)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "batch settings", func(p *policy.Policy) error {
			p.DefaultHarness = "stub-harness"
			p.SourcePipeline = sp
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	q := &spyBatchQueue{}
	runsDir := filepath.Join(e.dir, "runs")
	e.svc.AttachBatch(BatchOptions{RunsDir: runsDir, APIURL: "http://127.0.0.1:19999", Queue: q})
	return q, runsDir
}

func enqueueEvent(t *testing.T, e *env, id, level, secretBody string, at time.Time) {
	t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
			occurred_at, correlation_keys, sender, sender_verified, ingested_at) VALUES ('src_mail', ?, ?, 'src_mail', 'r', 'mail.received', 'subj', 'sum', ?, '', ?, '[]', 'a@b.c', 1, ?)`,
			id, id, secretBody, at.UnixNano(), at.UnixNano())
		if err != nil {
			return err
		}
		return runner.Enqueue(tx, id, level)
	}); err != nil {
		t.Fatal(err)
	}
}

func latestBatchRun(t *testing.T, e *env) proc.Record {
	t.Helper()
	var rec proc.Record
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT id FROM outcall_run WHERE category = ? ORDER BY seq DESC LIMIT 1`, string(domain.RunSourceBatch))
		if err != nil {
			return err
		}
		defer r.Close()
		if !r.Next() {
			return fmt.Errorf("no source_batch run found")
		}
		var id string
		if err := r.Scan(&id); err != nil {
			return err
		}
		rec, err = proc.Get(tx, id)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return rec
}

// B3 验收 1：满 N 条触发、等够 T 分钟触发、紧急开关（默认关走攒批、开则立刻触发）、同一时刻只跑一批、提示词不带原文。
func TestBatchTriggersSingleFlightAndMetadataOnlyPrompt(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	q, runsDir := registerBatchAgent(t, e, policy.SourcePipeline{
		BatchSize:       3,
		WaitMinutes:     30,
		UrgentImmediate: false,
		MaxBatch:        2,
		BatchPrompt:     "自定义批处理说明：按团队习惯分类。",
	})

	// 1) 放 1 条急件（开关关着）：不立刻触发，未满 3 条也没等够 30 分钟
	enqueueEvent(t, e, "ev_u1", runner.LevelUrgent, "SECRET-RAW-BODY-NEVER-IN-PROMPT", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	if q.dispatched != 0 {
		t.Fatalf("urgent_immediate=false should not trigger on 1 urgent event, dispatched=%d", q.dispatched)
	}

	// 2) 再放 2 条普通事件，满 3 条触发，但受 max_batch=2 限制只取 2 条（急件优先）
	enqueueEvent(t, e, "ev_n1", runner.LevelNormal, "SECRET-2", e.now)
	enqueueEvent(t, e, "ev_n2", runner.LevelNormal, "SECRET-3", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	if q.dispatched != 1 {
		t.Fatalf("expected 1 batch dispatched on count=3, got %d", q.dispatched)
	}
	run1 := latestBatchRun(t, e)
	if run1.Run.Category != domain.RunSourceBatch || run1.Run.Target != BatchTarget {
		t.Fatalf("unexpected run: %+v", run1.Run)
	}
	// 验证提示词含自定义说明和元信息，但绝不含原文正文！
	promptBytes, err := os.ReadFile(filepath.Join(runsDir, run1.Run.ID, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	prompt := string(promptBytes)
	if !strings.Contains(prompt, "自定义批处理说明") || !strings.Contains(prompt, "id=ev_u1") {
		t.Fatalf("prompt missing custom body or event id: %s", prompt)
	}
	if strings.Contains(prompt, "SECRET-RAW-BODY-NEVER-IN-PROMPT") {
		t.Fatalf("prompt must NOT include raw event body: %s", prompt)
	}

	// 3) 同一时刻只跑一批：再调 TickQueue 不起第二批
	enqueueEvent(t, e, "ev_n3", runner.LevelNormal, "SECRET-4", e.now)
	enqueueEvent(t, e, "ev_n4", runner.LevelNormal, "SECRET-5", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	if q.dispatched != 1 {
		t.Fatalf("must allow only one in-flight batch at a time, got dispatched=%d", q.dispatched)
	}

	// 4) 结束第一批（成功），验证 Notify 收到输出且空输出给固定句
	var notified []string
	e.svc.o.Batch.Notify = func(_ context.Context, _ domain.Run, summary string) error {
		notified = append(notified, summary)
		return nil
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunSucceeded), run1.Run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	run1.Run.State = domain.RunSucceeded
	e.svc.SettleBatch(bg, proc.Result{Run: run1.Run, Stdout: []byte("本批处理了 2 条，新建 1 条建议。")})
	if len(notified) != 1 || !strings.Contains(notified[0], "处理了 2 条") {
		t.Fatalf("unexpected notify: %+v", notified)
	}

	// 5) 放完剩余的，测试等够 T 分钟触发 + 空输出兜底
	e2 := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	q2, _ := registerBatchAgent(t, e2, policy.SourcePipeline{BatchSize: 10, WaitMinutes: 30, MaxBatch: 20})
	enqueueEvent(t, e2, "ev_wait", runner.LevelNormal, "body", e2.now)
	e2.now = e2.now.Add(29 * time.Minute)
	if err := e2.svc.TickQueue(bg); err != nil || q2.dispatched != 0 {
		t.Fatalf("29m should not trigger 30m wait: err=%v dispatched=%d", err, q2.dispatched)
	}
	e2.now = e2.now.Add(2 * time.Minute) // 31m
	if err := e2.svc.TickQueue(bg); err != nil || q2.dispatched != 1 {
		t.Fatalf("31m should trigger 30m wait: err=%v dispatched=%d", err, q2.dispatched)
	}
	var emptyMsg string
	e2.svc.o.Batch.Notify = func(_ context.Context, _ domain.Run, s string) error { emptyMsg = s; return nil }
	rWait := latestBatchRun(t, e2)
	rWait.Run.State = domain.RunSucceeded
	e2.svc.SettleBatch(bg, proc.Result{Run: rWait.Run, Stdout: nil})
	if emptyMsg != EmptyBatchOutputNotice {
		t.Fatalf("empty stdout should use fixed notice, got %q", emptyMsg)
	}

	// 6) 打开 urgent_immediate：1 条急件立刻触发，且遵守 daily_batch_limit
	e3 := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	q3, _ := registerBatchAgent(t, e3, policy.SourcePipeline{BatchSize: 10, WaitMinutes: 30, UrgentImmediate: true, DailyBatchLimit: 1})
	enqueueEvent(t, e3, "ev_imm", runner.LevelUrgent, "body", e3.now)
	if err := e3.svc.TickQueue(bg); err != nil || q3.dispatched != 1 {
		t.Fatalf("urgent_immediate=true should trigger immediately: err=%v dispatched=%d", err, q3.dispatched)
	}
	rImm := latestBatchRun(t, e3)
	if err := e3.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunSucceeded), rImm.Run.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	rImm.Run.State = domain.RunSucceeded
	e3.svc.SettleBatch(bg, proc.Result{Run: rImm.Run})
	enqueueEvent(t, e3, "ev_imm2", runner.LevelUrgent, "body", e3.now)
	if err := e3.svc.TickQueue(bg); err != nil || q3.dispatched != 1 {
		t.Fatalf("daily_batch_limit=1 should stop second batch today: err=%v dispatched=%d", err, q3.dispatched)
	}
}

// B3 验收 2：运行失败、超时、内核重启恢复三种情况，未处置事件都回到待处理并计次，超重试上限转入积压（process_failed）。
func TestBatchFailureTimeoutAndRestartRecovery(t *testing.T) {
	retries := 2
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	_, _ = registerBatchAgent(t, e, policy.SourcePipeline{BatchSize: 1, WaitMinutes: 30, MaxRetries: &retries})
	enqueueEvent(t, e, "ev_flaky", runner.LevelNormal, "body", e.now)

	checkEv := func(wantDisp domain.Disposition, wantQueue, wantReason string) {
		t.Helper()
		var ev domain.SourceEvent
		_ = e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, "ev_flaky"); return })
		if ev.Disposition != wantDisp || ev.Queue != wantQueue || ev.UnjudgedReason != wantReason {
			t.Fatalf("got disp=%s queue=%s reason=%s, want disp=%s queue=%s reason=%s",
				ev.Disposition, ev.Queue, ev.UnjudgedReason, wantDisp, wantQueue, wantReason)
		}
	}

	// 第 1 次：运行失败 -> 回到 queued（tries=1 <= 2）
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	r1 := latestBatchRun(t, e)
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunFailed), r1.Run.ID)
		return err
	})
	r1.Run.State = domain.RunFailed
	e.svc.SettleBatch(bg, proc.Result{Run: r1.Run, Reason: "exit 1"})
	checkEv(domain.DispositionPending, runner.QueueQueued, "")

	// 第 2 次：运行超时 -> 回到 queued（tries=2 <= 2）
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	r2 := latestBatchRun(t, e)
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunTimeout), r2.Run.ID)
		return err
	})
	r2.Run.State = domain.RunTimeout
	e.svc.SettleBatch(bg, proc.Result{Run: r2.Run, Reason: "timeout"})
	checkEv(domain.DispositionPending, runner.QueueQueued, "")

	// 第 3 次：模拟内核崩溃重启（事件仍停在 running，而 run 记录已非运行态）-> RecoverBatches 收尾，tries=3 > 2 转积压 process_failed
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	r3 := latestBatchRun(t, e)
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunFailed), r3.Run.ID)
		return err
	})
	if err := e.svc.RecoverBatches(bg); err != nil {
		t.Fatal(err)
	}
	checkEv(domain.DispositionUnjudged, "", domain.UnjudgedProcessFailed)
	var _ json.RawMessage
}

// FIX-1：BatchEnv 签发运行凭据失败时必须向外返回错误，且 proc.Dispatcher 禁止无令牌启动子进程，事件回到 queued。
func TestBatchEnvIssueErrorBlocksLaunch(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	if err := executors.Register(e.x, executors.Options{Owner: "owner"}); err != nil && !strings.Contains(err.Error(), "already registered") {
		t.Fatal(err)
	}
	marker := filepath.Join(e.dir, "launched-without-token")
	decl := executors.Declaration{
		Harness: "stub-harness",
		Headless: &executors.Headless{
			Command: []string{"/bin/sh", "-c", "touch " + marker},
			Session: executors.Session{Extract: "$.session_id"},
			Access:  map[string][]string{"read": {}},
		},
	}
	e.must("executor.register", decl)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "batch settings", func(p *policy.Policy) error {
			p.DefaultHarness = "stub-harness"
			p.SourcePipeline = policy.SourcePipeline{BatchSize: 1}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	done := make(chan proc.Result, 1)
	disp := proc.New(e.st, config.Config{MaxSubprocesses: 2, MaxShortSubprocesses: 1}, proc.Options{
		EnvErr: e.svc.BatchEnv,
		OnResult: func(res proc.Result) {
			e.svc.SettleBatch(bg, res)
			done <- res
		},
	})
	t.Cleanup(disp.Close)
	e.svc.AttachBatch(BatchOptions{RunsDir: filepath.Join(e.dir, "runs"), APIURL: "http://127.0.0.1:19999", Queue: disp})

	// 1) 直接调 BatchEnv：签发失败（当前分支 runidentity 尚未允许 source_batch 或 run 不存在）必须返回错误且 env 为 nil
	if env, err := e.svc.BatchEnv(domain.Run{ID: "run_missing", Category: domain.RunSourceBatch, Harness: "stub-harness"}); err == nil || env != nil {
		t.Fatalf("BatchEnv must return error on issue failure, got env=%v err=%v", env, err)
	}

	// 2) 走完整 TickQueue -> proc.Dispatcher：签发失败时禁止启动子进程（marker 不得存在），运行失败，事件重排回 queued
	enqueueEvent(t, e, "ev_notok", runner.LevelNormal, "body", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	select {
	case res := <-done:
		if res.Run.State != domain.RunFailed || !strings.Contains(res.Reason, "issue run credential") {
			t.Fatalf("expected failed run due to credential issue error, got state=%s reason=%q", res.Run.State, res.Reason)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for batch run failure")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("subprocess was launched despite BatchEnv credential issue failure")
	}
	var ev domain.SourceEvent
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, "ev_notok"); return })
	if ev.Queue != runner.QueueQueued || ev.Disposition != domain.DispositionPending {
		t.Fatalf("event should be requeued after credential failure, got queue=%s disp=%s", ev.Queue, ev.Disposition)
	}
}

// 处理 Agent 拉不起来时队列不再静默堆积：记一条健康告警，同一原因不刷屏，能拉起后再卡会再报。
func TestBatchBlockedWritesOneHealthAlert(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	q := &spyBatchQueue{}
	e.svc.AttachBatch(BatchOptions{RunsDir: filepath.Join(e.dir, "runs"), APIURL: "http://127.0.0.1:19999", Queue: q})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "batch settings", func(p *policy.Policy) error {
			p.SourcePipeline = policy.SourcePipeline{BatchSize: 1}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	alerts := func() (n int) {
		_ = e.st.View(bg, func(tx *store.Tx) error {
			es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert, Object: BatchTarget}, 0, 10)
			n = len(es)
			return err
		})
		return
	}
	enqueueEvent(t, e, "ev_blocked", runner.LevelNormal, "body", e.now)
	for i := 0; i < 2; i++ {
		if err := e.svc.TickQueue(bg); err != nil {
			t.Fatal(err)
		}
	}
	if q.dispatched != 0 || alerts() != 1 {
		t.Fatalf("blocked: dispatched=%d alerts=%d, want 0 and exactly 1", q.dispatched, alerts())
	}
	registerBatchAgent(t, e, policy.SourcePipeline{BatchSize: 1})
	e.svc.AttachBatch(BatchOptions{RunsDir: filepath.Join(e.dir, "runs"), APIURL: "http://127.0.0.1:19999", Queue: q})
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	if q.dispatched != 1 || alerts() != 1 {
		t.Fatalf("recovered: dispatched=%d alerts=%d", q.dispatched, alerts())
	}
}

// 未指定处理 Agent：用默认 Agent 和它权限卡上的工具档位，不借主 Agent 更高的权限。
func TestBatchWithoutAgentUsesDefaultAgentNotMain(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	registerBatchAgent(t, e, policy.SourcePipeline{BatchSize: 1})
	res := e.must("agent.configure", map[string]any{"harness": "stub-harness", "name": "daily", "enabled": true})
	var card struct{ ID string }
	if err := json.Unmarshal(res, &card); err != nil || card.ID == "" {
		t.Fatalf("card: %s %v", res, err)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "roles", func(p *policy.Policy) error {
			main := "legacy:stub-harness"
			p.DefaultAgentID, p.MainAgentID = &card.ID, &main
			p.AgentCards = map[string]policy.Card{main: {Tool: "full"}, card.ID: {Tool: "edit"}}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	enqueueEvent(t, e, "ev_d", runner.LevelNormal, "x", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	if r := latestBatchRun(t, e).Run; r.AgentID != card.ID || r.Access != "edit" {
		t.Fatalf("agent=%s access=%s", r.AgentID, r.Access)
	}
}
