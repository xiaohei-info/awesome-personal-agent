// Package context 是上下文与人格的 HTTP 路由（附录 B.2，设计 §7.6、§11.2）：
//
//	GET /context[?session&project&path&host&scope&item&instruction&format=inject]
//	GET /context/persona[?lang=zh|en&locale=<区域设置>]
//
// 路由里没有业务规则：增量投影、游标、注入渲染在 kernel/context，人格与它的固定声明在 kernel/policy；
// 本层只做认证后的参数搬运。会话取自 X-APA-Session 头（与审计同源），不带会话时不读不写游标。
// 捕获（/capture）属于 kernel-capture，不在这里。
package context

import (
	"net/http"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/readscope"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	kcontext "github.com/xiaohei-info/awesome-personal-agent/kernel/context"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
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
		Reader: kcontext.Reader{Level: p.Caller.Level, Principal: p.Caller.Actor.Principal, Run: p.Caller.Actor.Run, Harness: p.Caller.Actor.Harness}}
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
	run := func(tx *store.Tx) (err error) {
		if res, err = kcontext.Build(tx, req); err != nil {
			return err
		}
		return scoped(tx, p, &res)
	}
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

// scoped 按外部钥匙的来源档位裁上下文（§14.14）：够不到的事项连同它的投递、挂靠消息、委派记录一起去掉；
// 只有元信息的事项，投递不带摘要与选项；挂靠消息各按自己来源的档位裁。内部钥匙与运行凭据不变。
func scoped(tx *store.Tx, p *core.Principal, res *kcontext.Result) error {
	rs := readscope.New(tx, p.Caller)
	if rs.All() {
		return nil
	}
	tiers := map[string]string{}
	tier := func(id string) (string, error) {
		if t, ok := tiers[id]; ok {
			return t, nil
		}
		it, err := kitems.Get(tx, id)
		if err != nil {
			return readscope.None, err
		}
		t, err := rs.ItemTier(it)
		tiers[id] = t
		return t, err
	}
	items := res.Items[:0]
	for _, e := range res.Items {
		t, err := tier(e.ID)
		if err != nil {
			return err
		}
		if t != readscope.None {
			items = append(items, e)
		}
	}
	res.Items = items
	var ds []kcontext.DeliveryEntry
	for _, d := range res.Deliveries {
		t, err := tier(d.Item)
		if err != nil {
			return err
		}
		switch t {
		case readscope.None:
			continue
		case readscope.Meta:
			d.Summary, d.Options = "", nil
		}
		ds = append(ds, d)
	}
	res.Deliveries = ds
	var ms []kcontext.ItemMessages
	for _, m := range res.Messages {
		t, err := tier(m.Item)
		if err != nil {
			return err
		}
		if t == readscope.None {
			continue
		}
		m.Forwarded.Messages = rs.Messages(m.Forwarded.Messages)
		ms = append(ms, m)
	}
	res.Messages = ms
	var hs []kcontext.ItemHistory
	for _, h := range res.History {
		t, err := tier(h.Item)
		if err != nil {
			return err
		}
		if t != readscope.None {
			hs = append(hs, h)
		}
	}
	res.History = hs
	return nil
}

// persona 返回注入用的人格全文，以及同一语言的内置默认原文（Web 在没自定义时显示它）。
// 语言：lang > 策略里的 persona_lang > locale（调用方的进程区域设置）> 英文（policy.PickLang）。
func (h *handlers) persona(w http.ResponseWriter, r *http.Request, _ *core.Principal) error {
	q := r.URL.Query()
	var text, def string
	if err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		lang := policy.PickLang(pol, q.Get("lang"), q.Get("locale"))
		text, def = policy.PersonaText(pol, lang), policy.DefaultPersona(lang)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]string{"persona": text, "default": def})
}
