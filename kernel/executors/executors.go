// Package executors 是执行者名录与无头运行声明的唯一写入模块（设计 §6.17、§7.5、§14.12、附录 A.2）。
//
// 名录由宿主在第一次委派时随 delegate.create 登记（Enroll），身份字段（harness、kind、host）登记即固定；
// 旧插件的声明里的执行者在本人确认后并入名录，不删除已登记的条目。内核只把名录与声明当数据存取，
// 不执行其中任何命令，句柄只校验结构（handle.go），不解释。
//
// 声明文档用 JSON（YAML 的子集）。shortcut: 仓库没有 YAML 依赖，需要 YAML 文件时在 CLI 侧转换。
package executors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 执行者种类（§6.17）。
const (
	KindNativeSubagent = "native-subagent"
	KindExternalCLI    = "external-cli"
	KindRemoteAgent    = "remote-agent"
	KindHuman          = "human"
)

// MaxEntries 是名录条目上限：宿主把每个子 agent 实例都登记成执行者时在这里被拦下（§6.17）。
const MaxEntries = 256

// 声明状态：待确认 → 生效；被本人拒绝；被同 harness 的新声明取代。
const (
	StatePending    = "pending"
	StateActive     = "active"
	StateRejected   = "rejected"
	StateSuperseded = "superseded"
)

// Executor 是声明（旧插件）与本人维护时的一项。Host 是自报字段，内核不校验进程实际所在。
type Executor struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Scenarios []string `json:"scenarios,omitempty"`
	Host      string   `json:"host,omitempty"`
}

// Entry 是名录里的一条：Harness 是登记者（为空是本人维护的旧条目），Label 与 Handle 由登记的插件给出，
// CreatedAt 是首次登记的时间（旧条目没有）。
type Entry struct {
	Executor
	Harness   string          `json:"harness,omitempty"`
	Label     string          `json:"label,omitempty"`
	Handle    json.RawMessage `json:"handle,omitempty"`
	CreatedAt *time.Time      `json:"created_at,omitempty"`
}

// Session 的 Assign 与 Extract 二选一。
type Session struct {
	Assign  []string `json:"assign,omitempty"`
	Extract string   `json:"extract,omitempty"`
}

// Headless 是无头运行声明（A.2）。Command 是命令模板，只作数据保存。
type Headless struct {
	Command   []string            `json:"command"`
	Output    []string            `json:"output,omitempty"`
	FinalText string              `json:"final_text,omitempty"`
	Usage     *UsageSource        `json:"usage,omitempty"`
	Session   Session             `json:"session"`
	Resume    []string            `json:"resume,omitempty"`
	Access    map[string][]string `json:"access"`
	// NoPersona 是本人没开"后台运行带人格和记忆"时追加的标志（附录 A.2）；未声明表示这个宿主不区分。
	NoPersona []string `json:"no_persona,omitempty"`
	// Effort 是带思考强度的标志，其中的 {effort} 换成设置里的"后台 Agent 运行"一项；未声明表示这个宿主不区分。
	Effort     []string         `json:"effort,omitempty"`
	Credential string           `json:"credential,omitempty"`
	ExitCodes  map[string][]int `json:"exit_codes,omitempty"`
	// ExitOutput 收窄 exit_codes 的同名语义：声明后，还要求 stdout 或 stderr 含其中任一片段（附录 A.2）。
	ExitOutput map[string][]string `json:"exit_output,omitempty"`
	// Tools 与 ToolNotes 只供界面展示每一档给 Agent 的工具（附录 A.2），内核不解释。
	Tools     map[string][]Label `json:"tools,omitempty"`
	ToolNotes map[string]Label   `json:"tool_notes,omitempty"`
}

