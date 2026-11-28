// Package credentials 是 /settings/tokens 与 /settings/ext-keys 两组路由（设计 §14.2、§14.3，附录 B.2）：
// 签发、列出、吊销长期凭据，明文只在签发响应里出现一次。
//
// 路由不含授权规则：签发、吊销、认领、改档位都按"改设置"登记，由命令管线判定——只有 Web 会话通过，
// delegate 得到 web_only（退出码 20，留痕），其余 token 得到退出码 4，ext/webhook 密钥一律拒绝。
// 命令不接受幂等键，因此明文不会进入幂等结果表；账本里只有哈希。
package credentials

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 命令名。
const (
	CmdIssue  = "credential.issue"
	CmdList   = "credential.list"
	CmdRevoke = "credential.revoke"
	CmdAssign = "credential.assign"
	CmdScope  = "credential.scope.set"
)

// EvAssigned 记 token 被认领到某个 harness（§14.2）。
const EvAssigned = "credential.assigned"

// EvScopeChanged 记钥匙给谁用与逐来源档位被改（§14.2、§14.14），载荷带改前改后。
const EvScopeChanged = "credential.scope_changed"

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
	AgentID  string    `json:"agent_id,omitempty"`
	Group    string    `json:"group"`
	ID       string    `json:"id,omitempty"`
	Kind     auth.Kind `json:"kind,omitempty"`
	Label    string    `json:"label,omitempty"`
	Executor string    `json:"executor,omitempty"`
	Scope    string    `json:"scope,omitempty"`
	Harness  string    `json:"harness,omitempty"`
	// Audience 是 internal 或 external（缺省 external）；SourceScopes 是外部钥匙逐来源的档位，不在表里即不给（§14.14）。
	Audience     string            `json:"audience,omitempty"`
	SourceScopes map[string]string `json:"source_scopes,omitempty"`
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
		{Name: CmdIssue, Action: authz.ActionChangeSettings, Group: authz.GroupOwner, Page: "/settings/agents", Object: noObject,
			Validate: func(tx *store.Tx, _ authz.Object, raw json.RawMessage) error {
				in, g, err := parse(raw)
				if err != nil {
					return err
				}
				if !g.has(in.Kind) {
					return invalid("kind is not valid for /settings/" + g.path)
				}
				return registered(tx, in.SourceScopes)
			},
			Apply: func(c *command.Ctx) (any, error) {
				in, _, _ := parse(c.Input)
				if in.AgentID != "" {
					// 钥匙只能绑到同一程序隔离另写的声明（§14.5）。
					rec, ok, err := executors.Active(c.Tx, in.AgentID)
					if err != nil {
						return nil, err
					}
					if !ok || rec.Declaration.Base != in.Harness {
						return nil, invalid("agent_id must be a separate declaration of this harness")
					}
				}
				cred, secret, err := a.Issue(c.Tx, auth.Spec{Kind: in.Kind, Label: in.Label, Executor: in.Executor, Scope: in.Scope, Harness: in.Harness, AgentID: in.AgentID,
					Audience: in.Audience, SourceScopes: in.SourceScopes})
				if err != nil {
					return nil, err
				}
				return map[string]any{"credential": cred, "secret": secret}, nil
			}},
		{Name: CmdList, Action: authz.ActionWebRead, Group: authz.GroupView, Object: noObject,
			Validate: func(_ *store.Tx, _ authz.Object, raw json.RawMessage) error { _, _, err := parse(raw); return err },
			Apply: func(c *command.Ctx) (any, error) {
				_, g, _ := parse(c.Input)
				all, err := a.List(c.Tx)
				if err != nil {
					return nil, err
				}
				seen, err := eventlog.SeenHarness(c.Tx)
				if err != nil {
					return nil, err
				}
				var last map[string]time.Time
				if g.has(auth.Ext) {
					if last, err = kdoctor.LastSeen(c.Tx); err != nil {
						return nil, err
					}
				}
				out := []view{}
				for _, cr := range all {
					if g.has(cr.Kind) {
						// 事件 actor 的 principal 与 auth.Credential.Actor 同一取法：绑定了执行者时是执行者 id。
						p := cr.Executor
						if p == "" {
							p = cr.ID
						}
						v := view{Credential: cr, SeenHarness: seen[p]}
						if at, ok := last[kdoctor.SubjectExt+cr.Scope]; ok {
							v.LastSeenAt = &at
						}
						out = append(out, v)
					}
				}
				return map[string]any{"credentials": out}, nil
			}},
		{Name: CmdRevoke, Action: authz.ActionChangeSettings, Group: authz.GroupOwner, Page: "/settings/agents", Object: noObject,
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
		{Name: CmdAssign, Action: authz.ActionChangeSettings, Group: authz.GroupOwner, Page: "/settings/agents", Object: noObject,
			Validate: func(_ *store.Tx, _ authz.Object, raw json.RawMessage) error { _, _, err := parse(raw); return err },
			Apply: func(c *command.Ctx) (any, error) {
				in, g, _ := parse(c.Input)
				if cr, err := a.Get(c.Tx, in.ID); err != nil {
					return nil, err
				} else if !g.has(cr.Kind) {
					return nil, domain.NewError(domain.CodeNotFound, "credential not found", "list credentials and retry")
				}
				cr, err := a.Assign(c.Tx, in.ID, in.Harness)
				if err != nil {
					return nil, err
				}
				payload, err := json.Marshal(map[string]string{"harness": cr.Harness})
				if err != nil {
					return nil, err
				}
				if _, err := c.Emit(domain.Event{Type: EvAssigned, Object: cr.ID, Reason: "credential assigned to harness", Payload: payload}); err != nil {
					return nil, err
				}
				return map[string]any{"credential": cr}, nil
			}},
		{Name: CmdScope, Action: authz.ActionChangeSettings, Group: authz.GroupOwner, Page: "/settings/agents", Object: noObject,
			Validate: func(tx *store.Tx, _ authz.Object, raw json.RawMessage) error {
				in, _, err := parse(raw)
				if err != nil {
					return err
				}
				return registered(tx, in.SourceScopes)
			},
			Apply: func(c *command.Ctx) (any, error) {
				in, g, _ := parse(c.Input)
				before, err := a.Get(c.Tx, in.ID)
				if err != nil {
					return nil, err
				}
				if !g.has(before.Kind) {
					return nil, domain.NewError(domain.CodeNotFound, "credential not found", "list credentials and retry")
				}
				cr, err := a.SetScope(c.Tx, in.ID, in.Audience, in.SourceScopes)
				if err != nil {
					return nil, err
				}
				payload, err := json.Marshal(map[string]any{
					"before": map[string]any{"audience": before.Audience, "source_scopes": before.SourceScopes},
					"after":  map[string]any{"audience": cr.Audience, "source_scopes": cr.SourceScopes}})
				if err != nil {
					return nil, err
				}
				if _, err := c.Emit(domain.Event{Type: EvScopeChanged, Object: cr.ID, Reason: "credential scope changed", Payload: payload}); err != nil {
					return nil, err
				}
				return map[string]any{"credential": cr}, nil
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
		for _, g := range groups {
			r.Handle(core.Route{Method: "PUT", Path: "/settings/" + g.path + "/{id}/scope", MaxBody: 64 << 10, Handler: run(x, CmdScope, g, http.StatusOK, func(r *http.Request, in *input) error {
				var b struct {
					Audience     string            `json:"audience"`
					SourceScopes map[string]string `json:"source_scopes"`
				}
				if err := core.DecodeJSON(r, &b); err != nil {
					return err
				}
				in.ID, in.Audience, in.SourceScopes = r.PathValue("id"), b.Audience, b.SourceScopes
				return nil
			})})
		}
		r.Handle(core.Route{Method: "PATCH", Path: "/settings/tokens/{id}", MaxBody: 4 << 10, Handler: run(x, CmdAssign, groups[0], http.StatusOK, func(r *http.Request, in *input) error {
			var b struct {
				Harness string `json:"harness"`
			}
			if err := core.DecodeJSON(r, &b); err != nil {
				return err
			}
			in.ID, in.Harness = r.PathValue("id"), b.Harness
			return nil
		})})
	}, nil
}

// registered：钥匙档位里的来源必须已登记（对话捕获与取值规则由 kernel/auth 校验）。
func registered(tx *store.Tx, scopes map[string]string) error {
	for id := range scopes {
		if _, err := registry.Get(tx, id); err != nil {
			var de *domain.Error
			if errors.As(err, &de) && de.Code == domain.CodeNotFound {
				return invalid("source " + id + " is not registered")
			}
			return err
		}
	}
	return nil
}

// body 读签发请求体。kind 在 ext-keys 组固定为 ext；group 与 id 不由请求体决定。
func body(r *http.Request, in *input) error {
	var b input
	if err := core.DecodeJSON(r, &b); err != nil {
		return err
	}
	in.AgentID = b.AgentID
	in.Kind, in.Label, in.Executor, in.Scope, in.Harness = b.Kind, b.Label, b.Executor, b.Scope, b.Harness
	in.Audience, in.SourceScopes = b.Audience, b.SourceScopes
	if in.Group == "ext-keys" {
		in.Kind = auth.Ext
	}
	return nil
}

// view 是列表里的一条：凭据登记加最近一次调用自报的 harness（只作认领推荐）。
type view struct {
	auth.Credential
	SeenHarness string `json:"seen_harness,omitempty"`
	// LastSeenAt 只对 ext 密钥：同一 scope 的密钥最近一次经认证调用内核的时间（与 apa doctor 同源）。
	LastSeenAt *time.Time `json:"last_seen_at,omitempty"`
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
