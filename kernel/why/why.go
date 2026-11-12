// Package why 是回放读模型（设计 §15.1、§15.3）：按 Event 回放一个事项或一次运行的来龙去脉，
// 含授权依据与被拒绝的命令。只读；正文（reason、payload、evidence）按凭据级别裁剪，
// 信封与授权依据对所有级别可见。Web 事项详情的时间线与 `apa why` 是同一份数据。
package why

import (
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Request 是一次回放。ID 是事项 id 或运行 id（即 Event 的 object）；
// ScreeningPending 由调用方据账本给出：该事项有 web_only 初筛未返回的决策请求（§14.4）。
type Request struct {
	Actor            domain.Actor
	Level            authz.Level
	ID               string
	ScreeningPending bool
}

// Step 是时间线上的一条 Event。Redacted 为真表示该条有内容因凭据级别被裁掉；
// Tombstone 为真表示内容已被物理删除抹除（与凭据无关）。
type Step struct {
	Seq         int64           `json:"seq"`
	Type        string          `json:"type"`
	Object      string          `json:"object"`
	Actor       domain.Actor    `json:"actor"`
	At          string          `json:"at"`
	ChainDepth  int             `json:"chain_depth"`
	Basis       *domain.Basis   `json:"basis,omitempty"`
	ResolvedVia string          `json:"resolved_via,omitempty"`
	Grant       string          `json:"grant,omitempty"` // 依据授权单写入时的授权单 id，属授权依据，各级可见
	Reason      string          `json:"reason,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Evidence    string          `json:"evidence,omitempty"`
	Redacted    bool            `json:"redacted,omitempty"`
	Tombstone   bool            `json:"tombstone,omitempty"`
	// Judgment 是这条 Event 用到的判断（载荷带 judgment 字段，§8.6）：记录在就逐级展开，
	// 过了保留期只剩 Event 载荷里的结论。结论与逐级过程不含资料正文，所以各凭据级别都可见。
	Judgment *Judgment `json:"judgment,omitempty"`
}

// Judgment 是回放里的"判断"一行。Record 为空表示判断记录已过保留期（Expired），Answer 与 DecidedBy 取自命令 Event 的载荷。
type Judgment struct {
	ID        string         `json:"id"`
	Answer    string         `json:"answer,omitempty"`
	DecidedBy string         `json:"decided_by,omitempty"`
	Expired   bool           `json:"expired,omitempty"`
	Record    *record.Record `json:"record,omitempty"`
}

// Timeline 是回放结果。ScreeningPending 为真时（仅对非 Web 凭据）Steps 为空，只返回这一标记。
type Timeline struct {
	ID               string `json:"id"`
	ScreeningPending bool   `json:"screening_pending,omitempty"`
	Steps            []Step `json:"steps"`
}

// content 是各凭据级别可见的正文字段。Evidence 是本人原话与会话引用，只给 Web 与 delegate。
type content struct{ reason, payload, evidence bool }

func visible(l authz.Level) content {
	switch l {
	case authz.LevelWeb, authz.LevelDelegate:
		return content{true, true, true}
	case authz.LevelWorker, authz.LevelRun:
		return content{true, true, false}
	}
	return content{} // observer：只有信封与授权依据
}

// Replay 在 tx 内读取 r.ID 的全部 Event 并按 r.Level 裁剪。无 Event 返回 not_found。
func Replay(tx *store.Tx, p policy.Policy, r Request) (Timeline, error) {
	if r.ID == "" {
		return Timeline{}, domain.NewError(domain.CodeInvalidRequest, "id is empty", "pass an item or run id")
	}
	if d := authz.Decide(p, authz.Request{Actor: r.Actor, Level: r.Level, Action: authz.ActionRead, Object: authz.Object{ID: r.ID}}); d.Err != nil {
		return Timeline{}, d.Err
	}
	tl := Timeline{ID: r.ID, Steps: []Step{}}
	if r.ScreeningPending && r.Level != authz.LevelWeb {
		tl.ScreeningPending = true
		return tl, nil
	}
	entries, err := eventlog.ReadObject(tx, r.ID)
	if err != nil {
		return Timeline{}, err
	}
	if len(entries) == 0 {
		return Timeline{}, domain.NewError(domain.CodeNotFound, "no events for this id", "check the id")
	}
	for _, en := range entries {
		st := StepOf(en, r.Level)
		if st.Judgment, err = judgmentOf(tx, en); err != nil {
			return Timeline{}, err
		}
		tl.Steps = append(tl.Steps, st)
	}
	return tl, nil
}

// judgmentOf 展开 Event 载荷里的 judgment 引用；载荷不是对象、没有该字段或已被墓碑抹除都返回 nil。
// v0.1 的 run.finished（category: judge）没有这个字段，原样作为普通 Event 显示。
func judgmentOf(tx *store.Tx, en eventlog.Entry) (*Judgment, error) {
	if en.Tombstone || len(en.Event.Payload) == 0 {
		return nil, nil
	}
	var ref record.Ref
	if json.Unmarshal(en.Event.Payload, &ref) != nil || ref.Judgment == "" {
		return nil, nil
	}
	rec, err := record.Get(tx, ref.Judgment)
	if err == nil {
		return &Judgment{ID: rec.ID, Answer: rec.Answer, DecidedBy: rec.DecidedBy, Record: &rec}, nil
	}
	if e, ok := err.(*domain.Error); ok && e.Code == domain.CodeNotFound {
		return &Judgment{ID: ref.Judgment, Answer: ref.Answer, DecidedBy: ref.DecidedBy, Expired: true}, nil
	}
	return nil, err
}

// StepOf 把一条 Event 按凭据级别裁剪成 Step；回放与审计共用，保证两处裁剪一致。
func StepOf(en eventlog.Entry, l authz.Level) Step {
	e, vis := en.Event, visible(l)
	s := Step{Seq: e.Seq, Type: e.Type, Object: e.Object, Actor: e.Actor, At: e.At.Format("2006-01-02T15:04:05.999999999Z07:00"),
		ChainDepth: e.ChainDepth, Basis: e.Basis, ResolvedVia: e.ResolvedVia, Grant: e.Grant, Tombstone: en.Tombstone}
	if vis.reason {
		s.Reason = e.Reason
	}
	if vis.payload {
		s.Payload = e.Payload
	}
	if vis.evidence {
		s.Evidence = e.Evidence
	}
	s.Redacted = !en.Tombstone && ((e.Reason != "" && !vis.reason) || (len(e.Payload) > 0 && string(e.Payload) != "null" && !vis.payload) || (e.Evidence != "" && !vis.evidence))
	return s
}

// AuditRequest 是一次审计查询（§15.3）：按操作者、对象、类型筛选全量 Event（含被拒绝的命令）。
// Pending 是有 web_only 初筛未返回的事项 id；非 Web 凭据看不到这些对象的事件，与回放一致。
type AuditRequest struct {
	Actor   domain.Actor
	Level   authz.Level
	Filter  eventlog.Filter
	After   int64
	Limit   int
	Pending map[string]bool
}

// MaxAuditLimit 是单页上限。
const MaxAuditLimit = 500

// Audit 返回满足条件的 Event 页，按序号升序，内容按 r.Level 裁剪。Limit 为 0 用 100。
func Audit(tx *store.Tx, p policy.Policy, r AuditRequest) ([]Step, error) {
	if r.Limit < 0 || r.Limit > MaxAuditLimit || r.After < 0 {
		return nil, domain.NewError(domain.CodeInvalidRequest, "after or limit is out of range", "use after >= 0 and 1 <= limit <= 500")
	}
	if r.Limit == 0 {
		r.Limit = 100
	}
	if d := authz.Decide(p, authz.Request{Actor: r.Actor, Level: r.Level, Action: authz.ActionRead}); d.Err != nil {
		return nil, d.Err
	}
	steps := []Step{}
	for len(steps) < r.Limit {
		entries, err := eventlog.Query(tx, r.Filter, r.After, r.Limit)
		if err != nil {
			return nil, err
		}
		for _, en := range entries {
			r.After = en.Event.Seq
			if r.Level != authz.LevelWeb && r.Pending[en.Event.Object] {
				continue
			}
			steps = append(steps, StepOf(en, r.Level))
		}
		if len(entries) < r.Limit {
			break
		}
	}
	return steps[:min(len(steps), r.Limit)], nil
}
