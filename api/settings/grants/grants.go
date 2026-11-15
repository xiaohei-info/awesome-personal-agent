// Package grants 是授权单的路由（设计 §6.13、§14.5，附录 B.2）：
//
//	POST /settings/grants               签发（只有 Web 登录）
//	POST /settings/grants/{id}/revoke   撤销（只有 Web 登录）
//	GET  /settings/grants               Web 查看全部授权单，含已撤销与已过期
//	PUT  /settings/agents/{harness}/grant  替换该 Agent 的长期授权单（只有 Web 登录）
//	GET  /grants                        delegate 与运行凭据读取自己能用的有效授权单（只读）
//
// 路由里没有业务规则：校验、记录与 grant.issued / grant.revoked 事件在 kernel/policy。
// 三条 /settings 命令都按"改设置"登记，由命令管线判定——Web 会话通过，delegate 得到 web_only（退出码 20，留痕），
// 其余 token 得到退出码 4；agent 与能力包因此既不能创建，也不能撤销或延长授权单。
// 本节点不放行任何命令：授权单只是记录，X-APA-Grant 由后续节点读取。
package grants

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 命令名。
const (
	CmdIssue   = "grant.issue"
	CmdRevoke  = "grant.revoke"
	CmdList    = "grant.list"
	CmdReplace = "grant.replace"
)

type replaceInput struct {
	Agent string `json:"agent"`
	policy.GrantInput
}

type revokeInput struct {
	ID     string `json:"id"`
	Reason string `json:"reason,omitempty"`
}

// invalid 把内核的 ErrInvalid 映射成 invalid_request（退出码 2）。
func invalid(err error) error {
	if errors.Is(err, policy.ErrInvalid) {
		return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the grant and send it again; nothing was saved")
	}
	return err
}

