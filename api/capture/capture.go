// Package capture 是对话候选捕获的 HTTP 路由（附录 B.2，设计 §9.3、§11.2）：POST /capture。
//
// 路由里没有业务规则：请求体原样交给内核命令 inbound.capture，授权、项目是否开启捕获、运行身份下不生效、
// 增量去重与后续的候选抽取都在内核（kernel/inbound）。本层只把 HTTP 搬成命令请求；
// 候选抽取在提交之后异步进行，响应不等模型。会话取自 X-APA-Session 头（与审计同源）。
//
// 请求体：{"text", "project"?, "path"?, "host"?}。project 缺省时按 path+host 解析当前目录所在项目。
// 响应 200：{"captured": bool, "reason"?, "event"?, "project"?}；未开启捕获或运行身份下，captured 为 false，不是错误。
package capture

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
)

// IdempotencyHeader 携带幂等键。
const IdempotencyHeader = "Idempotency-Key"

// Routes 返回捕获路由分组，交给 core.Deps.Groups。x 须已经由 inbound.Service.Register 登记捕获命令。
func Routes(x *command.Executor) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "POST", Path: "/capture", Handler: func(w http.ResponseWriter, req *http.Request, p *core.Principal) error {
			var body map[string]any
			if err := core.DecodeJSON(req, &body); err != nil {
				return err
			}
			if body == nil {
				return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a single well-formed JSON object")
			}
			in, err := json.Marshal(body)
			if err != nil {
				return err
			}
			res, err := x.Do(context.WithoutCancel(req.Context()), command.Request{Identity: p.Identity, Command: inbound.CmdCapture,
				Input: in, IdemKey: req.Header.Get(IdempotencyHeader), Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL})
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", "application/json")
			_, err = w.Write(res.Value)
			return err
		}})
	}
}
