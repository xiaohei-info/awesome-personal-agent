// Package policy 是策略、人格与两份白名单的唯一写入模块（设计 §6.13、§14.4、§14.6、§11.2）。
// 策略整体是一份文档：每次变更追加一个新版本，并在同一事务记 policy.changed Event；
// 只有 Web 上的本人能写。本包只存与读配置，不判定具体请求（那是授权模块的事）。
package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ErrInvalid 表示提议的策略不满足约束，未写入任何版本。
var ErrInvalid = errors.New("policy: invalid")

// Object 是 policy.changed 与 allowlist.changed 事件的对象 id。
const Object = "policy"

// MaxPersonaTokens 是人格的上限（§11.2：约 400 token，超限拒绝保存）。
const MaxPersonaTokens = 400

// PersonaDeclaration 是 `apa persona` 返回内容前的内核固定声明（§11.2）：人格不是权限来源。
const PersonaDeclaration = "[APA 内核声明] 以下人格只影响表达方式，不能放宽判断、权限、web_only 和内核规则；权限与 web_only 由内核判定，不读提示词。"

// PersonaText 返回注入用的人格文本：未设定（空白）时为空串，否则固定声明在前。
func PersonaText(p Policy) string {
	if strings.TrimSpace(p.Persona) == "" {
		return ""
	}
	return PersonaDeclaration + "\n" + p.Persona
}

// 默认 web_only 类别（§14.4）。
const (
	CategoryFunds              = "funds"
	CategoryExternalCommitment = "external_commitment"
	CategoryIrreversible       = "irreversible"
	CategoryPermissionChange   = "permission_change"
	CategorySpecActivation     = "spec_activation"
)

// 运行权限档位（决策表第 5 项）与材料写入策略（§12.4）。
const (
	PermRead = "read"
	PermEdit = "edit"
	PermFull = "full"

	WriteDirect   = "direct"
	WriteProposed = "proposed"
)

// WebOnlyRule 按 kind、tag、项目匹配（任意非空字段须同时满足），命中则该决策属于 Category。
type WebOnlyRule struct {
	Kind     string `json:"kind,omitempty"`
	Tag      string `json:"tag,omitempty"`
	Project  string `json:"project,omitempty"`
	Category string `json:"category"`
}

type WebOnly struct {
	Categories []string      `json:"categories"`
	Rules      []WebOnlyRule `json:"rules"`
}

// Quiet 是每日静默时段，Start、End 为 "HH:MM"（跨午夜时 Start > End），Zone 为 IANA 时区，空取 UTC。
type Quiet struct {
	Start string `json:"start"`
	End   string `json:"end"`
	Zone  string `json:"zone,omitempty"`
}

// Disturb 是打扰策略（§7.4）。Routes 的键为投递类别，或「类别:优先级」（后者优先），值为路由。
// Quiet 为空表示不设静默；QuietExempt 为真时，临近截止的决策不受静默限制（默认静默优先）；
// MergeWindowSec 是低优先级提醒的合并窗口，0 表示不合并；RemindOnDeadline 为真时，截止已过仍未处理的事项再提醒一次。
// ThresholdJudge 为真时，决策投递在按优先级定路由之外再经判断型问“这条决策可以晚点送吗”（只能降低打扰：是则推迟到合并窗口后单独投递），默认关闭（§7.4）。
type Disturb struct {
	Routes           map[string]string `json:"routes"`
	Quiet            *Quiet            `json:"quiet,omitempty"`
	QuietExempt      bool              `json:"quiet_exempt,omitempty"`
	MergeWindowSec   int               `json:"merge_window_sec"`
	RemindOnDeadline bool              `json:"remind_on_deadline"`
	ThresholdJudge   bool              `json:"threshold_judge,omitempty"`
	// DailyCap 是每个自然日经渠道推到本人面前的提醒上限，0 不限（§7.4）；DefaultDailyCap 是缺省，旧版本策略文档没有此键时也取它。
	DailyCap int `json:"daily_cap"`
	// Mutes 按来源、项目或事项静音；到期（Until）后自动恢复。只在 Web 改。
	Mutes         []Mute `json:"mutes"`
	FollowupDefer bool   `json:"followup_defer"`
}

