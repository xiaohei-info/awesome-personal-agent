// Package store 是账本存储：SQLite（WAL、同步落盘）单进程单写者，
// 提供事务接口、按模块注册的只增迁移、启动时自动升级和运行中在线备份（设计 §7.7、§16.2、§18）。
// 本包不定义任何业务表；各模块通过 Registry 注册自己的迁移。
package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// LedgerFile 是数据目录下的账本文件名（§16.2）。
const LedgerFile = "ledger.db"

// ErrReadOnly 在 View 事务内写入时返回。
var ErrReadOnly = errors.New("store: write in read-only transaction")

// Store 持有唯一一条写连接；所有事务经互斥锁串行，即单写者。
type Store struct {
	mu     sync.Mutex
	c      *conn
	path   string
	ledger int
}

// SchemaLedger 是本账本的结构版本（`schema.ledger`，附录 B.5）：Open 成功后即登记的最高账本结构版本。
func (s *Store) SchemaLedger() int { return s.ledger }

// Open 打开（必要时创建）<home>/ledger.db，设置 WAL + 同步落盘，并按 reg 自动升级到最新版本。
// reg 为 nil 表示没有模块迁移。任何一步失败都不返回半开的 Store。
func Open(home string, reg *Registry) (*Store, error) {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return nil, fmt.Errorf("store: %w", err)
	}
	path := filepath.Join(home, LedgerFile)
	c, err := openConn(path)
	if err != nil {
		return nil, err
	}
	if reg == nil {
		reg = NewRegistry()
	}
	// 版本闸先于任何写入（含 WAL 切换与迁移表）：被拒绝的账本保持原样，旧版本二进制仍能打开。
	applied, err := c.readApplied()
	if err == nil {
		err = reg.gate(applied)
	}
	if err != nil {
		c.close()
		return nil, err
	}
	s := &Store{c: c, path: path, ledger: reg.SchemaLedger()}
	if err := s.setup(); err != nil {
		c.close()
		return nil, err
	}
	if err := s.migrate(reg, applied); err != nil {
		c.close()
		return nil, err
	}
	return s, nil
}

func (s *Store) setup() error {
	if err := s.c.execScript("PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON; PRAGMA synchronous=FULL;"); err != nil {
		return err
	}
	// 落盘语义依赖 WAL；切换失败（例如文件系统不支持）必须暴露，不静默降级。
	r, err := s.c.query("PRAGMA journal_mode=WAL", nil)
	if err != nil {
		return err
	}
	var mode string
	if r.Next() {
		err = r.Scan(&mode)
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if !strings.EqualFold(mode, "wal") {
		return fmt.Errorf("store: journal_mode is %q, want wal", mode)
	}
	return nil
}

// Path 返回账本文件路径。
func (s *Store) Path() string { return s.path }

// Close 关闭存储；等待进行中的事务结束。
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.c.close()
}

// Tx 是事务句柄，只在 Update/View 回调内有效。
type Tx struct {
	c        *conn
	readOnly bool
}

// Exec 执行一条带参数语句，返回受影响行数。
func (t *Tx) Exec(sql string, args ...any) (int64, error) {
	if t.readOnly {
		return 0, ErrReadOnly
	}
	return t.c.exec(sql, args)
}

// Query 执行查询；调用方必须 Close 返回的 Rows。
func (t *Tx) Query(sql string, args ...any) (*Rows, error) {
	return t.c.query(sql, args)
}

// Update 在一个写事务内运行 fn：fn 返回 nil 则提交（WAL 同步落盘后才返回），
// 返回错误或 panic 则回滚。
func (s *Store) Update(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, false, fn)
}

// View 在一个只读事务内运行 fn，总是回滚。
func (s *Store) View(ctx context.Context, fn func(*Tx) error) error {
	return s.run(ctx, true, fn)
}

func (s *Store) run(ctx context.Context, readOnly bool, fn func(*Tx) error) (err error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	begin := "BEGIN IMMEDIATE"
	if readOnly {
		// query_only 让 View 内经 Query 发出的写语句也失败。
		begin = "PRAGMA query_only=ON; BEGIN"
		defer s.c.execScript("PRAGMA query_only=OFF")
	}
	if err := s.c.execScript(begin); err != nil {
		return err
	}
	committed := false
	defer func() {
		if committed {
			return
		}
		rbErr := s.c.execScript("ROLLBACK")
		if r := recover(); r != nil {
			panic(r)
		}
		if err == nil {
			err = rbErr
		}
	}()
	if err = fn(&Tx{c: s.c, readOnly: readOnly}); err != nil {
		return err
	}
	if readOnly {
		return nil
	}
	if err = s.c.execScript("COMMIT"); err != nil {
		return err
	}
	committed = true
	return nil
}

// Backup 在线导出账本到 dest（§16.2）：分块拷贝，块间释放写锁，运行中的写事务不被长时间阻塞。
// 先写 dest+".tmp" 再原子改名，失败时不会留下半个备份覆盖旧备份。
func (s *Store) Backup(ctx context.Context, dest string) error {
	tmp := dest + ".tmp"
	if err := s.backupTo(ctx, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("store: %w", err)
	}
	return nil
}

func (s *Store) backupTo(ctx context.Context, tmp string) (err error) {
	for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm"} {
		if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("store: %w", err)
		}
	}
	s.mu.Lock()
	b, err := s.c.backupInit(tmp)
	s.mu.Unlock()
	if err != nil {
		return err
	}
	defer func() {
		// finish 用源连接的 TLS，必须与写事务互斥；它同时关闭目标连接。失败时清掉半成品。
		s.mu.Lock()
		ferr := b.finish()
		s.mu.Unlock()
		if err == nil {
			err = ferr
		}
		if err != nil {
			os.Remove(tmp)
			os.Remove(tmp + "-wal")
			os.Remove(tmp + "-shm")
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		s.mu.Lock()
		done, err := b.step(256)
		s.mu.Unlock()
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}
