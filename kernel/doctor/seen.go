package doctor

import (
	"context"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移：只有一张"最近一次见到"的记录表，是自检报告"最近领取或读取的时间""插件最近一次调用内核的时间"的来源。
var Module = store.Module{Name: "doctor", Migrations: []store.Migration{{
	Version: 1, Ledger: 3, Name: "ledger-v3-doctor-seen",
	SQL: `CREATE TABLE doctor_seen (
	subject TEXT PRIMARY KEY,
	at INTEGER NOT NULL
) WITHOUT ROWID;`,
}}}

// 记录对象的前缀：ext 密钥是 "ext:<scope>"，宿主插件是 "harness:<名>"。
const (
	SubjectExt     = "ext:"
	SubjectHarness = "harness:"
)

// Seen 记录对象最近一次经认证调用内核的时间。同一对象在 Every 内只写一次账本，高频长轮询不放大写入。
// 记录失败不影响请求：它只服务自检报告。
type Seen struct {
	Every time.Duration

	mu   sync.Mutex
	last map[string]time.Time
}

// NewSeen 返回记录器；every 为零取 5 秒。
func NewSeen(every time.Duration) *Seen {
	if every <= 0 {
		every = 5 * time.Second
	}
	return &Seen{Every: every, last: map[string]time.Time{}}
}

// Note 记下 subject 在 now 被见到。
func (s *Seen) Note(ctx context.Context, st *store.Store, subject string, now time.Time) error {
	s.mu.Lock()
	if prev, ok := s.last[subject]; ok && now.Sub(prev) < s.Every {
		s.mu.Unlock()
		return nil
	}
	s.last[subject] = now
	s.mu.Unlock()
	return st.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO doctor_seen (subject, at) VALUES (?, ?)
			ON CONFLICT(subject) DO UPDATE SET at = excluded.at WHERE excluded.at > at`, subject, now.UnixNano())
		return err
	})
}

func readSeen(tx *store.Tx) (map[string]time.Time, error) {
	r, err := tx.Query("SELECT subject, at FROM doctor_seen")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]time.Time{}
	for r.Next() {
		var sub string
		var at int64
		if err := r.Scan(&sub, &at); err != nil {
			return nil, err
		}
		out[sub] = time.Unix(0, at).UTC()
	}
	return out, r.Err()
}
