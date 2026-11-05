// Package credentials 是 /settings/tokens 与 /settings/ext-keys 两组路由（设计 §14.2、§14.3，附录 B.2）：
// 签发、列出、吊销长期凭据，明文只在签发响应里出现一次。
//
// 路由不含授权规则：三条命令都按"改设置"登记，由命令管线判定——只有 Web 会话通过，
// delegate 得到 web_only（退出码 20，留痕），其余 token 得到退出码 4，ext/webhook 密钥一律拒绝。
// 命令不接受幂等键，因此明文不会进入幂等结果表；账本里只有哈希。
package credentials

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 命令名。
const (
	CmdIssue  = "credential.issue"
	CmdList   = "credential.list"
	CmdRevoke = "credential.revoke"
)

// group 是一组路由管理的凭据种类。
type group struct {
	path  string
	kinds []auth.Kind
}

var groups = []group{
	{"tokens", []auth.Kind{auth.Observer, auth.Worker, auth.Delegate}},
	{"ext-keys", []auth.Kind{auth.Ext}},
}

func (g group) has(k auth.Kind) bool { return slices.Contains(g.kinds, k) }

func groupOf(name string) (group, bool) {
	for _, g := range groups {
		if g.path == name {
			return g, true
		}
	}
	return group{}, false
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

// input 是命令输入；group 由路由写入，不来自请求体。
type input struct {
	Group    string    `json:"group"`
	ID       string    `json:"id,omitempty"`
	Kind     auth.Kind `json:"kind,omitempty"`
	Label    string    `json:"label,omitempty"`
	Executor string    `json:"executor,omitempty"`
	Scope    string    `json:"scope,omitempty"`
}

func parse(raw json.RawMessage) (input, group, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return in, group{}, invalid("input is not valid JSON")
	}
	g, ok := groupOf(in.Group)
	if !ok {
		return in, g, invalid("unknown credential group")
	}
	return in, g, nil
}

// Mount 在 x 上登记三条命令，并返回路由分组；挂到 core.Deps.Groups 即生效。
func Mount(x *command.Executor, a *auth.Auth) (core.RouteGroup, error) {
	// 对象 id 不能为空：被拒绝的越权尝试要以它为对象记 command.rejected。
	noObject := func(*store.Tx, json.RawMessage) (authz.Object, error) {
		return authz.Object{ID: "settings:credentials"}, nil
	}
	for _, s := range []command.Spec{
		{Name: CmdIssue, Action: authz.ActionChangeSettings, Object: noObject,
			Validate: func(_ *store.Tx, _ authz.Object, raw json.RawMessage) error {
				in, g, err := parse(raw)
				if err != nil {
					return err
				}
				if !g.has(in.Kind) {
					return invalid("kind is not valid for /settings/" + g.path)
				}
				return nil
			},
			Apply: func(c *command.Ctx) (any, error) {
				in, _, _ := parse(c.Input)
				cred, secret, err := a.Issue(c.Tx, auth.Spec{Kind: in.Kind, Label: in.Label, Executor: in.Executor, Scope: in.Scope})
				if err != nil {
					return nil, err
				}
				return map[string]any{"credential": cred, "secret": secret}, nil
			}},
		{Name: CmdList, Action: authz.ActionChangeSettings, Object: noObject,
			Validate: func(_ *store.Tx, _ authz.Object, raw json.RawMessage) error { _, _, err := parse(raw); return err },
			Apply: func(c *command.Ctx) (any, error) {
				_, g, _ := parse(c.Input)
				all, err := a.List(c.Tx)
				if err != nil {
					return nil, err
				}
				out := []auth.Credential{}
				for _, cr := range all {
					if g.has(cr.Kind) {
						out = append(out, cr)
					}
				}
				return map[string]any{"credentials": out}, nil
			}},
		{Name: CmdRevoke, Action: authz.ActionChangeSettings, Object: noObject,
			Validate: func(_ *store.Tx, _ authz.Object, raw json.RawMessage) error { _, _, err := parse(raw); return err },
			Apply: func(c *command.Ctx) (any, error) {
				in, g, _ := parse(c.Input)
				all, err := a.List(c.Tx)
				if err != nil {
					return nil, err
				}
				// 只能吊销本组的凭据：tokens 路由碰不到 ext 密钥，反之亦然。
				if !slices.ContainsFunc(all, func(cr auth.Credential) bool { return cr.ID == in.ID && g.has(cr.Kind) }) {
					return nil, domain.NewError(domain.CodeNotFound, "credential not found", "list credentials and retry")
				}
				if err := a.Revoke(c.Tx, in.ID); err != nil {
					return nil, err
				}
				if all, err = a.List(c.Tx); err != nil {
					return nil, err
				}
				for _, cr := range all {
					if cr.ID == in.ID {
						return map[string]any{"credential": cr}, nil
					}
				}
				return nil, domain.NewError(domain.CodeInternal, "revoked credential vanished", "retry")
			}},
	} {
		if err := x.Register(s); err != nil {
			return nil, err
		}
	}
	return func(r *core.Router) {
		for _, g := range groups {
			base := "/settings/" + g.path
			r.Handle(core.Route{Method: "GET", Path: base, MaxBody: 1, Handler: run(x, CmdList, g, http.StatusOK, nil)})
			r.Handle(core.Route{Method: "POST", Path: base, MaxBody: 4 << 10, Handler: run(x, CmdIssue, g, http.StatusCreated, body)})
			r.Handle(core.Route{Method: "DELETE", Path: base + "/{id}", MaxBody: 1, Handler: run(x, CmdRevoke, g, http.StatusOK, func(r *http.Request, in *input) error {
				in.ID = r.PathValue("id")
				return nil
			})})
		}
	}, nil
}

// body 读签发请求体。kind 在 ext-keys 组固定为 ext；group 与 id 不由请求体决定。
func body(r *http.Request, in *input) error {
	var b input
	if err := core.DecodeJSON(r, &b); err != nil {
		return err
	}
	in.Kind, in.Label, in.Executor, in.Scope = b.Kind, b.Label, b.Executor, b.Scope
	if in.Group == "ext-keys" {
		in.Kind = auth.Ext
	}
	return nil
}

func run(x *command.Executor, cmd string, g group, status int, fill func(*http.Request, *input) error) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		in := input{Group: g.path}
		if fill != nil {
			if err := fill(r, &in); err != nil {
				return err
			}
		}
		raw, err := json.Marshal(in)
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
