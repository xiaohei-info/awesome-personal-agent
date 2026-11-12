package core

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// SessionCookie 是 Web 会话 Cookie 名。
	SessionCookie = "apa_session"
	// DefaultSessionTTL 是会话绝对有效期：从登录起算，持续使用也不续期（§14.13）。
	DefaultSessionTTL = 30 * 24 * time.Hour
	// DefaultSessionIdle 是会话空闲有效期：超过这么久没有使用即失效。
	DefaultSessionIdle = 7 * 24 * time.Hour
	// DefaultLoginDelay 是全部地址失败过多时每次登录尝试的延迟应答时长。
	DefaultLoginDelay = 2 * time.Second

	pbkdf2Iterations = 600_000 // OWASP 对 PBKDF2-HMAC-SHA256 的建议值
	// MinPasswordLen 是口令最少字符数；只在设置与修改时检查，已有的短口令继续可登录。
	MinPasswordLen = 12
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
func SetOwnerPassword(tx *store.Tx, password string) error { return setOwner(tx, password, "") }

// setOwner 写入新口令并作废除 keep（会话 id_hash，可为空）之外的全部会话。长度按字符数算。
func setOwner(tx *store.Tx, password, keep string) error {
	if utf8.RuneCountInString(password) < MinPasswordLen {
		return domain.NewError(domain.CodeInvalidRequest, "password is too short", "use at least 12 characters")
	}
	return storeOwner(tx, password, keep)
}

// storeOwner 不检查长度：长度规则只在设置与修改时由 setOwner 检查，已有的短口令不追溯。
func storeOwner(tx *store.Tx, password, keep string) error {
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
	_, err = tx.Exec("DELETE FROM apicore_session WHERE id_hash <> ?", keep)
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

// touchEvery：最近使用时间的写入粒度，避免每个请求都写账本。
const touchEvery = time.Minute

func (s *Server) resolveSession(ctx context.Context, id string) (*Principal, error) {
	if id == "" {
		return nil, errUnauthenticated()
	}
	var csrf string
	var used int64
	found := false
	now := s.d.Now()
	err := s.d.Store.View(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT csrf, expires_at, last_used_at FROM apicore_session WHERE id_hash = ?", hashID(id))
		if err != nil {
			return err
		}
		defer rows.Close()
		var expires int64
		if rows.Next() {
			if err := rows.Scan(&csrf, &expires, &used); err != nil {
				return err
			}
			// 绝对有效期与空闲有效期任一到期即失效；升级前的会话 expires_at 为 0，永远过期。
			found = now.Before(time.Unix(0, expires)) && now.Before(time.Unix(0, used).Add(s.d.SessionIdle))
		}
		return rows.Err()
	})
	if err == nil && found && now.Sub(time.Unix(0, used)) >= touchEvery { // 读多写少：至多每分钟写一次
		err = s.d.Store.Update(ctx, func(tx *store.Tx) error {
			_, err := tx.Exec("UPDATE apicore_session SET last_used_at = ? WHERE id_hash = ?", now.UnixNano(), hashID(id))
			return err
		})
	}
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errUnauthenticated()
	}
	return &Principal{Identity: command.Trusted(webCaller), Caller: webCaller, Web: true, csrf: csrf, session: hashID(id)}, nil
}

// audit 在 tx 内记一条登录相关的审计 Event（对外事件类型，附录 B.4）。载荷不得含口令。
func (s *Server) audit(tx *store.Tx, typ, reason string, payload map[string]any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: typ, Object: "owner", Reason: reason, Payload: body, At: s.d.Now(),
		Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}})
	return err
}

// 登录相关的审计事件类型。
const (
	evLogin           = domain.EvAuthLogin
	evLoginLocked     = domain.EvAuthLoginLocked
	evPasswordChanged = domain.EvAuthPasswordChanged
	evSessionRevoked  = domain.EvAuthSessionRevoked
)

// clientAddr 是请求的来源地址（不含端口）。经本机反向代理时取代理追加的最后一个 X-Forwarded-For；
// 非本机来源自称的 X-Forwarded-For 不可信，忽略。部署要求：本机反向代理必须追加（而不是透传）客户端的 X-Forwarded-For。
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isLoopbackAddr(r.RemoteAddr) {
		if xs := r.Header.Values("X-Forwarded-For"); len(xs) > 0 {
			parts := strings.Split(xs[len(xs)-1], ",")
			if v := strings.TrimSpace(parts[len(parts)-1]); v != "" {
				return v
			}
		}
	}
	return host
}

