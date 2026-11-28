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

// 运行权限档位（决策表第 5 项）与材料写入策略（§12.4）。
const (
	PermRead = "read"
	PermEdit = "edit"
	PermFull = "full"

	WriteDirect   = "direct"
	WriteProposed = "proposed"
)

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

// SourcePipeline 是来源处理线的设置（§9.3，设置 › 来源处理）。零值即默认：
// 攒够 10 条或最早一条等了 30 分钟触发一批、一批最多 20 条、紧急立即处理默认关、失败最多重试 2 次、每日不限批数、两份提示词留空用内置默认。
type SourcePipeline struct {
	// Executor 是跑批处理的执行者（执行者 id）；为空时取分工里的日常执行者（default_executor）。
	Executor string `json:"executor,omitempty"`
	// Agent 是旧的处理 Agent（Agent 卡 id）：已取消（2026-11-28），只读，只在执行者统一切换时换算成 Executor 后清空（§16.5）。
	Agent           string `json:"agent,omitempty"`
	BatchSize       int    `json:"batch_size,omitempty"`
	WaitMinutes     int    `json:"wait_minutes,omitempty"`
	UrgentImmediate bool   `json:"urgent_immediate,omitempty"`
	MaxBatch        int    `json:"max_batch,omitempty"`
	MaxRetries      *int   `json:"max_retries,omitempty"`
	DailyBatchLimit int    `json:"daily_batch_limit,omitempty"`
	JudgePrompt     string `json:"judge_prompt,omitempty"`
	OwnerIdentity   string `json:"owner_identity,omitempty"`
	BatchPrompt     string `json:"batch_prompt,omitempty"`
}

// 来源处理线参数的默认值与范围。
const (
	DefaultPipelineBatch      = 10
	MaxPipelineBatch          = 100
	DefaultPipelineWait       = 30
	MaxPipelineWait           = 1440
	DefaultPipelineMaxBatch   = 20
	DefaultPipelineMaxRetries = 2
	MaxPipelineRetries        = 10
	MaxPipelineDailyBatches   = 1000
	MaxPipelinePrompt         = 12000
)

func (s SourcePipeline) Batch() int { return cmp.Or(s.BatchSize, DefaultPipelineBatch) }
func (s SourcePipeline) Wait() time.Duration {
	return time.Duration(cmp.Or(s.WaitMinutes, DefaultPipelineWait)) * time.Minute
}
func (s SourcePipeline) MaxBatchSize() int { return cmp.Or(s.MaxBatch, DefaultPipelineMaxBatch) }
func (s SourcePipeline) Retries() int {
	if s.MaxRetries == nil {
		return DefaultPipelineMaxRetries
	}
	return *s.MaxRetries
}

const (
	// CharsPerToken 是资料预算的 token 估算系数：每这么多个字符记 1 个 token（向上取整）。写作预算使用它。
	// 模型由本人配置、分词各家不同，内核不带 tokenizer，按这个明写的系数估算（§8.4）。
	CharsPerToken = 1.5
	// MinBudgetTokens 与 MaxBudgetTokens 是写作资料预算的范围，设置页与服务端必须一致。
	MinBudgetTokens = 2000
	MaxBudgetTokens = 500000
)

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

