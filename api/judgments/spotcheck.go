package judgments

import (
	"net/http"
	"strconv"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/spotcheck"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// spotCheck 是 GET /judgments/spot-check：抽检池（只读，observer 及以上）。条数默认取策略的 spot_check.per_brief，
// ?limit= 可改小或放大。池横跨全部来源，运行凭据只读目标事项范围，所以不开放给它。
func (h *handlers) spotCheck(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := readable(p); err != nil {
		return err
	}
	if p.Caller.Level == authz.LevelRun {
		return domain.NewError(domain.CodePermissionDenied, "run credentials cannot read the spot-check pool", "use an owner or observer credential")
	}
	limit := -1
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 || n > 500 {
			return domain.NewError(domain.CodeInvalidRequest, "limit must be an integer within 1..500", "send a number between 1 and 500")
		}
		limit = n
	}
	var out []spotcheck.Entry
	err := h.st.View(r.Context(), func(tx *store.Tx) error {
		if limit < 0 {
			_, pol, err := policy.Current(tx)
			if err != nil {
				return err
			}
			limit = pol.Judgment.SpotCheck.PerBrief
		}
		var err error
		out, err = spotcheck.Pool(tx, h.sc.Now(), limit)
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"spot_check": out})
}

// review 是 POST /judgments/{id}/review：填抽检结论 {"result": "should_see" | "correct"}。
// 谁能填由命令授权（change_settings，只有 Web 会话通过），这里不再判断。
func (h *handlers) review(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	var in struct {
		Result string `json:"result"`
	}
	if err := core.DecodeJSON(r, &in); err != nil {
		return err
	}
	rec, err := h.sc.Review(r.Context(), p.Identity, r.PathValue("id"), in.Result, p.WebURL)
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, rec)
}
