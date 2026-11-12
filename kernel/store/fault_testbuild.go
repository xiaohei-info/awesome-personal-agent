//go:build apa_testclock

package store

import (
	"os"
	"strconv"
)

// migrationFault 读 APA_TEST_MIGRATION_FAIL_AFTER=<n>：迁移在完成第 n 步之后失败（整体回滚）。
// 只编进 apa_testclock 测试构建，发布构建不含这个入口。
func migrationFault() (int, bool) {
	v := os.Getenv("APA_TEST_MIGRATION_FAIL_AFTER")
	if v == "" {
		return 0, false
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, false
	}
	return n, true
}