// UsageSource 声明一次无头运行的 token 用量从哪读（附录 A.2）。取到的值只算本次运行，续接时不含此前运行。
// File 非空时读运行目录里的这个 JSON 文件，否则读 stdout 里最后一个带 input 与 output 字段的 JSON 行（Event 非空时只看
// type 等于它的行）。其余各项是对象里的字段路径。
type UsageSource struct {
	File       string `json:"file,omitempty"`
	Event      string `json:"event,omitempty"`
	Input      string `json:"input"`
	Output     string `json:"output"`
	CacheRead  string `json:"cache_read,omitempty"`
	CacheWrite string `json:"cache_write,omitempty"`
	Model      string `json:"model,omitempty"`
	// InputExcludesCache 表示 input 不含缓存读写，内核把两项加进输入。
	InputExcludesCache bool `json:"input_excludes_cache,omitempty"`
}

// RunUsage 是一次运行回报的用量；Input 含缓存读写，Model 可空。
type RunUsage struct {
	Input, Output, CacheRead, CacheWrite int64
	Model                                string
}

// Parse 从用量内容（File 的全文，或 stdout 末段）取用量；没有可用的对象，或输入输出全是 0（一次请求都没成）时 ok 为假。
func (s UsageSource) Parse(data []byte) (RunUsage, bool) {
	if s.File != "" {
		return s.from(data)
	}
	lines := bytes.Split(data, []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		if u, ok := s.from(lines[i]); ok {
			return u, true
		}
	}
	return RunUsage{}, false
}

func (s UsageSource) from(data []byte) (u RunUsage, ok bool) {
	var obj map[string]any
	if json.Unmarshal(bytes.TrimSpace(data), &obj) != nil || (s.Event != "" && obj["type"] != s.Event) {
		return u, false
	}
	in, iok := jsonCount(obj, s.Input)
	out, ook := jsonCount(obj, s.Output)
	if !iok || !ook {
		return u, false
	}
	u.Input, u.Output = in, out
	u.CacheRead, _ = jsonCount(obj, s.CacheRead)
	u.CacheWrite, _ = jsonCount(obj, s.CacheWrite)
	u.Model, _ = extractJSONPath(obj, []string{s.Model})
	if s.InputExcludesCache {
		u.Input += u.CacheRead + u.CacheWrite
	}
	return u, u.Input > 0 || u.Output > 0
}

func (s UsageSource) validate() error {
	if s.File != "" && (s.File != filepath.Base(s.File) || s.File == "." || s.File == "..") {
		return invalid("headless.usage.file must be a plain file name in the run directory")
	}
	if s.File != "" && s.Event != "" {
		return invalid("headless.usage.event only applies to stdout, not to usage.file")
	}
	if !validJSONPath(s.Input) || !validJSONPath(s.Output) {
		return invalid("headless.usage needs input and output as $.field paths")
	}
	for _, p := range []string{s.CacheRead, s.CacheWrite, s.Model} {
		if p != "" && !validJSONPath(p) {
			return invalid("headless.usage cache_read, cache_write and model must be $.field paths")
		}
	}
	return nil
}

// jsonCount 取 $.a.b 路径上的非负整数。
func jsonCount(obj map[string]any, path string) (int64, bool) {
	rest, ok := strings.CutPrefix(path, "$.")
	if !ok {
		return 0, false
	}
	var cur any = obj
	for _, seg := range strings.Split(rest, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return 0, false
		}
		cur = m[seg]
	}
	n, ok := cur.(float64)
	return int64(n), ok && n >= 0
}

// Label 是中英各一份的显示文字。
type Label struct {
	Zh string `json:"zh"`
	En string `json:"en"`
}

func (l Label) valid() bool { return strings.TrimSpace(l.Zh) != "" && strings.TrimSpace(l.En) != "" }

// Means 判断一次运行的退出是否符合声明里 key 的语义：退出码在 exit_codes[key] 里，
// 且声明了 exit_output[key] 时 stdout 或 stderr 含其中任一片段（区分分享同一退出码的失败）。
func (h Headless) Means(key string, code int, stdout, stderr []byte) bool {
	if !slices.Contains(h.ExitCodes[key], code) {
		return false
	}
	marks, ok := h.ExitOutput[key]
	if !ok {
		return true
	}
	for _, m := range marks {
		if bytes.Contains(stdout, []byte(m)) || bytes.Contains(stderr, []byte(m)) {
			return true
		}
	}
	return false
}

