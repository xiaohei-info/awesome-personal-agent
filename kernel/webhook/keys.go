package webhook

import (
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// KeyFiles 是 HMAC 接收密钥的明文保管处（设计 §14.2）：<Dir>/<来源 id>，目录 0700、文件 0600，每行一个密钥，新的在前。
// 文件只存明文，有效性仍以账本为准：读取时逐个按凭据哈希核对，吊销或过了轮换宽限期的立即不再可用，不必等文件更新。
// 明文不进账本与事件，也没有任何 API 读它。
type KeyFiles struct {
	Dir  string
	Auth *auth.Auth
}

func (k KeyFiles) path(sourceID string) string { return filepath.Join(k.Dir, sourceID) }

// Secrets 返回文件里仍是该来源有效接收密钥的明文；文件不存在返回空。
func (k KeyFiles) Secrets(tx *store.Tx, sourceID string) ([][]byte, error) {
	b, err := os.ReadFile(k.path(sourceID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, s := range strings.Fields(string(b)) {
		if c, err := k.Auth.Resolve(tx, s); err == nil && c.Kind == auth.Webhook && c.Scope == sourceID {
			out = append(out, []byte(s))
		}
	}
	return out, nil
}

// Keep 在签发或轮换的事务里写入新密钥，并保留仍有效的旧密钥（轮换宽限期内新旧并存）；失效的随之删掉。
// secret 为空时删除文件。先写临时文件再改名，崩溃不留半个文件；事务若随后回滚，新密钥在账本里不存在，读取时被滤掉。
func (k KeyFiles) Keep(tx *store.Tx, sourceID, secret string) error {
	if secret == "" {
		if err := os.Remove(k.path(sourceID)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	old, err := k.Secrets(tx, sourceID)
	if err != nil {
		return err
	}
	lines := []string{secret}
	for _, s := range old {
		if string(s) != secret {
			lines = append(lines, string(s))
		}
	}
	if err := os.MkdirAll(k.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Chmod(k.Dir, 0o700); err != nil { // MkdirAll 受 umask 影响
		return err
	}
	f, err := os.CreateTemp(k.Dir, ".tmp-"+sourceID+"-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString(strings.Join(lines, "\n") + "\n"); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), k.path(sourceID)) // CreateTemp 建的文件已是 0600
}

// hmacScheme：签名方案要明文密钥；共享 token 方案用凭据本身，只存哈希。
func hmacScheme(algorithm string) bool { return strings.HasPrefix(strings.ToLower(algorithm), "hmac-") }

// StoreKey 在签发或轮换 webhook 接收密钥的事务里调用（api/sources）：来源的校验方案是 HMAC 时把明文交给保管处，
// 否则删掉可能遗留的明文。改了来源的校验方案后要轮换一次密钥才会按新方案保管。
func (s *Service) StoreKey(tx *store.Tx, sourceID, secret string) error {
	src, err := registry.Get(tx, sourceID)
	if err != nil {
		return err
	}
	cfg, err := parseConfig(src)
	if err != nil {
		return err
	}
	if sc, ok := s.scheme(cfg.Scheme); !ok || !hmacScheme(sc.Algorithm) {
		secret = ""
	}
	return s.o.Secrets.Keep(tx, sourceID, secret)
}
