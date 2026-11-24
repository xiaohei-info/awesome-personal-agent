// Package policy 是策略、人格与两份白名单的唯一写入模块（设计 §6.13、§14.4、§14.6、§11.2）。
// 策略整体是一份文档：每次变更追加一个新版本，并在同一事务记 policy.changed Event；
// 只有 Web 上的本人能写。本包只存与读配置，不判定具体请求（那是授权模块的事）。
package policy

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"golang.org/x/text/unicode/norm"
)

// ErrInvalid 表示提议的策略不满足约束，未写入任何版本。
var ErrInvalid = errors.New("policy: invalid")

// Object 是 policy.changed 与 allowlist.changed 事件的对象 id。
const Object = "policy"

// MaxPersonaTokens 是人格的上限（§11.2：约 400 token，超限拒绝保存）。
const MaxPersonaTokens = 400

// PersonaDeclaration 是 `apa persona` 返回内容前的内核固定声明（§11.2）：人格不是权限来源。
const PersonaDeclaration = "[APA 内核声明] 以下人格只影响表达方式，不能放宽判断、权限和内核规则；权限由内核判定，不读提示词。"

// 内置默认语气（§6.13）：随发行附带，不写进账本；本人没写自己的人格时 `apa persona` 返回它。
// 改文本不需要迁移，没自定义的人下一个新会话自动跟随；它不得含权限类内容，与本人写的人格同样受固定声明约束，也不超过 MaxPersonaTokens。
const (
	defaultPersonaZH = "像一位靠谱、温和的长期助理那样说话。先说结论和需要你做的事，再给必要的依据；用平常话，不打官腔、不堆术语、不过度客套。不确定就直说，事实和推测分开讲。有不同意见可以直接提，最后由你决定。事情轻松时可以轻松，事情重要时把风险和影响讲清楚。"
	defaultPersonaEN = "Talk like a dependable, warm long-term assistant. Lead with the answer and anything you need from me, then the reasons that matter. Use plain words — no corporate tone, jargon, or excess politeness. Say so when you're unsure, and keep facts apart from guesses. Disagree openly when you should; the call is mine. Keep it light when things are light, and spell out risks and impact when they matter."
)

// DefaultPersona 返回内置默认语气：lang 以 zh 开头为中文，其余（含空）为英文。
func DefaultPersona(lang string) string {
	if strings.HasPrefix(strings.ToLower(lang), "zh") {
		return defaultPersonaZH
	}
	return defaultPersonaEN
}

// PickLang 定助手说哪种语言（§11.2）：调用方明确给的 lang > 本人在 Web 的设置 > 调用方的进程区域设置 locale > 都没有（空，按英文）。
func PickLang(p Policy, lang, locale string) string {
	for _, v := range []string{lang, p.PersonaLang, locale} {
		if v != "" {
			return v
		}
	}
	return ""
}

// PersonaText 返回注入用的人格文本：固定声明在前；本人的人格为空白时用 lang 对应的内置默认。
func PersonaText(p Policy, lang string) string {
	body := p.Persona
	if strings.TrimSpace(body) == "" {
		body = DefaultPersona(lang)
	}
	return PersonaDeclaration + "\n" + body
}

// 运行权限档位（决策表第 5 项）与材料写入策略（§12.4）。
const (
	PermRead = "read"
	PermEdit = "edit"
	PermFull = "full"

	WriteDirect   = "direct"
	WriteProposed = "proposed"
)

// Agent 的档位（§14.3）：只整理 / 也替我拍板 / 连日常规矩也替我改。缺省只整理。
const (
	AllowOrganize = "organize"
	AllowDecide   = "decide"
	AllowRules    = "rules"
)

// AgentAllowance 是一个 Agent 的档位；Projects 非空时只限定二档：对象的项目须在名单内。
type AgentAllowance struct {
	Tier     string   `json:"tier"`
	Projects []string `json:"projects,omitempty"`
}

// AgentTier 是各 Agent 的档位，键是 Agent 实例 id。只在 Web 改（3b）。
type AgentTier struct {
	ByAgent map[string]AgentAllowance `json:"by_agent"`
}

