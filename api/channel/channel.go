// Package channel 是回答回流的 HTTP 路由（附录 B.2，设计 §10.3）：
//
//	POST /deliveries/{id}/answer   渠道 ext 转达回答（渠道 ext 密钥）
//	GET  /forms/{令牌}              旧的一次性表单链接：302 到需要登录的 /decisions，不兑现令牌
//
// 路由里没有业务规则：回答人名单、ref 一致、版本作废都由 kernel/channel 的命令判定，被拒绝的错误由骨干渲染成
// 统一错误体。本层只做 HTTP 到命令请求的搬运：路径里的 id 注入输入（体里不许再写）。
package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// IdempotencyHeader 携带幂等键；同键同输入重放首次结果。
const IdempotencyHeader = "Idempotency-Key"

// Routes 返回渠道路由分组，交给 core.Deps.Groups。x 须已经 channel.Register。now 为 nil 用系统时钟，须与 x 同一时钟。
func Routes(x *command.Executor, st *store.Store, now func() time.Time) core.RouteGroup {
	if now == nil {
		now = clock.Now
	}
	h := &handlers{x: x, st: st, now: now}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "POST", Path: "/deliveries/{id}/answer", Handler: h.answer})
		// 旧的一次性表单链接只作导航：跳到需要登录的"等你决定"，令牌不再兑现（§14.3）。
		r.HandleRoot(core.Route{Method: "GET", Path: "/forms/{token}", Auth: core.AuthNone, Handler: h.formRedirect})
	}
}

type handlers struct {
	x   *command.Executor
	st  *store.Store
	now func() time.Time
}

// inject 解析 JSON 对象体，拒绝体里重复写路径参数，再把路径参数按命令输入的字段名写进去。
func inject(r *http.Request, field, value string) (json.RawMessage, error) {
	var body map[string]json.RawMessage
	if err := core.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	if body == nil { // JSON null
		return nil, domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
	}
	for k := range body {
		if strings.EqualFold(k, field) {
			return nil, domain.NewError(domain.CodeInvalidRequest, "field "+k+" comes from the URL and cannot be in the body", "remove it from the body")
		}
	}
	body[field], _ = json.Marshal(value)
	return json.Marshal(body)
}

func (h *handlers) answer(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in, err := inject(r, "delivery", r.PathValue("id"))
	if err != nil {
		return err
	}
	res, err := h.x.Do(context.WithoutCancel(r.Context()), command.Request{Identity: p.Identity, Command: channel.CmdAnswer,
		Input: in, IdemKey: r.Header.Get(IdempotencyHeader)})
	if err != nil {
		return err
	}
	return write(w, res)
}

// write 回 {"id": 事项, "replayed": 是否幂等重放}，不许被缓存。
func write(w http.ResponseWriter, res command.Result) error {
	var out struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(res.Value, &out); err != nil {
		return err
	}
	w.Header().Set("Cache-Control", "no-store")
	return core.WriteJSON(w, http.StatusOK, map[string]any{"id": out.ID, "replayed": res.Replayed})
}

// formRedirect 把旧表单链接带到 Web 的"等你决定"：持有链接不能证明是本人，回答要登录后在 Web 或渠道卡片上做。
func (h *handlers) formRedirect(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	http.Redirect(w, r, "/decisions", http.StatusFound)
	return nil
}
