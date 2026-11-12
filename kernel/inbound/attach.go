package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdAttach 是建议的第三种处置：补到已有事项（§7.2）。
const CmdAttach = "proposal.attach"

// CmdBatch 是批量处理的授权门（§7.2 proposal.batch）：每条建议的处置仍是各自的命令、各记一条 Event。
const CmdBatch = "proposal.batch"

type attachIn struct {
	ID     string `json:"id"`
	To     string `json:"to"`
	Reason string `json:"reason"`
	// Evidence 随 HTTP 体到达时是请求元数据（本人原话），命令管线另行读取；这里只接受不使用。
	Evidence string `json:"evidence,omitempty"`
}

func parseAttach(in json.RawMessage) (attachIn, error) {
	var v attachIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, domain.NewError(domain.CodeInvalidRequest, "invalid input: "+err.Error(), "send {id, to, reason?}")
	}
	return v, nil
}

// RegisterAttach 登记 proposal.attach：建议的来源事件改挂到目标事项并成为它的挂靠消息，建议的关联键登记到目标
// （此后同一线程的消息直接回到那里），标题与摘要作为一条进展记在目标上，建议转 dropped 并与目标建立 relates 关系。
// 全部写入在一个事务里：目标不合法时什么都不改。授权同 proposal.accept、proposal.dismiss（代答；运行身份得到 4）。
func RegisterAttach(x *command.Executor) error {
	// 批量的门：不对应任何事项，只过授权（运行身份 4、缺依据 23）。没有可处理的建议时靠它也能拒绝无权的调用。
	if err := x.Register(command.Spec{Name: CmdBatch, Action: authz.ActionAnswerDecision, Tier: authz.TierDecide,
		Object: func(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{}, nil },
		Apply:  func(*command.Ctx) (any, error) { return nil, nil }}); err != nil {
		return err
	}
	return x.Register(command.Spec{Name: CmdAttach, Action: authz.ActionAnswerDecision, Tier: authz.TierDecide,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseAttach(in)
			if err != nil {
				return authz.Object{}, err
			}
			it, err := items.Get(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			return authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Tags: it.Tags, Project: it.Project}, nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parseAttach(in)
			_, err := attachTarget(tx, v)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseAttach(c.Input)
			target, err := attachTarget(c.Tx, v)
			if err != nil {
				return nil, err
			}
			state := runner.AttachProcessed
			if target.Kind == domain.KindDelegation {
				state = runner.AttachPending // 委派事项的 agent 通过回执取走新消息（§9.4）
			}
			msgs, err := runner.Reattach(c.Tx, v.ID, v.To, state)
			if err != nil {
				return nil, err
			}
			var (
				keys []items.Correlation
				ids  []string
			)
			for _, m := range msgs {
				ids = append(ids, m.ID)
				for _, k := range m.CorrelationKeys {
					if x := (items.Correlation{Domain: m.DedupeKey.Domain, Key: k}); !slices.Contains(keys, x) {
						keys = append(keys, x)
					}
				}
			}
			ws, err := items.Absorb(c.Tx, v.ID, v.To, keys, ids, c.Caller.Actor.Principal, c.Now())
			if err != nil {
				return nil, err
			}
			reason := "attached to " + v.To
			if strings.TrimSpace(v.Reason) != "" {
				reason += ": " + v.Reason
			}
			for _, w := range ws {
				e := domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, Evidence: c.Evidence}
				switch {
				case w.Object == v.ID:
					e.Reason, e.ResolvedVia = reason, attachVia(c)
				case w.Type == domain.EvProposalAttached:
					e.Reason, e.ResolvedVia = reason, attachVia(c)
					fallthrough
				default: // 目标的每条事项事件各推进一次版本
					if _, err := command.Bump(c.Tx, w.Object); err != nil {
						return nil, err
					}
				}
				if _, err := c.Emit(e); err != nil {
					return nil, err
				}
			}
			return struct {
				ID string `json:"id"`
			}{v.ID}, nil
		}})
}

// attachVia 同 proposal.accept 的 resolved_via：Web 上的本人记 web，依据授权单记 grant，其余（附证据的 delegate）记 chat。
func attachVia(c *command.Ctx) string {
	switch {
	case c.Caller.Level == authz.LevelWeb:
		return "web"
	case c.Grant != "":
		return "grant"
	}
	return "chat"
}

