// Package health 是 GET /health 路由（附录 B.2，设计 §15.2）：把 kernel/health 汇总出的十一类信号
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

// Signal 是十一类信号之一。OK 为假时 Why 说明触发机制，Next 指向修复入口（只报不修），
// Severity 是 error（异常）或 warning（待本人处理的提醒，不影响 healthy）。
type Signal struct {
	ID       string `json:"id"`
	OK       bool   `json:"ok"`
	Count    int    `json:"count"`
	Severity string `json:"severity,omitempty"`
	Why      string `json:"why,omitempty"`
	Next     string `json:"next"`
	Detail   any    `json:"detail,omitempty"`
	// Rejections 只有 pack_rejections 给：逐个列出被拒的包、来源与原因（要改的字段和 C.7 的新写法）。detail 仍是包名列表。
	Rejections []PackRejection `json:"rejections,omitempty"`
}

// PackRejection 是 pack_rejections 的一条。
type PackRejection struct {
	Pack   string `json:"pack"`
	Source string `json:"source"`
	Reason string `json:"reason"`
}

// Severity 的取值。
const (
	SeverityError   = "error"
	SeverityWarning = "warning"
)

// Report 是 GET /health 的响应；Signals 恒为十一项且顺序固定（与设计 §15.2 表一致）。
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
			s.Why, s.Severity = why, SeverityError
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
	endpointDown := 0
	if v.Endpoint != "" {
		endpointDown = 1
	}
	type sourceCount struct {
		Source string `json:"source"`
		Count  int    `json:"count"`
	}
	backlog := map[string]any{"model_unavailable": 0, "undecided": 0, "expired": v.Expired}
	bySource, at := []sourceCount{}, map[string]int{}
	for _, e := range v.Unjudged {
		reason := domain.UnjudgedModelUnavailable // 原因为空是迁移前留下的，按模型不可用对待（同自动重判）
		if e.UnjudgedReason == domain.UnjudgedUndecided {
			reason = domain.UnjudgedUndecided
		}
		backlog[reason] = backlog[reason].(int) + 1
		i, ok := at[e.SourceID]
		if !ok {
			i, at[e.SourceID] = len(bySource), len(bySource)
			bySource = append(bySource, sourceCount{Source: e.SourceID})
		}
		bySource[i].Count++
	}
	backlog["sources"] = bySource
	backlogSig := sig("unjudged_backlog", len(v.Unjudged), "source events are still waiting for a judgement; those held back for lack of a model are judged again automatically once it answers, the rest wait for the owner",
		"source page: propose or ignore, one by one or per source", backlog)
	if !backlogSig.OK {
		backlogSig.Severity = SeverityWarning
	}

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
		sig("model_unavailable", len(v.ModelUnavailable)+endpointDown, "the model endpoint is not configured while events wait for it, keeps failing, or a judge call failed and has not succeeded since",
			"settings page: model endpoint", map[string]any{"endpoint": v.Endpoint, "screening": screening, "judgements": judgements}),
		backlogSig,
		sig("rule_fallbacks", len(v.RuleFallbacks), "a rule fell back to observe after its content changed",
			"re-review the rule in Web", ids(v.RuleFallbacks, func(f khealth.RuleFallback) string { return f.Rule })),
		sig("receipt_failures", len(v.ReceiptFailures), "a receipt or capture was rejected by the kernel (exit 2, 4 or 23) and has not succeeded since for that credential and command",
			"audit page; credential or basis problems go to settings, a receipt turned needs_input is handled in decision todos",
			ids(v.ReceiptFailures, func(r khealth.ReceiptFailure) string { return r.Object })),
		sig("other_alerts", len(v.OtherAlerts), "a health alert without a dedicated signal was recorded, such as a failed rule action",
			"audit page; open the object named in detail", ids(v.OtherAlerts, func(a khealth.OtherAlert) string { return a.Object })),
	}}
	return withRejections(rep, v.PackRejections)
}

// withRejections 给 pack_rejections 补上逐字段的原因（C.11）；本人据此就能修。
func withRejections(rep Report, rs []packs.Rejection) Report {
	for i := range rep.Signals {
		if rep.Signals[i].ID != "pack_rejections" {
			continue
		}
		for _, r := range rs {
			rep.Signals[i].Rejections = append(rep.Signals[i].Rejections, PackRejection{r.Pack, string(r.Source), r.Reason})
		}
	}
	return rep
}
