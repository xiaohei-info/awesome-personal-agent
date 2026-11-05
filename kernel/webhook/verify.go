package webhook

import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// timestampSkew 是带时间戳的签名方案允许的时钟偏差，超出视为重放。
const timestampSkew = 5 * time.Minute

var errUnauthenticated = errors.New("signature check failed")

// SecretSource 保管 HMAC 方案的签名密钥明文（KeyFiles 满足）。其他凭据只存哈希，HMAC 校验要明文（§14.2）。
type SecretSource interface {
	// Secrets 给出某来源当前有效的签名密钥（轮换宽限期内新旧并存），在校验的事务里调用。
	Secrets(tx *store.Tx, sourceID string) ([][]byte, error)
	// Keep 在签发或轮换的事务里记下新密钥并保留仍有效的旧密钥；secret 为空表示不再保管该来源的明文。
	Keep(tx *store.Tx, sourceID, secret string) error
}

// verify 按方案数据校验请求；signed 表示通过的是签名（HMAC）而不只是共享令牌。
// 校验失败返回 errUnauthenticated，原因写进返回的 reason（只记元数据，不含请求内容）。
func (s *Service) verify(tx *store.Tx, sc packs.WebhookScheme, sourceID string, h map[string]string, body []byte, now time.Time) (signed bool, reason string, err error) {
	got := h[strings.ToLower(sc.SignatureHeader)]
	got, ok := strings.CutPrefix(got, sc.SignaturePrefix)
	if sc.SignatureHeader == "" || !ok || got == "" {
		return false, "signature header missing", errUnauthenticated
	}
	switch strings.ToLower(sc.Algorithm) {
	case "token": // 通用共享令牌：请求头里的值必须是这个来源的 webhook 凭据
		c, err := s.o.Auth.Resolve(tx, strings.TrimSpace(got))
		if err != nil || c.Kind != auth.Webhook || c.Scope != sourceID {
			return false, "token rejected", errUnauthenticated
		}
		return false, "", nil
	case "hmac-sha256":
		return s.hmacOK(tx, sha256.New, sc, sourceID, got, h, body, now)
	case "hmac-sha1":
		return s.hmacOK(tx, sha1.New, sc, sourceID, got, h, body, now)
	}
	return false, "unsupported algorithm " + sc.Algorithm, errUnauthenticated
}

func (s *Service) hmacOK(tx *store.Tx, hf func() hash.Hash, sc packs.WebhookScheme, sourceID, got string, h map[string]string, body []byte, now time.Time) (bool, string, error) {
	sig, err := hex.DecodeString(strings.TrimSpace(got))
	if err != nil {
		return false, "signature is not hex", errUnauthenticated
	}
	ts := ""
	if sc.TimestampHeader != "" {
		ts = h[strings.ToLower(sc.TimestampHeader)]
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil {
			return false, "timestamp missing or malformed", errUnauthenticated
		}
		if d := now.Sub(time.Unix(sec, 0)); d > timestampSkew || d < -timestampSkew {
			return false, "timestamp outside tolerance", errUnauthenticated
		}
	}
	base := body
	if tpl := sc.SignedPayload; tpl != "" && tpl != "body" {
		base = []byte(strings.NewReplacer("{timestamp}", ts, "{body}", string(body)).Replace(tpl))
	}
	secrets, err := s.o.Secrets.Secrets(tx, sourceID)
	if err != nil {
		return false, "", fmt.Errorf("webhook: secrets for %s: %w", sourceID, err)
	}
	for _, k := range secrets {
		m := hmac.New(hf, k)
		m.Write(base)
		if subtle.ConstantTimeCompare(m.Sum(nil), sig) == 1 {
			return true, "", nil
		}
	}
	return false, "signature mismatch", errUnauthenticated
}
