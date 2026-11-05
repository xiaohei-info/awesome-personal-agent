package store

/*
#cgo LDFLAGS: -lsqlite3
#include <sqlite3.h>
#include <stdlib.h>

static int bind_text(sqlite3_stmt *s, int i, const char *p, int n) { return sqlite3_bind_text(s, i, p, n, SQLITE_TRANSIENT); }
static int bind_blob(sqlite3_stmt *s, int i, const void *p, int n) { return sqlite3_bind_blob(s, i, p, n, SQLITE_TRANSIENT); }
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// conn 是一条 SQLite 连接的薄封装；并发安全由 Store 的互斥锁保证。
type conn struct{ db *C.sqlite3 }

func openConn(path string) (*conn, error) {
	cpath := C.CString(path)
	defer C.free(unsafe.Pointer(cpath))
	var db *C.sqlite3
	rc := C.sqlite3_open_v2(cpath, &db, C.SQLITE_OPEN_READWRITE|C.SQLITE_OPEN_CREATE|C.SQLITE_OPEN_FULLMUTEX, nil)
	c := &conn{db: db}
	if rc != C.SQLITE_OK {
		err := c.err("open " + path)
		c.close()
		return nil, err
	}
	return c, nil
}

func (c *conn) close() error {
	if c.db == nil {
		return nil
	}
	rc := C.sqlite3_close(c.db)
	if rc != C.SQLITE_OK {
		return c.err("close")
	}
	c.db = nil
	return nil
}

func (c *conn) err(op string) error {
	return fmt.Errorf("store: %s: %s", op, C.GoString(C.sqlite3_errmsg(c.db)))
}

// execScript 执行不带参数的多语句脚本（迁移、PRAGMA）。
func (c *conn) execScript(sql string) error {
	cs := C.CString(sql)
	defer C.free(unsafe.Pointer(cs))
	var msg *C.char
	if rc := C.sqlite3_exec(c.db, cs, nil, nil, &msg); rc != C.SQLITE_OK {
		defer C.sqlite3_free(unsafe.Pointer(msg))
		return fmt.Errorf("store: exec: %s", C.GoString(msg))
	}
	return nil
}

func (c *conn) prepare(sql string, args []any) (*C.sqlite3_stmt, error) {
	cs := C.CString(sql)
	defer C.free(unsafe.Pointer(cs))
	var st *C.sqlite3_stmt
	if rc := C.sqlite3_prepare_v2(c.db, cs, -1, &st, nil); rc != C.SQLITE_OK {
		return nil, c.err("prepare")
	}
	if st == nil {
		return nil, errors.New("store: empty statement")
	}
	if n := int(C.sqlite3_bind_parameter_count(st)); n != len(args) {
		C.sqlite3_finalize(st)
		return nil, fmt.Errorf("store: statement has %d parameters, got %d args", n, len(args))
	}
	for i, a := range args {
		if err := c.bind(st, C.int(i+1), a); err != nil {
			C.sqlite3_finalize(st)
			return nil, err
		}
	}
	return st, nil
}

func (c *conn) bind(st *C.sqlite3_stmt, i C.int, a any) error {
	var rc C.int
	switch v := a.(type) {
	case nil:
		rc = C.sqlite3_bind_null(st, i)
	case int:
		rc = C.sqlite3_bind_int64(st, i, C.sqlite3_int64(v))
	case int64:
		rc = C.sqlite3_bind_int64(st, i, C.sqlite3_int64(v))
	case bool:
		n := 0
		if v {
			n = 1
		}
		rc = C.sqlite3_bind_int64(st, i, C.sqlite3_int64(n))
	case float64:
		rc = C.sqlite3_bind_double(st, i, C.double(v))
	case string:
		cs := C.CString(v)
		defer C.free(unsafe.Pointer(cs))
		rc = C.bind_text(st, i, cs, C.int(len(v)))
	case []byte:
		if len(v) == 0 {
			rc = C.sqlite3_bind_zeroblob(st, i, 0)
		} else {
			rc = C.bind_blob(st, i, unsafe.Pointer(&v[0]), C.int(len(v)))
		}
	default:
		return fmt.Errorf("store: unsupported arg type %T", a)
	}
	if rc != C.SQLITE_OK {
		return c.err("bind")
	}
	return nil
}

func (c *conn) exec(sql string, args []any) (int64, error) {
	st, err := c.prepare(sql, args)
	if err != nil {
		return 0, err
	}
	defer C.sqlite3_finalize(st)
	for {
		rc := C.sqlite3_step(st)
		if rc == C.SQLITE_DONE {
			return int64(C.sqlite3_changes(c.db)), nil
		}
		if rc != C.SQLITE_ROW {
			return 0, c.err("step")
		}
	}
}

// Rows 是查询结果游标，用完必须 Close；只能在产生它的 Tx 回调内使用。
type Rows struct {
	c    *conn
	st   *C.sqlite3_stmt
	err  error
	done bool
}

func (c *conn) query(sql string, args []any) (*Rows, error) {
	st, err := c.prepare(sql, args)
	if err != nil {
		return nil, err
	}
	return &Rows{c: c, st: st}, nil
}

// Next 前进一行；结束或出错返回 false，出错用 Err 取。
func (r *Rows) Next() bool {
	if r.done || r.st == nil {
		return false
	}
	switch C.sqlite3_step(r.st) {
	case C.SQLITE_ROW:
		return true
	case C.SQLITE_DONE:
	default:
		r.err = r.c.err("step")
	}
	r.done = true
	return false
}

// Scan 把当前行按列顺序读入 *int64、*string、*[]byte、*float64、*bool、*any；NULL 读成零值（*any 为 nil）。
func (r *Rows) Scan(dest ...any) error {
	if n := int(C.sqlite3_column_count(r.st)); n != len(dest) {
		return fmt.Errorf("store: row has %d columns, got %d dests", n, len(dest))
	}
	for i, d := range dest {
		col := C.int(i)
		typ := C.sqlite3_column_type(r.st, col)
		switch p := d.(type) {
		case *int64:
			*p = int64(C.sqlite3_column_int64(r.st, col))
		case *bool:
			*p = C.sqlite3_column_int64(r.st, col) != 0
		case *float64:
			*p = float64(C.sqlite3_column_double(r.st, col))
		case *string:
			*p = string(r.bytes(col))
		case *[]byte:
			*p = r.bytes(col)
		case *any:
			switch typ {
			case C.SQLITE_NULL:
				*p = nil
			case C.SQLITE_INTEGER:
				*p = int64(C.sqlite3_column_int64(r.st, col))
			case C.SQLITE_FLOAT:
				*p = float64(C.sqlite3_column_double(r.st, col))
			case C.SQLITE_BLOB:
				*p = r.bytes(col)
			default:
				*p = string(r.bytes(col))
			}
		default:
			return fmt.Errorf("store: unsupported scan dest %T", d)
		}
	}
	return nil
}

func (r *Rows) bytes(col C.int) []byte {
	n := C.sqlite3_column_bytes(r.st, col)
	p := C.sqlite3_column_blob(r.st, col)
	if p == nil || n == 0 {
		return []byte{}
	}
	return C.GoBytes(p, n)
}

func (r *Rows) Err() error { return r.err }

func (r *Rows) Close() error {
	if r.st != nil {
		C.sqlite3_finalize(r.st)
		r.st = nil
	}
	return r.err
}

type backup struct {
	b   *C.sqlite3_backup
	src *conn
}

func (c *conn) backupInit(dst *conn) (*backup, error) {
	name := C.CString("main")
	defer C.free(unsafe.Pointer(name))
	b := C.sqlite3_backup_init(dst.db, name, c.db, name)
	if b == nil {
		return nil, dst.err("backup init")
	}
	return &backup{b: b, src: c}, nil
}

// step 拷贝至多 pages 页；done 为真表示拷完。源库忙时返回 busy=true，调用方稍后重试。
func (b *backup) step(pages int) (done, busy bool, err error) {
	switch rc := C.sqlite3_backup_step(b.b, C.int(pages)); rc {
	case C.SQLITE_DONE:
		return true, false, nil
	case C.SQLITE_OK:
		return false, false, nil
	case C.SQLITE_BUSY, C.SQLITE_LOCKED:
		return false, true, nil
	default:
		return false, false, b.src.err("backup step")
	}
}

func (b *backup) finish() { C.sqlite3_backup_finish(b.b) }
