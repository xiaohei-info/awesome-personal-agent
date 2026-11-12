//go:build !apa_testclock

package store

import "testing"

// REQ-ENV-MIGFAULT：发布构建里这个变量不生效。
func TestMigrationFaultVariableIgnoredInReleaseBuild(t *testing.T) {
	t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", "1")
	s := open(t, t.TempDir(), mustReg(t, v3Items, v3New))
	if s.SchemaLedger() != 3 {
		t.Fatal("not migrated")
	}
}