// DefaultDailyCap 是日上限缺省值（§7.4）。
const DefaultDailyCap = 20

// Mute 的 Scope 取值。
const (
	MuteSource  = "source"
	MuteProject = "project"
	MuteItem    = "item"
)

// Mute 是一条静音：ID 为来源、项目或事项 id；Until 为空表示不到期。
type Mute struct {
	Scope string     `json:"scope"`
	ID    string     `json:"id"`
	Until *time.Time `json:"until,omitempty"`
}

// ParseClock 解析 "HH:MM"，返回当天的分钟数。
func ParseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// MaterialWrite 是材料 kind 到写入策略的映射；未登记的 kind 走 Default。
type MaterialWrite struct {
	Default string            `json:"default"`
	Kinds   map[string]string `json:"kinds"`
}

// PermissionCap 是运行权限上限：按 harness 设置，未设置的用 Default。
type PermissionCap struct {
	Default   string            `json:"default"`
	ByHarness map[string]string `json:"by_harness"`
}

// 端点位置与判断协议（§6.13、§8.8）。
const (
	LocalityLocal    = "local"
	LocalityExternal = "external"

	ProtocolProbability  = "probability-judgment/1"
	ProtocolChatLogprobs = "chat-logprobs"
	// ProtocolSystemOne 是原生分类模型服务的判断接口：一次请求带共享 state 与一组具名是非题（§8.8）。
	ProtocolSystemOne = "system-one"
)

// 判断端点的默认值（附录 B.2、§8.8）。
const (
	DefaultJudgeTimeoutMs        = 10000
	DefaultJudgeMaxBatch         = 32
	DefaultJudgeMaxMaterialChars = 8000
	DefaultScreeningTimeoutMs    = 30000
)

// EffectiveLocality 是端点位置的实际取值：未声明按 external（§14.14）。
func EffectiveLocality(l string) string {
	if l == LocalityLocal {
		return LocalityLocal
	}
	return LocalityExternal
}

// GenerateEndpoint 是对话补全兼容的生成端点；空 URL 表示未配置。APIKey 只存账本，不进 Event。
// Locality 由本人声明端点是否在自己可控的主机上，未声明按 external（§14.14）。
type GenerateEndpoint struct {
	URL      string `json:"url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
	Locality string `json:"locality,omitempty"`
}

// JudgeEndpoint 是分类（判断）端点；Policy 里为 nil 表示未配置。
type JudgeEndpoint struct {
	URL              string `json:"url"`
	APIKey           string `json:"api_key,omitempty"`
	Protocol         string `json:"protocol"`
	Model            string `json:"model,omitempty"`
	Locality         string `json:"locality"`
	TimeoutMs        int    `json:"timeout_ms"`
	MaxBatch         int    `json:"max_batch"`
	MaxMaterialChars int    `json:"max_material_chars"`
}

// legacyModelEndpoint 是 v0.1 策略文档里的单一 model_endpoint，只在读取旧版本时出现（§16.5）。
type legacyModelEndpoint struct {
	URL                string `json:"url"`
	Model              string `json:"model"`
	APIKey             string `json:"api_key"`
	ScreeningTimeoutMs int    `json:"screening_timeout_ms"`
}

// Channel 是渠道的登记属性（§10.3）。TextOnly：渠道只能发文字，决策投递带一次性表单链接。
type Channel struct {
	TextOnly bool `json:"text_only"`
}

// JudgmentBand 是策略对某模板不确定带的覆盖；Source 非空时只对该来源生效，且优先于模板级条目（§8.9、§8.10）。
type JudgmentBand struct {
	Template string  `json:"template"`
	Source   string  `json:"source,omitempty"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
}

// SpotCheck 是抽检设置：PerBrief 是每份简报的抽检条数。
type SpotCheck struct {
	PerBrief int `json:"per_brief"`
}

