// Package registry 是 Source 登记（设计 §6.10、§9.1、§14.12）：来源的类型、source_type、周期、超时、
// 去重域和启停的持久化与增改命令。本包只登记，不调度、不执行、不读来源凭据（credentials_path 只是被记录的路径）。
//
// 权限：可执行命令的登记等于授予主机代码执行能力，只在 Web 或主机配置文件做（source.register_script /
// source.update_script，delegate 得到 web_only）；delegate 只能登记随发行附带的拉取程序（pull，命令须在附带清单内）。
// 游标与统计由调度模块维护，本包的命令写不了它们。
package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 本模块的命令名。
const (
	CmdRegister     = "source.register"        // pull、webhook；delegate 可用
	CmdUpdate       = "source.update"          // pull、webhook
	CmdRegisterExec = "source.register_script" // script，只 Web
	CmdUpdateExec   = "source.update_script"   // script，只 Web
	CmdSetEnabled   = "source.set_enabled"     // 任意类型启停
	CmdSetOutbound  = "source.set_outbound"    // 外发范围，只 Web（§14.14）
	CmdRename       = "source.rename"          // 显示名；空名表示改回内核生成的名字
)

// 事件类型是本模块内部的账本记录，不在对外稳定表里。
const (
	EvRegistered = "source.registered"
	EvUpdated    = "source.updated"
	EvEnabled    = "source.enabled"
	EvDisabled   = "source.disabled"
	EvRenamed    = "source.renamed" // 只是显示名变化，没有订阅方
)

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "sources_registry", Migrations: []store.Migration{{
	Version: 1, Name: "create-source",
	SQL: `CREATE TABLE sources_registry_source (
	id TEXT PRIMARY KEY,
	type TEXT NOT NULL,
	source_type TEXT NOT NULL,
	command TEXT NOT NULL,
	schedule TEXT NOT NULL,
	timeout INTEGER NOT NULL,
	credentials_path TEXT NOT NULL,
	dedupe_domain TEXT NOT NULL,
	config TEXT NOT NULL,
	cursor TEXT NOT NULL,
	enabled INTEGER NOT NULL,
	stats TEXT NOT NULL
) WITHOUT ROWID`,
}, {
	// 账本结构版本 3（§6.10）：默认项目与外发范围。external 缺省为空即“未选择”，v0.1 登记的来源升级后都是未选择（§14.14）。
	Version: 2, Ledger: 3, Name: "ledger-v3-source-scope",
	SQL: `ALTER TABLE sources_registry_source ADD COLUMN default_project TEXT NOT NULL DEFAULT '';
ALTER TABLE sources_registry_source ADD COLUMN outbound_local TEXT NOT NULL DEFAULT 'full';
ALTER TABLE sources_registry_source ADD COLUMN outbound_external TEXT NOT NULL DEFAULT '';`,
}, {
	// 账本结构版本 4（§16.5 决定 46：版本 3 已冻结，结构变更另起版本）。显示名（§9.1）：只加一列，
	// 旧来源读出为空名，由 apa serve 启动时的 Backfill 按同一套规则补上。
	Version: 3, Ledger: 4, Name: "ledger-v4-source-name",
	SQL: `ALTER TABLE sources_registry_source ADD COLUMN name TEXT NOT NULL DEFAULT '';`,
}}}

// Options 是登记所需的外部事实。
type Options struct {
	// Bundled 是随发行附带的拉取程序名；pull 的 command 必须在其中。
	Bundled []string
	// Layout 非零时，credentials_path 必须落在该来源自己的目录里（§9.1）。
	Layout config.Layout
	// Declared 返回来源程序（接入层）为这条登记声明的显示名，没有声明时返回空串（§9.1）。
	// 具体程序与产品的名字不进内核，由装配方给出；为 nil 等于没有任何声明。
	Declared func(domain.Source) string
}

// MaxName 是显示名的上限（字符数）。
const MaxName = 40

