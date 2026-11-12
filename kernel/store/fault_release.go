//go:build !apa_testclock

package store

// migrationFault 在发布构建里恒为不注入：APA_TEST_MIGRATION_FAIL_AFTER 只在 apa_testclock 测试构建生效（REQ-ENV-MIGFAULT）。
func migrationFault() (int, bool) { return 0, false }
