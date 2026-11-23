package attention

import (
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// RowCount 是矩阵一行自 from 起的投递计数（设置页各单选旁的预览与只读总览，§3.3、§5）。
// Events 是不同的提醒条数（两层各一条投递只算一条）；Pushed 是真推出去的投递条数（经渠道或进对话，
// 不含只留今天页的、被暂停或日上限压下的），所以两层都推的一条提醒算 2。
type RowCount struct {
	Events int `json:"events"`
	Pushed int `json:"pushed"`
}

// RowCounts 按与选去处同一个 rowOf 把 from 起的投递归到行（行键取 rowOf 的第一个键：定时任务的产出归它的实例行）。
// 合并摘要（digest:）不属于任何一行，不计。只读。
func RowCounts(tx *store.Tx, from time.Time) (map[string]RowCount, error) {
	sent, err := delivery.Since(tx, from)
	if err != nil {
		return nil, err
	}
	out := map[string]RowCount{}
	seen := map[string]bool{}
	for _, s := range sent {
		if strings.HasPrefix(s.ID, "digest:") {
			continue
		}
		base := strings.TrimSuffix(s.ID, AgentSuffix)
		r, err := rowOf(tx, held{id: base, item: s.Item})
		if err != nil {
			return nil, err
		}
		if len(r.keys) == 0 {
			continue
		}
		c := out[r.keys[0]]
		if !seen[base] {
			seen[base] = true
			c.Events++
		}
		if s.Route != delivery.RouteWeb && s.Suppressed == "" {
			c.Pushed++
		}
		out[r.keys[0]] = c
	}
	return out, nil
}
