// Package retention 是来源原文保留天数的 HTTP 路由（附录 B.2，设计 §6.15）：
//
//	PUT /settings/sources/{id}/content-retention   请求体 {"days": 90}；null 为不清理
//	GET /settings/retention                        运行目录、请求体、判断记录的保留天数与最近一次清理的报数
//	PUT /settings/retention                        请求体 {"run_days"?, "ingest_days"?, "judgment_days"?}，0 为不清理
//
// 路由里没有业务规则：取值校验、授权与事件都在内核命令 source.set_content_retention。
// 只有 Web 会话能写：delegate 得到 web_only（退出码 20，留痕），其余凭据得到退出码 4。
// 响应是来源登记里的保留天数 {"days": 90|null}。清除本身由后台维护循环做（kernel/retention），不在路由里。
package retention

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/retention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// view 是 GET /settings/retention 的响应：生效的天数、没改过时的默认天数、最近一次清理（从没清理过为 null）。
type view struct {
	retention.Settings
	Defaults  retention.Settings `json:"defaults"`
	LastSweep *retention.Report  `json:"last_sweep"`
}

// Routes 返回路由分组；x 须已由 registry.Register 与 retention.Register 登记命令，def 是没改过时生效的天数。经 core.Deps.Groups 挂载。
func Routes(x *command.Executor, st *store.Store, def retention.Settings) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/settings/retention", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
				return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read settings", "use an owner or agent credential")
			}
			v := view{Defaults: def}
			err := st.View(r.Context(), func(tx *store.Tx) (err error) {
				if v.Settings, err = retention.Effective(tx, def); err != nil {
					return err
				}
				v.LastSweep, err = retention.Last(tx)
				return err
			})
			if err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, v)
		}})
		r.Handle(core.Route{Method: "PUT", Path: "/settings/retention", MaxBody: 1 << 10, Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			raw, err := io.ReadAll(r.Body)
			if err != nil {
				return err
			}
			res, err := x.Do(r.Context(), command.Request{Identity: p.Identity, Command: retention.CmdSet, Input: raw, Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL})
			if err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, json.RawMessage(res.Value))
		}})
		r.Handle(core.Route{Method: "PUT", Path: "/settings/sources/{id}/content-retention", MaxBody: 1 << 10, Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			var in map[string]json.RawMessage
			if err := core.DecodeJSON(r, &in); err != nil {
				return err
			}
			if in == nil {
				return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send {\"days\": <n or null>}")
			}
			in["id"], _ = json.Marshal(r.PathValue("id"))
			raw, err := json.Marshal(in)
			if err != nil {
				return err
			}
			res, err := x.Do(r.Context(), command.Request{Identity: p.Identity, Command: registry.CmdSetRetention, Input: raw, WebURL: p.WebURL})
			if err != nil {
				return err
			}
			var s domain.Source
			if err := json.Unmarshal(res.Value, &s); err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, map[string]*int{"days": s.ContentRetentionDays})
		}})
	}
}
