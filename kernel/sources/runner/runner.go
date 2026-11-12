// Package runner 是 Source 的调度与执行契约（设计 §9.2、§7.7）：
// 时钟到期时经外呼调用器拉起来源，解析标准事件 JSONL 与末行游标；事件在同一事务里先落盘、游标后推进，
// 按 (去重域, 事件 id) 去重；退出码 0..3 各有处置；同一来源已有排队或运行中的 Run 时不重复入队。
//
// 本包不做规则与判断（来源事件落盘后 disposition 为 pending，由入站流水线接手）。
// 游标与统计存在登记表的 cursor/stats 列，经 registry.SetRunState 写入；事项、停用、投递都经命令模型或对方的入口。
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdRunResult 是把一次拉取的结果记入账本的内部命令；每个 Run 只生效一次（幂等键是 run id）。
const CmdRunResult = "source.run_result"

const (
	timerPrefix   = "srcpull_"
	systemPrinc   = "kernel:sources-runner"
	maxBackoff    = time.Hour
	dispatchError = "dispatch"
)

var system = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: systemPrinc, Via: domain.ViaSource}, Level: authz.LevelWorker}

// Module 是本模块在存储层登记的迁移：来源事件与未入账的输出行。
var Module = store.Module{Name: "sources_runner", Migrations: []store.Migration{{
	Version: 1, Name: "create-event-and-reject",
	SQL: `CREATE TABLE sources_runner_event (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	dedupe_domain TEXT NOT NULL,
	event_id TEXT NOT NULL,
	id TEXT NOT NULL UNIQUE,
	source_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	type TEXT NOT NULL,
	subject TEXT NOT NULL,
	summary TEXT NOT NULL,
	detail TEXT NOT NULL,
	link TEXT NOT NULL,
	occurred_at INTEGER NOT NULL,
	correlation_keys TEXT NOT NULL,
	sender TEXT NOT NULL,
	sender_verified INTEGER NOT NULL,
	disposition TEXT NOT NULL DEFAULT 'pending',
	item TEXT NOT NULL DEFAULT '',
	attach_state TEXT NOT NULL DEFAULT '',
	UNIQUE (dedupe_domain, event_id)
);
CREATE INDEX sources_runner_event_source ON sources_runner_event(source_id, seq);
CREATE TABLE sources_runner_reject (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	source_id TEXT NOT NULL,
	run_id TEXT NOT NULL,
	line INTEGER NOT NULL,
	reason TEXT NOT NULL,
	raw TEXT NOT NULL,
	at INTEGER NOT NULL
);
CREATE INDEX sources_runner_reject_source ON sources_runner_reject(source_id, seq);`,
}, {
	// 未判定的原因与自动重判次数（§9.3）：模型恢复后只重判因模型不可用而未判定的事件；次数进处置的幂等键，重判不撞上次的结果。
	Version: 2, Name: "add-event-unjudged-reason",
	SQL: `ALTER TABLE sources_runner_event ADD COLUMN unjudged_reason TEXT NOT NULL DEFAULT '';
ALTER TABLE sources_runner_event ADD COLUMN rejudges INTEGER NOT NULL DEFAULT 0;
CREATE INDEX sources_runner_event_disposition ON sources_runner_event(disposition, seq);`,
}, {
	// 账本结构版本 3（§6.10）：来源事件的指标、合并键、多事项、最近一次判断记录 id；
	// 来源指标取值（按来源、关联键、指标名保存上一次取值，条件规则比较“变了、上穿、下穿”用）。
	Version: 3, Ledger: 3, Name: "ledger-v3-event-metrics-merge-judgment",
	SQL: `ALTER TABLE sources_runner_event ADD COLUMN metrics TEXT NOT NULL DEFAULT '';
ALTER TABLE sources_runner_event ADD COLUMN merge_key TEXT NOT NULL DEFAULT '';
ALTER TABLE sources_runner_event ADD COLUMN items TEXT NOT NULL DEFAULT '';
ALTER TABLE sources_runner_event ADD COLUMN judgment TEXT NOT NULL DEFAULT '';
CREATE INDEX sources_runner_event_merge_key ON sources_runner_event(merge_key, seq) WHERE merge_key <> '';
CREATE TABLE sources_runner_metric (
	source_id TEXT NOT NULL,
	correlation_key TEXT NOT NULL,
	name TEXT NOT NULL,
	value REAL NOT NULL,
	at INTEGER NOT NULL,
	PRIMARY KEY (source_id, correlation_key, name)
) WITHOUT ROWID;`,
}, {
	// 标准事件 v2 的 items[]（聚合消息）：账本结构版本 3 的 items 列存的是事项 id，消息另存一列，供 extract 出口抽取（§9.2）。
	Version: 4, Ledger: 3, Name: "event-messages",
	SQL: `ALTER TABLE sources_runner_event ADD COLUMN messages TEXT NOT NULL DEFAULT '';`,
}}}

// Stats 是登记表 stats 列的内容（来源页读取）。
type Stats struct {
	Runs                int       `json:"runs"`
	ConsecutiveFailures int       `json:"consecutive_failures"`
	Timeouts            int       `json:"timeouts"`
	LastSuccessAt       time.Time `json:"last_success_at,omitzero"`
	LastFailureAt       time.Time `json:"last_failure_at,omitzero"`
	LastError           string    `json:"last_error,omitempty"`
	NextAttemptAt       time.Time `json:"next_attempt_at,omitzero"` // 退避：此前到期的定时拉取被跳过
	NeedsInputItem      string    `json:"needs_input_item,omitempty"`
	TimerPeriod         string    `json:"timer_period,omitempty"` // 当前登记的定时节拍；与 schedule 不同说明要重建
}

