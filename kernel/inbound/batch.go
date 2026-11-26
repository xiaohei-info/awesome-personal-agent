package inbound

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// BatchTarget 是来源批处理运行的固定目标标识：外呼调度器按它保证同一时刻最多只跑一批（§9.3、§13.6）。
	BatchTarget = "source_batch"
	// BatchTemplate 是内置能力包 inbound-triage 里的批处理任务型模板名。
	BatchTemplate = "source-batch"
	// EmptyBatchOutputNotice 是批处理成功但未产生文字输出时的固定知会句（§2.3、B4）。
	EmptyBatchOutputNotice = "本批来源已处理完成，未输出文字说明。"

	pendingNotifyFile = "notify.pending.json"
)

type pendingNotification struct {
	Run     domain.Run `json:"run"`
	Summary string     `json:"summary"`
}

// BatchQueue 是外呼调用器的调度入口（*proc.Dispatcher 满足它）。
type BatchQueue interface {
	Dispatch(ctx context.Context) error
}

// BatchOptions 配置来源处理线的队列调度与批处理运行（§9.3、§13.6）。
type BatchOptions struct {
	RunsDir string
	APIURL  string
	Queue   BatchQueue
	// Notify 在批处理成功结束后把最终输出交给触达（投递矩阵 source_result 行，B4）。可为空。
	Notify func(ctx context.Context, run domain.Run, summary string) error
}

// AttachBatch 在装配方建好外呼调度器后挂接批处理运行选项。
func (s *Service) AttachBatch(o BatchOptions) {
	s.batching.Lock()
	defer s.batching.Unlock()
	s.o.Batch = o
}

func newBatchRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(b[:]), nil
}

func (s *Service) batchPromptTemplate() (packs.Prompt, bool) {
	if s.o.Packs == nil {
		return packs.Prompt{}, false
	}
	for _, pk := range s.o.Packs.List() {
		if pk.Name == DefaultPack {
			pr, ok := pk.Prompts[BatchTemplate]
			return pr, ok
		}
	}
	return packs.Prompt{}, false
}

// resolveBatchAgent 解析来源批处理使用的无头 Agent：优先策略 source_pipeline.agent，其次主 Agent，再其次默认 Agent。
func resolveBatchAgent(tx *store.Tx, pol policy.Policy) (executors.Selected, error) {
	if pol.SourcePipeline.Agent != "" {
		return executors.Resolve(tx, pol, executors.Selection{Item: pol.SourcePipeline.Agent})
	}
	sel, err := executors.Resolve(tx, pol, executors.Selection{Runner: "main_session"})
	if err != nil {
		return sel, err
	}
	if sel.Reason != "" && pol.MainAgentID == nil {
		return executors.Resolve(tx, pol, executors.Selection{})
	}
	return sel, nil
}

// buildBatchPrompt 组装批处理提示词：正文取本人在设置页自定义的说明或内置模板正文，后面只附本批事件的元信息
// （事件 id、来源、类型、时间、等级），绝不打包事件正文或原文（§9.3、§13.6）。
func buildBatchPrompt(base string, evs []domain.SourceEvent) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(base))
	b.WriteString("\n\n## 本批待处理来源事件（仅元信息，请用 `apa source event <id>` 读原文）\n")
	for i, ev := range evs {
		at := ev.OccurredAt.UTC().Format(time.RFC3339)
		level := ev.QueueLevel
		if level == "" {
			level = runner.LevelNormal
		}
		fmt.Fprintf(&b, "%d. id=%s | source=%s | type=%s | time=%s | level=%s\n",
			i+1, ev.ID, ev.SourceID, ev.Type, at, level)
	}
	return b.String()
}

func expandArgs(in []string, pairs ...string) []string {
	r := strings.NewReplacer(pairs...)
	out := make([]string, len(in))
	for i, a := range in {
		out[i] = r.Replace(a)
	}
	return out
}

