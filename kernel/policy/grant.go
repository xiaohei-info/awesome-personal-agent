package policy

import (
	"encoding/json"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 授权单是旧权限模型的代理授权（已取消，2026-11-20，§14.3）：不再签发、不再放行任何命令。
// policy_grant 表只读保留，旧单在切换时一次撤销（RevokeAllGrants），历史事件照常可读。

// Grantee 是被授权方，二选一（§6.13）：Credential 是一个 delegate token（Harness 只用于收窄和审计，不是安全边界）；
// 或 Agent，一个 harness 标识，判定时只比内核记录的值（凭据的 harness 列、运行记录的 harness）。
// Background 只在 Agent 式的单上出现：为真时该 Agent 的后台运行也能用这张单。
type Grantee struct {
	Instance   bool   `json:"instance,omitempty"`
	Credential string `json:"credential,omitempty"`
	Harness    string `json:"harness,omitempty"`
	Agent      string `json:"agent,omitempty"`
	Background bool   `json:"background,omitempty"`
}

// Scope 的非空项都满足才算命中；至少一项非空，或 All 为真（不限对象，与其余三项互斥）。
type Scope struct {
	Projects []string `json:"projects"`
	Kinds    []string `json:"kinds"`
	Items    []string `json:"items"`
	All      bool     `json:"all,omitempty"`
}

// ImportScope 是导入专用单的 import 段（来源名与累计条数上限）。导入已取消（§14.5），不再签发这类单；
// 字段只为旧单读出时原样带回，内核不再据它放行任何命令。
type ImportScope struct {
	Source     string `json:"source"`
	MaxRecords int    `json:"max_records"`
}

// Grant 是一张授权单。撤销字段是唯一会变的部分。
type Grant struct {
	ID            string       `json:"id"`
	Principal     string       `json:"principal"`
	Grantee       Grantee      `json:"grantee"`
	Scope         Scope        `json:"scope"`
	Actions       []string     `json:"actions"`
	Import        *ImportScope `json:"import,omitempty"` // 只读的历史值，见 ImportScope
	RiskCeiling   string       `json:"risk_ceiling"`
	ExpiresAt     time.Time    `json:"expires_at,omitzero"`
	Reason        string       `json:"reason,omitempty"`
	CreatedAt     time.Time    `json:"created_at"`
	RevokedAt     time.Time    `json:"revoked_at,omitzero"`
	RevokedReason string       `json:"revoked_reason,omitempty"`
}

// ListGrants 按签发顺序列出全部授权单，含已撤销与已过期（审计与切换用）。
func ListGrants(tx *store.Tx) ([]Grant, error) { return queryGrants(tx, "") }

// RevokeAllGrants 把所有未撤销的授权单标为撤销并各记 grant.revoked，返回被撤销的 id。只给权限模型切换用（§16.5）。
func RevokeAllGrants(tx *store.Tx, actor domain.Actor, now time.Time, reason string) ([]string, error) {
	gs, err := queryGrants(tx, "WHERE revoked_at = 0")
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for _, g := range gs {
		if _, err := tx.Exec(`UPDATE policy_grant SET revoked_at = ?, revoked_reason = ? WHERE id = ? AND revoked_at = 0`, now.UnixNano(), reason, g.ID); err != nil {
			return nil, err
		}
		g.RevokedAt, g.RevokedReason = now.UTC(), reason
		payload, err := json.Marshal(g)
		if err != nil {
			return nil, err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvGrantRevoked, Object: g.ID, Actor: actor, Reason: reason, Payload: payload, Grant: g.ID, At: now}); err != nil {
			return nil, err
		}
		ids = append(ids, g.ID)
	}
	return ids, nil
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
