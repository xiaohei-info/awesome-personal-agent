// Package deliveries 是投递领取协议的 HTTP 路由（附录 B.2，设计 §10.3、§14.2）：
//
//	GET  /deliveries/next?wait=      长轮询领取（带租约）
//	POST /deliveries/{id}/ack        送达回报（带版本号与外部消息 ref）
//	POST /deliveries/{id}/fail       失败回报（内核退避重试，超限退回 Web 未读）
//	GET  /deliveries/unread          Web 未读
//	GET  /deliveries/today           今天的推送计数（与日上限同口径）
//	POST /deliveries/{id}/read       本人在 Web 点击：确认（acknowledged）
//
// 路由里没有业务规则：租约、幂等、版本冲突、退避都在 kernel/delivery。本层只决定"这把凭据能碰哪条路由"：
//   - 渠道 ext 密钥（scope 为 channel.<名>）只碰路由 channel:<名>，渠道由密钥确定，请求里没有渠道参数；
//   - 网关插件 token（worker、delegate）只碰唤醒类路由；
//   - 其他凭据（observer、导出与转发 ext、webhook、运行凭据）一律拒绝；
//   - 未读、已读与今天的计数只有 Web 登录。
//
// 凭据在每个请求里重新校验，吊销下一次请求即生效。回答回流（/answer）不在这里。
package deliveries

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// MaxWait 是长轮询等待上限（秒）；更大的 wait 按它截断。
	MaxWait = 60
	// DefaultPoll 是长轮询内查看队列的间隔。
	DefaultPoll = 500 * time.Millisecond
	// ChannelScopePrefix 标明渠道 ext 密钥：scope 为 channel.<渠道名>。
	ChannelScopePrefix = "channel."
)

// Routes 返回投递路由分组，交给 core.Deps.Groups。poll 为 0 时取 DefaultPoll。
func Routes(st *store.Store, q *delivery.Queue, poll time.Duration) core.RouteGroup {
	if poll <= 0 {
		poll = DefaultPoll
	}
	h := &handlers{st: st, q: q, poll: poll}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/deliveries/next", Handler: h.next})
		r.Handle(core.Route{Method: "POST", Path: "/deliveries/{id}/ack", Handler: h.ack})
		r.Handle(core.Route{Method: "POST", Path: "/deliveries/{id}/fail", Handler: h.fail})
		r.Handle(core.Route{Method: "GET", Path: "/deliveries/unread", Handler: h.unread})
		r.Handle(core.Route{Method: "GET", Path: "/deliveries/today", Handler: h.today})
		r.Handle(core.Route{Method: "POST", Path: "/deliveries/{id}/read", Handler: h.read})
	}
}

type handlers struct {
	st   *store.Store
	q    *delivery.Queue
	poll time.Duration
}

func denied(why, next string) error {
	return domain.NewError(domain.CodePermissionDenied, why, next)
}

// route 由凭据确定调用方能碰的投递路由。
func route(p *core.Principal) (string, error) {
	if p.Web {
		return "", denied("Web login cannot claim or report deliveries", "use a channel ext key or a gateway plugin token")
	}
	switch p.Caller.Kind {
	case auth.Ext:
		// Bearer 把 ext 密钥解析成 Actor{Principal: "ext:<scope>"}。
		name, ok := strings.CutPrefix(p.Caller.Actor.Principal, "ext:"+ChannelScopePrefix)
		if !ok || name == "" {
			return "", denied("this ext key is not a channel key", "issue a channel ext key in Web settings")
		}
		return delivery.ChannelRoute(name), nil
	case auth.Worker, auth.Delegate:
		return delivery.RouteWake, nil
	}
	return "", denied("credential kind cannot use the delivery protocol", "use a channel ext key or a gateway plugin token")
}

func webOnly(p *core.Principal) error {
	if !p.Web {
		return denied("only the Web login can read unread deliveries", "open the Web UI")
	}
	return nil
}

