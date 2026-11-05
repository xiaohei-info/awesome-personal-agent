package store

import (
	"context"
	"database/sql"
	"fmt"

	"modernc.org/sqlite" // 纯 Go SQLite 驱动：免 cgo，保持静态单二进制（§18）
)

// conn 是唯一的一条 SQLite 连接；并发安全由 Store 的互斥锁保证。
type conn struct {
	db  *sql.DB
	raw *sql.Conn
}

func openConn(path string) (*conn, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	raw, err := db.Conn(context.Background())
	if err == nil {
		err = raw.PingContext(context.Background()) // 文件不是 SQLite 时在这里暴露
	}
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("store: open %s: %w", path, err)
	}
	return &conn{db: db, raw: raw}, nil
}

func (c *conn) close() error {
	if c.db == nil {
		return nil
	}
	err := c.raw.Close()
	if derr := c.db.Close(); err == nil {
		err = derr
	}
	c.db = nil
	if err != nil {
		return fmt.Errorf("store: close: %w", err)
	}
	return nil
}

// execScript 执行不带参数的多语句脚本（迁移、PRAGMA）。
func (c *conn) execScript(sql string) error {
	if _, err := c.raw.ExecContext(context.Background(), sql); err != nil {
		return fmt.Errorf("store: exec: %w", err)
	}
	return nil
}

func (c *conn) exec(q string, args []any) (int64, error) {
	res, err := c.raw.ExecContext(context.Background(), q, args...)
	if err != nil {
		return 0, fmt.Errorf("store: exec: %w", err)
	}
	return res.RowsAffected()
}

// Rows 是查询结果游标，用完必须 Close；只能在产生它的 Tx 回调内使用。
type Rows struct{ r *sql.Rows }

func (c *conn) query(q string, args []any) (*Rows, error) {
	r, err := c.raw.QueryContext(context.Background(), q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: query: %w", err)
	}
	return &Rows{r}, nil
}

// Next 前进一行；结束或出错返回 false，出错用 Err 取。
func (r *Rows) Next() bool { return r.r.Next() }

// Scan 把当前行按列顺序读入 *int64、*string、*[]byte、*float64、*bool、*any；NULL 读成零值（*any 为 nil）。
func (r *Rows) Scan(dest ...any) error {
	cols, err := r.r.Columns()
	if err != nil {
		return err
	}
	if len(cols) != len(dest) {
		return fmt.Errorf("store: row has %d columns, got %d dests", len(cols), len(dest))
	}
	vals := make([]any, len(dest))
	ptrs := make([]any, len(dest))
	for i := range vals {
		ptrs[i] = &vals[i]
	}
	if err := r.r.Scan(ptrs...); err != nil {
		return err
	}
	for i, d := range dest {
		v := vals[i]
		switch p := d.(type) {
		case *any:
			if b, ok := v.([]byte); ok { // 驱动复用缓冲区，拷贝一份
				v = append([]byte{}, b...)
			}
			*p = v
		case *int64:
			*p = 0
			if v != nil {
				n, ok := v.(int64)
				if !ok {
					return fmt.Errorf("store: column %d is %T, not integer", i, v)
				}
				*p = n
			}
		case *bool:
			*p = false
			if n, ok := v.(int64); ok {
				*p = n != 0
			} else if b, ok := v.(bool); ok {
				*p = b
			} else if v != nil {
				return fmt.Errorf("store: column %d is %T, not bool", i, v)
			}
		case *float64:
			*p = 0
			switch n := v.(type) {
			case nil:
			case float64:
				*p = n
			case int64:
				*p = float64(n)
			default:
				return fmt.Errorf("store: column %d is %T, not float", i, v)
			}
		case *string:
			*p = ""
			switch s := v.(type) {
			case nil:
			case string:
				*p = s
			case []byte:
				*p = string(s)
			default:
				return fmt.Errorf("store: column %d is %T, not text", i, v)
			}
		case *[]byte:
			*p = []byte{}
			switch s := v.(type) {
			case nil:
			case []byte:
				*p = append([]byte{}, s...)
			case string:
				*p = []byte(s)
			default:
				return fmt.Errorf("store: column %d is %T, not blob", i, v)
			}
		default:
			return fmt.Errorf("store: unsupported scan dest %T", d)
		}
	}
	return nil
}

func (r *Rows) Err() error { return r.r.Err() }

func (r *Rows) Close() error { return r.r.Close() }

type backup struct{ b *sqlite.Backup }

// backupInit 开始把本连接的主库备份到 dstPath（驱动内部另开目标连接）。
func (c *conn) backupInit(dstPath string) (*backup, error) {
	var b *sqlite.Backup
	err := c.raw.Raw(func(dc any) error {
		bc, ok := dc.(interface {
			NewBackup(string) (*sqlite.Backup, error)
		})
		if !ok {
			return fmt.Errorf("driver connection %T does not support backup", dc)
		}
		var err error
		b, err = bc.NewBackup(dstPath)
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("store: backup init: %w", err)
	}
	return &backup{b}, nil
}

// step 拷贝至多 pages 页；done 为真表示拷完。
func (b *backup) step(pages int) (done bool, err error) {
	more, err := b.b.Step(int32(pages))
	if err != nil {
		return false, fmt.Errorf("store: backup step: %w", err)
	}
	return !more, nil
}

func (b *backup) finish() error { return b.b.Finish() }
