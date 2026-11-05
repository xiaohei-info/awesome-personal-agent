// Package context 是上下文与人格的 HTTP 路由（附录 B.2，设计 §7.6、§11.2）：
//
//	GET /context[?session&project&path&host&scope&item&instruction&format=inject]
//	GET /context/persona
//
// 路由里没有业务规则：增量投影、游标、注入渲染在 kernel/context，人格与它的固定声明在 kernel/policy；
// 本层只做认证后的参数搬运。会话取自 X-APA-Session 头（与审计同源），不带会话时不读不写游标。
// 捕获（/capture）属于 kernel-capture，不在这里。
package context

import (
	"net/http"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Routes 返回上下文路由分组，交给 core.Deps.Groups。
func Routes(st *store.Store) core.RouteGroup {
	h := &handlers{st: st}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/context", Handler: readable(h.context)})
		r.Handle(core.Route{Method: "GET", Path: "/context/persona", Handler: readable(h.persona)})
	}
}

type handlers struct{ st *store.Store }

// readable：ext 与 webhook 凭据另有用途，不能读账本（与其他读路由一致）。
func readable(h core.Handler) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
			return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
		}
		return h(w, r, p)
	}
}

func invalid(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

func (h *handlers) context(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	q := r.URL.Query()
	req := kcontext.Request{Session: p.Caller.Actor.Session,
		Scope:  projects.Scope{Flag: q.Get("project"), Cwd: q.Get("path"), Host: q.Get("host")},
		Item:   q.Get("item"),
		Reader: kcontext.Reader{Level: p.Caller.Level, Principal: p.Caller.Actor.Principal, Run: p.Caller.Actor.Run}}
	if req.Scope.Cwd != "" && req.Scope.Host == "" {
		return invalid("host is required with path", "send the machine's host name")
	}
	switch q.Get("scope") {
	case "", "project", "cwd":
	case "all":
		req.All = true
	case "item":
		req.ItemScope = true
	default:
		return invalid("scope must be project, cwd, item or all", "use --scope project, cwd, item or all")
	}
	if req.Item != "" && !req.ItemScope {
		return invalid("item is only valid with scope=item", "add --scope item or remove --item")
	}
	if v := q.Get("instruction"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return invalid("instruction must be a non-negative integer", "send the instruction version the plugin holds")
		}
		req.Instruction = n
	}
	if f := q.Get("format"); f != "" && f != "inject" && f != "json" {
		return invalid("format must be json or inject", "use --format inject or omit it")
	}

	var res kcontext.Result
	run := func(tx *store.Tx) (err error) { res, err = kcontext.Build(tx, req); return }
	var err error
	if req.Session != "" || req.ItemScope { // 要推进游标或挂靠消息，需要写事务
		err = h.st.Update(r.Context(), run)
	} else {
		err = h.st.View(r.Context(), run)
	}
	if err != nil {
		return err
	}
	if q.Get("format") == "inject" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, err := w.Write([]byte(res.Inject()))
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res)
}

func (h *handlers) persona(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	var text string
	if err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		text = policy.PersonaText(pol)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]string{"persona": text})
}
