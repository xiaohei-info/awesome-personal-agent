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
	"reflect"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// maskedKey 是读取时代替模型端点密钥的占位；PUT 回传它表示"保持原密钥"。
const maskedKey = "***"

// section 是策略文档的一个顶层段。get 取出用于响应，set 严格解码请求体并替换该段。
type section struct {
	key   string
	group authz.Group // 该段属于哪一组（§14.3）；同一次 PUT 改到的每一组都要开放
	page  string      // 该段所在的 Web 设置页，拒绝时 web_url 落在这里
	get   func(*kpolicy.Policy) any
	// view 非空时代替 get：响应要读账本里别的数据（在读策略的同一事务里）。
	view func(*store.Tx, *kpolicy.Policy) (any, error)
	set  func(*kpolicy.Policy, json.RawMessage) error
	// setTx 非空时代替 set：写入要读账本里别的数据（在写策略的同一事务里）。
	setTx func(*store.Tx, *kpolicy.Policy, json.RawMessage) error
}

func strictDecode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "a settings section is malformed or has unknown fields", "send the section in the shape that GET returns")
	}
	return nil
}

func sect[T any](key string, group authz.Group, page string, field func(*kpolicy.Policy) *T) section {
	return section{
		key: key, group: group, page: page,
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

// disturbSection 是 disturb 段：请求体缺 daily_cap 时取缺省 20，而不是被当成"不限"。
// 响应里的 delivery 总是生效的投递矩阵：没保存过时是按旧键换算的结果，delivery_saved 为假（只读字段，PUT 回传时忽略）。
// 还没保存过矩阵时，PUT 回传的 delivery 与换算值一字不差就当作没改矩阵（照 GET 的形状改旧键的客户端照旧可用，不写入、不冻结）；
// 矩阵一经写入，旧键 routes、push.*.route、proactive.task_push 与 instance_push 冻结：PUT 改了它们为 400；
// PUT 没带 delivery 时沿用已存的矩阵（不会因此退回旧键）。
var disturbSection = section{
	key: "disturb", group: authz.GroupRules, page: "/settings/notify",
	get: func(p *kpolicy.Policy) any { return p.Disturb },
	view: func(tx *store.Tx, p *kpolicy.Policy) (any, error) {
		d := p.Disturb
		saved := d.Delivery != nil
		if !saved {
			packs, err := instancePacks(tx)
			if err != nil {
				return nil, err
			}
			m := d.Matrix(packs)
			d.Delivery = &m
		}
		return disturbBody{Disturb: d, DeliverySaved: &saved}, nil
	},
	setTx: func(tx *store.Tx, p *kpolicy.Policy, raw json.RawMessage) error {
		v := disturbBody{Disturb: kpolicy.Disturb{DailyCap: kpolicy.DefaultDailyCap}}
		if err := strictDecode(raw, &v); err != nil {
			return err
		}
		if v.Delivery != nil && p.Disturb.Delivery == nil {
			packs, err := instancePacks(tx)
			if err != nil {
				return err
			}
			if reflect.DeepEqual(*v.Delivery, p.Disturb.Matrix(packs)) {
				v.Delivery = nil
			}
		}
		if v.Delivery == nil {
			v.Delivery = p.Disturb.Delivery
		}
		if v.Delivery != nil && !kpolicy.FrozenEqual(p.Disturb, v.Disturb) {
			return domain.NewError(domain.CodeInvalidRequest,
				"disturb routes, push.*.route, proactive.task_push and proactive.instance_push are frozen once disturb.delivery is saved",
				"change where reminders go in disturb.delivery and send the frozen keys back unchanged")
		}
		stampSince(p.Disturb.Proactive, v.Proactive)
		p.Disturb = v.Disturb
		return nil
	},
}

// disturbBody 是 disturb 段的请求与响应形状：策略的 disturb 加只读的 delivery_saved。
// threshold_judge、followup_defer 是已取消（2026-11-23）的两个开关：旧客户端照旧整段回传时接受并忽略，响应里不再出现。
type disturbBody struct {
	kpolicy.Disturb
	DeliverySaved  *bool `json:"delivery_saved,omitempty"`
	ThresholdJudge *bool `json:"threshold_judge,omitempty"`
	FollowupDefer  *bool `json:"followup_defer,omitempty"`
}

// instancePacks 是全部定时任务实例 id → 类型，用于换算按实例覆盖的矩阵行。
func instancePacks(tx *store.Tx) (map[string]string, error) {
	list, err := instances.List(tx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(list))
	for _, in := range list {
		out[in.ID] = in.Pack
	}
	return out, nil
}

// stampSince：到点提醒的开启时刻只由这里记（请求体里带的一律不认）：从关到开记现在，一直开着沿用原值，关着清空。
// 早于它的提醒点不推，所以开启时不会把过去的提醒一次灌进来（§7.4）。
func stampSince(old, next *kpolicy.Proactive) {
	if next == nil {
		return
	}
	next.Since = nil
	if !next.On {
		return
	}
	if old != nil && old.On && old.Since != nil {
		next.Since = old.Since
		return
	}
	now := clock.Now().UTC()
	next.Since = &now
}

const (
	rules    = authz.GroupRules
	agents   = authz.GroupAgents
	outbound = authz.GroupOutbound
	owner    = authz.GroupOwner
)

var (
	policySections = []section{
		disturbSection,
		sect("persona", rules, "/settings/general", func(p *kpolicy.Policy) *string { return &p.Persona }),
		sect("persona_lang", rules, "/settings/general", func(p *kpolicy.Policy) *string { return &p.PersonaLang }),
		sect("background_run_timeout_sec", rules, "/settings/agents", func(p *kpolicy.Policy) *int { return &p.BackgroundRunTimeoutSec }),
		sect("channels", rules, "/settings/channels", func(p *kpolicy.Policy) *map[string]kpolicy.Channel { return &p.Channels }),
		sect("limits", rules, "/settings/agents", func(p *kpolicy.Policy) *kpolicy.Limits { return &p.Limits }),
		sect("default_agent_id", agents, "/settings/agents", func(p *kpolicy.Policy) **string { return &p.DefaultAgentID }),
		sect("main_agent_id", agents, "/settings/agents", func(p *kpolicy.Policy) **string { return &p.MainAgentID }),
		sect("rule_agent", agents, "/settings/agents", func(p *kpolicy.Policy) *map[string]string { return &p.RuleAgent }),
		sect("default_harness", agents, "/settings/agents", func(p *kpolicy.Policy) *string { return &p.DefaultHarness }),
		sect("main_session_launch", agents, "/settings/agents", func(p *kpolicy.Policy) *kpolicy.MainSessionLaunch { return &p.MainSessionLaunch }),
	}
	allowlistSections = []section{
		sect("forward_allowlist", rules, "/sources", func(p *kpolicy.Policy) *map[string][]string { return &p.ForwardAllowlist }),
		sect("channel_answerers", owner, "/settings/channels", func(p *kpolicy.Policy) *map[string][]string { return &p.ChannelAnswerers }),
	}
	accessLimitSections = []section{
		// 权限卡只有本人能改；这里只给读，写走 PUT /settings/agents/{agent}/card（一次只改一张卡）。
		{key: "agent_cards", group: owner, page: "/settings/agents", get: func(p *kpolicy.Policy) any { return p.AgentCards }},
	}
	// 笔记库登记（§12.8）：整体替换；名字重复、模板不以协议开头或用了被拒绝的协议由内核判为 400。
	noteVaultSections = []section{
		sect("vaults", outbound, "/settings/note-vaults", func(p *kpolicy.Policy) *[]kpolicy.NoteVault { return &p.NoteVaults }),
	}
	judgmentSections = []section{
		sect("bands", rules, "/judgment", func(p *kpolicy.Policy) *[]kpolicy.JudgmentBand { return &p.Judgment.Bands }),
		sect("shadow", rules, "/judgment", func(p *kpolicy.Policy) *[]string { return &p.Judgment.Shadow }),
		sect("spot_check", rules, "/judgment", func(p *kpolicy.Policy) *kpolicy.SpotCheck { return &p.Judgment.SpotCheck }),
	}
	modelEndpointSections = []section{
		{key: "judge_endpoint", group: outbound, page: "/settings/models", get: getJudge, set: setJudge},
		{key: "generate_endpoint", group: outbound, page: "/settings/models", get: getGenerate, set: setGenerate},
		// 暂停模型：两个端点原样保留，内核按未配置运行（§8.7）；没有端点时内核规整为 false。
		sect("models_paused", rules, "/settings/models", func(p *kpolicy.Policy) *bool { return &p.ModelsPaused }),
		// v0.1 的请求形状：只在 PUT 里被接受（等同只设生成端点，位置 external），响应一律是新形状（附录 B.2）。
		{key: legacyKey, group: outbound, page: "/settings/models", set: setLegacy},
	}
)

// mergeSections 是设置 › 跨来源合并（§9.3）：执行合并的 Agent（same、off；空即 same）与合并任务本身的设置。
// 提示词的校验在内核（缺段、空段、少结论词、超长都是 400，不生效）；prompt_at 只读，提示词有变化时由这里记下保存时刻。
var mergeSections = []section{
	sect("merge_judge", rules, "/settings/merge", func(p *kpolicy.Policy) *string { return &p.MergeJudge }),
	{key: "merge", group: rules, page: "/settings/merge",
		get: func(p *kpolicy.Policy) any { return p.Merge },
		set: func(p *kpolicy.Policy, raw json.RawMessage) error {
			var v kpolicy.MergeSettings
			if err := strictDecode(raw, &v); err != nil {
				return err
			}
			v.Prompt = strings.TrimSpace(v.Prompt)
			v.PromptAt = p.Merge.PromptAt
			if v.Prompt != p.Merge.Prompt {
				now := clock.Now().UTC()
				v.PromptAt = &now
			}
			p.Merge = v
			return nil
		}},
}

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
	// 资料预算（估算 token）只有 Web 能改（策略写入口本身要求 Web 登录）；省略时保持已存的值，不让旧形状的请求悄悄重置它；
	// 给了就必须在范围内，0 也拒绝（所以用指针区分"没带"与"带了 0"）。
	MaxMaterialTokens *int `json:"max_material_tokens"`
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

// keepBudget：请求没带预算时沿用已存的值；带了就必须在范围内（0、负数、范围外都 400，不当作"没设"）。
func keepBudget(in *int, old int) (int, error) {
	if in == nil {
		return old, nil
	}
	return *in, kpolicy.CheckBudgetTokens("generate_endpoint max_material_tokens", *in)
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
		"max_material_tokens": g.MaterialBudget()}
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
	budget, err := keepBudget(r.MaxMaterialTokens, p.GenerateEndpoint.MaxMaterialTokens)
	if err != nil {
		return err
	}
	p.GenerateEndpoint = kpolicy.GenerateEndpoint{URL: r.URL, APIKey: keepKey(r.APIKey, p.GenerateEndpoint.APIKey), Model: r.Model, Locality: r.Locality, Protocol: r.Protocol,
		MaxMaterialTokens: budget}
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
		MaxMaterialTokens: p.GenerateEndpoint.MaxMaterialTokens}
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
			"/settings/merge":           mergeSections,
		} {
			r.Handle(core.Route{Method: "GET", Path: path, Handler: read(st, path, secs)})
			r.Handle(core.Route{Method: "PUT", Path: path, Handler: write(st, path, secs)})
		}
		r.Handle(core.Route{Method: "POST", Path: "/settings/proactive/preview", Handler: proactivePreview(st)})
		r.Handle(core.Route{Method: "PUT", Path: "/settings/channels/{name}/answerers", Handler: answerers(st)})
		r.Handle(core.Route{Method: "PUT", Path: "/settings/agents/{agent}/card", Handler: agentCard(st)})
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
		basis, err := Authorize(st, r, p, authz.GroupChannels, "/settings/channels", "settings:/settings/channels/{name}")
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
		basis, err := Authorize(st, r, p, authz.GroupOwner, "/settings/channels", "settings:/settings/channels/{name}/answerers")
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

