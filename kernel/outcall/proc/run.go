// Package proc 是外呼调用器的子进程部分（设计 §7.7、§6.11）：
// Run 的写入接口（在调用方事务内入队）、长时/短任务两个队列的额度与优先级、
// 白名单环境、超时杀进程组，以及重启恢复。
// 本包不发通知也不发事件，结果只交回调用模块（Options.OnResult）。
package proc

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Queue 是子进程额度的两个互不挤占的队列（§7.7）。
type Queue string

const (
	QueueLong  Queue = "long"  // task、background，受 max_subprocesses 限制
	QueueShort Queue = "short" // source、transform、reader，受 max_short_subprocesses 限制
	// QueueExternal 是进程不在本机的 Run（唤醒真实主会话，§13.6）：不占额度、调度器不拉起，写入即 running；
	// 由调用模块按回执 Settle，到期由调度器按超时落账，重启恢复不判失败。
	QueueExternal Queue = "external"
)

// QueueOf 返回类别所属队列；judge、generate 走 HTTP，不起子进程，ok 为 false。
func QueueOf(c domain.RunCategory) (q Queue, ok bool) {
	switch c {
	case domain.RunTask, domain.RunBackground:
		return QueueLong, true
	case domain.RunSource, domain.RunTransform, domain.RunReader:
		return QueueShort, true
	}
	return "", false
}

// Module 是本模块在存储层登记的迁移。命令规格随 Run 落账本，重启后 queued 的 Run 才能原样重新执行；
// 运行凭据等秘密不入账本，由 Options.Env 在启动时注入。
var Module = store.Module{Name: "outcall_proc", Migrations: []store.Migration{{
	Version: 1, Name: "create-run",
	SQL: `CREATE TABLE outcall_run (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	id TEXT NOT NULL UNIQUE,
	queue TEXT NOT NULL,
	category TEXT NOT NULL,
	runner TEXT NOT NULL,
	launch TEXT NOT NULL,
	target TEXT NOT NULL,
	harness TEXT NOT NULL,
	session_id TEXT NOT NULL,
	access TEXT NOT NULL,
	basis TEXT NOT NULL,
	chain_depth INTEGER NOT NULL,
	state TEXT NOT NULL,
	exit_code INTEGER,
	started_at INTEGER NOT NULL DEFAULT 0,
	finished_at INTEGER NOT NULL DEFAULT 0,
	timeout INTEGER NOT NULL,
	fingerprint TEXT NOT NULL,
	spec TEXT NOT NULL,
	explicit INTEGER NOT NULL,
	enqueued_at INTEGER NOT NULL,
	pgid INTEGER NOT NULL DEFAULT 0,
	reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX outcall_run_state ON outcall_run(queue, state, explicit, seq);
CREATE INDEX outcall_run_target ON outcall_run(target, state);`,
}, {
	// 账本结构版本 3：判断记录（§6.16）。不含资料正文；levels 是逐级结果的 JSON；
	// review 为空表示未抽检，否则 should_see 或 correct。物理删除来源事件或事项时按 subject 一并删除。
	Version: 2, Ledger: 3, Name: "ledger-v3-judgment",
	SQL: `CREATE TABLE outcall_judgment (
	id TEXT PRIMARY KEY,
	use TEXT NOT NULL,
	template TEXT NOT NULL,
	subject TEXT NOT NULL,
	source TEXT NOT NULL DEFAULT '',
	fingerprint TEXT NOT NULL,
	band TEXT NOT NULL,
	levels TEXT NOT NULL,
	decided_by TEXT NOT NULL,
	answer TEXT NOT NULL,
	low_confidence INTEGER NOT NULL DEFAULT 0,
	shadow INTEGER NOT NULL DEFAULT 0,
	review TEXT NOT NULL DEFAULT '',
	reviewed_at INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX outcall_judgment_subject ON outcall_judgment(subject);
CREATE INDEX outcall_judgment_use ON outcall_judgment(use, created_at);
CREATE INDEX outcall_judgment_source ON outcall_judgment(source, created_at) WHERE source <> '';`,
}}}

// Spec 是子进程的统一契约："命令 + stdin JSON → 退出码 + stdout"。
type Spec struct {
	Command []string        `json:"command"` // argv，Command[0] 是可执行文件
	Stdin   json.RawMessage `json:"stdin"`   // 必须是合法 JSON；空则传 {}
	Dir     string          `json:"dir"`     // 工作目录：运行目录，或模板声明的项目检出（projects.WorkDir）
	// RunDir 是 agent 运行的运行目录，非空才写运行上下文文件（§7.7）。
	RunDir string `json:"run_dir,omitempty"`
	// ContextName 是运行上下文文件名，默认运行 id。续接同一会话时沿用上一次运行的（Record.ContextName）：
	// harness 在建会话时冻结客户端环境，续接后工具侧拿到的还是原来那条路径。
	ContextName string `json:"context_name,omitempty"`
}

