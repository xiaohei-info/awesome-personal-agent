// Package policy 是策略设置分组的 HTTP 路由（附录 B.2，设计 §14.3、§14.4、§11.2）：
// /settings/policy、/settings/allowlists、/settings/model-endpoints、/settings/access-limits、/settings/judgment。
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

// disturbSection 与 sect 相同，但请求体缺 daily_cap 时取缺省 20，而不是被当成"不限"。
var disturbSection = section{
	key: "disturb",
	get: func(p *kpolicy.Policy) any { return p.Disturb },
	set: func(p *kpolicy.Policy, raw json.RawMessage) error {
		v := kpolicy.Disturb{DailyCap: kpolicy.DefaultDailyCap}
		if err := strictDecode(raw, &v); err != nil {
			return err
		}
		p.Disturb = v
		return nil
	},
}

var (
	policySections = []section{
		sect("web_only", func(p *kpolicy.Policy) *kpolicy.WebOnly { return &p.WebOnly }),
		disturbSection,
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
	judgmentSections = []section{
		sect("bands", func(p *kpolicy.Policy) *[]kpolicy.JudgmentBand { return &p.Judgment.Bands }),
		sect("shadow", func(p *kpolicy.Policy) *[]string { return &p.Judgment.Shadow }),
		sect("spot_check", func(p *kpolicy.Policy) *kpolicy.SpotCheck { return &p.Judgment.SpotCheck }),
	}
	modelEndpointSections = []section{
		{key: "judge_endpoint", get: getJudge, set: setJudge},
		{key: "generate_endpoint", get: getGenerate, set: setGenerate},
		sect("screening_timeout_ms", func(p *kpolicy.Policy) *int { return &p.ScreeningTimeoutMs }),
		// 暂停模型：两个端点原样保留，内核按未配置运行（§8.7）；没有端点时内核规整为 false。
		sect("models_paused", func(p *kpolicy.Policy) *bool { return &p.ModelsPaused }),
		// v0.1 的请求形状：只在 PUT 里被接受（等同只设生成端点，位置 external），响应一律是新形状（附录 B.2）。
		{key: legacyKey, set: setLegacy},
	}
)

// legacyKey 是 v0.1 的 PUT 体里的段名；它没有 get，不会出现在响应里。
const legacyKey = "model_endpoint"

// 端点请求体里 api_key 为 nil 或占位 "***" 表示保持原密钥，显式 "" 表示清除；读取时只给 api_key_set 与 api_key_hint。
// api_key_hint 是只读字段：照 GET 的形状回传时出现在请求体里，忽略它，免得严格解码拒绝。
type endpointReq struct {
	URL              string  `json:"url"`
	APIKey           *string `json:"api_key"`
	APIKeyHint       *string `json:"api_key_hint"`
	Protocol         string  `json:"protocol"`
	Model            string  `json:"model"`
	Locality         string  `json:"locality"`
	TimeoutMs        int     `json:"timeout_ms"`
	MaxBatch         int     `json:"max_batch"`
	MaxMaterialChars int     `json:"max_material_chars"`
}

type generateReq struct {
	URL        string  `json:"url"`
	APIKey     *string `json:"api_key"`
	APIKeyHint *string `json:"api_key_hint"`
	Model      string  `json:"model"`
	Locality   string  `json:"locality"`
}

// keyHint 是已存密钥的末 4 个字符，供 Web 显示"已设置 ····末 4 位"；短于 12 个字符的密钥不给，免得暴露过大比例。
func keyHint(k string) string {
	r := []rune(k)
	if len(r) < 12 {
		return ""
	}
	return string(r[len(r)-4:])
}

func keepKey(in *string, old string) string {
	if in == nil || *in == maskedKey {
		return old
	}
	return *in
}

func getJudge(p *kpolicy.Policy) any {
	j := p.JudgeEndpoint
	if j == nil {
		return nil
	}
	return map[string]any{"url": j.URL, "api_key_set": j.APIKey != "", "api_key_hint": keyHint(j.APIKey), "protocol": j.Protocol, "model": j.Model, "locality": j.Locality,
		"timeout_ms": j.TimeoutMs, "max_batch": j.MaxBatch, "max_material_chars": j.MaxMaterialChars}
}

func setJudge(p *kpolicy.Policy, raw json.RawMessage) error {
	var r *endpointReq
	if err := strictDecode(raw, &r); err != nil {
		return err
	}
	if r == nil {
		p.JudgeEndpoint = nil
		return nil
	}
	old := ""
	if p.JudgeEndpoint != nil {
		old = p.JudgeEndpoint.APIKey
	}
	p.JudgeEndpoint = &kpolicy.JudgeEndpoint{URL: r.URL, APIKey: keepKey(r.APIKey, old), Protocol: r.Protocol, Model: r.Model,
		Locality: r.Locality, TimeoutMs: r.TimeoutMs, MaxBatch: r.MaxBatch, MaxMaterialChars: r.MaxMaterialChars}
	return nil
}

func getGenerate(p *kpolicy.Policy) any {
	g := p.GenerateEndpoint
	if g.URL == "" {
		return nil
	}
	return map[string]any{"url": g.URL, "api_key_set": g.APIKey != "", "api_key_hint": keyHint(g.APIKey), "model": g.Model, "locality": g.Locality}
}

func setGenerate(p *kpolicy.Policy, raw json.RawMessage) error {
	var r *generateReq
	if err := strictDecode(raw, &r); err != nil {
		return err
	}
	if r == nil {
		p.GenerateEndpoint = kpolicy.GenerateEndpoint{}
		return nil
	}
	p.GenerateEndpoint = kpolicy.GenerateEndpoint{URL: r.URL, APIKey: keepKey(r.APIKey, p.GenerateEndpoint.APIKey), Model: r.Model, Locality: r.Locality}
	return nil
}

func setLegacy(p *kpolicy.Policy, raw json.RawMessage) error {
	var r struct {
		generateReq
		ScreeningTimeoutMs int `json:"screening_timeout_ms"`
	}
	if err := strictDecode(raw, &r); err != nil {
		return err
	}
	p.GenerateEndpoint = kpolicy.GenerateEndpoint{URL: r.URL, APIKey: keepKey(r.APIKey, p.GenerateEndpoint.APIKey), Model: r.Model, Locality: kpolicy.LocalityExternal}
	p.ScreeningTimeoutMs = r.ScreeningTimeoutMs
	return nil
}

// Routes 返回策略设置路由分组，交给 core.Deps.Groups。
func Routes(st *store.Store) core.RouteGroup {
	return func(r *core.Router) {
		for path, secs := range map[string][]section{
			"/settings/policy":          policySections,
			"/settings/allowlists":      allowlistSections,
			"/settings/model-endpoints": modelEndpointSections,
			"/settings/access-limits":   accessLimitSections,
			"/settings/judgment":        judgmentSections,
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
		if s.get != nil {
			out[s.key] = s.get(&pol)
		}
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
		if err := st.View(r.Context(), func(tx *store.Tx) (err error) { v, pol, err = kpolicy.Stored(tx); return }); err != nil {
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
		if _, old := body[legacyKey]; old && len(body) > 1 && known[legacyKey].set != nil {
			return domain.NewError(domain.CodeInvalidRequest, "model_endpoint (v0.1 shape) cannot be combined with the new sections", "send either the old shape or the new one")
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
			_, pol, err = kpolicy.Stored(tx) // 设置页显示存储的原文：暂停时端点照常在内
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
