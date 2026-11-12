package ledgerfixture

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// gen 在测试临时目录里生成结构版本 v 的夹具，返回目录与清单。
func gen(t *testing.T, v int) (string, *Manifest) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "home")
	m, err := Generate(dir, v)
	if err != nil {
		t.Fatalf("Generate(V%d): %v", v, err)
	}
	return dir, m
}

// copyTree 复制整个数据目录：验收里 `cp -R $LEDGER_FIXTURE_V2 $FX_HOME` 的等价操作。
func copyTree(t *testing.T, src string) string {
	t.Helper()
	dst := filepath.Join(t.TempDir(), "home")
	err := filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return dst
}

// treeHash 是整个目录的文件清单与哈希（验收的 `find . -type f | sha256sum`）；任何字节变化都会改变它。
func treeHash(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		h := sha256.Sum256(b)
		lines = append(lines, hex.EncodeToString(h[:])+"  "+rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func open(t *testing.T, dir string, v int) *store.Store {
	t.Helper()
	reg, err := RegistryAt(v)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir, reg)
	if err != nil {
		t.Fatalf("open %s at V%d: %v", dir, v, err)
	}
	return st
}

func count(t *testing.T, st *store.Store, q string, args ...any) int {
	t.Helper()
	var n int64
	err := st.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(q, args...)
		if err != nil {
			return err
		}
		defer r.Close()
		if !r.Next() {
			return fmt.Errorf("no row")
		}
		return r.Scan(&n)
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return int(n)
}

// tableCounts 数出账本里每张表（不含 sqlite 内部表与全文索引影子表）的行数。
func tableCounts(t *testing.T, st *store.Store) map[string]int {
	t.Helper()
	var names []string
	err := st.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' AND name NOT LIKE 'material_search_%' ORDER BY name`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var n string
			if err := r.Scan(&n); err != nil {
				return err
			}
			names = append(names, n)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]int{}
	for _, n := range names {
		out[n] = count(t, st, "SELECT COUNT(*) FROM "+n)
	}
	return out
}

// eventHashes 读出全部 Event 的载荷哈希（seq → sha256），并返回序号序列。
func eventHashes(t *testing.T, st *store.Store) (map[string]string, []int64) {
	t.Helper()
	hs := map[string]string{}
	var seqs []int64
	err := st.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT seq, payload FROM eventlog_event ORDER BY seq`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var seq int64
			var p string
			if err := r.Scan(&seq, &p); err != nil {
				return err
			}
			hs[fmt.Sprint(seq)] = sha(p)
			seqs = append(seqs, seq)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatal(err)
	}
	return hs, seqs
}

func strCol(t *testing.T, st *store.Store, q string, args ...any) []string {
	t.Helper()
	var out []string
	err := st.View(context.Background(), func(tx *store.Tx) error {
		r, err := tx.Query(q, args...)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var s string
			if err := r.Scan(&s); err != nil {
				return err
			}
			out = append(out, s)
		}
		return r.Err()
	})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return out
}

func mustLatest(t *testing.T) int {
	t.Helper()
	l, err := Latest()
	if err != nil {
		t.Fatal(err)
	}
	if l < 3 {
		t.Fatalf("主干账本结构版本是 %d：ledger-schema-v3 的产物应当已把它推到至少 3", l)
	}
	return l
}
