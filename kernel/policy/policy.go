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
// ThresholdJudge 为真时，决策投递在按优先级定路由之外再经判断型做投递阈值判定（只能降低打扰），默认关闭（§7.4）。
type Disturb struct {
	Routes           map[string]string `json:"routes"`
	Quiet            *Quiet            `json:"quiet,omitempty"`
	QuietExempt      bool              `json:"quiet_exempt,omitempty"`
	MergeWindowSec   int               `json:"merge_window_sec"`
	RemindOnDeadline bool              `json:"remind_on_deadline"`
	ThresholdJudge   bool              `json:"threshold_judge,omitempty"`
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

// ModelEndpoint 为空 URL 表示未配置。APIKey 只存账本，不进 Event。
type ModelEndpoint struct {
	URL                string `json:"url,omitempty"`
	Model              string `json:"model,omitempty"`
	APIKey             string `json:"api_key,omitempty"`
	ScreeningTimeoutMs int    `json:"screening_timeout_ms"`
}

// Channel 是渠道的登记属性（§10.3）。TextOnly：渠道只能发文字，决策投递带一次性表单链接。
type Channel struct {
	TextOnly bool `json:"text_only"`
}

// Policy 是一个版本的完整配置。两份白名单相互独立：键是来源或渠道，值是发送者或回答人。
type Policy struct {
	WebOnly          WebOnly             `json:"web_only"`
	Disturb          Disturb             `json:"disturb"`
	MaterialWrite    MaterialWrite       `json:"material_write"`
	PermissionCap    PermissionCap       `json:"permission_cap"`
	ModelEndpoint    ModelEndpoint       `json:"model_endpoint"`
	Persona          string              `json:"persona"` // 只影响表达方式，不进入任何权限判定
	ForwardAllowlist map[string][]string `json:"forward_allowlist"`
	ChannelAnswerers map[string][]string `json:"channel_answerers"`
	Channels         map[string]Channel  `json:"channels"`
	// DefaultHarness 是规则未指定 harness 时任务型与后台运行用的 harness（§13.6）；为空表示未设置，任务型转 needs_input。
	DefaultHarness string `json:"default_harness"`
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
		}, MergeWindowSec: 30 * 60, RemindOnDeadline: true}, // 合并窗口默认 30 分钟（§7.4）
		// 规范、方案类须本人确认；纪要类直写（§12.4）。未登记的 kind 取更严的 proposed。
		MaterialWrite: MaterialWrite{Default: WriteProposed, Kinds: map[string]string{
			"minutes": WriteDirect, "reference": WriteDirect, "worknote": WriteDirect,
			"requirement": WriteProposed, "design": WriteProposed, "guideline": WriteProposed,
		}},
		PermissionCap:    PermissionCap{Default: PermRead, ByHarness: map[string]string{}},
		ModelEndpoint:    ModelEndpoint{ScreeningTimeoutMs: 30000},
		ForwardAllowlist: map[string][]string{},
		ChannelAnswerers: map[string][]string{},
		Channels:         map[string]Channel{},
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

	if p.ModelEndpoint.ScreeningTimeoutMs <= 0 {
		return p, invalid("model_endpoint screening_timeout_ms %d <= 0", p.ModelEndpoint.ScreeningTimeoutMs)
	}
	if p.ModelEndpoint.URL != "" {
		u, err := url.Parse(p.ModelEndpoint.URL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return p, invalid("model_endpoint url %q is not an http(s) URL", p.ModelEndpoint.URL)
		}
	}

	if n := EstimateTokens(p.Persona); n > MaxPersonaTokens {
		return p, invalid("persona is about %d tokens, limit %d", n, MaxPersonaTokens)
	}
	p.ForwardAllowlist = normList(p.ForwardAllowlist)
	p.ChannelAnswerers = normList(p.ChannelAnswerers)
	if p.Channels == nil {
		p.Channels = map[string]Channel{}
	}
	return p, nil
}

// clone 经 JSON 深拷贝，使变更函数不会污染调用方持有的快照。
func clone(p Policy) Policy {
	b, _ := json.Marshal(p)
	var out Policy
	_ = json.Unmarshal(b, &out)
	return out
}

func decode(doc string) (Policy, error) {
	var p Policy
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		return Policy{}, fmt.Errorf("policy: stored document: %w", err)
	}
	return normalize(p)
}

// Current 返回最新版本号与配置；尚无版本时为 (0, Default())。
func Current(tx *store.Tx) (int, Policy, error) {
	r, err := tx.Query("SELECT version, doc FROM policy_version ORDER BY version DESC LIMIT 1")
	if err != nil {
		return 0, Policy{}, err
	}
	defer r.Close()
	if !r.Next() {
		return 0, Default(), r.Err()
	}
	var (
		v   int64
		doc string
	)
	if err := r.Scan(&v, &doc); err != nil {
		return 0, Policy{}, err
	}
	p, err := decode(doc)
	return int(v), p, err
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
	cur, old, err := Current(tx)
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
	if shown.ModelEndpoint.APIKey != "" {
		shown.ModelEndpoint.APIKey = "***"
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
