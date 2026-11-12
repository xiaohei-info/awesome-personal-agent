// Package core 是 HTTP 服务骨干（设计 §14.13、§17.1，附录 B.2、B.3）：监听、认证（Bearer 与 Web 会话 Cookie）、
// CSRF 防护、统一错误响应和 /v1 路由注册。路由里没有业务规则——授权与校验留在内核命令管线（kernel/command）。
//
// 业务分组各自在自己的文件里 init() 调 Register，不改中心路由表；认证分组（/auth/login、/auth/logout）也是这样接入的。
package core

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// APIPrefix 是版本前缀；/ingest 等外部系统 URL 经 HandleRoot 注册，不带它（§17.1）。
	APIPrefix = "/v1"
	// DefaultMaxBody 是请求体默认上限；需要更大（如附件）的路由用 Route.MaxBody 声明。
	DefaultMaxBody int64 = 1 << 20
	// CSRFHeader 是 Cookie 会话的写请求必须携带的令牌头。
	CSRFHeader = "X-CSRF-Token"
	// EvidenceHeader 携带 delegate 代理操作的本人原话（§14.3、§14.5）；骨干统一读入 Principal.Evidence。
	EvidenceHeader = "X-APA-Evidence"
	// SessionKindHeader 携带接入方自报的会话类型（direct、group、unknown；§14.5）；缺失即未上报。
	SessionKindHeader = "X-APA-Session-Kind"
	// GrantHeader 携带 delegate 代理操作的授权单 id，与 EvidenceHeader 二选一（§14.5）；骨干统一读入 Principal.Grant。
	GrantHeader = "X-APA-Grant"
	// ownerPrincipal 是本人的操作者标识（单用户系统，与 Web 会话的 Actor 一致），delegate 凭据代其行事。
	// shortcut: 单用户系统写死本人为 owner，引入多用户时改由凭据携带。
	ownerPrincipal = "owner"
)

// AuthMode 说明路由由骨干认证，还是自己认证。
type AuthMode int

const (
	// AuthRequired：必须带有效 Bearer 或 Web 会话；Handler 收到非空 Principal。
	AuthRequired AuthMode = iota
	// AuthNone：骨干不认证，Principal 为 nil。用于登录，以及凭据另有形态的路由（webhook 接收密钥、一次性表单令牌）。
	AuthNone
)

// Principal 是认证结果。Identity 交给 command.Executor.Do，由命令管线在事务内再次校验并授权。
type Principal struct {
	Identity command.Identity
	Caller   command.Caller
	Web      bool   // 来自 Web 会话 Cookie
	Evidence string // EvidenceHeader 的值；写命令的 command.Request.Evidence 取自这里
	Grant    string // GrantHeader 的值；写命令的 command.Request.Grant 取自这里，二选一与同时携带的判定都在内核
	WebURL   string // 请求对象在 Web 的链接；写命令的 command.Request.WebURL 取自这里，随 20/22/23 拒绝返回
	csrf     string
	session  string // Web 会话的 id_hash；Bearer 为空
}

// Handler 处理一个请求。返回错误由骨干渲染成统一错误体；返回 nil 表示 Handler 已写完响应。
type Handler func(w http.ResponseWriter, r *http.Request, p *Principal) error

// Route 是一条路由。
type Route struct {
	Method  string // GET、POST……
	Path    string // 以 / 开头，可含 {name} 通配（net/http 模式）
	Auth    AuthMode
	MaxBody int64 // 0 用 DefaultMaxBody
	Handler Handler
	// Page：该 GET 路径同时是人点开的链接。浏览器导航（Accept 含 text/html）交给 Web 的前端页面，
	// 页面自己的 fetch（Accept: application/json）仍走 Handler。
	Page bool
}

// RouteGroup 登记一组路由。
type RouteGroup func(*Router)

// Router 收集路由，并在启动时一次性校验。
type Router struct {
	routes []Route
	root   []bool // 与 routes 平行：是否不带版本前缀
	errs   []error
}

