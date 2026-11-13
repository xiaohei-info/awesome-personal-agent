package summaries

import (
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// DigestEntry 是简报"知会"一节的一条：摘要出口（then: digest）的来源事件。不建事项、不提醒、不调模型（设计 §9.3、§3.12）。
type DigestEntry struct {
	Event   string `json:"event"`
	Source  string `json:"source"`
	Title   string `json:"title"`
	Summary string `json:"summary,omitempty"`
	Link    string `json:"link,omitempty"`
}

// PendingDigest 按落盘顺序返回处置仍是 digest、且还没汇入过任何简报的来源事件。
// 被转为建议的事件处置已变，自然不再出现；事件仍留在来源页。
func PendingDigest(tx *store.Tx) ([]DigestEntry, error) {
	evs, err := runner.ByDisposition(tx, domain.DispositionDigest)
	if err != nil {
		return nil, err
	}
	done := map[string]bool{}
	r, err := tx.Query(`SELECT event FROM summaries_digested`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for r.Next() {
		var id string
		if err := r.Scan(&id); err != nil {
			return nil, err
		}
		done[id] = true
	}
	if err := r.Err(); err != nil {
		return nil, err
	}
	out := []DigestEntry{}
	for _, e := range evs {
		if !done[e.ID] {
			out = append(out, DigestEntry{Event: e.ID, Source: e.SourceID, Title: e.Subject, Summary: e.Summary, Link: e.Link})
		}
	}
	return out, nil
}

// MarkDigested 记下这些事件已汇入 brief 事项的简报；重复记同一条是空操作，"只汇入一次"以第一次为准。
func MarkDigested(tx *store.Tx, brief string, es []DigestEntry, at time.Time) error {
	for _, e := range es {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO summaries_digested (event, brief, at) VALUES (?, ?, ?)`, e.Event, brief, at.UnixNano()); err != nil {
			return err
		}
	}
	return nil
}