// typeNames 是升级补名时既没有接入层声明、也没有 source_type 显示名时按来源类型起的兜底名。
var typeNames = map[domain.SourceType]string{domain.SourcePull: "定时拉取", domain.SourceScript: "自定义脚本", domain.SourceWebhook: "外部推送"}

const (
	minSchedule = time.Second
	maxTimeout  = 24 * 60 * 60 // 秒
)

var (
	idRe   = regexp.MustCompile(`^src_[a-z0-9][a-z0-9_-]{0,59}$`)
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_.:-]{0,63}$`) // source_type、去重域
	progRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
)

// SourceInput 是登记与（来自主机配置文件的）整体覆盖的输入。
type SourceInput struct {
	ID              string            `json:"id"`
	Name            string            `json:"name,omitempty"` // 缺省由内核按 source_type 生成
	Type            domain.SourceType `json:"type,omitempty"`
	SourceKind      string            `json:"source_type,omitempty"`
	Command         string            `json:"command,omitempty"`
	Schedule        string            `json:"schedule,omitempty"`
	Timeout         int               `json:"timeout,omitempty"`
	CredentialsPath string            `json:"credentials_path,omitempty"`
	DedupeDomain    string            `json:"dedupe_domain,omitempty"`
	DefaultProject  string            `json:"default_project,omitempty"`
	Config          json.RawMessage   `json:"config,omitempty"`
	Enabled         *bool             `json:"enabled,omitempty"` // 缺省启用
}

// UpdateInput 只含可改字段；类型不可改，游标与统计不在其中（未知字段被拒绝）。
type UpdateInput struct {
	ID              string           `json:"id"`
	SourceKind      *string          `json:"source_type,omitempty"`
	Command         *string          `json:"command,omitempty"`
	Schedule        *string          `json:"schedule,omitempty"`
	Timeout         *int             `json:"timeout,omitempty"`
	CredentialsPath *string          `json:"credentials_path,omitempty"`
	DedupeDomain    *string          `json:"dedupe_domain,omitempty"`
	DefaultProject  *string          `json:"default_project,omitempty"` // 空串清除
	Config          *json.RawMessage `json:"config,omitempty"`
}

// OutboundInput 设置外发范围。Local 缺省保持；External 缺省保持，null 表示改回未选择。
type OutboundInput struct {
	ID       string          `json:"id"`
	Local    json.RawMessage `json:"local,omitempty"`
	External json.RawMessage `json:"external,omitempty"`
}

type renameInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

type enabledInput struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

func invalid(why, next string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, next)
}

func strict(in json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(in))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("bad input: "+err.Error(), "fix the request and retry")
	}
	if d.More() {
		return invalid("bad input: trailing data", "fix the request and retry")
	}
	return nil
}

// Register 把本模块的命令登记到 x。
func Register(x *command.Executor, o Options) error {
	m := &mod{o: o, bundled: map[string]bool{}}
	for _, b := range o.Bundled {
		if !progRe.MatchString(b) {
			return fmt.Errorf("registry: invalid bundled program name %q", b)
		}
		m.bundled[b] = true
	}
	specs := []command.Spec{
		m.spec(CmdRegister, authz.ActionManageSource, opRegister, false),
		m.spec(CmdUpdate, authz.ActionManageSource, opUpdate, false),
		// ActionChangeSettings：delegate 得 web_only（退出码 20 并留痕），worker/observer 得 4。
		m.spec(CmdRegisterExec, authz.ActionChangeSettings, opRegister, true),
		m.spec(CmdUpdateExec, authz.ActionChangeSettings, opUpdate, true),
		// ActionChangeSettings、不带代理写档位：外发范围只在 Web，delegate 得 web_only（退出码 20 并留痕）。
		{Name: CmdSetOutbound, Action: authz.ActionChangeSettings, Tier: authz.TierNone, Object: objectOf,
			Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
				_, err := planOutbound(tx, in)
				return err
			},
			Apply: m.applyOutbound},
		{Name: CmdSetEnabled, Action: authz.ActionManageSource, Tier: authz.TierWords, Object: objectOf,
			Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
				var e enabledInput
				if err := strict(in, &e); err != nil {
					return err
				}
				_, err := Get(tx, e.ID)
				return err
			},
			Apply: m.applyEnabled},
		// 与来源的其他管理命令同档：delegate 改名要带本人原话。
		{Name: CmdRename, Action: authz.ActionManageSource, Tier: authz.TierWords, Object: objectOf,
			Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
				_, _, err := m.planRename(tx, in)
				return err
			},
			Apply: m.applyRename},
	}
	for _, s := range specs {
		if err := x.Register(s); err != nil {
			return err
		}
	}
	return nil
}

type mod struct {
	o       Options
	bundled map[string]bool
}

type op int

const (
	opRegister op = iota
	opUpdate
)

func (m *mod) spec(name string, a authz.Action, o op, exec bool) command.Spec {
	tier := authz.TierWords // 登记可执行命令的是设置类（第三档），不是代理写
	if exec {
		tier = authz.TierNone
	}
	return command.Spec{Name: name, Action: a, Tier: tier, Object: objectOf,
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			_, _, err := m.plan(tx, o, exec, in)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			s, ev, err := m.plan(c.Tx, o, exec, c.Input)
			if err != nil {
				return nil, err
			}
			if err := save(c.Tx, s, o == opRegister); err != nil {
				return nil, err
			}
			if _, err := c.Emit(domain.Event{Type: ev, Object: s.ID, Payload: snapshot(s)}); err != nil {
				return nil, err
			}
			return s, nil
		}}
}

// objectOf 取 id 作授权对象；其余字段不影响授权，授权只看命令。
func objectOf(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var h struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(in, &h); err != nil {
		return authz.Object{}, invalid("bad input: "+err.Error(), "fix the request and retry")
	}
	return authz.Object{ID: h.ID}, nil
}

// plan 解出输入、套上现有记录并整体校验，返回将要落盘的 Source 与事件类型。
func (m *mod) plan(tx *store.Tx, o op, exec bool, in json.RawMessage) (domain.Source, string, error) {
	var s domain.Source
	if o == opRegister {
		var r SourceInput
		if err := strict(in, &r); err != nil {
			return s, "", err
		}
		if !idRe.MatchString(r.ID) {
			return s, "", invalid("id must look like src_<lowercase name>", "pick another id")
		}
		if _, err := Get(tx, r.ID); err == nil {
			return s, "", domain.NewError(domain.CodeConflict, "source "+r.ID+" already exists", "use the update command")
		} else if !isNotFound(err) {
			return s, "", err
		}
		name, err := cleanName(r.Name)
		if err != nil {
			return s, "", err
		}
		s = domain.Source{ID: r.ID, Name: name, Type: r.Type, SourceKind: r.SourceKind, Command: r.Command, Schedule: r.Schedule,
			Timeout: r.Timeout, CredentialsPath: r.CredentialsPath, DedupeDomain: r.DedupeDomain, DefaultProject: r.DefaultProject,
			Outbound: domain.Outbound{Local: ScopeFull}, Config: r.Config, Enabled: true}
		if r.Enabled != nil {
			s.Enabled = *r.Enabled
		}
		if exec && s.Type == "" {
			s.Type = domain.SourceScript
		}
	} else {
		var u UpdateInput
		if err := strict(in, &u); err != nil {
			return s, "", err
		}
		cur, err := Get(tx, u.ID)
		if err != nil {
			return s, "", err
		}
		s = cur
		if u.SourceKind != nil {
			s.SourceKind = *u.SourceKind
		}
		if u.Command != nil {
			s.Command = *u.Command
		}
		if u.Schedule != nil {
			s.Schedule = *u.Schedule
		}
		if u.Timeout != nil {
			s.Timeout = *u.Timeout
		}
		if u.CredentialsPath != nil {
			s.CredentialsPath = *u.CredentialsPath
		}
		if u.DedupeDomain != nil {
			s.DedupeDomain = *u.DedupeDomain
		}
		if u.DefaultProject != nil {
			s.DefaultProject = *u.DefaultProject
		}
		if u.Config != nil {
			s.Config = *u.Config
		}
	}
	if err := m.check(s, exec); err != nil {
		return s, "", err
	}
	if o == opRegister {
		if s.Name == "" {
			n, err := m.declaredName(s)
			if err != nil {
				return s, "", err
			}
			s.Name = n
		}
		if err := nameFree(tx, s.Name, s.ID); err != nil {
			return s, "", err
		}
	}
	if s.DefaultProject != "" {
		if _, err := projects.Get(tx, s.DefaultProject); err != nil {
			return s, "", err
		}
	}
	if o == opRegister {
		return s, EvRegistered, nil
	}
	return s, EvUpdated, nil
}

// check 校验一条完整的登记。exec 表示走的是只 Web 的命令。
func (m *mod) check(s domain.Source, exec bool) error {
	isScript := s.Type == domain.SourceScript
	if isScript != exec {
		if isScript {
			return invalid("script sources run an arbitrary command and can only be handled by "+CmdRegisterExec+" / "+CmdUpdateExec+" (Web)", "ask the owner to do it in the Web UI")
		}
		return invalid("only script sources are handled by this command", "use "+CmdRegister+" / "+CmdUpdate)
	}
	switch s.Type {
	case domain.SourcePull:
		if !m.bundled[s.Command] {
			return invalid("a pull source must run a program bundled with the release", "use a bundled program, or ask the owner to register a script source in the Web UI")
		}
	case domain.SourceScript:
		if strings.TrimSpace(s.Command) == "" {
			return invalid("a script source needs a command", "set command")
		}
	case domain.SourceWebhook:
		if s.Command != "" || s.Schedule != "" || s.Timeout != 0 {
			return invalid("a webhook source has no command, schedule or timeout", "remove those fields")
		}
	default:
		return invalid(fmt.Sprintf("unknown source type %q", s.Type), "use pull, script or webhook")
	}
	if s.Type != domain.SourceWebhook {
		if d, err := time.ParseDuration(s.Schedule); err != nil || d < minSchedule {
			return invalid("schedule must be a duration of at least 1s, e.g. 15m", "fix schedule")
		}
		if s.Timeout < 1 || s.Timeout > maxTimeout {
			return invalid("timeout must be 1..86400 seconds", "fix timeout")
		}
	}
	if s.SourceKind != "" && !nameRe.MatchString(s.SourceKind) {
		return invalid("source_type must be a short lowercase label", "fix source_type")
	}
	if s.DedupeDomain != "" && !nameRe.MatchString(s.DedupeDomain) {
		return invalid("dedupe_domain must be a short lowercase label", "fix dedupe_domain")
	}
	if len(s.Config) > 0 {
		var obj map[string]any
		if err := json.Unmarshal(s.Config, &obj); err != nil || obj == nil {
			return invalid("config must be a JSON object", "fix config")
		}
	}
	return m.checkCredentials(s)
}

func (m *mod) checkCredentials(s domain.Source) error {
	if s.CredentialsPath == "" || m.o.Layout.Sources == "" {
		return nil
	}
	dir := m.o.Layout.SourceDir(s.ID)
	p := filepath.Clean(s.CredentialsPath)
	if p != dir && !strings.HasPrefix(p, dir+string(filepath.Separator)) {
		return invalid("credentials_path must be inside the source's own directory", "use a path under "+dir)
	}
	return nil
}

// cleanName 去掉首尾空白并校验显示名：不超过 MaxName 个字符、不含控制字符。空串表示没给名字。
func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > MaxName {
		return "", invalid(fmt.Sprintf("name must be at most %d characters", MaxName), "shorten the name")
	}
	if strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return "", invalid("name must not contain control characters", "fix the name")
	}
	return name, nil
}

// nameFree 报告显示名是否没被 except 以外的来源占用（不区分大小写）；占用时返回 conflict。
func nameFree(tx *store.Tx, name, except string) error {
	taken, err := takenNames(tx, except)
	if err != nil {
		return err
	}
	if taken[strings.ToLower(name)] {
		return domain.NewError(domain.CodeConflict, "another source is already named "+strconv.Quote(name), "pick another name")
	}
	return nil
}

func takenNames(tx *store.Tx, except string) (map[string]bool, error) {
	r, err := tx.Query(`SELECT name FROM sources_registry_source WHERE id != ? AND name != ''`, except)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]bool{}
	for r.Next() {
		var n string
		if err := r.Scan(&n); err != nil {
			return nil, err
		}
		out[strings.ToLower(n)] = true
	}
	return out, r.Err()
}

// declaredName 是登记没给名字时接入层声明的名字；没有声明时拒绝登记（§9.1：每个来源接入都必须有名字）。
func (m *mod) declaredName(s domain.Source) (string, error) {
	var n string
	if m.o.Declared != nil {
		n = m.o.Declared(s)
	}
	n, err := cleanName(n)
	if err != nil {
		return "", err
	}
	if n == "" {
		return "", invalid("source "+s.ID+" has no display name and its source program declares none",
			"give a name (Web name field, apa source add --name, the name field in sources.json), or declare one in the program's source.yaml")
	}
	return n, nil
}

// fallbackName 只用于升级补名：没有接入层声明的旧来源按 source_type 的显示名（没有则按来源类型）起名，
// 已被占用时依次加序号「邮件 2」「邮件 3」……
func fallbackName(tx *store.Tx, kindNames map[string]string, s domain.Source) (string, error) {
	base := kindNames[s.SourceKind]
	if base == "" {
		base = typeNames[s.Type]
	}
	return numbered(tx, base, s.ID)
}

// numbered 返回 base，被 except 以外的来源占用时依次加序号。
func numbered(tx *store.Tx, base, except string) (string, error) {
	taken, err := takenNames(tx, except)
	if err != nil {
		return "", err
	}
	name := base
	for i := 2; taken[strings.ToLower(name)]; i++ {
		name = fmt.Sprintf("%s %d", base, i)
	}
	return name, nil
}

// planRename 解出改名输入并校验；空名表示改回接入层声明的名字（没有声明时拒绝）。返回改名后的登记与原名。
func (m *mod) planRename(tx *store.Tx, in json.RawMessage) (domain.Source, string, error) {
	var ri renameInput
	if err := strict(in, &ri); err != nil {
		return domain.Source{}, "", err
	}
	s, err := Get(tx, ri.ID)
	if err != nil {
		return s, "", err
	}
	prev := s.Name
	name, err := cleanName(ri.Name)
	if err != nil {
		return s, prev, err
	}
	if name == "" {
		if name, err = m.declaredName(s); err != nil {
			return s, prev, err
		}
	}
	if err := nameFree(tx, name, s.ID); err != nil {
		return s, prev, err
	}
	s.Name = name
	return s, prev, nil
}

func (m *mod) applyRename(c *command.Ctx) (any, error) {
	s, prev, err := m.planRename(c.Tx, c.Input)
	if err != nil {
		return nil, err
	}
	if s.Name == prev { // 未变不记事件（幂等）
		return s, nil
	}
	if _, err := c.Tx.Exec(`UPDATE sources_registry_source SET name = ? WHERE id = ?`, s.Name, s.ID); err != nil {
		return nil, err
	}
	payload, _ := json.Marshal(map[string]string{"name": s.Name, "previous": prev})
	if _, err := c.Emit(domain.Event{Type: EvRenamed, Object: s.ID, Payload: payload}); err != nil {
		return nil, err
	}
	return s, nil
}

// namer 是启动补名的行为者：系统按规则补上，不是本人改名。
var namer = domain.Actor{Type: domain.ActorSystem, Principal: "kernel:sources", Via: domain.ViaClock}

// NamesObject 是升级补名留下的健康说明的对象：有来源只能按类型兜底起名时记一条，建议本人起名。
const NamesObject = "sources:names"

// Backfill 给还没有显示名的来源（升级前登记的）起名并存下，每个记一条 source.renamed（行为者 kernel:sources）：
// 先用接入层声明的名字（declared 同 Options.Declared），取不到的为了兼容按 kindNames（source_type → 名字，装配方给出）
// 或来源类型兜底，名字被占用时加序号；有兜底的另记一条健康说明（对象 NamesObject），建议本人起名。这是唯一保留兜底的地方。
// 只写名字列，不碰游标、统计、外发范围，也不经命令管线、不进入判断或投递。apa serve 启动时调用。
func Backfill(ctx context.Context, st *store.Store, declared func(domain.Source) string, kindNames map[string]string, at time.Time) (int, error) {
	n := 0
	err := st.Update(ctx, func(tx *store.Tx) error {
		n = 0
		srcs, err := List(tx)
		if err != nil {
			return err
		}
		var guessed []string
		for _, s := range srcs { // 按 id 顺序：同名的序号在补名时按 id 排
			if s.Name != "" {
				continue
			}
			var name string
			if declared != nil {
				name, _ = cleanName(declared(s)) // 声明不合法时按没有声明处理，不挡启动
			}
			if name != "" {
				name, err = numbered(tx, name, s.ID)
			} else {
				guessed = append(guessed, s.ID)
				name, err = fallbackName(tx, kindNames, s)
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE sources_registry_source SET name = ? WHERE id = ?`, name, s.ID); err != nil {
				return err
			}
			payload, _ := json.Marshal(map[string]string{"name": name, "previous": ""})
			if _, err := eventlog.Append(tx, domain.Event{Type: EvRenamed, Object: s.ID, Actor: namer, Payload: payload,
				Reason: "upgrade: sources registered before display names get one", At: at}); err != nil {
				return err
			}
			n++
		}
		if len(guessed) == 0 {
			return nil
		}
		p, _ := json.Marshal(map[string]any{"kind": "source_names", "sources": guessed})
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: NamesObject, Actor: namer, Payload: p, At: at,
			Reason: fmt.Sprintf("upgrade: %d source(s) had no declared display name and were named by type (%s); give them a name on the sources page", len(guessed), strings.Join(guessed, ", "))})
		return err
	})
	return n, err
}

