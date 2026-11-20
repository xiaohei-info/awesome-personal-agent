// Package policy 是策略设置分组的 HTTP 路由（附录 B.2，设计 §14.3、§14.4、§11.2）：
// /settings/policy、/settings/allowlists、/settings/model-endpoints、/settings/access-limits、/settings/judgment、/settings/note-vaults。
//
// 路由里没有业务规则：校验、规整、人格长度、新版本与事件全部由内核 kernel/policy.Update 判定。
// 本层只做两件事：按段的档位判权（3a 改日常看 Agent 档位，3b 只给刚输过口令的 Web 本人，Authorize），
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
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
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
	key  string
	tier authz.Tier // 3a 改日常或 3b 造新权力（§14.3）；同一次 PUT 取其中最高的一档判
	page string     // 该段所在的 Web 设置页，拒绝时 web_url 落在这里
	get  func(*kpolicy.Policy) any
	set  func(*kpolicy.Policy, json.RawMessage) error
}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "a settings section is malformed or has unknown fields", "send the section in the shape that GET returns")
	}
	return nil
}

func sect[T any](key string, tier authz.Tier, page string, field func(*kpolicy.Policy) *T) section {
	return section{
		key: key, tier: tier, page: page,
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
	key: "disturb", tier: authz.TierRules, page: "/settings/notify",
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

const (
	rules = authz.TierRules
	power = authz.TierPower
)

var (
	policySections = []section{
		disturbSection,
		sect("persona", rules, "/settings/general", func(p *kpolicy.Policy) *string { return &p.Persona }),
		sect("persona_lang", rules, "/settings/general", func(p *kpolicy.Policy) *string { return &p.PersonaLang }),
		sect("channels", rules, "/settings/channels", func(p *kpolicy.Policy) *map[string]kpolicy.Channel { return &p.Channels }),
		sect("default_agent_id", power, "/settings/agents", func(p *kpolicy.Policy) **string { return &p.DefaultAgentID }),
		sect("main_agent_id", power, "/settings/agents", func(p *kpolicy.Policy) **string { return &p.MainAgentID }),
		sect("rule_agent", power, "/settings/agents", func(p *kpolicy.Policy) *map[string]string { return &p.RuleAgent }),
		sect("default_harness", power, "/settings/agents", func(p *kpolicy.Policy) *string { return &p.DefaultHarness }),
		sect("main_session_launch", power, "/settings/agents", func(p *kpolicy.Policy) *kpolicy.MainSessionLaunch { return &p.MainSessionLaunch }),
	}
	allowlistSections = []section{
		sect("forward_allowlist", rules, "/sources", func(p *kpolicy.Policy) *map[string][]string { return &p.ForwardAllowlist }),
		sect("channel_answerers", power, "/settings/channels", func(p *kpolicy.Policy) *map[string][]string { return &p.ChannelAnswerers }),
	}
	accessLimitSections = []section{
		sect("permission_cap", power, "/settings/agents", func(p *kpolicy.Policy) *kpolicy.PermissionCap { return &p.PermissionCap }),
	}
	// 笔记库登记（§12.8）：整体替换；名字重复、模板不以协议开头或用了被拒绝的协议由内核判为 400。
	noteVaultSections = []section{
		sect("vaults", power, "/settings/note-vaults", func(p *kpolicy.Policy) *[]kpolicy.NoteVault { return &p.NoteVaults }),
	}
	judgmentSections = []section{
		sect("bands", rules, "/judgment", func(p *kpolicy.Policy) *[]kpolicy.JudgmentBand { return &p.Judgment.Bands }),
		sect("shadow", rules, "/judgment", func(p *kpolicy.Policy) *[]string { return &p.Judgment.Shadow }),
		sect("spot_check", rules, "/judgment", func(p *kpolicy.Policy) *kpolicy.SpotCheck { return &p.Judgment.SpotCheck }),
	}
	modelEndpointSections = []section{
		{key: "judge_endpoint", tier: power, page: "/settings/models", get: getJudge, set: setJudge},
		{key: "generate_endpoint", tier: power, page: "/settings/models", get: getGenerate, set: setGenerate},
		// 暂停模型：两个端点原样保留，内核按未配置运行（§8.7）；没有端点时内核规整为 false。
		sect("models_paused", rules, "/settings/models", func(p *kpolicy.Policy) *bool { return &p.ModelsPaused }),
		// v0.1 的请求形状：只在 PUT 里被接受（等同只设生成端点，位置 external），响应一律是新形状（附录 B.2）。
		{key: legacyKey, tier: power, page: "/settings/models", set: setLegacy},
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
	Protocol   string  `json:"protocol"`
	Model      string  `json:"model"`
	Locality   string  `json:"locality"`
	// 资料预算只有 Web 能改（策略写入口本身要求 Web 登录）；省略时保持已存的值，不让旧形状的请求悄悄重置它。
	MaxMaterialChars int `json:"max_material_chars"`
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

// keepBudget：请求没带预算（0）时沿用已存的值；负数交策略校验拒绝。
func keepBudget(in, old int) int {
	if in == 0 {
		return old
	}
	return in
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
	return map[string]any{"url": g.URL, "api_key_set": g.APIKey != "", "api_key_hint": keyHint(g.APIKey), "protocol": kpolicy.GenerateProtocol(g.Protocol), "model": g.Model, "locality": g.Locality,
		"max_material_chars": g.MaterialBudget()}
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
	p.GenerateEndpoint = kpolicy.GenerateEndpoint{URL: r.URL, APIKey: keepKey(r.APIKey, p.GenerateEndpoint.APIKey), Model: r.Model, Locality: r.Locality, Protocol: r.Protocol,
		MaxMaterialChars: keepBudget(r.MaxMaterialChars, p.GenerateEndpoint.MaxMaterialChars)}
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
	p.GenerateEndpoint = kpolicy.GenerateEndpoint{URL: r.URL, APIKey: keepKey(r.APIKey, p.GenerateEndpoint.APIKey), Model: r.Model, Locality: kpolicy.LocalityExternal, Protocol: r.Protocol,
		MaxMaterialChars: p.GenerateEndpoint.MaxMaterialChars}
	return nil // screening_timeout_ms 随初筛取消，旧形状里带了也忽略
}

// Routes 返回策略设置路由分组，交给 core.Deps.Groups。a 用于删除自写程序渠道时吊销它的密钥。
func Routes(st *store.Store, a *auth.Auth) core.RouteGroup {
	return func(r *core.Router) {
		for path, secs := range map[string][]section{
			"/settings/policy":          policySections,
			"/settings/allowlists":      allowlistSections,
			"/settings/model-endpoints": modelEndpointSections,
			"/settings/access-limits":   accessLimitSections,
			"/settings/judgment":        judgmentSections,
			"/settings/note-vaults":     noteVaultSections,
		} {
			r.Handle(core.Route{Method: "GET", Path: path, Handler: read(st, path, secs)})
			r.Handle(core.Route{Method: "PUT", Path: path, Handler: write(st, path, secs)})
		}
		r.Handle(core.Route{Method: "PUT", Path: "/settings/channels/{name}/answerers", Handler: answerers(st)})
		r.Handle(core.Route{Method: "PUT", Path: "/settings/agents/{agent}/tier", Handler: agentTier(st)})
		r.Handle(core.Route{Method: "DELETE", Path: "/settings/channels/{name}", MaxBody: 1, Handler: removeChannel(st, a)})
	}
}

// channelsRoutePrefix 是渠道 ext 名字不能占用的前缀：channel.ch-… 是内置渠道连接的路由（kernel/connections.IDPrefix）。
const channelsRoutePrefix = "ch-"

// removeChannel 删除一个自写程序渠道（§4.1）：在同一个事务里吊销该渠道所有未吊销的渠道密钥，
// 再从策略的 channels 段与 channel_answerers 里删掉这个名字。要么三件都成，要么都不动。
// 名字本来就不在时不报错（幂等），返回当前策略版本号与本次吊销的钥匙数。
// 指向它的提醒路由不动：路由按「提醒与打扰」的现有规则列出并标明（§7.4）。
func removeChannel(st *store.Store, a *auth.Auth) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		basis, err := Authorize(st, r, p, authz.TierPower, "/settings/channels", "settings:/settings/channels/{name}")
		if err != nil {
			return err
		}
		name := r.PathValue("name")
		if strings.HasPrefix(name, channelsRoutePrefix) {
			return domain.NewError(domain.CodeInvalidRequest, "channel names starting with "+channelsRoutePrefix+" are built-in connections", "delete a built-in channel from its own card")
		}
		var (
			v       int
			revoked int
		)
		err = st.Update(detached(r), func(tx *store.Tx) error {
			creds, err := a.List(tx)
			if err != nil {
				return err
			}
			for _, cr := range creds {
				if cr.Kind != auth.Ext || cr.Scope != "channel."+name || !cr.RevokedAt.IsZero() {
					continue
				}
				if err := a.Revoke(tx, cr.ID); err != nil {
					return err
				}
				revoked++
			}
			v, err = kpolicy.UpdateWith(tx, p.Caller.Actor, basis, "settings: delete channel "+name, func(np *kpolicy.Policy) error {
				delete(np.Channels, name)
				delete(np.ChannelAnswerers, name)
				return nil
			})
			return err
		})
		if errors.Is(err, kpolicy.ErrInvalid) {
			return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the setting and send it again; nothing was saved")
		}
		if err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, map[string]any{"version": v, "revoked": revoked})
	}
}

// answerers 只替换一个渠道的回答人白名单（§14.6），其余渠道不动；空名单即清空。
func answerers(st *store.Store) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		basis, err := Authorize(st, r, p, authz.TierPower, "/settings/channels", "settings:/settings/channels/{name}/answerers")
		if err != nil {
			return err
		}
		var in struct {
			Answerers []string `json:"answerers"`
		}
		raw, err := readBody(r)
		if err != nil {
			return err
		}
		if err := strictDecode(raw, &in); err != nil {
			return err
		}
		name := r.PathValue("name")
		v, pol, err := UpdateKey(st, r, p, basis, "settings: answerers of channel "+name, func(np *kpolicy.Policy) {
			if np.ChannelAnswerers == nil {
				np.ChannelAnswerers = map[string][]string{}
			}
			np.ChannelAnswerers[name] = in.Answerers
		})
		if err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, map[string]any{"version": v, "answerers": nonNil(pol.ChannelAnswerers[name])})
	}
}