// Request 是入队请求。Run.ID 为空时自动生成；State 总是被置为 queued。
type Request struct {
	Run      domain.Run
	Spec     Spec
	Explicit bool // 本人显式触发：在所在队列内优先
	External bool // 进程不在本机（QueueExternal）：不需要 Spec.Command
}

// Record 是账本里的一条 Run 及其执行附注。
type Record struct {
	Run      domain.Run
	Queue    Queue
	Spec     Spec
	Explicit bool
	Reason   string // 失败、超时、恢复的原因；成功为空
	PGID     int    // 运行中子进程的进程组，0 表示尚未拉起
}

// ContextName 返回这条 Run 的运行上下文文件名。
func (r Record) ContextName() string {
	if r.Spec.ContextName != "" {
		return r.Spec.ContextName
	}
	return r.Run.ID
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the run request")
}

// inFlightGuarded 为真的类别遵守"同一目标同时只有一个排队或运行中的 Run"：
// 事项的任务型/后台运行（不变量 7）与来源拉取（定时到期不重复入队）。
func inFlightGuarded(c domain.RunCategory) bool {
	return c == domain.RunTask || c == domain.RunBackground || c == domain.RunSource
}

func newID() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "run_" + hex.EncodeToString(b[:]), nil
}

// Enqueue 在调用方事务内写入一条 queued 的 Run（与投影、Event 一起提交；提交后才会被执行）。
// 同一目标已有排队或运行中的 Run（带目标事项的排队运行算在途）时返回 conflict，事务内不写任何东西。
func Enqueue(tx *store.Tx, req Request, now time.Time) (domain.Run, error) {
	r := req.Run
	q, ok := QueueOf(r.Category)
	if req.External {
		q = QueueExternal
	}
	switch {
	case !ok:
		return domain.Run{}, invalid(fmt.Sprintf("category %q does not start a subprocess", r.Category))
	case !req.External && (len(req.Spec.Command) == 0 || req.Spec.Command[0] == ""):
		return domain.Run{}, invalid("command is empty")
	case r.TimeoutSec <= 0:
		return domain.Run{}, invalid("timeout must be positive")
	case r.ChainDepth < 0:
		return domain.Run{}, invalid("chain_depth < 0")
	case len(req.Spec.Stdin) > 0 && !json.Valid(req.Spec.Stdin):
		return domain.Run{}, invalid("stdin is not valid JSON")
	}
	if inFlightGuarded(r.Category) && r.Target != "" {
		busy, err := InFlight(tx, r.Target)
		if err != nil {
			return domain.Run{}, err
		}
		if busy {
			return domain.Run{}, domain.NewError(domain.CodeConflict, "target already has a queued or running run", "wait for the current run to finish")
		}
	}
	if r.ID == "" {
		id, err := newID()
		if err != nil {
			return domain.Run{}, err
		}
		r.ID = id
	}
	r.State, r.ExitCode, r.StartedAt, r.FinishedAt = domain.RunQueued, nil, nil, nil
	var started int64
	if req.External {
		r.State, r.StartedAt, started = domain.RunRunning, &now, now.UnixNano()
	}
	spec := req.Spec
	if len(spec.Stdin) == 0 {
		spec.Stdin = json.RawMessage(`{}`)
	}
	specJSON, _ := json.Marshal(spec)
	basis, _ := json.Marshal(r.Basis)
	_, err := tx.Exec(`INSERT INTO outcall_run (id, queue, category, runner, launch, target, harness, session_id, access,
		basis, chain_depth, state, timeout, fingerprint, spec, explicit, enqueued_at, started_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, string(q), string(r.Category), r.Runner, r.Launch, r.Target, r.Harness, r.SessionID, r.Access,
		string(basis), int64(r.ChainDepth), string(r.State), int64(r.TimeoutSec), r.Fingerprint,
		string(specJSON), b2i(req.Explicit), now.UnixNano(), started)
	if err != nil {
		return domain.Run{}, err
	}
	return r, nil
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

const cols = `id, queue, category, runner, launch, target, harness, session_id, access, basis, chain_depth, state,
	exit_code, started_at, finished_at, timeout, fingerprint, spec, explicit, pgid, reason`

func scan(rows *store.Rows) (Record, error) {
	var (
		rec                                 Record
		id, queue, cat, runner, launch, tgt string
		harness, sid, access, basis, state  string
		depth, started, finished, timeout   int64
		fp, spec, reason                    string
		explicit, pgid                      int64
		exit                                any
	)
	if err := rows.Scan(&id, &queue, &cat, &runner, &launch, &tgt, &harness, &sid, &access, &basis, &depth, &state,
		&exit, &started, &finished, &timeout, &fp, &spec, &explicit, &pgid, &reason); err != nil {
		return rec, err
	}
	r := domain.Run{ID: id, Category: domain.RunCategory(cat), Runner: runner, Launch: launch, Target: tgt,
		Harness: harness, SessionID: sid, Access: access, ChainDepth: int(depth), State: domain.RunState(state),
		TimeoutSec: int(timeout), Fingerprint: fp}
	if err := json.Unmarshal([]byte(basis), &r.Basis); err != nil {
		return rec, fmt.Errorf("proc: run %s basis: %w", id, err)
	}
	if err := json.Unmarshal([]byte(spec), &rec.Spec); err != nil {
		return rec, fmt.Errorf("proc: run %s spec: %w", id, err)
	}
	if n, ok := exit.(int64); ok {
		c := int(n)
		r.ExitCode = &c
	}
	if started != 0 {
		t := time.Unix(0, started)
		r.StartedAt = &t
	}
	if finished != 0 {
		t := time.Unix(0, finished)
		r.FinishedAt = &t
	}
	rec.Run, rec.Queue, rec.Explicit, rec.PGID, rec.Reason = r, Queue(queue), explicit != 0, int(pgid), reason
	return rec, nil
}

func query(tx *store.Tx, where string, args ...any) ([]Record, error) {
	rows, err := tx.Query("SELECT "+cols+" FROM outcall_run "+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Record
	for rows.Next() {
		rec, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// Get 按 id 取 Run；不存在返回 not_found。
func Get(tx *store.Tx, id string) (Record, error) {
	recs, err := query(tx, "WHERE id = ?", id)
	if err != nil {
		return Record{}, err
	}
	if len(recs) == 0 {
		return Record{}, domain.NewError(domain.CodeNotFound, "run not found", "list runs to find the id")
	}
	return recs[0], nil
}

// List 按入队顺序返回全部 Run（`apa run list` 的依据）。
func List(tx *store.Tx) ([]Record, error) { return query(tx, "ORDER BY seq") }

// InFlight 报告目标是否有排队或运行中的 Run；带目标事项的排队运行算在途。
func InFlight(tx *store.Tx, target string) (bool, error) {
	recs, err := query(tx, "WHERE target = ? AND state IN ('queued','running') LIMIT 1", target)
	return len(recs) > 0, err
}

// External 返回运行中的外部 Run（QueueExternal），供调用模块按回执收尾。
func External(tx *store.Tx) ([]Record, error) {
	return query(tx, "WHERE queue = ? AND state = 'running' ORDER BY seq", string(QueueExternal))
}

// HasRun 报告目标是否有过任何 Run（含已结束的）。
func HasRun(tx *store.Tx, target string) (bool, error) {
	recs, err := query(tx, "WHERE target = ? LIMIT 1", target)
	return len(recs) > 0, err
}

// LastSuccess 返回某类别、某目标最近一次成功的结束时间；没有则 ok 为 false。
func LastSuccess(tx *store.Tx, c domain.RunCategory, target string) (t time.Time, ok bool, err error) {
	recs, err := query(tx, "WHERE category = ? AND target = ? AND state = 'succeeded' ORDER BY finished_at DESC LIMIT 1", string(c), target)
	if err != nil || len(recs) == 0 || recs[0].Run.FinishedAt == nil {
		return time.Time{}, false, err
	}
	return *recs[0].Run.FinishedAt, true, nil
}

// QueueStat 是健康视图要的单个队列统计：运行数、排队数、排队中最长等待。
type QueueStat struct {
	Running, Queued int
	LongestWait     time.Duration
}

// Stats 汇总两个队列。
func Stats(tx *store.Tx, now time.Time) (map[Queue]QueueStat, error) {
	out := map[Queue]QueueStat{QueueLong: {}, QueueShort: {}}
	rows, err := tx.Query(`SELECT queue, state, COUNT(*), MIN(enqueued_at) FROM outcall_run
		WHERE state IN ('queued','running') GROUP BY queue, state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var q, st string
		var n, oldest int64
		if err := rows.Scan(&q, &st, &n, &oldest); err != nil {
			return nil, err
		}
		s := out[Queue(q)]
		if st == string(domain.RunRunning) {
			s.Running = int(n)
		} else {
			s.Queued, s.LongestWait = int(n), now.Sub(time.Unix(0, oldest))
		}
		out[Queue(q)] = s
	}
	return out, rows.Err()
}

// Settle 在调用方事务内把 Run 推进到终态，转移必须合法（queued→failed，running→终态）。
// exit 为 nil 表示没有退出码（拉起失败、恢复时进程已不在）。
func Settle(tx *store.Tx, id string, state domain.RunState, exit *int, reason string, now time.Time) error {
	rec, err := Get(tx, id)
	if err != nil {
		return err
	}
	if !state.Terminal() || !rec.Run.State.CanTransition(state) {
		return domain.NewError(domain.CodeConflict, fmt.Sprintf("run is %s, cannot become %s", rec.Run.State, state), "runs only move queued → running → terminal")
	}
	var ec any
	if exit != nil {
		ec = int64(*exit)
	}
	_, err = tx.Exec("UPDATE outcall_run SET state = ?, exit_code = ?, finished_at = ?, reason = ?, pgid = 0 WHERE id = ?",
		string(state), ec, now.UnixNano(), reason, id)
	return err
}