// DefaultSpotCheckPerBrief 是抽检条数缺省值（§8.10）；旧版本策略文档没有此键时也取它。
const DefaultSpotCheckPerBrief = 5

// Judgment 是判断设置（§6.13）：不确定带覆盖、影子模式的模板列表与抽检。只在 Web 改。
type Judgment struct {
	Bands     []JudgmentBand `json:"bands"`
	Shadow    []string       `json:"shadow"`
	SpotCheck SpotCheck      `json:"spot_check"`
}

// Policy 是一个版本的完整配置。两份白名单相互独立：键是来源或渠道，值是发送者或回答人。
type Policy struct {
	WebOnly          WebOnly          `json:"web_only"`
	Disturb          Disturb          `json:"disturb"`
	MaterialWrite    MaterialWrite    `json:"material_write"`
	PermissionCap    PermissionCap    `json:"permission_cap"`
	JudgeEndpoint    *JudgeEndpoint   `json:"judge_endpoint"`
	GenerateEndpoint GenerateEndpoint `json:"generate_endpoint"`
	// ScreeningTimeoutMs 是 web_only 初筛的超时（v0.1 时在 model_endpoint 里）。
	ScreeningTimeoutMs int                 `json:"screening_timeout_ms"`
	Persona            string              `json:"persona"` // 只影响表达方式，不进入任何权限判定
	ForwardAllowlist   map[string][]string `json:"forward_allowlist"`
	ChannelAnswerers   map[string][]string `json:"channel_answerers"`
	Channels           map[string]Channel  `json:"channels"`
	// DefaultHarness 是规则未指定 harness 时任务型与后台运行用的 harness（§13.6）；为空表示未设置，任务型转 needs_input。
	DefaultHarness string   `json:"default_harness"`
	Judgment       Judgment `json:"judgment"`
	// ModelsPaused 为真时两个端点的配置与密钥原样保留，但内核按两者都未配置运行（Current 给出的就是这个视图，§8.7）。
	// 没有任何端点时规整为假。只在 Web 改。
	ModelsPaused bool `json:"models_paused"`
}

// Default 是版本 0 的配置：默认类别、权限上限 read、人格与白名单为空。
func Default() Policy {
	return Policy{
		WebOnly: WebOnly{
			// 已按字典序排列：normalize 会排序，默认值必须是它的不动点。
			Categories: []string{CategoryExternalCommitment, CategoryFunds, CategoryIrreversible, CategoryPermissionChange, CategorySpecActivation},
			Rules:      []WebOnlyRule{},
		},
		Disturb: Disturb{Routes: map[string]string{
			string(domain.DeliveryDecision): "inject", string(domain.DeliveryBrief): "inject",
			string(domain.DeliveryReminder): "inject", string(domain.DeliveryHealth): "web",
			string(domain.DeliveryWake): "web",
		}, MergeWindowSec: 30 * 60, RemindOnDeadline: true, DailyCap: DefaultDailyCap, Mutes: []Mute{}}, // 合并窗口默认 30 分钟（§7.4）
		// 规范、方案类须本人确认；纪要类直写（§12.4）。未登记的 kind 取更严的 proposed。
		MaterialWrite: MaterialWrite{Default: WriteProposed, Kinds: map[string]string{
			"minutes": WriteDirect, "reference": WriteDirect, "worknote": WriteDirect,
			"requirement": WriteProposed, "design": WriteProposed, "guideline": WriteProposed,
		}},
		PermissionCap:      PermissionCap{Default: PermRead, ByHarness: map[string]string{}},
		ScreeningTimeoutMs: DefaultScreeningTimeoutMs,
		ForwardAllowlist:   map[string][]string{},
		ChannelAnswerers:   map[string][]string{},
		Channels:           map[string]Channel{},
		Judgment:           Judgment{Bands: []JudgmentBand{}, Shadow: []string{}, SpotCheck: SpotCheck{PerBrief: DefaultSpotCheckPerBrief}},
	}
}

