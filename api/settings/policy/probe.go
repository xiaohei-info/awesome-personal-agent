package policy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ProbeRoutes 返回"设置 › 模型"的两条探测路由（附录 B.2；设计 §4.1、§15.5）：
// POST /settings/model-endpoints/models 取模型列表，POST /settings/model-endpoints/test 测试连接。
//
// 只放行 Web 会话；只读账本，不写 Event，不改策略（保存仍走 PUT）。探测只经外呼调用器发出，
// 测试用 doctor 的内置示例题，不带任何账本内容，所以不受来源外发范围约束（§14.14）。
// 同一类端点同一时刻只跑一个探测，其余返回 409。
func ProbeRoutes(st *store.Store, env kdoctor.Env) core.RouteGroup {
	if env.Model == nil {
		env.Model = &model.Client{}
	}
	pr := &prober{st: st, env: env, busy: map[string]bool{}}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "POST", Path: "/settings/model-endpoints/models", MaxBody: 64 << 10, Handler: pr.models})
		r.Handle(core.Route{Method: "POST", Path: "/settings/model-endpoints/test", MaxBody: 64 << 10, Handler: pr.test})
	}
}

const (
	kindJudge    = "judge"
	kindGenerate = "generate"
)

type prober struct {
	st  *store.Store
	env kdoctor.Env
	mu  sync.Mutex
	// shortcut: 进程内互斥只防同一类端点的连点与并发；需要按会话限速时再接限速器。
	busy map[string]bool
}

func (pr *prober) acquire(kind string) (func(), error) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	if pr.busy[kind] {
		return nil, domain.NewError(domain.CodeConflict, "a probe of this endpoint is already running", "wait for it to finish, then try again")
	}
	pr.busy[kind] = true
	return func() { pr.mu.Lock(); delete(pr.busy, kind); pr.mu.Unlock() }, nil
}

func badKind() error {
	return domain.NewError(domain.CodeInvalidRequest, "endpoint must be judge or generate", "send endpoint: judge or generate")
}

// apiKey 决定探测用的密钥：显式给出的照用（"" 表示不带）；省略或为占位 *** 时沿用该端点已存的密钥，
// 但只在请求的 URL 与已存 URL 同源时沿用，免得把已存密钥发到另一台主机。
func (pr *prober) apiKey(ctx context.Context, kind, target string, in *string) (string, error) {
	if in != nil && *in != maskedKey {
		return *in, nil
	}
	var pol kpolicy.Policy
	if err := pr.st.View(ctx, func(tx *store.Tx) (err error) { _, pol, err = kpolicy.Stored(tx); return }); err != nil {
		return "", err
	}
	saved, key := pol.GenerateEndpoint.URL, pol.GenerateEndpoint.APIKey
	if kind == kindJudge {
		saved, key = "", ""
		if j := pol.JudgeEndpoint; j != nil {
			saved, key = j.URL, j.APIKey
		}
	}
	if key == "" {
		return "", nil
	}
	if !sameOrigin(target, saved) {
		return "", domain.NewError(domain.CodeInvalidRequest, "the stored API key is only reused for the saved endpoint's origin", "enter the API key again for the new URL")
	}
	return key, nil
}