// Allowance 返回 agent 的档位；没有归属的调用者与没设过的 Agent 都是只整理。
func (p Policy) Allowance(agent string) AgentAllowance {
	if a, ok := p.AgentTier.ByAgent[agent]; ok && agent != "" {
		return a
	}
	return AgentAllowance{Tier: AllowOrganize}
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
// 投递推迟判定 threshold_judge 与跟进推迟判断 followup_defer 已取消（2026-11-23，§7.4）：旧版本策略文档里的这两个键读入后忽略，下次保存时不再写出。
type Disturb struct {
	Routes           map[string]string `json:"routes"`
	Quiet            *Quiet            `json:"quiet,omitempty"`
	QuietExempt      bool              `json:"quiet_exempt,omitempty"`
	MergeWindowSec   int               `json:"merge_window_sec"`
	RemindOnDeadline bool              `json:"remind_on_deadline"`
	// DailyCap 是每个自然日经渠道推到本人面前的提醒上限，0 不限（§7.4）；DefaultDailyCap 是缺省，旧版本策略文档没有此键时也取它。
	DailyCap int `json:"daily_cap"`
	// Mutes 按来源、项目或事项静音，或范围 all 暂停所有提醒；到期（Until）后自动恢复。只在 Web 改。
	Mutes []Mute `json:"mutes"`
	// Push 是"推送哪些事"的逐类开关（§7.4）：键为 PushCategories 之一；缺的类别取 PushDefaults。
	// 旧版本策略文档没有此键，读作全部取默认。
	Push map[string]PushRule `json:"push,omitempty"`
	// Proactive 是主动提醒（事项到点提醒、简报与复盘送到哪）；没有此键取 DefaultProactive。
	Proactive *Proactive `json:"proactive,omitempty"`
	// Delivery 是投递矩阵（delivery.go）：各类提醒送到哪的唯一权威。没有此键时按 Routes、Push 的 Route、
	// Proactive 的 TaskPush 与 InstancePush 换算；有了之后这几个旧键冻结，内核不再读它们。
	Delivery *Delivery `json:"delivery,omitempty"`
}

// PushRule 是一类推送：On 为假时不产出投递；Route 为空时用决策类的路由；Min 只用于 new_proposals（攒够几条才推）。
type PushRule struct {
	On    bool   `json:"on"`
	Route string `json:"route,omitempty"`
	Min   int    `json:"min,omitempty"`
}

// 推送类别（§7.4）。需要你决定或确认的沿用决策类的路由，不在这里。
const (
	PushSourceFailing = "source_failing"  // 来源连续失败、认证或配置失败、永久失败
	PushTemplateRun   = "template_failed" // 简报、周报、例行任务没出来
	PushRunFailed     = "run_failed"      // 其他 Agent 运行失败或超时
	PushNewProposals  = "new_proposals"   // 新建议攒够 Min 条
)

// PushCategories 是推送类别的固定顺序。
var PushCategories = []string{PushSourceFailing, PushTemplateRun, PushRunFailed, PushNewProposals}

// PushDefaults：要本人动手的默认开，纯信息的默认关（新建议默认关，阈值 10 条）。
var PushDefaults = map[string]PushRule{
	PushSourceFailing: {On: true}, PushTemplateRun: {On: true}, PushRunFailed: {On: true}, PushNewProposals: {On: false, Min: 10},
}

// PushRuleFor 返回一类推送的生效设置：策略里有就用它（Min 缺省时补默认），没有取默认。
func (d Disturb) PushRuleFor(cat string) PushRule {
	r, ok := d.Push[cat]
	if !ok {
		return PushDefaults[cat]
	}
	if r.Min <= 0 {
		r.Min = PushDefaults[cat].Min
	}
	return r
}

// DefaultDailyCap 是日上限缺省值（§7.4）。
const DefaultDailyCap = 20

// 执行跨来源合并的 Agent（MergeJudge 的取值）：same 与生成事项和建议相同（写作模型，整批一次调用）；off 关闭合并，只走规则。
const (
	MergeSame = "same"
	MergeOff  = "off"
)

// MergeSettings 是跨来源合并任务本身的设置（§9.3）。零值即默认：全部来源参与、纳入已有事项与被忽略记录、
// 攒够 100 条或最早一条等了 30 分钟成批、一批资料至多 DefaultMergeBudgetTokens、用内置提示词。用排除名单而不是勾选名单，以后新接的来源默认参与。
type MergeSettings struct {
	ExcludeSources []string `json:"exclude_sources"`
	SkipExisting   bool     `json:"skip_existing"`
	BatchSize      int      `json:"batch_size"`   // 0 取默认
	WaitMinutes    int      `json:"wait_minutes"` // 0 取默认
	// BudgetTokens 是一批打包给模型的资料上限（按 CharsPerToken 估算的 token）；没有这个键取默认，给了就必须在范围内（0 也拒绝）。
	BudgetTokens *int       `json:"budget_tokens,omitempty"`
	Prompt       string     `json:"prompt"` // 空串用内置提示词
	PromptAt     *time.Time `json:"prompt_at,omitempty"`
	// Similarity 已不参与判定（规则检索改为只按分数排序取前 Candidates 条，不设下限）；只为旧策略文档读取兼容而保留，原样往返。
	Similarity string `json:"similarity,omitempty"`
	// Candidates 是规则检索给每条待归类建议至多带几条已有事项（按分数从高到低）；0 取默认。
	Candidates int `json:"candidates,omitempty"`
	// BodyMaxBytes 是批内整合后正文（要点清单 + 来源原文）的总长上限（字节）；0 取默认。超限时要点清单整段保留，来源原文按成员均分余下的额度。
	BodyMaxBytes int `json:"body_max_bytes,omitempty"`
}

// Cands 是生效的每条候选条数。
func (m MergeSettings) Cands() int { return cmp.Or(m.Candidates, DefaultMergeCandidates) }

// BodyMax 是生效的整合正文总长上限（字节）。
func (m MergeSettings) BodyMax() int { return cmp.Or(m.BodyMaxBytes, DefaultMergeBodyBytes) }

// 合并批量参数的默认值与范围。
const (
	DefaultMergeBatch = 100
	MaxMergeBatch     = 100
	DefaultMergeWait  = 30
	MaxMergePrompt    = 8000
	// 每条待归类建议带的候选条数：缺省 20，可设 1–50。
	DefaultMergeCandidates = 20
	MaxMergeCandidates     = 50
	// 整合正文总长上限（字节）：缺省 32 KiB。单条建议正文至多 budget.ProposalBodyChars（4000）个字符，中文约 12 KB；
	// 线上建议正文 p95 约 4.9 KB、p99 约 7.4 KB，32 KiB 能让 3–4 个成员的原文在常见长度下原样附全。可设 4 KiB–256 KiB。
	DefaultMergeBodyBytes = 32 << 10
	MinMergeBodyBytes     = 4 << 10
	MaxMergeBodyBytes     = 256 << 10

	// CharsPerToken 是资料预算的 token 估算系数：每这么多个字符记 1 个 token（向上取整）。合并与写作两处预算共用它。
	// 模型由本人配置、分词各家不同，内核不带 tokenizer，按这个明写的系数估算（§9.3）。
	CharsPerToken = 1.5
	// DefaultMergeBudgetTokens 与改动前借用的写作预算 24000 字等价：24000 ÷ 1.5 = 16000。
	DefaultMergeBudgetTokens = 16000
	// MinBudgetTokens 装得下一条新建议带满 5 条候选（标题、摘要各按上限）。
	// MaxBudgetTokens 取 1M 上下文窗口的一半，给提示词与输出留余地；它不是技术上限，超出所配模型的上下文时那次调用会失败。
	// 合并与写作两处预算共用这一对范围，设置页与服务端必须一致。
	MinBudgetTokens = 2000
	MaxBudgetTokens = 500000
)

// MergePromptSections 是合并提示词必须有的段落；MergeActions 是提示词必须写到的结论词（输出结构只认这四个）。
var (
	MergePromptSections = []string{"## 任务", "## 四种结论", "## 判定依据", "## 不许做的事"}
	MergeActions        = []string{"attach", "merge", "duplicate", "keep"}
)

// Batch 与 Wait 是生效的批量参数（未设取默认）。
func (m MergeSettings) Batch() int { return cmp.Or(m.BatchSize, DefaultMergeBatch) }
func (m MergeSettings) Wait() time.Duration {
	return time.Duration(cmp.Or(m.WaitMinutes, DefaultMergeWait)) * time.Minute
}

// Budget 是生效的资料上限（估算 token，未设取默认）。
func (m MergeSettings) Budget() int {
	if m.BudgetTokens == nil {
		return DefaultMergeBudgetTokens
	}
	return *m.BudgetTokens
}

// TokensOfChars 按 CharsPerToken 把字符数估成 token 数，向上取整。
func TokensOfChars(chars int) int { return int(math.Ceil(float64(chars) / CharsPerToken)) }

// CheckBudgetTokens 校验一个资料预算（token）在 MinBudgetTokens 到 MaxBudgetTokens 之间；0、负数、过小、过大都拒绝并写明范围，不截到边界。
func CheckBudgetTokens(field string, n int) error {
	if n < MinBudgetTokens || n > MaxBudgetTokens {
		return invalid("%s %d: use %d to %d", field, n, MinBudgetTokens, MaxBudgetTokens)
	}
	return nil
}

// TokenChars 是 TokensOfChars 的反向：token 预算最多装下多少个字符（向下取整，保证估出的 token 数不超预算）。
func TokenChars(tokens int) int { return int(math.Floor(float64(tokens) * CharsPerToken)) }

// Includes 报告来源是否参与合并。没有来源的记录（本人或 Agent 直接建的）总是参与。
func (m MergeSettings) Includes(source string) bool {
	return source == "" || !slices.Contains(m.ExcludeSources, source)
}

// CheckMergePrompt 校验本人改过的合并提示词：四段齐全且各段有内容、四个结论词都写到、不超长。空串表示用内置的，合法。
func CheckMergePrompt(body string) error {
	if strings.TrimSpace(body) == "" {
		return nil
	}
	if n := len([]rune(body)); n > MaxMergePrompt {
		return invalid("merge prompt is %d characters; keep it within %d", n, MaxMergePrompt)
	}
	for i, h := range MergePromptSections {
		at := strings.Index(body, h)
		if at < 0 {
			return invalid("merge prompt is missing the section %q", h)
		}
		rest := body[at+len(h):]
		end := len(rest)
		if j := strings.Index(rest, "\n## "); j >= 0 {
			end = j
		}
		if strings.TrimSpace(rest[:end]) == "" {
			return invalid("merge prompt section %q is empty", MergePromptSections[i])
		}
	}
	for _, a := range MergeActions {
		if !strings.Contains(body, a) {
			return invalid("merge prompt never mentions the conclusion %q", a)
		}
	}
	return nil
}

// Mute 的 Scope 取值。
const (
	MuteSource  = "source"
	MuteProject = "project"
	MuteItem    = "item"
	// MuteAll 是"暂停所有提醒"（设置 › 提醒与推送的总开关）：ID 为空；问题照样进等你决定，健康类（来源连续失败、Agent 运行失败等）照常送达。
	MuteAll = "all"
)

// Mute 是一条静音：ID 为来源、项目或事项 id（范围 all 时为空）；Until 为空表示不到期。
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

// PermissionCap 是运行权限上限：按 harness 设置，未设置的用 Default。
type PermissionCap struct {
	ByAgent   map[string]string `json:"by_agent,omitempty"`
	Default   string            `json:"default"`
	ByHarness map[string]string `json:"by_harness"`
}

// MainSessionLaunch 是以 Agent 本人身份做事时怎么拉起（§13.6）：按 Agent（harness）与按规则（规则键）各一份，
// 值是 headless（单独开一个后台会话）或 wake（在和本人的对话里接着做）；没有的键表示没选，按下一级取。只在 Web 改。
// 旧版本策略文档没有这个段，读作两份空表。
type MainSessionLaunch struct {
	ByAgent   map[string]string `json:"by_agent,omitempty"`
	ByHarness map[string]string `json:"by_harness"`
	ByRule    map[string]string `json:"by_rule"`
}

// 拉起方式的两个取值，与 domain.Run.Launch 一致。
const (
	LaunchHeadless = "headless"
	LaunchWake     = "wake"
)

// 端点位置与判断协议（§6.13、§8.8）。
const (
	LocalityLocal    = "local"
	LocalityExternal = "external"

	ProtocolProbability  = "probability-judgment/1"
	ProtocolChatLogprobs = "chat-logprobs"
	// ProtocolSystemOne 是原生分类模型服务的判断接口：一次请求带共享 state 与一组具名是非题（§8.8）。
	ProtocolSystemOne = "system-one"
)

// 生成端点的协议（§8.1）：按接口形态命名。空串等同 ProtocolChatCompletions（旧版本策略文档没有此键）。
const (
	ProtocolChatCompletions = "chat-completions"
	ProtocolMessages        = "messages"
	ProtocolResponses       = "responses"
	ProtocolGenerateContent = "generate-content"
)

// GenerateProtocol 是生成端点协议的实际取值：未写按 chat-completions。
func GenerateProtocol(p string) string {
	if p == "" {
		return ProtocolChatCompletions
	}
	return p
}

// 判断端点的默认值（附录 B.2、§8.8）。
const (
	DefaultJudgeTimeoutMs        = 10000
	DefaultJudgeMaxBatch         = 32
	DefaultJudgeMaxMaterialChars = 8000
	// DefaultGenerateMaxMaterialChars 是改动前写作预算的缺省字数（§8.4），只用来换算与锁等价：缺省 token 数 = 它 ÷ CharsPerToken。
	DefaultGenerateMaxMaterialChars = 24000
	// DefaultGenerateBudgetTokens 是写作资料预算的缺省值，与旧缺省逐字等价（24000 ÷ 1.5）。
	DefaultGenerateBudgetTokens = 16000
)

// EffectiveLocality 是端点位置的实际取值：未声明按 external（§14.14）。
func EffectiveLocality(l string) string {
	if l == LocalityLocal {
		return LocalityLocal
	}
	return LocalityExternal
}

// GenerateEndpoint 是生成端点；空 URL 表示未配置。APIKey 只存账本，不进 Event。
// Locality 由本人声明端点是否在自己可控的主机上，未声明按 external（§14.14）。Protocol 见 GenerateProtocol。
type GenerateEndpoint struct {
	URL      string `json:"url,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	Model    string `json:"model,omitempty"`
	Locality string `json:"locality,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	// MaxMaterialTokens 是每次调用的资料预算（按 CharsPerToken 估算的 token）；已存的策略没有这个键，读作默认值。
	MaxMaterialTokens int `json:"max_material_tokens,omitempty"`
	// MaxMaterialChars 是旧版按字数存的预算，只读：读取时一次换算进 MaxMaterialTokens 并清空，之后存回的文档不再有它。
	MaxMaterialChars int `json:"max_material_chars,omitempty"`
}

// MaterialBudget 是资料预算的实际取值（token）：未设置按默认值。
func (g GenerateEndpoint) MaterialBudget() int {
	if g.MaxMaterialTokens <= 0 {
		return DefaultGenerateBudgetTokens
	}
	return g.MaxMaterialTokens
}

// MaterialChars 是资料预算折成的字符数，交给按字符裁剪的 budget.Fit。
func (g GenerateEndpoint) MaterialChars() int { return TokenChars(g.MaterialBudget()) }

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
	URL    string `json:"url"`
	Model  string `json:"model"`
	APIKey string `json:"api_key"`
}

