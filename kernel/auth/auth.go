// Package auth 签发、校验、吊销长期凭据（设计 §14.2）：observer/worker/delegate token、
// ext 密钥和 webhook 接收密钥。只存 SHA-256 哈希，明文只在签发（或轮换）时返回一次；
// 吊销在同一账本事务里生效，下一次 Resolve 即拒绝；校验通过后解析成 domain.Actor（§6.7）。
// 谁有权签发或吊销（Web、delegate）由调用方命令层判断；Web 登录会话与运行凭据不在本包。
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Kind 是凭据种类。
type Kind string

const (
	Observer Kind = "observer"
	Worker   Kind = "worker"
	Delegate Kind = "delegate"
	Ext      Kind = "ext"     // Scope = ext 种类（§10.3）
	Webhook  Kind = "webhook" // Scope = 只能写入的 source id
)

var prefix = map[Kind]string{Observer: "apa_obs_", Worker: "apa_wrk_", Delegate: "apa_dlg_", Ext: "apa_ext_", Webhook: "apa_whk_"}

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "auth", Migrations: []store.Migration{{
	Version: 1, Name: "create-credential",
	SQL: `CREATE TABLE auth_credential (
	id TEXT PRIMARY KEY,
	kind TEXT NOT NULL,
	hash TEXT NOT NULL UNIQUE,
	label TEXT NOT NULL,
	executor TEXT NOT NULL,
	scope TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL DEFAULT 0,
	revoked_at INTEGER NOT NULL DEFAULT 0
);`,
}}}

// Credential 是凭据登记，不含明文也不对外暴露哈希。
type Credential struct {
	ID        string    `json:"id"`
	Kind      Kind      `json:"kind"`
	Label     string    `json:"label,omitempty"`
	Executor  string    `json:"executor,omitempty"` // 绑定的执行者 id；空表示未绑定
	Scope     string    `json:"scope,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at,omitzero"` // 零值：不过期；轮换后的旧密钥才有
	RevokedAt time.Time `json:"revoked_at,omitzero"`
}

// IsExportExt 报告凭据是否是导出 ext 密钥（scope 为 export 或 export.<名>，§10.3）：只读事项与材料。
func IsExportExt(kind Kind, principal string) bool {
	return kind == Ext && (principal == "ext:export" || strings.HasPrefix(principal, "ext:export."))
}

// Spec 是签发参数。
type Spec struct {
	Kind     Kind
	Label    string
	Executor string // 只对 worker 有意义（回执据此校验）；delegate 无需绑定
	Scope    string // ext、webhook 必填，其余种类必须为空
}

// Auth 签发与校验凭据；now 可注入以便测试过期。
type Auth struct{ now func() time.Time }

