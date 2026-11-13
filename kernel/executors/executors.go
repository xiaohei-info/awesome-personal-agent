// Package executors 是执行者清单与无头运行声明的唯一写入模块（设计 §6.13、§7.5、§14.12、附录 A.2）。
//
// 清单是声明式数据：本人在 Web 维护，插件经 declare 提交声明后进入待确认，确认前不生效；
// 本人也可以直接用主机配置文件登记。内核只把声明当数据存取，不执行其中任何命令，
// 也不保存"执行者 id → 调用方式"的映射（那在各 harness 插件里）。
//
// 声明文档用 JSON（YAML 的子集）。shortcut: 仓库没有 YAML 依赖，需要 YAML 文件时在 CLI 侧转换。
package executors

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 执行者种类（§6.13）。
const (
	KindNativeSubagent = "native-subagent"
	KindExternalCLI    = "external-cli"
	KindHuman          = "human"
)

// 声明状态：待确认 → 生效；被本人拒绝；被同 harness 的新声明取代。
const (
	StatePending    = "pending"
	StateActive     = "active"
	StateRejected   = "rejected"
	StateSuperseded = "superseded"
)

// Executor 是清单的一项。Host 只是说明字段，内核不校验进程实际所在。
type Executor struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Scenarios []string `json:"scenarios,omitempty"`
	Host      string   `json:"host,omitempty"`
}

// Session 的 Assign 与 Extract 二选一。
type Session struct {
	Assign  []string `json:"assign,omitempty"`
	Extract string   `json:"extract,omitempty"`
}

// Headless 是无头运行声明（A.2）。Command 是命令模板，只作数据保存。
type Headless struct {
	Command    []string            `json:"command"`
	Output     []string            `json:"output,omitempty"`
	Session    Session             `json:"session"`
	Resume     []string            `json:"resume,omitempty"`
	Access     map[string][]string `json:"access"`
	Credential string              `json:"credential,omitempty"`
	ExitCodes  map[string][]int    `json:"exit_codes,omitempty"`
	// ExitOutput 收窄 exit_codes 的同名语义：声明后，还要求 stdout 或 stderr 含其中任一片段（附录 A.2）。
	ExitOutput map[string][]string `json:"exit_output,omitempty"`
}

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

type MainSession struct {
	Launch string   `json:"launch,omitempty"`
	Args   []string `json:"args,omitempty"`
}

// Declaration 是 `apa declare` 提交或主机配置文件登记的整份声明。
type Declaration struct {
	Harness     string       `json:"harness"`
	Headless    *Headless    `json:"headless,omitempty"`
	MainSession *MainSession `json:"main_session,omitempty"`
	Executors   []Executor   `json:"executors,omitempty"`
	Selfcheck   *Selfcheck   `json:"selfcheck,omitempty"`
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
	return d, d.validate()
}

func (d Declaration) validate() error {
	if d.Harness == "" || strings.ContainsAny(d.Harness, " \t\r\n") {
		return invalid("harness is required and must not contain whitespace")
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
	switch e.Kind {
	case KindNativeSubagent, KindExternalCLI, KindHuman:
	default:
		return invalid("executor %q: kind must be native-subagent, external-cli or human", e.ID)
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
	if len(h.Access) == 0 {
		return invalid("headless.access must declare at least one of read, edit, full")
	}
	for tier := range h.Access {
		if !accessTiers[tier] {
			return invalid("headless.access tier %q is not read, edit or full", tier)
		}
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

func scanEntry(r *store.Rows) (Executor, string, error) {
	var e Executor
	var scen, harness string
	if err := r.Scan(&e.ID, &e.Kind, &scen, &e.Host, &harness); err != nil {
		return e, "", err
	}
	return e, harness, json.Unmarshal([]byte(scen), &e.Scenarios)
}

// List 返回生效的执行者清单，按 id 排序。
func List(tx *store.Tx) ([]Executor, error) {
	out := []Executor{}
	err := query(tx, func(r *store.Rows) error {
		e, _, err := scanEntry(r)
		out = append(out, e)
		return err
	}, "SELECT id, kind, scenarios, host, harness FROM executors_entry ORDER BY id")
	return out, err
}

// Get 返回生效清单里的一项；ok 为假表示不在清单里（委派据此返回退出码 5）。
func Get(tx *store.Tx, id string) (e Executor, ok bool, err error) {
	err = query(tx, func(r *store.Rows) (err error) {
		e, _, err = scanEntry(r)
		ok = err == nil
		return
	}, "SELECT id, kind, scenarios, host, harness FROM executors_entry WHERE id = ?", id)
	return
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
	err = query(tx, func(r *store.Rows) (err error) {
		e, harness, err = scanEntry(r)
		ok = err == nil
		return
	}, "SELECT id, kind, scenarios, host, harness FROM executors_entry WHERE id = ?", id)
	return
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

// activate 让 d 生效：该 harness 原有的声明项整体换成 d 里的项。调用方先做冲突检查。
func activate(tx *store.Tx, d Declaration) error {
	if _, err := tx.Exec("DELETE FROM executors_entry WHERE harness = ?", d.Harness); err != nil {
		return err
	}
	for _, e := range d.Executors {
		if err := putEntry(tx, e, d.Harness); err != nil {
			return err
		}
	}
	return nil
}