// Channel 是渠道的登记属性（§10.3）。TextOnly：渠道只能发文字，决策投递只带一条需要登录的链接。
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
	// 旧版本文档里的 web_only、material_write、screening_timeout_ms 段已随旧权限模型取消，解码时忽略（§14.3）。
	Disturb          Disturb          `json:"disturb"`
	PermissionCap    PermissionCap    `json:"permission_cap"`
	AgentTier        AgentTier        `json:"agent_tier"`
	JudgeEndpoint    *JudgeEndpoint   `json:"judge_endpoint"`
	GenerateEndpoint GenerateEndpoint `json:"generate_endpoint"`
	Persona          string           `json:"persona"` // 只影响表达方式，不进入任何权限判定
	// PersonaLang 是助手说哪种语言：zh、en，空串 = 跟随系统（调用方的区域设置）。旧版本策略文档没有此键，读作空串。
	PersonaLang      string              `json:"persona_lang"`
	ForwardAllowlist map[string][]string `json:"forward_allowlist"`
	ChannelAnswerers map[string][]string `json:"channel_answerers"`
	Channels         map[string]Channel  `json:"channels"`
	// DefaultHarness 是规则未指定 harness 时任务型与后台运行用的 harness（§13.6）；为空表示未设置，任务型转 needs_input。
	DefaultHarness    string            `json:"default_harness"`
	DefaultAgentID    *string           `json:"default_agent_id,omitempty"`
	MainAgentID       *string           `json:"main_agent_id,omitempty"`
	RuleAgent         map[string]string `json:"rule_agent,omitempty"`
	MainSessionLaunch MainSessionLaunch `json:"main_session_launch"`
	Judgment          Judgment          `json:"judgment"`
	// ModelsPaused 为真时两个端点的配置与密钥原样保留，但内核按两者都未配置运行（Current 给出的就是这个视图，§8.7）。
	// 没有任何端点时规整为假。只在 Web 改。
	ModelsPaused bool `json:"models_paused"`
	// MergeJudge 是执行跨来源合并的 Agent（§9.3）：same（缺省，与生成事项和建议相同：写作模型，整批一次调用）、
	// off（关闭，只按来源幂等键与合并键去重）。旧文档没有此键读作 same。
	MergeJudge string `json:"merge_judge,omitempty"`
	// Merge 是合并任务本身的设置；旧文档没有此键读作全部默认。
	Merge MergeSettings `json:"merge"`
	// NoteVaults 是笔记库登记（§6.13、§12.8）：名字在实例内唯一，OpenURL 是"在笔记应用打开"的 URL 模板。
	// 已存的策略版本没有这个键，读作没有登记，不改写。只在 Web 改。
	NoteVaults []NoteVault `json:"note_vaults"`
}