// agentTier 设一个 Agent 的档位（§14.3）：只整理 / 也替我拍板（可限项目）/ 连日常规矩也替我改。设档位本身是 3b。
// 可带 version 做乐观并发。Agent 是否存在不在这里校验：没有卡片的 id 不会被任何调用者匹配到。
func agentTier(st *store.Store) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		basis, err := Authorize(st, r, p, authz.TierPower, "/settings/agents", "settings:/settings/agents/{agent}/tier")
		if err != nil {
			return err
		}
		var in struct {
			Tier     string   `json:"tier"`
			Projects []string `json:"projects"`
			Version  *int     `json:"version"`
		}
		raw, err := readBody(r)
		if err != nil {
			return err
		}
		if err := strictDecode(raw, &in); err != nil {
			return err
		}
		agent := r.PathValue("agent")
		var (
			v   int
			pol kpolicy.Policy
		)
		err = st.Update(detached(r), func(tx *store.Tx) error {
			cur, _, err := kpolicy.Stored(tx)
			if err != nil {
				return err
			}
			if in.Version != nil && cur != *in.Version {
				return domain.NewError(domain.CodeConflict, "settings changed", "reload Agent settings")
			}
			if v, err = kpolicy.UpdateWith(tx, p.Caller.Actor, basis, "settings: tier of agent "+agent, func(np *kpolicy.Policy) error {
				if np.AgentTier.ByAgent == nil {
					np.AgentTier.ByAgent = map[string]kpolicy.AgentAllowance{}
				}
				np.AgentTier.ByAgent[agent] = kpolicy.AgentAllowance{Tier: in.Tier, Projects: in.Projects}
				return nil
			}); err != nil {
				return err
			}
			_, pol, err = kpolicy.Stored(tx)
			return err
		})
		if errors.Is(err, kpolicy.ErrInvalid) {
			return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the setting and send it again; nothing was saved")
		}
		if err != nil {
			return err
		}
		al := pol.Allowance(agent)
		return core.WriteJSON(w, http.StatusOK, map[string]any{"version": v, "tier": al.Tier, "projects": nonNil(al.Projects)})
	}
}