// StructuredOutput 报告声明的 output 参数或 final_text 是否表示结构化（JSON / JSONL）输出。
func (h Headless) StructuredOutput() bool {
	if h.FinalText != "" {
		return true
	}
	for _, a := range h.Output {
		if strings.Contains(strings.ToLower(a), "json") {
			return true
		}
	}
	return false
}

// ExtractFinalText 按执行器声明从无头运行的 stdout 提取最终助手文本（附录 A.2）。
// 声明了 FinalText 或结构化输出，或 stdout 本身为 JSONL 事件流时，只提取最终结果或助手文本事件，
// 过滤工具调用、工具返回、系统事件与非 JSON 运行日志；提取不到时返回空串，绝不把原始结构化日志泄给下游。
func (h Headless) ExtractFinalText(stdout []byte) string {
	raw := strings.TrimSpace(string(stdout))
	if raw == "" {
		return ""
	}
	paths := []string{"$.text", "$.result"}
	if h.FinalText != "" {
		paths = []string{h.FinalText}
	}
	structured := h.StructuredOutput()
	var (
		hasJSONEvent bool
		finalText    string
		hasFinal     bool
		chunks       []string
	)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(line), &obj); err != nil {
			continue
		}
		typ, _ := obj["type"].(string)
		if typ != "" {
			hasJSONEvent = true
		} else if !structured {
			continue
		}
		switch typ {
		case "result", "final", "":
			if s, ok := extractJSONPath(obj, paths); ok && s != "" {
				finalText = s
				hasFinal = true
			}
		case "text", "assistant":
			if s, ok := extractJSONPath(obj, paths); ok && s != "" {
				chunks = append(chunks, s)
			}
		default:
			// system / init / tool_use / tool_result / tool_call / thinking / error 等非最终助手文本事件一律过滤
		}
	}
	if structured || hasJSONEvent {
		if hasFinal {
			return strings.TrimSpace(finalText)
		}
		return strings.TrimSpace(strings.Join(chunks, ""))
	}
	return raw
}

func validJSONPath(p string) bool {
	if !strings.HasPrefix(p, "$.") || len(p) <= 2 {
		return false
	}
	for _, seg := range strings.Split(p[2:], ".") {
		if seg == "" {
			return false
		}
	}
	return true
}

func extractJSONPath(obj map[string]any, paths []string) (string, bool) {
	for _, p := range paths {
		if !strings.HasPrefix(p, "$.") {
			continue
		}
		var cur any = obj
		ok := true
		for _, seg := range strings.Split(p[2:], ".") {
			m, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur, ok = m[seg]
			if !ok {
				break
			}
		}
		if ok {
			if s, isStr := cur.(string); isStr {
				return s, true
			}
		}
	}
	return "", false
}

// MainSession 声明以 Agent 本人身份运行时能怎么拉起（§13.6、附录 A.2）：无头运行必有；Wake 为真表示 Agent 侧装了唤醒插件，
// 也能在真实主会话里接着做。用哪种由本人在 Web 选（策略的 main_session_launch），不在声明里。
// Launch 是旧声明的单值：wake 视为支持唤醒，并作为本人没选时的初始值；headless 或空不表达任何选择。
// WakeMissing 是插件按实际启用情况判定的、唤醒还缺的项（显示文字与打开方法），只供界面与自检展示，内核不解释。
type MainSession struct {
	Wake        bool      `json:"wake,omitempty"`
	Launch      string    `json:"launch,omitempty"`
	Args        []string  `json:"args,omitempty"`
	WakeMissing []WakeGap `json:"wake_missing,omitempty"`
}

// WakeGap 是唤醒缺的一项：一句产品语言的说明，加怎么打开。
type WakeGap struct {
	Label
	How Label `json:"how"`
}

// CanWake：声明支持唤醒（新字段 wake，或旧声明 launch: wake）。
func (m *MainSession) CanWake() bool { return m != nil && (m.Wake || m.Launch == "wake") }

// Initial 是旧声明 launch: wake 留下的初始选择；其余为空。
func (m *MainSession) Initial() string {
	if m != nil && m.Launch == "wake" {
		return "wake"
	}
	return ""
}

