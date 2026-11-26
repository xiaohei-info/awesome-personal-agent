// Package judgments 是判断记录读取的 HTTP 路由（附录 B.2，设计 §6.16）：
//
//	GET /judgments?subject&use&template&source&after&limit
//	GET /judgments/{id}
//	GET /judgments/spot-check, POST /judgments/{id}/review   （抽检，设计 §8.10；见 spotcheck.go）
//
// 路由里没有业务规则：记录的读取与分页在 kernel/judgment/record。本层只决定哪把凭据能读：
// observer 及以上读全部（记录不含资料正文）；运行凭据只读目标事项范围内的记录，
// 范围外按不存在处理（404，退出码 5），不泄露记录是否存在；ext、webhook 凭据不能读。
// GET /judgments/stats?use&template&source&since 是计数与四项指标（设计 §8.10，见 stats.go）。
// 没有传入抽检服务时的 spot-check 不能被 {id} 通配吃掉，否则对照测试会把它当成已挂载。
package judgments

import (
	"net/http"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/spotcheck"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Routes 返回判断记录路由分组，交给 core.Deps.Groups。sc 非空时同时挂抽检路由。
func Routes(st *store.Store, sc *spotcheck.Service) core.RouteGroup {
	h := &handlers{st: st, sc: sc}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/judgments", Handler: h.list})
		r.Handle(core.Route{Method: "GET", Path: "/judgments/{id}", Handler: h.get})
		r.Handle(core.Route{Method: "GET", Path: "/judgments/stats", Handler: h.stats})
		var reserved []string
		if sc != nil {
			r.Handle(core.Route{Method: "GET", Path: "/judgments/spot-check", Page: true, Handler: h.spotCheck})
			r.Handle(core.Route{Method: "POST", Path: "/judgments/{id}/review", MaxBody: 4 << 10, Handler: h.review})
		} else {
			reserved = append(reserved, "/judgments/spot-check")
		}
		for _, reserved := range reserved {
			r.Handle(core.Route{Method: "GET", Path: reserved, Auth: core.AuthNone, Handler: noRoute})
		}
	}
}

func noRoute(http.ResponseWriter, *http.Request, *core.Principal) error {
	return domain.NewError(domain.CodeNotFound, "no such route", "check the path and method against the API list")
}

type handlers struct {
	st *store.Store
	sc *spotcheck.Service
}

func readable(p *core.Principal) error {
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read judgment records", "use an owner, observer or run credential")
	}
	return nil
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	id := r.PathValue("id")
	var rec record.Record
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		var err error
		rec, err = record.Get(tx, id)
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, rec)
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	q := r.URL.Query()
	f := record.Filter{Subject: q.Get("subject"), Use: q.Get("use"), Template: q.Get("template"), Source: q.Get("source"), After: q.Get("after")}
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return domain.NewError(domain.CodeInvalidRequest, "limit must be a positive integer", "send a number between 1 and 500")
		}
		f.Limit = n
	}
	var (
		recs []record.Record
		more bool
	)
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		var err error
		recs, more, err = record.List(tx, f)
		return err
	})
	if err != nil {
		return err
	}
	if recs == nil {
		recs = []record.Record{}
	}
	out := map[string]any{"judgments": recs}
	if more {
		out["next_after"] = recs[len(recs)-1].ID
	}
	return core.WriteJSON(w, http.StatusOK, out)
}
