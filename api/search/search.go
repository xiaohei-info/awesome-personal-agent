// Package search 是统一检索的 HTTP 路由 GET /search（附录 B.2，设计 §7.1、§14.14）。
// 排名、上限、分页与零命中计数都在 kernel/search；本层只搬参数，并把调用者的读取闸（api/readscope）交给内核复核命中。
// 属"查看"组：所有读路由的组判定在 api/core。
package search

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/api/readscope"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	ks "github.com/xiaohei-info/awesome-personal-agent/kernel/search"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Routes 返回检索路由分组，交给 core.Deps.Groups。
func Routes(st *store.Store) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/search", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			return get(w, r, p, st)
		}})
	}
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the parameter and retry")
}

func get(w http.ResponseWriter, r *http.Request, p *core.Principal, st *store.Store) error {
	// ext 与 webhook 凭据另有用途，不能读账本，只有导出 ext 例外（与 GET /items 一致，§10.3）。
	if (p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook) && !auth.IsExportExt(p.Caller.Kind, p.Caller.Actor.Principal) {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
	}
	v := r.URL.Query()
	q := ks.Query{Text: v.Get("q"), Project: v.Get("project"), Here: v.Get("here"), Phase: v.Get("phase"), Kind: v.Get("kind"),
		Tags: v["tag"], Item: v.Get("item"), Cursor: v.Get("cursor"), Record: true}
	for _, t := range v["type"] {
		q.Types = append(q.Types, strings.Split(t, ",")...)
	}
	switch v.Get("include_dropped") {
	case "", "false":
	case "true":
		q.IncludeDropped = true
	default:
		return invalid("include_dropped must be true or false")
	}
	for name, dst := range map[string]*time.Time{"after": &q.After, "before": &q.Before} {
		if s := v.Get(name); s != "" {
			t, err := time.Parse(time.RFC3339, s)
			if err != nil {
				if t, err = time.ParseInLocation("2006-01-02", s, time.Local); err != nil {
					return invalid(name + " must be an RFC 3339 time or YYYY-MM-DD")
				}
			}
			*dst = t
		}
	}
	if s := v.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > ks.MaxLimit {
			return invalid("limit must be 1 to 50")
		}
		q.Limit = n
	}
	res, err := ks.Search(r.Context(), st, q, func(tx *store.Tx) ks.Gate { return readscope.New(tx, p.Caller).Gate() })
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, res)
}
