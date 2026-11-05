// Package why 是回放读模型（设计 §15.1、§15.3）：按 Event 回放一个事项或一次运行的来龙去脉，
// 含授权依据与被拒绝的命令。只读；正文（reason、payload、evidence）按凭据级别裁剪，
// 信封与授权依据对所有级别可见。Web 事项详情的时间线与 `apa why` 是同一份数据。
package why

import (
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
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
	Reason      string          `json:"reason,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	Evidence    string          `json:"evidence,omitempty"`
	Redacted    bool            `json:"redacted,omitempty"`
	Tombstone   bool            `json:"tombstone,omitempty"`
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
	vis := visible(r.Level)
	for _, en := range entries {
		e := en.Event
		s := Step{Seq: e.Seq, Type: e.Type, Object: e.Object, Actor: e.Actor, At: e.At.Format("2006-01-02T15:04:05.999999999Z07:00"),
			ChainDepth: e.ChainDepth, Basis: e.Basis, ResolvedVia: e.ResolvedVia, Tombstone: en.Tombstone}
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
		tl.Steps = append(tl.Steps, s)
	}
	return tl, nil
}
