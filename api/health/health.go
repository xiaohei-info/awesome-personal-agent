// Package health 是 GET /health 路由（附录 B.2，设计 §15.2）：把 kernel/health 汇总出的十三类信号
// 连同修复入口原样给出，`apa health` 与 Web 读同一份。
//
// 路由里没有业务规则：是否异常由 kernel/health 判定，这里只做格式搬运。
// why、next 只写机制（哪类记录、去哪里修），不带来源报错、事项标题等账本里的用户内容；
// detail 只给 id 与计数，需要内容的人经修复入口按自己的权限去看。
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	khealth "github.com/xiaohei-info/awesome-personal-agent/kernel/health"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Signal 是十三类信号之一。OK 为假时 Why 说明触发机制，Next 指向修复入口（只报不修），
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
	// Receipts 只有 receipt_failures 给：逐条列出被拒的命令、退出码与原因码，展示层据原因码说人话、给下一步。
	// detail 仍是对象 id 列表。
	Receipts []ReceiptRejection `json:"receipts,omitempty"`
}

// ReceiptRejection 是 receipt_failures 的一条：只含机制（命令、退出码、稳定的原因码、凭据），不带告警里的原因原文——
// 那可能夹着账本内容；要原文的人经操作记录按自己的权限去看。
type ReceiptRejection struct {
	Object     string      `json:"object"`
	Command    string      `json:"command"`
	Exit       int         `json:"exit"`
	Code       domain.Code `json:"code,omitempty"`
	Credential string      `json:"credential"`
}

// RunFailure 是 outcall.detail.failures 的一条：失败的 Agent 运行、为哪件事跑、故障类别码。
type RunFailure struct {
	Run      string             `json:"run"`
	Target   string             `json:"target"`
	Category domain.RunCategory `json:"category"`
	Fault    *domain.Fault      `json:"fault,omitempty"`
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

// Report 是 GET /health 的响应；Signals 恒为十三项且顺序固定（与设计 §15.2 表一致）。
type Report struct {
	Healthy bool     `json:"healthy"`
	Signals []Signal `json:"signals"`
}

// Routes 返回健康路由分组，交给 core.Deps.Groups。ph 为空表示没有能力包加载器。
// rejudge 是运行配置的自动重判窗口（unjudged_rejudge_days），只原样带进 unjudged_backlog 的 detail，0 表示关闭。
func Routes(st *store.Store, q *delivery.Queue, ph khealth.PackHealth, stallAfter, rejudge time.Duration) core.RouteGroup {
	h := &handlers{st: st, opts: khealth.Options{Deliveries: q, Packs: ph, DelegationStallAfter: stallAfter}, rejudgeDays: int(rejudge / (24 * time.Hour))}
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/health", Handler: h.get})
	}
}

type handlers struct {
	st          *store.Store
	opts        khealth.Options
	rejudgeDays int
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
	return core.WriteJSON(w, http.StatusOK, build(v, h.rejudgeDays))
}

func ids[T any](xs []T, id func(T) string) []string {
	out := make([]string, 0, len(xs))
	for _, x := range xs {
		out = append(out, id(x))
	}
	return out
}

