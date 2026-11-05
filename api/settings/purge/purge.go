// Package purge 是物理删除的 HTTP 路由（附录 B.2，设计 §6.15）：
//
//	POST /settings/purge   请求体 {"object": <事项或材料 id>}
//
// 路由里没有业务规则：请求体原样交给内核命令 purge.object（kernel/purge），授权、校验、墓碑和删除事件都在那里——
// 只有 Web 会话通过，delegate 得到 web_only（退出码 20，留痕），其余 token 得到退出码 4，不提供 CLI 入口。
// 响应就是内核结果（对象 id、种类、墓碑条数、blob 哈希），不含账本内容。
package purge

import (
	"encoding/json"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kpurge "github.com/xiaohei-info/awesome-personal-agent/kernel/purge"
)

// Routes 返回路由分组；x 须已由 kernel/purge.Register 登记命令。经 core.Deps.Groups 挂载。
func Routes(x *command.Executor) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "POST", Path: "/settings/purge", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			var in json.RawMessage
			if err := core.DecodeJSON(r, &in); err != nil {
				return err
			}
			res, err := x.Do(r.Context(), command.Request{Identity: p.Identity, Command: kpurge.Command, Input: in})
			if err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, res.Value)
		}})
	}
}
