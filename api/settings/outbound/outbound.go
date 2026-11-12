// Package outbound 是外发范围设置的 HTTP 路由（附录 B.2，设计 §14.14）：
//
//	GET/PUT /settings/sources/{id}/outbound               来源的外发范围 {"local", "external"}
//	GET/PUT /settings/projects/{id}/capture-outbound      项目对话捕获的外发范围 {"value"}
//
// 路由里没有业务规则：取值校验、授权与事件都在内核命令（source.set_outbound、project.capture_outbound.set）。
// 只有 Web 会话能写：delegate 得到 web_only（退出码 20，留痕），其余凭据得到退出码 4。读取只放行 Web 会话（同其他设置）。
// external、value 为 null 表示未选择；外部范围没有缺省值。
package outbound

import (
	"encoding/json"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type api struct {
	st *store.Store
	x  *command.Executor
}

// Routes 返回路由分组；x 须已由 registry.Register 与 projects.Register 登记命令。经 core.Deps.Groups 挂载。
func Routes(st *store.Store, x *command.Executor) core.RouteGroup {
	a := &api{st, x}
	return func(r *core.Router) {
		for _, rt := range []core.Route{
			{Method: "GET", Path: "/settings/sources/{id}/outbound", Handler: web(a.getSource)},
			{Method: "PUT", Path: "/settings/sources/{id}/outbound", MaxBody: 1 << 10, Handler: a.putSource},
			{Method: "GET", Path: "/settings/projects/{id}/capture-outbound", Handler: web(a.getProject)},
			{Method: "PUT", Path: "/settings/projects/{id}/capture-outbound", MaxBody: 1 << 10, Handler: a.putProject},
		} {
			r.Handle(rt)
		}
	}
}

func web(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if !p.Web {
			return domain.NewError(domain.CodeWebOnly, "settings can only be used by the owner in the Web UI", "ask the owner to change it in the Web settings")
		}
		return h(w, r, p)
	}
}

func (a *api) getSource(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var s domain.Source
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { s, err = registry.Get(tx, r.PathValue("id")); return }); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, s.Outbound)
}

func (a *api) getProject(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var p domain.Project
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { p, err = kp.Get(tx, r.PathValue("id")); return }); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"value": p.CaptureOutbound})
}

// put 把请求体的对象加上路径里的 id 交给命令；字段由命令严格校验。
func (a *api) put(r *http.Request, p *core.Principal, cmd string) error {
	var in map[string]json.RawMessage
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	if in == nil {
		return domain.NewError(domain.CodeInvalidRequest, "request body must be a JSON object", "send a JSON object")
	}
	in["id"], _ = json.Marshal(r.PathValue("id"))
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: cmd, Input: raw, Evidence: p.Evidence, Grant: p.Grant, WebURL: p.WebURL})
	return err
}

func (a *api) putSource(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := a.put(r, p, registry.CmdSetOutbound); err != nil {
		return err
	}
	return a.getSource(w, r, p)
}

func (a *api) putProject(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := a.put(r, p, kp.CmdSetCaptureOutbound); err != nil {
		return err
	}
	return a.getProject(w, r, p)
}
