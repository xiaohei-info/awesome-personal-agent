// Package agentsmd 是 AGENTS.md 分组的 HTTP 路由（附录 B.2，设计 §12.5）：
//
//	GET /agents-md?repo        取最新渲染与最近一次检查
//	POST /agents-md/check      上报工作区文件的 hash（或内容、或缺失），返回三态
//
// 路由里没有业务规则：渲染、三态判定、漂移事项都在 kernel/agentsmd，本层只做认证后的参数搬运。
// 本层不写工作区文件（agents.sync 是 CLI 的事，内核只说该写什么）。
package agentsmd

import (
	"encoding/json"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// IdempotencyHeader 是 POST 可选的幂等键头。
const IdempotencyHeader = "Idempotency-Key"

type api struct {
	x   *command.Executor
	svc *agentsmd.Service
}

// Routes 返回 AGENTS.md 路由分组；svc 须已由 kernel/agentsmd.New 在 x 上登记命令。经 core.Deps.Groups 挂载。
func Routes(x *command.Executor, svc *agentsmd.Service) core.RouteGroup {
	a := &api{x, svc}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/agents-md", Handler: readable(a.get)})
		r.Handle(core.Route{Method: "POST", Path: "/agents-md/check", Handler: readable(a.check)})
	}
}

// readable：observer 及以上可读；ext 与 webhook 凭据另有用途，不能读账本（与 api/projects 一致）。
func readable(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
			return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read AGENTS.md", "use an owner or agent credential")
		}
		return h(w, r, p)
	}
}

func (a *api) get(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		return domain.NewError(domain.CodeInvalidRequest, "repo is required", "pass ?repo=<remote url>")
	}
	v, err := a.svc.Get(r.Context(), repo)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, v)
}

func (a *api) check(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in json.RawMessage
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	res, err := a.x.Do(r.Context(), command.Request{Identity: p.Identity, Command: "agents.check", Input: in, IdemKey: r.Header.Get(IdempotencyHeader)})
	if err != nil {
		return err
	}
	var vd agentsmd.Verdict
	if err := json.Unmarshal(res.Value, &vd); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, vd)
}
