//go:build apa_testclock

package ledgerfixture

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// REQ-ENV-MIGFAULT：测试构建里 APA_TEST_MIGRATION_FAIL_AFTER=<n> 让真实的迁移链在第 n 步之后失败，整体回滚。
// 运行：go test -tags apa_testclock ./tests/fixtures/...（发布构建不含这个入口，见 kernel/store/fault_release_test.go）。
func TestInjectedMigrationFaultRollsBackTheRealChain(t *testing.T) {
	latest := mustLatest(t)
	for _, n := range []int{1, 3, 7} {
		t.Run(fmt.Sprintf("after_%d", n), func(t *testing.T) {
			dir, m := gen(t, 2)
			home := copyTree(t, dir)
			want := treeHash(t, home)
			t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", fmt.Sprint(n))
			reg, _ := RegistryAt(latest)
			st, err := store.Open(home, reg)
			if err == nil {
				st.Close()
				t.Fatal("注入失败后迁移不该成功")
			}
			if !strings.Contains(err.Error(), "injected") {
				t.Errorf("应是注入的失败: %v", err)
			}
			if got := treeHash(t, home); got != want {
				t.Fatalf("回滚后数据目录与升级前不一致")
			}
			t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", "")
			old := open(t, home, 2)
			defer old.Close()
			if got := tableCounts(t, old); fmt.Sprint(got) != fmt.Sprint(m.Tables) {
				t.Errorf("回滚后行数变了")
			}
		})
	}
}
