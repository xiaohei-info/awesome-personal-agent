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
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 本模块的命令名。
const (
	CmdRegister     = "source.register"        // pull、webhook；delegate 可用
	CmdUpdate       = "source.update"          // pull、webhook
	CmdRegisterExec = "source.register_script" // script，只 Web
	CmdUpdateExec   = "source.update_script"   // script，只 Web
	CmdSetEnabled   = "source.set_enabled"     // 任意类型启停
)

// 事件类型是本模块内部的账本记录，不在对外稳定表里。
const (
	EvRegistered = "source.registered"
	EvUpdated    = "source.updated"
	EvEnabled    = "source.enabled"
	EvDisabled   = "source.disabled"
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
}}}

// Options 是登记所需的外部事实。
type Options struct {
	// Bundled 是随发行附带的拉取程序名；pull 的 command 必须在其中。
	Bundled []string
	// Layout 非零时，credentials_path 必须落在该来源自己的目录里（§9.1）。
	Layout config.Layout
}

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
	Type            domain.SourceType `json:"type,omitempty"`
	SourceKind      string            `json:"source_type,omitempty"`
	Command         string            `json:"command,omitempty"`
	Schedule        string            `json:"schedule,omitempty"`
	Timeout         int               `json:"timeout,omitempty"`
	CredentialsPath string            `json:"credentials_path,omitempty"`
	DedupeDomain    string            `json:"dedupe_domain,omitempty"`
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
	Config          *json.RawMessage `json:"config,omitempty"`
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
		s = domain.Source{ID: r.ID, Type: r.Type, SourceKind: r.SourceKind, Command: r.Command, Schedule: r.Schedule,
			Timeout: r.Timeout, CredentialsPath: r.CredentialsPath, DedupeDomain: r.DedupeDomain, Config: r.Config, Enabled: true}
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
		if u.Config != nil {
			s.Config = *u.Config
		}
	}
	if err := m.check(s, exec); err != nil {
		return s, "", err
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
			(id, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, config, cursor, enabled, stats)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, '')`,
			s.ID, string(s.Type), s.SourceKind, s.Command, s.Schedule, s.Timeout, s.CredentialsPath, s.DedupeDomain, cfg, b2i(s.Enabled))
		return err
	}
	_, err := tx.Exec(`UPDATE sources_registry_source SET source_type = ?, command = ?, schedule = ?, timeout = ?,
		credentials_path = ?, dedupe_domain = ?, config = ? WHERE id = ?`,
		s.SourceKind, s.Command, s.Schedule, s.Timeout, s.CredentialsPath, s.DedupeDomain, cfg, s.ID)
	return err
}

// ---- 读取（供调度、Ingest 等模块使用；只读） ----

const cols = `id, type, source_type, command, schedule, timeout, credentials_path, dedupe_domain, config, cursor, enabled, stats`

func scan(r *store.Rows) (domain.Source, error) {
	var (
		s                domain.Source
		typ, cfg, stats  string
		timeout, enabled int64
	)
	if err := r.Scan(&s.ID, &typ, &s.SourceKind, &s.Command, &s.Schedule, &timeout, &s.CredentialsPath,
		&s.DedupeDomain, &cfg, &s.Cursor, &enabled, &stats); err != nil {
		return s, err
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
		Timeout: &s.Timeout, CredentialsPath: &s.CredentialsPath, DedupeDomain: &s.DedupeDomain, Config: &cfg})
	if _, err := x.Do(ctx, command.Request{Identity: id, Command: upd, Input: u}); err != nil {
		return err
	}
	if s.Enabled == nil {
		return nil
	}
	e, _ := json.Marshal(enabledInput{ID: s.ID, Enabled: *s.Enabled})
	_, err = x.Do(ctx, command.Request{Identity: id, Command: CmdSetEnabled, Input: e})
	return err
}
