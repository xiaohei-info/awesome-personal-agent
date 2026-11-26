package core

import (
	"context"
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Authorize 判一次不走命令管线的读写（设置路由、接口层的检查，§14.3）：与命令同一个 authz.Decide——只有本人的段对 Agent 是 20，
// 其余按调用者所属 Agent 的权限卡判这一组；Web 本人做后果较大的组须刚输过口令。被拒的按同一条留痕路径记 command.rejected（§6.7）；
// 记账失败不改变拒绝结果。object 是留痕用的对象名（如 settings:/settings/policy）。
func Authorize(st *store.Store, r *http.Request, p *Principal, group authz.Group, page, object string) (*domain.Basis, error) {
	basis, rej, err := Decide(st, r, p, group, page, object)
	if rej != nil {
		Record(st, r, rej)
	}
	return basis, err
}

// Rejection 是一次待留痕的拒绝。
type Rejection struct {
	req authz.Request
	d   authz.Decision
}

// Decide 判定但不留痕：被拒时返回 Rejection，由调用方决定记不记（读设置时逐段试探，只记最后的结论）。
func Decide(st *store.Store, r *http.Request, p *Principal, group authz.Group, page, object string) (*domain.Basis, *Rejection, error) {
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return nil, nil, domain.NewError(domain.CodePermissionDenied, "credential kind cannot change settings", "use an owner or agent credential")
	}
	var (
		pv  int
		pol = policy.Default()
	)
	if !p.Web { // Agent 的权限卡在策略里；Web 本人不看权限卡，不必先读（读不出的策略交给写入报 400）
		if err := st.View(r.Context(), func(tx *store.Tx) (err error) { pv, pol, err = policy.Current(tx); return }); err != nil {
			return nil, nil, err
		}
	}
	agent := p.Caller.Agent()
	ar := authz.Request{Actor: p.Caller.Actor, Level: p.Caller.Level, Action: authz.ActionChangeSettings, Group: group, Fresh: p.Caller.Fresh,
		Agent: agent, Card: pol.Card(agent), Now: clock.Now(), Page: page, Object: authz.Object{ID: object}}
	d := authz.Decide(pol, ar)
	if d.Err == nil {
		via := domain.BasisAllowance
		if p.Web {
			via = domain.BasisOwnerWeb
		}
		return &domain.Basis{PolicyVersion: pv, Via: via, Tier: string(group), Agent: agent}, nil, nil
	}
	return nil, &Rejection{ar, d}, d.Err
}

// Record 按同一条留痕路径记 command.rejected；记账失败不改变拒绝结果。
func Record(st *store.Store, r *http.Request, rej *Rejection) {
	if ev, ok := authz.RejectionEvent(rej.req, rej.d); ok {
		ev.At = clock.Now()
		_ = st.Update(context.WithoutCancel(r.Context()), func(tx *store.Tx) error { _, err := eventlog.Append(tx, ev); return err })
	}
}

// Gate 把接口层的检查并入统一判定：Web 本人直接放行（写入仍经命令管线判口令），其余调用者按权限卡判 group，
// 没开为 23、只有本人的为 20，与命令同一条留痕路径。page 是本人可以直接做这件事的页面。
func Gate(st *store.Store, group authz.Group, page string, h Handler) Handler {
	return func(w http.ResponseWriter, r *http.Request, p *Principal) error {
		if !p.Web {
			if _, err := Authorize(st, r, p, group, page, "api:"+r.URL.Path); err != nil {
				return err
			}
		}
		return h(w, r, p)
	}
}
