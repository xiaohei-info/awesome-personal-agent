//go:build apa_testclock

package serve

import (
	"bytes"
	"context"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ACT-UPG-15：迁移中途失败时，serve 向本人报告"失败、已回滚"，并带出注入标记；账本不变。
func TestServeReportsFailedMigrationAsRolledBack(t *testing.T) {
	home := t.TempDir() + "/h"
	if err := config.LayoutOf(home).Ensure(); err != nil {
		t.Fatal(err)
	}
	old, err := store.Open(home, v01Registry(t))
	if err != nil {
		t.Fatal(err)
	}
	old.Close()
	t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", "2")

	var out, errb bytes.Buffer
	c := &client.Ctx{Env: envOf(home, t.TempDir(), "", &out, &errb)}
	err = run(context.Background(), c, func(net.Addr) {})
	var de *domain.Error
	if !errors.As(err, &de) {
		t.Fatalf("want a domain error, got %v", err)
	}
	for _, want := range []string{"migration fault injected after step 2", "rolled back"} {
		if !strings.Contains(de.Why, want) {
			t.Errorf("why %q lacks %q", de.Why, want)
		}
	}
	if strings.Contains(de.Why, home) {
		t.Errorf("why leaks the home path: %q", de.Why)
	}

	t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", "")
	s, err := store.Open(home, v01Registry(t))
	if err != nil {
		t.Fatalf("ledger changed by the failed migration: %v", err)
	}
	s.Close()
}
