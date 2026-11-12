package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

var evMod = Module{Name: "eventlog", Migrations: []Migration{{Version: 1, Name: "c",
	SQL: "CREATE TABLE eventlog_event (seq INTEGER PRIMARY KEY AUTOINCREMENT, v TEXT NOT NULL DEFAULT '')"}}}

func evReg(t *testing.T, mods ...Module) *Registry {
	t.Helper()
	r := NewRegistry()
	for _, m := range append([]Module{evMod}, mods...) {
		if err := r.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

func addRows(t *testing.T, s *Store, n int) {
	t.Helper()
	if err := s.Update(context.Background(), func(tx *Tx) error {
		for i := 0; i < n; i++ {
			if _, err := tx.Exec("INSERT INTO eventlog_event (v) VALUES ('x')"); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// BackupFile 在线导出的副本是单文件、可只读检视；源账本不被迁移也不被改动，Pending 只读判断。
func TestBackupFileInspectAndPending(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home, evReg(t))
	if err != nil {
		t.Fatal(err)
	}
	addRows(t, s, 4)
	s.Close()

	newer := evReg(t, v3New)
	pending, err := Pending(home, newer)
	if err != nil || !pending {
		t.Fatalf("pending = %v, %v; want true", pending, err)
	}
	if pending, err := Pending(home, evReg(t)); err != nil || pending {
		t.Fatalf("up-to-date ledger pending = %v, %v", pending, err)
	}
	if pending, err := Pending(t.TempDir(), newer); err != nil || pending {
		t.Fatalf("no ledger pending = %v, %v", pending, err)
	}

	before := dirDigest(t, home)
	dest := filepath.Join(t.TempDir(), "copy.db")
	if err := BackupFile(context.Background(), home, dest); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dest + "-wal"); err == nil {
		t.Error("backup copy has a -wal file")
	}
	info, err := Inspect(dest, evReg(t))
	if err != nil || info.Ledger != 1 || info.EventMaxSeq != 4 || !info.EventContinuous || info.Tables["eventlog_event"] != 4 {
		t.Fatalf("info = %+v, %v", info, err)
	}
	if after := dirDigest(t, home); after != before {
		t.Error("BackupFile / Pending changed the source home")
	}
	if p, _ := Pending(home, newer); !p {
		t.Error("BackupFile must not migrate the source")
	}

	// 注册表不认识副本里的迁移：版本闸。
	s2, _ := Open(t.TempDir(), newer)
	s2.Close()
	if _, err := Inspect(s2.Path(), evReg(t)); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("unknown migrations: %v", err)
	}
	if _, err := Inspect(filepath.Join(t.TempDir(), "none.db"), evReg(t)); !errors.Is(err, ErrNotLedger) {
		t.Errorf("missing file: %v", err)
	}
	junk := filepath.Join(t.TempDir(), "junk.db")
	os.WriteFile(junk, []byte("this is not sqlite at all, just text padding padding padding"), 0o600)
	if _, err := Inspect(junk, evReg(t)); !errors.Is(err, ErrNotLedger) {
		t.Errorf("junk file: %v", err)
	}
}

func TestPendingRefusesUnknownMigrations(t *testing.T) {
	home := t.TempDir()
	s, err := Open(home, evReg(t, v3New))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Pending(home, evReg(t)); !errors.Is(err, ErrSchemaTooNew) {
		t.Errorf("err = %v, want ErrSchemaTooNew", err)
	}
}