// Handle 登记 /v1 下的路由。
func (r *Router) Handle(rt Route) { r.add(rt, false) }

// HandleRoot 登记不带版本前缀的路由（如 /ingest/{source_id}）。
func (r *Router) HandleRoot(rt Route) { r.add(rt, true) }

func (r *Router) add(rt Route, root bool) {
	switch {
	case rt.Method == "" || strings.ContainsAny(rt.Method, " \t"):
		r.errs = append(r.errs, fmt.Errorf("core: route %q has no valid method", rt.Path))
	case !strings.HasPrefix(rt.Path, "/"):
		r.errs = append(r.errs, fmt.Errorf("core: route path %q must start with /", rt.Path))
	case rt.Handler == nil:
		r.errs = append(r.errs, fmt.Errorf("core: route %s %s has no handler", rt.Method, rt.Path))
	default:
		r.routes = append(r.routes, rt)
		r.root = append(r.root, root)
	}
}

type namedGroup struct {
	name  string
	group RouteGroup
}

var (
	regMu    sync.Mutex
	registry []namedGroup
)

// Register 登记一个路由分组；各分组在自己文件的 init() 里调用。同名重复是编程错误，立即 panic。
func Register(name string, g RouteGroup) {
	regMu.Lock()
	defer regMu.Unlock()
	for _, n := range registry {
		if n.name == name {
			panic("core: duplicate route group " + name)
		}
	}
	registry = append(registry, namedGroup{name, g})
}

// Deps 是 Server 的依赖。
type Deps struct {
	Store      *store.Store
	Auth       *auth.Auth
	Now        func() time.Time // nil 用系统时钟
	SessionTTL time.Duration    // 会话绝对有效期；0 用 DefaultSessionTTL
	// SessionIdle 是会话空闲有效期，超过这么久没用就失效；0 用 DefaultSessionIdle。
	SessionIdle time.Duration
	// LoginDelay 是全部地址失败过多时每次登录尝试的延迟应答时长；0 用 DefaultLoginDelay。
	LoginDelay time.Duration
	Logger     *slog.Logger // nil 用 slog.Default()
	// Guard 非空时包装每个 Bearer 认证入口（唤醒租约，§13.6）：内核据账本把认证出的调用者降级，本层不判定。
	Guard func(command.Identity) command.Identity
	// Groups 是除 Register 登记外额外挂载的分组（测试与嵌入用）。
	Groups []RouteGroup
	// Web 非空时，未命中 API 路由的 GET/HEAD（/v1 之外）交给它——嵌入的 Web 中台。
	Web http.Handler
}

// Server 是 HTTP 服务。
type Server struct {
	d       Deps
	handler http.Handler
	lim     *limiter
}

// New 装配路由并校验；任何登记错误（重复、缺处理器、路径非法）都在启动时返回，不留到请求时。
func New(d Deps) (*Server, error) {
	if d.Store == nil || d.Auth == nil {
		return nil, errors.New("core: Store and Auth are required")
	}
	if d.Now == nil {
		d.Now = clock.Now
	}
	if d.SessionTTL <= 0 {
		d.SessionTTL = DefaultSessionTTL
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.SessionIdle <= 0 {
		d.SessionIdle = DefaultSessionIdle
	}
	if d.LoginDelay <= 0 {
		d.LoginDelay = DefaultLoginDelay
	}
	s := &Server{d: d, lim: newLimiter()}

	rt := &Router{}
	groups := []RouteGroup{s.authRoutes}
	regMu.Lock()
	for _, n := range registry {
		groups = append(groups, n.group)
	}
	regMu.Unlock()
	groups = append(groups, d.Groups...)
	for _, g := range groups {
		g(rt)
	}

	mux := http.NewServeMux()
	seen := map[string]bool{}
	errs := rt.errs
	for i, route := range rt.routes {
		path := route.Path
		if !rt.root[i] {
			path = APIPrefix + path
		}
		pattern := route.Method + " " + path
		if seen[pattern] {
			errs = append(errs, fmt.Errorf("core: duplicate route %s", pattern))
			continue
		}
		seen[pattern] = true
		h := s.wrap(route)
		if route.Page && route.Method == http.MethodGet && d.Web != nil {
			api := h
			h = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(r.Header.Get("Accept"), "text/html") {
					d.Web.ServeHTTP(w, r)
					return
				}
				api.ServeHTTP(w, r)
			})
		}
		mux.Handle(pattern, h)
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if d.Web != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) && !strings.HasPrefix(r.URL.Path, APIPrefix+"/") {
			d.Web.ServeHTTP(w, r)
			return
		}
		s.writeError(w, domain.NewError(domain.CodeNotFound, "no such route", "check the path and method against the API list"))
	})
	s.handler = s.base(mux)
	return s, nil
}