func startOfLocalDay(now time.Time) time.Time {
	loc := now.In(time.Local)
	return time.Date(loc.Year(), loc.Month(), loc.Day(), 0, 0, 0, 0, time.Local)
}

// TickQueue 是来源处理线唯一的定时驱动（§9.3）：先补投上次失败留下的批处理结论通知，
// 再检查队列条件（立即开关+急件、满 N 条、最早一条等够 T 分钟），满足时在同一事务里取批并把 source_batch 运行入队，同一时刻只跑一批。
func (s *Service) TickQueue(ctx context.Context) error {
	if s.o.Batch.Queue == nil || s.o.Batch.RunsDir == "" {
		return nil
	}
	s.batching.Lock()
	defer s.batching.Unlock()

	notifyErr := s.flushPendingNotifications(ctx)
	tickErr := s.tickQueueLocked(ctx)
	return errors.Join(notifyErr, tickErr)
}

func (s *Service) tickQueueLocked(ctx context.Context) error {

	now := s.o.Now()
	var (
		pol      policy.Policy
		st       runner.QueueStat
		busy     bool
		todayCnt int
		selected executors.Selected
	)
	if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		if _, pol, err = policy.Current(tx); err != nil {
			return err
		}
		if busy, err = proc.InFlight(tx, BatchTarget); err != nil || busy {
			return err
		}
		if st, err = runner.Queued(tx); err != nil || st.Urgent+st.Normal == 0 {
			return err
		}
		if pol.SourcePipeline.DailyBatchLimit > 0 {
			if todayCnt, err = proc.CountCategorySince(tx, domain.RunSourceBatch, startOfLocalDay(now)); err != nil {
				return err
			}
		}
		selected, err = resolveBatchAgent(tx, pol)
		return err
	}); err != nil {
		return err
	}
	if busy || st.Urgent+st.Normal == 0 {
		return nil
	}
	sp := pol.SourcePipeline
	if sp.DailyBatchLimit > 0 && todayCnt >= sp.DailyBatchLimit {
		return nil
	}
	total := st.Urgent + st.Normal
	immediate := sp.UrgentImmediate && st.Urgent > 0
	byCount := total >= sp.Batch()
	byWait := !st.Oldest.IsZero() && now.Sub(st.Oldest) >= sp.Wait()
	if !immediate && !byCount && !byWait {
		return nil
	}
	pr, ok := s.batchPromptTemplate()
	if !ok {
		return fmt.Errorf("inbound: batch prompt template %s/%s not loaded", DefaultPack, BatchTemplate)
	}
	access := selected.Snapshot.PermissionCap // 工具档位取所属 Agent 权限卡上的值（§14.5）
	if why := selected.AccessReason(access); why != "" {
		return nil
	}
	if selected.Record.Declaration.Headless == nil {
		return nil
	}
	hl := *selected.Record.Declaration.Headless
	runID, err := newBatchRunID()
	if err != nil {
		return err
	}
	runDir := filepath.Join(s.o.Batch.RunsDir, runID)
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		return err
	}
	basePrompt := strings.TrimSpace(sp.BatchPrompt)
	if basePrompt == "" {
		basePrompt = pr.Body
	}

	var launched bool
	err = s.o.Store.Update(ctx, func(tx *store.Tx) error {
		inFlight, err := proc.InFlight(tx, BatchTarget)
		if err != nil || inFlight {
			return err
		}
		evs, err := runner.Claim(tx, runID, sp.MaxBatchSize())
		if err != nil || len(evs) == 0 {
			return err
		}
		prompt := buildBatchPrompt(basePrompt, evs)
		if err := os.WriteFile(filepath.Join(runDir, "prompt.txt"), []byte(prompt), 0o600); err != nil {
			return err
		}
		argv := expandArgs(hl.Command, "{prompt}", prompt, "{run_dir}", runDir)
		argv = append(argv, selected.Args...)
		argv = append(argv, hl.Output...)
		argv = append(argv, hl.Access[access]...)
		sessionID := ""
		if len(hl.Session.Assign) > 0 {
			sessionID = runID
			argv = append(argv, expandArgs(hl.Session.Assign, "{session_id}", sessionID)...)
		}
		run := domain.Run{
			ID:            runID,
			Category:      domain.RunSourceBatch,
			Runner:        BatchTarget,
			Launch:        "headless",
			Target:        BatchTarget,
			Harness:       selected.Card.Harness,
			SessionID:     sessionID,
			Access:        access,
			TimeoutSec:    int(pol.BackgroundRunTimeout() / time.Second),
			AgentSnapshot: &selected.Snapshot,
		}
		if err := executors.CheckRun(tx, run); err != nil {
			return err
		}
		if _, err := proc.Enqueue(tx, proc.Request{
			Run:  run,
			Spec: proc.Spec{OptionEnv: selected.Env, Command: argv, Dir: runDir, RunDir: runDir},
		}, now); err != nil {
			return err
		}
		body, _ := json.Marshal(run)
		if _, err := eventlog.Append(tx, domain.Event{
			Type: domain.EvAgentQueued, Object: BatchTarget, Actor: system.Actor, Payload: body, At: now,
		}); err != nil {
			return err
		}
		launched = true
		return nil
	})
	if err != nil {
		_ = os.RemoveAll(runDir)
		return err
	}
	if !launched {
		_ = os.RemoveAll(runDir)
		return nil
	}
	return s.o.Batch.Queue.Dispatch(ctx)
}

