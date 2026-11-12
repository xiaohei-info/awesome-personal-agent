// Package events 是事件订阅的 HTTP 路由（附录 B.2、B.4，设计 §10.1）：
//
//	GET /events?cursor=&limit=&wait=
//
// 长轮询：返回序号大于 cursor 的已登记类型信封 {"events": [...], "next": N}。wait>0 时无新事件会挂起，
// 事件到达立即返回，超时返回空页。游标由订阅方自持：处理成功后才把 next 当作下一次的 cursor（至少一次）。
//
// 路由里没有业务规则：信封形状与"只开放登记类型"在 kernel/eventfeed。本层只决定哪把凭据能读。
package events

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventfeed"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	defaultLimit = 100
	maxLimit     = 500
	// MaxWait 是长轮询等待上限（秒）；更大的 wait 按它截断。
	MaxWait = 60
	// DefaultPoll 是长轮询内查看账本的间隔。
	DefaultPoll = 500 * time.Millisecond
)

// Routes 返回事件订阅路由分组，交给 core.Deps.Groups。poll 为 0 时取 DefaultPoll。
func Routes(st *store.Store, poll time.Duration) core.RouteGroup {
	if poll <= 0 {
		poll = DefaultPoll
	}
	h := &handlers{st: st, poll: poll}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/events", Handler: h.list})
	}
}

type handlers struct {
	st   *store.Store
	poll time.Duration
}

func invalid(why, next string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, next)
}

// authorize：observer 及以上可读；ext 只有导出与转发两种（scope 为 export、forward 或带 .<名> 后缀），
// 渠道 ext、webhook 另有用途；运行凭据限于一次运行，不订阅全账本。
func authorize(p *core.Principal) error {
	denied := func() error {
		return domain.NewError(domain.CodePermissionDenied, "credential cannot subscribe to events", "use an observer token or an export/forward ext key")
	}
	switch {
	case p.Caller.Level == authz.LevelRun, p.Caller.Kind == auth.Webhook:
		return denied()
	case p.Caller.Kind == auth.Ext:
		scope := strings.TrimPrefix(p.Caller.Actor.Principal, "ext:")
		for _, kind := range []string{"export", "forward"} {
			if scope == kind || strings.HasPrefix(scope, kind+".") {
				return nil
			}
		}
		return denied()
	}
	return nil
}

func intParam(q map[string][]string, name string, def, lo, hi int64) (int64, error) {
	v := ""
	if s := q[name]; len(s) > 0 {
		v = s[0]
	}
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < lo || n > hi {
		return 0, invalid(name+" is out of range", "use "+strconv.FormatInt(lo, 10)+" <= "+name+" <= "+strconv.FormatInt(hi, 10))
	}
	return n, nil
}

func (h *handlers) list(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	if err := authorize(p); err != nil {
		return err
	}
	q := r.URL.Query()
	cursor, err := intParam(q, "cursor", 0, 0, 1<<62)
	if err != nil {
		return err
	}
	limit, err := intParam(q, "limit", defaultLimit, 1, maxLimit)
	if err != nil {
		return err
	}
	wait, err := intParam(q, "wait", 0, 0, 1<<31)
	if err != nil {
		return err
	}
	deadline := clock.Now().Add(time.Duration(min(wait, MaxWait)) * time.Second)
	for {
		var page eventfeed.Page
		if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
			page, err = eventfeed.Read(tx, cursor, int(limit))
			return
		}); err != nil {
			return err
		}
		// 只有不可见事件时 Next 前进但没有信封：带着前进的游标继续等，不卡在原地。
		cursor = page.Next
		left := deadline.Sub(clock.Now())
		if len(page.Envelopes) > 0 || left <= 0 {
			if page.Envelopes == nil {
				page.Envelopes = []eventfeed.Envelope{}
			}
			return core.WriteJSON(w, http.StatusOK, page)
		}
		select {
		case <-r.Context().Done():
			return r.Context().Err()
		case <-time.After(min(h.poll, left)):
		}
	}
}
