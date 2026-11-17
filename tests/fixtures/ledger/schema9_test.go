package ledgerfixture

import (
	"reflect"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 8 → 9：只增连接草稿表；已有各表行数与 Event 载荷不变；结构版本 8 的程序拒绝打开升级后的账本。
func TestUpgradeV8ToV9AddsConnectionDraftsOnly(t *testing.T) {
	if latest := mustLatest(t); latest < 9 {
		t.Fatalf("schema.ledger = %d, 想要至少 9", latest)
	}
	dir, m := gen(t, 8)
	home := copyTree(t, dir)
	st := open(t, home, 9)
	for tbl, n := range m.Tables {
		if tbl == "store_migrations" { // 迁移记录本身多一行
			n++
		}
		if got := count(t, st, "SELECT COUNT(*) FROM "+tbl); got != n {
			t.Errorf("%s 行数 %d → %d", tbl, n, got)
		}
	}
	if n := count(t, st, "SELECT COUNT(*) FROM connections_draft"); n != 0 {
		t.Errorf("新表 connections_draft 升级后应为空: %d", n)
	}
	if hs, _ := eventHashes(t, st); !reflect.DeepEqual(hs, m.EventPayloadSHA) {
		t.Error("已有 Event 载荷变了")
	}
	st.Close()
	reg8, err := RegistryAt(8)
	if err != nil {
		t.Fatal(err)
	}
	if old, err := store.Open(home, reg8); err == nil {
		old.Close()
		t.Fatal("结构版本 8 的程序不应能打开结构版本 9 的账本")
	} else if !strings.Contains(err.Error(), "newer") {
		t.Errorf("应报账本比程序新: %v", err)
	}
}