// BatchEnv 在外呼调用器拉起 source_batch 子进程前签发运行凭据并组装环境变量（§7.7、§13.6）。
// 签发失败时向外返回错误，禁止在没有运行令牌的情况下启动子进程。
func (s *Service) BatchEnv(r domain.Run) (map[string]string, error) {
	if r.Category != domain.RunSourceBatch {
		return nil, nil
	}
	var token string
	if err := s.o.Store.Update(context.Background(), func(tx *store.Tx) error {
		var err error
		token, err = runidentity.Issue(tx, r.ID, s.o.Now())
		return err
	}); err != nil {
		return nil, fmt.Errorf("issue run credential: %w", err)
	}
	if token == "" {
		return nil, fmt.Errorf("issue run credential: empty token")
	}
	return map[string]string{
		"APA_URL":     s.o.Batch.APIURL,
		"APA_RUN":     r.ID,
		"APA_HARNESS": r.Harness,
		"APA_TOKEN":   token,
	}, nil
}

// SettleBatch 在 source_batch 运行结束后收尾队列状态：成功转 done 并把输出作为知会发给主会话；
// 失败或超时时未处置的事件放回 queued 并计次，超上限转 unjudged（process_failed）。
func (s *Service) SettleBatch(ctx context.Context, res proc.Result) {
	if res.Run.Category != domain.RunSourceBatch {
		return
	}
	ok := res.Run.State == domain.RunSucceeded
	var (
		fin         runner.Finished
		hl          executors.Headless
		hasHL       bool
		summary     string
		pendingPath string
	)
	if err := s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		fin, err = runner.Finish(tx, res.Run.ID, ok, pol.SourcePipeline.Retries())
		if err != nil {
			return err
		}
		if res.Run.Harness != "" {
			hl, hasHL, _ = executors.ActiveHeadless(tx, res.Run.Harness)
		}
		payload, _ := json.Marshal(map[string]any{
			"run":      res.Run.ID,
			"category": res.Run.Category,
			"state":    res.Run.State,
			"done":     len(fin.Done),
			"requeued": len(fin.Requeued),
			"dropped":  len(fin.Dropped),
			"reason":   res.Reason,
		})
		if _, err = eventlog.Append(tx, domain.Event{
			Type: domain.EvRunFinished, Object: BatchTarget, Actor: system.Actor, Payload: payload, At: s.o.Now(),
		}); err != nil {
			return err
		}
		if ok && s.o.Batch.Notify != nil {
			summary = strings.TrimSpace(extractBatchSummary(hl, hasHL, res.Stdout))
			if summary == "" {
				summary = EmptyBatchOutputNotice
			}
			p, werr := s.writePendingNotify(res.Run, summary)
			if werr != nil {
				return werr
			}
			pendingPath = p
		}
		return nil
	}); err != nil {
		if pendingPath != "" {
			_ = os.Remove(pendingPath)
		}
		if s.o.OnError != nil {
			s.o.OnError(fmt.Errorf("inbound: settle batch %s: %w", res.Run.ID, err))
		}
		return
	}
	if ok && s.o.Batch.Notify != nil {
		if err := s.deliverBatchNotification(ctx, res.Run, summary, pendingPath); err != nil && s.o.OnError != nil {
			s.o.OnError(fmt.Errorf("inbound: notify batch %s: %w", res.Run.ID, err))
		}
	}
}