// Declaration 是 `apa declare` 提交或主机配置文件登记的整份声明。
type Declaration struct {
	InstanceOptions *InstanceOptions `json:"instance_options,omitempty"`
	Harness         string           `json:"harness"`
	Name            *Label           `json:"name,omitempty"` // 界面称呼这个 Agent 的名字（附录 A.2）
	Headless        *Headless        `json:"headless,omitempty"`
	MainSession     *MainSession     `json:"main_session,omitempty"`
	Executors       []Executor       `json:"executors,omitempty"`
	Selfcheck       *Selfcheck       `json:"selfcheck,omitempty"`
}

// Selfcheck 是插件的自检命令（A.1、A.2）：只作数据保存，由 `apa doctor` 在 APA 主机上执行，内核不执行。
type Selfcheck struct {
	Command []string `json:"command"`
}

// Record 是一份存下来的声明及其状态。
type Record struct {
	Harness     string      `json:"harness"`
	Rev         int64       `json:"rev"`
	State       string      `json:"state"`
	Declaration Declaration `json:"declaration"`
}

// Module 是本模块在存储层登记的迁移。entry.harness 为空表示本人维护的项。
var Module = store.Module{Name: "executors", Migrations: []store.Migration{{
	Version: 1, Name: "create-entry-and-declaration",
	SQL: `CREATE TABLE executors_entry (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	scenarios TEXT NOT NULL,
	host TEXT NOT NULL,
	harness TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE executors_declaration (
	harness TEXT NOT NULL,
	rev INTEGER NOT NULL,
	doc TEXT NOT NULL,
	state TEXT NOT NULL,
	PRIMARY KEY (harness, rev)
) WITHOUT ROWID;`,
}, {
	// 账本结构版本 5（§6.17）：宿主登记的显示名、句柄与首次登记时间。只增列，已有条目取空值。
	Version: 2, Ledger: 5, Name: "ledger-v5-executor-enroll",
	SQL: `ALTER TABLE executors_entry ADD COLUMN label TEXT NOT NULL DEFAULT '';
ALTER TABLE executors_entry ADD COLUMN handle TEXT NOT NULL DEFAULT '';
ALTER TABLE executors_entry ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0;`,
}, {
	Version: 3, Ledger: 9, Name: "agent-cards", SQL: `CREATE TABLE executors_agent (id TEXT PRIMARY KEY, harness TEXT NOT NULL, name TEXT NOT NULL, options TEXT NOT NULL, enabled INTEGER NOT NULL, rev INTEGER NOT NULL) WITHOUT ROWID;`,
}}}

func invalid(format string, a ...any) error {
	return domain.NewError(domain.CodeInvalidRequest, fmt.Sprintf(format, a...), "fix the declaration and retry")
}

func conflict(format string, a ...any) error {
	return domain.NewError(domain.CodeConflict, fmt.Sprintf(format, a...), "remove the existing entry first, or use another id")
}

func notFound(what string) error {
	return domain.NewError(domain.CodeNotFound, what+" not found", "list first and retry")
}

// decode 严格解码：未知字段、多余内容一律拒绝。
func decode(data []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("malformed JSON: %v", err)
	}
	if d.More() {
		return invalid("trailing content after JSON document")
	}
	return nil
}

// ParseDeclaration 解析并校验一份声明（A.2、§6.13）。
func ParseDeclaration(data []byte) (Declaration, error) {
	var d Declaration
	if err := decode(data, &d); err != nil {
		return Declaration{}, err
	}
	if o := d.InstanceOptions; o != nil && len(o.Fields)+len(o.Args)+len(o.Env)+len(o.Cwd) == 0 {
		d.InstanceOptions = nil
	}
	return d, d.validate()
}

