package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

var ctx = context.Background()

func mustReg(t *testing.T, mods ...Module) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, m := range mods {
		if err := r.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func open(t *testing.T, home string, reg *Registry) *Store {
	t.Helper()
	s, err := Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func one(t *testing.T, s *Store, sql string, args ...any) (v any) {
	t.Helper()
	err := s.View(ctx, func(tx *Tx) error {
		r, err := tx.Query(sql, args...)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			if err := r.Scan(&v); err != nil {
				return err
			}
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

var items = Module{Name: "items", Migrations: []Migration{
	{Version: 1, Name: "create", SQL: "CREATE TABLE items_item (id INTEGER PRIMARY KEY, title TEXT NOT NULL)"},
	{Version: 2, Name: "add-note", SQL: "ALTER TABLE items_item ADD COLUMN note TEXT; CREATE INDEX items_item_title ON items_item(title)"},
}}
var events = Module{Name: "events", Migrations: []Migration{
	{Version: 1, Name: "create", SQL: "CREATE TABLE events_event (id INTEGER PRIMARY KEY, item_id INTEGER, body BLOB)"},
}}

// ACT-DPL-05：落盘语义（WAL + 同步落盘），重启后数据仍在。
func TestOpenIsWALFullSyncAndDurableAcrossReopen(t *testing.T) {
	home := filepath.Join(t.TempDir(), "apa")
	s := open(t, home, mustReg(t, items))
	if got := one(t, s, "PRAGMA journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %v", got)
	}
	if got := one(t, s, "PRAGMA synchronous"); got != int64(2) { // 2 = FULL
		t.Fatalf("synchronous = %v", got)
	}
	if fi, err := os.Stat(home); err != nil || fi.Mode().Perm() != 0o700 {
		t.Fatalf("data dir: %v %v", fi, err)
	}
	if err := s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO items_item (id, title) VALUES (?, ?)", 1, "买菜")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s2 := open(t, home, mustReg(t, items))
	if got := one(t, s2, "SELECT title FROM items_item WHERE id=1"); got != "买菜" {
		t.Fatalf("after reopen: %v", got)
	}
}

func TestUpdateCommitsRollsBackOnErrorAndPanic(t *testing.T) {
	s := open(t, t.TempDir(), mustReg(t, items))
	ins := func(tx *Tx, id int) { tx.Exec("INSERT INTO items_item (id, title) VALUES (?, 'x')", id) }
	boom := errors.New("boom")
	err := s.Update(ctx, func(tx *Tx) error { ins(tx, 1); return boom })
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("panic swallowed")
			}
		}()
		s.Update(ctx, func(tx *Tx) error { ins(tx, 2); panic("p") })
	}()
	if n := one(t, s, "SELECT count(*) FROM items_item"); n != int64(0) {
		t.Fatalf("rolled back rows leaked: %v", n)
	}
	// 回滚后存储仍可用。
	if err := s.Update(ctx, func(tx *Tx) error { ins(tx, 3); return nil }); err != nil {
		t.Fatal(err)
	}
	if n := one(t, s, "SELECT count(*) FROM items_item"); n != int64(1) {
		t.Fatalf("count = %v", n)
	}
}

func TestSingleWriterSerializesConcurrentUpdates(t *testing.T) {
	s := open(t, t.TempDir(), mustReg(t, items))
	s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO items_item (id, title) VALUES (1, '0')")
		return err
	})
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.Update(ctx, func(tx *Tx) error {
				r, err := tx.Query("SELECT CAST(title AS INTEGER) FROM items_item WHERE id=1")
				if err != nil {
					return err
				}
				var n int64
				r.Next()
				r.Scan(&n)
				r.Close()
				_, err = tx.Exec("UPDATE items_item SET title=? WHERE id=1", fmt.Sprint(n+1))
				return err
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := one(t, s, "SELECT title FROM items_item WHERE id=1"); got != "40" {
		t.Fatalf("lost update: %v", got)
	}
}

func TestViewIsReadOnlyAndContextHonored(t *testing.T) {
	s := open(t, t.TempDir(), mustReg(t, items))
	err := s.View(ctx, func(tx *Tx) error { _, err := tx.Exec("INSERT INTO items_item (id,title) VALUES (1,'x')"); return err })
	if !errors.Is(err, ErrReadOnly) {
		t.Fatalf("Exec in View: %v", err)
	}
	err = s.View(ctx, func(tx *Tx) error {
		r, err := tx.Query("INSERT INTO items_item (id,title) VALUES (1,'x')")
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Err()
	})
	if err == nil {
		t.Fatal("write via View.Query succeeded")
	}
	// query_only 已复位，Update 仍可写。
	if err := s.Update(ctx, func(tx *Tx) error { _, err := tx.Exec("INSERT INTO items_item (id,title) VALUES (1,'x')"); return err }); err != nil {
		t.Fatal(err)
	}
	c, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.Update(c, func(*Tx) error { t.Fatal("ran"); return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
}

func TestValueRoundTrip(t *testing.T) {
	s := open(t, t.TempDir(), mustReg(t, events))
	blob := []byte{0, 1, 2, 255}
	s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO events_event (id, item_id, body) VALUES (?, ?, ?)", int64(1), nil, blob)
		if err == nil {
			_, err = tx.Exec("INSERT INTO events_event (id, item_id, body) VALUES (?, ?, ?)", 2, true, []byte{})
		}
		if err != nil {
			t.Fatal(err)
		}
		return nil
	})
	s.View(ctx, func(tx *Tx) error {
		r, _ := tx.Query("SELECT id, item_id, body FROM events_event ORDER BY id")
		defer r.Close()
		r.Next()
		var id int64
		var item any
		var body []byte
		r.Scan(&id, &item, &body)
		if id != 1 || item != nil || string(body) != string(blob) {
			t.Errorf("row1: %v %v %v", id, item, body)
		}
		r.Next()
		var ok bool
		r.Scan(&id, &ok, &body)
		if id != 2 || !ok || len(body) != 0 {
			t.Errorf("row2: %v %v %v", id, ok, body)
		}
		if err := r.Scan(&id); err == nil {
			t.Error("column count mismatch not reported")
		}
		return nil
	})
	err := s.Update(ctx, func(tx *Tx) error { _, err := tx.Exec("SELECT ?", struct{}{}); return err })
	if err == nil {
		t.Fatal("unsupported arg accepted")
	}
	err = s.Update(ctx, func(tx *Tx) error { _, err := tx.Exec("SELECT ?"); return err })
	if err == nil {
		t.Fatal("missing arg accepted")
	}
}

// ACT-DPL-06：只增迁移、按模块命名空间、启动自动升级。
func TestMigrationsApplyPerModuleAndUpgradeOnStartup(t *testing.T) {
	home := t.TempDir()
	v1 := mustReg(t,
		Module{Name: "items", Migrations: items.Migrations[:1]},
		events)
	s := open(t, home, v1)
	if n := one(t, s, "SELECT count(*) FROM store_migrations"); n != int64(2) {
		t.Fatalf("applied = %v", n)
	}
	s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO items_item (id,title) VALUES (1,'keep')")
		return err
	})
	s.Close()

	// 新版本多登记一步：重启即升级，旧数据保留。
	s2 := open(t, home, mustReg(t, items, events))
	if n := one(t, s2, "SELECT count(*) FROM store_migrations WHERE module='items'"); n != int64(2) {
		t.Fatalf("items applied = %v", n)
	}
	if got := one(t, s2, "SELECT title FROM items_item WHERE id=1"); got != "keep" {
		t.Fatalf("data lost: %v", got)
	}
	if _, err := s2.c.exec("UPDATE items_item SET note='n'", nil); err != nil {
		t.Fatalf("new column missing: %v", err)
	}
	s2.Close()
	// 再次启动不重复应用。
	s3 := open(t, home, mustReg(t, items, events))
	if n := one(t, s3, "SELECT count(*) FROM store_migrations"); n != int64(3) {
		t.Fatalf("applied = %v", n)
	}
}