// StatsOf 取来源的统计；还没跑过为零值。
func StatsOf(s domain.Source) (st Stats, err error) {
	if len(s.Stats) > 0 {
		err = json.Unmarshal(s.Stats, &st)
	}
	return st, err
}

func encode(st Stats) json.RawMessage {
	b, err := json.Marshal(st)
	if err != nil {
		panic(err) // 固定结构
	}
	return b
}

// Dispatcher 是外呼调用器的调度入口（*proc.Dispatcher 满足它）。
type Dispatcher interface {
	Dispatch(ctx context.Context) error
}

// Options 是调度所需的外部事实。
type Options struct {
	Store *store.Store
	Exec  *command.Executor
	// Layout 非零时，来源子进程的工作目录是该来源自己的目录（§9.1）。
	Layout config.Layout
	Now    func() time.Time
	// OnError 接收无处可返回的后台错误（提交后同步时间轮的失败）。
	OnError func(error)
}

// Runner 持有无状态的调度逻辑；状态全在账本里。
type Runner struct {
	o     Options
	disp  Dispatcher
	dirty atomic.Bool // 本轮时钟有新入队的 Run，等 Tick 后派发
}

// New 创建 Runner。
func New(o Options) (*Runner, error) {
	if o.Store == nil || o.Exec == nil {
		return nil, errors.New("runner: Store and Exec are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Runner{o: o}, nil
}

// Attach 接上外呼调用器（它的 OnResult 又要指向 Handle，所以不能在 New 里给）。
func (r *Runner) Attach(d Dispatcher) { r.disp = d }

func (r *Runner) fail(err error) {
	if r.o.OnError != nil {
		r.o.OnError(err)
	}
}

// Register 登记内部命令，并让来源登记的变更（登记、改周期、启停）提交后同步时间轮。
func (r *Runner) Register(x *command.Executor) error {
	err := x.Register(command.Spec{Name: CmdRunResult, Action: authz.ActionUpdateOwn, Object: objectOf,
		Apply: r.applyResult})
	if err != nil {
		return err
	}
	if err := x.Register(command.Spec{Name: CmdIngest, Action: authz.ActionUpdateOwn, Object: objectOf, Apply: r.applyIngest}); err != nil {
		return err
	}
	x.OnCommit(func(c command.Commit) {
		if strings.HasPrefix(c.Command, "source.") && c.Command != CmdRunResult && c.Command != CmdIngest {
			if err := r.Sync(context.Background()); err != nil {
				r.fail(fmt.Errorf("runner: sync timers: %w", err))
			}
		}
	})
	return nil
}

func objectOf(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var h struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal(in, &h); err != nil {
		return authz.Object{}, domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
	}
	// 内核自己维护调度状态：来源的"执行者"记为内核身份，使这些写入属于"改自己的对象"。
	return authz.Object{ID: h.SourceID, Assignee: systemPrinc}, nil
}

func timerID(source string) string { return timerPrefix + source }

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

// Sync 让每个启用的 pull/script 来源在时钟里有且只有一个与 schedule 一致的周期 Timer，
// 停用或改过周期的随之撤掉或重建。幂等；启动时调用一次，之后由登记变更触发。
func (r *Runner) Sync(ctx context.Context) error {
	return r.o.Store.Update(ctx, func(tx *store.Tx) error {
		srcs, err := registry.List(tx)
		if err != nil {
			return err
		}
		for _, s := range srcs {
			st, err := StatsOf(s)
			if err != nil {
				return fmt.Errorf("source %s: bad stats: %w", s.ID, err)
			}
			want := ""
			if s.Enabled && s.Type != domain.SourceWebhook {
				want = s.Schedule
			}
			if st.TimerPeriod == want {
				continue
			}
			if st.TimerPeriod != "" {
				if err := clock.Cancel(tx, timerID(s.ID)); err != nil && !isNotFound(err) {
					return err
				}
			}
			if want != "" {
				if _, err := clock.Add(tx, domain.Timer{ID: timerID(s.ID), Target: s.ID, Kind: domain.TimerRecur,
					At: r.o.Now(), Recurrence: want}); err != nil {
					return err
				}
			}
			st.TimerPeriod = want
			if err := registry.SetRunState(tx, s.ID, s.Cursor, encode(st)); err != nil {
				return err
			}
		}
		return nil
	})
}

// Tick 扫描一次时钟，并把到期入队的来源 Run 派发出去。
func (r *Runner) Tick(ctx context.Context, c *clock.Clock) error {
	_, tickErr := c.Tick(ctx)
	if r.dirty.Swap(false) && r.disp != nil {
		if err := r.disp.Dispatch(ctx); err != nil {
			return errors.Join(tickErr, fmt.Errorf("runner: %s: %w", dispatchError, err))
		}
	}
	return tickErr
}

// Owns 判断时钟交来的命令是否归本模块；组装处据它在处理器间分派。
func (r *Runner) Owns(c clock.Command) bool {
	return c.Kind == domain.TimerRecur && strings.HasPrefix(c.TimerID, timerPrefix)
}

// HandleTimer 处理来源的定时到期：在时钟的事务里把拉取入队。时钟只有一个处理器，
// 装配处把它放在处理器链里；不是来源节拍的命令原样忽略。
func (r *Runner) HandleTimer(tx *store.Tx, c clock.Command) error {
	if c.Kind != domain.TimerRecur || !strings.HasPrefix(c.TimerID, timerPrefix) {
		return nil
	}
	src, err := registry.Get(tx, c.Target)
	if err != nil && !isNotFound(err) {
		return err
	}
	if err != nil || !src.Enabled {
		return cancelTimer(tx, c.TimerID) // 来源已删除或停用：节拍自行撤掉
	}
	st, err := StatsOf(src)
	if err != nil {
		return err
	}
	now := r.o.Now()
	if now.Before(st.NextAttemptAt) {
		return nil // 退避中
	}
	run, spec, err := r.plan(src, st, now)
	if err != nil {
		return err
	}
	_, err = proc.Enqueue(tx, proc.Request{Run: run, Spec: spec}, now)
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeConflict {
		return nil // 同一来源已有排队或运行中的 Run：不重复入队，游标未动，下次不漏
	}
	if err == nil {
		r.dirty.Store(true)
	}
	return err
}

func cancelTimer(tx *store.Tx, id string) error {
	if err := clock.Cancel(tx, id); err != nil && !isNotFound(err) {
		return err
	}
	return nil
}

type stdinWindow struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type stdinDoc struct {
	SourceID        string          `json:"source_id"`
	Config          json.RawMessage `json:"config"`
	Cursor          string          `json:"cursor"`
	Window          stdinWindow     `json:"window"`
	RunID           string          `json:"run_id"`
	CredentialsPath string          `json:"credentials_path,omitempty"` // 只传路径，内核不读内容
}

// plan 组装来源 Run 与它的 "命令 + stdin JSON"（§9.2）。
// shortcut: 命令按空白切分，不支持带引号的参数；需要复杂参数时写进来源的 config。
func (r *Runner) plan(s domain.Source, st Stats, now time.Time) (domain.Run, proc.Spec, error) {
	argv := strings.Fields(s.Command)
	if len(argv) == 0 {
		return domain.Run{}, proc.Spec{}, domain.NewError(domain.CodeInvalidRequest, "source "+s.ID+" has no command", "fix the registration")
	}
	id, err := newRunID()
	if err != nil {
		return domain.Run{}, proc.Spec{}, err
	}
	doc := stdinDoc{SourceID: s.ID, Config: s.Config, Cursor: s.Cursor, RunID: id, CredentialsPath: s.CredentialsPath,
		Window: stdinWindow{To: now.UTC().Format(time.RFC3339)}}
	if len(doc.Config) == 0 {
		doc.Config = json.RawMessage(`{}`)
	}
	if !st.LastSuccessAt.IsZero() {
		doc.Window.From = st.LastSuccessAt.UTC().Format(time.RFC3339)
	}
	in, err := json.Marshal(doc)
	if err != nil {
		return domain.Run{}, proc.Spec{}, err
	}
	spec := proc.Spec{Command: argv, Stdin: in}
	if r.o.Layout.Sources != "" {
		spec.Dir = r.o.Layout.SourceDir(s.ID)
		if err := os.MkdirAll(spec.Dir, 0o700); err != nil {
			return domain.Run{}, proc.Spec{}, err
		}
	}
	return domain.Run{ID: id, Category: domain.RunSource, Target: s.ID, TimeoutSec: s.Timeout}, spec, nil
}

func newRunID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(b[:]), nil
}

// ---- 结果处置 ----

type class string

const (
	clsOK         class = "ok"
	clsTransient  class = "transient"   // 退出码 1、超时、拉不起来、其他退出码
	clsNeedsHuman class = "needs_input" // 退出码 2
	clsPermanent  class = "permanent"   // 退出码 3
)

// resultIn 是内部命令的输入：解析后的结果，不含原始 stdout。
type resultIn struct {
	SourceID   string        `json:"source_id"`
	RunID      string        `json:"run_id"`
	Class      class         `json:"class"`
	Timeout    bool          `json:"timeout,omitempty"`
	Reason     string        `json:"reason,omitempty"`
	FinishedAt time.Time     `json:"finished_at"`
	Events     []parsedEvent `json:"events,omitempty"`
	Rejects    []Reject      `json:"rejects,omitempty"`
	Cursor     string        `json:"cursor,omitempty"`
	Item       string        `json:"item,omitempty"` // 退出码 2 时为本人建的 needs_input 事项
}

// classify 把 Run 的终态映射成处置类别（§9.2 的退出码表）。
func classify(run domain.Run, reason string) (class, string) {
	switch {
	case run.State == domain.RunSucceeded:
		return clsOK, ""
	case run.State == domain.RunTimeout:
		return clsTransient, "timeout"
	case run.ExitCode == nil:
		return clsTransient, orElse(reason, "run failed without exit code")
	}
	switch *run.ExitCode {
	case 1:
		return clsTransient, "exit status 1"
	case 2:
		return clsNeedsHuman, "exit status 2 (auth or config failure)"
	case 3:
		return clsPermanent, "exit status 3 (permanent failure)"
	}
	return clsTransient, fmt.Sprintf("unexpected exit status %d", *run.ExitCode)
}

func orElse(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Handle 处置一次来源 Run 的终态，作为外呼调用器的 OnResult（启动恢复判为失败的 Run 也交给它）。
// 非来源类别的 Run 原样忽略。
func (r *Runner) Handle(ctx context.Context, res proc.Result) error {
	run := res.Run
	if run.Category != domain.RunSource {
		return nil
	}
	in := resultIn{SourceID: run.Target, RunID: run.ID, FinishedAt: r.o.Now()}
	if run.FinishedAt != nil {
		in.FinishedAt = *run.FinishedAt
	}
	in.Class, in.Reason = classify(run, res.Reason)
	in.Timeout = run.State == domain.RunTimeout
	if in.Class == clsOK {
		p := parse(res.Stdout)
		in.Events, in.Rejects = p.events, p.rejects
		switch {
		case res.Overflow: // 末行不是真正的末行，哪怕它长得像游标也不认
			in.Class, in.Reason = clsTransient, "output exceeds max_source_output_mb; events before the limit were kept, cursor held"
		case !p.cursorOK:
			in.Class, in.Reason = clsTransient, "last line is not a valid cursor"
		default:
			in.Cursor = p.cursor
		}
	}
	if in.Class == clsNeedsHuman {
		item, err := r.needsInput(ctx, in)
		if err != nil {
			return err
		}
		in.Item = item
	}
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = r.o.Exec.Do(ctx, command.Request{Identity: system, Command: CmdRunResult, Input: body, IdemKey: run.ID})
	return err
}

// needsInput 为认证或配置失败建一个 needs_input 事项找本人；来源已有未处理的这类事项时复用，不重复建。
func (r *Runner) needsInput(ctx context.Context, in resultIn) (string, error) {
	var open string
	err := r.o.Store.View(ctx, func(tx *store.Tx) error {
		s, err := registry.Get(tx, in.SourceID)
		if err != nil {
			return err
		}
		st, err := StatsOf(s)
		if err != nil || st.NeedsInputItem == "" {
			return err
		}
		it, err := items.Get(tx, st.NeedsInputItem)
		if err == nil && it.Phase == domain.PhaseNeedsInput {
			open = it.ID
		} else if err != nil && !isNotFound(err) {
			return err
		}
		return nil
	})
	if err != nil || open != "" {
		return open, err
	}
	body, err := json.Marshal(map[string]any{
		"kind": domain.KindTask, "phase": domain.PhaseNeedsInput,
		"title":    "来源 " + in.SourceID + " 认证或配置失败",
		"assignee": systemPrinc, // 内核身份，使这次写入属于"改自己的事项"（同任务型的做法）
		"origin":   map[string]string{"source": in.SourceID, "ref": in.RunID},
		"ask": map[string]any{
			"question":  "来源 " + in.SourceID + " 的拉取返回了认证或配置失败（退出码 2），请检查凭据与配置后再启用或等待下次拉取。",
			"free_text": true,
		},
		"reason": "source run " + in.RunID,
	})
	if err != nil {
		return "", err
	}
	res, err := r.o.Exec.Do(ctx, command.Request{Identity: system, Command: "item.create", Input: body, IdemKey: in.RunID})
	if err != nil {
		return "", err
	}
	var ref struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &ref); err != nil || ref.ID == "" {
		return "", fmt.Errorf("runner: item.create returned no id: %v", err)
	}
	return ref.ID, nil
}

// applyResult 在一个事务里落事件、推进游标、更新统计；游标只在事件落盘之后、且末行合法时前进。
func (r *Runner) applyResult(c *command.Ctx) (any, error) {
	var in resultIn
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return nil, domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
	}
	tx := c.Tx
	src, err := registry.Get(tx, in.SourceID)
	if err != nil {
		return nil, err
	}
	st, err := StatsOf(src)
	if err != nil {
		return nil, err
	}
	for i, rj := range in.Rejects {
		if _, err := tx.Exec(`INSERT INTO sources_runner_reject (source_id, run_id, line, reason, raw, at) VALUES (?,?,?,?,?,?)`,
			in.SourceID, in.RunID, rj.Line, rj.Reason, rj.Raw, in.FinishedAt.UnixNano()); err != nil {
			return nil, fmt.Errorf("reject %d: %w", i, err)
		}
	}
	for _, ev := range in.Events {
		if err := insertEvent(tx, src, in.RunID, ev); err != nil {
			return nil, err
		}
	}
	cursor := src.Cursor
	st.Runs++
	if in.Class == clsOK {
		cursor = in.Cursor
		st.ConsecutiveFailures, st.LastError, st.NextAttemptAt = 0, "", time.Time{}
		st.LastSuccessAt = in.FinishedAt
	} else {
		st.ConsecutiveFailures++
		st.LastFailureAt, st.LastError = in.FinishedAt, in.Reason
		if in.Timeout {
			st.Timeouts++
		}
		st.NextAttemptAt = time.Time{}
		switch in.Class {
		case clsTransient:
			st.NextAttemptAt = nextAttempt(in.FinishedAt, src.Schedule, st.ConsecutiveFailures)
		case clsNeedsHuman:
			st.NeedsInputItem = in.Item
			if err := r.failed(c, src, in, "auth_or_config"); err != nil {
				return nil, err
			}
		case clsPermanent:
			st.TimerPeriod = ""
			if err := cancelTimer(tx, timerID(src.ID)); err != nil {
				return nil, err
			}
			if _, err := registry.SetEnabled(c, src.ID, false); err != nil { // 停用与记账同一事务
				return nil, err
			}
			if err := r.failed(c, src, in, "permanent"); err != nil {
				return nil, err
			}
			if err := r.healthAlert(c, src, in); err != nil {
				return nil, err
			}
		}
	}
	if err := registry.SetRunState(tx, src.ID, cursor, encode(st)); err != nil {
		return nil, err
	}
	return map[string]any{"id": src.ID, "class": in.Class, "cursor": cursor}, nil
}

