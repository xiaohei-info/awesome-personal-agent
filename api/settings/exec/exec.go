// Package exec 是执行者与声明设置分组的 HTTP 路由（附录 B.2，设计 §14.7、§14.12）：
//
//	GET/POST /settings/executors、DELETE /settings/executors/{id}        执行者清单
//	GET /settings/declarations、POST /settings/declarations/{harness}/{rev}/confirm|reject   待确认声明
//	GET/POST /settings/commands                                           可执行命令（无头运行声明）的直接登记
//	GET/PUT /settings/projects/{id}/allowed-executors                     项目允许的执行者
//
// 路由里没有业务规则：写请求原样交给内核命令（kernel/executors、kernel/projects），授权、校验、事件都在那里——
// 只有 Web 会话通过，delegate 得到 web_only（退出码 20，留痕），其余 token 得到退出码 4。
// 读请求不经授权命令，所以本层只放行 Web 会话。
//
// /settings/commands 对应 executor.register：本人直接登记一份声明，立即生效（§14.12）。
package exec

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kexec "github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type api struct {
	st *store.Store
	x  *command.Executor
}

// Routes 返回路由分组；x 须已由 kernel/executors.Register 与 kernel/projects.Register 登记命令。经 core.Deps.Groups 挂载。
func Routes(st *store.Store, x *command.Executor) core.RouteGroup {
	a := &api{st, x}
	return func(r *core.Router) {
		for _, rt := range []core.Route{
			{Method: "GET", Path: "/settings/executors", Handler: web(a.listExecutors)},
			{Method: "POST", Path: "/settings/executors", Handler: a.putExecutor},
			{Method: "DELETE", Path: "/settings/executors/{id}", MaxBody: 1, Handler: a.removeExecutor},
			{Method: "GET", Path: "/settings/declarations", Handler: web(a.declarations(""))},
			{Method: "POST", Path: "/settings/declarations/{harness}/{rev}/confirm", MaxBody: 1, Handler: a.decide("executor.confirm")},
			{Method: "POST", Path: "/settings/declarations/{harness}/{rev}/reject", MaxBody: 1, Handler: a.decide("executor.reject")},
			{Method: "GET", Path: "/settings/commands", Handler: web(a.declarations(kexec.StateActive))},
			{Method: "POST", Path: "/settings/commands", Handler: a.registerCommand},
			{Method: "GET", Path: "/settings/projects/{id}/allowed-executors", Handler: web(a.getAllowed)},
			{Method: "PUT", Path: "/settings/projects/{id}/allowed-executors", MaxBody: 64 << 10, Handler: a.setAllowed},
		} {
			r.Handle(rt)
		}
	}
}

// web 只放行 Web 会话：设置的读取不走授权命令，agent 不需要看。
func web(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if !p.Web {
			return domain.NewError(domain.CodeWebOnly, "settings can only be used by the owner in the Web UI", "ask the owner to change it in the Web settings")
		}
		return h(w, r, p)
	}
}

// run 以请求的凭据执行一条内核命令并渲染结果。
func (a *api) run(w http.ResponseWriter, r *http.Request, p *core.Principal, status int, cmd string, in any) error {
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	res, err := a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: cmd, Input: raw})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, status, res.Value)
}

func (a *api) listExecutors(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	in := map[string]string{}
	if proj := r.URL.Query().Get("project"); proj != "" {
		in["project"] = proj
	}
	return a.run(w, r, p, http.StatusOK, "executor.list", in)
}

// putExecutor / registerCommand：请求体就是内核命令的输入，由内核严格校验。
func (a *api) putExecutor(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in json.RawMessage
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	return a.run(w, r, p, http.StatusOK, "executor.put", in)
}

func (a *api) registerCommand(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in json.RawMessage
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	return a.run(w, r, p, http.StatusCreated, "executor.register", in)
}

func (a *api) removeExecutor(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	return a.run(w, r, p, http.StatusOK, "executor.remove", map[string]string{"id": r.PathValue("id")})
}

// declarations 列出某状态的声明；state 为空时取查询参数，再缺省为待确认。
func (a *api) declarations(state string) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		st := state
		if st == "" {
			st = r.URL.Query().Get("state")
		}
		in := map[string]string{}
		if st != "" {
			in["state"] = st
		}
		return a.run(w, r, p, http.StatusOK, "executor.pending", in)
	}
}

func (a *api) decide(cmd string) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		rev, err := strconv.ParseInt(r.PathValue("rev"), 10, 64)
		if err != nil {
			return domain.NewError(domain.CodeInvalidRequest, "rev must be an integer", "use the rev shown in the declaration list")
		}
		return a.run(w, r, p, http.StatusOK, cmd, map[string]any{"harness": r.PathValue("harness"), "rev": rev})
	}
}

func (a *api) getAllowed(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var proj domain.Project
	if err := a.st.View(r.Context(), func(tx *store.Tx) (err error) { proj, err = kp.Get(tx, r.PathValue("id")); return }); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, allowedView(proj))
}

// setAllowed：请求体 {"executors": null|[...]}——null 不限制，[] 只能指派给本人（§14.7）。
func (a *api) setAllowed(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var body struct {
		Executors json.RawMessage `json:"executors"`
	}
	if err := core.DecodeJSON(r, &body); err != nil {
		return err
	}
	id := r.PathValue("id")
	in := map[string]any{"id": id}
	if body.Executors != nil { // 缺字段不能被当成 null（不限制）；交给内核报 invalid_request
		in["executors"] = body.Executors
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return err
	}
	if _, err := a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: "project.allowed_executors.set", Input: raw}); err != nil {
		return err
	}
	return a.getAllowed(w, r, p)
}

func allowedView(p domain.Project) map[string]any {
	return map[string]any{"id": p.ID, "allowed_executors": p.AllowedExecutors}
}