// Module 是本模块在存储层登记的迁移：只增表，版本号即主键，旧版本永不改写。
var Module = store.Module{Name: "policy", Migrations: []store.Migration{{
	Version: 1, Name: "create-policy-version",
	SQL: `CREATE TABLE policy_version (
	version INTEGER PRIMARY KEY,
	doc TEXT NOT NULL,
	at INTEGER NOT NULL
);`,
}, {
	// 授权单（§6.13）：doc 是不可变内容，撤销状态是唯一会变的列。
	Version: 2, Name: "create-policy-grant",
	SQL: `CREATE TABLE policy_grant (
	id TEXT PRIMARY KEY,
	credential TEXT NOT NULL,
	doc TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	revoked_at INTEGER NOT NULL DEFAULT 0,
	revoked_reason TEXT NOT NULL DEFAULT ''
);
CREATE INDEX policy_grant_credential ON policy_grant(credential);`,
}}}

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, a...))
}

// EstimateTokens 给出保守的 token 估计：CJK 与其他非 ASCII 字符各计 1，ASCII 约每 4 字符 1。
func EstimateTokens(s string) int {
	wide, ascii := 0, 0
	for _, r := range s {
		switch {
		case r > unicode.MaxASCII:
			wide++
		case !unicode.IsSpace(r):
			ascii++
		}
	}
	return wide + (ascii+3)/4
}

