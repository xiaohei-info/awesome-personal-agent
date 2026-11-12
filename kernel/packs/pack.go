// Package packs 是能力包加载器（solution-design §13、附录 C）：读取内置包与数据目录包，按 C.11 校验，
// 注册 kind 的 schema。能力包只是数据，加载器不执行其中任何内容。
package packs

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
)

// Pack 是一个已通过全部校验的能力包。
type Pack struct {
	Name           string           `yaml:"name"`
	Version        string           `yaml:"version"`
	Description    string           `yaml:"description"`
	Kinds          []Kind           `yaml:"kinds"`
	MaterialKinds  []MaterialKind   `yaml:"material_kinds"`
	InstanceParams map[string]any   `yaml:"instance_params"`
	ClockRules     []ClockRule      `yaml:"clock_rules"`
	SourceRules    []SourceRule     `yaml:"source_rules"`
	WebhookSchemes []WebhookScheme  `yaml:"webhook_schemes"`
	WebhookMaps    []WebhookMapping `yaml:"webhook_mappings"`
	EventRules     []EventRule      `yaml:"event_rules"`

	Prompts   map[string]Prompt `yaml:"-"` // 键是文件名去掉 .md
	Views     map[string]View   `yaml:"-"` // 键是文件名去掉 .yaml
	Templates map[string]string `yaml:"-"` // 键是文件名去掉 .md

	Hash       string            `yaml:"-"` // 整个包目录内容的 sha256
	RuleHashes map[string]string `yaml:"-"` // "<组>/<id>" → 规则内容 sha256，主动服务据此判断规则是否被改动（§13.3）
}

type Kind struct {
	Name       string         `yaml:"name"`
	Title      string         `yaml:"title"`
	SpecSchema map[string]any `yaml:"spec_schema"`
}

// MaterialKind 扩展材料 kind；WritePolicy 是默认写入策略 direct 或 proposed（§12.4）。
type MaterialKind struct {
	Name        string `yaml:"name"`
	Title       string `yaml:"title"`
	WritePolicy string `yaml:"write_policy"`
}

// Action 是规则里的一个枚举动作，键值平铺；action 键是动作名。
type Action map[string]any

type ClockRule struct {
	ID       string   `yaml:"id"`
	Type     string   `yaml:"type"`
	Schedule string   `yaml:"schedule"`
	Mode     string   `yaml:"mode"`
	Do       []Action `yaml:"do"`
	// 提醒类规则的参数（C.4）；到期行为由内核固定，不写 do[]。
	RemindRounds *int   `yaml:"remind_rounds"` // 只用于 follow_up，默认 2
	DeferWith    string `yaml:"defer_with"`    // 只用于 follow_up：yes_no 判断型模板名
	StaleAfter   string `yaml:"stale_after"`   // 只用于 stale，默认 14d
}

type SourceRule struct {
	ID        string         `yaml:"id"`
	Match     map[string]any `yaml:"match"`
	Where     []Condition    `yaml:"where"`
	Kind      string         `yaml:"kind"`
	ProjectBy string         `yaml:"project_by"`
	Then      string         `yaml:"then"`
	// 各步所用的提示词模板名（C.5）；省略时取内置模板。
	Screen          string   `yaml:"screen"`
	Draft           string   `yaml:"draft"`
	Prescreen       string   `yaml:"prescreen"`
	Extract         string   `yaml:"extract"`
	CorrelationKeys []string `yaml:"correlation_keys"`
	OnUpdate        string   `yaml:"on_update"` // refresh | attach：关联键命中本人未委派的事项时（§9.4）；省略按来源对象型处理
	Accept          string   `yaml:"accept"`
	Mode            string   `yaml:"mode"`
}

type WebhookScheme struct {
	ID               string     `yaml:"id"`
	SignatureHeader  string     `yaml:"signature_header"`
	SignaturePrefix  string     `yaml:"signature_prefix"`
	Algorithm        string     `yaml:"algorithm"`
	SignedPayload    string     `yaml:"signed_payload"`
	TimestampHeader  string     `yaml:"timestamp_header"`
	DeliveryIDHeader string     `yaml:"delivery_id_header"`
	Handshake        *Handshake `yaml:"handshake"`
}

type Handshake struct {
	When    map[string]any `yaml:"when"`
	Respond map[string]any `yaml:"respond"`
}

type WebhookMapping struct {
	ID             string         `yaml:"id"`
	Match          map[string]any `yaml:"match"`
	Fields         map[string]any `yaml:"fields"`
	SenderVerified string         `yaml:"sender_verified"`
}

type EventRule struct {
	ID   string         `yaml:"id"`
	When map[string]any `yaml:"when"`
	Mode string         `yaml:"mode"`
	Do   []Action       `yaml:"do"`
}