// agentCard 设一个 Agent 的权限卡（§14.5）：十组的开放与到期、拍板的项目限定、工具档位、后台运行看哪些消息。
// 改权限卡只有本人能做，须刚输过口令。可带 version 做乐观并发。Agent 是否存在不在这里校验：没有卡片的 id 不会被任何调用者匹配到。
func agentCard(st *store.Store) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		basis, err := Authorize(st, r, p, authz.GroupOwner, "/settings/agents", "settings:/settings/agents/{agent}/card")
		if err != nil {
			return err
		}
		var in struct {
			kpolicy.Card
			Version *int `json:"version"`
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
			if v, err = kpolicy.UpdateWith(tx, p.Caller.Actor, basis, "settings: permission card of agent "+agent, func(np *kpolicy.Policy) error {
				if np.AgentCards == nil {
					np.AgentCards = map[string]kpolicy.Card{}
				}
				np.AgentCards[agent] = in.Card
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
		return core.WriteJSON(w, http.StatusOK, map[string]any{"version": v, "card": pol.Card(agent)})
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

// rulesOnly：提醒设置的预览与总览属"改日常规则与提醒"（§14.3），Web 本人直接放行。
func rulesOnly(st *store.Store, r *http.Request, p *core.Principal) error {
	if p.Web {
		return nil
	}
	_, err := core.Authorize(st, r, p, authz.GroupRules, "/settings/notify", "api:"+r.URL.Path)
	return err
}

// Authorize 判一次不走命令管线的设置读写（§14.3），见 core.Authorize。
func Authorize(st *store.Store, r *http.Request, p *core.Principal, group authz.Group, page, object string) (*domain.Basis, error) {
	return core.Authorize(st, r, p, group, page, object)
}

// visible 返回 secs 里调用者的权限卡开放了的段；一段都没有时按第一段的组拒绝并留痕一次。
func visible(st *store.Store, r *http.Request, p *core.Principal, secs []section, object string) ([]section, error) {
	var (
		shown    []section
		first    *core.Rejection
		firstErr error
	)
	for _, s := range secs {
		_, rej, err := core.Decide(st, r, p, s.group, s.page, object)
		switch {
		case err == nil:
			shown = append(shown, s)
		case firstErr == nil:
			first, firstErr = rej, err
		}
	}
	if len(shown) == 0 {
		if first != nil {
			core.Record(st, r, first)
		}
		return nil, firstErr
	}
	return shown, nil
}

// authorizeAll 判一组段改到的每一组（去重，按段的顺序），返回第一组的依据；任何一组被拒即整体拒绝。
func authorizeAll(st *store.Store, r *http.Request, p *core.Principal, secs []section, object string) (*domain.Basis, error) {
	var basis *domain.Basis
	seen := map[authz.Group]bool{}
	for _, s := range secs {
		if seen[s.group] {
			continue
		}
		seen[s.group] = true
		b, err := Authorize(st, r, p, s.group, s.page, object)
		if err != nil {
			return nil, err
		}
		if basis == nil {
			basis = b
		}
	}
	return basis, nil
}

func render(tx *store.Tx, version int, pol kpolicy.Policy, secs []section) (map[string]any, error) {
	out := map[string]any{"version": version}
	for _, s := range secs {
		switch {
		case s.view != nil:
			v, err := s.view(tx, &pol)
			if err != nil {
				return nil, err
			}
			out[s.key] = v
		case s.get != nil:
			out[s.key] = s.get(&pol)
		}
	}
	return out, nil
}

func read(st *store.Store, path string, secs []section) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		// 读：Web 本人看全部段；Agent 只看权限卡开放了的组的段（改之前要知道现值）。一段都看不到时按第一段的组拒绝，
		// 与写入同一条留痕路径。
		shown := secs
		if !p.Web {
			var err error
			if shown, err = visible(st, r, p, secs, "settings:"+path); err != nil {
				return err
			}
		}
		var out map[string]any
		if err := st.View(r.Context(), func(tx *store.Tx) error {
			v, pol, err := kpolicy.Stored(tx)
			if err == nil {
				out, err = render(tx, v, pol, shown)
			}
			return err
		}); err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, out)
	}
}