// dedupSorted 去掉空白与重复并排序；全部为空时返回非 nil 空切片，保证序列化稳定。
func dedupSorted(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func normList(m map[string][]string) map[string][]string {
	out := map[string][]string{}
	for k, v := range m {
		if k = strings.TrimSpace(k); k == "" {
			continue
		}
		if v = dedupSorted(v); len(v) > 0 {
			out[k] = v
		}
	}
	return out
}

func oneOf(v string, set ...string) bool {
	for _, s := range set {
		if v == s {
			return true
		}
	}
	return false
}

// normalize 校验并规整 p；失败返回 ErrInvalid。
func normalize(p Policy) (Policy, error) {
	p.WebOnly.Categories = dedupSorted(p.WebOnly.Categories)
	if len(p.WebOnly.Categories) == 0 {
		return p, invalid("web_only categories is empty")
	}
	cats := map[string]bool{}
	for _, c := range p.WebOnly.Categories {
		cats[c] = true
	}
	if p.WebOnly.Rules == nil {
		p.WebOnly.Rules = []WebOnlyRule{}
	}
	for i, r := range p.WebOnly.Rules {
		if r.Kind == "" && r.Tag == "" && r.Project == "" {
			return p, invalid("web_only rule %d matches nothing (kind/tag/project all empty)", i)
		}
		if !cats[r.Category] {
			return p, invalid("web_only rule %d category %q is not in categories", i, r.Category)
		}
	}

	if len(p.Disturb.Routes) == 0 {
		return p, invalid("disturb routes is empty")
	}
	for k, route := range p.Disturb.Routes {
		class, prio, hasPrio := strings.Cut(k, ":")
		if !oneOf(class, string(domain.DeliveryDecision), string(domain.DeliveryBrief), string(domain.DeliveryReminder), string(domain.DeliveryHealth), string(domain.DeliveryWake)) {
			return p, invalid("disturb key %q: unknown delivery class", k)
		}
		if hasPrio && !domain.Priority(prio).Valid() {
			return p, invalid("disturb key %q: unknown priority", k)
		}
		if strings.TrimSpace(route) == "" {
			return p, invalid("disturb key %q: route is empty", k)
		}
	}

	if p.Disturb.DailyCap < 0 {
		return p, invalid("disturb daily_cap %d < 0", p.Disturb.DailyCap)
	}
	if p.Disturb.Mutes == nil {
		p.Disturb.Mutes = []Mute{}
	}
	for i, m := range p.Disturb.Mutes {
		if !oneOf(m.Scope, MuteSource, MuteProject, MuteItem) || strings.TrimSpace(m.ID) == "" {
			return p, invalid("disturb mute %d needs scope source|project|item and a non-empty id", i)
		}
	}
	if p.Disturb.MergeWindowSec < 0 {
		return p, invalid("disturb merge_window_sec %d < 0", p.Disturb.MergeWindowSec)
	}
	if q := p.Disturb.Quiet; q != nil {
		a, errA := ParseClock(q.Start)
		b, errB := ParseClock(q.End)
		if errA != nil || errB != nil || a == b {
			return p, invalid("disturb quiet %q-%q must be two different HH:MM times", q.Start, q.End)
		}
		if _, err := time.LoadLocation(q.Zone); err != nil {
			return p, invalid("disturb quiet zone %q", q.Zone)
		}
	}

	if !oneOf(p.MaterialWrite.Default, WriteDirect, WriteProposed) {
		return p, invalid("material_write default %q", p.MaterialWrite.Default)
	}
	for k, v := range p.MaterialWrite.Kinds {
		if k == "" || !oneOf(v, WriteDirect, WriteProposed) {
			return p, invalid("material_write kind %q policy %q", k, v)
		}
	}
	if p.MaterialWrite.Kinds == nil {
		p.MaterialWrite.Kinds = map[string]string{}
	}

	if !oneOf(p.PermissionCap.Default, PermRead, PermEdit, PermFull) {
		return p, invalid("permission_cap default %q", p.PermissionCap.Default)
	}
	for h, v := range p.PermissionCap.ByHarness {
		if h == "" || !oneOf(v, PermRead, PermEdit, PermFull) {
			return p, invalid("permission_cap harness %q level %q", h, v)
		}
	}
	if p.PermissionCap.ByHarness == nil {
		p.PermissionCap.ByHarness = map[string]string{}
	}

	if p.ScreeningTimeoutMs <= 0 {
		return p, invalid("screening_timeout_ms %d <= 0", p.ScreeningTimeoutMs)
	}
	if err := normalizeEndpoints(&p); err != nil {
		return p, err
	}
	if p.GenerateEndpoint.URL == "" && p.JudgeEndpoint == nil {
		p.ModelsPaused = false
	}

	if n := EstimateTokens(p.Persona); n > MaxPersonaTokens {
		return p, invalid("persona is about %d tokens, limit %d", n, MaxPersonaTokens)
	}
	p.ForwardAllowlist = normList(p.ForwardAllowlist)
	p.ChannelAnswerers = normList(p.ChannelAnswerers)
	if p.Channels == nil {
		p.Channels = map[string]Channel{}
	}
	return normalizeJudgment(p)
}

// normalizeJudgment 校验判断设置：带的两端在 [0,1] 且下限不大于上限，同一（模板，来源）只能出现一次。
func normalizeJudgment(p Policy) (Policy, error) {
	j := &p.Judgment
	if j.Bands == nil {
		j.Bands = []JudgmentBand{}
	}
	seen := map[[2]string]bool{}
	for i, b := range j.Bands {
		if strings.TrimSpace(b.Template) == "" {
			return p, invalid("judgment band %d has no template", i)
		}
		if !(b.Low >= 0 && b.High <= 1 && b.Low <= b.High) { // 写成取反，NaN 也被拒
			return p, invalid("judgment band %d (%s): need 0 <= low <= high <= 1, got low=%v high=%v (low must not be greater than high)", i, b.Template, b.Low, b.High)
		}
		k := [2]string{b.Template, b.Source}
		if seen[k] {
			return p, invalid("judgment band %d: template %q source %q is listed twice", i, b.Template, b.Source)
		}
		seen[k] = true
	}
	j.Shadow = dedupSorted(j.Shadow)
	if j.Shadow == nil {
		j.Shadow = []string{}
	}
	if j.SpotCheck.PerBrief < 0 {
		return p, invalid("judgment spot_check per_brief %d < 0", j.SpotCheck.PerBrief)
	}
	return p, nil
}

func checkURL(field, raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return invalid("%s url %q is not an http(s) URL", field, raw)
	}
	return nil
}

