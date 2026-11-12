package policy

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 授权单（设计 §6.13、§14.5）：本人在 Web 事先签发的代理授权。本包只管记录本身——签发、撤销、列出；
// 授权单何时放行命令是授权判定的事，这里不判定。内容不可改，要改就撤销后重签。

// RiskNormal 是目前唯一的风险上限：只放行未判为 web_only 的对象。
const RiskNormal = "normal"

// MaxGrantTTL 是授权单有效期的上限（§6.13：最长 90 天）。
const MaxGrantTTL = 90 * 24 * time.Hour

// GrantableActions 是授权单 actions 的全部可选值：附录 B.1「第二档（可用授权单）」一列。
// 项目、来源、能力包实例管理只接受本人原话，不在其中。
var GrantableActions = []string{
	"item.create", "item.update", "item.transition", "item.relate",
	"decision.answer", "proposal.accept", "proposal.dismiss", "proposal.attach",
	"revision.confirm", "revision.reject",
	"delegate.report", "followmsg.handback",
}

// Grantee 是被授权方：Credential 必填且是一个 delegate token；Harness 只用于收窄和审计，不是安全边界。
type Grantee struct {
	Credential string `json:"credential"`
	Harness    string `json:"harness,omitempty"`
}

// Scope 的非空项都满足才算命中；至少一项非空。
type Scope struct {
	Projects []string `json:"projects"`
	Kinds    []string `json:"kinds"`
	Items    []string `json:"items"`
}

// GrantInput 是签发请求；id、主体、签发时间由内核给出。
type GrantInput struct {
	Grantee     Grantee   `json:"grantee"`
	Scope       Scope     `json:"scope"`
	Actions     []string  `json:"actions"`
	RiskCeiling string    `json:"risk_ceiling,omitempty"` // 空取 normal
	ExpiresAt   time.Time `json:"expires_at"`
	Reason      string    `json:"reason,omitempty"`
}

