package doctor

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
)

// PackView 是能力包加载器对自检的视图（*packs.Loader 满足）。
type PackView interface {
	Health() []packs.Rejection
	Source(name string) (packs.Source, bool)
	List() []*packs.Pack
}

// Env 是内核一侧检查依赖的外部事实。
type Env struct {
	Model *model.Client
	Packs PackView
	// CommandExists 判断来源登记的命令能否找到；为空按 PATH 与路径查找（内核不拉起进程，只看文件）。
	CommandExists func(command string) bool
	Now           func() time.Time
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return clock.Now()
}

// Kernel 读取账本事实后运行内核一侧的全部检查。
func Kernel(ctx context.Context, f Facts, e Env) []Check {
	return []Check{tier(f), modelEndpoint(ctx, f, e), sources(f, e), packCheck(e), channels(f, e)}
}

func ts(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ---- 运行形态（§2.4） ----

func hasHost(f Facts) bool {
	for _, d := range f.Declarations {
		if d.Declaration.Headless != nil {
			return true
		}
	}
	return false
}

func tier(f Facts) Check {
	gen := f.Policy.GenerateEndpoint
	model2 := gen.URL != "" && gen.Model != ""
	host := hasHost(f)
	c := Check{Name: Tier, Status: OK, Data: map[string]any{"model_endpoint": model2, "host": host}}
	switch {
	case !model2:
		c.Data["tier"] = 1
		c.Why = "tier 1 (kernel only): no generate endpoint is configured; this is a complete mode, not an error"
		c.Next = "to reach tier 2, configure the generate endpoint in Web settings (model endpoints)"
	case !host:
		c.Data["tier"] = 2
		c.Why = "tier 2 (kernel + model endpoint): no confirmed headless declaration from a host harness"
		c.Next = "to reach tier 3, install the host plugin and confirm its declaration in Web"
	default:
		c.Data["tier"] = 3
		c.Why = "tier 3 (kernel + model endpoint + host): a generate endpoint and a confirmed host declaration are in place"
		c.Next = "nothing to do"
	}
	return c
}

// ---- 模型端点（§8.8） ----

func modelEndpoint(ctx context.Context, f Facts, e Env) Check {
	c := Check{Name: ModelEndpoint, Status: OK}
	c.Add(ProbeJudge(ctx, f.Policy.JudgeEndpoint, e))
	c.Add(ProbeGenerate(ctx, f.Policy.GenerateEndpoint, e))
	switch c.Status {
	case OK:
		c.Why, c.Next = "configured endpoints answered the built-in sample request; unconfigured ones are not an error", "nothing to do"
	default:
		c.Why, c.Next = "an endpoint did not answer the built-in sample request as the protocol requires; see details", "fix the endpoint in Web settings (model endpoints)"
	}
	return c
}

// ---- 来源（§9、§14.14） ----

func sources(f Facts, e Env) Check {
	c := Check{Name: Sources, Status: OK}
	external := ExternalInUse(f.Policy)
	for _, s := range f.Sources {
		c.Add(SourceDetail(s, external, e.exists))
	}
	if len(f.Sources) == 0 {
		c.Why, c.Next = "no source is registered (optional)", "register sources in Web or in sources.json when you want events to flow in"
		return c
	}
	if c.Status == OK {
		c.Why, c.Next = fmt.Sprintf("%d sources: commands exist and the outbound scope fits the endpoints", len(f.Sources)), "nothing to do"
	} else {
		c.Why, c.Next = "some sources need attention; see details", "fix the registration or choose the outbound scope on the source page"
	}
	return c
}

func (e Env) exists(cmd string) bool {
	if e.CommandExists != nil {
		return e.CommandExists(cmd)
	}
	return CommandExists(cmd)
}

// CommandExists 按来源执行的方式（命令按空白切分，第一段为程序）判断程序是否存在且可执行：含 / 的当路径，否则在 PATH 里找。
func CommandExists(command string) bool {
	argv := strings.Fields(command)
	if len(argv) == 0 {
		return false
	}
	ok := func(p string) bool {
		st, err := os.Stat(p)
		return err == nil && st.Mode().IsRegular() && st.Mode().Perm()&0o111 != 0
	}
	if strings.ContainsRune(argv[0], '/') {
		return ok(argv[0])
	}
	for _, d := range filepath.SplitList(os.Getenv("PATH")) {
		if d == "" {
			d = "."
		}
		if ok(filepath.Join(d, argv[0])) {
			return true
		}
	}
	return false
}

// SourceDetail 给出一个来源的结论：命令是否存在、外发范围是否与端点位置相符、最近一次运行结果。
// 只写机制与时间，不带来源报错原文（可能含用户内容）。external 为真表示配置了外部端点。
func SourceDetail(s domain.Source, external bool, exists func(string) bool) Detail {
	d := Detail{Subject: s.ID, Status: OK, Data: map[string]any{"enabled": s.Enabled, "type": string(s.Type)}}
	var why, next []string
	if s.Type != domain.SourceWebhook {
		if exists(s.Command) {
			d.Data["command"] = "found"
		} else {
			d.Status = Error
			d.Data["command"] = "missing"
			why = append(why, "the registered command does not exist or is not executable on this host")
			next = append(next, "fix the command registered for this source")
		}
	}
	ext := "unset"
	if s.Outbound.External != nil {
		ext = *s.Outbound.External
	}
	d.Data["outbound"] = map[string]any{"local": s.Outbound.Local, "external": ext}
	switch {
	case external && s.Outbound.External == nil:
		d.Status = Worst(d.Status, Warning)
		why = append(why, "the external outbound scope is not chosen; new events are held as outbound_unset until you choose")
		next = append(next, "choose the external outbound scope of this source in Web")
	case external && ext == registry.ScopeFull:
		d.Status = Worst(d.Status, Warning)
		why = append(why, "full text of this source is being sent to an external model endpoint")
		next = append(next, "narrow the external outbound scope on the source page if that is not intended")
	}
	st, err := runner.StatsOf(s)
	switch {
	case err != nil:
		d.Data["last_run"] = "unreadable"
	case st.Runs == 0 && st.LastSuccessAt.IsZero() && st.LastFailureAt.IsZero():
		d.Data["last_run"] = "never"
	case st.ConsecutiveFailures > 0:
		d.Status = Worst(d.Status, Warning)
		d.Data["last_run"] = "failed"
		d.Data["consecutive_failures"] = st.ConsecutiveFailures
		d.Data["last_failure_at"] = ts(st.LastFailureAt)
		why = append(why, fmt.Sprintf("the last run failed (%d consecutive failures)", st.ConsecutiveFailures))
		next = append(next, "open the source page; auth failures have a needs_input item")
	default:
		d.Data["last_run"] = "ok"
		d.Data["last_success_at"] = ts(st.LastSuccessAt)
	}
	if len(why) == 0 {
		d.Why, d.Next = "command found, outbound scope fits the endpoints, last run: "+fmt.Sprint(d.Data["last_run"]), ""
	} else {
		d.Why, d.Next = strings.Join(why, "; "), strings.Join(next, "; ")
	}
	return d
}

// ---- 能力包（附录 C.11） ----

func packCheck(e Env) Check {
	c := Check{Name: Packs, Status: OK}
	if e.Packs == nil {
		c.Why, c.Next = "no pack loader is attached", "nothing to do"
		return c
	}
	rejected := e.Packs.Health()
	for _, r := range rejected {
		why := "pack rejected: " + r.Reason
		if cur, ok := e.Packs.Source(r.Pack); ok {
			why += fmt.Sprintf("; the %s pack of the same name is in effect", cur)
		}
		c.Add(Detail{Subject: r.Pack, Status: Error, Why: why,
			Next: "fix the pack under packs/" + r.Pack + " as appendix C.7 and C.11 describe; it is loaded again on the next hot reload or restart",
			Data: map[string]any{"source": string(r.Source)}})
	}
	c.Data = map[string]any{"loaded": len(e.Packs.List()), "rejected": len(rejected)}
	if len(rejected) == 0 {
		c.Why, c.Next = fmt.Sprintf("%d packs loaded, none rejected", len(e.Packs.List())), "nothing to do"
	} else {
		c.Why, c.Next = fmt.Sprintf("%d pack(s) were rejected; the last valid or builtin version stays in effect", len(rejected)), "fix the packs listed in details"
	}
	return c
}

// ---- 渠道与导出（§10.3） ----

func channels(f Facts, e Env) Check {
	c := Check{Name: Channels, Status: OK}
	covered := map[string]bool{}
	keys := append([]ExtKey(nil), f.ExtKeys...)
	sort.Slice(keys, func(i, j int) bool { return keys[i].Scope < keys[j].Scope })
	for _, k := range keys {
		d := Detail{Subject: k.Scope, Status: OK, Data: map[string]any{"kind": "export"}}
		last, seen := f.Seen[SubjectExt+k.Scope]
		if seen {
			d.Data["last_seen_at"] = ts(last)
		}
		when := "never"
		if seen {
			when = ts(last)
		}
		if name := ChannelName(k.Scope); name != "" {
			route := delivery.ChannelRoute(name)
			covered[route] = true
			n := f.Backlog[route]
			d.Data["kind"], d.Data["backlog"] = "channel", n
			if n > 0 {
				d.Status = Warning
				d.Why = fmt.Sprintf("%d deliveries are queued and not yet sent; the channel ext last claimed at %s", n, when)
				d.Next = "start the channel ext (apa ext run) and check its key; queued deliveries fall back to Web after delivery_unclaimed_after"
			} else {
				d.Why = fmt.Sprintf("no delivery backlog; the channel ext last claimed at %s", when)
			}
		} else {
			d.Why = "the ext key last read at " + when
		}
		c.Add(d)
	}
	var orphan []string
	for route, n := range f.Backlog {
		if strings.HasPrefix(route, "channel:") && !covered[route] && n > 0 {
			orphan = append(orphan, route)
		}
	}
	sort.Strings(orphan)
	for _, route := range orphan {
		c.Add(Detail{Subject: route, Status: Warning, Data: map[string]any{"kind": "channel", "backlog": f.Backlog[route]},
			Why:  fmt.Sprintf("%d deliveries are queued on a route that has no active ext key", f.Backlog[route]),
			Next: "issue a channel ext key in Web settings for this channel, or let the queued deliveries fall back to Web"})
	}
	switch {
	case len(c.Details) == 0:
		c.Why, c.Next = "no channel or export ext key is issued (optional)", "issue an ext key in Web settings when you want a channel or an export"
	case c.Status == OK:
		c.Why, c.Next = "no delivery backlog; last claim and read times are in details", "nothing to do"
	default:
		c.Why, c.Next = "deliveries are waiting for a channel ext; see details", "start the channel ext or check its key"
	}
	return c
}