// nextAttempt：第 1 次连续失败下个节拍照常重试；之后每次翻倍，封顶一小时（至少一个周期）。
// shortcut: 只在节拍上跳过，不是独立的重试定时器；需要比周期更密的重试时再加。
func nextAttempt(at time.Time, schedule string, failures int) time.Time {
	p, err := time.ParseDuration(schedule)
	if err != nil || failures < 2 {
		return time.Time{}
	}
	shift := min(failures-1, 20)
	d := p<<shift - p/2
	return at.Add(min(d, max(maxBackoff, p)))
}

// AppendInternal 把内核自己产生的来源事件（不经拉取，如对话增量捕获）落盘为 pending；
// 按 src 的去重键幂等，重复提交不产生第二条。事件 id 取 ev.ID，处置字段由入站流水线维护。
func AppendInternal(tx *store.Tx, src domain.Source, ev domain.SourceEvent) error {
	return insertEvent(tx, src, "", parsedEvent{ID: ev.ID, Type: ev.Type, Subject: ev.Subject, Summary: ev.Summary,
		Detail: ev.Detail, Link: ev.Link, OccurredAt: ev.OccurredAt, CorrelationKeys: ev.CorrelationKeys,
		Sender: ev.Sender, SenderVerified: ev.SenderVerified})
}

