// Package channel 是渠道相关的内核规则（设计 §10.3）：渠道回答的校验在 answer.go。
package channel

import "github.com/xiaohei-info/awesome-personal-agent/kernel/store"

const routePrefix = "channel:"

// 一次性表单回答已取消（2026-11-20，§14.3）：持有链接不能证明是本人。channel_form 表只读保留，
// 旧链接由 Web 跳转到需要登录的页面，令牌不再兑现。
// Module 登记表单令牌表（只读保留，不再写入）。
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