func build(v khealth.View, rejudgeDays int) Report {
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
	failures := make([]RunFailure, 0, len(v.Outcall.Failures))
	for _, r := range v.Outcall.Failures {
		failures = append(failures, RunFailure{Run: r.ID, Target: r.Target, Category: r.Category, Fault: r.Fault})
	}
	outcall := map[string]any{"long": queue(v.Outcall.Long), "short": queue(v.Outcall.Short),
		"timeouts": ids(v.Outcall.Timeouts, func(r domain.Run) string { return r.ID }), "failures": failures}
	type modelDetail struct {
		Kind     string `json:"kind"`
		Object   string `json:"object"`
		Endpoint string `json:"endpoint"` // judge 或 generate：这次失败归哪个端点
	}
	screening, judgements := []string{}, []modelDetail{}
	for _, m := range v.ModelUnavailable {
		if m.Kind == khealth.ScreeningKind {
			screening = append(screening, m.Object)
		} else {
			judgements = append(judgements, modelDetail{m.Kind, m.Object, m.Endpoint})
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
	backlog := map[string]any{"model_unavailable": 0, "models_paused": 0, "outbound_unset": 0, "undecided": 0, "restored": 0, "expired": v.Expired, "rejudge_days": rejudgeDays}
	bySource, at := []sourceCount{}, map[string]int{}
	for _, e := range v.Unjudged {
		reason := domain.UnjudgedModelUnavailable // 原因为空是迁移前留下的，按模型不可用对待（同自动重判）
		switch e.UnjudgedReason {
		case domain.UnjudgedUndecided, domain.UnjudgedOutboundUnset, domain.UnjudgedRestored, domain.UnjudgedModelsPaused:
			reason = e.UnjudgedReason
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
	next := "source page: propose or ignore, one by one or per source"
	if backlog[domain.UnjudgedOutboundUnset].(int) > 0 {
		next = "source page: choose the external outbound scope of the sources in detail (held events are judged again once chosen); " + next
	}
	backlogSig := sig("unjudged_backlog", len(v.Unjudged), "source events are still waiting for a judgement; those held back for lack of a model, while the models were paused, or for lack of a chosen outbound scope are judged again automatically once it is fixed (within the rejudge window), the rest wait for the owner",
		next, backlog)
	if !backlogSig.OK {
		backlogSig.Severity = SeverityWarning
	}

	warn := func(s Signal) Signal { // 提醒级：异常时是 warning，不影响 healthy
		if !s.OK {
			s.Severity = SeverityWarning
		}
		return s
	}
	rep := Report{Healthy: v.Healthy(), Signals: []Signal{
		sig("source_failures", len(v.SourceFailures), "a source's latest pulls are failing (its auth or config failures have a needs_input item), or it is disabled; a successful pull clears it",
			"source page: open the source and fix its credentials or config", srcs),
		sig("outcall", len(v.Outcall.Timeouts)+len(v.Outcall.Failures), "an outcall run timed out, or an Agent run failed (did not start, exited non-zero), and it is still the latest run of its target (within 24 hours); its result was not recorded and the item was not changed; queue depth, longest wait and each failure's fault code are in detail",
			"run detail or the item; fix what the fault code names on the host, then start it again from the item or the schedule; if queues stay backed up, raise the matching quota in run settings", outcall),
		sig("delivery_backlog", len(v.DeliveryBacklog), "deliveries are queued or retrying without being sent",
			"channel ext and interruption policy", ids(v.DeliveryBacklog, func(d domain.Delivery) string { return d.ID })),
		sig("chain_trips", len(v.ChainTrips), "the chain-depth breaker stopped a command or background run, and the owner has not handled that item since (objectless trips clear after 24 hours)",
			"item detail; re-delegate by hand", ids(v.ChainTrips, func(t chain.Trip) string { return t.Object })),
		sig("agentsmd_drift", len(v.AgentsMDDrift), "a rendered AGENTS.md no longer matches the repo file",
			"project page", ids(v.AgentsMDDrift, func(d agentsmd.OpenDrift) string { return d.Item })),
		sig("pack_rejections", len(v.PackRejections), "a pack failed to load; the last valid version is kept",
			"fix the pack, then hot-reload", ids(v.PackRejections, func(r packs.Rejection) string { return r.Pack })),
		sig("model_unavailable", len(v.ModelUnavailable)+endpointDown, "the model endpoint is not configured while events wait for it, keeps failing, or a model call failed and that endpoint has not succeeded since (a real call or a passing test of the saved endpoint clears it)",
			"settings page: model endpoint", map[string]any{"judge": v.Judge, "generate": v.Generate, "endpoint": v.Endpoint, "paused": v.Generate == khealth.EndpointPaused,
				"screening": screening, "judgements": judgements}),
		backlogSig,
		sig("rule_fallbacks", len(v.RuleFallbacks), "a rule fell back to observe after its content changed and its mode has not been set since",
			"re-review the rule in Web", ids(v.RuleFallbacks, func(f khealth.RuleFallback) string { return f.Rule })),
		sig("receipt_failures", len(v.ReceiptFailures), "a receipt or capture was rejected by the kernel (exit 2, 4 or 23), has not succeeded since for that credential and command, and its item has not ended",
			"audit page; credential or basis problems go to settings, a receipt turned needs_input is handled in decision todos",
			ids(v.ReceiptFailures, func(r khealth.ReceiptFailure) string { return r.Object })),
		sig("other_alerts", len(v.OtherAlerts), "a health alert without a dedicated signal was recorded in the last 24 hours, such as a failed rule action, and its object has not been handled since",
			"audit page; open the object named in detail", ids(v.OtherAlerts, func(a khealth.OtherAlert) string { return a.Object })),
		warn(sig("stalled_delegations", len(v.StalledDelegations), "a delegated item has had no receipt or progress for longer than delegation_stall_after",
			"delegations page: nudge, reassign or delegate again", ids(v.StalledDelegations, func(d khealth.StalledDelegation) string { return d.Item }))),
		warn(sig("clock_lag", len(v.ClockLags), "a timer fired 5 minutes or more after its scheduled time (including timers missed while the kernel was down); clears after an hour without lateness",
			"health page; if it keeps happening, check host sleep and load", ids(v.ClockLags, func(l khealth.ClockLag) string { return l.Timer }))),
	}}
	rep = withRejections(rep, v.PackRejections)
	for i := range rep.Signals {
		if rep.Signals[i].ID == "receipt_failures" {
			for _, r := range v.ReceiptFailures {
				rep.Signals[i].Receipts = append(rep.Signals[i].Receipts, ReceiptRejection{r.Object, r.Command, r.Exit, r.Code, r.Credential})
			}
		}
	}
	return rep
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

// recordProbes：自检对已保存端点的探测通过时，经 kernel/health.RecordProbe 记下（只在该端点确有未恢复的失败时写），
// 诊断页因此在自检通过后不再显示该端点此前的失败（§15.2）。
func recordProbes(ctx context.Context, st *store.Store, ph khealth.PackHealth, checks []kdoctor.Check, actor domain.Actor) error {
	var eps []string
	for _, c := range checks {
		if c.Name != kdoctor.ModelEndpoint {
			continue
		}
		for _, d := range c.Details {
			if d.Status != kdoctor.OK || d.Code != kdoctor.CodeOK {
				continue
			}
			switch d.Subject {
			case "judge_endpoint":
				eps = append(eps, khealth.EndpointJudge)
			case "generate_endpoint":
				eps = append(eps, khealth.EndpointGenerate)
			}
		}
	}
	if len(eps) == 0 {
		return nil
	}
	return st.Update(ctx, func(tx *store.Tx) error {
		for _, ep := range eps {
			if err := khealth.RecordProbe(tx, ph, ep, actor, clock.Now()); err != nil {
				return err
			}
		}
		return nil
	})
}

// DoctorRoutes 返回 GET /doctor（附录 B.2，设计 §15.5）：内核一侧的自检结果，与 `apa doctor` 的对应项同一份代码。
// 涉及本机文件与宿主的检查（数据目录、运行配置、宿主插件、重复提醒）只在 CLI 里做，这里没有。
func DoctorRoutes(st *store.Store, env kdoctor.Env) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/doctor", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			if p.Caller.Kind == auth.Ext || p.Caller.Kind == auth.Webhook {
				return domain.NewError(domain.CodePermissionDenied, "credential kind cannot read the ledger", "use an owner or agent credential")
			}
			f, err := kdoctor.ReadFacts(r.Context(), st)
			if err != nil {
				return err
			}
			if env.Conns != nil {
				if err := f.ReadConnections(r.Context(), st, env.Conns, kdoctor.KeyFactOf(env.KeyFile)); err != nil {
					return err
				}
			}
			checks := kdoctor.Kernel(r.Context(), f, env)
			if err := recordProbes(r.Context(), st, env.Packs, checks, p.Caller.Actor); err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, kdoctor.Assemble(checks...))
		}})
	}
}
