// Package secrets 是连接凭据的保管库（设计 §5）：密文在账本 secrets_value 表，一行一个凭据字段；
// 主密钥是数据目录里单独的 0600 文件。AES-256-GCM，附加认证数据是「属主 + 字段名」，密文挪到别的行解不开。
//
// 本包是内核基础设施，不依赖业务模块。读出明文只有 Reveal 一个入口，红线检查只允许外呼调用器（kernel/outcall）引用它；
// 其余调用方只能写入、删除、看哪些字段可用（不解密）。主密钥缺失时生成新密钥照常启动，旧密文因 key_id 不符视为不可用，
// 由连接状态提示本人重新填写（§5.5）。
package secrets

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// KeyFile 是主密钥在数据目录里的文件名。
const KeyFile = "master.key"

// Module 登记密文表（账本结构版本 7，只增表）。
var Module = store.Module{Name: "secrets", Migrations: []store.Migration{{
	Version: 1, Ledger: 7, Name: "ledger-v7-secrets",
	SQL: `CREATE TABLE secrets_value (
    owner TEXT NOT NULL,
    field TEXT NOT NULL,
    key_id TEXT NOT NULL,
    nonce TEXT NOT NULL,
    ciphertext TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    PRIMARY KEY (owner, field)
) WITHOUT ROWID;`,
}}}

// Vault 持有主密钥；零值不可用，用 Open 或 Generate 得到。
type Vault struct {
	key  []byte
	id   string
	path string
}

func keyID(key []byte) string {
	h := sha256.Sum256(append([]byte("apa-secrets-key-id\x00"), key...))
	return hex.EncodeToString(h[:8])
}

// ID 是主密钥的标识（不可逆推密钥），密文行据它判断是不是用当前密钥加密的。
func (v *Vault) ID() string { return v.id }

// Path 是主密钥文件位置。
func (v *Vault) Path() string { return v.path }

func readKey(path string) (*Vault, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != 32 {
		return nil, fmt.Errorf("secrets: %s is not a 32-byte master key", path)
	}
	return &Vault{key: b, id: keyID(b), path: path}, nil
}

// writeKey 以 0600 新建密钥文件；文件已存在时失败，不覆盖。
func writeKey(path string) (*Vault, error) {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	if _, err := f.Write(key); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	if err := errors.Join(f.Sync(), f.Close()); err != nil {
		os.Remove(path)
		return nil, err
	}
	return &Vault{key: key, id: keyID(key), path: path}, nil
}

// Open 读取 home 里的主密钥；不存在时生成一个（新部署与升级后第一次启动，§5.5），created 为真。
// 若上次 Rekey 停在半路（旁边留着 .new 文件），按账本里的 key_id 判断事务是否已提交，把它走完。
func Open(ctx context.Context, st *store.Store, home string) (v *Vault, created bool, err error) {
	path := filepath.Join(home, KeyFile)
	if err := finishRekey(ctx, st, path); err != nil {
		return nil, false, err
	}
	v, err = readKey(path)
	if errors.Is(err, fs.ErrNotExist) {
		v, err = writeKey(path)
		return v, err == nil, err
	}
	return v, false, err
}

// Load 只读地读取 home 里的主密钥（自检用）；不存在返回 fs.ErrNotExist，不生成。
func Load(home string) (*Vault, error) { return readKey(filepath.Join(home, KeyFile)) }

// Generate 生成一个只在内存里的密钥（测试用，不写文件）。
func Generate() *Vault {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	return &Vault{key: key, id: keyID(key)}
}

func aad(owner, field string) []byte { return []byte(owner + "\x00" + field) }

