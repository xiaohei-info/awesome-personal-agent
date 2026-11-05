// Package items 是事项分组的 HTTP 路由（附录 B.2，设计 §7.1、§14.3）：
// GET/POST /items、GET/PATCH /items/{id}、POST /items/{id}/transition|relations|revert。
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

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// IdempotencyHeader 携带幂等键；同键同输入重放首次结果，同键异输入返回 conflict。
	IdempotencyHeader = "Idempotency-Key"
	// IfMatchHeader 携带期望的对象版本（整数，可带引号）；不符返回 conflict。
	IfMatchHeader = "If-Match"
	// EvidenceHeader 携带 delegate 动作的证据。
	EvidenceHeader = "X-APA-Evidence"
)

// Routes 返回事项路由分组，交给 core.Deps.Groups。x 须已经 kitems.Register。
func Routes(x *command.Executor, st *store.Store) core.RouteGroup {
	h := &handlers{x: x, st: st}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/items", Handler: h.list})
		r.Handle(core.Route{Method: "GET", Path: "/items/{id}", Handler: h.get})
		r.Handle(core.Route{Method: "POST", Path: "/items", Handler: h.write("item.create", "", http.StatusCreated)})
		r.Handle(core.Route{Method: "PATCH", Path: "/items/{id}", Handler: h.write("item.update", "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/transition", Handler: h.write("item.transition", "id", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/relations", Handler: h.write("item.relate", "from", http.StatusOK)})
		r.Handle(core.Route{Method: "POST", Path: "/items/{id}/revert", Handler: h.write("item.revert", "id", http.StatusOK)})
	}
}

type handlers struct {
	x  *command.Executor
	st *store.Store
}

// readable：读事项对 observer 及以上开放；ext 与 webhook 凭据另有用途，不能读账本。
func readable(p *core.Principal) error {
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
	var its []domain.Item
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) { its, err = kitems.List(tx, f); return }); err != nil {
		return err
	}
	if its == nil {
		its = []domain.Item{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"items": its})
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	var (
		it   domain.Item
		rels []domain.Relation
	)
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		if it, err = kitems.Get(tx, r.PathValue("id")); err != nil {
			return err
		}
		rels, err = kitems.Relations(tx, it.ID)
		return err
	}); err != nil {
		return err
	}
	if rels == nil {
		rels = []domain.Relation{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"item": it, "relations": rels})
}

// write 把请求体原样交给命令。pathKey 非空时把路径 id 写入体的该键（体里自带同名键直接拒绝）。
func (h *handlers) write(cmd, pathKey string, status int) core.Handler {
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
			IdemKey: r.Header.Get(IdempotencyHeader), Evidence: r.Header.Get(EvidenceHeader)}
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
		return core.WriteJSON(w, status, map[string]any{"id": out.ID, "version": res.Version, "replayed": res.Replayed})
	}
}