func write(st *store.Store, path string, secs []section) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		// 先拒绝这条路由上一组都做不了的调用者：不论请求体是什么都先拒绝（不给它探请求形状）。
		if !p.Web {
			if _, err := visible(st, r, p, secs, "settings:"+path); err != nil {
				return err
			}
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
		basis, err := authorizeAll(st, r, p, touched, "settings:"+path)
		if err != nil {
			return err
		}
		for _, s := range touched {
			if s.set == nil && s.setTx == nil {
				return domain.NewError(domain.CodeInvalidRequest, "section "+s.key+" is read-only on this route", "change a permission card with PUT /settings/agents/{agent}/card")
			}
		}
		var out map[string]any
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
			v, err := kpolicy.UpdateWith(tx, p.Caller.Actor, basis, "settings: PUT "+path, func(np *kpolicy.Policy) error {
				if np.DefaultAgentID != nil && body["default_harness"] != nil {
					return domain.NewError(domain.CodeConflict, "已启用 Agent 角色设置", "请用新的角色入口")
				}
				for k, raw := range body {
					if k == "main_session_launch" && len(np.MainSessionLaunch.ByAgent) > 0 {
						var fields map[string]json.RawMessage
						if json.Unmarshal(raw, &fields) == nil && fields["by_agent"] == nil {
							return domain.NewError(domain.CodeConflict, "请重新载入 Agent 设置以保留实例策略", "reload Agent settings")
						}
					}
					set := known[k].set
					if st := known[k].setTx; st != nil {
						set = func(p *kpolicy.Policy, raw json.RawMessage) error { return st(tx, p, raw) }
					}
					if err := set(np, raw); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				return err
			}
			_, pol, err := kpolicy.Stored(tx) // 设置页显示存储的原文：暂停时端点照常在内
			if err == nil {
				out, err = render(tx, v, pol, secs)
			}
			return err
		})
		if errors.Is(err, kpolicy.ErrInvalid) {
			return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the setting and send it again; nothing was saved")
		}
		if err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, out)
	}
}

// detached：写入一旦开始就不因客户端断开而半途取消。
func detached(r *http.Request) context.Context { return context.WithoutCancel(r.Context()) }