// jsonOrEmpty 把可选字段存成 JSON；没有时存空串，与迁移的列缺省一致。
func jsonOrEmpty(v any, empty bool) (string, error) {
	if empty {
		return "", nil
	}
	b, err := json.Marshal(v)
	return string(b), err
}

func insertEvent(tx *store.Tx, src domain.Source, runID string, ev parsedEvent) error {
	key := src.DedupeKeyFor(ev.ID)
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	keys, err := json.Marshal(append([]string{}, ev.CorrelationKeys...))
	if err != nil {
		return err
	}
	verified := 0
	if ev.SenderVerified {
		verified = 1
	}
	metrics, err := jsonOrEmpty(ev.Metrics, len(ev.Metrics) == 0)
	if err != nil {
		return err
	}
	msgs, err := jsonOrEmpty(ev.Items, len(ev.Items) == 0)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO sources_runner_event
		(dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link, occurred_at, correlation_keys, sender, sender_verified,
		 metrics, merge_key, messages)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		key.Domain, key.EventID, "sev_"+hex.EncodeToString(rnd[:]), src.ID, runID, ev.Type, ev.Subject, ev.Summary, ev.Detail,
		ev.Link, ev.OccurredAt.UnixNano(), string(keys), ev.Sender, verified, metrics, ev.MergeKey, msgs)
	return err
}

