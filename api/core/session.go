package core

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"net/http"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// SessionCookie 是 Web 会话 Cookie 名。
	SessionCookie = "apa_session"
	// DefaultSessionTTL 是会话绝对有效期；单用户系统，不做滑动续期。
	DefaultSessionTTL = 7 * 24 * time.Hour

	pbkdf2Iterations = 600_000 // OWASP 对 PBKDF2-HMAC-SHA256 的建议值
	minPasswordLen   = 8
)

// testIterations 仅测试用，降低口令哈希成本；生产恒为 0 即用 pbkdf2Iterations。
var testIterations int

// Module 是本模块在存储层登记的迁移：本人口令（单行）和 Web 会话。会话只存 ID 的哈希。
var Module = store.Module{Name: "apicore", Migrations: []store.Migration{{
	Version: 1, Name: "create-owner-and-session",
	SQL: `CREATE TABLE apicore_owner (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	salt BLOB NOT NULL,
	iterations INTEGER NOT NULL,
	hash BLOB NOT NULL
);
CREATE TABLE apicore_session (
	id_hash TEXT PRIMARY KEY,
	csrf TEXT NOT NULL,
	created_at INTEGER NOT NULL,
	expires_at INTEGER NOT NULL
) WITHOUT ROWID;`,
}, {
	// 账本结构版本 3（§14.13）：会话带最近使用时间（空闲有效期）、来源地址与浏览器标识。
	// 升级前的会话没有这些信息，按设计整体失效：expires_at 置 0（已过期），行保留不删。
	Version: 2, Ledger: 3, Name: "ledger-v3-session-validity",
	SQL: `ALTER TABLE apicore_session ADD COLUMN last_used_at INTEGER NOT NULL DEFAULT 0;
ALTER TABLE apicore_session ADD COLUMN addr TEXT NOT NULL DEFAULT '';
ALTER TABLE apicore_session ADD COLUMN agent TEXT NOT NULL DEFAULT '';
UPDATE apicore_session SET expires_at = 0;`,
}}}

func iterations() int {
	if testIterations > 0 {
		return testIterations
	}
	return pbkdf2Iterations
}

// SetOwnerPassword 设置或更换本人的 Web 登录口令（apa init 调用）。更换同时作废所有现存会话。
func SetOwnerPassword(tx *store.Tx, password string) error {
	if len(password) < minPasswordLen {
		return domain.NewError(domain.CodeInvalidRequest, "password is too short", "use at least 8 characters")
	}
	salt, err := randomBytes(16)
	if err != nil {
		return err
	}
	n := iterations()
	h, err := pbkdf2.Key(sha256.New, password, salt, n, 32)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO apicore_owner (id, salt, iterations, hash) VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET salt = excluded.salt, iterations = excluded.iterations, hash = excluded.hash`, salt, n, h); err != nil {
		return err
	}
	_, err = tx.Exec("DELETE FROM apicore_session")
	return err
}

// checkPassword 校验口令；未设置口令与口令错误对外无区别。
func checkPassword(tx *store.Tx, password string) (bool, error) {
	rows, err := tx.Query("SELECT salt, iterations, hash FROM apicore_owner WHERE id = 1")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return false, rows.Err()
	}
	var salt, want []byte
	var n int64
	if err := rows.Scan(&salt, &n, &want); err != nil {
		return false, err
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, int(n), len(want))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}

func randomBytes(n int) ([]byte, error) {
	b := make([]byte, n)
	_, err := rand.Read(b)
	return b, err
}

func randomToken() (string, error) {
	b, err := randomBytes(32)
	return base64.RawURLEncoding.EncodeToString(b), err
}

func hashID(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:])
}

// webCaller 是 Web 上的本人：最高权限入口（§14.13），由命令管线按 LevelWeb 授权。
var webCaller = command.Caller{
	Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb},
	Level: authz.LevelWeb,
}

func (s *Server) resolveSession(ctx context.Context, id string) (*Principal, error) {
	if id == "" {
		return nil, errUnauthenticated()
	}
	var csrf string
	var expires int64
	found := false
	err := s.d.Store.View(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT csrf, expires_at FROM apicore_session WHERE id_hash = ?", hashID(id))
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			found = true
			if err := rows.Scan(&csrf, &expires); err != nil {
				return err
			}
		}
		return rows.Err()
	})
	if err != nil {
		return nil, err
	}
	if !found || !s.d.Now().Before(time.Unix(0, expires)) {
		return nil, errUnauthenticated()
	}
	return &Principal{Identity: command.Trusted(webCaller), Caller: webCaller, Web: true, csrf: csrf}, nil
}

// ---- /auth/login、/auth/logout ----

func (s *Server) authRoutes(r *Router) {
	r.Handle(Route{Method: "POST", Path: "/auth/login", Auth: AuthNone, MaxBody: 4 << 10, Handler: s.login})
	r.Handle(Route{Method: "POST", Path: "/auth/logout", Handler: s.logout})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request, _ *Principal) error {
	if err := checkOrigin(r); err != nil { // 防登录 CSRF
		return err
	}
	// 暴露到本机之外必须经 HTTPS（§14.13）：非本机来源的明文请求不发会话。
	if !secureTransport(r) && !isLoopbackAddr(r.RemoteAddr) {
		return domain.NewError(domain.CodePermissionDenied, "login over plain HTTP from outside this host is refused", "reach the server through HTTPS (reverse proxy or tunnel)")
	}
	var in struct {
		Password string `json:"password"`
	}
	if err := DecodeJSON(r, &in); err != nil {
		return err
	}
	id, err := randomToken()
	if err != nil {
		return err
	}
	csrf, err := randomToken()
	if err != nil {
		return err
	}
	now := s.d.Now()
	expires := now.Add(s.d.SessionTTL)
	ok := false
	err = s.d.Store.Update(r.Context(), func(tx *store.Tx) error {
		var err error
		if ok, err = checkPassword(tx, in.Password); err != nil || !ok {
			return err
		}
		if _, err := tx.Exec("DELETE FROM apicore_session WHERE expires_at <= ?", now.UnixNano()); err != nil {
			return err
		}
		_, err = tx.Exec("INSERT INTO apicore_session (id_hash, csrf, created_at, expires_at) VALUES (?, ?, ?, ?)",
			hashID(id), csrf, now.UnixNano(), expires.UnixNano())
		return err
	})
	if err != nil {
		return err
	}
	if !ok {
		return errUnauthenticated()
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: id, Path: "/", Expires: expires,
		HttpOnly: true, Secure: secureTransport(r), SameSite: http.SameSiteStrictMode})
	return WriteJSON(w, http.StatusOK, map[string]any{"csrf_token": csrf, "expires_at": expires.UTC().Format(time.RFC3339)})
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if !p.Web {
		return domain.NewError(domain.CodeInvalidRequest, "logout applies to Web sessions only", "revoke tokens in Web settings")
	}
	c, _ := r.Cookie(SessionCookie) // authenticate 已确认存在
	if err := s.d.Store.Update(r.Context(), func(tx *store.Tx) error {
		_, err := tx.Exec("DELETE FROM apicore_session WHERE id_hash = ?", hashID(c.Value))
		return err
	}); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: secureTransport(r), SameSite: http.SameSiteStrictMode})
	return WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