// locality 未声明按 external（§14.14）。
func normLocality(field, v string) (string, error) {
	switch v {
	case "":
		return LocalityExternal, nil
	case LocalityLocal, LocalityExternal:
		return v, nil
	}
	return "", invalid("%s locality %q must be local or external", field, v)
}

// NormalizeEndpoints 按保存时的同一套规则校验并补全两个端点；Web 的"测试连接"用它检查未保存的草稿。
// 不合规时返回包装 ErrInvalid 的错误。
func NormalizeEndpoints(p *Policy) error { return normalizeEndpoints(p) }

// normalizeEndpoints 校验两个端点并补全默认值；生成端点的 URL 为空时整个视为未配置。
func normalizeEndpoints(p *Policy) (err error) {
	if g := &p.GenerateEndpoint; g.URL == "" {
		*g = GenerateEndpoint{}
	} else {
		if err = checkURL("generate_endpoint", g.URL); err != nil {
			return err
		}
		if g.Locality, err = normLocality("generate_endpoint", g.Locality); err != nil {
			return err
		}
	}
	j := p.JudgeEndpoint
	if j == nil {
		return nil
	}
	if err = checkURL("judge_endpoint", j.URL); err != nil {
		return err
	}
	if j.Locality, err = normLocality("judge_endpoint", j.Locality); err != nil {
		return err
	}
	switch j.Protocol {
	case "":
		j.Protocol = ProtocolProbability
	case ProtocolProbability:
	case ProtocolChatLogprobs, ProtocolSystemOne:
		if j.Model == "" {
			return invalid("judge_endpoint model is required for protocol %s", j.Protocol)
		}
	default:
		return invalid("judge_endpoint protocol %q", j.Protocol)
	}
	for _, f := range []struct {
		name string
		v    *int
		def  int
	}{{"timeout_ms", &j.TimeoutMs, DefaultJudgeTimeoutMs}, {"max_batch", &j.MaxBatch, DefaultJudgeMaxBatch}, {"max_material_chars", &j.MaxMaterialChars, DefaultJudgeMaxMaterialChars}} {
		if *f.v < 0 {
			return invalid("judge_endpoint %s %d < 0", f.name, *f.v)
		}
		if *f.v == 0 {
			*f.v = f.def
		}
	}
	return nil
}

// clone 经 JSON 深拷贝，使变更函数不会污染调用方持有的快照。
func clone(p Policy) Policy {
	b, _ := json.Marshal(p)
	var out Policy
	_ = json.Unmarshal(b, &out)
	return out
}

// decode 读一个存储的版本；legacy 为真表示它还是 v0.1 的 model_endpoint 形状（读作生成端点，不改写存储）。
func decode(doc string) (p Policy, legacy bool, err error) {
	w := struct {
		Policy
		Legacy *legacyModelEndpoint `json:"model_endpoint"`
	}{Policy: Policy{Disturb: Disturb{DailyCap: DefaultDailyCap}, Judgment: Judgment{SpotCheck: SpotCheck{PerBrief: DefaultSpotCheckPerBrief}}}} // 旧文档没有 daily_cap：升级后日上限即生效
	if err := json.Unmarshal([]byte(doc), &w); err != nil {
		return Policy{}, false, fmt.Errorf("policy: stored document: %w", err)
	}
	p = w.Policy
	if l := w.Legacy; l != nil {
		legacy = true
		p.GenerateEndpoint = GenerateEndpoint{URL: l.URL, Model: l.Model, APIKey: l.APIKey}
		p.ScreeningTimeoutMs = l.ScreeningTimeoutMs
	}
	p, err = normalize(p)
	return p, legacy, err
}

// Current 返回最新版本号与生效中的配置；尚无版本时为 (0, Default())。
// 模型暂停时两个端点在这里已视为未配置（ModelsPaused 仍为真，供调用方区分原因）：内核各处据此退化，与没配端点一致（§8.7）。
func Current(tx *store.Tx) (int, Policy, error) {
	v, p, _, err := latest(tx)
	if p.ModelsPaused {
		p.GenerateEndpoint, p.JudgeEndpoint = GenerateEndpoint{}, nil
	}
	return v, p, err
}

