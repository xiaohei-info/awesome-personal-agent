package store

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// v2 是升级前的账本（结构版本 2），v3 在其上加表加列，并带一个新模块（序号 1 属于结构版本 3）。
var (
	v2Mod = Module{Name: "items", Migrations: []Migration{
		{Version: 1, Name: "create", SQL: "CREATE TABLE items_item (id INTEGER PRIMARY KEY, title TEXT NOT NULL)"},
		{Version: 2, Name: "add-note", SQL: "ALTER TABLE items_item ADD COLUMN note TEXT"},
	}}
	v3Items = Module{Name: "items", Migrations: append(append([]Migration{}, v2Mod.Migrations...),
		Migration{Version: 3, Name: "planned", SQL: "ALTER TABLE items_item ADD COLUMN planned_for TEXT NOT NULL DEFAULT ''", Ledger: 3})}
	v3New = Module{Name: "judgments", Migrations: []Migration{
		{Version: 1, Name: "create", SQL: "CREATE TABLE judgments_j (id TEXT PRIMARY KEY)", Ledger: 3},
		{Version: 2, Name: "index", SQL: "CREATE INDEX judgments_j_id ON judgments_j(id)", Ledger: 3},
	}}
)

// dirDigest 对数据目录下全部文件（含 -wal、-shm）取内容哈希，用来断言“一个字节都没变”。
func dirDigest(t *testing.T, dir string) string {
	t.Helper()
	var names []string
	filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			names = append(names, p)
		}
		return nil
	})
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		f, err := os.Open(n)
		if err != nil {
			t.Fatal(err)
		}
		fmt.Fprintf(h, "%s\x00", filepath.Base(n))
		io.Copy(h, f)
		f.Close()
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func TestLedgerVersionIsDeclaredNotGuessed(t *testing.T) {
	r := NewRegistry()
	if err := r.Register(Module{Name: "legacy", Migrations: []Migration{{Version: 1, Name: "a", SQL: "SELECT 1"}, {Version: 2, Name: "b", SQL: "SELECT 1"}}}); err != nil {
		t.Fatal(err)
	}
	if got := r.SchemaLedger(); got != 2 {
		t.Fatalf("v0.1 migrations carry their own version: ledger = %d, want 2", got)
	}
	for name, m := range map[string]Module{
		"undeclared beyond 2": {Name: "u", Migrations: []Migration{{Version: 1, Name: "a", SQL: "SELECT 1"}, {Version: 2, Name: "b", SQL: "SELECT 1"}, {Version: 3, Name: "c", SQL: "SELECT 1"}}},
		"goes backwards":      {Name: "b", Migrations: []Migration{{Version: 1, Name: "a", SQL: "SELECT 1", Ledger: 3}, {Version: 2, Name: "b", SQL: "SELECT 1", Ledger: 2}}},
		"negative":            {Name: "n", Migrations: []Migration{{Version: 1, Name: "a", SQL: "SELECT 1", Ledger: -1}}},
	} {
		if r.Register(m) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := r.Register(v3New); err != nil {
		t.Fatal(err)
	}
	if got := r.SchemaLedger(); got != 3 {
		t.Fatalf("ledger = %d, want 3", got)
	}
	home := t.TempDir()
	s := open(t, home, mustReg(t, v3Items, v3New))
	if s.SchemaLedger() != 3 {
		t.Fatalf("store ledger = %d", s.SchemaLedger())
	}
	if names := mustReg(t, v3New, v3Items).Modules(); names[0].Name != "items" || names[1].Name != "judgments" {
		t.Fatalf("modules not in apply order: %v", names)
	}
}

// ACT-UPG-05：升级只增不删，已有行原样保留。
func TestUpgradeFromV2KeepsRowsAndAddsStructure(t *testing.T) {
	home := t.TempDir()
	s := open(t, home, mustReg(t, v2Mod))
	s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO items_item (id, title, note) VALUES (1, 'keep', 'n')")
		return err
	})
	s.Close()

	s = open(t, home, mustReg(t, v3Items, v3New))
	if got := one(t, s, "SELECT title || '/' || note || '/' || planned_for FROM items_item WHERE id = 1"); got != "keep/n/" {
		t.Fatalf("row after upgrade = %q", got)
	}
	if n := one(t, s, "SELECT count(*) FROM judgments_j"); n != int64(0) {
		t.Fatalf("new table: %v", n)
	}
	if n := one(t, s, "SELECT count(*) FROM store_migrations"); n != int64(5) {
		t.Fatalf("applied = %v", n)
	}
}

// 整个升级是一个事务：第二个模块的迁移失败，第一个模块已执行的步骤也一起回滚。
func TestUpgradeIsOneTransactionAcrossModules(t *testing.T) {
	home := t.TempDir()
	open(t, home, mustReg(t, v2Mod)).Close()

	broken := Module{Name: "judgments", Migrations: []Migration{
		{Version: 1, Name: "create", SQL: "CREATE TABLE judgments_j (id TEXT PRIMARY KEY)", Ledger: 3},
		{Version: 2, Name: "bad", SQL: "INSERT INTO nope VALUES (1)", Ledger: 3},
	}}
	_, err := Open(home, mustReg(t, v3Items, broken))
	if err == nil || !strings.Contains(err.Error(), "judgments/2") {
		t.Fatalf("err = %v", err)
	}
	// 回滚后：items 的第 3 步没留下列，账本仍是结构版本 2，旧模块集合仍能打开。
	s := open(t, home, mustReg(t, v2Mod))
	if _, err := s.c.exec("UPDATE items_item SET planned_for = ''", nil); err == nil {
		t.Fatal("items step 3 survived the failed upgrade")
	}
	if n := one(t, s, "SELECT count(*) FROM store_migrations"); n != int64(2) {
		t.Fatalf("applied = %v", n)
	}
}

// ACT-UPG-14：旧程序打开新结构版本的账本，拒绝启动，并且一个字节都不改。
func TestVersionGateRefusesNewerLedgerUntouched(t *testing.T) {
	home := t.TempDir()
	open(t, home, mustReg(t, v2Mod)).Close()
	open(t, home, mustReg(t, v3Items, v3New)).Close()
	before := dirDigest(t, home)

	for name, reg := range map[string]*Registry{
		"old items":      mustReg(t, v2Mod, v3New),
		"module missing": mustReg(t, v3Items),
		"nil registry":   nil,
	} {
		_, err := Open(home, reg)
		if !errors.Is(err, ErrSchemaTooNew) {
			t.Fatalf("%s: err = %v", name, err)
		}
		if got := dirDigest(t, home); got != before {
			t.Fatalf("%s: ledger directory changed by a refused open", name)
		}
	}
	if err := func() error { _, err := Open(home, mustReg(t, v2Mod, v3New)); return err }(); !strings.Contains(err.Error(), "ledger structure version 3") {
		// 报错要说清楚这个程序支持到哪个结构版本。
		t.Fatalf("message does not name the supported ledger version: %v", err)
	}
}

// 空目录第一次启动不算“新账本被拒”：迁移表还不存在时直接建。
func TestFreshHomeOpens(t *testing.T) {
	s := open(t, t.TempDir(), mustReg(t, v3Items, v3New))
	if s.SchemaLedger() != 3 {
		t.Fatal("fresh ledger not at 3")
	}
}
