package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"time"
)

// Migration 是模块的一步只增迁移。Version 从 1 起连续；已发布的迁移不可修改（校验和保护）。
//
// Ledger 是这一步所属的账本结构版本（`schema.ledger`，附录 B.5）：0 表示等于 Version，只允许 Version ≤ 2
// 的 v0.1 迁移这样写；之后的迁移必须显式声明。账本结构版本随发行只增，一个模块的迁移序号与它无关
// （新增的模块第一步也可以属于结构版本 3）。
type Migration struct {
	Version int
	Name    string
	SQL     string
	Ledger  int
}

// LedgerVersion 返回这一步所属的账本结构版本。
func (m Migration) LedgerVersion() int {
	if m.Ledger > 0 {
		return m.Ledger
	}
	return m.Version
}

// Checksum 是迁移的校验和（名字加 SQL）；已被真实账本应用过的迁移，校验和不得再变。
func (m Migration) Checksum() string { return checksum(m) }

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
	prev := 0
	for i, mg := range m.Migrations {
		if mg.Version != i+1 {
			return fmt.Errorf("store: module %q migration %d: version must be %d", m.Name, mg.Version, i+1)
		}
		if mg.SQL == "" {
			return fmt.Errorf("store: module %q migration %d: empty SQL", m.Name, mg.Version)
		}
		if mg.Ledger < 0 || (mg.Ledger == 0 && mg.Version > 2) {
			return fmt.Errorf("store: module %q migration %d: ledger structure version must be declared", m.Name, mg.Version)
		}
		if lv := mg.LedgerVersion(); lv < prev {
			return fmt.Errorf("store: module %q migration %d: ledger structure version %d goes backwards from %d", m.Name, mg.Version, lv, prev)
		} else {
			prev = lv
		}
	}
	r.modules[m.Name] = m
	return nil
}

// Modules 返回已登记的模块，按名字排序（迁移的应用顺序）。
func (r *Registry) Modules() []Module {
	ms := make([]Module, 0, len(r.modules))
	for _, m := range r.modules {
		ms = append(ms, m)
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })
	return ms
}

// SchemaLedger 是登记的全部迁移里最高的账本结构版本，即发布物 `schema.ledger`。
func (r *Registry) SchemaLedger() int {
	v := 0
	for _, m := range r.modules {
		for _, mg := range m.Migrations {
			v = max(v, mg.LedgerVersion())
		}
	}
	return v
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

// readApplied 读出已应用的迁移（module → version → checksum）。账本里还没有迁移表时返回空。
// 只读：版本闸要在任何写入之前运行，被拒的账本一个字节都不能变。
func (c *conn) readApplied() (map[string]map[int]string, error) {
	applied := map[string]map[int]string{}
	r, err := c.query("SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = 'store_migrations'", nil)
	if err != nil {
		return nil, err
	}
	exists := r.Next()
	if err := r.Close(); err != nil {
		return nil, err
	}
	if !exists {
		return applied, nil
	}
	rows, err := c.query("SELECT module, version, checksum FROM store_migrations", nil)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mod, sum string
		var ver int64
		if err := rows.Scan(&mod, &ver, &sum); err != nil {
			return nil, err
		}
		if applied[mod] == nil {
			applied[mod] = map[int]string{}
		}
		applied[mod][int(ver)] = sum
	}
	return applied, rows.Err()
}

// gate 是版本闸与历史校验：账本里有本程序不认识的迁移就拒绝（降级或漏登记模块），已应用的迁移被改动也拒绝。
func (r *Registry) gate(applied map[string]map[int]string) error {
	for mod, vers := range applied {
		m, ok := r.modules[mod]
		for v, sum := range vers {
			if !ok || v > len(m.Migrations) {
				return fmt.Errorf("%w: module %q version %d (this build supports ledger structure version %d)", ErrSchemaTooNew, mod, v, r.SchemaLedger())
			}
			if checksum(m.Migrations[v-1]) != sum {
				return fmt.Errorf("%w: module %q version %d", ErrMigrationChanged, mod, v)
			}
		}
	}
	return nil
}

// migrate 在一个事务里按模块名顺序应用全部待办迁移：任一步失败（含测试构建的注入失败），整体回滚，账本不变。
func (s *Store) migrate(reg *Registry, applied map[string]map[int]string) error {
	ctx := context.Background()
	failAfter, inject := migrationFault()
	return s.Update(ctx, func(tx *Tx) error {
		if _, err := tx.Exec(migrationsTable); err != nil {
			return err
		}
		steps := 0
		for _, m := range reg.Modules() {
			for _, mg := range m.Migrations {
				if _, done := applied[m.Name][mg.Version]; done {
					continue
				}
				if err := s.c.execScript(mg.SQL); err != nil {
					return fmt.Errorf("store: migrate %s/%d %s: %w", m.Name, mg.Version, mg.Name, err)
				}
				if _, err := tx.Exec("INSERT INTO store_migrations (module, version, name, checksum, applied_at) VALUES (?, ?, ?, ?, ?)",
					m.Name, mg.Version, mg.Name, checksum(mg), time.Now().Unix()); err != nil {
					return fmt.Errorf("store: migrate %s/%d %s: %w", m.Name, mg.Version, mg.Name, err)
				}
				steps++
				if inject && steps == failAfter {
					return fmt.Errorf("store: migration fault injected after step %d (rolled back)", steps)
				}
			}
		}
		return nil
	})
}
