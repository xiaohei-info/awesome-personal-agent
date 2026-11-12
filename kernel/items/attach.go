package items

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Write 是 Absorb 已落投影、等调用方在同一事务追加的一条事项事件。
type Write struct {
	Type    string
	Object  string
	Payload json.RawMessage
}

// Absorb 在调用方事务内把建议 prop 并入已有事项 target（§7.2 补到已有事项）：
// 关联键登记到 target（已登记的跳过）、建议的标题与摘要作为一条进展记在 target 上、
// 建议转 dropped 并与 target 建 relates 关系。messages 是随之改挂的来源事件 id，只进事件快照。
// 返回按序追加的事件：target 的键与进展，建议的 item.phase_changed，最后是以 target 为对象的 proposal.attached。
// 投影与折叠一致由调用方追加这些事件保证；target 每条事件对应一次 command.Bump，由调用方负责。
// 目标是否合法（未关闭、不在 proposed）由调用方先校验。
func Absorb(tx *store.Tx, prop, target string, keys []Correlation, messages []string, actor string, at time.Time) ([]Write, error) {
	var out []Write
	cur, err := Get(tx, target)
	if err != nil {
		return nil, err
	}
	for _, k := range keys {
		if checkCorrelation(k) != nil {
			continue // 来源事件里不合规的键登记不了，也就从未能命中
		}
		has, err := hasCorrelation(tx, target, k)
		if err != nil {
			return nil, err
		}
		if has {
			continue
		}
		if err := setCorrelation(tx, target, k, false); err != nil {
			return nil, err
		}
		cur = stamped(cur, at)
		if err := save(tx, cur); err != nil {
			return nil, err
		}
		k := k
		w, err := write(domain.EvItemUpdated, target, payload{Op: "relate", Item: cur, Key: &k})
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	before, err := Get(tx, prop)
	if err != nil {
		return nil, err
	}
	text := before.Title
	if before.Summary != "" {
		text += "\n" + before.Summary
	}
	prev, err := Progress(tx, target)
	if err != nil {
		return nil, err
	}
	p := domain.Progress{ID: fmt.Sprintf("%s.p%06d", target, len(prev)+1), Text: text, Actor: actor, At: at.UTC()}
	if err := insertProgress(tx, target, p); err != nil {
		return nil, err
	}
	cur = stamped(cur, at)
	if err := save(tx, cur); err != nil {
		return nil, err
	}
	w, err := write(domain.EvItemProgressAdded, target, payload{Op: "progress", Item: cur, Progress: &p})
	if err != nil {
		return nil, err
	}
	out = append(out, w)

	after := before
	after.Phase = domain.PhaseDropped
	if e := domain.CheckEntry(after, after.Phase, "attached", true); e != nil {
		return nil, e
	}
	after = stamped(after, at)
	if err := save(tx, after); err != nil {
		return nil, err
	}
	if err := syncTimer(tx, before, after); err != nil {
		return nil, err
	}
	rel := domain.Relation{Type: domain.RelRelates, From: prop, To: target}
	if err := setRelation(tx, rel, false); err != nil {
		return nil, err
	}
	w, err = write(domain.EvItemPhaseChanged, prop, payload{Op: "proposal.attach", Before: &before, Item: after, Relation: &rel, To: target})
	if err != nil {
		return nil, err
	}
	out = append(out, w)

	// 对外的 proposal.attached 以被补到的事项为对象（附录 B.4）；它也是目标的一次事项事件。
	cur = stamped(cur, at)
	if err := save(tx, cur); err != nil {
		return nil, err
	}
	w, err = write(domain.EvProposalAttached, target, payload{Op: "proposal.attach", Item: cur, Proposal: prop, Messages: messages})
	if err != nil {
		return nil, err
	}
	return append(out, w), nil
}

func write(typ, object string, p payload) (Write, error) {
	b, err := marshalPayload(p)
	return Write{Type: typ, Object: object, Payload: b}, err
}

// Recommendation 是建议信息（§6.3 proposal）里批量处理要用的推荐。
type Recommendation struct {
	Recommend string `json:"recommend"` // accept | attach | dismiss
	AttachTo  string `json:"attach_to"`
	Reason    string `json:"reason"`
}

// RecommendationOf 读建议的推荐；没有建议信息、没有推荐或推荐不是三种处置之一时 ok 为假。
// 建议信息由入站流水线写在 items.proposal 列；本函数只读。
func RecommendationOf(tx *store.Tx, id string) (rec Recommendation, ok bool, err error) {
	r, err := tx.Query(`SELECT proposal FROM items WHERE id = ?`, id)
	if err != nil {
		return rec, false, err
	}
	defer r.Close()
	var raw string
	if r.Next() {
		if err = r.Scan(&raw); err != nil {
			return rec, false, err
		}
	}
	if err = r.Err(); err != nil || raw == "" {
		return rec, false, err
	}
	if err := json.Unmarshal([]byte(raw), &rec); err != nil {
		return rec, false, fmt.Errorf("items: corrupt proposal of %s: %w", id, err)
	}
	switch rec.Recommend {
	case "accept", "dismiss":
		return rec, true, nil
	case "attach":
		return rec, rec.AttachTo != "", nil
	}
	return rec, false, nil
}

// SetProposal 在调用方事务内写入建议信息（§6.3 proposal），只有入站流水线的处置命令调用。
// 规则可在记处置前自动接受，所以不要求仍在 proposed。调用方追加返回的事件并为 id 做一次 command.Bump。
func SetProposal(tx *store.Tx, id string, p domain.Proposal, at time.Time) (Write, error) {
	cur, err := Get(tx, id)
	if err != nil {
		return Write{}, err
	}
	next := cur
	next.Proposal = &p
	next = stamped(next, at)
	if err := save(tx, next); err != nil {
		return Write{}, err
	}
	return write(domain.EvItemUpdated, id, payload{Op: "update", Before: &cur, Item: next})
}

// AddProposalSource 在调用方事务内把来源事件 source 记入建议 id 的 proposal.sources（§9.3 合并键命中）。
// 建议必须仍在 proposed；已经记过该来源时什么都不改，返回 false；其余同 SetProposal。
func AddProposalSource(tx *store.Tx, id, source string, at time.Time) (w Write, changed bool, err error) {
	cur, err := Get(tx, id)
	if err != nil {
		return w, false, err
	}
	if cur.Phase != domain.PhaseProposed {
		return w, false, domain.NewError(domain.CodeConflict, "item "+id+" is "+string(cur.Phase)+", not proposed", "re-read the item")
	}
	p := domain.Proposal{}
	if cur.Proposal != nil {
		p = *cur.Proposal
	}
	if slices.Contains(p.Sources, source) {
		return w, false, nil
	}
	p.Sources = append(slices.Clone(p.Sources), source)
	w, err = SetProposal(tx, id, p, at)
	return w, err == nil, err
}
