// Package health 是 GET /health 路由（附录 B.2，设计 §15.2）：把 kernel/health 汇总出的十类信号
// 连同修复入口原样给出，`apa health` 与 Web 读同一份。
//
// 路由里没有业务规则：是否异常由 kernel/health 判定，这里只做格式搬运。
// why、next 只写机制（哪类记录、去哪里修），不带来源报错、事项标题等账本里的用户内容；
// detail 只给 id 与计数，需要内容的人经修复入口按自己的权限去看。
package health

import (
	"net/http"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	khealth "github.com/xiaohei-info/awesome-personal-agent/kernel/health"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Signal 是十类信号之一。OK 为假时 Why 说明触发机制，Next 指向修复入口（只报不修）。
type Signal struct {
	ID     string `json:"id"`
	OK     bool   `json:"ok"`
	Count  int    `json:"count"`
	Why    string `json:"why,omitempty"`
	Next   string `json:"next"`
	Detail any    `json:"detail,omitempty"`
}

// Report 是 GET /health 的响应；Signals 恒为十项且顺序固定（与设计 §15.2 表一致）。
type Report struct {
	Healthy bool     `json:"healthy"`
	Signals []Signal `json:"signals"`
}

// Routes 返回健康路由分组，交给 core.Deps.Groups。ph 为空表示没有能力包加载器。
func Routes(st *store.Store, q *delivery.Queue, ph khealth.PackHealth) core.RouteGroup {
	h := &handlers{st: st, opts: khealth.Options{Deliveries: q, Packs: ph}}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/health", Handler: h.get})
	}
}

type handlers struct {
	st   *store.Store
	opts khealth.Options
}

func (h *handlers) get(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
	// ext 与 webhook 凭据另有用途，不能读账本（与 /why 一致）。
	if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
		return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
	}
	var v khealth.View
	if err := h.st.View(r.Context(), func(tx *store.Tx) (err error) {
		v, err = khealth.Collect(tx, h.opts)
		return
	}); err != nil {
		return err
	}
	return core.WriteJSON(w, http.StatusOK, build(v))
}

func ids[T any](xs []T, id func(T) string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, id(x))
	}
	return out
}

func build(v khealth.View) Report {
	sig := func(id string, n int, why, next string, detail any) Signal {
		s := Signal{ID: id, OK: n == 0, Count: n, Next: next, Detail: detail}
		if n > 0 {
			s.Why = why
		}
		return s
	}
	type sourceDetail struct {
		Source              string `json:"source"`
		Enabled             bool   `json:"enabled"`
		ConsecutiveFailures int    `json:"consecutive_failures"`
		NeedsInputItem      string `json:"needs_input_item,omitempty"`
	}
	srcs := make([]sourceDetail, 0, len(v.SourceFailures))
	for _, f := range v.SourceFailures {
		srcs = append(srcs, sourceDetail{f.Source, f.Enabled, f.ConsecutiveFailures, f.NeedsInputItem})
	}
	queue := func(q khealth.QueueHealth) map[string]any {
		return map[string]any{"running": q.Running, "queued": q.Queued, "longest_wait_seconds": int64(q.LongestWait.Seconds())}
	}
	outcall := map[string]any{"long": queue(v.Outcall.Long), "short": queue(v.Outcall.Short),
		"timeouts": ids(v.Outcall.Timeouts, func(r domain.Run) string { return r.ID })}
	type modelDetail struct {
		Kind   string `json:"kind"`
		Object string `json:"object"`
	}
	screening, judgements := []string{}, []modelDetail{}
	for _, m := range v.ModelUnavailable {
		if m.Kind == khealth.ScreeningKind {
			screening = append(screening, m.Object)
		} else {
			judgements = append(judgements, modelDetail{m.Kind, m.Object})
		}
	}
	unjudged := ids(v.Unjudged, func(e domain.SourceEvent) string { return e.ID })

	rep := Report{Healthy: v.Healthy(), Signals: []Signal{
		sig("source_failures", len(v.SourceFailures), "a source has consecutive pull failures, is disabled, or needs input",
			"source page; auth failures have a needs_input item", srcs),
		sig("outcall", len(v.Outcall.Timeouts), "outcall runs timed out; queue depth and longest wait are in detail",
			"run detail; if queues stay backed up, raise the matching quota in run settings", outcall),
		sig("delivery_backlog", len(v.DeliveryBacklog), "deliveries are queued or retrying without being sent",
			"channel ext and interruption policy", ids(v.DeliveryBacklog, func(d domain.Delivery) string { return d.ID })),
		sig("chain_trips", len(v.ChainTrips), "the chain-depth breaker stopped a command or background run",
			"item detail; re-delegate by hand", ids(v.ChainTrips, func(t chain.Trip) string { return t.Object })),
		sig("agentsmd_drift", len(v.AgentsMDDrift), "a rendered AGENTS.md no longer matches the repo file",
			"project page", ids(v.AgentsMDDrift, func(d agentsmd.OpenDrift) string { return d.Item })),
		sig("pack_rejections", len(v.PackRejections), "a pack failed to load; the last valid version is kept",
			"fix the pack, then hot-reload", ids(v.PackRejections, func(r packs.Rejection) string { return r.Pack })),
		sig("model_unavailable", len(v.ModelUnavailable)+len(unjudged), "a judge call failed or timed out and has not succeeded since, or source events remain unjudged",
			"settings page and source page", map[string]any{"screening": screening, "judgements": judgements, "unjudged": unjudged}),
		sig("rule_fallbacks", len(v.RuleFallbacks), "a rule fell back to observe after its content changed",
			"re-review the rule in Web", ids(v.RuleFallbacks, func(f khealth.RuleFallback) string { return f.Rule })),
		sig("receipt_failures", len(v.ReceiptFailures), "a receipt or capture was rejected by the kernel (exit 2, 4 or 23) and has not succeeded since for that credential and command",
			"audit page; credential or basis problems go to settings, a receipt turned needs_input is handled in decision todos",
			ids(v.ReceiptFailures, func(r khealth.ReceiptFailure) string { return r.Object })),
		sig("other_alerts", len(v.OtherAlerts), "a health alert without a dedicated signal was recorded, such as a failed rule action",
			"audit page; open the object named in detail", ids(v.OtherAlerts, func(a khealth.OtherAlert) string { return a.Object })),
	}}
	return rep
}
