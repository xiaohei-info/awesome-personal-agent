package direct

import (
	"encoding/json"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Scope 是 proposal_outcome 的统计范围：同一来源、kind、项目；空字段表示该维度不限。
type Scope struct {
	Source  string
	Kind    string
	Project string
}

// Stats 是建议被本人接受与丢弃的次数。
type Stats struct {
	Accepted   int `json:"accepted"`
	Dismissed  int `json:"dismissed"`
	WindowDays int `json:"window_days"`
}

// proposalStats 从已有 Event 汇总（账本不为统计新增表或字段，§8.4）。只计本人的决定：
// proposal.accept / proposal.dismiss 在 Web 上或带证据的代答（resolved_via 为 web、chat）；
// 规则自动接受（没有 resolved_via）、决策超时与内容已成墓碑的 Event 不计入。
// shortcut: 每次调用扫一遍窗口内的 item.phase_changed；Event 量大到拖慢判断时，加按类型与时间的索引读取。
func proposalStats(tx *store.Tx, sc Scope, now time.Time, window time.Duration) (Stats, error) {
	st := Stats{WindowDays: int(window / (24 * time.Hour))}
	since := now.Add(-window)
	var after int64
	for {
		page, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvItemPhaseChanged}, after, 500)
		if err != nil {
			return st, err
		}
		for _, en := range page {
			after = en.Event.Seq
			if en.Tombstone || en.Event.At.Before(since) {
				continue
			}
			if via := en.Event.ResolvedVia; via != decisions.ViaWeb && via != decisions.ViaChat {
				continue
			}
			var p struct {
				Before *struct {
					Phase domain.Phase `json:"phase"`
				} `json:"before"`
				Item struct {
					Phase   domain.Phase   `json:"phase"`
					Kind    domain.Kind    `json:"kind"`
					Project string         `json:"project"`
					Origin  *domain.Origin `json:"origin"`
				} `json:"item"`
			}
			if json.Unmarshal(en.Event.Payload, &p) != nil || p.Before == nil || p.Before.Phase != domain.PhaseProposed {
				continue
			}
			source := ""
			if p.Item.Origin != nil {
				source = p.Item.Origin.Source
			}
			if (sc.Source != "" && sc.Source != source) || (sc.Kind != "" && sc.Kind != string(p.Item.Kind)) ||
				(sc.Project != "" && sc.Project != p.Item.Project) {
				continue
			}
			switch p.Item.Phase {
			case domain.PhaseOpen:
				st.Accepted++
			case domain.PhaseDropped:
				st.Dismissed++
			}
		}
		if len(page) < 500 {
			return st, nil
		}
	}
}