// Handler 返回完整的 http.Handler（含 panic 恢复与响应头）。
func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) base(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Cache-Control", "no-store")
		h.Set("X-Content-Type-Options", "nosniff")
		defer func() {
			if v := recover(); v != nil {
				s.d.Logger.Error("handler panic", "path", r.URL.Path, "panic", v)
				s.writeError(w, errors.New("panic"))
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) wrap(rt Route) http.Handler {
	limit := rt.MaxBody
	if limit <= 0 {
		limit = DefaultMaxBody
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		var p *Principal
		if rt.Auth == AuthRequired {
			var err error
			if p, err = s.authenticate(r); err != nil {
				s.writeError(w, err)
				return
			}
		}
		if p != nil {
			p.Evidence, p.Grant, p.WebURL = r.Header.Get(EvidenceHeader), r.Header.Get(GrantHeader), webLink(r)
		}
		if err := rt.Handler(w, r, p); err != nil {
			s.writeError(w, err)
		}
	})
}

// webLink 是请求对象在 Web 的页面：/{分组}/{id}，没有 id 的路由取分组页。
func webLink(r *http.Request) string {
	scheme := "http"
	if secureTransport(r) {
		scheme = "https"
	}
	link := scheme + "://" + r.Host
	if seg, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, APIPrefix), "/"), "/"); seg != "" {
		link += "/" + seg
		if id := r.PathValue("id"); id != "" {
			link += "/" + url.PathEscape(id)
		}
	}
	return link
}

// sessionKind 把头的值收成三个枚举之一：缺失为空（未上报），其余不认识的值按 unknown。
func sessionKind(v string) string {
	switch v = strings.ToLower(strings.TrimSpace(v)); v {
	case "":
		return ""
	case domain.SessionDirect, domain.SessionGroup:
		return v
	}
	return domain.SessionUnknown
}

// ---- 认证 ----

// authenticate 先看 Authorization：带了就只按 Bearer 判（无效不回退到 Cookie）；否则看会话 Cookie。
func (s *Server) authenticate(r *http.Request) (*Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		secret := auth.ParseBearer(h)
		origin := auth.Origin{Harness: r.Header.Get("X-APA-Harness"), Session: r.Header.Get("X-APA-Session"), SessionKind: sessionKind(r.Header.Get(SessionKindHeader))}
		var id command.Identity = command.Bearer{Auth: s.d.Auth, Secret: secret, Origin: origin, OnBehalfOf: ownerPrincipal}
		if runidentity.IsSecret(secret) { // 运行凭据：worker 级、绑定一次运行、没有代答能力
			id = runidentity.Identity{Secret: secret, Origin: origin}
		}
		if s.d.Guard != nil {
			id = s.d.Guard(id)
		}
		var caller command.Caller
		if err := s.d.Store.View(r.Context(), func(tx *store.Tx) (err error) { caller, err = id.Authenticate(tx); return }); err != nil {
			return nil, err
		}
		return &Principal{Identity: id, Caller: caller}, nil
	}
	c, err := r.Cookie(SessionCookie)
	if err != nil {
		return nil, errUnauthenticated()
	}
	p, err := s.resolveSession(r.Context(), c.Value)
	if err != nil {
		return nil, err
	}
	if !safeMethod(r.Method) {
		if err := checkOrigin(r); err != nil {
			return nil, err
		}
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(p.csrf)) != 1 {
			return nil, domain.NewError(domain.CodePermissionDenied, "CSRF token is missing or wrong", "send the csrf_token from login in the "+CSRFHeader+" header")
		}
	}
	return p, nil
}