func (s *Service) writePendingNotify(run domain.Run, summary string) (string, error) {
	if s.o.Batch.RunsDir == "" || run.ID == "" || filepath.Base(run.ID) != run.ID || run.ID == "." || run.ID == ".." {
		return "", nil
	}
	dir := filepath.Join(s.o.Batch.RunsDir, run.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	b, err := json.Marshal(pendingNotification{Run: run, Summary: summary})
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, pendingNotifyFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return path, nil
}

func (s *Service) deliverBatchNotification(ctx context.Context, run domain.Run, summary, pendingPath string) error {
	s.notifying.Lock()
	defer s.notifying.Unlock()
	if pendingPath != "" {
		if _, err := os.Stat(pendingPath); os.IsNotExist(err) {
			return nil
		}
	}
	if err := s.o.Batch.Notify(ctx, run, summary); err != nil {
		return err
	}
	if pendingPath != "" {
		if err := os.Remove(pendingPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (s *Service) flushPendingNotifications(ctx context.Context) error {
	if s.o.Batch.Notify == nil || s.o.Batch.RunsDir == "" {
		return nil
	}
	entries, err := os.ReadDir(s.o.Batch.RunsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var errs []error
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		path := filepath.Join(s.o.Batch.RunsDir, entry.Name(), pendingNotifyFile)
		raw, err := os.ReadFile(path)
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			errs = append(errs, fmt.Errorf("inbound: read pending notify %s: %w", entry.Name(), err))
			continue
		}
		var p pendingNotification
		if err := json.Unmarshal(raw, &p); err != nil {
			errs = append(errs, fmt.Errorf("inbound: decode pending notify %s: %w", entry.Name(), err))
			continue
		}
		if err := s.deliverBatchNotification(ctx, p.Run, p.Summary, path); err != nil {
			errs = append(errs, fmt.Errorf("inbound: retry notify batch %s: %w", p.Run.ID, err))
		}
	}
	return errors.Join(errs...)
}

func extractBatchSummary(hl executors.Headless, hasHL bool, stdout []byte) string {
	if !hasHL {
		hl = executors.Headless{}
	}
	return hl.ExtractFinalText(stdout)
}

// RecoverBatches 在内核启动时检查仍停在 running 的事件：对应运行已不在队列或已结束（没有存活进程）的，
// 按失败收尾放回待处理并计次，防止重启后事件永久卡在"处理中"（§9.3）；同时补投上次未写成的批处理结论通知。
func (s *Service) RecoverBatches(ctx context.Context) error {
	recErr := s.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		runs, err := runner.RunningRuns(tx)
		if err != nil {
			return err
		}
		retries := pol.SourcePipeline.Retries()
		for _, runID := range runs {
			rec, err := proc.Get(tx, runID)
			alive := err == nil && (rec.Run.State == domain.RunQueued || rec.Run.State == domain.RunRunning)
			if alive {
				continue
			}
			if _, err := runner.Finish(tx, runID, false, retries); err != nil {
				return err
			}
		}
		return nil
	})
	notifyErr := s.flushPendingNotifications(ctx)
	return errors.Join(recErr, notifyErr)
}