func (d Declaration) validate() error {
	if err := d.InstanceOptions.validate(); err != nil {
		return err
	}
	if d.Harness == "" || strings.ContainsAny(d.Harness, " \t\r\n") {
		return invalid("harness is required and must not contain whitespace")
	}
	if d.Name != nil && !d.Name.valid() {
		return invalid("name needs both zh and en")
	}
	if sc := d.Selfcheck; sc != nil {
		if len(sc.Command) == 0 || slices.Contains(sc.Command, "") {
			return invalid("selfcheck.command must list non-empty argv elements")
		}
	}
	if d.Headless == nil && len(d.Executors) == 0 {
		return invalid("declaration needs headless and/or executors")
	}
	if d.Headless != nil {
		if err := d.Headless.validate(); err != nil {
			return err
		}
	} else if d.MainSession != nil {
		return invalid("main_session needs a headless declaration")
	}
	if m := d.MainSession; m != nil && m.Launch != "" && m.Launch != "headless" && m.Launch != "wake" {
		return invalid("main_session.launch must be headless or wake")
	}
	if m := d.MainSession; m != nil && len(m.WakeMissing) > 0 {
		if m.CanWake() {
			return invalid("main_session.wake_missing is only for a declaration that cannot wake")
		}
		for _, g := range m.WakeMissing {
			if !g.valid() || !g.How.valid() {
				return invalid("main_session.wake_missing: each item needs zh, en and how {zh, en}")
			}
		}
	}
	seen := map[string]bool{}
	for _, e := range d.Executors {
		if err := e.validate(); err != nil {
			return err
		}
		if seen[e.ID] {
			return invalid("duplicate executor id %q", e.ID)
		}
		seen[e.ID] = true
	}
	return nil
}

func (e Executor) validate() error {
	if e.ID == "" || strings.ContainsAny(e.ID, " \t\r\n") {
		return invalid("executor id is required and must not contain whitespace")
	}
	if !validKind(e.Kind) {
		return invalid("executor %q: kind must be native-subagent, external-cli, remote-agent or human", e.ID)
	}
	for _, s := range e.Scenarios {
		if s == "" {
			return invalid("executor %q: empty scenario", e.ID)
		}
	}
	return nil
}

var (
	placeholders = map[string]bool{"{prompt}": true, "{run_dir}": true}
	accessTiers  = map[string]bool{"read": true, "edit": true, "full": true}
)

func (h *Headless) validate() error {
	if len(h.Command) == 0 {
		return invalid("headless.command is required")
	}
	for _, a := range h.Command {
		if a == "" {
			return invalid("headless.command has an empty element")
		}
		if err := checkPlaceholders(a); err != nil {
			return err
		}
	}
	if (len(h.Session.Assign) > 0) == (h.Session.Extract != "") {
		return invalid("headless.session needs exactly one of assign and extract")
	}
	if h.FinalText != "" && !validJSONPath(h.FinalText) {
		return invalid("headless.final_text must be a $.field path")
	}
	if h.Usage != nil {
		if err := h.Usage.validate(); err != nil {
			return err
		}
	}
	if len(h.Access) == 0 {
		return invalid("headless.access must declare at least one of read, edit, full")
	}
	for tier := range h.Access {
		if !accessTiers[tier] {
			return invalid("headless.access tier %q is not read, edit or full", tier)
		}
	}
	for tier, labels := range h.Tools {
		if _, ok := h.Access[tier]; !ok {
			return invalid("headless.tools.%s needs headless.access.%s", tier, tier)
		}
		for _, l := range labels {
			if !l.valid() {
				return invalid("headless.tools.%s: each tool needs both zh and en", tier)
			}
		}
	}
	for tier, l := range h.ToolNotes {
		if _, ok := h.Access[tier]; !ok {
			return invalid("headless.tool_notes.%s needs headless.access.%s", tier, tier)
		}
		if !l.valid() {
			return invalid("headless.tool_notes.%s needs both zh and en", tier)
		}
	}
	if slices.Contains(h.NoPersona, "") {
		return invalid("headless.no_persona has an empty element")
	}
	if h.Credential != "" && h.Credential != "env" && h.Credential != "file" {
		return invalid("headless.credential must be env or file")
	}
	for key, marks := range h.ExitOutput {
		if len(h.ExitCodes[key]) == 0 {
			return invalid("headless.exit_output.%s needs exit_codes.%s", key, key)
		}
		if len(marks) == 0 || slices.Contains(marks, "") {
			return invalid("headless.exit_output.%s must list non-empty strings", key)
		}
	}
	return nil
}

