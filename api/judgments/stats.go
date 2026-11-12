package judgments

import (
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/stats"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// stats 是 GET /judgments/stats：各级调用次数与四项指标。统计横跨全部来源，运行凭据只读目标事项范围，所以不开放给它。
func (h *handlers) stats(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	if p.Caller.Level == authz.LevelRun {
		return domain.NewError(domain.CodePermissionDenied, "run credentials cannot read judgment stats", "use an owner or observer credential")
	}
	now := clock.Now()
	if h.sc != nil {
		now = h.sc.Now()
	}
	q := r.URL.Query()
	since, err := stats.ParseSince(q.Get("since"), now)
	if err != nil {
		return err
	}
	var out stats.Result
	err = h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		out, err = stats.Compute(tx, stats.Filter{Use: q.Get("use"), Template: q.Get("template"), Source: q.Get("source"), Since: since})
		return
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, out)
}