// New 返回 Auth；now 为 nil 时用系统时钟。
func New(now func() time.Time) *Auth {
	if now == nil {
		now = time.Now
	}
	return &Auth{now: now}
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func unauthenticated() error {
	return domain.NewError(domain.CodeUnauthenticated, "credential is missing, unknown, revoked or expired", "obtain a valid credential in Web")
}

func (s Spec) validate() error {
	if _, ok := prefix[s.Kind]; !ok {
		return invalid("unknown credential kind")
	}
	scoped := s.Kind == Ext || s.Kind == Webhook
	if scoped && s.Scope == "" {
		return invalid("scope is required for " + string(s.Kind))
	}
	if !scoped && s.Scope != "" {
		return invalid("scope is only valid for ext and webhook")
	}
	if s.Executor != "" && s.Kind != Worker {
		return invalid("executor binding is only valid for worker")
	}
	return nil
}

func randomString(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

func hashOf(secret string) string {
	h := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(h[:])
}

// Issue 在 tx 内签发凭据。返回的明文是唯一一次可见的机会，账本里只有哈希。
func (a *Auth) Issue(tx *store.Tx, s Spec) (Credential, string, error) {
	if err := s.validate(); err != nil {
		return Credential{}, "", err
	}
	id, err := randomString(12)
	if err != nil {
		return Credential{}, "", err
	}
	body, err := randomString(32) // 256 位熵，快哈希足够
	if err != nil {
		return Credential{}, "", err
	}
	secret := prefix[s.Kind] + body
	c := Credential{ID: "cred_" + id, Kind: s.Kind, Label: s.Label, Executor: s.Executor, Scope: s.Scope, CreatedAt: a.now()}
	if _, err := tx.Exec(`INSERT INTO auth_credential (id, kind, hash, label, executor, scope, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		c.ID, string(c.Kind), hashOf(secret), c.Label, c.Executor, c.Scope, c.CreatedAt.UnixNano()); err != nil {
		return Credential{}, "", err
	}
	return c, secret, nil
}

const cols = "id, kind, label, executor, scope, created_at, expires_at, revoked_at"

func scan(r *store.Rows) (Credential, error) {
	var c Credential
	var kind string
	var created, expires, revoked int64
	if err := r.Scan(&c.ID, &kind, &c.Label, &c.Executor, &c.Scope, &created, &expires, &revoked); err != nil {
		return c, err
	}
	c.Kind, c.CreatedAt = Kind(kind), time.Unix(0, created)
	if expires != 0 {
		c.ExpiresAt = time.Unix(0, expires)
	}
	if revoked != 0 {
		c.RevokedAt = time.Unix(0, revoked)
	}
	return c, nil
}

func one(tx *store.Tx, where string, arg string) (Credential, bool, error) {
	r, err := tx.Query("SELECT "+cols+" FROM auth_credential WHERE "+where, arg)
	if err != nil {
		return Credential{}, false, err
	}
	defer r.Close()
	if !r.Next() {
		return Credential{}, false, r.Err()
	}
	c, err := scan(r)
	return c, err == nil, err
}

// Resolve 校验请求携带的明文凭据：未知、已吊销、已过期统一返回 unauthenticated（退出码 3），不泄露区别。
func (a *Auth) Resolve(tx *store.Tx, secret string) (Credential, error) {
	if secret == "" {
		return Credential{}, unauthenticated()
	}
	c, ok, err := one(tx, "hash = ?", hashOf(secret))
	if err != nil {
		return Credential{}, err
	}
	if !ok || !c.RevokedAt.IsZero() || (!c.ExpiresAt.IsZero() && !a.now().Before(c.ExpiresAt)) {
		return Credential{}, unauthenticated()
	}
	return c, nil
}

// Revoke 吊销凭据，立即生效。重复吊销保留首次时间，不报错（幂等）；不存在返回 not_found。
func (a *Auth) Revoke(tx *store.Tx, id string) error {
	if _, ok, err := one(tx, "id = ?", id); err != nil {
		return err
	} else if !ok {
		return domain.NewError(domain.CodeNotFound, "credential not found", "list credentials and retry")
	}
	_, err := tx.Exec("UPDATE auth_credential SET revoked_at = ? WHERE id = ? AND revoked_at = 0", a.now().UnixNano(), id)
	return err
}

// Rotate 为同种类、同绑定签发新密钥，旧密钥在 grace 后失效（新旧短期并存）。
// grace 为 0 时旧密钥立即失效。已吊销或已轮换过（expires_at 非零，宽限期中或已过期）的凭据不能再轮换，
// 否则会复活过期密钥、重复签发有效密钥，并让 expires_at 被推迟。
func (a *Auth) Rotate(tx *store.Tx, id string, grace time.Duration) (Credential, string, error) {
	old, ok, err := one(tx, "id = ?", id)
	if err != nil {
		return Credential{}, "", err
	}
	if !ok {
		return Credential{}, "", domain.NewError(domain.CodeNotFound, "credential not found", "list credentials and retry")
	}
	if !old.RevokedAt.IsZero() {
		return Credential{}, "", domain.NewError(domain.CodeConflict, "credential is revoked", "issue a new credential")
	}
	if !old.ExpiresAt.IsZero() {
		return Credential{}, "", domain.NewError(domain.CodeConflict, "credential is already rotated", "rotate the current valid credential or issue a new one")
	}
	if grace < 0 {
		return Credential{}, "", invalid("grace must not be negative")
	}
	c, secret, err := a.Issue(tx, Spec{Kind: old.Kind, Label: old.Label, Executor: old.Executor, Scope: old.Scope})
	if err != nil {
		return Credential{}, "", err
	}
	if _, err := tx.Exec("UPDATE auth_credential SET expires_at = ? WHERE id = ?", a.now().Add(grace).UnixNano(), id); err != nil {
		return Credential{}, "", err
	}
	return c, secret, nil
}

// Get 按 id 取凭据登记（含已吊销）；不存在返回 not_found。
func (a *Auth) Get(tx *store.Tx, id string) (Credential, error) {
	c, ok, err := one(tx, "id = ?", id)
	if err == nil && !ok {
		err = domain.NewError(domain.CodeNotFound, "credential not found", "list credentials and retry")
	}
	return c, err
}

// List 按签发顺序列出凭据登记（含已吊销，便于审计）。
func (a *Auth) List(tx *store.Tx) ([]Credential, error) {
	r, err := tx.Query("SELECT " + cols + " FROM auth_credential ORDER BY created_at, id")
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Credential
	for r.Next() {
		c, err := scan(r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, r.Err()
}

// Origin 是请求携带的会话信息，审计到会话粒度。
type Origin struct{ Harness, Session string }

// Actor 把凭据解析成变更的操作者（§6.7）。token 是 agent 经 CLI；ext 与 webhook 是系统侧入口。
// Principal 优先用绑定的执行者 id，否则用凭据 id，保证操作者至少到 token 粒度。
func (c Credential) Actor(o Origin) domain.Actor {
	switch c.Kind {
	case Ext:
		return domain.Actor{Type: domain.ActorSystem, Principal: "ext:" + c.Scope, Via: domain.ViaExt}
	case Webhook:
		return domain.Actor{Type: domain.ActorSystem, Principal: "source:" + c.Scope, Via: domain.ViaSource}
	}
	p := c.Executor
	if p == "" {
		p = c.ID
	}
	return domain.Actor{Type: domain.ActorAgent, Principal: p, Via: domain.ViaCLI, Harness: o.Harness, Session: o.Session}
}

// CanReceiptFor 判断凭据能否为 executor 的事项回执（§14.3 回执）：delegate 可以；
// worker 必须绑定到该执行者；其余种类一律不行。
func (c Credential) CanReceiptFor(executor string) bool {
	switch c.Kind {
	case Delegate:
		return true
	case Worker:
		return c.Executor != "" && c.Executor == executor
	}
	return false
}

// ParseBearer 从 Authorization 头取出明文；格式不对返回空串，由 Resolve 统一拒绝。
func ParseBearer(header string) string {
	if v, ok := strings.CutPrefix(header, "Bearer "); ok {
		return strings.TrimSpace(v)
	}
	return ""
}