// attachTarget 校验补到的目标：建议仍是 proposed；目标存在（404）、未关闭、不在 proposed（409）。
func attachTarget(tx *store.Tx, v attachIn) (domain.Item, error) {
	var zero domain.Item
	if strings.TrimSpace(v.To) == "" {
		return zero, domain.NewError(domain.CodeInvalidRequest, "to is required", "give the item the proposal should be attached to")
	}
	prop, err := items.Get(tx, v.ID)
	if err != nil {
		return zero, err
	}
	if prop.Phase != domain.PhaseProposed {
		return zero, domain.NewError(domain.CodeConflict, "the item is not a pending proposal", "re-read the item; the first answer stands")
	}
	if v.To == v.ID {
		return zero, domain.NewError(domain.CodeInvalidRequest, "a proposal cannot be attached to itself", "choose another item")
	}
	target, err := items.Get(tx, v.To)
	if err != nil {
		return zero, err
	}
	switch {
	case target.Phase.Terminal():
		return zero, domain.NewError(domain.CodeConflict, "target item "+v.To+" is closed", "choose an open item, or accept the proposal as a new item")
	case target.Phase == domain.PhaseProposed:
		return zero, domain.NewError(domain.CodeConflict, "target item "+v.To+" is itself a pending proposal", "accept it first, or choose an open item")
	}
	return target, nil
}

// ---- 批量处理（§7.2 proposal.batch） ----

// MaxBatch 是一次批量至多处理的建议条数。
const MaxBatch = 200

// 批量的两种方式。
const (
	BatchRecommended = "recommended"
	BatchExplicit    = "explicit"
)

// BatchFilter 限定按推荐处理的范围；都空表示全部带推荐的建议。
type BatchFilter struct {
	Source  string   `json:"source,omitempty"`
	Project string   `json:"project,omitempty"`
	IDs     []string `json:"ids,omitempty"`
}

// BatchAction 是处置清单里的一条。
type BatchAction struct {
	ID     string `json:"id"`
	Do     string `json:"do"` // accept | attach | dismiss
	To     string `json:"to,omitempty"`
	Reason string `json:"reason,omitempty"`
}

// BatchRequest 二选一：Mode=recommended 配 Filter；Mode=explicit 配 Actions。
type BatchRequest struct {
	Mode    string        `json:"mode"`
	Filter  BatchFilter   `json:"filter,omitzero"`
	Actions []BatchAction `json:"actions,omitempty"`
}

// BatchResult 是一条建议的处理结果。失败时 Code 与 Why 说明原因，建议不变。
type BatchResult struct {
	ID   string      `json:"id"`
	Do   string      `json:"do"`
	OK   bool        `json:"ok"`
	Code domain.Code `json:"code,omitempty"`
	Why  string      `json:"why,omitempty"`
}

func usage(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "see the batch format in the API reference")
}

// Batch 逐条处理建议。形态错误、超过 MaxBatch、身份不能代答（4）或缺依据（23）时整批不生效（身份与依据先过一道门，与第一条无关）；
// 其余按条各自成败，每条成功的处置各是一条命令、各记一条 Event、可逐条撤销。req 带调用者身份与依据，
// Command、Input 由本函数按每条填写；req.IdemKey 非空时每条派生自己的幂等键。
func Batch(ctx context.Context, x *command.Executor, st *store.Store, req command.Request, b BatchRequest) ([]BatchResult, error) {
	acts, err := planBatch(ctx, st, b)
	if err != nil {
		return nil, err
	}
	// 身份与本人依据对整批一视同仁，先于逐条处理一次判定（运行身份 4、缺依据 23、会话不是直接对话）。
	// 授权单按对象校验范围且只列具体命令，不能过这道门：授权单请求的失败全部落到各条结果里。
	if req.Grant == "" {
		gate := req
		gate.Command, gate.Input, gate.IdemKey = CmdBatch, json.RawMessage(`{}`), ""
		if _, err := x.Do(ctx, gate); err != nil {
			return nil, err
		}
	}
	out := make([]BatchResult, 0, len(acts))
	var denied error
	for _, a := range acts {
		r := req
		r.Command, r.Input = batchCommand(a)
		if req.IdemKey != "" {
			r.IdemKey = req.IdemKey + "#" + a.ID
		}
		_, err := x.Do(ctx, r)
		if err == nil {
			out = append(out, BatchResult{ID: a.ID, Do: a.Do, OK: true})
			continue
		}
		var de *domain.Error
		if !errors.As(err, &de) {
			return out, err
		}
		if de.Code == domain.CodePermissionDenied && denied == nil {
			denied = err
		}
		out = append(out, BatchResult{ID: a.ID, Do: a.Do, Code: de.Code, Why: de.Why})
	}
	// 凭据级别不够（授权单路径上没有门）：每一条都被拒绝说明整批都不会生效。
	if denied != nil && !slices.ContainsFunc(out, func(r BatchResult) bool { return r.Code != domain.CodePermissionDenied }) {
		return nil, denied
	}
	return out, nil
}