// Stored 返回最新版本号与存储的配置原文（暂停时端点与密钥照常在内）。只给设置页的读写与本人手动的端点探测用。
func Stored(tx *store.Tx) (int, Policy, error) {
	v, p, _, err := latest(tx)
	return v, p, err
}

func latest(tx *store.Tx) (int, Policy, bool, error) {
	r, err := tx.Query("SELECT version, doc FROM policy_version ORDER BY version DESC LIMIT 1")
	if err != nil {
		return 0, Policy{}, false, err
	}
	defer r.Close()
	if !r.Next() {
		return 0, Default(), false, r.Err()
	}
	var (
		v   int64
		doc string
	)
	if err := r.Scan(&v, &doc); err != nil {
		return 0, Policy{}, false, err
	}
	p, legacy, err := decode(doc)
	return int(v), p, legacy, err
}

// sections 把配置拆成顶层段，用于判断哪些段变了。
func sections(p Policy) (map[string]json.RawMessage, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	m := map[string]json.RawMessage{}
	return m, json.Unmarshal(b, &m)
}

// Update 是策略的唯一写入口：actor 必须是 Web 上的本人，否则返回 web_only 错误（退出码 20）。
// mutate 在当前配置的副本上修改；结果经校验后若与当前不同，就追加新版本并在同一事务记
// policy.changed（改了白名单时另记 allowlist.changed）。无变化时不产生版本，返回当前版本号。
func Update(tx *store.Tx, actor domain.Actor, reason string, mutate func(*Policy) error) (int, error) {
	if actor.Type != domain.ActorHuman || actor.Via != domain.ViaWeb {
		return 0, domain.NewError(domain.CodeWebOnly, "policy can only be changed by the owner in the Web UI", "ask the owner to change it in the Web settings")
	}
	cur, old, legacy, err := latest(tx)
	if err != nil {
		return 0, err
	}
	next := clone(old)
	if err := mutate(&next); err != nil {
		return 0, err
	}
	if next, err = normalize(next); err != nil {
		return 0, err
	}
	oldSec, err := sections(old)
	if err != nil {
		return 0, err
	}
	newSec, err := sections(next)
	if err != nil {
		return 0, err
	}
	var changed []string
	for k, v := range newSec {
		if !bytes.Equal(v, oldSec[k]) {
			changed = append(changed, k)
		}
	}
	if legacy { // 最新版本还是旧形状：本人保存即写成新字段，哪怕值没变（§16.5）
		changed = append(changed, "model_endpoint")
	}
	if len(changed) == 0 {
		return cur, nil
	}
	sort.Strings(changed)

	doc, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	version := cur + 1
	at := clock.Now()
	if _, err := tx.Exec("INSERT INTO policy_version (version, doc, at) VALUES (?, ?, ?)", int64(version), string(doc), at.UnixNano()); err != nil {
		return 0, err
	}

	shown := clone(next)
	if shown.GenerateEndpoint.APIKey != "" {
		shown.GenerateEndpoint.APIKey = "***"
	}
	if shown.JudgeEndpoint != nil && shown.JudgeEndpoint.APIKey != "" {
		shown.JudgeEndpoint.APIKey = "***"
	}
	payload, err := json.Marshal(map[string]any{"version": version, "previous": cur, "sections": changed, "policy": shown})
	if err != nil {
		return 0, err
	}
	ev := domain.Event{Type: domain.EvPolicyChanged, Object: Object, Actor: actor, Reason: reason, Payload: payload, At: at}
	if _, err := eventlog.Append(tx, ev); err != nil {
		return 0, err
	}
	for _, k := range changed {
		if k == "forward_allowlist" || k == "channel_answerers" {
			ev.Type = domain.EvAllowlistChanged
			if _, err := eventlog.Append(tx, ev); err != nil {
				return 0, err
			}
			break
		}
	}
	return version, nil
}
