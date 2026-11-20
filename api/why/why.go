// Package why 是回放与审计分组的 HTTP 路由（附录 B.2，设计 §15.1、§15.3）：
// GET /why/{id}（事项或运行回放）、GET /audit/events（按操作者、对象、类型筛选的 Event 查询），
// 以及处理经过 GET /source-events/{id}/trace 与 GET /items/{id}/trace。
//
// 路由里没有业务规则：读取、授权与按凭据级别的内容裁剪都在 kernel/why；
// 本层只做认证后的参数搬运，以及把"哪些事项有未返回的 web_only 初筛"从账本取给内核。
// /events（SSE 订阅）属于 api-events，不在这里。
package why

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	kwhy "github.com/xiaohei-info/awesome-personal-agent/kernel/why"
)

// Routes 返回回放与审计路由分组，交给 core.Deps.Groups。
func Routes(st *store.Store) core.RouteGroup {
	h := &handlers{st: st}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/why/{id}", Handler: h.why})
		r.Handle(core.Route{Method: "GET", Path: "/audit/events", Handler: h.audit})
		r.Handle(core.Route{Method: "GET", Path: "/source-events/{id}/trace", Handler: h.messageTrace})
		r.Handle(core.Route{Method: "GET", Path: "/items/{id}/trace", Handler: h.itemTrace})
	}
}

type handlers struct{ st *store.Store }

// readable：ext 与 webhook 凭据另有用途，不能读账本。
func readable(p *core.Principal) error {
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
	}
	return nil
}

func (h *handlers) why(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	id := r.PathValue("id")
	var tl kwhy.Timeline
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		tl, err = kwhy.Replay(tx, pol, kwhy.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, ID: id})
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, tl)
}

func (h *handlers) audit(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	q := r.URL.Query()
	num := func(key string) (int64, error) {
		if q.Get(key) == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(q.Get(key), 10, 64)
		if err != nil {
			return 0, domain.NewError(domain.CodeInvalidRequest, key+" must be an integer", "send a number")
		}
		return n, nil
	}
	after, err := num("after")
	if err != nil {
		return err
	}
	before, err := num("before")
	if err != nil {
		return err
	}
	limit, err := num("limit")
	if err != nil {
		return err
	}
	var desc bool
	switch q.Get("order") {
	case "", "asc":
	case "desc":
		desc = true
	default:
		return domain.NewError(domain.CodeInvalidRequest, "order must be asc or desc", "send order=desc for newest first")
	}
	req := kwhy.AuditRequest{Actor: p.Caller.Actor, Level: p.Caller.Level, After: after, Before: before, Desc: desc, Limit: int(limit),
		Filter: eventlog.Filter{Actor: q.Get("actor"), Object: q.Get("object"), Type: q.Get("type"), Grant: q.Get("grant"), Imported: q.Get("imported")}}
	var steps []kwhy.Step
	if err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		steps, err = kwhy.Audit(tx, pol, req)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"events": steps})
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

// traceable：处理经过含来源原文的标题与发件人，口径同 GET /sources/{id}/events——运行凭据不可读（§9.4、§14.3）。
func traceable(p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	if p.Caller.Level == authz.LevelRun {
		return domain.NewError(domain.CodePermissionDenied, "run credential cannot read source messages", "read forwarded messages from the context instead")
	}
	return nil
}

func (h *handlers) messageTrace(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := traceable(p); err != nil {
		return err
	}
	id := r.PathValue("id")
	var t kwhy.Trace
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		ev, err := runner.Event(tx, id)
		if err != nil {
			return err
		}
		if !p.Web && inbound.IsCaptureSource(ev.SourceID) { // 对话内容只给本人；其他凭据与不存在一样
			return domain.NewError(domain.CodeNotFound, "source event not found", "check the id")
		}
		t, err = kwhy.MessageTrace(tx, pol, kwhy.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, ID: id})
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, t)
}

func (h *handlers) itemTrace(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := traceable(p); err != nil {
		return err
	}
	id := r.PathValue("id")
	var t kwhy.Trace
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		if _, err := kitems.Get(tx, id); err != nil {
			return err
		}
		if t, err = kwhy.ItemTrace(tx, pol, kwhy.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, ID: id}); err != nil {
			return err
		}
		if !p.Web {
			t.Origins = slices.DeleteFunc(t.Origins, func(o kwhy.Trace) bool { return inbound.IsCaptureSource(o.Message.Source) })
		}
		return nil
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, t)
}