func (h *handlers) next(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	rt, err := route(p)
	if err != nil {
		return err
	}
	wait := 0
	if v := r.URL.Query().Get("wait"); v != "" {
		if wait, err = strconv.Atoi(v); err != nil || wait < 0 {
			return domain.NewError(domain.CodeInvalidRequest, "wait must be a non-negative number of seconds", "send wait=50 or omit it")
		}
	}
	deadline := clock.Now().Add(time.Duration(min(wait, MaxWait)) * time.Second)
	for {
		var d *domain.Delivery
		if err := h.st.Update(r.Context(), func(tx *store.Tx) (err error) { d, err = h.q.Claim(tx, rt); return }); err != nil {
			return err
		}
		if d != nil {
			return core.WriteJSON(w, http.StatusOK, d)
		}
		left := deadline.Sub(clock.Now())
		if left <= 0 {
			w.WriteHeader(http.StatusNoContent)
			return nil
		}
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case <-time.After(min(h.poll, left)):
		}
	}
}

type report struct {
	Version     int64  `json:"version"`
	ExternalRef string `json:"external_ref"`
	Reason      string `json:"reason"`
}

// ack 与 fail 同形：校验调用方对这条投递有权（路由一致），再交给队列。
// 版本已作废时队列回 Stale，这里转成 409，ext 不应再发。
func (h *handlers) report(apply func(tx *store.Tx, id string, in report) (domain.Delivery, delivery.Result, error)) core.Handler {
	return func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
		rt, err := route(p)
		if err != nil {
			return err
		}
		var in report
		if err := core.DecodeJSON(r, &in); err != nil {
			return err
		}
		if in.Version < 1 {
			return domain.NewError(domain.CodeInvalidRequest, "version is required", "send the version from the claimed delivery")
		}
		id := r.PathValue("id")
		var d domain.Delivery
		var res delivery.Result
		err = h.st.Update(r.Context(), func(tx *store.Tx) error {
			cur, err := h.q.Get(tx, id)
			if err != nil {
				return err
			}
			if cur.Route != rt { // 别的渠道的投递对这把密钥等同不存在
				return domain.NewError(domain.CodeNotFound, "delivery "+id+" not found", "check the delivery id")
			}
			d, res, err = apply(tx, id, in)
			return err
		})
		if err != nil {
			return err
		}
		if res == delivery.Stale {
			return domain.NewError(domain.CodeConflict, "delivery version is no longer current", "do not send it; claim the next delivery")
		}
		return core.WriteJSON(w, http.StatusOK, d)
	}
}

func (h *handlers) ack(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	return h.report(func(tx *store.Tx, id string, in report) (domain.Delivery, delivery.Result, error) {
		return h.q.Ack(tx, id, in.Version, in.ExternalRef)
	})(w, r, p)
}

func (h *handlers) fail(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	return h.report(func(tx *store.Tx, id string, in report) (domain.Delivery, delivery.Result, error) {
		return h.q.Fail(tx, id, in.Version, in.Reason)
	})(w, r, p)
}

func (h *handlers) unread(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := webOnly(p); err != nil {
		return err
	}
	var ds []domain.Delivery
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) { ds, err = h.q.Unread(tx); return }); err != nil {
		return err
	}
	if ds == nil {
		ds = []domain.Delivery{}
	}
	return core.WriteJSON(w, http.StatusOK, map[string]any{"deliveries": ds})
}

// today 返回当前自然日的推送计数（delivery.TodayCounts）：只读，不写 Event。
func (h *handlers) today(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := webOnly(p); err != nil {
		return err
	}
	var c delivery.TodayCounts
	if err := h.st.View(r.Context(), func(tx *store.Tx) error {
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		c, err = h.q.Today(tx, pol)
		return err
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, c)
}

func (h *handlers) read(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := webOnly(p); err != nil {
		return err
	}
	id := r.PathValue("id")
	err := h.st.Update(r.Context(), func(tx *store.Tx) error {
		d, err := h.q.Get(tx, id)
		if err != nil {
			return err
		}
		_, res, err := h.q.Confirm(tx, id, d.Version)
		if err == nil && res == delivery.Stale {
			err = domain.NewError(domain.CodeConflict, "delivery "+id+" is superseded", "re-read the unread list")
		}
		return err
	})
	if err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, map[string]string{"id": id})
}