func TestMigrationHistoryIsAppendOnly(t *testing.T) {
	home := t.TempDir()
	open(t, home, mustReg(t, items, events)).Close()

	edited := Module{Name: "items", Migrations: append([]Migration{{Version: 1, Name: "create", SQL: "CREATE TABLE items_item (id INTEGER PRIMARY KEY)"}}, items.Migrations[1:]...)}
	if _, err := Open(home, mustReg(t, edited, events)); !errors.Is(err, ErrMigrationChanged) {
		t.Fatalf("edited migration: %v", err)
	}
	if _, err := Open(home, mustReg(t, Module{Name: "items", Migrations: items.Migrations[:1]}, events)); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("dropped migration: %v", err)
	}
	if _, err := Open(home, mustReg(t, items)); !errors.Is(err, ErrSchemaTooNew) {
		t.Fatalf("unregistered module: %v", err)
	}
}

func TestFailedMigrationRollsBackAtomically(t *testing.T) {
	home := t.TempDir()
	bad := Module{Name: "bad", Migrations: []Migration{
		{Version: 1, Name: "ok", SQL: "CREATE TABLE bad_a (id INTEGER)"},
		{Version: 2, Name: "half", SQL: "CREATE TABLE bad_b (id INTEGER); INSERT INTO nope VALUES (1)"},
	}}
	_, err := Open(home, mustReg(t, bad))
	if err == nil || !strings.Contains(err.Error(), "bad/2") {
		t.Fatalf("err = %v", err)
	}
	// 整个升级在一个事务里：第 2 步失败，第 1 步也一起回滚；修复后同版本号可再次应用。
	fixed := Module{Name: "bad", Migrations: []Migration{bad.Migrations[0], {Version: 2, Name: "half", SQL: "CREATE TABLE bad_b (id INTEGER)"}}}
	s := open(t, home, mustReg(t, fixed))
	if n := one(t, s, "SELECT count(*) FROM store_migrations"); n != int64(2) {
		t.Fatalf("applied = %v", n)
	}
}