// Prompt 是提示词模板：front matter 加正文。
type Prompt struct {
	Meta PromptMeta
	Body string
}

type PromptMeta struct {
	Agent        string `yaml:"agent"`
	RunAs        string `yaml:"run_as"`
	QuestionType string `yaml:"question_type"`
	Choices      []any  `yaml:"choices"`
	// ConfidenceThreshold 与 EscalateTo 是 v0.1 已移除的字段：只为识别并拒绝（C.7、C.11），通过校验的模板恒为空。
	ConfidenceThreshold *float64       `yaml:"confidence_threshold"`
	EscalateTo          string         `yaml:"escalate_to"`
	OutputSchema        map[string]any `yaml:"output_schema"`
	AnswerBy            []string       `yaml:"answer_by"`    // 判断型：答题顺序；校验后缺省已按题型补全
	Band                *Band          `yaml:"band"`         // yes_no 且含 classifier：不确定带；校验后两端都有值
	OnUndecided         any            `yaml:"on_undecided"` // 判断型必填
	SingleShot          bool           `yaml:"single_shot"`
	Criteria            string         `yaml:"criteria"`
	RelayTo             string         `yaml:"relay_to"` // 生成型：信息不足时接力的任务型模板
	EventStats          string         `yaml:"event_stats"`
	Timeout             string         `yaml:"timeout"`
	Access              string         `yaml:"access"`
	Cwd                 string         `yaml:"cwd"`
	ResultFields        []string       `yaml:"result_fields"`   // 任务型：spec 里要写全的字段，顶层键或 a[].b（§8.4）
	ResultMaterial      bool           `yaml:"result_material"` // 任务型：结果是一条材料修订
	Tools               any            `yaml:"tools"`           // 只为识别并拒绝"生成型声明工具"
}

// Band 是不确定带 {low, high}（C.7）；只写一端时另一端取相同的值。
type Band struct {
	Low  *float64 `yaml:"low"`
	High *float64 `yaml:"high"`
}

// View 是 Web 视图配置，止步于列表列、筛选、卡片字段（C.8）。JSON 形状即 GET /packs 的 views（附录 B.2）。
type View struct {
	Kind string `yaml:"kind" json:"kind"`
	List struct {
		Columns []string `yaml:"columns" json:"columns"`
		Filters []string `yaml:"filters" json:"filters"`
	} `yaml:"list" json:"list"`
	Card struct {
		Fields []string `yaml:"fields" json:"fields"`
	} `yaml:"card" json:"card"`
}