// guardAttempt 在校验口令之前调用，成功时返回的 release 必须在这次尝试的结果（含失败计数）记完后调用。
// 来源地址处于锁定期，或并发的未决尝试已用满名额，返回 429（login_locked，带 Retry-After）；
// 全部地址失败过多时延迟应答，正确口令也一样。
func (s *Server) guardAttempt(w http.ResponseWriter, r *http.Request, addr string) (release func(), err error) {
	now := s.d.Now()
	if left := s.lim.begin(addr, now); left > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(left.Seconds())+1))
		return nil, domain.NewError(domain.CodeLoginLocked, "too many failed attempts from this address", "wait for the lock to expire and try again")
	}
	release = func() { s.lim.end(addr) }
	if s.lim.flooded(now) {
		t := time.NewTimer(s.d.LoginDelay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-r.Context().Done():
			release()
			return nil, r.Context().Err()
		}
	}
	return release, nil
}

// attemptFailed 记一次失败的口令校验：计数、必要时锁定并记审计 Event，窗口内失败过多时记一条 health.alert。
func (s *Server) attemptFailed(ctx context.Context, addr string) error {
	now := s.d.Now()
	locked, until, flood := s.lim.failure(addr, now)
	if !locked && flood == 0 {
		return nil
	}
	return s.d.Store.Update(ctx, func(tx *store.Tx) error {
		if locked {
			if err := s.audit(tx, evLoginLocked, "login locked after repeated failures",
				map[string]any{"addr": addr, "until": until.UTC().Format(time.RFC3339)}); err != nil {
				return err
			}
		}
		if flood > 0 {
			body, _ := json.Marshal(map[string]any{"kind": "login_flood", "failures": flood, "window_minutes": int(floodWindow.Minutes())})
			_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: "auth", Reason: "too many failed logins across addresses",
				Payload: body, At: now, Actor: domain.Actor{Type: domain.ActorSystem, Principal: "auth", Via: domain.ViaWeb}})
			return err
		}
		return nil
	})
}

// ---- /auth/login、/auth/logout ----

func (s *Server) authRoutes(r *Router) {
	r.Handle(Route{Method: "POST", Path: "/auth/login", Auth: AuthNone, MaxBody: 4 << 10, Handler: s.login})
	r.Handle(Route{Method: "POST", Path: "/auth/logout", Handler: s.logout})
	r.Handle(Route{Method: "POST", Path: "/auth/password", MaxBody: 4 << 10, Handler: s.changePassword})
	r.Handle(Route{Method: "GET", Path: "/auth/sessions", Handler: s.listSessions})
	r.Handle(Route{Method: "DELETE", Path: "/auth/sessions/{id}", Handler: s.revokeSession})
	r.Handle(Route{Method: "DELETE", Path: "/auth/sessions", Handler: s.revokeAllSessions})
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
	addr := clientAddr(r)
	release, err := s.guardAttempt(w, r, addr)
	if err != nil {
		return err
	}
	defer release()
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
		if _, err := tx.Exec("DELETE FROM apicore_session WHERE expires_at <= ? OR last_used_at <= ?",
			now.UnixNano(), now.Add(-s.d.SessionIdle).UnixNano()); err != nil {
			return err
		}
		agent := r.UserAgent()
		if len(agent) > 256 {
			agent = strings.ToValidUTF8(agent[:256], "")
		}
		if _, err := tx.Exec("INSERT INTO apicore_session (id_hash, csrf, created_at, expires_at, last_used_at, addr, agent) VALUES (?, ?, ?, ?, ?, ?, ?)",
			hashID(id), csrf, now.UnixNano(), expires.UnixNano(), now.UnixNano(), addr, agent); err != nil {
			return err
		}
		return s.audit(tx, evLogin, "login succeeded", map[string]any{"addr": addr, "agent": agent})
	})
	if err != nil {
		return err
	}
	if !ok {
		if err := s.attemptFailed(r.Context(), addr); err != nil {
			return err
		}
		return errUnauthenticated()
	}
	s.lim.success(addr)
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: id, Path: "/", Expires: expires,
		HttpOnly: true, Secure: secureTransport(r), SameSite: http.SameSiteStrictMode})
	return WriteJSON(w, http.StatusOK, map[string]any{"csrf_token": csrf, "expires_at": expires.UTC().Format(time.RFC3339)})
}