func errUnauthenticated() error {
	return domain.NewError(domain.CodeUnauthenticated, "credential is missing, unknown, revoked or expired", "obtain a valid credential in Web")
}

func safeMethod(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}

// checkOrigin：带 Origin 的写请求必须与 Host 同源（"null" 与外站一律拒绝）。浏览器的跨站写必带 Origin。
func checkOrigin(r *http.Request) error {
	o := r.Header.Get("Origin")
	if o == "" {
		return nil
	}
	if u, err := url.Parse(o); err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
		return domain.NewError(domain.CodePermissionDenied, "cross-site request refused", "send the request from the Web UI's own origin")
	}
	return nil
}

// secureTransport：请求是否经 TLS 到达。X-Forwarded-Proto 只信本机反向代理（回环来源）。
func secureTransport(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return isLoopbackAddr(r.RemoteAddr) && strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func isLoopbackAddr(hostport string) bool {
	h, _, err := net.SplitHostPort(hostport)
	if err != nil {
		h = hostport
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// ---- JSON 与错误 ----

// WriteJSON 写 JSON 响应。
func WriteJSON(w http.ResponseWriter, status int, v any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	return json.NewEncoder(w).Encode(v)
}

// DecodeJSON 严格读取请求体：必须是恰好一个 JSON 值；过大、畸形、尾随内容都返回 invalid_request。
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	err := dec.Decode(v)
	if err == nil {
		if _, perr := dec.Token(); perr != io.EOF {
			err = errors.New("trailing content")
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		why := "request body is not valid JSON"
		if errors.As(err, &tooBig) {
			why = "request body is too large"
		}
		return domain.NewError(domain.CodeInvalidRequest, why, "send a single well-formed JSON object within the size limit")
	}
	return nil
}

// writeError 渲染统一错误体（附录 B.3）。领域错误原样输出；其余一律是 internal，细节只进日志。
func (s *Server) writeError(w http.ResponseWriter, err error) {
	var de *domain.Error
	if !errors.As(err, &de) {
		s.d.Logger.Error("internal error", "err", err)
		de = domain.NewError(domain.CodeInternal, "the server failed to complete the request", "retry; if it persists, check the server log")
	}
	_ = WriteJSON(w, de.HTTPStatus(), de)
}

// ---- 监听 ----

func listenAddr(addr string) string {
	if addr == "" {
		return config.Default().Listen
	}
	return addr
}

// Listen 监听 addr；空串用配置默认值（回环地址）。暴露到本机之外必须由配置显式写出非回环地址。
func Listen(addr string) (net.Listener, error) {
	return net.Listen("tcp", listenAddr(addr))
}

// shutdownGrace 是停止时留给在途请求自然结束的时间；之后取消请求上下文，让长轮询退出，不拖住关闭。
const shutdownGrace = 2 * time.Second

// Serve 在 l 上服务，ctx 取消后优雅关闭（最多等 10 秒）并返回 nil。
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	base, stop := context.WithCancel(context.Background())
	defer stop()
	srv := &http.Server{Handler: s.handler, ReadHeaderTimeout: 10 * time.Second,
		BaseContext: func(net.Listener) context.Context { return base }}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(l) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer time.AfterFunc(shutdownGrace, stop).Stop()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
