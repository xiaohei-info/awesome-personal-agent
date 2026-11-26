package runner

import (
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 来源处理线的队列状态（§9.3） ----
// judge 判"处理"或规则直通的事件进待处理；取批与运行入队在同一事务里转处理中；运行成功转已处理，失败放回待处理并计次。
// 队列只存在事件表的四列里，不另建批次表；这一批的记录是运行记录本身。不记事件，调用方自己负责留痕。

// 队列状态与等级（domain.SourceEvent.Queue、QueueLevel）。
const (
	QueueQueued  = "queued"
	QueueRunning = "running"
	QueueDone    = "done"

	LevelUrgent = "urgent"
	LevelNormal = "normal"
)

// Enqueue 把一条 pending、不在队列里的事件放进待处理。
func Enqueue(tx *store.Tx, id, level string) error {
	if level != LevelUrgent && level != LevelNormal {
		return domain.NewError(domain.CodeInvalidRequest, "queue level "+level+" is unknown", "use urgent or normal")
	}
	n, err := tx.Exec(`UPDATE sources_runner_event SET queue_state = ?, queue_level = ?, queue_run = '', queue_tries = 0
		WHERE id = ? AND disposition = ? AND queue_state = ''`, QueueQueued, level, id, string(domain.DispositionPending))
	if err == nil && n == 0 {
		return domain.NewError(domain.CodeConflict, "source event "+id+" is not pending outside the queue", "re-read the source event")
	}
	return err
}

// QueueStat 是待处理的条数（按等级）与最早一条普通事件的入账时刻（攒批的等待从入账算起）。
type QueueStat struct {
	Urgent, Normal int
	Oldest         time.Time
}

// Queued 读待处理的统计。
func Queued(tx *store.Tx) (QueueStat, error) {
	var st QueueStat
	var u, n, oldest int64
	r, err := tx.Query(`SELECT
		COALESCE(SUM(CASE WHEN queue_level = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN queue_level <> ? THEN 1 ELSE 0 END), 0),
		COALESCE(MIN(CASE WHEN queue_level <> ? THEN COALESCE(ingested_at, occurred_at) END), 0)
		FROM sources_runner_event WHERE queue_state = ?`, LevelUrgent, LevelUrgent, LevelUrgent, QueueQueued)
	if err != nil {
		return st, err
	}
	defer r.Close()
	if r.Next() {
		if err := r.Scan(&u, &n, &oldest); err != nil {
			return st, err
		}
	}
	st.Urgent, st.Normal = int(u), int(n)
	if oldest > 0 {
		st.Oldest = time.Unix(0, oldest)
	}
	return st, r.Err()
}

// Claim 在调用方事务内取至多 limit 条待处理事件转处理中并记下运行 id：立即的在前，同等级按落盘顺序。
// 调用方在同一事务里把运行入队，事务串行化保证同一事件只被一批取到。
func Claim(tx *store.Tx, run string, limit int) ([]domain.SourceEvent, error) {
	evs, err := queryEvents(tx, `WHERE id IN (SELECT id FROM sources_runner_event WHERE queue_state = ?
		ORDER BY CASE WHEN queue_level = ? THEN 0 ELSE 1 END, seq LIMIT ?)`, QueueQueued, LevelUrgent, limit)
	if err != nil {
		return nil, err
	}
	for _, e := range evs {
		if _, err := tx.Exec(`UPDATE sources_runner_event SET queue_state = ?, queue_run = ? WHERE id = ? AND queue_state = ?`,
			QueueRunning, run, e.ID, QueueQueued); err != nil {
			return nil, err
		}
	}
	return evs, nil
}

// InRun 按落盘顺序返回某次运行取走的事件（处理中或已处理）。
func InRun(tx *store.Tx, run string) ([]domain.SourceEvent, error) {
	return queryEvents(tx, `WHERE queue_run = ? AND queue_state IN (?, ?)`, run, QueueRunning, QueueDone)
}

// Finished 是一次运行收尾的结果：Requeued 放回待处理，Dropped 到了重试上限、转入积压（unjudged，原因 process_failed）。
type Finished struct {
	Done, Requeued, Dropped []string
}

// Finish 在调用方事务内收尾运行 run 取走的事件：ok 为真全部转已处理；为假时已经有了去向（不再是 pending）的转已处理，
// 其余计次放回待处理，计次超过 maxTries 的离开队列、转入积压。
func Finish(tx *store.Tx, run string, ok bool, maxTries int) (Finished, error) {
	var f Finished
	r, err := tx.Query(`SELECT id, disposition, queue_tries FROM sources_runner_event WHERE queue_run = ? AND queue_state = ? ORDER BY seq`, run, QueueRunning)
	if err != nil {
		return f, err
	}
	type row struct {
		id, disp string
		tries    int64
	}
	var rows []row
	for r.Next() {
		var x row
		if err := r.Scan(&x.id, &x.disp, &x.tries); err != nil {
			r.Close()
			return f, err
		}
		rows = append(rows, x)
	}
	if err := r.Close(); err != nil {
		return f, err
	}
	for _, x := range rows {
		var q string
		var args []any
		switch {
		case ok || x.disp != string(domain.DispositionPending):
			q, args = `UPDATE sources_runner_event SET queue_state = ? WHERE id = ?`, []any{QueueDone, x.id}
			f.Done = append(f.Done, x.id)
		case int(x.tries)+1 <= maxTries:
			q, args = `UPDATE sources_runner_event SET queue_state = ?, queue_run = '', queue_tries = queue_tries + 1 WHERE id = ?`, []any{QueueQueued, x.id}
			f.Requeued = append(f.Requeued, x.id)
		default:
			q = `UPDATE sources_runner_event SET queue_state = '', queue_run = '', queue_tries = queue_tries + 1,
				disposition = ?, unjudged_reason = ? WHERE id = ?`
			args = []any{string(domain.DispositionUnjudged), domain.UnjudgedProcessFailed, x.id}
			f.Dropped = append(f.Dropped, x.id)
		}
		if _, err := tx.Exec(q, args...); err != nil {
			return f, err
		}
	}
	return f, nil
}

// Running 报告事件是否正被一次批处理运行处理（处理中）：事件去向命令接受这种 pending 事件（§9.3）。
func Running(ev domain.SourceEvent) bool {
	return ev.Disposition == domain.DispositionPending && ev.Queue == QueueRunning
}