func requireWeb(p *Principal) error {
	if !p.Web {
		return domain.NewError(domain.CodeWebOnly, "sessions and the login password can only be managed by the owner in the Web UI", "ask the owner to do it in Web settings")
	}
	return nil
}

// changePassword 改口令：需当前口令，新口令至少 12 个字符；成功后除当前会话外的会话全部失效。
// 当前口令错误与登录失败共用同一套限速，劫持到会话的人不能借此绕过锁定去猜口令。
func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := requireWeb(p); err != nil {
		return err
	}
	var in struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := DecodeJSON(r, &in); err != nil {
		return err
	}
	addr := clientAddr(r)
	release, err := s.guardAttempt(w, r, addr)
	if err != nil {
		return err
	}
	defer release()
	wrong := false
	err = s.d.Store.Update(r.Context(), func(tx *store.Tx) error {
		ok, err := checkPassword(tx, in.Current)
		if err != nil {
			return err
		}
		if !ok {
			wrong = true
			return nil
		}
		if err := setOwner(tx, in.New, p.session); err != nil {
			return err
		}
		return s.audit(tx, evPasswordChanged, "login password changed; other sessions revoked", map[string]any{"addr": addr})
	})
	if err != nil {
		return err
	}
	if wrong {
		if err := s.attemptFailed(r.Context(), addr); err != nil {
			return err
		}
		return domain.NewError(domain.CodePermissionDenied, "the current password is wrong", "enter the current login password")
	}
	s.lim.success(addr)
	return WriteJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

type sessionInfo struct {
	ID         string    `json:"id"`
	CreatedAt  time.Time `json:"created_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	Addr       string    `json:"addr"`
	Agent      string    `json:"agent"`
	Current    bool      `json:"current"`
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := requireWeb(p); err != nil {
		return err
	}
	now := s.d.Now()
	out := []sessionInfo{}
	err := s.d.Store.View(r.Context(), func(tx *store.Tx) error {
		rows, err := tx.Query(`SELECT id_hash, created_at, last_used_at, addr, agent FROM apicore_session
			WHERE expires_at > ? AND last_used_at > ? ORDER BY created_at, id_hash`, now.UnixNano(), now.Add(-s.d.SessionIdle).UnixNano())
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var i sessionInfo
			var created, used int64
			if err := rows.Scan(&i.ID, &created, &used, &i.Addr, &i.Agent); err != nil {
				return err
			}
			i.CreatedAt, i.LastUsedAt, i.Current = time.Unix(0, created).UTC(), time.Unix(0, used).UTC(), i.ID == p.session
			out = append(out, i)
		}
		return rows.Err()
	})
	if err != nil {
		return err
	}
	return WriteJSON(w, http.StatusOK, map[string]any{"sessions": out})
}

func (s *Server) revokeSession(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := requireWeb(p); err != nil {
		return err
	}
	id := r.PathValue("id")
	n, err := s.revoke(r.Context(), "WHERE id_hash = ?", id)
	if err != nil {
		return err
	}
	if n == 0 {
		return domain.NewError(domain.CodeNotFound, "session not found", "list sessions and retry")
	}
	return WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": n})
}

func (s *Server) revokeAllSessions(w http.ResponseWriter, r *http.Request, p *Principal) error {
	if err := requireWeb(p); err != nil {
		return err
	}
	n, err := s.revoke(r.Context(), "")
	if err != nil {
		return err
	}
	return WriteJSON(w, http.StatusOK, map[string]any{"ok": true, "revoked": n})
}

// revoke 删除 where 命中的会话（空表示全部）并在同一事务记一条审计 Event；返回删除条数，0 条不记 Event。
func (s *Server) revoke(ctx context.Context, where string, args ...any) (n int64, err error) {
	err = s.d.Store.Update(ctx, func(tx *store.Tx) error {
		if n, err = tx.Exec("DELETE FROM apicore_session "+where, args...); err != nil || n == 0 {
			return err
		}
		return s.audit(tx, evSessionRevoked, "sessions revoked", map[string]any{"count": n, "all": where == ""})
	})
	return
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