// NoteVault 是一个已登记的笔记库。OpenURL 的占位符 {vault}（库名）与 {path}（库内相对路径）渲染时各自做 URL 编码；
// 为空表示只登记名字、没有打开方式。内核只做字符串替换，不认识任何笔记应用。
type NoteVault struct {
	Name    string `json:"name"`
	OpenURL string `json:"open_url"`
}

// 打开方式模板不接受的协议：脚本类与文件类（§12.8、风险 43）。
var forbiddenOpenSchemes = []string{"javascript", "data", "vbscript", "file"}

var schemePrefix = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9+.-]*):`)

func normalizeNoteVaults(vs []NoteVault) ([]NoteVault, error) {
	out := []NoteVault{}
	seen := map[string]bool{}
	for i, v := range vs {
		name := norm.NFC.String(strings.TrimSpace(v.Name))
		if name == "" || strings.ContainsAny(name, "/\\") || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return nil, invalid("note_vaults %d: name is empty or contains a slash or control character", i)
		}
		if seen[name] {
			return nil, invalid("note_vaults %d: name %q is listed twice", i, name)
		}
		seen[name] = true
		tpl := v.OpenURL
		if tpl != "" {
			m := schemePrefix.FindStringSubmatch(tpl)
			if m == nil || strings.IndexFunc(tpl, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
				return nil, invalid("note_vaults %d (%s): open_url must start with a protocol and contain no whitespace", i, name)
			}
			if oneOf(strings.ToLower(m[1]), forbiddenOpenSchemes...) {
				return nil, invalid("note_vaults %d (%s): protocol %q is not allowed in open_url", i, name, m[1])
			}
		}
		out = append(out, NoteVault{Name: name, OpenURL: tpl})
	}
	return out, nil
}

// OpenURL 按库名与库内相对路径渲染"在笔记应用打开"的地址：{vault}、{path} 各自按 URI 组件编码（§12.8），
// 占位符因此既能放在路径里也能放在查询参数里。库未登记或没有打开方式时返回空串。
func (p Policy) OpenURL(vault, path string) string {
	for _, v := range p.NoteVaults {
		if v.Name == vault && v.OpenURL != "" {
			return strings.NewReplacer("{vault}", uriComponent(vault), "{path}", uriComponent(path)).Replace(v.OpenURL)
		}
	}
	return ""
}

// uriComponent 与 Web 的 encodeURIComponent 同一规则：字母、数字与 -_.!~*'() 原样，其余字节按 %XX 编码
// （& = + 空格 / % 都编码）。两端用同一组用例守住（settings-vaults/open-url-cases.json）。
func uriComponent(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

// Default 是版本 0 的配置：默认类别、权限上限 read、人格与白名单为空。
func Default() Policy {
	return Policy{
		Disturb: Disturb{Routes: map[string]string{
			string(domain.DeliveryDecision): "inject", string(domain.DeliveryBrief): "inject",
			string(domain.DeliveryReminder): "inject", string(domain.DeliveryHealth): "web",
			string(domain.DeliveryWake): "web",
		}, MergeWindowSec: 30 * 60, RemindOnDeadline: true, DailyCap: DefaultDailyCap, Mutes: []Mute{}}, // 合并窗口默认 30 分钟（§7.4）
		PermissionCap:     PermissionCap{Default: PermRead, ByHarness: map[string]string{}},
		AgentTier:         AgentTier{ByAgent: map[string]AgentAllowance{}},
		MainSessionLaunch: MainSessionLaunch{ByHarness: map[string]string{}, ByRule: map[string]string{}},
		ForwardAllowlist:  map[string][]string{},
		ChannelAnswerers:  map[string][]string{},
		Channels:          map[string]Channel{},
		NoteVaults:        []NoteVault{},
		Judgment:          Judgment{Bands: []JudgmentBand{}, Shadow: []string{}, SpotCheck: SpotCheck{PerBrief: DefaultSpotCheckPerBrief}},
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
}, {
	// 导入专用单已写入的累计条数（§6.13、§14.5）：和撤销状态一样是 doc 之外会变的列，只增不减。账本结构版本 8 的一部分。
	Version: 3, Ledger: 8, Name: "ledger-v8-grant-import-used",
	SQL: `ALTER TABLE policy_grant ADD COLUMN used_records INTEGER NOT NULL DEFAULT 0;`,
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
	if !oneOf(p.MergeJudge, "", MergeSame, MergeOff) {
		return p, invalid("merge_judge %q: use same or off", p.MergeJudge)
	}
	if m := p.Merge; m.BatchSize != 0 && (m.BatchSize < 2 || m.BatchSize > MaxMergeBatch) {
		return p, invalid("merge batch_size %d: use 2 to %d", m.BatchSize, MaxMergeBatch)
	}
	if m := p.Merge; m.WaitMinutes != 0 && (m.WaitMinutes < 5 || m.WaitMinutes > 1440) {
		return p, invalid("merge wait_minutes %d: use 5 to 1440", m.WaitMinutes)
	}
	if b := p.Merge.BudgetTokens; b != nil {
		if err := CheckBudgetTokens("merge budget_tokens", *b); err != nil {
			return p, err
		}
	}
	if err := CheckMergePrompt(p.Merge.Prompt); err != nil {
		return p, err
	}
	if c := p.Merge.Candidates; c < 0 || c > MaxMergeCandidates {
		return p, invalid("merge candidates %d: use 1 to %d, or 0 for the default", c, MaxMergeCandidates)
	}
	if b := p.Merge.BodyMaxBytes; b != 0 && (b < MinMergeBodyBytes || b > MaxMergeBodyBytes) {
		return p, invalid("merge body_max_bytes %d: use %d to %d, or 0 for the default", b, MinMergeBodyBytes, MaxMergeBodyBytes)
	}
	for k, r := range p.Disturb.Push {
		if !slices.Contains(PushCategories, k) {
			return p, invalid("disturb push %q: unknown category", k)
		}
		if r.Min < 0 {
			return p, invalid("disturb push %q: min must not be negative", k)
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

	if m := p.Disturb.Delivery; m != nil {
		c := *m
		c.Rows = maps.Clone(m.Rows)
		if err := c.check(); err != nil {
			return p, err
		}
		p.Disturb.Delivery = &c
	}
	if p.Disturb.DailyCap < 0 {
		return p, invalid("disturb daily_cap %d < 0", p.Disturb.DailyCap)
	}
	if pr := p.Disturb.Proactive; pr != nil {
		c, err := CheckProactive(*pr) // 规整不改调用方传进来的切片
		if err != nil {
			return p, err
		}
		p.Disturb.Proactive = &c
	}
	if p.Disturb.Mutes == nil {
		p.Disturb.Mutes = []Mute{}
	}
	for i, m := range p.Disturb.Mutes {
		if m.Scope == MuteAll {
			if m.ID != "" {
				return p, invalid("disturb mute %d: scope all takes no id", i)
			}
			continue
		}
		if !oneOf(m.Scope, MuteSource, MuteProject, MuteItem) || strings.TrimSpace(m.ID) == "" {
			return p, invalid("disturb mute %d needs scope source|project|item and a non-empty id, or scope all without id", i)
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

	if p.AgentTier.ByAgent == nil {
		p.AgentTier.ByAgent = map[string]AgentAllowance{}
	}
	for a, v := range p.AgentTier.ByAgent {
		if a == "" || !oneOf(v.Tier, AllowOrganize, AllowDecide, AllowRules) {
			return p, invalid("agent_tier agent %q tier %q", a, v.Tier)
		}
		if v.Projects = dedupSorted(v.Projects); len(v.Projects) == 0 {
			v.Projects = nil
		}
		p.AgentTier.ByAgent[a] = v
	}

	if !oneOf(p.PermissionCap.Default, PermRead, PermEdit, PermFull) {
		return p, invalid("permission_cap default %q", p.PermissionCap.Default)
	}
	for h, v := range p.PermissionCap.ByAgent {
		if h == "" || !oneOf(v, PermRead, PermEdit, PermFull) {
			return p, invalid("permission_cap agent %q level %q", h, v)
		}
	}
	for h, v := range p.PermissionCap.ByHarness {
		if h == "" || !oneOf(v, PermRead, PermEdit, PermFull) {
			return p, invalid("permission_cap harness %q level %q", h, v)
		}
	}
	if p.PermissionCap.ByHarness == nil {
		p.PermissionCap.ByHarness = map[string]string{}
	}
	for field, m := range map[string]*map[string]string{"by_harness": &p.MainSessionLaunch.ByHarness, "by_rule": &p.MainSessionLaunch.ByRule, "by_agent": &p.MainSessionLaunch.ByAgent} {
		if *m == nil {
			*m = map[string]string{}
		}
		for k, v := range *m {
			if k == "" || !oneOf(v, LaunchHeadless, LaunchWake) {
				return p, invalid("main_session_launch %s %q launch %q", field, k, v)
			}
		}
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
	if !oneOf(p.PersonaLang, "", "zh", "en") {
		return p, invalid("persona_lang %q", p.PersonaLang)
	}
	var err error
	if p.NoteVaults, err = normalizeNoteVaults(p.NoteVaults); err != nil {
		return p, err
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
		if !oneOf(g.Protocol, "", ProtocolChatCompletions, ProtocolMessages, ProtocolResponses, ProtocolGenerateContent) {
			return invalid("generate_endpoint protocol %q", g.Protocol)
		}
		if g.MaxMaterialTokens == 0 && g.MaxMaterialChars > 0 { // 旧文档：换算一次；有了 token 值就以它为准，所以重复读不会二次换算
			// 旧值不受新范围约束，读取时落进范围，免得整份策略读不出来。
			g.MaxMaterialTokens = min(max(int(float64(g.MaxMaterialChars)/CharsPerToken), MinBudgetTokens), MaxBudgetTokens)
		}
		g.MaxMaterialChars = 0
		if g.MaxMaterialTokens == 0 {
			g.MaxMaterialTokens = DefaultGenerateBudgetTokens
		}
		if err = CheckBudgetTokens("generate_endpoint max_material_tokens", g.MaxMaterialTokens); err != nil {
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

// SwitchObject 是权限模型切换事件（authz.switched）的对象。
const SwitchObject = "authz"

// Switched 报告账本是否已切换到权限模型第二版（§16.5）：有一条 authz.switched 即已切换。
func Switched(tx *store.Tx) (bool, error) {
	es, err := eventlog.Query(tx, eventlog.Filter{Object: SwitchObject, Type: domain.EvAuthzSwitched}, 0, 1)
	return len(es) > 0, err
}

// legacyKeys 是旧权限模型留在策略文档里的段：高风险类别、初筛超时、按材料类别直接写（2026-11-20 取消，§14.3）。
var legacyKeys = []string{"material_write", "screening_timeout_ms", "web_only"}

// LegacyKeys 返回最新一版存储文档里还带着的旧权限模型段；空表示已切换到新文档。
func LegacyKeys(tx *store.Tx) ([]string, error) {
	m, err := StoredRaw(tx, legacyKeys...)
	var out []string
	for _, k := range legacyKeys {
		if _, ok := m[k]; ok {
			out = append(out, k)
		}
	}
	return out, err
}

// StoredRaw 返回最新一版存储文档里 keys 这些顶层段的原文（含已不在结构体里的旧段）；没有的段不在结果里。
// 调用方自己负责不把端点密钥之类的段带出去：只取需要的段。
func StoredRaw(tx *store.Tx, keys ...string) (map[string]json.RawMessage, error) {
	out := map[string]json.RawMessage{}
	r, err := tx.Query("SELECT doc FROM policy_version ORDER BY version DESC LIMIT 1")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if !r.Next() {
		return out, r.Err()
	}
	var doc string
	if err := r.Scan(&doc); err != nil {
		return nil, err
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(doc), &m); err != nil {
		return nil, err
	}
	for _, k := range keys {
		if v, ok := m[k]; ok {
			out[k] = v
		}
	}
	return out, nil
}

// RulesSections 是策略里属于 3a 改日常的顶层段（§14.3）：Agent 档位放开到 3a 可改，规矩改动清单列出并可撤回。
// 其余段都是 3b。设置路由的段表与它一致（api/settings/policy）。
var RulesSections = []string{"channels", "disturb", "forward_allowlist", "judgment", "models_paused", "persona", "persona_lang"}

// At 读第 v 版策略（存储原文，不按模型暂停改写）；没有这一版返回 not_found。
func At(tx *store.Tx, v int) (Policy, error) {
	var doc string
	found := false
	r, err := tx.Query("SELECT doc FROM policy_version WHERE version = ?", int64(v))
	if err != nil {
		return Policy{}, err
	}
	defer r.Close()
	if r.Next() {
		found = true
		if err := r.Scan(&doc); err != nil {
			return Policy{}, err
		}
	}
	if err := r.Err(); err != nil {
		return Policy{}, err
	}
	if !found {
		if v == 0 {
			return Default(), nil
		}
		return Policy{}, domain.NewError(domain.CodeNotFound, fmt.Sprintf("policy version %d does not exist", v), "list policy changes and retry")
	}
	p, _, err := decode(doc)
	return p, err
}

// Sections 把配置拆成顶层段（段名 → JSON），供比较前后值与撤回。
func Sections(p Policy) (map[string]json.RawMessage, error) { return sections(p) }

// Restore 把 p 的 keys 段换成 from 的值（撤回一次段改动），其余段不动。
func Restore(p *Policy, from Policy, keys []string) error {
	cur, err := sections(*p)
	if err != nil {
		return err
	}
	src, err := sections(from)
	if err != nil {
		return err
	}
	for _, k := range keys {
		cur[k] = src[k]
	}
	b, err := json.Marshal(cur)
	if err != nil {
		return err
	}
	var out Policy
	if err := json.Unmarshal(b, &out); err != nil {
		return err
	}
	*p = out
	return nil
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

// Update 是策略的唯一写入口；谁能改哪一段由调用方先经 authz.Decide 判定，actor 只用于留痕。
// mutate 在当前配置的副本上修改；结果经校验后若与当前不同，就追加新版本并在同一事务记
// policy.changed（改了白名单时另记 allowlist.changed）。无变化时不产生版本，返回当前版本号。
func Update(tx *store.Tx, actor domain.Actor, reason string, mutate func(*Policy) error) (int, error) {
	return UpdateWith(tx, actor, nil, reason, mutate)
}

// UpdateWith 同 Update，并在 policy.changed 上记依据 basis（档位、所属 Agent），规矩改动清单据此认出 Agent 改的段（§14.3）。
func UpdateWith(tx *store.Tx, actor domain.Actor, basis *domain.Basis, reason string, mutate func(*Policy) error) (int, error) {
	cur, old, legacy, err := latest(tx) // 判权在调用方：设置路由与命令管线都先过 authz.Decide（§14.3）
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
	dropped, err := LegacyKeys(tx) // 旧权限模型的段：下一次写入就不再带它们（旧版本留在历史里）
	if err != nil {
		return 0, err
	}
	changed = append(changed, dropped...)
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
	ev := domain.Event{Type: domain.EvPolicyChanged, Object: Object, Actor: actor, Basis: basis, Reason: reason, Payload: payload, At: at}
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