func readBody(r *http.Request) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := core.DecodeJSON(r, &raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func nonNil(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

// UpdateKey 经 kernel/policy.Update 改策略里的一处（如白名单的一个键），返回新版本号与存储的策略。
// 调用方须先经 Authorize 判权。
func UpdateKey(st *store.Store, r *http.Request, p *core.Principal, basis *domain.Basis, reason string, mutate func(*kpolicy.Policy)) (int, kpolicy.Policy, error) {
	var (
		v   int
		pol kpolicy.Policy
	)
	err := st.Update(detached(r), func(tx *store.Tx) error {
		var err error
		if v, err = kpolicy.UpdateWith(tx, p.Caller.Actor, basis, reason, func(np *kpolicy.Policy) error { mutate(np); return nil }); err != nil {
			return err
		}
		_, pol, err = kpolicy.Stored(tx)
		return err
	})
	if errors.Is(err, kpolicy.ErrInvalid) {
		return 0, pol, domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the setting and send it again; nothing was saved")
	}
	return v, pol, err
}

// webOnly：只读的探测类路由（模型端点测试）只给 Web 本人。
func webOnly(p *core.Principal) error {
	if p.Web {
		return nil
	}
	return domain.NewError(domain.CodeWebOnly, "settings can only be used by the owner in the Web UI", "ask the owner to change it in the Web settings")
}

// Authorize 判一次不走命令管线的设置写入（§14.3）：与命令同一个 authz.Decide——3b 硬线（Web 本人且刚输过口令），
// 3a 看调用者所属 Agent 的档位。被拒的按同一条留痕路径记 command.rejected（§6.7）；记账失败不改变拒绝结果。
// object 是留痕用的对象名（如 settings:/settings/policy）。
func Authorize(st *store.Store, r *http.Request, p *core.Principal, tier authz.Tier, page, object string) (*domain.Basis, error) {
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return nil, domain.NewError(domain.CodePermissionDenied, "credential kind cannot change settings", "use an owner or agent credential")
	}
	var (
		pv  int
		pol = kpolicy.Default()
	)
	if !p.Web { // Agent 的档位在策略里；Web 本人不看档位，不必先读（读不出的策略交给写入报 400）
		if err := st.View(r.Context(), func(tx *store.Tx) (err error) { pv, pol, err = kpolicy.Current(tx); return }); err != nil {
			return nil, err
		}
	}
	agent := p.Caller.Agent()
	ar := authz.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, Action: authz.ActionChangeSettings, Tier: tier, Fresh: p.Caller.Fresh,
		Agent: agent, Allowance: pol.Allowance(agent), Page: page, Object: authz.Object{ID: object}}
	d := authz.Decide(pol, ar)
	if d.Err == nil {
		via := domain.BasisAllowance
		if p.Web {
			via = domain.BasisOwnerWeb
		}
		return &domain.Basis{PolicyVersion: pv, Via: via, Tier: tier.String(), Agent: agent}, nil
	}
	if ev, ok := authz.RejectionEvent(ar, d); ok {
		ev.At = clock.Now()
		_ = st.Update(detached(r), func(tx *store.Tx) error { _, err := eventlog.Append(tx, ev); return err })
	}
	return nil, d.Err
}

