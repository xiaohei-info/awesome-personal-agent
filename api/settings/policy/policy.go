// Package policy 是策略设置分组的 HTTP 路由（附录 B.2，设计 §14.3、§14.4、§11.2）：
// /settings/policy、/settings/allowlists、/settings/model-endpoints、/settings/access-limits。
//
// 路由里没有业务规则：校验、规整、人格长度、新版本与事件全部由内核 kernel/policy.Update 判定。
// 本层只做两件事：只放行 Web 会话（任何 Bearer 凭据，无论级别，一律 web_only，退出码 20），
// 以及把 HTTP 体按"段"搬进/搬出策略文档。
//
// 每条路由是策略文档的几个顶层段：GET 返回 {version, <段>...}；PUT 只替换请求体里出现的段，
// 其余段不动，成功返回新版本号（与当前相同则不产生版本）。体里出现路由之外的段、或段内未知字段，一律 invalid_request。
package policy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// maskedKey 是读取时代替模型端点密钥的占位；PUT 回传它表示"保持原密钥"。
const maskedKey = "***"

// section 是策略文档的一个顶层段。get 取出用于响应，set 严格解码请求体并替换该段。
type section struct {
	key string
	get func(*kpolicy.Policy) any
	set func(*kpolicy.Policy, json.RawMessage) error
}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "a settings section is malformed or has unknown fields", "send the section in the shape that GET returns")
	}
	return nil
}

func sect[T any](key string, field func(*kpolicy.Policy) *T) section {
	return section{
		key: key,
		get: func(p *kpolicy.Policy) any { return *field(p) },
		set: func(p *kpolicy.Policy, raw json.RawMessage) error {
			var v T
			if err := strictDecode(raw, &v); err != nil {
				return err
			}
			*field(p) = v
			return nil
		},
	}
}

var (
	policySections = []section{
		sect("web_only", func(p *kpolicy.Policy) *kpolicy.WebOnly { return &p.WebOnly }),
		sect("disturb", func(p *kpolicy.Policy) *kpolicy.Disturb { return &p.Disturb }),
		sect("material_write", func(p *kpolicy.Policy) *kpolicy.MaterialWrite { return &p.MaterialWrite }),
		sect("persona", func(p *kpolicy.Policy) *string { return &p.Persona }),
		sect("channels", func(p *kpolicy.Policy) *map[string]kpolicy.Channel { return &p.Channels }),
		sect("default_harness", func(p *kpolicy.Policy) *string { return &p.DefaultHarness }),
	}
	allowlistSections = []section{
		sect("forward_allowlist", func(p *kpolicy.Policy) *map[string][]string { return &p.ForwardAllowlist }),
		sect("channel_answerers", func(p *kpolicy.Policy) *map[string][]string { return &p.ChannelAnswerers }),
	}
	accessLimitSections = []section{
		sect("permission_cap", func(p *kpolicy.Policy) *kpolicy.PermissionCap { return &p.PermissionCap }),
	}
	modelEndpointSections = []section{{
		key: "model_endpoint",
		// 密钥只进账本：读取时只暴露是否已设置。
		get: func(p *kpolicy.Policy) any {
			e := p.ModelEndpoint
			if e.APIKey != "" {
				e.APIKey = maskedKey
			}
			return e
		},
		set: func(p *kpolicy.Policy, raw json.RawMessage) error {
			var e kpolicy.ModelEndpoint
			if err := strictDecode(raw, &e); err != nil {
				return err
			}
			if e.APIKey == maskedKey {
				e.APIKey = p.ModelEndpoint.APIKey
			}
			p.ModelEndpoint = e
			return nil
		},
	}}
)

// Routes 返回策略设置路由分组，交给 core.Deps.Groups。
func Routes(st *store.Store) core.RouteGroup {
	return func(r *core.Router) {
		for path, secs := range map[string][]section{
			"/settings/policy":          policySections,
			"/settings/allowlists":      allowlistSections,
			"/settings/model-endpoints": modelEndpointSections,
			"/settings/access-limits":   accessLimitSections,
		} {
			r.Handle(core.Route{Method: "GET", Path: path, Handler: read(st, secs)})
			r.Handle(core.Route{Method: "PUT", Path: path, Handler: write(st, path, secs)})
		}
	}
}

// webOnly：设置只有 Web 会话能碰（读也算：模型端点、白名单属于账本之外的配置，agent 不需要看）。
func webOnly(p *core.Principal) error {
	if p.Web {
		return nil
	}
	return domain.NewError(domain.CodeWebOnly, "settings can only be used by the owner in the Web UI", "ask the owner to change it in the Web settings")
}

// auditRejected 与命令管线一致地记 command.rejected（§6.7）：授权判定决定是否应记账（delegate 的退出码 20 记，worker、observer 的 4 不记）。
// 记账失败不改变拒绝结果。
func auditRejected(st *store.Store, r *http.Request, p *core.Principal, path string) {
	ar := authz.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, Action: authz.ActionChangeSettings, Object: authz.Object{ID: "settings:" + path}}
	ev, ok := authz.RejectionEvent(ar, authz.Decide(kpolicy.Default(), ar))
	if !ok {
		return
	}
	ev.At = clock.Now()
	_ = st.Update(detached(r), func(tx *store.Tx) error { _, err := eventlog.Append(tx, ev); return err })
}

func render(version int, pol kpolicy.Policy, secs []section) map[string]any {
	out := map[string]any{"version": version}
	for _, s := range secs {
		out[s.key] = s.get(&pol)
	}
	return out
}

func read(st *store.Store, secs []section) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if err := webOnly(p); err != nil {
			return err
		}
		var (
			v   int
			pol kpolicy.Policy
		)
		if err := st.View(r.Context(), func(tx *store.Tx) (err error) { v, pol, err = kpolicy.Current(tx); return }); err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, render(v, pol, secs))
	}
}

func write(st *store.Store, path string, secs []section) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if err := webOnly(p); err != nil {
			auditRejected(st, r, p, path)
			return err
		}
		var body map[string]json.RawMessage
		if err := core.DecodeJSON(r, &body); err != nil {
			return err
		}
		if len(body) == 0 { // 含 JSON null 与空对象
			return domain.NewError(domain.CodeInvalidRequest, "request body must be a non-empty JSON object", "send the sections to replace")
		}
		known := map[string]section{}
		for _, s := range secs {
			known[s.key] = s
		}
		for k := range body {
			if _, ok := known[k]; !ok {
				return domain.NewError(domain.CodeInvalidRequest, "section "+k+" does not belong to this settings route", "see the API list for which route owns it")
			}
		}
		var (
			v   int
			pol kpolicy.Policy
		)
		err := st.Update(detached(r), func(tx *store.Tx) error {
			var err error
			v, err = kpolicy.Update(tx, p.Caller.Actor, "web settings: PUT "+path, func(np *kpolicy.Policy) error {
				for k, raw := range body {
					if err := known[k].set(np, raw); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			_, pol, err = kpolicy.Current(tx)
			return err
		})
		if errors.Is(err, kpolicy.ErrInvalid) {
			return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the setting and send it again; nothing was saved")
		}
		if err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, render(v, pol, secs))
	}
}

// detached：写入一旦开始就不因客户端断开而半途取消。
func detached(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }
