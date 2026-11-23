package inbound

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ---- 忽略记录与按批撤销（§7.2） ----

// Dismissal 是一条建议当前这次忽略的记录：哪一批（逐条忽略的没有）、理由、时刻、操作者。
type Dismissal struct {
	Batch  string       `json:"batch,omitempty"`
	Reason string       `json:"reason,omitempty"`
	At     time.Time    `json:"at"`
	Actor  domain.Actor `json:"actor"`
}

// Dismissals 给出 dropped 事项里"最近一次阶段变更是忽略建议"的那些的忽略记录；被合并折叠、放弃等转入 dropped 的不在其中。
// 之后又改过阶段的（重新打开后再放弃）以最近一次为准，所以按批撤销不会撤到别的操作。只读。
// shortcut: 逐条读事项的事件，已忽略的到数千条时改为在 items 上记最近一次阶段变更的 seq。
func Dismissals(tx *store.Tx, its []domain.Item) (map[string]Dismissal, error) {
	out := map[string]Dismissal{}
	for _, it := range its {
		if it.Phase != domain.PhaseDropped {
			continue
		}
		es, err := eventlog.ReadObject(tx, it.ID)
		if err != nil {
			return nil, err
		}
		for i := len(es) - 1; i >= 0; i-- {
			e := es[i].Event
			if e.Type != domain.EvItemPhaseChanged || es[i].Tombstone {
				continue
			}
			var p struct {
				Op    string `json:"op"`
				Batch string `json:"batch"`
			}
			if json.Unmarshal(e.Payload, &p) == nil && p.Op == "proposal.dismiss" {
				out[it.ID] = Dismissal{Batch: p.Batch, Reason: e.Reason, At: e.At, Actor: e.Actor}
			}
			break
		}
	}
	return out, nil
}

// newBatch 是一次批量处理的批次号：带幂等键时由键决定（重放得到同一批次、同一输入），否则按时刻与首条取。
func newBatch(req command.Request, first string, now time.Time) string {
	seed := "key|" + req.IdemKey
	if req.IdemKey == "" {
		seed = "at|" + strconv.FormatInt(now.UnixNano(), 10) + "|" + first
	}
	h := sha256.Sum256([]byte(seed))
	return "pb_" + hex.EncodeToString(h[:])[:12]
}
