// Package capture 是对话候选捕获的 HTTP 路由（附录 B.2，设计 §9.3、§11.2）：POST /capture。
//
// 路由里没有业务规则：请求体原样交给内核命令 inbound.capture，授权、项目的捕获字段、运行身份下不生效、
// 增量去重与后续的候选抽取都在内核（kernel/inbound）。本层只把 HTTP 搬成命令请求；
// 候选抽取在提交之后异步进行，响应不等模型。会话取自 X-APA-Session 头（与审计同源）。
//
// 请求体：{"text", "project"?, "path"?, "host"?, "failures"?, "capture_schema"?, "speaker_role"?, "origin_type"?, "status_hint"?}（failures 是插件补报的没送达的失败）。project 缺省时按 path+host 解析当前目录所在项目。
// capture_schema 为 1 表示插件标注了发言者角色（human、assistant、system）、来源类型（external_chat、assistant_output、
// system_notice、cron_echo、background_internal）与可选的状态提示（open、resolved）；只有外部人发起的轮次进入候选抽取，
// 其余的过滤在内核（kernel/inbound，附录 B.2、B.6）。
// 响应 200：{"captured": bool, "reason"?, "event"?, "project"?}；没有项目、项目捕获被关掉、运行身份下、
// 不是外部人发起的轮次（reason 为 non_external_origin）或本轮只是确认已完成（already_resolved），captured 为 false，不是错误。
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
				Input: in, IdemKey: req.Header.Get(IdempotencyHeader), WebURL: p.WebURL})
			if err != nil {
				return err
			}
			w.Header().Set("Content-Type", "application/json")
			_, err = w.Write(res.Value)
			return err
		}})
	}
}