// Mount 在 x 上登记三条命令，并返回路由分组；挂到 core.Deps.Groups 即生效。st 用于 GET /grants 的只读查询。
func Mount(x *command.Executor, a *auth.Auth, st *store.Store) (core.RouteGroup, error) {
	// 对象 id 不能为空：被拒绝的越权尝试要以它为对象记 command.rejected。
	noObject := func(*store.Tx, json.RawMessage) (authz.Object, error) {
		return authz.Object{ID: "settings:grants"}, nil
	}
	for _, s := range []command.Spec{
		{Name: CmdIssue, Action: authz.ActionChangeSettings, Object: noObject,
			Apply: func(c *command.Ctx) (any, error) {
				var in policy.GrantInput
				if err := json.Unmarshal(c.Input, &in); err != nil {
					return nil, domain.NewError(domain.CodeInvalidRequest, "input is not a valid grant", "send the grant in the documented shape")
				}
				g, err := policy.IssueGrant(c.Tx, a, c.Caller.Actor, c.Now(), in)
				return g, invalid(err)
			}},
		{Name: CmdRevoke, Action: authz.ActionChangeSettings, Object: noObject,
			Apply: func(c *command.Ctx) (any, error) {
				var in revokeInput
				if err := json.Unmarshal(c.Input, &in); err != nil {
					return nil, domain.NewError(domain.CodeInvalidRequest, "input is not valid JSON", "fix the request and retry")
				}
				return policy.RevokeGrant(c.Tx, c.Caller.Actor, c.Now(), in.ID, in.Reason)
			}},
		{Name: CmdReplace, Action: authz.ActionChangeSettings, Object: noObject,
			Apply: func(c *command.Ctx) (any, error) {
				var in replaceInput
				if err := json.Unmarshal(c.Input, &in); err != nil {
					return nil, domain.NewError(domain.CodeInvalidRequest, "input is not a valid grant", "send the grant in the documented shape")
				}
				revoked, g, err := policy.ReplaceAgentGrant(c.Tx, a, c.Caller.Actor, c.Now(), in.Agent, in.GrantInput)
				return map[string]any{"grant": g, "revoked": revoked}, invalid(err)
			}},
		{Name: CmdList, Action: authz.ActionChangeSettings, Object: noObject,
			Apply: func(c *command.Ctx) (any, error) {
				gs, err := policy.ListGrants(c.Tx)
				return map[string]any{"grants": gs}, err
			}},
	} {
		if err := x.Register(s); err != nil {
			return nil, err
		}
	}
	run := func(cmd string, status int, in func(*http.Request) (any, error)) core.Handler {
		return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			v, err := in(r)
			if err != nil {
				return err
			}
			raw, err := json.Marshal(v)
			if err != nil {
				return err
			}
			res, err := x.Do(r.Context(), command.Request{Identity: p.Identity, Command: cmd, Input: raw})
			if err != nil {
				return err
			}
			return core.WriteJSON(w, status, res.Value)
		}
	}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/settings/grants", MaxBody: 1,
			Handler: run(CmdList, http.StatusOK, func(*http.Request) (any, error) { return struct{}{}, nil })})
		r.Handle(core.Route{Method: "POST", Path: "/settings/grants", MaxBody: 8 << 10,
			Handler: run(CmdIssue, http.StatusCreated, func(r *http.Request) (any, error) {
				var in json.RawMessage
				return in, core.DecodeJSON(r, &in)
			})})
		r.Handle(core.Route{Method: "POST", Path: "/settings/grants/{id}/revoke", MaxBody: 4 << 10,
			Handler: run(CmdRevoke, http.StatusOK, func(r *http.Request) (any, error) {
				var b revokeInput
				if err := core.DecodeJSON(r, &b); err != nil {
					return nil, err
				}
				return revokeInput{ID: r.PathValue("id"), Reason: b.Reason}, nil
			})})
		r.Handle(core.Route{Method: "PUT", Path: "/settings/agents/{harness}/grant", MaxBody: 8 << 10,
			Handler: run(CmdReplace, http.StatusOK, func(r *http.Request) (any, error) {
				var in replaceInput
				if err := core.DecodeJSON(r, &in.GrantInput); err != nil {
					return nil, err
				}
				in.Agent = r.PathValue("harness")
				return in, nil
			})})
		r.Handle(core.Route{Method: "GET", Path: "/grants", MaxBody: 1, Handler: mine(a, st)})
	}, nil
}

// mine 只给 delegate 凭据与运行凭据：其余一律退出码 4（含 Web 会话——Web 用 /settings/grants）。
// delegate 得到该凭据名下的单，加上签给凭据所属 Agent 的 agent 式单；凭据一旦吊销，认证阶段已拒绝。
// 运行凭据只得到签给运行记录所属 Agent、且允许后台使用的单（§14.5）；唤醒租约期间被降级的 delegate 照 delegate 列出，但用不了。
func mine(a *auth.Auth, st *store.Store) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		run := p.Caller.Level == authz.LevelRun && p.Caller.RunHarness != ""
		if p.Web || (p.Caller.Kind != auth.Delegate && !run) {
			return domain.NewError(domain.CodePermissionDenied, "only a delegate credential or a background run can list its grants", "do not retry; grants are issued and viewed by the owner in the Web UI")
		}
		out := []policy.Grant{}
		err := st.View(r.Context(), func(tx *store.Tx) error {
			now := clock.Now()
			if run {
				gs, err := policy.AgentGrants(tx, p.Caller.RunHarness, now)
				for _, g := range gs {
					if g.Grantee.Background {
						out = append(out, g)
					}
				}
				return err
			}
			cred, err := a.Resolve(tx, auth.ParseBearer(r.Header.Get("Authorization")))
			if err != nil {
				return err
			}
			if out, err = policy.ActiveGrants(tx, a, cred.ID, now); err != nil || cred.Harness == "" {
				return err
			}
			gs, err := policy.AgentGrants(tx, cred.Harness, now)
			out = append(out, gs...)
			return err
		})
		if err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, map[string]any{"grants": out})
	}
}
