// Package channel 是渠道相关的内核规则（设计 §10.3）。目前是一次性表单令牌：只在只能发文字的渠道上、
// 为非 web_only 决策发放，账本只存哈希。回答校验在 answer.go。
package channel

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// FormTTL 是表单链接的有效期（§10.3 默认 24 小时）。
// shortcut: 还不可配置；§21.1 第 10 条要求可配置，接配置时改成读配置。
const FormTTL = 24 * time.Hour

const routePrefix = "channel:"

// Module 登记表单令牌表。
var Module = store.Module{Name: "channel", Migrations: []store.Migration{{
	Version: 1, Name: "create-form",
	SQL: `CREATE TABLE channel_form (
	hash TEXT PRIMARY KEY,
	item TEXT NOT NULL,
	version INTEGER NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL,
	used_at INTEGER NOT NULL DEFAULT 0
) WITHOUT ROWID;`,
}, {
	Version: 2, Name: "add-form-channel",
	SQL: `ALTER TABLE channel_form ADD COLUMN channel TEXT NOT NULL DEFAULT '';`,
}}}

// Form 是一个令牌授予的全部内容：只能回答这一个事项这一版本的决策。
type Form struct {
	Item    string
	Version int64
	Channel string // 发放时的渠道，决定入口 resolved_via 记它
}

func hashOf(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// IssueForm 在调用方事务里为决策投递发放令牌并返回明文。路由不是只能发文字的渠道，或决策属于 web_only 时不发放，返回空串。
func IssueForm(tx *store.Tx, pol policy.Policy, it domain.Item, route string, version int64, now time.Time) (string, error) {
	name, ok := strings.CutPrefix(route, routePrefix)
	if !ok || !pol.Channels[name].TextOnly || it.Ask == nil {
		return "", nil
	}
	if authz.WebOnly(pol, authz.Object{Kind: string(it.Kind), Tags: it.Tags, Project: it.Project, Category: it.Ask.RiskClass}) {
		return "", nil
	}
	b := make([]byte, 32) // 256 位熵，快哈希足够
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(b)
	_, err := tx.Exec(`INSERT INTO channel_form (hash, item, version, channel, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?)`,
		hashOf(token), it.ID, version, name, now.UnixNano(), now.Add(FormTTL).UnixNano())
	return token, err
}

// PeekForm 只读令牌：未知返回 not_found，用过或已过期返回 conflict；不改状态。
func PeekForm(tx *store.Tx, token string, now time.Time) (Form, error) {
	r, err := tx.Query(`SELECT item, version, channel, expires_at, used_at FROM channel_form WHERE hash = ?`, hashOf(token))
	if err != nil {
		return Form{}, err
	}
	defer r.Close()
	if !r.Next() {
		if err := r.Err(); err != nil {
			return Form{}, err
		}
		return Form{}, domain.NewError(domain.CodeNotFound, "form link not found", "use the link from the latest message, or answer in the Web UI")
	}
	var (
		f             Form
		expires, used int64
	)
	if err := r.Scan(&f.Item, &f.Version, &f.Channel, &expires, &used); err != nil {
		return Form{}, err
	}
	if used != 0 || now.UnixNano() >= expires {
		return Form{}, domain.NewError(domain.CodeConflict, "form link has been used or has expired", "answer in the Web UI")
	}
	return f, nil
}

// RedeemForm 在调用方事务里兑现令牌：未知返回 not_found，用过或已过期返回 conflict。事务回滚时令牌不算用过。
func RedeemForm(tx *store.Tx, token string, now time.Time) (Form, error) {
	f, err := PeekForm(tx, token, now)
	if err != nil {
		return Form{}, err
	}
	_, err = tx.Exec(`UPDATE channel_form SET used_at = ? WHERE hash = ?`, now.UnixNano(), hashOf(token))
	return f, err
}
