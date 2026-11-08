// Package why 是回放与审计分组的 HTTP 路由（附录 B.2，设计 §15.1、§15.3）：
// GET /why/{id}（事项或运行回放）与 GET /audit/events（按操作者、对象、类型筛选的 Event 查询）。
//
// 路由里没有业务规则：读取、授权与按凭据级别的内容裁剪都在 kernel/why；
// 本层只做认证后的参数搬运，以及把"哪些事项有未返回的 web_only 初筛"从账本取给内核。
// /events（SSE 订阅）属于 api-events，不在这里。
package why

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	kwhy "github.com/xiaohei-info/awesome-personal-agent/kernel/why"
)

// Routes 返回回放与审计路由分组，交给 core.Deps.Groups。
func Routes(st *store.Store) core.RouteGroup {
	h := &handlers{st: st}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/why/{id}", Handler: h.why})
		r.Handle(core.Route{Method: "GET", Path: "/audit/events", Handler: h.audit})
	}
}

type handlers struct{ st *store.Store }

// readable：ext 与 webhook 凭据另有用途，不能读账本。
func readable(p *core.Principal) error {
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
	}
	return nil
}

func (h *handlers) why(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	id := r.PathValue("id")
	var tl kwhy.Timeline
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		req := kwhy.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, ID: id}
		// id 可能是运行 id：不是事项就没有初筛状态。
		switch it, err := kitems.Get(tx, id); {
		case err == nil:
			req.ScreeningPending = it.Ask != nil && it.Ask.ScreeningPending
		case !isNotFound(err):
			return err
		}
		tl, err = kwhy.Replay(tx, pol, req)
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, tl)
}

func (h *handlers) audit(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	q := r.URL.Query()
	num := func(key string) (int64, error) {
		if q.Get(key) == "" {
			return 0, nil
		}
		n, err := strconv.ParseInt(q.Get(key), 10, 64)
		if err != nil {
			return 0, domain.NewError(domain.CodeInvalidRequest, key+" must be an integer", "send a number")
		}
		return n, nil
	}
	after, err := num("after")
	if err != nil {
		return err
	}
	limit, err := num("limit")
	if err != nil {
		return err
	}
	req := kwhy.AuditRequest{Actor: p.Caller.Actor, Level: p.Caller.Level, After: after, Limit: int(limit),
		Filter: eventlog.Filter{Actor: q.Get("actor"), Object: q.Get("object"), Type: q.Get("type"), Grant: q.Get("grant")}}
	var steps []kwhy.Step
	if err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		if req.Level != authz.LevelWeb { // 非 Web 凭据看不到初筛未返回事项的事件，与 /why 一致
			its, err := kitems.List(tx, kitems.Filter{})
			if err != nil {
				return err
			}
			req.Pending = map[string]bool{}
			for _, it := range its {
				if it.Ask != nil && it.Ask.ScreeningPending {
					req.Pending[it.ID] = true
				}
			}
		}
		steps, err = kwhy.Audit(tx, pol, req)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"events": steps})
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}
