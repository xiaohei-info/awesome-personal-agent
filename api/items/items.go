// Package items 是事项分组的 HTTP 路由（附录 B.2，设计 §7.1、§14.3）：
// GET/POST /items、GET/PATCH /items/{id}、POST /items/{id}/transition|relations|progress|revert、GET /today。
//
// 路由里没有业务规则：写入全部映射到内核命令（item.create/update/transition/relate/revert），
// 授权（含"只有 Web 登录才能撤销"）、校验、内核字段防写、乐观版本与幂等都由命令管线判定。
// 本层只做 HTTP 到命令请求的搬运：
//   - 路径里的 id 注入请求体（体里不许再写 id/from，避免两处不一致）；
//   - Idempotency-Key 头 → 幂等键；If-Match 头 → 期望版本（版本是内核字段，不能写在请求体里）；
//   - X-APA-Evidence 头（缺省取体里的 evidence）→ delegate 动作所需的本人原话证据。
package items

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/screening"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// IdempotencyHeader 携带幂等键；同键同输入重放首次结果，同键异输入返回 conflict。
	IdempotencyHeader = "Idempotency-Key"
	// IfMatchHeader 携带期望的对象版本（整数，可带引号）；不符返回 conflict。
	IfMatchHeader = "If-Match"
)

// Routes 返回事项路由分组，交给 core.Deps.Groups。x 须已经 kitems.Register。
// staleAfter 给"今天"久未推进一组的阈值（取自 stale 规则，热加载后即时生效）；省略时用默认的 14 天。
func Routes(x *command.Executor, st *store.Store, staleAfter ...func() time.Duration) core.RouteGroup {
	h := &handlers{x: x, st: st, staleAfter: func() time.Duration { return kitems.StaleAfter }}
	if len(staleAfter) > 0 && staleAfter[0] != nil {
		h.staleAfter = staleAfter[0]
	}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/items", Handler: h.list})
		r.Handle(core.Route{Method: "GET", Path: "/items/{id}", Handler: h.get})
		r.Handle(core.Route{Method: "POST", Path: "/items", Handler: h.write("item.create", "", http.StatusCreated)})
		r.Handle(core.Route{Method: "PATCH", Path: "/items/{id}", Handler: h.write("item.update", "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/transition", Handler: h.write("item.transition", "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/relations", Handler: h.write("item.relate", "from", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/progress", Handler: h.write("item.progress", "id", http.StatusCreated, h.withProgress)})
		r.Handle(core.Route{Method: "GET", Path: "/today", Handler: h.today})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/revert", Handler: h.write("item.revert", "id", http.StatusOK)})
	}
}

type handlers struct {
	x          *command.Executor
	st         *store.Store
	staleAfter func() time.Duration
}

// readable：读事项对 observer 及以上开放；ext 与 webhook 凭据另有用途，不能读账本，只有导出 ext 例外（§10.3）。
func readable(p *core.Principal) error {
	if auth.IsExportExt(p.Caller.Kind, p.Caller.Actor.Principal) {
		return nil
	}
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read items", "use an owner or agent credential")
	}
	return nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	q := r.URL.Query()
	f := kitems.Filter{Phase: domain.Phase(q.Get("phase")), Kind: domain.Kind(q.Get("kind")),
		Project: q.Get("project"), Assignee: q.Get("assignee")}
	var (
		its  []domain.Item
		rejs = map[string]Rejection{}
	)
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if its, err = kitems.List(tx, f); err != nil {
			return err
		}
		for _, it := range its {
			if rj, err := LastRejection(tx, it); err != nil {
				return err
			} else if rj != nil {
				rejs[it.ID] = *rj
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if its == nil {
		its = []domain.Item{}
	}
	for i := range its {
		its[i] = screening.Redact(its[i], p.Caller.Level)
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"items": its, "last_rejections": rejs})
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	var (
		it   domain.Item
		rels []domain.Relation
		rej  *Rejection
		prog []domain.Progress
	)
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if it, err = kitems.Get(tx, r.PathValue("id")); err != nil {
			return err
		}
		if rels, err = kitems.Relations(tx, it.ID); err != nil {
			return err
		}
		if prog, err = kitems.Progress(tx, it.ID); err != nil {
			return err
		}
		rej, err = LastRejection(tx, it)
		return err
	}); err != nil {
		return err
	}
	if prog == nil {
		prog = []domain.Progress{}
	}
	shown := screening.Redact(it, p.Caller.Level)
	if rels == nil || shown.Ask != it.Ask {
		rels = []domain.Relation{} // 初筛中的事项对非 Web 凭据不带关系
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"item": shown, "relations": rels, "progress": prog, "last_rejection": rej})
}

// today：四组取材全在内核（kitems.TodayItems）；初筛中的事项照常按凭据级别脱敏。
func (h *handlers) today(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	var l kitems.TodayList
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		l, err = kitems.TodayItemsAfter(tx, time.Now(), h.staleAfter())
		return err
	}); err != nil {
		return err
	}
	for _, g := range [][]domain.Item{l.Planned, l.Due, l.Pending, l.Stale} {
		for i := range g {
			g[i] = screening.Redact(g[i], p.Caller.Level)
		}
	}
	return core.WriteJSON(w, http.StatusOK, l)
}

