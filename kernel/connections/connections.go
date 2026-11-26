// Package connections 是连接登记（设计 §1.5、§7.6）：本人配好的渠道实例——名字、配置、启停、凭据与现状。
// 本包只登记与读，不发送、不调用插件；凭据经 kernel/secrets 加密保管，配置与凭据在同一事务里保存和删除。
//
// 权限：全部命令只在 Web（与改策略同级，ActionChangeSettings：delegate 得 web_only 退出码 20，其他凭据得 4）。
// Event 载荷不含凭据值，只列改了哪些凭据字段的名字。现状（connections_status）只存最近一次，覆盖写、不记 Event。
// 来源不在这里：来源登记留在 kernel/sources/registry（§1.2 不合表）。
package connections

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/secrets"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 命令名（附录 B.1 设置类，只在 Web）。
const (
	CmdCreate    = "connection.create"
	CmdConfigure = "connection.configure"
	CmdEnable    = "connection.enable"
	CmdDisable   = "connection.disable"
	CmdRename    = "connection.rename"
	CmdRemove    = "connection.remove"
)

// 事件类型是本模块内部的账本记录，暂不进订阅流（§8）。
const (
	EvCreated    = "connection.created"
	EvConfigured = "connection.configured"
	EvEnabled    = "connection.enabled"
	EvDisabled   = "connection.disabled"
	EvRenamed    = "connection.renamed"
	EvRemoved    = "connection.removed"
)

// 卡片状态（读接口的 state）。
const (
	StateOK       = "ok"
	StateProblem  = "problem"
	StateDisabled = "disabled"
)

// IDPrefix 是渠道连接 id 的前缀：投递路由 channel:<id>；签发渠道 ext 密钥时拒绝用这个前缀的名字（§8）。
const IDPrefix = "ch-"

// MaxName 是名字的上限（字符数），与来源一致。
const MaxName = 40

// Module 登记连接表与现状表（账本结构版本 7，只增表）。
var Module = store.Module{Name: "connections", Migrations: []store.Migration{{
	Version: 1, Ledger: 7, Name: "ledger-v7-connections",
	SQL: `CREATE TABLE connections_connection (
    id TEXT PRIMARY KEY,
    direction TEXT NOT NULL,
    type TEXT NOT NULL,
    name TEXT NOT NULL,
    config TEXT NOT NULL,
    enabled INTEGER NOT NULL,
    created_at INTEGER NOT NULL,
    updated_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE UNIQUE INDEX connections_connection_name ON connections_connection(name);
CREATE TABLE connections_status (
    connection TEXT PRIMARY KEY,
    problem TEXT NOT NULL DEFAULT '',
    detail TEXT NOT NULL DEFAULT '',
    last_ok_at INTEGER NOT NULL DEFAULT 0,
    last_error_at INTEGER NOT NULL DEFAULT 0,
    cursor TEXT NOT NULL DEFAULT '',
    counters TEXT NOT NULL DEFAULT '{}'
) WITHOUT ROWID;`,
}}}