// MainSessionLaunch 是以 Agent 本人身份做事时怎么拉起（§13.6）：按 Agent（声明）与按规则（规则键）各一份，
// 值是 headless（单独开一个后台会话）或 wake（在和本人的对话里接着做）；没有的键表示没选，按下一级取。只在 Web 改。
// 旧版本策略文档没有这个段，读作两份空表。
type MainSessionLaunch struct {
	// ByAgent 是旧的按 Agent 卡一份：已取消（2026-11-28），只读，执行者统一切换时并入 ByHarness（卡已成为同 id 的声明）后清空。
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
	// 后台运行时限（§13.6、§6.13）：所有后台运行（推送主会话的 wake/headless、派给执行者的 task 与续接）共用同一个值，
	// 唯一语义是防卡死、防无限运行，不影响事项本身。默认 4 小时，设置页按小时编辑（0.25–24 小时）。
	DefaultBackgroundRunTimeoutSec = 4 * 60 * 60
	MinBackgroundRunTimeoutSec     = 15 * 60
	MaxBackgroundRunTimeoutSec     = 24 * 60 * 60
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
	// 旧版本文档里的 web_only、material_write、screening_timeout_ms 段已随旧权限模型取消，解码时忽略（§14.3）；
	// agent_tier 与 permission_cap 两段随权限卡取消，只在切换时按原文换算（§16.5）。
	Disturb Disturb `json:"disturb"`
	// AgentCards 是各 Agent 的权限卡（§14.5），键是声明（harness；隔离另写的声明是原 Agent 卡 id）。只有本人能改。
	// 执行者统一切换前的旧键 legacy:<程序> 由切换改成 <程序>，读取时也按 AgentKey 认。
	AgentCards map[string]Card `json:"agent_cards"`
	// Limits 是同时运行数与连锁层数（§14.9）；旧文档没有此键，读作全部取运行配置的初值。
	Limits           Limits              `json:"limits"`
	JudgeEndpoint    *JudgeEndpoint      `json:"judge_endpoint"`
	GenerateEndpoint GenerateEndpoint    `json:"generate_endpoint"`
	ForwardAllowlist map[string][]string `json:"forward_allowlist"`
	ChannelAnswerers map[string][]string `json:"channel_answerers"`
	Channels         map[string]Channel  `json:"channels"`
	// DefaultExecutor 与 MainExecutor 是分工（§13.6）：没指定执行者的后台运行用日常执行者，以本人身份做事（main_session）用主执行者；
	// RuleExecutor 是某条规则固定用的执行者（规则键 → 执行者 id）。都是执行者 id，空表示没选。
	DefaultExecutor string            `json:"default_executor,omitempty"`
	MainExecutor    string            `json:"main_executor,omitempty"`
	RuleExecutor    map[string]string `json:"rule_executor,omitempty"`
	// DefaultHarness、DefaultAgentID、MainAgentID、RuleAgent 是按 Agent 卡选人的旧字段：已取消（2026-11-28），只读，
	// 只在执行者统一切换时换算成上面三项后清空（§16.5）；v0.2.0 之后第一个版本去掉读兼容。
	DefaultHarness    string            `json:"default_harness,omitempty"`
	DefaultAgentID    *string           `json:"default_agent_id,omitempty"`
	MainAgentID       *string           `json:"main_agent_id,omitempty"`
	RuleAgent         map[string]string `json:"rule_agent,omitempty"`
	MainSessionLaunch MainSessionLaunch `json:"main_session_launch"`
	Judgment          Judgment          `json:"judgment"`
	// ModelsPaused 为真时两个端点的配置与密钥原样保留，但内核按两者都未配置运行（Current 给出的就是这个视图，§8.7）。
	// 没有任何端点时规整为假。只在 Web 改。
	ModelsPaused bool `json:"models_paused"`
	// SourcePipeline 是来源处理线的设置（§9.3）；旧文档没有此键读作全部默认，旧键 merge_judge/merge 读取时忽略。
	SourcePipeline SourcePipeline `json:"source_pipeline"`
	// NoteVaults 是笔记库登记（§6.13、§12.8）：名字在实例内唯一，OpenURL 是"在笔记应用打开"的 URL 模板。
	// 已存的策略版本没有这个键，读作没有登记，不改写。只在 Web 改。
	NoteVaults []NoteVault `json:"note_vaults"`
	// BackgroundRunTimeoutSec 是所有后台运行的统一时限（秒）；旧策略版本没有此键，读作 DefaultBackgroundRunTimeoutSec（4 小时）。
	BackgroundRunTimeoutSec int `json:"background_run_timeout_sec,omitempty"`
	// BackgroundPersona 为真时后台运行带上 Agent 平时的人格、记忆与常驻说明；默认关，关着时拉起命令追加声明的
	// headless.no_persona 标志（§13.6）。旧策略版本没有此键，读作关。
	BackgroundPersona bool `json:"background_persona"`
	// Thinking 是各类模型调用与后台运行的思考强度（low、medium、high，或 off 表示不发这个参数）；旧策略版本没有此键，空读作各自的默认值（§8.1、§13.6）。
	Thinking Thinking `json:"thinking"`
}