// checkPlaceholders 拒绝命令模板里未登记的 {占位符}。
func checkPlaceholders(arg string) error {
	for rest := arg; ; {
		i := strings.IndexByte(rest, '{')
		if i < 0 {
			return nil
		}
		j := strings.IndexByte(rest[i:], '}')
		if j < 0 {
			return invalid("headless.command: unclosed placeholder in %q", arg)
		}
		if p := rest[i : i+j+1]; !placeholders[p] {
			return invalid("headless.command: unknown placeholder %s", p)
		}
		rest = rest[i+j+1:]
	}
}

// ---- 存取 ----

func query(tx *store.Tx, each func(*store.Rows) error, sql string, args ...any) error {
	r, err := tx.Query(sql, args...)
	if err != nil {
		return err
	}
	defer r.Close()
	for r.Next() {
		if err := each(r); err != nil {
			return err
		}
	}
	return r.Err()
}

func validKind(k string) bool {
	switch k {
	case KindNativeSubagent, KindExternalCLI, KindRemoteAgent, KindHuman:
		return true
	}
	return false
}

const entryCols = "e.id, e.kind, e.scenarios, e.host, e.harness, e.label, e.handle, e.created_at"

func scanEntry(r *store.Rows) (Entry, error) {
	var e Entry
	var scen, handle string
	var created int64
	if err := r.Scan(&e.ID, &e.Kind, &scen, &e.Host, &e.Harness, &e.Label, &handle, &created); err != nil {
		return e, err
	}
	if handle != "" {
		e.Handle = json.RawMessage(handle)
	}
	e.CreatedAt = unixTime(created)
	return e, json.Unmarshal([]byte(scen), &e.Scenarios)
}

func unixTime(ns int64) *time.Time {
	if ns <= 0 {
		return nil
	}
	t := time.Unix(0, ns).UTC()
	return &t
}

// List 返回执行者名录，按 id 排序。
func List(tx *store.Tx) ([]Entry, error) {
	out := []Entry{}
	err := query(tx, func(r *store.Rows) error {
		e, err := scanEntry(r)
		out = append(out, e)
		return err
	}, "SELECT "+entryCols+" FROM executors_entry e ORDER BY e.id")
	return out, err
}

// Get 返回名录里的一项；ok 为假表示不在名录里。
func Get(tx *store.Tx, id string) (e Entry, ok bool, err error) {
	err = query(tx, func(r *store.Rows) (err error) {
		e, err = scanEntry(r)
		ok = err == nil
		return
	}, "SELECT "+entryCols+" FROM executors_entry e WHERE e.id = ?", id)
	return
}

// Ref 返回名单判定用的执行者信息；不在名录里时只有 id。
func Ref(tx *store.Tx, id string) (domain.ExecutorRef, error) {
	e, ok, err := Get(tx, id)
	if err != nil || !ok {
		return domain.ExecutorRef{ID: id}, err
	}
	return domain.ExecutorRef{ID: id, Host: e.Host, Harness: e.Harness}, nil
}

// Candidates 返回名录里当前命中项目名单的执行者 id（随退出码 21 返回）；项目不限制时为空。
func Candidates(tx *store.Tx, p domain.Project, owner string) ([]string, error) {
	if !p.Restricted() {
		return nil, nil
	}
	es, err := List(tx)
	if err != nil {
		return nil, err
	}
	out := []string{}
	for _, e := range es {
		if e.ID != owner && p.ExecutorAllowed(domain.ExecutorRef{ID: e.ID, Host: e.Host, Harness: e.Harness}, owner) {
			out = append(out, e.ID)
		}
	}
	return out, nil
}

func scanRecord(r *store.Rows) (Record, error) {
	var rec Record
	var doc string
	if err := r.Scan(&rec.Harness, &rec.Rev, &doc, &rec.State); err != nil {
		return rec, err
	}
	return rec, json.Unmarshal([]byte(doc), &rec.Declaration)
}