func sameOrigin(a, b string) bool {
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	return ea == nil && eb == nil && ua.Host != "" && strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func invalidDraft(err error) error {
	if errors.Is(err, kpolicy.ErrInvalid) {
		return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the endpoint settings and try again")
	}
	return err
}

// models：取对话补全兼容端点或 system-one 判断接口的模型列表。探测本身的成败是数据（200 + ok:false + reason），不是 HTTP 错误。
func (pr *prober) models(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := webOnly(p); err != nil {
		return err
	}
	var in struct {
		Endpoint string  `json:"endpoint"`
		URL      string  `json:"url"`
		APIKey   *string `json:"api_key"`
		Protocol string  `json:"protocol"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	listURL := in.URL
	switch in.Endpoint {
	case kindGenerate:
	case kindJudge:
		switch in.Protocol {
		case kpolicy.ProtocolChatLogprobs:
		case kpolicy.ProtocolSystemOne:
			listURL = model.SystemOneBase(in.URL)
		default:
			return domain.NewError(domain.CodeInvalidRequest, "protocol "+kpolicy.ProtocolProbability+" has no model list",
				"type the model name, or leave it empty for the endpoint to report")
		}
	default:
		return badKind()
	}
	draft := kpolicy.Policy{GenerateEndpoint: kpolicy.GenerateEndpoint{URL: in.URL}}
	if in.URL == "" {
		return domain.NewError(domain.CodeInvalidRequest, "url is required", "enter the endpoint URL")
	}
	if err := kpolicy.NormalizeEndpoints(&draft); err != nil {
		return invalidDraft(err)
	}
	key, err := pr.apiKey(r.Context(), in.Endpoint, in.URL, in.APIKey)
	if err != nil {
		return err
	}
	release, err := pr.acquire(in.Endpoint)
	if err != nil {
		return err
	}
	defer release()
	start := clock.Now()
	ids, truncated, err := pr.env.Model.ListModels(r.Context(), listURL, key)
	out := map[string]any{"ok": err == nil, "elapsed_ms": clock.Now().Sub(start).Milliseconds()}
	if err == nil {
		out["models"], out["truncated"] = ids, truncated
		return core.WriteJSON(w, http.StatusOK, out)
	}
	if r.Context().Err() != nil { // 本人取消了：不再回应
		return r.Context().Err()
	}
	var se *model.StatusError
	switch {
	case errors.As(err, &se):
		out["reason"], out["status"] = "http_status", se.Code
		switch se.Code {
		case http.StatusUnauthorized, http.StatusForbidden:
			out["reason"] = "unauthorized"
		case http.StatusNotFound:
			out["reason"] = "not_found"
		}
		if se.Region {
			out["reason"] = "region_unavailable"
		}
	case errors.Is(err, model.ErrTimeout):
		out["reason"] = "timeout"
	case errors.Is(err, model.ErrBadResponse):
		out["reason"] = "bad_response"
	default:
		out["reason"] = "unreachable"
	}
	return core.WriteJSON(w, http.StatusOK, out)
}

// test：用 doctor 的内置示例题探测草稿端点（与 `apa doctor` 同一份代码），返回同形的明细。
func (pr *prober) test(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := webOnly(p); err != nil {
		return err
	}
	var in struct {
		Endpoint string          `json:"endpoint"`
		Config   json.RawMessage `json:"config"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	var draft kpolicy.Policy
	var target string
	var key *string
	switch in.Endpoint {
	case kindJudge:
		var c endpointReq
		if err := strictDecode(in.Config, &c); err != nil {
			return err
		}
		draft.JudgeEndpoint = &kpolicy.JudgeEndpoint{URL: c.URL, Protocol: c.Protocol, Model: c.Model, Locality: c.Locality,
			TimeoutMs: c.TimeoutMs, MaxBatch: c.MaxBatch, MaxMaterialChars: c.MaxMaterialChars}
		target, key = c.URL, c.APIKey
	case kindGenerate:
		var c generateReq
		if err := strictDecode(in.Config, &c); err != nil {
			return err
		}
		if c.URL == "" || c.Model == "" {
			return domain.NewError(domain.CodeInvalidRequest, "generate_endpoint url and model are required for a test", "enter the URL and choose a model")
		}
		draft.GenerateEndpoint = kpolicy.GenerateEndpoint{URL: c.URL, Model: c.Model, Locality: c.Locality}
		target, key = c.URL, c.APIKey
	default:
		return badKind()
	}
	if err := kpolicy.NormalizeEndpoints(&draft); err != nil {
		return invalidDraft(err)
	}
	k, err := pr.apiKey(r.Context(), in.Endpoint, target, key)
	if err != nil {
		return err
	}
	release, err := pr.acquire(in.Endpoint)
	if err != nil {
		return err
	}
	defer release()
	var d kdoctor.Detail
	if in.Endpoint == kindJudge {
		draft.JudgeEndpoint.APIKey = k
		d = kdoctor.ProbeJudge(r.Context(), draft.JudgeEndpoint, pr.env)
	} else {
		draft.GenerateEndpoint.APIKey = k
		d = kdoctor.ProbeGenerate(r.Context(), draft.GenerateEndpoint, pr.env)
	}
	if r.Context().Err() != nil {
		return r.Context().Err()
	}
	return core.WriteJSON(w, http.StatusOK, d)
}
