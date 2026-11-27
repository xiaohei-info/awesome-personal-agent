// Package usage 是用量路由（附录 B.2）：GET /usage?days=1|7|30，只读汇总在 kernel/usage。
package usage

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	kusage "github.com/xiaohei-info/awesome-personal-agent/kernel/usage"
)

// Routes 返回用量路由分组。读取口径同运行记录：observer 及以上，权限卡"查看"组由入口统一判定。
func Routes(st *store.Store) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/usage", Handler: func(w http.ResponseWriter, req *http.Request, p *core.Principal) error {
			if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
				return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
			}
			if d := authz.CanRead(authz.Request{Actor: p.Caller.Actor, Level: p.Caller.Level}); d.Err != nil {
				return d.Err
			}
			days := 1
			if s := req.URL.Query().Get("days"); s != "" {
				n, err := strconv.Atoi(s)
				if err != nil || (n != 1 && n != 7 && n != 30) {
					return domain.NewError(domain.CodeInvalidRequest, "days is not a supported range", "use days=1, 7 or 30")
				}
				days = n
			}
			// "今天"按主机日界，与今天视图一致。
			now := clock.Now().In(time.Local)
			from := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.Local).AddDate(0, 0, 1-days)
			var out kusage.Summary
			if err := st.View(req.Context(), func(tx *store.Tx) (err error) { out, err = kusage.Summarize(tx, from); return }); err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, out)
		}})
	}
}
