package policy

import (
	"errors"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// proactivePreview 是 POST /settings/proactive/preview（§7.4 主动提醒）：按请求体里还没保存的设置与当前数据，
// 算"现在开启的话接下来 24 小时会提醒多少条、合成几批、最近一批在什么时候、送到哪"。只读，不写任何东西，只给 Web 本人。
// 请求体是 disturb.proactive 的形状；校验与保存时同一套（不合法为 400）。
func proactivePreview(st *store.Store) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		if err := rulesOnly(st, r, p); err != nil {
			return err
		}
		raw, err := readBody(r)
		if err != nil {
			return err
		}
		s := kpolicy.DefaultProactive()
		if err := strictDecode(raw, &s); err != nil {
			return err
		}
		var pv attention.Preview
		err = st.View(r.Context(), func(tx *store.Tx) error {
			_, pol, err := kpolicy.Current(tx)
			if err != nil {
				return err
			}
			if s, err = kpolicy.CheckProactive(s); err != nil {
				return err
			}
			s.On = true // 预览的就是"开启之后"
			pv, err = attention.SchedulePreview(tx, pol, s, clock.Now())
			return err
		})
		if errors.Is(err, kpolicy.ErrInvalid) {
			return domain.NewError(domain.CodeInvalidRequest, err.Error(), "fix the setting and preview again")
		}
		if err != nil {
			return err
		}
		return core.WriteJSON(w, http.StatusOK, pv)
	}
}