// Thinking 按用途分三项：判断（要不要处理、急不急）、写作（一句话摘要与起草）、后台 Agent 运行。
type Thinking struct {
	Judge      string `json:"judge"`
	Write      string `json:"write"`
	Background string `json:"background"`
}

// 思考强度的默认值。
const (
	DefaultJudgeThinking      = "low"
	DefaultWriteThinking      = "medium"
	DefaultBackgroundThinking = "medium"
	ThinkingOff               = "off" // 不指定：不发该参数，端点不接受时的出口
)

// Effective 返回补全默认值后的三项。
func (t Thinking) Effective() Thinking {
	return Thinking{cmp.Or(t.Judge, DefaultJudgeThinking), cmp.Or(t.Write, DefaultWriteThinking), cmp.Or(t.Background, DefaultBackgroundThinking)}
}

// ModelEffort 是一次模型调用要发的思考强度：判断类取判断项，其余取写作项；不指定返回空。
func (t Thinking) ModelEffort(judging bool) string {
	e := t.Effective()
	v := e.Write
	if judging {
		v = e.Judge
	}
	if v == ThinkingOff {
		return ""
	}
	return v
}

// BackgroundRunTimeout 返回生效的后台运行时限；未设置时取 DefaultBackgroundRunTimeoutSec（4 小时）。
func (p Policy) BackgroundRunTimeout() time.Duration {
	return time.Duration(cmp.Or(p.BackgroundRunTimeoutSec, DefaultBackgroundRunTimeoutSec)) * time.Second
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

// Default 是版本 0 的配置：默认类别、没有权限卡（Agent 取默认卡）、人格与白名单为空。
func Default() Policy {
	return Policy{
		Disturb: Disturb{Routes: map[string]string{
			string(domain.DeliveryDecision): "inject", string(domain.DeliveryBrief): "inject",
			string(domain.DeliveryReminder): "inject", string(domain.DeliveryHealth): "web",
			string(domain.DeliveryWake): "web",
		}, MergeWindowSec: 30 * 60, RemindOnDeadline: true, DailyCap: DefaultDailyCap, Mutes: []Mute{}}, // 合并窗口默认 30 分钟（§7.4）
		AgentCards:              map[string]Card{},
		MainSessionLaunch:       MainSessionLaunch{ByHarness: map[string]string{}, ByRule: map[string]string{}},
		ForwardAllowlist:        map[string][]string{},
		ChannelAnswerers:        map[string][]string{},
		Channels:                map[string]Channel{},
		NoteVaults:              []NoteVault{},
		Judgment:                Judgment{Bands: []JudgmentBand{}, Shadow: []string{}, SpotCheck: SpotCheck{PerBrief: DefaultSpotCheckPerBrief}},
		BackgroundRunTimeoutSec: DefaultBackgroundRunTimeoutSec,
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
	if p.BackgroundRunTimeoutSec == 0 {
		p.BackgroundRunTimeoutSec = DefaultBackgroundRunTimeoutSec
	}
	if p.BackgroundRunTimeoutSec < MinBackgroundRunTimeoutSec || p.BackgroundRunTimeoutSec > MaxBackgroundRunTimeoutSec {
		return p, invalid("background_run_timeout_sec %d: use %d to %d (0.25h to 24h)", p.BackgroundRunTimeoutSec, MinBackgroundRunTimeoutSec, MaxBackgroundRunTimeoutSec)
	}
	for _, e := range []string{p.Thinking.Judge, p.Thinking.Write, p.Thinking.Background} {
		if !oneOf(e, "", ThinkingOff, "low", "medium", "high") {
			return p, invalid("thinking %q: use off, low, medium or high", e)
		}
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

	if err := normalizeCards(&p); err != nil {
		return p, err
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

	p.DefaultExecutor, p.MainExecutor = strings.TrimSpace(p.DefaultExecutor), strings.TrimSpace(p.MainExecutor)
	for k, v := range p.RuleExecutor {
		if k == "" || strings.TrimSpace(v) == "" {
			return p, invalid("rule_executor %q needs a rule key and an executor id", k)
		}
	}

	if err := normalizeEndpoints(&p); err != nil {
		return p, err
	}
	if p.GenerateEndpoint.URL == "" && p.JudgeEndpoint == nil {
		p.ModelsPaused = false
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
	if err := normalizeSourcePipeline(&p.SourcePipeline); err != nil {
		return p, err
	}
	return normalizeJudgment(p)
}

func normalizeSourcePipeline(sp *SourcePipeline) error {
	sp.Agent = strings.TrimSpace(sp.Agent)
	sp.Executor = strings.TrimSpace(sp.Executor)
	if sp.BatchSize < 0 || sp.BatchSize > MaxPipelineBatch {
		return invalid("source_pipeline batch_size %d: use 1 to %d", sp.BatchSize, MaxPipelineBatch)
	}
	if sp.WaitMinutes < 0 || sp.WaitMinutes > MaxPipelineWait {
		return invalid("source_pipeline wait_minutes %d: use 1 to %d", sp.WaitMinutes, MaxPipelineWait)
	}
	if sp.MaxBatch < 0 || sp.MaxBatch > MaxPipelineBatch {
		return invalid("source_pipeline max_batch %d: use 1 to %d", sp.MaxBatch, MaxPipelineBatch)
	}
	if sp.MaxRetries != nil && (*sp.MaxRetries < 0 || *sp.MaxRetries > MaxPipelineRetries) {
		return invalid("source_pipeline max_retries %d: use 0 to %d", *sp.MaxRetries, MaxPipelineRetries)
	}
	if sp.DailyBatchLimit < 0 || sp.DailyBatchLimit > MaxPipelineDailyBatches {
		return invalid("source_pipeline daily_batch_limit %d: use 0 to %d", sp.DailyBatchLimit, MaxPipelineDailyBatches)
	}
	if len([]rune(sp.JudgePrompt)) > MaxPipelinePrompt {
		return invalid("source_pipeline judge_prompt exceeds %d characters", MaxPipelinePrompt)
	}
	if len([]rune(sp.OwnerIdentity)) > MaxPipelinePrompt {
		return invalid("source_pipeline owner_identity exceeds %d characters", MaxPipelinePrompt)
	}
	if len([]rune(sp.BatchPrompt)) > MaxPipelinePrompt {
		return invalid("source_pipeline batch_prompt exceeds %d characters", MaxPipelinePrompt)
	}
	return nil
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
	}{Policy: Policy{Disturb: Disturb{DailyCap: DefaultDailyCap}, Judgment: Judgment{SpotCheck: SpotCheck{PerBrief: DefaultSpotCheckPerBrief}}, BackgroundRunTimeoutSec: DefaultBackgroundRunTimeoutSec}} // 旧文档没有这些键：升级后默认值即生效
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

// legacyKeys 是旧权限模型留在策略文档里的段：高风险类别、初筛超时、按材料类别直接写（2026-11-20 取消）；
// 档位与工具上限（2026-11-26 由权限卡取代，§14.3、§16.5）。
var legacyKeys = []string{"agent_tier", "material_write", "permission_cap", "screening_timeout_ms", "web_only"}

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

// RulesSections 是策略里属于"改日常规则与提醒"的顶层段（§14.3）：权限卡开放这一组的 Agent 可改，规矩改动清单列出并可撤回。
// 其余段属别的组或只有本人。设置路由的段表与它一致（api/settings/policy）。
var RulesSections = []string{"background_run_timeout_sec", "channels", "disturb", "forward_allowlist", "judgment", "limits", "models_paused"}

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
	for k := range oldSec { // 带 omitempty 的段清空后不再出现在新文档里，也是改动
		if _, ok := newSec[k]; !ok {
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