// top 是一组段里最高的档与它所在的页（同档取第一个）。
func top(secs []section) (authz.Tier, string) {
	t, page := authz.TierRules, ""
	for _, s := range secs {
		if page == "" || s.tier > t {
			t, page = s.tier, s.page
		}
	}
	return t, page
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

func read(st *store.Store, path string, secs []section) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		// 读：Web 本人看全部段；档位放开到 3a 的 Agent 只看 3a 段（改之前要知道现值），3b 段不给。
		// 只有 3b 段的路由对 Agent 是硬线；拒绝与写入同一条留痕路径。
		shown := secs
		if !p.Web {
			shown = nil
			for _, s := range secs {
				if s.tier == authz.TierRules {
					shown = append(shown, s)
				}
			}
			tier, page := authz.TierPower, secs[0].page
			if len(shown) > 0 {
				tier, page = authz.TierRules, shown[0].page
			}
			if _, err := Authorize(st, r, p, tier, page, "settings:"+path); err != nil {
				return err
			}
		}
		var (
			v   int
			pol kpolicy.Policy
		)
		if err := st.View(r.Context(), func(tx *store.Tx) (err error) { v, pol, err = kpolicy.Stored(tx); return }); err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, render(v, pol, shown))
	}
}

func write(st *store.Store, path string, secs []section) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		// 先按这条路由最低的一档判：连最低一档都做不了的调用者，不论请求体是什么都先拒绝（不给它探请求形状）。
		low, lowPage := authz.TierPower, ""
		for _, s := range secs {
			if s.tier <= low {
				low, lowPage = s.tier, s.page
			}
		}
		if _, err := Authorize(st, r, p, low, lowPage, "settings:"+path); err != nil {
			return err
		}
		var body map[string]json.RawMessage
		if err := core.DecodeJSON(r, &body); err != nil {
			return err
		}
		if len(body) == 0 { // 含 JSON null 与空对象
			return domain.NewError(domain.CodeInvalidRequest, "request body must be a non-empty JSON object", "send the sections to replace")
		}
		version := body["version"]
		delete(body, "version")
		roles := body["default_agent_id"] != nil || body["main_agent_id"] != nil
		if roles && (version == nil || body["default_agent_id"] == nil || body["main_agent_id"] == nil || string(body["default_agent_id"]) == "null" || string(body["main_agent_id"]) == "null" || body["default_harness"] != nil) {
			return domain.NewError(domain.CodeInvalidRequest, "请同时保存两种角色及策略版本", "重新载入设置")
		}
		known := map[string]section{}
		for _, s := range secs {
			known[s.key] = s
		}
		if _, old := body[legacyKey]; old && len(body) > 1 && known[legacyKey].set != nil {
			return domain.NewError(domain.CodeInvalidRequest, "model_endpoint (v0.1 shape) cannot be combined with the new sections", "send either the old shape or the new one")
		}
		var touched []section
		for k := range body {
			s, ok := known[k]
			if !ok {
				return domain.NewError(domain.CodeInvalidRequest, "section "+k+" does not belong to this settings route", "see the API list for which route owns it")
			}
			touched = append(touched, s)
		}
		tier, page := top(touched)
		basis, err := Authorize(st, r, p, tier, page, "settings:"+path)
		if err != nil {
			return err
		}
		var (
			v   int
			pol kpolicy.Policy
		)
		err = st.Update(detached(r), func(tx *store.Tx) error {
			var err error
			if version != nil {
				var expected int
				if err := json.Unmarshal(version, &expected); err != nil {
					return domain.NewError(domain.CodeInvalidRequest, "版本必须是整数", "重新载入设置")
				}
				current, _, err := kpolicy.Stored(tx)
				if err != nil {
					return err
				}
				if current != expected {
					return domain.NewError(domain.CodeConflict, "设置已变更", "重新载入设置")
				}
			}
			v, err = kpolicy.UpdateWith(tx, p.Caller.Actor, basis, "settings: PUT "+path, func(np *kpolicy.Policy) error {
				if np.DefaultAgentID != nil && body["default_harness"] != nil {
					return domain.NewError(domain.CodeConflict, "已启用 Agent 角色设置", "请用新的角色入口")
				}
				for k, raw := range body {
					if k == "permission_cap" && len(np.PermissionCap.ByAgent) > 0 || k == "main_session_launch" && len(np.MainSessionLaunch.ByAgent) > 0 {
						var fields map[string]json.RawMessage
						if json.Unmarshal(raw, &fields) == nil && fields["by_agent"] == nil {
							return domain.NewError(domain.CodeConflict, "请重新载入 Agent 设置以保留实例策略", "reload Agent settings")
						}
					}
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