// planOutbound 解出外发范围的设置并套在现有登记上；取值非法返回 invalid_request。
func planOutbound(tx *store.Tx, in json.RawMessage) (domain.Source, error) {
	var o OutboundInput
	if err := strict(in, &o); err != nil {
		return domain.Source{}, err
	}
	s, err := Get(tx, o.ID)
	if err != nil {
		return s, err
	}
	if o.Local != nil {
		var l string
		if json.Unmarshal(o.Local, &l) != nil || !ValidScope(l) {
			return s, invalid("outbound.local must be full, excerpt, meta or none", "fix local")
		}
		s.Outbound.Local = l
	}
	if o.External != nil {
		var ext *string
		if err := json.Unmarshal(o.External, &ext); err != nil || (ext != nil && !ValidScope(*ext)) {
			return s, invalid("outbound.external must be full, excerpt, meta, none or null", "fix external")
		}
		s.Outbound.External = ext
	}
	return s, nil
}

func (m *mod) applyOutbound(c *command.Ctx) (any, error) {
	s, err := planOutbound(c.Tx, c.Input)
	if err != nil {
		return nil, err
	}
	cur, _ := Get(c.Tx, s.ID)
	if sameOutbound(cur.Outbound, s.Outbound) { // 未变不记事件（幂等）
		return s, nil
	}
	if _, err := c.Tx.Exec(`UPDATE sources_registry_source SET outbound_local = ?, outbound_external = ? WHERE id = ?`,
		s.Outbound.Local, derefOr(s.Outbound.External, ""), s.ID); err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvUpdated, Object: s.ID, Payload: snapshot(s)}); err != nil {
		return nil, err
	}
	return s, nil
}