func (r *Runner) failed(c *command.Ctx, src domain.Source, in resultIn, kind string) error {
	payload, _ := json.Marshal(map[string]any{"source_id": src.ID, "run_id": in.RunID, "kind": kind, "reason": in.Reason})
	_, err := c.Emit(domain.Event{Type: domain.EvSourceFailed, Object: src.ID, Payload: payload})
	return err
}

// healthAlert 记永久失败的 health.alert；健康类投递由 kernel/health 统一产出。
func (r *Runner) healthAlert(c *command.Ctx, src domain.Source, in resultIn) error {
	payload, _ := json.Marshal(map[string]any{"kind": "source", "source_id": src.ID, "run_id": in.RunID, "reason": in.Reason})
	_, err := c.Emit(domain.Event{Type: domain.EvHealthAlert, Object: src.ID,
		Reason: "source " + src.ID + " disabled after permanent failure (exit 3): " + in.Reason, Payload: payload})
	return err
}

// ---- 读取（来源页与入站流水线使用；只读） ----

const eventCols = `id, source_id, dedupe_domain, event_id, type, subject, summary, detail, link, occurred_at,
	correlation_keys, sender, sender_verified, disposition, item, attach_state, unjudged_reason, rejudges,
	metrics, merge_key, items, judgment, messages`

// Events 按落盘顺序返回某来源的来源事件。
func Events(tx *store.Tx, sourceID string) ([]domain.SourceEvent, error) {
	return queryEvents(tx, `WHERE source_id = ?`, sourceID)
}

// Pending 按落盘顺序返回所有还没有处置的来源事件（disposition 为 pending），供入站流水线接手。
func Pending(tx *store.Tx) ([]domain.SourceEvent, error) {
	return queryEvents(tx, `WHERE disposition = ?`, string(domain.DispositionPending))
}

