// Package followmsg 是跟进消息分组的 HTTP 路由（附录 B.2、设计 §9.4）：
// POST /messages/{id}/handback 与 GET /items/{id}/messages。
//
// 路由里没有业务规则：交回映射到内核命令 followmsg.handback，归属（只有负责该事项的执行者，4）、
// 理由必填、同一消息只能交回一次（6）都由命令管线判定，本层只把路径 id 注入请求体的 message；
// 读取的过滤由 kernel/allowlist 判定（§14.6），本层只挡不能读账本的凭据。
package followmsg

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/allowlist"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// IdempotencyHeader 携带幂等键；同键同输入重放首次结果。
const IdempotencyHeader = "Idempotency-Key"

// Routes 返回跟进消息路由分组，交给 core.Deps.Groups。x 须已经登记 followmsg 命令。
func Routes(x *command.Executor) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/items/{id}/messages", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
				return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read attached messages", "use an owner or agent credential")
			}
			var msgs []allowlist.Attached
			if err := x.Store().View(r.Context(), func(tx *store.Tx) (err error) {
				msgs, err = allowlist.Messages(tx, r.PathValue("id"), p.Caller.Level)
				return err
			}); err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, map[string]any{"messages": msgs})
		}})
		r.Handle(core.Route{Method: "POST", Path: "/messages/{id}/handback", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			var body map[string]json.RawMessage
			if err := core.DecodeJSON(r, &body); err != nil {
				return err
			}
			if body == nil {
				return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
			}
			for k := range body {
				if strings.EqualFold(k, "message") || strings.EqualFold(k, "id") {
					return domain.NewError(domain.CodeInvalidRequest, "field "+k+" comes from the URL and cannot be in the body", "remove it from the body")
				}
			}
			body["message"], _ = json.Marshal(r.PathValue("id"))
			in, err := json.Marshal(body)
			if err != nil {
				return err
			}
			res, err := x.Do(context.WithoutCancel(r.Context()), command.Request{Identity: p.Identity, Command: "followmsg.handback", Input: in, IdemKey: r.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL})
			if err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, map[string]any{"message": r.PathValue("id"), "state": "handed_back", "version": res.Version, "replayed": res.Replayed})
		}})
	}
}