// RuleChangeRoutes 是规矩改动清单（§14.3 3a 的兜底，附录 B.2）：只给 Web 本人。
//
//	GET  /audit/rule-changes?agent=&before=&limit=   3a 改动，带前值、后值与能否撤回
//	POST /audit/rule-changes/{seq}/revert            按次撤回一条
//	POST /audit/rule-changes/revert  {"agent"}       按 Agent 批量撤回（逐条，各自成败）
//
// 撤回以本人身份经命令管线或策略写入执行；能否撤回、逆操作是什么都由 kernel/why 判定。
func RuleChangeRoutes(st *store.Store, x *command.Executor) core.RouteGroup {
	h := &ruleChanges{st: st, x: x}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/audit/rule-changes", Handler: h.list})
		r.Handle(core.Route{Method: "POST", Path: "/audit/rule-changes/{seq}/revert", MaxBody: 1 << 10, Handler: h.revertOne})
		r.Handle(core.Route{Method: "POST", Path: "/audit/rule-changes/revert", MaxBody: 1 << 10, Handler: h.revertAgent})
	}
}

type ruleChanges struct {
	st *store.Store
	x  *command.Executor
}

func ownerOnly(p *core.Principal) error {
	if !p.Web {
		return domain.NewError(domain.CodeWebOnly, "rule changes are reviewed by the owner in the Web UI", "open Agent changes in the Web UI")
	}
	return nil
}

func (h *ruleChanges) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := ownerOnly(p); err != nil {
		return err
	}
	q := r.URL.Query()
	before, _ := strconv.ParseInt(q.Get("before"), 10, 64)
	limit, _ := strconv.Atoi(q.Get("limit"))
	var out []kwhy.RuleChange
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		out, err = kwhy.RuleChanges(tx, q.Get("agent"), before, limit)
		return
	}); err != nil {
		return err
	}
	if out == nil {
		out = []kwhy.RuleChange{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"changes": out})
}

// revert 撤回 seq 那一次；撤不回时返回 409 与原因码（reason）。
func (h *ruleChanges) revert(r *http.Request, p *core.Principal, seq int64) error {
	ctx := context.WithoutCancel(r.Context())
	var rv *kwhy.Reversal
	var rc kwhy.RuleChange
	err := h.st.Update(ctx, func(tx *store.Tx) error {
		var err error
		if rc, rv, err = kwhy.ReverseOf(tx, seq); err != nil || rv == nil || rv.Command != "" {
			return err
		}
		from, err := policy.At(tx, rv.From)
		if err != nil {
			return err
		}
		_, err = policy.UpdateWith(tx, p.Caller.Actor, &domain.Basis{Via: domain.BasisOwnerWeb, Tier: "rules"}, "revert of rule change "+strconv.FormatInt(seq, 10),
			func(np *policy.Policy) error { return policy.Restore(np, from, rv.Sections) })
		return err
	})
	if err != nil {
		return err
	}
	if rv == nil {
		e := domain.NewError(domain.CodeConflict, "this change cannot be reverted automatically", "change it back on its settings page")
		e.Reason = rc.Revert.Why
		return e
	}
	if rv.Command == "" {
		return nil
	}
	// shortcut: 判定与执行分在两个事务，期间又被改过会以命令自己的校验为准；要严格就把逆命令并进同一事务。
	_, err = h.x.Do(ctx, command.Request{Identity: p.Identity, Command: rv.Command, Input: rv.Input})
	return err
}

func (h *ruleChanges) revertOne(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := ownerOnly(p); err != nil {
		return err
	}
	seq, err := strconv.ParseInt(r.PathValue("seq"), 10, 64)
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "seq must be an integer", "send the change's seq")
	}
	if err := h.revert(r, p, seq); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "seq": seq})
}

// revertAgent 把一个 Agent 的 3a 改动从新到旧逐条撤回；撤不回的照列原因，不中断其余。
func (h *ruleChanges) revertAgent(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := ownerOnly(p); err != nil {
		return err
	}
	var in struct {
		Agent string `json:"agent"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in.Agent == "" {
		return domain.NewError(domain.CodeInvalidRequest, "agent is required", "send the Agent id")
	}
	var list []kwhy.RuleChange
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		list, err = kwhy.RuleChanges(tx, in.Agent, 0, 500)
		return
	}); err != nil {
		return err
	}
	type result struct {
		Seq    int64  `json:"seq"`
		OK     bool   `json:"ok"`
		Reason string `json:"reason,omitempty"`
	}
	out := []result{}
	for _, c := range list {
		if !c.Revert.OK {
			out = append(out, result{Seq: c.Seq, Reason: c.Revert.Why})
			continue
		}
		res := result{Seq: c.Seq, OK: true}
		if err := h.revert(r, p, c.Seq); err != nil {
			res.OK = false
			var de *domain.Error
			if errors.As(err, &de) {
				res.Reason = string(de.Code)
				if de.Reason != "" {
					res.Reason = de.Reason
				}
			} else {
				return err
			}
		}
		out = append(out, res)
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"results": out})
}