// write 把请求体原样交给命令。pathKey 非空时把路径 id 写入体的该键（体里自带同名键直接拒绝）。
func (h *handlers) write(cmd, pathKey string, status int, extra ...func(r *http.Request, id string, out map[string]any) error) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		var body map[string]json.RawMessage
		if err := core.DecodeJSON(r, &body); err != nil {
			return err
		}
		if body == nil { // JSON null
			return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
		}
		if pathKey != "" {
			for k := range body {
				if strings.EqualFold(k, "id") || strings.EqualFold(k, pathKey) {
					return domain.NewError(domain.CodeInvalidRequest, "field "+k+" comes from the URL and cannot be in the body", "remove it from the body")
				}
			}
			id, _ := json.Marshal(r.PathValue("id"))
			body[pathKey] = id
		}
		in, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req := command.Request{Identity: p.Identity, Command: cmd, Input: in,
			IdemKey: r.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL}
		if req.Evidence == "" {
			_ = json.Unmarshal(body["evidence"], &req.Evidence)
		}
		if v := r.Header.Get(IfMatchHeader); v != "" {
			n, err := strconv.ParseInt(strings.Trim(v, `"`), 10, 64)
			if err != nil || n < 0 {
				return domain.NewError(domain.CodeInvalidRequest, IfMatchHeader+" must be an object version number", "send the version from the last read")
			}
			req.ExpectVersion = &n
		}
		res, err := h.x.Do(context.WithoutCancel(r.Context()), req)
		if err != nil {
			return err
		}
		var out struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(res.Value, &out); err != nil {
			return err
		}
		if res.Replayed {
			status = http.StatusOK
		}
		resp := map[string]any{"id": out.ID, "version": res.Version, "replayed": res.Replayed}
		if !res.Replayed { // 重放时"最后一条"可能已是别的进展，不附
			for _, f := range extra {
				if err := f(r, out.ID, resp); err != nil {
					return err
				}
			}
		}
		return core.WriteJSON(w, status, resp)
	}
}

// withProgress 在 progress 写入的响应里附上刚追加的那条进展（附录 B.2）。
func (h *handlers) withProgress(r *http.Request, id string, out map[string]any) error {
	return h.st.View(r.Context(), func(tx *store.Tx) error {
		ps, err := kitems.Progress(tx, id)
		if err == nil && len(ps) > 0 {
			out["progress"] = ps[len(ps)-1]
		}
		return err
	})
}

// Rejection 是事项上最近一次被拒绝的代理操作（退出码 23，§14.5）：决策待办与建议流据此标明"这次被拒绝的代答/处理"。
// 只带机制（操作者、原因、授权单 id），不含账本用户内容。
type Rejection struct {
	Seq   int64        `json:"seq"`
	At    time.Time    `json:"at"`
	Actor domain.Actor `json:"actor"`
	Code  domain.Code  `json:"code"`
	Why   string       `json:"why"`
	Grant string       `json:"grant,omitempty"`
}

// LastRejection 返回 needs_input、proposed 事项上仍未被处理的最近一次 23 拒绝；其后若有回答或阶段变化则视为已处理，返回 nil。
// 读的是内核已记的 command.rejected，不重新判定。
func LastRejection(tx *store.Tx, it domain.Item) (*Rejection, error) {
	if it.Phase != domain.PhaseNeedsInput && it.Phase != domain.PhaseProposed {
		return nil, nil
	}
	es, err := eventlog.ReadObject(tx, it.ID)
	if err != nil {
		return nil, err
	}
	for i := len(es) - 1; i >= 0; i-- {
		e := es[i].Event
		switch e.Type {
		case domain.EvDecisionResolved, domain.EvItemPhaseChanged:
			return nil, nil
		case domain.EvCommandRejected:
			var pl struct {
				Code  domain.Code `json:"code"`
				Exit  int         `json:"exit"`
				Why   string      `json:"why"`
				Grant string      `json:"grant"`
			}
			if json.Unmarshal(e.Payload, &pl) != nil || pl.Exit != domain.ExitBasisRequired {
				continue
			}
			return &Rejection{Seq: e.Seq, At: e.At, Actor: e.Actor, Code: pl.Code, Why: pl.Why, Grant: pl.Grant}, nil
		}
	}
	return nil, nil
}
