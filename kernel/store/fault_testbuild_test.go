//go:build apa_testclock

package store

import (
	"strings"
	"testing"
)

// ACT-UPG-15：测试构建里，迁移在第 n 步之后失败，整体回滚，账本与升级前一致，旧模块集合仍能打开。
func TestMigrationFaultInjectionRollsBackEverything(t *testing.T) {
	home := t.TempDir()
	s := open(t, home, mustReg(t, v2Mod))
	s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO items_item (id, title) VALUES (1, 'keep')")
		return err
	})
	s.Close()

	for _, n := range []string{"1", "2", "3"} {
		t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", n)
		_, err := Open(home, mustReg(t, v3Items, v3New))
		want := "migration fault injected after step " + n
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("n=%s: err = %v", n, err)
		}
		t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", "")
		s := open(t, home, mustReg(t, v2Mod))
		if got := one(t, s, "SELECT title FROM items_item WHERE id = 1"); got != "keep" {
			t.Fatalf("n=%s: data = %v", n, got)
		}
		if got := one(t, s, "SELECT count(*) FROM store_migrations"); got != int64(2) {
			t.Fatalf("n=%s: applied = %v", n, got)
		}
		if _, err := s.c.exec("SELECT count(*) FROM judgments_j", nil); err == nil {
			t.Fatalf("n=%s: new table survived rollback", n)
		}
		s.Close()
	}

	t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", "")
	s = open(t, home, mustReg(t, v3Items, v3New))
	if s.SchemaLedger() != 3 {
		t.Fatal("clean retry did not reach ledger 3")
	}
}
