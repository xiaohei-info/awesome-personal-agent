package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock/wall"
)

// Migration 是模块的一步只增迁移。Version 从 1 起连续；已发布的迁移不可修改（校验和保护）。
type Migration struct {
	Version int
	Name    string
	SQL     string
}

// Module 是一个模块的迁移序列。迁移按模块命名空间登记，并行开发的模块各写各的，互不争用同一文件。
type Module struct {
	Name       string
	Migrations []Migration
}

var (
	moduleName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

	// ErrMigrationChanged 表示已应用的迁移与登记内容不一致（被改动、删除或重排）。
	ErrMigrationChanged = errors.New("store: applied migration changed")
	// ErrSchemaTooNew 表示账本里有当前程序不认识的迁移（降级或漏登记模块）。
	ErrSchemaTooNew = errors.New("store: ledger schema is newer than this build")
)

// Registry 收集各模块的迁移。
type Registry struct{ modules map[string]Module }

func NewRegistry() *Registry { return &Registry{modules: map[string]Module{}} }

// Register 登记一个模块；模块名重复、命名不合法、版本不从 1 连续、SQL 为空都会被拒绝。
func (r *Registry) Register(m Module) error {
	if !moduleName.MatchString(m.Name) {
		return fmt.Errorf("store: invalid module name %q", m.Name)
	}
	if _, ok := r.modules[m.Name]; ok {
		return fmt.Errorf("store: module %q already registered", m.Name)
	}
	for i, mg := range m.Migrations {
		if mg.Version != i+1 {
			return fmt.Errorf("store: module %q migration %d: version must be %d", m.Name, mg.Version, i+1)
		}
		if mg.SQL == "" {
			return fmt.Errorf("store: module %q migration %d: empty SQL", m.Name, mg.Version)
		}
	}
	r.modules[m.Name] = m
	return nil
}

func checksum(m Migration) string {
	h := sha256.Sum256([]byte(m.Name + "\x00" + m.SQL))
	return hex.EncodeToString(h[:])
}

const migrationsTable = `CREATE TABLE IF NOT EXISTS store_migrations (
	module TEXT NOT NULL,
	version INTEGER NOT NULL,
	name TEXT NOT NULL,
	checksum TEXT NOT NULL,
	applied_at INTEGER NOT NULL,
	PRIMARY KEY (module, version)
) WITHOUT ROWID`

// migrate 校验已应用历史并按模块名顺序应用待办迁移；每步一个事务，失败则该步整体回滚。
func (s *Store) migrate(reg *Registry) error {
	if reg == nil {
		reg = NewRegistry()
	}
	ctx := context.Background()
	if err := s.Update(ctx, func(tx *Tx) error { _, err := tx.Exec(migrationsTable); return err }); err != nil {
		return err
	}
	applied := map[string]map[int]string{}
	err := s.View(ctx, func(tx *Tx) error {
		rows, err := tx.Query("SELECT module, version, checksum FROM store_migrations")
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var mod, sum string
			var ver int64
			if err := rows.Scan(&mod, &ver, &sum); err != nil {
				return err
			}
			if applied[mod] == nil {
				applied[mod] = map[int]string{}
			}
			applied[mod][int(ver)] = sum
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	names := make([]string, 0, len(reg.modules))
	for n := range reg.modules {
		names = append(names, n)
	}
	sort.Strings(names)
	for mod, vers := range applied {
		m, ok := reg.modules[mod]
		for v, sum := range vers {
			if !ok || v > len(m.Migrations) {
				return fmt.Errorf("%w: module %q version %d", ErrSchemaTooNew, mod, v)
			}
			if checksum(m.Migrations[v-1]) != sum {
				return fmt.Errorf("%w: module %q version %d", ErrMigrationChanged, mod, v)
			}
		}
	}
	for _, n := range names {
		for _, mg := range reg.modules[n].Migrations {
			if _, done := applied[n][mg.Version]; done {
				continue
			}
			err := s.Update(ctx, func(tx *Tx) error {
				if err := s.c.execScript(mg.SQL); err != nil {
					return err
				}
				_, err := tx.Exec("INSERT INTO store_migrations (module, version, name, checksum, applied_at) VALUES (?, ?, ?, ?, ?)",
					n, mg.Version, mg.Name, checksum(mg), wall.Now().Unix())
				return err
			})
			if err != nil {
				return fmt.Errorf("store: migrate %s/%d %s: %w", n, mg.Version, mg.Name, err)
			}
		}
	}
	return nil
}
