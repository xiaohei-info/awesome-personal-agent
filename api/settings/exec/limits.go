package exec

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Fallback 是三项上限没设过时的初值（运行配置 config.yaml），与内核运行时取的同一份。
type Fallback struct{ Long, Short, Chain int }

// 改不了的规则（§14.5），稳定类别码，Web 按词表说明。
var fixedRules = []string{
	"runs_not_captured", "done_needs_evidence", "same_account_host",
}

// deniedWindow 是"最近被拦下"的统计窗口；deniedShown 是随清单返回的明细条数上限。
const (
	deniedWindow = 7 * 24 * time.Hour
	deniedShown  = 20
)

// Key 是这个 Agent 名下的一把钥匙；不含哈希与明文，id 只给前缀。
type Key struct {
	ID       string `json:"id"`
	Kind     string `json:"kind"`
	Audience string `json:"audience"`
	Sources  int    `json:"sources"` // 外部受众的钥匙给了几个来源
}

// Denial 是一次被拦下的命令，按四行说明：没做成什么、哪条限制、在哪看、谁能改（§14.5）。
type Denial struct {
	At          time.Time `json:"at"`
	Object      string    `json:"object"`
	Action      string    `json:"action"`
	Group       string    `json:"group"`
	Code        string    `json:"code"`
	Reason      string    `json:"reason"`
	Exit        int       `json:"exit"`
	WebURL      string    `json:"web_url,omitempty"`
	SettingsURL string    `json:"settings_url,omitempty"`
	OwnerOnly   bool      `json:"owner_only"` // 只有本人能做：权限卡也开放不了
}

// Limits 是 GET /agents/{agent}/limits：一个 Agent 当前受到的全部限制（权限卡加相关的全局与对象设置）。
type Limits struct {
	Agent            string      `json:"agent"`
	Card             policy.Card `json:"card"`
	Keys             []Key       `json:"keys"`
	ExecutorProjects int         `json:"executor_projects"` // 设了允许执行者名单的项目数
	RunTimeoutSec    int         `json:"run_timeout_sec"`
	LongRuns         int         `json:"long_runs"`
	ShortRuns        int         `json:"short_runs"`
	ChainDepth       int         `json:"chain_depth"`
	TrustedOnly      bool        `json:"trusted_only"` // 后台运行只看信任发件人的消息
	CaptureOff       int         `json:"capture_off"`  // 关了对话捕获的项目数
	FixedRules       []string    `json:"fixed_rules"`  // 改不了的规则
	DeniedWeek       int         `json:"denied_week"`  // 最近 7 天被拦下的次数
	Denied           []Denial    `json:"denied"`       // 最近的几次，新的在前
}

// LimitsRoutes 挂 GET /agents/{agent}/limits。agent 为 me 时取调用者自己所属的 Agent（apa why denied 用它）。
// 本人看任何 Agent；Agent 只能看自己（读属"查看"组，由 API 入口统一判）。
func LimitsRoutes(st *store.Store, fb Fallback) core.RouteGroup {
	return func(r *core.Router) {
		r.Handle(core.Route{Method: "GET", Path: "/agents/{agent}/limits", Handler: func(w http.ResponseWriter, r *http.Request, p *core.Principal) error {
			agent := r.PathValue("agent")
			if agent == "me" {
				agent = p.Caller.Agent()
			}
			if !p.Web && agent != p.Caller.Agent() {
				return domain.NewError(domain.CodePermissionDenied, "an Agent can only read its own limits", "use /agents/me/limits")
			}
			var out Limits
			err := st.View(r.Context(), func(tx *store.Tx) (err error) {
				out, err = limits(tx, agent, fb, clock.Now())
				return
			})
			if err != nil {
				return err
			}
			return core.WriteJSON(w, http.StatusOK, out)
		}})
	}
}

func limits(tx *store.Tx, agent string, fb Fallback, now time.Time) (Limits, error) {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return Limits{}, err
	}
	card := pol.Card(agent)
	out := Limits{Agent: agent, Card: card, Keys: []Key{}, RunTimeoutSec: int(pol.BackgroundRunTimeout() / time.Second),
		LongRuns: pol.Limits.Long(fb.Long), ShortRuns: pol.Limits.Short(fb.Short), ChainDepth: pol.Limits.Chain(fb.Chain),
		TrustedOnly: !card.RunsAllMessages, FixedRules: fixedRules, Denied: []Denial{}}
	creds, err := auth.New(nil).List(tx)
	if err != nil {
		return out, err
	}
	for _, c := range creds {
		owner := c.AgentID
		if owner == "" && c.Harness != "" {
			owner = "legacy:" + c.Harness
		}
		if owner != agent || !c.RevokedAt.IsZero() || agent == "" {
			continue
		}
		id := c.ID
		if len(id) > 8 {
			id = id[:8]
		}
		out.Keys = append(out.Keys, Key{ID: id, Kind: string(c.Kind), Audience: c.Audience, Sources: len(c.SourceScopes)})
	}
	prs, err := kp.List(tx)
	if err != nil {
		return out, err
	}
	for _, pr := range prs {
		if pr.AllowedExecutors != nil {
			out.ExecutorProjects++
		}
		if !pr.Capture {
			out.CaptureOff++
		}
	}
	since := now.Add(-deniedWindow)
	es, err := eventlog.QueryBefore(tx, eventlog.Filter{Type: domain.EvCommandRejected}, 0, 1000)
	if err != nil {
		return out, err
	}
	for _, e := range es { // QueryBefore 最新的在前
		ev := e.Event
		if ev.At.Before(since) {
			break
		}
		var pl struct {
			Action string  `json:"action"`
			Tier   string  `json:"tier"`
			Code   string  `json:"code"`
			Reason string  `json:"reason"`
			Exit   int     `json:"exit"`
			Agent  string  `json:"agent"`
			WebURL *string `json:"web_url"`
		}
		if json.Unmarshal(ev.Payload, &pl) != nil || pl.Agent != agent {
			continue
		}
		out.DeniedWeek++
		if len(out.Denied) < deniedShown {
			d := Denial{At: ev.At, Object: ev.Object, Action: pl.Action, Group: pl.Tier, Code: pl.Code, Reason: pl.Reason, Exit: pl.Exit,
				OwnerOnly: pl.Exit == domain.ExitWebOnly}
			if pl.WebURL != nil {
				d.WebURL = *pl.WebURL
			}
			if pl.Exit == domain.ExitTierClosed {
				d.SettingsURL = authz.SettingsURL(agent)
			}
			out.Denied = append(out.Denied, d)
		}
	}
	return out, nil
}