func TestRegistryRejectsBadModules(t *testing.T) {
	r := NewRegistry()
	for name, m := range map[string]Module{
		"bad name":  {Name: "Items"},
		"gap":       {Name: "a", Migrations: []Migration{{Version: 2, Name: "x", SQL: "SELECT 1"}}},
		"empty sql": {Name: "b", Migrations: []Migration{{Version: 1, Name: "x", SQL: ""}}},
	} {
		if r.Register(m) == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := r.Register(Module{Name: "ok"}); err != nil {
		t.Fatal(err)
	}
	if r.Register(Module{Name: "ok"}) == nil {
		t.Error("duplicate module accepted")
	}
}

// ACT-DPL-06：运行中在线备份，备份期间写入不中断，备份是可独立打开的一致快照。
func TestOnlineBackupWhileWriting(t *testing.T) {
	home := t.TempDir()
	reg := mustReg(t, items, events)
	s := open(t, home, reg)
	pad := strings.Repeat("x", 2000)
	for i := 0; i < 300; i++ {
		s.Update(ctx, func(tx *Tx) error {
			_, err := tx.Exec("INSERT INTO items_item (id, title) VALUES (?, ?)", i, pad)
			return err
		})
	}
	var stop atomic.Bool
	var written atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 1000; !stop.Load(); i++ {
			if err := s.Update(ctx, func(tx *Tx) error {
				_, err := tx.Exec("INSERT INTO items_item (id, title) VALUES (?, ?)", i, pad)
				return err
			}); err != nil {
				t.Error(err)
				return
			}
			written.Add(1)
		}
	}()
	dest := filepath.Join(t.TempDir(), "backup.db")
	err := s.Backup(ctx, dest)
	stop.Store(true)
	wg.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if written.Load() == 0 {
		t.Log("no concurrent write landed during backup (fast machine)")
	}
	for _, p := range []string{dest + ".tmp", dest + "-wal", dest + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("stray file %s", p)
		}
	}
	// 备份作为数据目录里的账本打开：完整性通过、迁移历史在、起始数据齐全、不需再升级。
	bh := t.TempDir()
	if err := os.Rename(dest, filepath.Join(bh, LedgerFile)); err != nil {
		t.Fatal(err)
	}
	b := open(t, bh, reg)
	if got := one(t, b, "PRAGMA integrity_check"); got != "ok" {
		t.Fatalf("integrity_check = %v", got)
	}
	if n := one(t, b, "SELECT count(*) FROM items_item WHERE id < 300"); n != int64(300) {
		t.Fatalf("backup missing rows: %v", n)
	}
	if n := one(t, b, "SELECT count(*) FROM store_migrations"); n != int64(3) {
		t.Fatalf("migrations in backup = %v", n)
	}
}

func TestBackupOverwritesAndFailureKeepsOldBackup(t *testing.T) {
	s := open(t, t.TempDir(), mustReg(t, items))
	dest := filepath.Join(t.TempDir(), "b.db")
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	s.Update(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO items_item (id,title) VALUES (1,'new')")
		return err
	})
	if err := s.Backup(ctx, dest); err != nil {
		t.Fatal(err)
	}
	h := t.TempDir()
	os.Rename(dest, filepath.Join(h, LedgerFile))
	if got := one(t, open(t, h, mustReg(t, items)), "SELECT title FROM items_item"); got != "new" {
		t.Fatalf("second backup stale: %v", got)
	}
	// 取消的备份不覆盖已有文件、不留临时文件。
	old, _ := os.ReadFile(filepath.Join(h, LedgerFile))
	c, cancel := context.WithCancel(ctx)
	cancel()
	dest2 := filepath.Join(h, "keep.db")
	os.WriteFile(dest2, old, 0o600)
	if err := s.Backup(c, dest2); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if cur, _ := os.ReadFile(dest2); string(cur) != string(old) {
		t.Fatal("old backup clobbered")
	}
	if _, err := os.Stat(dest2 + ".tmp"); err == nil {
		t.Fatal("tmp left behind")
	}
	if err := s.Backup(ctx, filepath.Join(h, "no-such-dir", "b.db")); err == nil {
		t.Fatal("backup into missing dir succeeded")
	}
}

func TestOpenFailsOnUnusableHome(t *testing.T) {
	f := filepath.Join(t.TempDir(), "file")
	os.WriteFile(f, nil, 0o600)
	if _, err := Open(filepath.Join(f, "sub"), nil); err == nil {
		t.Fatal("opened under a regular file")
	}
	// 非 SQLite 内容必须报错，不能静默重建。
	h := t.TempDir()
	os.WriteFile(filepath.Join(h, LedgerFile), []byte(strings.Repeat("not a database", 100)), 0o600)
	if _, err := Open(h, nil); err == nil {
		t.Fatal("garbage ledger accepted")
	}
}
