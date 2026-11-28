package why

// 讨论里一条发言 @ 了谁、各自到哪一步了（§6.3）：读由它建出的委派子事项、委派记录与唤醒投递，都是现成数据，不另存。

import (
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// @ 的状态（稳定取值，Web 按词表说明）。执行者：running 在跑（含排队）、replied 已回复（子事项完成）、blocked 卡住或没交回结果、
// stopped 不做了（子事项放弃）、missing 子事项已不在；主会话：sent 已发出、delivered 已送达、not_delivered 没送到（留 Web 未读）、withdrawn 已作废。
const (
	MentionRunning      = "running"
	MentionReplied      = "replied"
	MentionBlocked      = "blocked"
	MentionStopped      = "stopped"
	MentionMissing      = "missing"
	MentionSent         = "sent"
	MentionDelivered    = "delivered"
	MentionNotDelivered = "not_delivered"
	MentionWithdrawn    = "withdrawn"
)

// Mention 是发言里的一个 @：目标（执行者 id 或主会话）、种类、状态，执行者另带由它建出的子事项与最近一次运行。
type Mention struct {
	Target string `json:"target"`
	Kind   string `json:"kind"` // executor | main
	State  string `json:"state"`
	Item   string `json:"item,omitempty"`
	Run    string `json:"run,omitempty"`
}

// mentions 给事项 item 上发言 comment 的每个 @ 标出状态。
func mentions(tx *store.Tx, item, comment string, targets []string) ([]Mention, error) {
	var kids []domain.Item
	out := make([]Mention, 0, len(targets))
	for _, t := range targets {
		if t == domain.MentionMain {
			m := Mention{Target: t, Kind: "main", State: MentionSent}
			st, ok, err := delivery.StateOf(tx, delivery.MentionPrefix+comment)
			if err != nil {
				return nil, err
			}
			if ok {
				switch st {
				case "delivered", "acknowledged":
					m.State = MentionDelivered
				case "fallback_web":
					m.State = MentionNotDelivered
				case "superseded":
					m.State = MentionWithdrawn
				}
			}
			out = append(out, m)
			continue
		}
		if kids == nil {
			var err error
			if kids, err = items.Children(tx, item); err != nil {
				return nil, err
			}
		}
		m := Mention{Target: t, Kind: "executor", State: MentionMissing}
		for _, k := range kids {
			if k.Assignee != t || k.Origin == nil || k.Origin.Source != delegation.OriginMention || k.Origin.Ref != comment {
				continue
			}
			m.Item = k.ID
			ds, err := delegation.Dispatches(tx, k.ID)
			if err != nil {
				return nil, err
			}
			running := false
			for _, d := range ds {
				if d.Run != "" {
					m.Run = d.Run
				}
				running = running || d.State == delegation.DispatchRunning
			}
			switch {
			case k.Phase == domain.PhaseDone:
				m.State = MentionReplied
			case k.Phase == domain.PhaseDropped:
				m.State = MentionStopped
			case running:
				m.State = MentionRunning
			default:
				m.State = MentionBlocked
			}
		}
		out = append(out, m)
	}
	return out, nil
}
