// Package runs 是运行记录分组的 HTTP 路由（附录 B.2，设计 §6.11）：
// GET /runs（可按 state、category、target 筛选）、GET /runs/{id}，以及网关插件换取唤醒运行凭据的 POST /runs/{id}/credential（§13.6）。
//
// 路由里没有业务规则：Run 的读取在 kernel/outcall/proc，授权判定在 kernel/authz；
// 本层只做认证后的参数校验与搬运。响应只含 §6.11 的字段（状态、执行统计、授权依据），
// 不返回命令规格、stdin 与失败附注——它们可能带账本内容或运行环境。
// 健康汇总（队列统计）属于 kernel-health，不在这里。
package runs

import (
	"net/http"
	"strconv"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	defaultLimit = 100
	maxLimit     = 500
)

// Routes 返回运行记录路由分组，交给 core.Deps.Groups。
func Routes(st *store.Store) core.RouteGroup {
	h := &handlers{st: st}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/runs", Handler: h.list})
		r.Handle(core.Route{Method: "GET", Path: "/runs/{id}", Handler: h.get})
		r.Handle(core.Route{Method: "POST", Path: "/runs/{id}/credential", MaxBody: 1 << 10, Handler: h.credential})
	}
}

type handlers struct{ st *store.Store }

// authorize：observer 及以上可读；ext 与 webhook 凭据另有用途，不能读账本。判定交给 authz。
func authorize(tx *store.Tx, p *core.Principal) error {
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	// Decision.Err 是 *domain.Error：直接返回会把 nil 指针包成非空 error。
	if d := authz.Decide(pol, authz.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, Action: authz.ActionRead}); d.Err != nil {
		return d.Err
	}
	return nil
}

// credential 只搬运：谁能换、换什么都由 runidentity.Exchange 判定。明文只在这个响应里出现一次。
func (h *handlers) credential(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	id := r.PathValue("id")
	var (
		token   string
		expires time.Time
	)
	if err := h.st.Update(r.Context(), func(tx *store.Tx) (err error) {
		token, expires, err = runidentity.Exchange(tx, p.Caller, id, time.Now())
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"run": id, "token": token, "expires_at": expires.UTC()})
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var run domain.Run
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		if err := authorize(tx, p); err != nil {
			return err
		}
		rec, err := proc.Get(tx, r.PathValue("id"))
		run = rec.Run
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, run)
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	q := r.URL.Query()
	state, category, target := domain.RunState(q.Get("state")), domain.RunCategory(q.Get("category")), q.Get("target")
	if state != "" && !validState(state) {
		return invalid("state is not a run state", "use queued, running, succeeded, failed or timeout")
	}
	if category != "" && !validCategory(category) {
		return invalid("category is not a run category", "use judge, generate, task, background, source, transform or reader")
	}
	limit := defaultLimit
	if s := q.Get("limit"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 1 || n > maxLimit {
			return invalid("limit is out of range", "use 1 <= limit <= "+strconv.Itoa(maxLimit))
		}
		limit = n
	}
	out := []domain.Run{}
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		if err := authorize(tx, p); err != nil {
			return err
		}
		recs, err := proc.List(tx)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			run := rec.Run
			if (state == "" || run.State == state) && (category == "" || run.Category == category) && (target == "" || run.Target == target) {
				out = append(out, run)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(out) > limit { // 保留最新的 limit 条，仍按入队顺序
		out = out[len(out)-limit:]
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"runs": out})
}

func invalid(why, next string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, next)
}

func validState(s domain.RunState) bool {
	switch s {
	case domain.RunQueued, domain.RunRunning, domain.RunSucceeded, domain.RunFailed, domain.RunTimeout:
		return true
	}
	return false
}

func validCategory(c domain.RunCategory) bool {
	switch c {
	case domain.RunJudge, domain.RunGenerate, domain.RunTask, domain.RunBackground, domain.RunSource, domain.RunTransform, domain.RunReader:
		return true
	}
	return false
}