// SetDisposition 在调用方事务内给一条还是 pending 的来源事件写处置结果与（若有）建出的事项；
// d 为 unjudged 时 reason 是未判定的原因（domain.Unjudged*），其余处置忽略 reason。
// 这些列只由入站流水线经本函数与下面的积压函数维护；不记事件，调用方自己负责留痕。
func SetDisposition(tx *store.Tx, id string, d domain.Disposition, item, reason string) error {
	switch d {
	case domain.DispositionAttached, domain.DispositionProposed, domain.DispositionIgnored, domain.DispositionUnjudged,
		domain.DispositionDigest, domain.DispositionAlerted:
	default:
		return domain.NewError(domain.CodeInvalidRequest, "disposition "+string(d)+" cannot be set", "use attached, proposed, ignored, digest, alerted or unjudged")
	}
	if d != domain.DispositionUnjudged {
		reason = ""
	}
	n, err := tx.Exec(`UPDATE sources_runner_event SET disposition = ?, item = ?, unjudged_reason = ? WHERE id = ? AND disposition = ?`,
		string(d), item, reason, id, string(domain.DispositionPending))
	if err == nil && n == 0 {
		return domain.NewError(domain.CodeConflict, "source event "+id+" is not pending", "re-read the source event")
	}
	return err
}

// ---- 未判定积压（§9.3）：unjudged 与 expired 合称积压，expired 不再自动重判 ----

// rejudgeable 是自动重判的对象：因模型不可用而未判定的；原因为空是迁移前留下的，同样按模型不可用对待。
const rejudgeable = `disposition = 'unjudged' AND unjudged_reason IN ('', 'model_unavailable')`

// Requeue 在调用方事务内把一条可重判的 unjudged 事件放回 pending，重判次数加一并返回新次数。不记事件。
func Requeue(tx *store.Tx, id string) (int, error) { return requeue(tx, id, rejudgeable) }

// RequeueOutbound 同 Requeue，对象是因外发范围未选择而未判定的事件；调用方先确认范围已选定（§14.14）。
func RequeueOutbound(tx *store.Tx, id string) (int, error) {
	return requeue(tx, id, `disposition = 'unjudged' AND unjudged_reason = 'outbound_unset'`)
}

func requeue(tx *store.Tx, id, where string) (int, error) {
	n, err := tx.Exec(`UPDATE sources_runner_event SET disposition = 'pending', unjudged_reason = '', rejudges = rejudges + 1
		WHERE id = ? AND `+where, id)
	if err != nil {
		return 0, err
	}
	if n == 0 {
		return 0, domain.NewError(domain.CodeConflict, "source event "+id+" is not unjudged for lack of a model", "re-read the source event")
	}
	ev, err := Event(tx, id)
	return ev.Rejudges, err
}

// Resolve 在调用方事务内结清一条积压事件：to 为 expired 时只接受 unjudged；为 proposed、ignored 时接受 unjudged 与 expired
// （本人处理），proposed 同时关联事项。不记事件，调用方自己负责留痕。
func Resolve(tx *store.Tx, id string, to domain.Disposition, item string) error {
	from := `disposition IN ('unjudged', 'expired')`
	switch to {
	case domain.DispositionExpired:
		from = `disposition = 'unjudged'`
	case domain.DispositionProposed, domain.DispositionIgnored:
	default:
		return domain.NewError(domain.CodeInvalidRequest, "disposition "+string(to)+" cannot settle the backlog", "use proposed, ignored or expired")
	}
	n, err := tx.Exec(`UPDATE sources_runner_event SET disposition = ?, item = ? WHERE id = ? AND `+from, string(to), item, id)
	if err == nil && n == 0 {
		return domain.NewError(domain.CodeConflict, "source event "+id+" is not in the unjudged backlog", "re-read the source event")
	}
	return err
}

// RejudgeCandidates 返回 since 之后发生、可自动重判的 unjudged 事件，至多 limit 条；重判次数少的在前，同次数按落盘顺序，
// 一条总是失败的事件不会一直挡在队首。
func RejudgeCandidates(tx *store.Tx, since time.Time, limit int) ([]domain.SourceEvent, error) {
	as, err := querySeq(tx, `WHERE `+rejudgeable+` AND occurred_at >= ? ORDER BY rejudges, seq LIMIT ?`, since.UnixNano(), limit)
	out := make([]domain.SourceEvent, len(as))
	for i, a := range as {
		out[i] = a.SourceEvent
	}
	return out, err
}

// OutboundUnset 返回 since 之后发生、因外发范围未选择而未判定的事件（选定后由入站模块放回重判），按落盘顺序。
func OutboundUnset(tx *store.Tx, since time.Time) ([]domain.SourceEvent, error) {
	as, err := querySeq(tx, `WHERE disposition = 'unjudged' AND unjudged_reason = 'outbound_unset' AND occurred_at >= ? ORDER BY seq`, since.UnixNano())
	out := make([]domain.SourceEvent, len(as))
	for i, a := range as {
		out[i] = a.SourceEvent
	}
	return out, err
}

// StaleUnjudged 返回 before 之前发生、仍是 unjudged 的事件 id（到期转 expired）。
func StaleUnjudged(tx *store.Tx, before time.Time) ([]string, error) {
	return ids(tx, `WHERE disposition = 'unjudged' AND occurred_at < ? ORDER BY seq`, before.UnixNano())
}

// Backlog 返回积压事件（unjudged 与 expired）的 id；source 非空时只看该来源。
func Backlog(tx *store.Tx, source string) ([]string, error) {
	if source == "" {
		return ids(tx, `WHERE disposition IN ('unjudged', 'expired') ORDER BY seq`)
	}
	return ids(tx, `WHERE disposition IN ('unjudged', 'expired') AND source_id = ? ORDER BY seq`, source)
}