func sameOutbound(a, b domain.Outbound) bool {
	return a.Local == b.Local && derefOr(a.External, "-") == derefOr(b.External, "-")
}

func derefOr(p *string, d string) string {
	if p == nil {
		return d
	}
	return *p
}

func (m *mod) applyEnabled(c *command.Ctx) (any, error) {
	var e enabledInput
	if err := strict(c.Input, &e); err != nil {
		return nil, err
	}
	return SetEnabled(c, e.ID, e.Enabled)
}

// SetEnabled 在命令上下文里启停来源并记事件；状态未变不记事件（幂等）。
// 供调度模块在自己的事务里停用永久失败的来源，与它的记账一起提交。
func SetEnabled(c *command.Ctx, id string, enabled bool) (domain.Source, error) {
	s, err := Get(c.Tx, id)
	if err != nil {
		return s, err
	}
	if s.Enabled == enabled {
		return s, nil
	}
	s.Enabled = enabled
	if _, err := c.Tx.Exec(`UPDATE sources_registry_source SET enabled = ? WHERE id = ?`, b2i(s.Enabled), s.ID); err != nil {
		return s, err
	}
	ev := EvDisabled
	if s.Enabled {
		ev = EvEnabled
	}
	_, err = c.Emit(domain.Event{Type: ev, Object: s.ID, Payload: snapshot(s)})
	return s, err
}

