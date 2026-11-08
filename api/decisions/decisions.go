// Package decisions 是决策分组的 HTTP 路由（附录 B.2，设计 §7.2、§14.4）：
// POST /items/{id}/ask|decide|accept|dismiss。
//
// 路由里没有业务规则：写入全部映射到内核命令（decision.request、decision.answer、proposal.accept、proposal.dismiss），
// 授权（web_only 20、初筛未返回 22、运行身份 4、代答证据）、校验与"只认第一次回答"（6）都由命令管线判定，
// 被拒绝的错误由骨干渲染成统一错误体。本层只做 HTTP 到命令请求的搬运：
//   - 路径里的 id 注入请求体（体里不许再写 id）；
//   - Idempotency-Key 头 → 幂等键；If-Match 头 → 期望版本；
//   - X-APA-Evidence 头（缺省取体里的 evidence）→ 代答所需的本人原话；
//   - 事项的 Web 链接交给命令请求，由管线在 20/22 拒绝时随错误返回。
package decisions

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const (
	// IdempotencyHeader 携带幂等键；同键同输入重放首次结果，同键异输入返回 conflict。
	IdempotencyHeader = "Idempotency-Key"
	// IfMatchHeader 携带期望的对象版本（整数，可带引号）；不符返回 conflict。
	IfMatchHeader = "If-Match"
)

// Routes 返回决策路由分组，交给 core.Deps.Groups。x 须已经 decisions.Register 与 items.Register。
// webBase 是 Web 界面的根地址（如 http://127.0.0.1:7777），事项链接为 webBase/items/{id}；
// 空串时取请求的 Host，指向同一个服务。
func Routes(x *command.Executor, webBase string) core.RouteGroup {
	h := &handlers{x: x, webBase: strings.TrimRight(webBase, "/")}
	return func(r *core.Router) {
		for path, cmd := range map[string]string{
			"ask": "decision.request", "decide": "decision.answer",
			"accept": "proposal.accept", "dismiss": "proposal.dismiss",
		} {
			r.Handle(core.Route{Method: "POST", Path: "/items/{id}/" + path, Handler: h.write(cmd)})
		}
	}
}

type handlers struct {
	x       *command.Executor
	webBase string
}

func (h *handlers) webURL(r *http.Request) string {
	base := h.webBase
	if base == "" {
		base = "http://" + r.Host
	}
	return base + "/items/" + url.PathEscape(r.PathValue("id"))
}

func (h *handlers) write(cmd string) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		var body map[string]json.RawMessage
		if err := core.DecodeJSON(r, &body); err != nil {
			return err
		}
		if body == nil { // JSON null
			return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
		}
		for k := range body {
			if strings.EqualFold(k, "id") {
				return domain.NewError(domain.CodeInvalidRequest, "field "+k+" comes from the URL and cannot be in the body", "remove it from the body")
			}
		}
		req := command.Request{Identity: p.Identity, Command: cmd, WebURL: h.webURL(r),
			IdemKey: r.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant}
		// evidence 是请求元数据，不是命令输入；命令对未知字段严格拒绝，所以从体里取出。
		if raw, ok := body["evidence"]; ok {
			delete(body, "evidence")
			if req.Evidence == "" {
				if err := json.Unmarshal(raw, &req.Evidence); err != nil {
					return domain.NewError(domain.CodeInvalidRequest, "evidence must be a string", "send the owner's words as a string")
				}
			}
		}
		body["id"], _ = json.Marshal(r.PathValue("id"))
		in, err := json.Marshal(body)
		if err != nil {
			return err
		}
		req.Input = in
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
		return core.WriteJSON(w, http.StatusOK, map[string]any{"id": r.PathValue("id"), "version": res.Version, "replayed": res.Replayed})
	}
}