// Grant 是一张授权单。撤销字段是唯一会变的部分。
type Grant struct {
	ID            string    `json:"id"`
	Principal     string    `json:"principal"`
	Grantee       Grantee   `json:"grantee"`
	Scope         Scope     `json:"scope"`
	Actions       []string  `json:"actions"`
	RiskCeiling   string    `json:"risk_ceiling"`
	ExpiresAt     time.Time `json:"expires_at"`
	Reason        string    `json:"reason,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	RevokedAt     time.Time `json:"revoked_at,omitzero"`
	RevokedReason string    `json:"revoked_reason,omitempty"`
}

// Active 报告授权单在 at 时刻是否未撤销且未过期。凭据是否仍有效另见 ActiveGrants。
func (g Grant) Active(at time.Time) bool { return g.RevokedAt.IsZero() && at.Before(g.ExpiresAt) }

func webOnlyErr() error {
	return domain.NewError(domain.CodeWebOnly, "grants can only be issued or revoked by the owner in the Web UI", "ask the owner to use the Web settings")
}

func newGrantID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "grt_" + hex.EncodeToString(b), nil
}

// normalizeGrant 校验 in 并返回规整后的授权单（不含 id、主体与签发时间）。
func normalizeGrant(tx *store.Tx, a *auth.Auth, now time.Time, in GrantInput) (Grant, error) {
	g := Grant{Grantee: in.Grantee, Reason: in.Reason, RiskCeiling: in.RiskCeiling, ExpiresAt: in.ExpiresAt.UTC(),
		Scope:   Scope{Projects: dedupSorted(in.Scope.Projects), Kinds: dedupSorted(in.Scope.Kinds), Items: dedupSorted(in.Scope.Items)},
		Actions: dedupSorted(in.Actions)}
	if len(g.Scope.Projects)+len(g.Scope.Kinds)+len(g.Scope.Items) == 0 {
		return g, invalid("scope needs at least one of projects, kinds, items")
	}
	if len(g.Actions) == 0 {
		return g, invalid("actions is empty")
	}
	for _, act := range g.Actions {
		if !oneOf(act, GrantableActions...) { // 通配符与只接受本人原话的命令都落在这里
			return g, invalid("action %q cannot be granted; see appendix B.1 for the grantable commands", act)
		}
	}
	if g.RiskCeiling == "" {
		g.RiskCeiling = RiskNormal
	}
	if g.RiskCeiling != RiskNormal {
		return g, invalid("risk_ceiling %q: only %q exists", g.RiskCeiling, RiskNormal)
	}
	if in.ExpiresAt.IsZero() {
		return g, invalid("expires_at is required")
	}
	if !g.ExpiresAt.After(now) || g.ExpiresAt.After(now.Add(MaxGrantTTL)) {
		return g, invalid("expires_at must be in the future and within 90 days")
	}
	g.Grantee.Harness = strings.TrimSpace(g.Grantee.Harness)
	if g.Grantee.Credential == "" {
		return g, invalid("grantee.credential is required")
	}
	cred, err := a.Get(tx, g.Grantee.Credential)
	if err != nil {
		return g, invalid("grantee.credential is not a known credential")
	}
	if cred.Kind != auth.Delegate || !cred.RevokedAt.IsZero() {
		return g, invalid("grantee.credential must be a live delegate token")
	}
	return g, nil
}

// IssueGrant 是授权单的唯一签发入口：actor 必须是 Web 上的本人，否则返回 web_only（退出码 20）。
// 记录与 grant.issued 事件在同一事务提交。校验失败返回 ErrInvalid，什么都不写。
func IssueGrant(tx *store.Tx, a *auth.Auth, actor domain.Actor, now time.Time, in GrantInput) (Grant, error) {
	if actor.Type != domain.ActorHuman || actor.Via != domain.ViaWeb {
		return Grant{}, webOnlyErr()
	}
	g, err := normalizeGrant(tx, a, now, in)
	if err != nil {
		return g, err
	}
	if g.ID, err = newGrantID(); err != nil {
		return g, err
	}
	g.Principal, g.CreatedAt = actor.Principal, now.UTC()
	doc, err := json.Marshal(g)
	if err != nil {
		return g, err
	}
	if _, err := tx.Exec(`INSERT INTO policy_grant (id, credential, doc, expires_at, created_at) VALUES (?, ?, ?, ?, ?)`,
		g.ID, g.Grantee.Credential, string(doc), g.ExpiresAt.UnixNano(), now.UnixNano()); err != nil {
		return g, err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: domain.EvGrantIssued, Object: g.ID, Actor: actor, Reason: g.Reason, Payload: doc, Grant: g.ID, At: now})
	return g, err
}

// RevokeGrant 撤销授权单，立即生效；actor 必须是 Web 上的本人。重复撤销保留首次时间且不再记事件；不存在返回 not_found。
func RevokeGrant(tx *store.Tx, actor domain.Actor, now time.Time, id, reason string) (Grant, error) {
	if actor.Type != domain.ActorHuman || actor.Via != domain.ViaWeb {
		return Grant{}, webOnlyErr()
	}
	g, err := GetGrant(tx, id)
	if err != nil || !g.RevokedAt.IsZero() {
		return g, err
	}
	if _, err := tx.Exec(`UPDATE policy_grant SET revoked_at = ?, revoked_reason = ? WHERE id = ? AND revoked_at = 0`, now.UnixNano(), reason, id); err != nil {
		return g, err
	}
	g.RevokedAt, g.RevokedReason = now.UTC(), reason
	payload, err := json.Marshal(g)
	if err != nil {
		return g, err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: domain.EvGrantRevoked, Object: g.ID, Actor: actor, Reason: reason, Payload: payload, Grant: g.ID, At: now})
	return g, err
}

const grantCols = "doc, revoked_at, revoked_reason"

func scanGrant(r *store.Rows) (Grant, error) {
	var (
		doc, why string
		revoked  int64
		g        Grant
	)
	if err := r.Scan(&doc, &revoked, &why); err != nil {
		return g, err
	}
	if err := json.Unmarshal([]byte(doc), &g); err != nil {
		return g, err
	}
	if revoked != 0 {
		g.RevokedAt, g.RevokedReason = time.Unix(0, revoked).UTC(), why
	}
	return g, nil
}

func queryGrants(tx *store.Tx, where string, args ...any) ([]Grant, error) {
	r, err := tx.Query("SELECT "+grantCols+" FROM policy_grant "+where+" ORDER BY created_at, id", args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := []Grant{}
	for r.Next() {
		g, err := scanGrant(r)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, r.Err()
}

// GetGrant 按 id 取授权单；不存在返回 not_found。
func GetGrant(tx *store.Tx, id string) (Grant, error) {
	gs, err := queryGrants(tx, "WHERE id = ?", id)
	if err != nil {
		return Grant{}, err
	}
	if len(gs) == 0 {
		return Grant{}, domain.NewError(domain.CodeNotFound, "grant not found", "list grants and retry")
	}
	return gs[0], nil
}

// ListGrants 按签发顺序列出全部授权单，含已撤销与已过期（Web 审计用）。
func ListGrants(tx *store.Tx) ([]Grant, error) { return queryGrants(tx, "") }

// ActiveGrants 列出凭据名下在 at 时刻未撤销、未过期的授权单；凭据本身已吊销或已过期时授权单同时失效，返回空。
func ActiveGrants(tx *store.Tx, a *auth.Auth, credential string, at time.Time) ([]Grant, error) {
	cred, err := a.Get(tx, credential)
	if err != nil {
		if e, ok := err.(*domain.Error); ok && e.Code == domain.CodeNotFound {
			return []Grant{}, nil
		}
		return nil, err
	}
	if !cred.RevokedAt.IsZero() || (!cred.ExpiresAt.IsZero() && !at.Before(cred.ExpiresAt)) {
		return []Grant{}, nil
	}
	all, err := queryGrants(tx, "WHERE credential = ? AND revoked_at = 0", credential)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, g := range all {
		if g.Active(at) {
			out = append(out, g)
		}
	}
	return out, nil
}