func (v *Vault) aead() (cipher.AEAD, error) {
	b, err := aes.NewCipher(v.key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

func (v *Vault) seal(owner, field, plain string) (nonce, ct string, err error) {
	g, err := v.aead()
	if err != nil {
		return "", "", err
	}
	n := make([]byte, g.NonceSize())
	if _, err := rand.Read(n); err != nil {
		return "", "", err
	}
	enc := base64.StdEncoding.EncodeToString
	return enc(n), enc(g.Seal(nil, n, []byte(plain), aad(owner, field))), nil
}

func (v *Vault) open(owner, field, nonce, ct string) (string, error) {
	n, err := base64.StdEncoding.DecodeString(nonce)
	if err != nil {
		return "", err
	}
	c, err := base64.StdEncoding.DecodeString(ct)
	if err != nil {
		return "", err
	}
	g, err := v.aead()
	if err != nil {
		return "", err
	}
	p, err := g.Open(nil, n, c, aad(owner, field))
	return string(p), err
}

// Put 在调用方事务里加密写入（覆盖）一个凭据字段。
func (v *Vault) Put(tx *store.Tx, owner, field, plain string, now time.Time) error {
	nonce, ct, err := v.seal(owner, field, plain)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO secrets_value (owner, field, key_id, nonce, ciphertext, updated_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT(owner, field) DO UPDATE SET key_id = excluded.key_id, nonce = excluded.nonce, ciphertext = excluded.ciphertext, updated_at = excluded.updated_at`,
		owner, field, v.id, nonce, ct, now.UnixNano())
	return err
}

// Delete 删除一个凭据字段；不存在不算错。
func Delete(tx *store.Tx, owner, field string) error {
	_, err := tx.Exec(`DELETE FROM secrets_value WHERE owner = ? AND field = ?`, owner, field)
	return err
}

// DeleteOwner 删除属主的全部凭据字段（连接删除时）。
func DeleteOwner(tx *store.Tx, owner string) error {
	_, err := tx.Exec(`DELETE FROM secrets_value WHERE owner = ?`, owner)
	return err
}

// Usable 返回属主已保存的凭据字段及其是否可用（用当前主密钥加密）；不解密。
func (v *Vault) Usable(tx *store.Tx, owner string) (map[string]bool, error) {
	out := map[string]bool{}
	r, err := tx.Query(`SELECT field, key_id FROM secrets_value WHERE owner = ?`, owner)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	for r.Next() {
		var f, id string
		if err := r.Scan(&f, &id); err != nil {
			return nil, err
		}
		out[f] = id == v.id
	}
	return out, r.Err()
}

type row struct{ owner, field, keyID, nonce, ct string }

func rows(tx *store.Tx, where string, args ...any) ([]row, error) {
	r, err := tx.Query(`SELECT owner, field, key_id, nonce, ciphertext FROM secrets_value `+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []row
	for r.Next() {
		var x row
		if err := r.Scan(&x.owner, &x.field, &x.keyID, &x.nonce, &x.ct); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, r.Err()
}

// Reveal 解出属主的指定字段明文。解不开（主密钥换过或丢失）的字段不出现在结果里，由调用方按缺凭据处理。
// 只允许外呼调用器在调用连接插件前使用（红线检查 §4.4）。
func (v *Vault) Reveal(tx *store.Tx, owner string, fields []string) (map[string]string, error) {
	want := map[string]bool{}
	for _, f := range fields {
		want[f] = true
	}
	rs, err := rows(tx, "WHERE owner = ?", owner)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, x := range rs {
		if !want[x.field] || x.keyID != v.id {
			continue
		}
		if p, err := v.open(x.owner, x.field, x.nonce, x.ct); err == nil {
			out[x.field] = p
		}
	}
	return out, nil
}

// Stats 是自检看到的密文概况：只有数量。
type Stats struct {
	Rows       int `json:"rows"`
	Unreadable int `json:"unreadable"` // 用当前主密钥解不开（含 key_id 不符）
	OtherKey   int `json:"other_key"`  // key_id 不是当前主密钥（换钥没走完或主密钥丢失）
}

// Inspect 逐行试解（结果丢弃），只返回数量。
func (v *Vault) Inspect(tx *store.Tx) (Stats, error) {
	rs, err := rows(tx, "")
	if err != nil {
		return Stats{}, err
	}
	s := Stats{Rows: len(rs)}
	for _, x := range rs {
		if x.keyID != v.id {
			s.OtherKey++
			s.Unreadable++
			continue
		}
		if _, err := v.open(x.owner, x.field, x.nonce, x.ct); err != nil {
			s.Unreadable++
		}
	}
	return s, nil
}

// Rekey 换主密钥（服务停止时由 apa secrets rekey 调用，§5.5）：新密钥先写到旁边的 .new 文件，
// 再在一个事务里把全部可解的行重新加密，最后原子替换密钥文件。中途退出时由下次 Open 走完。
// 用旧密钥解不开的行原样保留（它们本来就不可用），返回重新加密的行数。
func Rekey(ctx context.Context, st *store.Store, old *Vault) (int, *Vault, error) {
	tmp := old.path + ".new"
	nv, err := writeKey(tmp)
	if err != nil {
		return 0, nil, err
	}
	n := 0
	err = st.Update(ctx, func(tx *store.Tx) error {
		rs, err := rows(tx, "")
		if err != nil {
			return err
		}
		for _, x := range rs {
			if x.keyID != old.id {
				continue
			}
			p, err := old.open(x.owner, x.field, x.nonce, x.ct)
			if err != nil {
				continue
			}
			nonce, ct, err := nv.seal(x.owner, x.field, p)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE secrets_value SET key_id = ?, nonce = ?, ciphertext = ? WHERE owner = ? AND field = ?`,
				nv.id, nonce, ct, x.owner, x.field); err != nil {
				return err
			}
			n++
		}
		return nil
	})
	if err != nil {
		os.Remove(tmp)
		return 0, nil, err
	}
	if err := os.Rename(tmp, old.path); err != nil {
		return n, nil, err // 已提交：下次 Open 会认出并走完
	}
	nv.path = old.path
	return n, nv, nil
}

// finishRekey 处理上次 Rekey 留下的 .new 文件：账本里已有行用它加密（事务已提交）就替换密钥文件，否则丢弃。
func finishRekey(ctx context.Context, st *store.Store, path string) error {
	tmp := path + ".new"
	nv, err := readKey(tmp)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return os.Remove(tmp) // 没写完的新密钥：事务不可能用过它
	}
	used := false
	if err := st.View(ctx, func(tx *store.Tx) error {
		rs, err := rows(tx, "WHERE key_id = ? LIMIT 1", nv.id)
		used = len(rs) > 0
		return err
	}); err != nil {
		return err
	}
	if used {
		return os.Rename(tmp, path)
	}
	return os.Remove(tmp)
}