// Connection 是读接口的形状。凭据只回是否已设置且可用，不回值、不回末几位（§5.3）。
type Connection struct {
	ID          string            `json:"id"`
	Direction   string            `json:"direction"`
	Type        string            `json:"type"`
	Name        string            `json:"name"`
	Config      map[string]string `json:"config"`
	Secrets     map[string]bool   `json:"secrets"`
	Enabled     bool              `json:"enabled"`
	State       string            `json:"state"`
	Problem     string            `json:"problem,omitempty"`
	Detail      string            `json:"detail,omitempty"`
	LastOKAt    *time.Time        `json:"last_ok_at,omitempty"`
	LastErrorAt *time.Time        `json:"last_error_at,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// Service 持有凭据保管库与接入声明；无状态，所有状态在账本里。
type Service struct {
	v    *secrets.Vault
	decl func(typ string) (conn.Decl, bool)
}

// New 返回连接服务；decl 由装配方给出（声明在 connectors/，内核不 import）。
func New(v *secrets.Vault, decl func(string) (conn.Decl, bool)) *Service {
	return &Service{v: v, decl: decl}
}

// Vault 返回凭据保管库（自检用）。
func (s *Service) Vault() *secrets.Vault { return s.v }

func invalid(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

func notFound(id string) error {
	return domain.NewError(domain.CodeNotFound, "connection "+id+" not found", "reload the settings page")
}

func strict(in json.RawMessage, v any) error {
	d := json.NewDecoder(bytes.NewReader(in))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return invalid("bad input: "+err.Error(), "fix the request and retry")
	}
	return nil
}

type createInput struct {
	Type    string            `json:"type"`
	Name    string            `json:"name,omitempty"`
	Config  map[string]string `json:"config,omitempty"`
	Secrets map[string]string `json:"secrets,omitempty"`
	Enabled bool              `json:"enabled,omitempty"`
}

// configureInput：config 缺省保持；给出时整体替换非凭据配置。secrets 里出现的键：非空替换、空串清除，没出现的保持。
type configureInput struct {
	ID      string             `json:"id"`
	Config  *map[string]string `json:"config,omitempty"`
	Secrets map[string]string  `json:"secrets,omitempty"`
}

type idInput struct {
	ID string `json:"id"`
}

type renameInput struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Register 登记六条命令。
func (s *Service) Register(x *command.Executor) error {
	spec := func(name string, apply func(*command.Ctx) (any, error)) command.Spec {
		return command.Spec{Name: name, Action: authz.ActionChangeSettings, Group: authz.GroupChannels, Page: "/settings/channels", Object: objectOf, Apply: apply}
	}
	for _, sp := range []command.Spec{
		spec(CmdCreate, s.applyCreate), spec(CmdConfigure, s.applyConfigure),
		spec(CmdEnable, func(c *command.Ctx) (any, error) { return s.applyEnabled(c, true) }),
		spec(CmdDisable, func(c *command.Ctx) (any, error) { return s.applyEnabled(c, false) }),
		spec(CmdRename, s.applyRename), spec(CmdRemove, s.applyRemove),
	} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

func objectOf(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var h idInput
	if err := json.Unmarshal(in, &h); err != nil {
		return authz.Object{}, invalid("bad input: "+err.Error(), "fix the request and retry")
	}
	return authz.Object{ID: h.ID}, nil
}

func cleanName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > MaxName || strings.ContainsAny(name, "\n\r\t") {
		return "", invalid("name must be 1 to 40 characters on one line", "choose another name")
	}
	return name, nil
}

func nameFree(tx *store.Tx, name, except string) error {
	var id string
	r, err := tx.Query(`SELECT id FROM connections_connection WHERE name = ? AND id != ?`, name, except)
	if err != nil {
		return err
	}
	defer r.Close()
	if r.Next() {
		if err := r.Scan(&id); err != nil {
			return err
		}
		return domain.NewError(domain.CodeConflict, "another connection is already called "+name, "choose another name")
	}
	return r.Err()
}

func newID(tx *store.Tx) (string, error) {
	for {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		id := IDPrefix + hex.EncodeToString(b)
		if _, err := get(tx, id); isNotFound(err) {
			return id, nil
		} else if err != nil {
			return "", err
		}
	}
}

func fieldErr(err error) error {
	if fe, ok := err.(*conn.FieldError); ok {
		return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the field "+fe.Field)
	}
	return err
}

// planSecrets 校验输入里的凭据键都是声明过的凭据字段，返回要改的字段名（排序）。
func planSecrets(d conn.Decl, in map[string]string) ([]string, error) {
	keys := map[string]bool{}
	for _, k := range d.SecretKeys() {
		keys[k] = true
	}
	var changed []string
	for k, v := range in {
		if !keys[k] {
			return nil, invalid("field "+k+" is not a credential of this connection type", "fix the request and retry")
		}
		if len(v) > 4096 {
			return nil, invalid("credential "+k+" is too long", "check the value")
		}
		changed = append(changed, k)
	}
	sort.Strings(changed)
	return changed, nil
}

func (s *Service) writeSecrets(c *command.Ctx, id string, in map[string]string, keys []string) error {
	for _, k := range keys {
		var err error
		if in[k] == "" {
			err = secrets.Delete(c.Tx, id, k)
		} else {
			err = s.v.Put(c.Tx, id, k, in[k], c.Now())
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func payload(r record, changed []string) json.RawMessage {
	b, err := json.Marshal(map[string]any{"id": r.ID, "direction": r.Direction, "type": r.Type, "name": r.Name,
		"config": r.Config, "enabled": r.Enabled, "secrets_changed": changed})
	if err != nil {
		panic(err) // 固定结构
	}
	return b
}

func (s *Service) applyCreate(c *command.Ctx) (any, error) {
	var in createInput
	if err := strict(c.Input, &in); err != nil {
		return nil, err
	}
	d, ok := s.decl(in.Type)
	if !ok {
		return nil, invalid("unknown connection type "+in.Type, "choose a type from GET /connections/types")
	}
	changed, err := planSecrets(d, in.Secrets)
	if err != nil {
		return nil, err
	}
	cfg, err := d.Normalize(in.Config, func(k string) bool { return in.Secrets[k] != "" })
	if err != nil {
		return nil, fieldErr(err)
	}
	if in.Name == "" {
		in.Name = d.Default.Zh
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return nil, err
	}
	if err := nameFree(c.Tx, name, ""); err != nil {
		return nil, err
	}
	id, err := newID(c.Tx)
	if err != nil {
		return nil, err
	}
	r := record{ID: id, Direction: d.Direction, Type: d.Type, Name: name, Config: cfg, Enabled: in.Enabled, CreatedAt: c.Now(), UpdatedAt: c.Now()}
	if err := insert(c.Tx, r); err != nil {
		return nil, err
	}
	if err := s.writeSecrets(c, id, in.Secrets, changed); err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvCreated, Object: id, Payload: payload(r, changed)}); err != nil {
		return nil, err
	}
	return s.view(c.Tx, r)
}

func (s *Service) applyConfigure(c *command.Ctx) (any, error) {
	var in configureInput
	if err := strict(c.Input, &in); err != nil {
		return nil, err
	}
	r, err := get(c.Tx, in.ID)
	if err != nil {
		return nil, err
	}
	d, ok := s.decl(r.Type)
	if !ok {
		return nil, invalid("this connection type is not available in this build", "remove the connection")
	}
	changed, err := planSecrets(d, in.Secrets)
	if err != nil {
		return nil, err
	}
	usable, err := s.v.Usable(c.Tx, r.ID)
	if err != nil {
		return nil, err
	}
	have := func(k string) bool {
		if v, given := in.Secrets[k]; given {
			return v != ""
		}
		return usable[k]
	}
	cfg := r.Config
	if in.Config != nil {
		cfg = *in.Config
	}
	if r.Config, err = d.Normalize(cfg, have); err != nil {
		return nil, fieldErr(err)
	}
	r.UpdatedAt = c.Now()
	if err := update(c.Tx, r); err != nil {
		return nil, err
	}
	if err := s.writeSecrets(c, r.ID, in.Secrets, changed); err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvConfigured, Object: r.ID, Payload: payload(r, changed)}); err != nil {
		return nil, err
	}
	return s.view(c.Tx, r)
}

func (s *Service) applyEnabled(c *command.Ctx, on bool) (any, error) {
	var in idInput
	if err := strict(c.Input, &in); err != nil {
		return nil, err
	}
	r, err := get(c.Tx, in.ID)
	if err != nil {
		return nil, err
	}
	if r.Enabled == on { // 未变不记事件（幂等）
		return s.view(c.Tx, r)
	}
	if on {
		if _, ok := s.decl(r.Type); !ok {
			return nil, invalid("this connection type is not available in this build", "remove the connection")
		}
	}
	r.Enabled, r.UpdatedAt = on, c.Now()
	if err := update(c.Tx, r); err != nil {
		return nil, err
	}
	ev := EvDisabled
	if on {
		ev = EvEnabled
	}
	if _, err := c.Emit(domain.Event{Type: ev, Object: r.ID, Payload: payload(r, nil)}); err != nil {
		return nil, err
	}
	return s.view(c.Tx, r)
}

func (s *Service) applyRename(c *command.Ctx) (any, error) {
	var in renameInput
	if err := strict(c.Input, &in); err != nil {
		return nil, err
	}
	r, err := get(c.Tx, in.ID)
	if err != nil {
		return nil, err
	}
	name, err := cleanName(in.Name)
	if err != nil {
		return nil, err
	}
	if name == r.Name {
		return s.view(c.Tx, r)
	}
	if err := nameFree(c.Tx, name, r.ID); err != nil {
		return nil, err
	}
	r.Name, r.UpdatedAt = name, c.Now()
	if err := update(c.Tx, r); err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvRenamed, Object: r.ID, Payload: payload(r, nil)}); err != nil {
		return nil, err
	}
	return s.view(c.Tx, r)
}

// applyRemove 只删已停用的连接：配置、凭据与现状一起删；打扰策略里仍指向它的路由从此落 Web 未读。
func (s *Service) applyRemove(c *command.Ctx) (any, error) {
	var in idInput
	if err := strict(c.Input, &in); err != nil {
		return nil, err
	}
	r, err := get(c.Tx, in.ID)
	if err != nil {
		return nil, err
	}
	if r.Enabled {
		return nil, domain.NewError(domain.CodeConflict, "the connection is still enabled", "disable it first")
	}
	for _, q := range []string{`DELETE FROM connections_connection WHERE id = ?`, `DELETE FROM connections_status WHERE connection = ?`} {
		if _, err := c.Tx.Exec(q, r.ID); err != nil {
			return nil, err
		}
	}
	if err := secrets.DeleteOwner(c.Tx, r.ID); err != nil {
		return nil, err
	}
	if _, err := c.Emit(domain.Event{Type: EvRemoved, Object: r.ID, Payload: payload(r, nil)}); err != nil {
		return nil, err
	}
	return map[string]string{"id": r.ID}, nil
}