// CountExpired 返回已过期的积压事件数（健康视图只给计数）。
func CountExpired(tx *store.Tx) (int, error) {
	r, err := tx.Query(`SELECT COUNT(*) FROM sources_runner_event WHERE disposition = 'expired'`)
	if err != nil {
		return 0, err
	}
	defer r.Close()
	var n int64
	if r.Next() {
		err = r.Scan(&n)
	}
	return int(n), errors.Join(err, r.Err())
}

func ids(tx *store.Tx, tail string, args ...any) ([]string, error) {
	rows, err := tx.Query(`SELECT id FROM sources_runner_event `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// 挂靠消息状态（§9.4 attach_state）。
const (
	AttachPending    = "pending"
	AttachForwarded  = "forwarded"
	AttachProcessed  = "processed"
	AttachHandedBack = "handed_back"
	AttachReleased   = "released"
)

// attachFrom 是每个目标状态允许的前一状态；"" 表示尚未挂靠。
// "" → processed 是来源对象的更新：内容已直接写回事项，无需转发（§9.4）。
var attachFrom = map[string][]string{
	AttachPending:    {""},
	AttachForwarded:  {AttachPending},
	AttachProcessed:  {AttachForwarded, ""},
	AttachHandedBack: {AttachPending, AttachForwarded},
	AttachReleased:   {AttachPending, AttachForwarded},
}

// SetAttachState 在调用方事务内推进一条来源事件的挂靠状态，只允许 attachFrom 里的转移，否则是冲突。
// 从未挂靠（""）转入时同时把事项记为 disposition=attached；转入 handed_back、released 时把 disposition 放回 pending，
// 让事件重新进入判断流（item 保留，作关联线索）。item 只在从未挂靠转入时使用。
// 这两列只由跟进消息归属经本函数维护；不记事件，调用方自己负责留痕。
func SetAttachState(tx *store.Tx, id, to, item string) error {
	from, ok := attachFrom[to]
	if !ok {
		return domain.NewError(domain.CodeInvalidRequest, "attach state "+to+" cannot be set", "use pending, forwarded, processed, handed_back or released")
	}
	for _, f := range from {
		var (
			n   int64
			err error
		)
		switch {
		case f == "": // 首次挂靠
			n, err = tx.Exec(`UPDATE sources_runner_event SET attach_state = ?, disposition = ?, item = ? WHERE id = ? AND attach_state = ? AND disposition = ?`,
				to, string(domain.DispositionAttached), item, id, f, string(domain.DispositionPending))
		case to == AttachHandedBack || to == AttachReleased:
			n, err = tx.Exec(`UPDATE sources_runner_event SET attach_state = ?, disposition = ? WHERE id = ? AND attach_state = ? AND disposition = ?`,
				to, string(domain.DispositionPending), id, f, string(domain.DispositionAttached))
		default: // forwarded、processed：处置不变
			n, err = tx.Exec(`UPDATE sources_runner_event SET attach_state = ? WHERE id = ? AND attach_state = ? AND disposition = ?`,
				to, id, f, string(domain.DispositionAttached))
		}
		if err != nil || n > 0 {
			return err
		}
	}
	return domain.NewError(domain.CodeConflict, "source event "+id+" cannot move to attach state "+to, "re-read the source event")
}

// Origins 返回去重域 dedupeDomain 里带关联键 key 的来源事件建出的事项（disposition=proposed），按 id 排序、不重复。
// 关联键只在同一去重域内比较：不同来源的同名键不互相命中。
func Origins(tx *store.Tx, dedupeDomain, key string) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT item FROM sources_runner_event
		WHERE dedupe_domain = ? AND disposition = ? AND item != ''
		AND EXISTS (SELECT 1 FROM json_each(correlation_keys) WHERE value = ?) ORDER BY item`,
		dedupeDomain, string(domain.DispositionProposed), key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// Attachment 是挂在某事项上的来源事件，Seq 是它的落盘顺序（跟进消息的确认游标）。
type Attachment struct {
	domain.SourceEvent
	Seq int64
}

// Attachments 按落盘顺序返回挂在事项上、仍是 attached 的来源事件。
func Attachments(tx *store.Tx, item string) ([]Attachment, error) {
	return querySeq(tx, `WHERE disposition = ? AND item = ? ORDER BY seq`, string(domain.DispositionAttached), item)
}

// EventPage 按落盘顺序返回某来源 after 之后的至多 limit 条事件，ds 非空时只看这些处置；还有下一页时 next 是下一次的 after，否则为 0。
func EventPage(tx *store.Tx, sourceID string, ds []domain.Disposition, after int64, limit int) ([]domain.SourceEvent, int64, error) {
	where, args := `WHERE source_id = ? AND seq > ?`, []any{sourceID, after}
	if len(ds) > 0 {
		where += ` AND disposition IN (?` + strings.Repeat(`,?`, len(ds)-1) + `)`
		for _, d := range ds {
			args = append(args, string(d))
		}
	}
	as, err := querySeq(tx, where+` ORDER BY seq LIMIT ?`, append(args, limit+1)...)
	if err != nil {
		return nil, 0, err
	}
	var next int64
	if len(as) > limit {
		next, as = as[limit-1].Seq, as[:limit]
	}
	out := make([]domain.SourceEvent, len(as))
	for i, a := range as {
		out[i] = a.SourceEvent
	}
	return out, next, nil
}

func querySeq(tx *store.Tx, tail string, args ...any) ([]Attachment, error) {
	rows, err := tx.Query(`SELECT seq, `+eventCols+` FROM sources_runner_event `+tail, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attachment
	for rows.Next() {
		var (
			a                      Attachment
			at, verified, rejudges int64
			keys, disp             string
			x                      extraCols
		)
		e := &a.SourceEvent
		if err := rows.Scan(&a.Seq, &e.ID, &e.SourceID, &e.DedupeKey.Domain, &e.DedupeKey.EventID, &e.Type, &e.Subject, &e.Summary,
			&e.Detail, &e.Link, &at, &keys, &e.Sender, &verified, &disp, &e.Item, &e.AttachState, &e.UnjudgedReason, &rejudges, &x.metrics, &e.MergeKey, &x.items, &e.Judgment, &x.messages); err != nil {
			return nil, err
		}
		if err := x.fill(e); err != nil {
			return nil, err
		}
		e.OccurredAt, e.SenderVerified, e.Disposition = time.Unix(0, at).UTC(), verified != 0, domain.Disposition(disp)
		e.Rejudges = int(rejudges)
		if err := json.Unmarshal([]byte(keys), &e.CorrelationKeys); err != nil {
			return nil, fmt.Errorf("runner: event %s correlation_keys: %w", e.ID, err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// extraCols 是 v2 新增、需要解码的 JSON 列（空串表示没有）。
type extraCols struct{ metrics, items, messages string }

func (x extraCols) fill(e *domain.SourceEvent) error {
	for _, c := range []struct {
		raw, name string
		dst       any
	}{{x.metrics, "metrics", &e.Metrics}, {x.items, "items", &e.Items}, {x.messages, "messages", &e.Messages}} {
		if c.raw == "" {
			continue
		}
		if err := json.Unmarshal([]byte(c.raw), c.dst); err != nil {
			return fmt.Errorf("runner: event %s %s: %w", e.ID, c.name, err)
		}
	}
	return nil
}

// Event 按 id 读一条来源事件。
func Event(tx *store.Tx, id string) (domain.SourceEvent, error) {
	es, err := queryEvents(tx, `WHERE id = ?`, id)
	if err != nil {
		return domain.SourceEvent{}, err
	}
	if len(es) == 0 {
		return domain.SourceEvent{}, domain.NewError(domain.CodeNotFound, "source event "+id+" not found", "list the source's events or the item's attached messages")
	}
	return es[0], nil
}

func queryEvents(tx *store.Tx, where string, args ...any) ([]domain.SourceEvent, error) {
	rows, err := tx.Query(`SELECT `+eventCols+` FROM sources_runner_event `+where+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []domain.SourceEvent
	for rows.Next() {
		var (
			e                      domain.SourceEvent
			at, verified, rejudges int64
			keys, disp             string
			x                      extraCols
		)
		if err := rows.Scan(&e.ID, &e.SourceID, &e.DedupeKey.Domain, &e.DedupeKey.EventID, &e.Type, &e.Subject, &e.Summary,
			&e.Detail, &e.Link, &at, &keys, &e.Sender, &verified, &disp, &e.Item, &e.AttachState, &e.UnjudgedReason, &rejudges, &x.metrics, &e.MergeKey, &x.items, &e.Judgment, &x.messages); err != nil {
			return nil, err
		}
		if err := x.fill(&e); err != nil {
			return nil, err
		}
		e.OccurredAt, e.SenderVerified, e.Disposition = time.Unix(0, at).UTC(), verified != 0, domain.Disposition(disp)
		e.Rejudges = int(rejudges)
		if err := json.Unmarshal([]byte(keys), &e.CorrelationKeys); err != nil {
			return nil, fmt.Errorf("runner: event %s correlation_keys: %w", e.ID, err)
		}
		if len(e.CorrelationKeys) == 0 {
			e.CorrelationKeys = nil
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// Rejects 按发生顺序返回某来源没有入账的输出行及原因。
func Rejects(tx *store.Tx, sourceID string) ([]Reject, error) {
	out, _, err := RejectPage(tx, sourceID, 0, math.MaxInt32)
	return out, err
}

// RejectPage 按发生顺序返回某来源 after 之后的至多 limit 条拒收记录；还有下一页时 next 是下一次的 after，否则为 0（与 EventPage 同一约定）。
func RejectPage(tx *store.Tx, sourceID string, after int64, limit int) ([]Reject, int64, error) {
	rows, err := tx.Query(`SELECT seq, source_id, run_id, line, reason, raw, at FROM sources_runner_reject WHERE source_id = ? AND seq > ? ORDER BY seq LIMIT ?`,
		sourceID, after, limit+1)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var (
		out  []Reject
		seqs []int64
	)
	for rows.Next() {
		var (
			j             Reject
			seq, line, at int64
		)
		if err := rows.Scan(&seq, &j.SourceID, &j.RunID, &line, &j.Reason, &j.Raw, &at); err != nil {
			return nil, 0, err
		}
		j.Line, j.At = int(line), time.Unix(0, at).UTC()
		out, seqs = append(out, j), append(seqs, seq)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	var next int64
	if len(out) > limit {
		next, out = seqs[limit-1], out[:limit]
	}
	return out, next, nil
}

// Unjudged 按落盘顺序返回判断型初筛未能判定的来源事件（disposition 为 unjudged）；健康视图只读。
func Unjudged(tx *store.Tx) ([]domain.SourceEvent, error) {
	return queryEvents(tx, `WHERE disposition = ?`, string(domain.DispositionUnjudged))
}