var (
	packName    = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	placeholder = regexp.MustCompile(`\{\s*([A-Za-z_][A-Za-z0-9_.]*)\s*\}`)
	staleAfter  = regexp.MustCompile(`^[1-9][0-9]*d$`)
	resultField = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\[\]\.[A-Za-z_][A-Za-z0-9_]*)?$`)

	// 提示词正文只能用内核提供的固定占位符（C.7）。
	promptPlaceholders = map[string]bool{"item_id": true, "run_id": true, "project_id": true, "materials_path": true,
		"instruction_path": true}

	// 能力包不能携带命令、脚本或策略字段（§13.3）；这些键出现在 schema 之外的任何位置都拒绝整个包。
	forbiddenKeys = map[string]bool{"command": true, "commands": true, "cmd": true, "exec": true, "script": true,
		"scripts": true, "shell": true, "run": true, "args": true, "argv": true, "hook": true, "hooks": true,
		"policy": true, "trust": true, "trust_level": true, "web_only": true, "forward_whitelist": true,
		"whitelist": true, "allowed_executors": true,
		// 自报规则类别、不确定带的策略覆盖、外发范围、打扰上限都是策略（C.11）。
		"category": true, "judgment": true, "bands": true, "outbound": true, "capture_outbound": true,
		"disturb": true, "daily_cap": true}
	// 这些键的值是用户自定义的 JSON Schema，里面的属性名不受上面的限制。
	schemaKeys = map[string]bool{"spec_schema": true, "instance_params": true, "output_schema": true}

	scriptSuffixes = []string{".sh", ".bash", ".py", ".js", ".rb", ".pl", ".exe", ".bat", ".ps1"}

	// 动作枚举及各自允许的键（C.6）。新增动作先改设计。
	actionKeys = map[string][]string{
		"set_phase":            {"phase"},
		"add_tag":              {"tag"},
		"create_item":          {"kind", "assignee", "parent"},
		"create_timer":         {"after", "at", "schedule"},
		"call_agent":           {"prompt"},
		"start_background_run": {"runner", "prompt", "access", "cwd", "harness", "executor"},
	}
)

// Actions 返回动作枚举（C.6），按名字排序；装配测试据它核对每个动作都有处理器。
func Actions() []string {
	out := make([]string, 0, len(actionKeys))
	for a := range actionKeys {
		out = append(out, a)
	}
	sort.Strings(out)
	return out
}

// parsePack 读取并校验 root 下的一个包目录（C.11）。dir 是目录名，必须等于包名。
// 任何一项不通过都拒绝整个包。返回的 hash 在校验失败时也有值（有文件可读时），供调用方去重。
func parsePack(fsys fs.FS, dir string, checkSchema func(json.RawMessage) error) (*Pack, string, error) {
	files, err := readTree(fsys, dir)
	if err != nil {
		return nil, "", err
	}
	hash := treeHash(files)
	p, err := validatePack(dir, files, checkSchema)
	if err != nil {
		return nil, hash, err
	}
	p.Hash = hash
	return p, hash, nil
}

func readTree(fsys fs.FS, dir string) (map[string][]byte, error) {
	files := map[string][]byte{}
	err := fs.WalkDir(fsys, dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return fmt.Errorf("%s: not a regular file", p)
		}
		b, err := fs.ReadFile(fsys, p)
		if err != nil {
			return err
		}
		files[strings.TrimPrefix(p, dir+"/")] = b
		return nil
	})
	return files, err
}

func treeHash(files map[string][]byte) string {
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		fmt.Fprintf(h, "%s\x00%d\x00", n, len(files[n]))
		h.Write(files[n])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func validatePack(dir string, files map[string][]byte, checkSchema func(json.RawMessage) error) (*Pack, error) {
	raw, ok := files["pack.yaml"]
	if !ok {
		return nil, errors.New("pack.yaml is missing")
	}
	// 目录布局之外的文件一律拒绝：包里不能夹带代码（§13.1）。隐藏文件（编辑器残留等）忽略。
	var names []string
	for n := range files {
		if strings.HasPrefix(path.Base(n), ".") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		if !inLayout(n) {
			return nil, fmt.Errorf("%s: file is outside the pack layout (pack.yaml, prompts/*.md, views/*.yaml, templates/*.md)", n)
		}
	}

	var p Pack
	if err := decodeStrict(raw, &p); err != nil {
		return nil, fmt.Errorf("pack.yaml: %w", err)
	}
	if err := packMeta(&p, dir); err != nil {
		return nil, err
	}
	return finish(&p, files, names, checkSchema)
}

func inLayout(n string) bool {
	dir, base := path.Split(n)
	switch dir {
	case "":
		return base == "pack.yaml"
	case "prompts/", "templates/":
		return strings.HasSuffix(base, ".md") && len(base) > 3
	case "views/":
		return strings.HasSuffix(base, ".yaml") && len(base) > 5
	}
	return false
}

// decodeStrict 解析 yaml：先扫描禁用键与脚本路径，再按结构严格解码（未知字段、类型不符都报错）。
func decodeStrict(data []byte, out any) error {
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil {
		return err
	}
	if err := scan(&node, false); err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

func scan(n *yaml.Node, inSchema bool) error {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			if err := scan(c, inSchema); err != nil {
				return err
			}
		}
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			k, val := n.Content[i], n.Content[i+1]
			if !inSchema && forbiddenKeys[k.Value] {
				return fmt.Errorf("line %d: field %q is not allowed: packs carry no commands, scripts or policy", k.Line, k.Value)
			}
			if err := scan(val, inSchema || schemaKeys[k.Value]); err != nil {
				return err
			}
		}
	case yaml.ScalarNode:
		if !inSchema && n.Tag == "!!str" && looksLikeScript(n.Value) {
			return fmt.Errorf("line %d: value %q looks like an executable path", n.Line, n.Value)
		}
	}
	return nil
}

func looksLikeScript(s string) bool {
	if strings.ContainsAny(s, " \t\n{}$") {
		return false
	}
	for _, p := range []string{"./", "../", "/", "~/"} {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	l := strings.ToLower(s)
	for _, suf := range scriptSuffixes {
		if strings.HasSuffix(l, suf) {
			return true
		}
	}
	return false
}

func packMeta(p *Pack, dir string) error {
	if p.Name == "" || p.Version == "" || p.Description == "" {
		return errors.New("name, version and description are required")
	}
	if !packName.MatchString(p.Name) {
		return fmt.Errorf("name %q must be lowercase letters, digits and dashes", p.Name)
	}
	if p.Name != dir {
		return fmt.Errorf("name %q does not match directory %q", p.Name, dir)
	}
	return nil
}

func finish(p *Pack, files map[string][]byte, names []string, checkSchema func(json.RawMessage) error) (*Pack, error) {
	schema := func(where string, s map[string]any) error {
		b, err := json.Marshal(s)
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		if err := checkSchema(b); err != nil {
			return fmt.Errorf("%s: invalid JSON Schema: %w", where, err)
		}
		return nil
	}
	seen := map[string]bool{}
	for _, k := range p.Kinds {
		if k.Name == "" {
			return nil, errors.New("kinds: name is required")
		}
		if seen["kind/"+k.Name] {
			return nil, fmt.Errorf("kinds: duplicate kind %q", k.Name)
		}
		seen["kind/"+k.Name] = true
		if k.SpecSchema != nil {
			if err := schema("kinds."+k.Name+".spec_schema", k.SpecSchema); err != nil {
				return nil, err
			}
		}
	}
	for _, m := range p.MaterialKinds {
		if m.Name == "" || (m.WritePolicy != "direct" && m.WritePolicy != "proposed") {
			return nil, fmt.Errorf("material_kinds: %q needs a name and write_policy direct|proposed", m.Name)
		}
	}
	if p.InstanceParams != nil {
		if err := schema("instance_params", p.InstanceParams); err != nil {
			return nil, err
		}
	}

	p.RuleHashes = map[string]string{}
	add := func(group, id string, rule any) error {
		if id == "" {
			return fmt.Errorf("%s: rule id is required", group)
		}
		key := group + "/" + id
		if _, dup := p.RuleHashes[key]; dup {
			return fmt.Errorf("%s: duplicate id %q", group, id)
		}
		b, _ := json.Marshal(rule) // 由 yaml 解码出的值，必可序列化
		sum := sha256.Sum256(b)
		p.RuleHashes[key] = hex.EncodeToString(sum[:])
		return nil
	}
	var actions []Action
	for _, r := range p.ClockRules {
		if err := add("clock_rules", r.ID, r); err != nil {
			return nil, err
		}
		if r.Type != "recur" && r.Type != "follow_up" && r.Type != "stale" && r.Type != "check" {
			return nil, fmt.Errorf("clock_rules.%s: type %q must be recur, follow_up, stale or check", r.ID, r.Type)
		}
		if err := checkReminderRule(r); err != nil {
			return nil, err
		}
		// 含 {instance.…} 占位符的 schedule 由实例参数提供，创建实例时再校验。
		if r.Schedule != "" && !strings.Contains(r.Schedule, "{") {
			if _, err := clock.ParseSchedule(r.Schedule); err != nil {
				return nil, fmt.Errorf("clock_rules.%s: schedule: %w", r.ID, err)
			}
		}
		if err := checkMode("clock_rules."+r.ID, r.Mode); err != nil {
			return nil, err
		}
		actions = append(actions, r.Do...)
	}
	for _, r := range p.EventRules {
		if err := add("event_rules", r.ID, r); err != nil {
			return nil, err
		}
		if err := checkMode("event_rules."+r.ID, r.Mode); err != nil {
			return nil, err
		}
		if err := flat("event_rules."+r.ID+".when", r.When); err != nil {
			return nil, err
		}
		actions = append(actions, r.Do...)
	}
	for _, r := range p.SourceRules {
		if err := add("source_rules", r.ID, r); err != nil {
			return nil, err
		}
		if err := checkSourceRule(r); err != nil {
			return nil, err
		}
	}
	for _, s := range p.WebhookSchemes {
		if err := add("webhook_schemes", s.ID, s); err != nil {
			return nil, err
		}
	}
	for _, m := range p.WebhookMaps {
		if err := add("webhook_mappings", m.ID, m); err != nil {
			return nil, err
		}
	}
	for _, a := range actions {
		if err := checkAction(a); err != nil {
			return nil, err
		}
	}

	if err := loadParts(p, files, names, schema); err != nil {
		return nil, err
	}
	for _, a := range actions {
		if name, _ := a["prompt"].(string); name != "" {
			if _, ok := p.Prompts[name]; !ok {
				return nil, fmt.Errorf("action %s: prompt %q not found in prompts/", a["action"], name)
			}
		}
	}
	return p, nil
}

// checkReminderRule 校验提醒类规则的专属字段（C.4、C.11）。
func checkReminderRule(r ClockRule) error {
	where := "clock_rules." + r.ID
	reminder := r.Type == "follow_up" || r.Type == "stale"
	if reminder && len(r.Do) > 0 {
		return fmt.Errorf("%s: a %s rule cannot carry do[] (v0.1 used it to call a judge): delete do[]; expiry behaviour is fixed by the kernel (C.4)", where, r.Type)
	}
	if r.Type != "follow_up" && (r.RemindRounds != nil || r.DeferWith != "") {
		return fmt.Errorf("%s: remind_rounds and defer_with are only for follow_up rules", where)
	}
	if r.Type == "follow_up" && r.RemindRounds != nil && *r.RemindRounds < 1 {
		return fmt.Errorf("%s: remind_rounds must be at least 1", where)
	}
	if r.Type != "stale" && r.StaleAfter != "" {
		return fmt.Errorf("%s: stale_after is only for stale rules", where)
	}
	if r.StaleAfter != "" && !staleAfter.MatchString(r.StaleAfter) {
		if d, err := time.ParseDuration(r.StaleAfter); err != nil || d <= 0 {
			return fmt.Errorf("%s: stale_after %q must be a positive duration such as 14d or 72h", where, r.StaleAfter)
		}
	}
	return nil
}

func checkMode(where, mode string) error {
	switch mode {
	case "", "off", "observe":
		return nil
	case "auto":
		return fmt.Errorf("%s: mode auto is not allowed in a pack; auto can only be switched in Web", where)
	}
	return fmt.Errorf("%s: mode %q must be off or observe", where, mode)
}

func checkSourceRule(r SourceRule) error {
	where := "source_rules." + r.ID
	if err := flat(where+".match", r.Match); err != nil {
		return err
	}
	if r.ProjectBy != "" && r.ProjectBy != "repo_remote" && r.ProjectBy != "none" && r.ProjectBy != "draft" && !strings.HasPrefix(r.ProjectBy, "fixed:") {
		return fmt.Errorf("%s: project_by %q must be repo_remote, fixed:<project>, draft or none", where, r.ProjectBy)
	}
	switch r.OnUpdate {
	case "", "refresh", "attach":
	default:
		return fmt.Errorf("%s: on_update %q must be refresh or attach", where, r.OnUpdate)
	}
	switch r.Then {
	case "", "proposed", "ignore", "screen", "judge", "extract", "digest", "alert":
	default:
		return fmt.Errorf("%s: then %q must be proposed, ignore, screen, extract, digest or alert (judge is the old name of screen)", where, r.Then)
	}
	if r.Then == "alert" && len(r.Where) == 0 {
		return fmt.Errorf("%s: an alert rule must carry where[]: alert cannot hang on every event", where)
	}
	for i, c := range r.Where {
		if err := c.check(fmt.Sprintf("%s.where[%d]", where, i)); err != nil {
			return err
		}
	}
	switch r.Accept {
	case "", "manual":
		return checkMode(where, r.Mode)
	case "auto": // 动作类规则：省略 mode 即初始 observe（C.4）
		return checkMode(where, r.Mode)
	}
	return fmt.Errorf("%s: accept %q must be manual or auto", where, r.Accept)
}

// flat 要求条件是平铺的相等或成员判断：值只能是标量或标量列表，不是表达式（C.5、C.6）。
func flat(where string, m map[string]any) error {
	for k, val := range m {
		switch x := val.(type) {
		case map[string]any:
			return fmt.Errorf("%s.%s: conditions are flat equality or membership, nested objects are not supported", where, k)
		case []any:
			for _, e := range x {
				switch e.(type) {
				case map[string]any, []any:
					return fmt.Errorf("%s.%s: list members must be scalars", where, k)
				}
			}
		}
	}
	return nil
}

func checkAction(a Action) error {
	name, _ := a["action"].(string)
	allowed, ok := actionKeys[name]
	if !ok {
		return fmt.Errorf("action %q is not in the enumeration (set_phase, add_tag, create_item, create_timer, call_agent, start_background_run)", name)
	}
	for k := range a {
		if k == "action" {
			continue
		}
		found := false
		for _, ak := range allowed {
			found = found || ak == k
		}
		if !found {
			return fmt.Errorf("action %s: field %q is not allowed", name, k)
		}
	}
	if name == "start_background_run" {
		switch a["runner"] {
		case "task", "main_session":
		default:
			return fmt.Errorf("action start_background_run: runner must be task or main_session")
		}
	}
	return nil
}

func loadParts(p *Pack, files map[string][]byte, names []string, schema func(string, map[string]any) error) error {
	p.Prompts = map[string]Prompt{}
	p.Views = map[string]View{}
	p.Templates = map[string]string{}
	var bad []string // 提示词的问题一次列全，健康视图才能逐字段说明
	for _, n := range names {
		dir, base := path.Split(n)
		switch dir {
		case "prompts/":
			name := strings.TrimSuffix(base, ".md")
			pr, err := parsePrompt(files[n], n, schema)
			if err != nil {
				bad = append(bad, fmt.Sprintf("%s: %v", n, err))
				continue
			}
			p.Prompts[name] = pr
		case "views/":
			var vw View
			if err := decodeStrict(files[n], &vw); err != nil {
				return fmt.Errorf("%s: %w", n, err)
			}
			if vw.Kind == "" {
				return fmt.Errorf("%s: kind is required", n)
			}
			for _, l := range []*[]string{&vw.List.Columns, &vw.List.Filters, &vw.Card.Fields} {
				if *l == nil { // 未声明的列表对外是 []，不是 null
					*l = []string{}
				}
			}
			p.Views[strings.TrimSuffix(base, ".yaml")] = vw
		case "templates/":
			p.Templates[strings.TrimSuffix(base, ".md")] = string(files[n])
		}
	}
	if len(bad) > 0 {
		return errors.New(strings.Join(bad, "; "))
	}
	return checkPromptRefs(p)
}

// checkPromptRefs 校验跨模板的引用：relay_to 只能由生成型指向本包的任务型模板，defer_with 指向本包的 yes_no 判断型模板。
func checkPromptRefs(p *Pack) error {
	names := make([]string, 0, len(p.Prompts))
	for n := range p.Prompts {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		// instruction_path 只在由带 instruction 参数的实例驱动的运行里有值（C.7）；包没有这个参数，占位符永远是空的。
		if usesPlaceholder(p.Prompts[n].Body, "instruction_path") && !declaresInstruction(p.InstanceParams) {
			return fmt.Errorf("prompts/%s.md: {instruction_path} needs a string parameter %q in instance_params", n, "instruction")
		}
		to := p.Prompts[n].Meta.RelayTo
		if to == "" {
			continue
		}
		t, ok := p.Prompts[to]
		switch {
		case !ok:
			return fmt.Errorf("prompts/%s.md: relay_to %q does not exist in prompts/", n, to)
		case t.Meta.Agent != "task":
			return fmt.Errorf("prompts/%s.md: relay_to %q must be a task prompt (generate relays to task only)", n, to)
		}
	}
	for _, r := range p.ClockRules {
		if r.DeferWith == "" {
			continue
		}
		if t, ok := p.Prompts[r.DeferWith]; !ok || t.Meta.Agent != "judge" || t.Meta.QuestionType != "yes_no" {
			return fmt.Errorf("clock_rules.%s: defer_with %q must be a yes_no judge prompt in prompts/", r.ID, r.DeferWith)
		}
	}
	return nil
}

// UsesPlaceholder 报告提示词正文是否含占位符 {name}（括号内允许空白）。
func UsesPlaceholder(body, name string) bool { return usesPlaceholder(body, name) }

func usesPlaceholder(body, name string) bool {
	for _, mt := range placeholder.FindAllStringSubmatch(body, -1) {
		if mt[1] == name {
			return true
		}
	}
	return false
}

func declaresInstruction(params map[string]any) bool {
	props, _ := params["properties"].(map[string]any)
	ins, _ := props["instruction"].(map[string]any)
	return ins["type"] == "string"
}

func parsePrompt(data []byte, file string, schema func(string, map[string]any) error) (Prompt, error) {
	head, body, ok := splitFrontMatter(data)
	if !ok {
		return Prompt{}, errors.New("front matter (--- ... ---) is required")
	}
	var m PromptMeta
	if err := decodeStrict(head, &m); err != nil {
		return Prompt{}, fmt.Errorf("front matter: %w", err)
	}
	for _, mt := range placeholder.FindAllStringSubmatch(body, -1) {
		if !promptPlaceholders[mt[1]] {
			return Prompt{}, fmt.Errorf("unknown placeholder {%s}; allowed: item_id, run_id, project_id, materials_path, instruction_path", mt[1])
		}
	}
	if err := checkPromptMeta(&m, file, schema); err != nil {
		return Prompt{}, err
	}
	return Prompt{Meta: m, Body: body}, nil
}

func splitFrontMatter(data []byte) (head []byte, body string, ok bool) {
	s := strings.ReplaceAll(string(data), "\r\n", "\n")
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", false
	}
	rest := s[4:]
	i := strings.Index(rest, "\n---")
	if i < 0 {
		return nil, "", false
	}
	after := rest[i+4:]
	if after != "" && !strings.HasPrefix(after, "\n") {
		return nil, "", false
	}
	return []byte(rest[:i]), strings.TrimPrefix(after, "\n"), true
}

func checkPromptMeta(m *PromptMeta, file string, schema func(string, map[string]any) error) error {
	switch {
	case m.Agent != "" && m.RunAs != "":
		return errors.New("declare either agent or run_as, not both")
	case m.Agent == "" && m.RunAs == "":
		return errors.New("agent or run_as is required")
	}
	if m.EventStats != "" && m.EventStats != "proposal_outcome" {
		return fmt.Errorf("event_stats %q must be proposal_outcome", m.EventStats)
	}
	if m.Timeout != "" {
		if d, err := time.ParseDuration(m.Timeout); err != nil || d <= 0 {
			return fmt.Errorf("timeout %q is not a positive duration", m.Timeout)
		}
	}
	if m.Cwd != "" && m.Cwd != "run_dir" && m.Cwd != "project_checkout" {
		return fmt.Errorf("cwd %q must be run_dir or project_checkout", m.Cwd)
	}
	if err := removedFields(m); err != nil {
		return err
	}
	if m.OutputSchema != nil {
		if err := schema(file+" output_schema", m.OutputSchema); err != nil {
			return err
		}
	}
	switch {
	case m.Agent != "task" && (len(m.ResultFields) > 0 || m.ResultMaterial):
		return errors.New("result_fields and result_material are only for task prompts")
	case len(m.ResultFields) > 0 && m.ResultMaterial:
		return errors.New("declare either result_fields or result_material, not both")
	}
	for _, f := range m.ResultFields {
		if !resultField.MatchString(f) {
			return fmt.Errorf("result_fields %q must be a top-level key or a[].b", f)
		}
	}
	if m.RunAs != "" {
		if m.RunAs != "main_session" {
			return fmt.Errorf("run_as %q must be main_session", m.RunAs)
		}
		if m.EventStats != "" {
			return errors.New("event_stats is only allowed on judge and generate prompts")
		}
		if m.hasJudgeFields() || m.OutputSchema != nil || m.RelayTo != "" {
			return errors.New("run_as main_session cannot carry judge or generate fields")
		}
		return nil
	}
	switch m.Agent {
	case "judge":
		if m.OutputSchema != nil {
			return errors.New("a judge prompt produces a text answer: output_schema is not allowed")
		}
		if m.Tools != nil {
			return errors.New("a judge prompt cannot declare tools")
		}
		if m.RelayTo != "" {
			return errors.New("relay_to is only for generate prompts")
		}
		switch m.QuestionType {
		case "yes_no", "score":
		case "choice":
			if len(m.Choices) == 0 {
				return errors.New("question_type choice requires choices")
			}
		default:
			return fmt.Errorf("a judge prompt needs question_type yes_no, choice or score, got %q", m.QuestionType)
		}
		if err := checkJudge(m); err != nil {
			return err
		}
	case "generate":
		if m.OutputSchema == nil {
			return errors.New("a generate prompt requires output_schema")
		}
		if m.Tools != nil {
			return errors.New("a generate prompt cannot declare tools")
		}
		if m.hasJudgeFields() {
			return errors.New("question_type, choices, answer_by, band, on_undecided, single_shot and criteria are for judge prompts")
		}
	case "task":
		if m.hasJudgeFields() || m.EventStats != "" || m.RelayTo != "" {
			return errors.New("judge fields, event_stats and relay_to are not for task prompts")
		}
	default:
		return fmt.Errorf("agent %q must be judge, generate or task", m.Agent)
	}
	if m.Agent != "task" && (m.Access != "" || m.Cwd != "") {
		return errors.New("access and cwd are only for task prompts")
	}
	return nil
}

func (m *PromptMeta) hasJudgeFields() bool {
	return m.QuestionType != "" || m.Choices != nil || m.AnswerBy != nil || m.Band != nil ||
		m.OnUndecided != nil || m.SingleShot || m.Criteria != ""
}

// removedFields 拒绝 v0.1 的写法并给出 C.7 的对应写法。所有问题一次列全。
func removedFields(m *PromptMeta) error {
	var bad []string
	if m.ConfidenceThreshold != nil {
		bad = append(bad, "confidence_threshold is removed: for yes_no questions use band and answer_by (the kernel no longer reads the model's self-reported confidence)")
	}
	if m.EscalateTo != "" {
		switch m.Agent {
		case "generate":
			bad = append(bad, "escalate_to is removed on generate prompts: use relay_to: <task prompt>")
		default:
			bad = append(bad, "escalate_to is removed on judge prompts: write escalation of the same question in answer_by; write the next step after a verdict in the rule that uses it (source rule draft/extract, rule do[])")
		}
	}
	for _, c := range m.Choices {
		if s := fmt.Sprint(c); s == "需要解释" || s == "need_explanation" {
			bad = append(bad, fmt.Sprintf("choices option %q is removed: delete it; express \"cannot answer\" with on_undecided and chain a generate prompt in the rule when an explanation is needed", s))
		}
	}
	if m.Agent == "judge" && m.OnUndecided == nil {
		bad = append(bad, "on_undecided is required on judge prompts: set the more conservative answer used when no level decides (yes/no, one of choices, or a number)")
	}
	if len(bad) == 0 {
		return nil
	}
	return errors.New(strings.Join(bad, "; "))
}

// checkJudge 校验判断型新字段的组合（C.7、C.11），并补全 answer_by 缺省与 band 的单端写法。
func checkJudge(m *PromptMeta) error {
	hasClassifier := m.QuestionType == "yes_no" && m.AnswerBy == nil // 缺省的 yes_no 含 classifier
	for _, b := range m.AnswerBy {
		switch b {
		case "classifier":
			if m.QuestionType != "yes_no" {
				return fmt.Errorf("answer_by classifier is only for yes_no questions, not %s", m.QuestionType)
			}
			hasClassifier = true
		case "chat":
		default:
			return fmt.Errorf("answer_by %q must be classifier or chat", b)
		}
	}
	if m.AnswerBy != nil && len(m.AnswerBy) == 0 {
		return errors.New("answer_by cannot be empty")
	}
	if m.AnswerBy == nil {
		m.AnswerBy = []string{"chat"}
		if m.QuestionType == "yes_no" {
			m.AnswerBy = []string{"classifier", "chat"}
		}
	}
	if !hasClassifier && (m.Band != nil || m.SingleShot) {
		return errors.New("band and single_shot need classifier in answer_by")
	}
	if m.Band != nil {
		lo, hi := m.Band.Low, m.Band.High
		if lo == nil && hi == nil {
			return errors.New("band needs low or high")
		}
		if lo == nil {
			lo = hi
		}
		if hi == nil {
			hi = lo
		}
		if *lo < 0 || *lo > 1 || *hi < 0 || *hi > 1 {
			return errors.New("band low and high must be within 0..1")
		}
		if *lo > *hi {
			return errors.New("band low must not exceed high")
		}
		m.Band = &Band{Low: lo, High: hi}
	}
	return checkOnUndecided(m)
}

func checkOnUndecided(m *PromptMeta) error {
	switch m.QuestionType {
	case "yes_no":
		if v, _ := m.OnUndecided.(string); v != "yes" && v != "no" {
			return fmt.Errorf("on_undecided %v must be yes or no for a yes_no question", m.OnUndecided)
		}
	case "choice":
		for _, c := range m.Choices {
			if fmt.Sprint(c) == fmt.Sprint(m.OnUndecided) {
				return nil
			}
		}
		return fmt.Errorf("on_undecided %v must be one of choices", m.OnUndecided)
	case "score":
		switch m.OnUndecided.(type) {
		case int, int64, float64:
		default:
			return fmt.Errorf("on_undecided %v must be a number for a score question", m.OnUndecided)
		}
	}
	return nil
}

// Condition 是来源规则 where[] 里的一条字段条件（C.5）：平铺的比较，不是表达式。
type Condition struct {
	Field string `yaml:"field"`
	Op    string `yaml:"op"`
	Value any    `yaml:"value"`
}

// EventFields 是 where[] 可引用的标准事件顶层字段；此外还有 metrics.<指标名>。
var EventFields = []string{"type", "subject", "summary", "detail", "link", "sender", "sender_verified", "merge_key", "source"}

// MetricName 返回 field 是 metrics.<名> 时的指标名。
func MetricName(field string) (string, bool) {
	n, ok := strings.CutPrefix(field, "metrics.")
	return n, ok && n != ""
}

// IsChangeOp 报告 op 是否是与同一序列上一次取值比较的变化类条件。
func IsChangeOp(op string) bool {
	return op == "changed" || op == "crosses_above" || op == "crosses_below"
}

func (c Condition) check(where string) error {
	_, isMetric := MetricName(c.Field)
	if !isMetric && !slices.Contains(EventFields, c.Field) {
		return fmt.Errorf("%s: field %q must be a standard event field (%s) or metrics.<name>", where, c.Field, strings.Join(EventFields, ", "))
	}
	_, isInt := c.Value.(int)
	_, isFloat := c.Value.(float64)
	numeric := isInt || isFloat
	switch c.Op {
	case "eq", "ne":
		switch c.Value.(type) {
		case string, bool, int, float64:
			return nil
		}
		return fmt.Errorf("%s: op %s needs a scalar value", where, c.Op)
	case "in":
		l, ok := c.Value.([]any)
		if !ok || len(l) == 0 {
			return fmt.Errorf("%s: op in needs a non-empty list value", where)
		}
		return nil
	case "gt", "gte", "lt", "lte":
		if !isMetric || !numeric {
			return fmt.Errorf("%s: op %s compares a number and can only be used on metrics.<name>", where, c.Op)
		}
	case "changed":
		if !isMetric {
			return fmt.Errorf("%s: op changed can only be used on metrics.<name>", where)
		}
		if c.Value != nil {
			return fmt.Errorf("%s: op changed takes no value", where)
		}
	case "crosses_above", "crosses_below":
		if !isMetric {
			return fmt.Errorf("%s: op %s can only be used on metrics.<name>", where, c.Op)
		}
		if !numeric {
			return fmt.Errorf("%s: op %s needs a numeric value", where, c.Op)
		}
	default:
		return fmt.Errorf("%s: op %q must be eq, ne, in, gt, gte, lt, lte, changed, crosses_above or crosses_below", where, c.Op)
	}
	return nil
}