// snapshot 是事件载荷：登记内容，不含调度模块维护的游标与统计。
func snapshot(s domain.Source) json.RawMessage {
	s.Cursor, s.Stats = "", nil
	b, err := json.Marshal(s)
	if err != nil {
		panic(err) // 固定结构
	}
	return b
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func save(tx *store.Tx, s domain.Source, insert bool) error {
	cfg := string(s.Config)
	if insert {
		_, err := tx.Exec(`INSERT INTO sources_registry_source
			(id, name, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, default_project, outbound_local, outbound_external, config, cursor, enabled, stats)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, '')`,
			s.ID, s.Name, string(s.Type), s.SourceKind, s.Command, s.Schedule, s.Timeout, s.CredentialsPath, s.DedupeDomain,
			s.DefaultProject, s.Outbound.Local, derefOr(s.Outbound.External, ""), cfg, b2i(s.Enabled))
		return err
	}
	_, err := tx.Exec(`UPDATE sources_registry_source SET source_type = ?, command = ?, schedule = ?, timeout = ?,
		credentials_path = ?, dedupe_domain = ?, default_project = ?, config = ? WHERE id = ?`,
		s.SourceKind, s.Command, s.Schedule, s.Timeout, s.CredentialsPath, s.DedupeDomain, s.DefaultProject, cfg, s.ID)
	return err
}

// ---- 读取（供调度、Ingest 等模块使用；只读） ----

const cols = `id, name, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, default_project, outbound_local, outbound_external, config, cursor, enabled, stats`

func scan(r *store.Rows) (domain.Source, error) {
	var (
		s                domain.Source
		typ, cfg, stats  string
		ext              string
		timeout, enabled int64
	)
	if err := r.Scan(&s.ID, &s.Name, &typ, &s.SourceKind, &s.Command, &s.Schedule, &timeout, &s.CredentialsPath,
		&s.DedupeDomain, &s.DefaultProject, &s.Outbound.Local, &ext, &cfg, &s.Cursor, &enabled, &stats); err != nil {
		return s, err
	}
	if ext != "" { // 空串即未选择
		s.Outbound.External = &ext
	}
	s.Type, s.Timeout, s.Enabled = domain.SourceType(typ), int(timeout), enabled != 0
	if cfg != "" {
		s.Config = json.RawMessage(cfg)
	}
	if stats != "" {
		s.Stats = json.RawMessage(stats)
	}
	return s, nil
}

func isNotFound(err error) bool {
	de, ok := err.(*domain.Error)
	return ok && de.Code == domain.CodeNotFound
}

// Get 读取一条登记；不存在返回 not_found。
func Get(tx *store.Tx, id string) (domain.Source, error) {
	r, err := tx.Query(`SELECT `+cols+` FROM sources_registry_source WHERE id = ?`, id)
	if err != nil {
		return domain.Source{}, err
	}
	defer r.Close()
	if !r.Next() {
		if err := r.Err(); err != nil {
			return domain.Source{}, err
		}
		return domain.Source{}, domain.NewError(domain.CodeNotFound, "source "+id+" not found", "list sources and retry")
	}
	return scan(r)
}

// SetRunState 在调用方事务内写游标与统计。这两列只由调度模块（kernel/sources/runner）经本函数维护，
// 本包的命令写不了它们；不记事件，调用方自己负责留痕。
func SetRunState(tx *store.Tx, id, cursor string, stats json.RawMessage) error {
	n, err := tx.Exec(`UPDATE sources_registry_source SET cursor = ?, stats = ? WHERE id = ?`, cursor, string(stats), id)
	if err == nil && n == 0 {
		return domain.NewError(domain.CodeNotFound, "source "+id+" not found", "list sources and retry")
	}
	return err
}

// List 按 id 升序返回全部登记。
func List(tx *store.Tx) ([]domain.Source, error) {
	r, err := tx.Query(`SELECT ` + cols + ` FROM sources_registry_source ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []domain.Source
	for r.Next() {
		s, err := scan(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, r.Err()
}

// ---- 主机配置文件（§14.12） ----

// LoadFile 读取主机上的配置文件 {"sources":[…]} 并逐条登记或覆盖。id 用调用方给的身份
// （启动流程代表本人在主机上，应传 Web 级 Trusted）；任何一条失败即返回，已生效的条目不回滚。
func LoadFile(ctx context.Context, x *command.Executor, id command.Identity, path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var f struct {
		Sources []SourceInput `json:"sources"`
	}
	if err := strict(data, &f); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	for _, s := range f.Sources {
		if err := apply(ctx, x, id, s); err != nil {
			return fmt.Errorf("%s: source %q: %w", path, s.ID, err)
		}
	}
	return nil
}

func apply(ctx context.Context, x *command.Executor, id command.Identity, s SourceInput) error {
	exec := s.Type == domain.SourceScript || s.Type == ""
	reg, upd := CmdRegister, CmdUpdate
	if exec {
		reg, upd = CmdRegisterExec, CmdUpdateExec
	}
	body, _ := json.Marshal(s)
	_, err := x.Do(ctx, command.Request{Identity: id, Command: reg, Input: body})
	if de, ok := err.(*domain.Error); !ok || de.Code != domain.CodeConflict {
		return err
	}
	cfg := s.Config
	u, _ := json.Marshal(UpdateInput{ID: s.ID, SourceKind: &s.SourceKind, Command: &s.Command, Schedule: &s.Schedule,
		Timeout: &s.Timeout, CredentialsPath: &s.CredentialsPath, DedupeDomain: &s.DedupeDomain, DefaultProject: &s.DefaultProject, Config: &cfg})
	if _, uerr := x.Do(ctx, command.Request{Identity: id, Command: upd, Input: u}); uerr != nil {
		if isNotFound(uerr) { // 不是 id 已存在，是登记本身冲突（如显示名重名）：报登记的原因
			return err
		}
		return uerr
	}
	if s.Name != "" { // 文件里改了名字：与 Web 改名同一条规则
		n, _ := json.Marshal(renameInput{ID: s.ID, Name: s.Name})
		if _, err := x.Do(ctx, command.Request{Identity: id, Command: CmdRename, Input: n}); err != nil {
			return err
		}
	}
	if s.Enabled == nil {
		return nil
	}
	e, _ := json.Marshal(enabledInput{ID: s.ID, Enabled: *s.Enabled})
	_, err = x.Do(ctx, command.Request{Identity: id, Command: CmdSetEnabled, Input: e})
	return err
}