var batchVerbs = map[string]string{"accept": "proposal.accept", "dismiss": "proposal.dismiss", "attach": CmdAttach}

func batchCommand(a BatchAction) (string, json.RawMessage) {
	m := map[string]string{"id": a.ID}
	if a.Reason != "" {
		m["reason"] = a.Reason
	}
	if a.Do == "attach" {
		m["to"] = a.To
	}
	in, _ := json.Marshal(m)
	return batchVerbs[a.Do], in
}

func planBatch(ctx context.Context, st *store.Store, b BatchRequest) ([]BatchAction, error) {
	switch b.Mode {
	case BatchExplicit:
		if len(b.Actions) == 0 || len(b.Actions) > MaxBatch {
			return nil, usage("explicit mode takes 1 to 200 actions")
		}
		if b.Filter.Source != "" || b.Filter.Project != "" || len(b.Filter.IDs) > 0 {
			return nil, usage("filter belongs to recommended mode")
		}
		for _, a := range b.Actions {
			if _, ok := batchVerbs[a.Do]; !ok || strings.TrimSpace(a.ID) == "" {
				return nil, usage("every action needs an id and do of accept, attach or dismiss")
			}
		}
		return b.Actions, nil
	case BatchRecommended:
		if len(b.Actions) > 0 {
			return nil, usage("actions belong to explicit mode")
		}
		var acts []BatchAction
		err := st.View(ctx, func(tx *store.Tx) (err error) { acts, err = recommended(tx, b.Filter); return })
		if err == nil && len(acts) > MaxBatch {
			err = usage("at most 200 proposals per batch; narrow the filter")
		}
		return acts, err
	}
	return nil, usage("mode must be recommended or explicit")
}

// recommended 把范围内带推荐的建议翻成处置：忽略取推荐理由，补到取推荐的目标。没有推荐的不动。
func recommended(tx *store.Tx, f BatchFilter) ([]BatchAction, error) {
	props, err := items.List(tx, items.Filter{Phase: domain.PhaseProposed, Project: f.Project})
	if err != nil {
		return nil, err
	}
	var out []BatchAction
	for _, p := range props {
		if len(f.IDs) > 0 && !slices.Contains(f.IDs, p.ID) {
			continue
		}
		rec, ok, err := items.RecommendationOf(tx, p.ID)
		if err != nil || !ok {
			if err != nil {
				return nil, err
			}
			continue
		}
		if f.Source != "" {
			from, err := fromSource(tx, p.ID, f.Source)
			if err != nil {
				return nil, err
			}
			if !from {
				continue
			}
		}
		a := BatchAction{ID: p.ID, Do: rec.Recommend, To: rec.AttachTo}
		if rec.Recommend == "dismiss" {
			a.Reason = rec.Reason
		}
		out = append(out, a)
	}
	return out, nil
}

func fromSource(tx *store.Tx, item, source string) (bool, error) {
	r, err := tx.Query(`SELECT 1 FROM sources_runner_event WHERE item = ? AND source_id = ? AND disposition = ? LIMIT 1`,
		item, source, string(domain.DispositionProposed))
	if err != nil {
		return false, err
	}
	defer r.Close()
	return r.Next(), r.Err()
}