// Records 返回某状态的声明（state 为空返回全部），按 harness、版本排序。
func Records(tx *store.Tx, state string) ([]Record, error) {
	out := []Record{}
	err := query(tx, func(r *store.Rows) error {
		rec, err := scanRecord(r)
		out = append(out, rec)
		return err
	}, "SELECT harness, rev, doc, state FROM executors_declaration WHERE (? = '' OR state = ?) ORDER BY harness, rev", state, state)
	return out, err
}

// ActiveHeadless 返回 harness 当前生效的无头运行声明；没有生效声明或声明不含 headless 时 ok 为假。
func ActiveHeadless(tx *store.Tx, harness string) (h Headless, ok bool, err error) {
	rec, found, err := record(tx, harness, 0, StateActive)
	if err != nil || !found || rec.Declaration.Headless == nil {
		return Headless{}, false, err
	}
	return *rec.Declaration.Headless, true, nil
}

// record 取一份声明；rev 为 0 表示不限版本，state 为空表示不限状态。
func record(tx *store.Tx, harness string, rev int64, state string) (rec Record, ok bool, err error) {
	err = query(tx, func(r *store.Rows) (err error) {
		rec, err = scanRecord(r)
		ok = err == nil
		return
	}, `SELECT harness, rev, doc, state FROM executors_declaration
		WHERE harness = ? AND (? = 0 OR rev = ?) AND (? = '' OR state = ?) ORDER BY rev DESC LIMIT 1`,
		harness, rev, rev, state, state)
	return
}

// insertDeclaration 追加新版本，旧的待确认版本作废；返回新版本号。
func insertDeclaration(tx *store.Tx, d Declaration, state string) (int64, error) {
	doc, err := json.Marshal(d)
	if err != nil {
		return 0, err
	}
	var rev int64
	if err := query(tx, func(r *store.Rows) error { return r.Scan(&rev) },
		"SELECT COALESCE(MAX(rev), 0) + 1 FROM executors_declaration WHERE harness = ?", d.Harness); err != nil {
		return 0, err
	}
	if _, err := tx.Exec("UPDATE executors_declaration SET state = ? WHERE harness = ? AND state = ?", StateSuperseded, d.Harness, StatePending); err != nil {
		return 0, err
	}
	_, err = tx.Exec("INSERT INTO executors_declaration (harness, rev, doc, state) VALUES (?, ?, ?, ?)", d.Harness, rev, string(doc), state)
	return rev, err
}

// checkCollisions：声明里的执行者 id 不能撞上别人（本人维护的或其他 harness 的）已生效的项。
func checkCollisions(tx *store.Tx, harness string, execs []Executor) error {
	for _, e := range execs {
		_, owner, found, err := entryOwner(tx, e.ID)
		if err != nil {
			return err
		}
		if found && owner != harness {
			return conflict("executor id %q is already in the list", e.ID)
		}
	}
	return nil
}

func entryOwner(tx *store.Tx, id string) (e Executor, harness string, ok bool, err error) {
	en, ok, err := Get(tx, id)
	return en.Executor, en.Harness, ok, err
}

func putEntry(tx *store.Tx, e Executor, harness string) error {
	scen, err := json.Marshal(append([]string{}, e.Scenarios...))
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO executors_entry (id, kind, scenarios, host, harness) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET kind = excluded.kind, scenarios = excluded.scenarios, host = excluded.host`,
		e.ID, e.Kind, string(scen), e.Host, harness)
	return err
}

// activate 让 d 生效：声明里的执行者并入名录（新增或更新），不删除该 harness 已登记的其他条目（§6.17）。
// 调用方先做冲突检查。
func activate(tx *store.Tx, d Declaration) error {
	cards, err := storedCards(tx, d.Harness)
	if err != nil {
		return err
	}
	for _, card := range cards {
		if card.Enabled {
			if err := d.InstanceOptions.check(card.Values); err != nil {
				return conflict("%s: %v", card.Name, err)
			}
		}
	}
	for _, e := range d.Executors {
		if err := putEntry(tx, e, d.Harness); err != nil {
			return err
		}
	}
	return nil
}
