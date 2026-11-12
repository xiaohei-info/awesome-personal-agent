package store

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ErrNotLedger 表示文件不是完好的账本（打不开、完整性检查不过或缺迁移表）。
var ErrNotLedger = errors.New("store: not a healthy ledger")

// Info 是对一份账本文件的只读检视，备份清单与校验都取自这里。
type Info struct {
	Ledger          int            // 已应用迁移所属的最高账本结构版本（`schema.ledger`）
	Modules         map[string]int // 模块 → 已应用的最高迁移序号
	Tables          map[string]int64
	EventMaxSeq     int64
	EventContinuous bool // 事件序号从 1 起连续无缺口（没有事件表或没有事件也算连续）
}

// eventTable 是 eventlog 模块的事件表；备份校验要核对它的序号连续性（设计 §16.5）。
const eventTable = "eventlog_event"

// openReadOnly 以只读、不可变方式打开一份静止的账本文件（备份副本）：不创建 -wal/-shm，不改动任何字节。
func openReadOnly(path string) (*conn, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrNotLedger, err)
	}
	u := url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro&immutable=1"}
	return openConn(u.String())
}

// Inspect 只读地检视一份静止的账本文件（备份副本，不能是正被写入的账本）。
// 账本里有 reg 不认识的迁移时返回 ErrSchemaTooNew；完整性检查失败或不是账本返回 ErrNotLedger。
func Inspect(path string, reg *Registry) (Info, error) {
	c, err := openReadOnly(path)
	if err != nil {
		return Info{}, fmt.Errorf("%w: %v", ErrNotLedger, err)
	}
	defer c.close()
	return inspectConn(c, reg)
}

func inspectConn(c *conn, reg *Registry) (info Info, err error) {
	bad := func(format string, a ...any) (Info, error) {
		return Info{}, fmt.Errorf("%w: %s", ErrNotLedger, fmt.Sprintf(format, a...))
	}
	var res string
	if err := scan1(c, "PRAGMA integrity_check", &res); err != nil {
		return bad("%v", err)
	}
	if res != "ok" {
		return bad("integrity check: %s", res)
	}
	applied, err := c.readApplied()
	if err != nil {
		return bad("%v", err)
	}
	if err := reg.gate(applied); err != nil {
		return Info{}, err
	}
	info.Modules = map[string]int{}
	for mod, vers := range applied {
		for v := range vers {
			info.Modules[mod] = max(info.Modules[mod], v)
			info.Ledger = max(info.Ledger, reg.modules[mod].Migrations[v-1].LedgerVersion())
		}
	}
	names, err := tableNames(c)
	if err != nil {
		return bad("%v", err)
	}
	info.Tables = map[string]int64{}
	for _, n := range names {
		var cnt int64
		if err := scan1(c, `SELECT count(*) FROM "`+strings.ReplaceAll(n, `"`, `""`)+`"`, &cnt); err != nil {
			return bad("%v", err)
		}
		info.Tables[n] = cnt
	}
	info.EventContinuous = true
	if cnt, ok := info.Tables[eventTable]; ok && cnt > 0 {
		var lo, hi int64
		r, err := c.query("SELECT min(seq), max(seq) FROM "+eventTable, nil)
		if err != nil {
			return bad("%v", err)
		}
		if r.Next() {
			err = r.Scan(&lo, &hi)
		}
		if cerr := r.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return bad("%v", err)
		}
		info.EventMaxSeq = hi
		info.EventContinuous = lo == 1 && hi == cnt
	}
	return info, nil
}

func tableNames(c *conn) ([]string, error) {
	r, err := c.query("SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'", nil)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var names []string
	for r.Next() {
		var n string
		if err := r.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, r.Err()
}

func scan1(c *conn, q string, dst any) error {
	r, err := c.query(q, nil)
	if err != nil {
		return err
	}
	defer r.Close()
	if !r.Next() {
		return fmt.Errorf("no row from %q", q)
	}
	return r.Scan(dst)
}

// Pending 报告 <home>/ledger.db 是否还有 reg 里未应用的迁移；账本不存在时为 false。
// 只读（不切 WAL、不建迁移表），版本闸不过时返回与 Open 相同的错误。
func Pending(home string, reg *Registry) (bool, error) {
	path := filepath.Join(home, LedgerFile)
	if _, err := os.Stat(path); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: %w", err)
	}
	c, err := openConn(path)
	if err != nil {
		return false, err
	}
	defer c.close()
	applied, err := c.readApplied()
	if err != nil {
		return false, err
	}
	if err := reg.gate(applied); err != nil {
		return false, err
	}
	for _, m := range reg.Modules() {
		for _, mg := range m.Migrations {
			if _, done := applied[m.Name][mg.Version]; !done {
				return true, nil
			}
		}
	}
	return false, nil
}

// BackupFile 把 <home>/ledger.db 在线导出为 dest：不迁移、不改源账本，服务运行中也可执行。
// 导出的副本是单文件（journal_mode=DELETE），不带 -wal/-shm，可直接哈希与只读检视。
func BackupFile(ctx context.Context, home, dest string) error {
	c, err := openConn(filepath.Join(home, LedgerFile))
	if err != nil {
		return err
	}
	s := &Store{c: c, path: filepath.Join(home, LedgerFile)}
	defer s.Close()
	if err := c.execScript("PRAGMA busy_timeout=5000"); err != nil {
		return err
	}
	if err := s.Backup(ctx, dest); err != nil {
		return err
	}
	d, err := openConn(dest)
	if err != nil {
		return err
	}
	if err := d.execScript("PRAGMA journal_mode=DELETE"); err != nil {
		d.close()
		return err
	}
	return d.close()
}
