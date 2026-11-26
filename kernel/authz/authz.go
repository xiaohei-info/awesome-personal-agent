// Package authz 是统一授权判定（设计 §7.7、§14.3）：输入调用者、动作、档位、对象，
// 输出允许或拒绝（带退出码与原因）。纯函数，无 I/O，不写任何数据。
//
// 判定顺序固定：硬线（3b，不读档位）→ 钥匙范围 → 档位 → 项目允许的执行者。
// 钥匙只决定能碰哪些对象；档位只决定能做到哪一档。调用方自报的原话、会话类型、宿主名都不是输入。
package authz

import (
	"encoding/json"
	"net/url"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// Level 是调用者的钥匙种类（§14.2）。LevelWeb 只授予 Web 上的本人。
type Level string

const (
	LevelObserver Level = "observer"
	LevelWorker   Level = "worker"
	LevelDelegate Level = "delegate"
	LevelRun      Level = "run" // 运行凭据：只标明哪次运行、属于哪个 Agent；范围与 delegate 钥匙相同，权限随所属 Agent
	LevelWeb      Level = "web"
)

// Action 决定钥匙范围（能碰哪些对象），与档位无关。
type Action string

const (
	ActionRead            Action = "read"
	ActionWebRead         Action = "web_read" // 只有 Web 本人能读（钥匙清单、待确认的接入声明）
	ActionCreateProposed  Action = "create_proposed"
	ActionUpdateOwn       Action = "update_own" // 自己的事项、回执、建子事项与 Timer、提交决策请求
	ActionReviseMaterial  Action = "revise_material"
	ActionDelegate        Action = "delegate"
	ActionEditCheckout    Action = "edit_checkout"
	ActionSubmitPlugin    Action = "submit_plugin"
	ActionEditAnyItem     Action = "edit_any_item"
	ActionAnswerDecision  Action = "answer_decision" // 回答问题、处理建议、确认修订
	ActionManageProject   Action = "manage_project"
	ActionManageSource    Action = "manage_source"
	ActionChangeSettings  Action = "change_settings"
	ActionChannelAnswer   Action = "channel_answer" // 本人经已配对渠道回答，凭渠道回答人名单
	ActionReassign        Action = "reassign"
	ActionMoveIntoProject Action = "move_into_project"
	ActionRuleCreateChild Action = "rule_create_child"
	ActionRunnerTask      Action = "runner_task"
)

// Group 是命令所属的组（§14.3）：权限卡上的十组，加只有本人与系统内部。零值无效：每条命令必须登记一组。
type Group string

const (
	GroupView         Group = policy.GroupView
	GroupOrganize     Group = policy.GroupOrganize
	GroupSourceEvents Group = policy.GroupSourceEvents
	GroupDecide       Group = policy.GroupDecide
	GroupRules        Group = policy.GroupRules
	GroupConnect      Group = policy.GroupConnect
	GroupOutbound     Group = policy.GroupOutbound
	GroupChannels     Group = policy.GroupChannels
	GroupAgents       Group = policy.GroupAgents
	GroupPurge        Group = policy.GroupPurge
	GroupOwner        Group = "owner"    // 只有本人：改权限卡、钥匙、口令与会话、渠道回答人；任何设置都开放不了
	GroupInternal     Group = "internal" // 系统内部：只有内核自己（与 Web 本人）能调
)

// Grave 报告组 g 是否后果较大：Web 本人做要刚输过口令，规则与时钟做不了（§14.3、§14.13）。
func (g Group) Grave() bool {
	switch g {
	case GroupConnect, GroupOutbound, GroupChannels, GroupAgents, GroupPurge, GroupOwner:
		return true
	}
	return false
}

// OnCard 报告组 g 是否是权限卡上的一组（只有本人与系统内部不在卡上）。
func (g Group) OnCard() bool { return g != "" && g != GroupOwner && g != GroupInternal }

// Object 是动作的对象。全部字段来自账本或内核，不来自调用方。
type Object struct {
	ID       string
	Assignee string // 事项当前的执行者

	Kind    string
	Project string // "替我拍板"的项目限定按它比对

	// 执行者校验点（§14.7）。ProjectInfo 为 nil 表示无项目，不校验。
	ProjectInfo *domain.Project
	Executor    string
	// Candidates 是名录里当前在名单内的执行者 id，只随拒绝返回。
	Candidates []string

	// ToProject 非 nil 表示命令要把对象移入该项目（空串为移出）："替我拍板"的项目限定要求来源与目标都在范围内。
	ToProject *string

	// Creator 仅供调用方留痕；判定从不读取它。
	Creator domain.Actor
}

// Request 是一次判定的全部输入。
type Request struct {
	Actor  domain.Actor
	Level  Level
	Action Action
	Group  Group
	Object Object
	// Fresh：Web 会话刚再输过一次口令（后果较大的组与只有本人的事要求，§14.3）。只由内核的会话层给出。
	Fresh bool
	// Agent 与 Card：调用者所属的 Agent（内核记录的凭据归属或运行记录）与它的权限卡；没有归属时为默认卡。
	Agent   string
	Card    policy.Card
	Now     time.Time // 判定时刻：权限卡上的开放到期由它比较得出
	Owner   string    // 本人的执行者 id，指派给本人始终允许
	WebURL  string    // 对象在 Web 的页面：本人可以直接做这件事的地方
	Page    string    // 动作所在的设置页；空则用 WebURL
	Channel string    // ActionChannelAnswer：渠道
	Sender  string    // ActionChannelAnswer：回答人
}

type Verdict string

const (
	Allow Verdict = "allow"
	Deny  Verdict = "deny"
)

// Decision 是判定结果；Err 在 Allow 时为 nil。
type Decision struct {
	Verdict Verdict
	Err     *domain.Error
}

// ExitCode 是 CLI 退出码：允许为 0。
func (d Decision) ExitCode() int {
	if d.Err == nil {
		return domain.ExitOK
	}
	return d.Err.ExitCode()
}

var allow = Decision{Verdict: Allow}

func reject(code domain.Code, reason, why, next, link string) Decision {
	e := domain.NewError(code, why, next)
	e.Reason = reason
	if link != "" {
		e.WebURL = &link
	}
	return Decision{Verdict: Deny, Err: e}
}

func outOfScope() Decision {
	return reject(domain.CodePermissionDenied, domain.ReasonOutOfScope, "this key cannot reach this object", "do not retry with this key", "")
}

// directURL 是本人可以直接做这件事的页面：拍板落在对象页，其余落在动作所在的设置页。
func directURL(r Request) string {
	if r.Group == GroupDecide && r.WebURL != "" {
		return r.WebURL
	}
	return pick(r.Page, r.WebURL)
}

func pick(page, url string) string {
	if page != "" {
		return page
	}
	return url
}

// SettingsURL 是 Agent 权限卡的页面，被拦时作为"去哪开"返回。
func SettingsURL(agent string) string {
	if agent == "" {
		return "/settings/agents"
	}
	return "/settings/agents/" + url.PathEscape(agent) + "#permissions"
}

// Decide 对 r 给出判定。p 是当前生效的策略版本（只用到渠道回答人名单）。
func Decide(p policy.Policy, r Request) Decision {
	if !validLevel(r) {
		return outOfScope()
	}
	if r.Action == ActionChannelAnswer {
		return decideChannel(p, r)
	}
	if r.Group == "" {
		return reject(domain.CodeInvalidRequest, "", "command has no group", "report this as a kernel bug", "")
	}
	web, system := r.Level == LevelWeb, r.Actor.Type == domain.ActorSystem
	// 1 只有本人与后果较大的组：Web 本人须刚输过口令；规则与时钟做不了；Agent 做只有本人的事一律 20，任何设置都开放不了。
	switch {
	case web && r.Group.Grave() && !r.Fresh:
		return reject(domain.CodeReauthRequired, domain.ReasonReauthRequired, "this action needs the password entered again",
			"enter the password again and retry", "")
	case !web && (r.Group == GroupOwner || system && r.Group.Grave()):
		return reject(domain.CodeWebOnly, domain.ReasonHardLine, "only the owner can do this, in the Web UI; no setting opens it to an Agent",
			"give the Web link to the owner and stop; do not retry", pick(r.Page, r.WebURL))
	}
	// 2 钥匙范围。
	if !inScope(r) {
		return outOfScope()
	}
	// 3 权限卡：Web 本人与内核自己（规则、时钟，依据是本人开启的规则模式）不看权限卡。
	if !web && !system {
		if r.Group == GroupInternal { // 只给内核自己：外部钥匙调不到，也不能被权限卡开放
			return reject(domain.CodeWebOnly, domain.ReasonHardLine, "this command is run by the kernel itself", "do not call it", "")
		}
		if !cardAllows(r) {
			d := reject(domain.CodeTierClosed, domain.ReasonTierClosed, "this Agent's permission card does not allow "+string(r.Group)+" here",
				"do not retry; hand this to the owner: the group can be opened at settings_url, or the owner can do it at web_url", directURL(r))
			d.Err.Group = string(r.Group)
			su := SettingsURL(r.Agent)
			d.Err.SettingsURL = &su
			return d
		}
	}
	return checkExecutor(r)
}

// cardAllows：卡上这一组开着且没到期；"替我拍板"设了项目限定时对象（与要移入）的项目也在名单内。
func cardAllows(r Request) bool {
	if !r.Card.Allows(string(r.Group), r.Now) {
		return false
	}
	if r.Group != GroupDecide {
		return true
	}
	return inProjects(r.Card, r.Object.Project) && (r.Object.ToProject == nil || inProjects(r.Card, *r.Object.ToProject))
}

func inProjects(c policy.Card, project string) bool {
	return len(c.DecideProjects) == 0 || contains(c.DecideProjects, project)
}

// DecideOpen 报告权限卡 c 在 now 时是否在项目 project 上开放了"替我拍板"。
// 材料"改已有直接生效还是留待确认"也按它（§12.4）。
func DecideOpen(c policy.Card, project string, now time.Time) bool {
	return c.Allows(policy.GroupDecide, now) && inProjects(c, project)
}

// CanRead 是只读路由的钥匙范围判定（无效级别与 ext、webhook 之类读不到账本的钥匙为 4）；
// "查看"组由 API 入口对全部读取统一判定（§14.3），这里不看权限卡。
func CanRead(r Request) Decision {
	r.Action = ActionRead
	if !validLevel(r) || !inScope(r) {
		return outOfScope()
	}
	return allow
}

func validLevel(r Request) bool {
	switch r.Level {
	case LevelObserver, LevelWorker, LevelDelegate, LevelRun:
		return true
	case LevelWeb:
		return r.Actor.Type == domain.ActorHuman && r.Actor.Via == domain.ViaWeb
	}
	return false
}

func own(r Request) bool { return r.Object.Assignee != "" && r.Object.Assignee == r.Actor.Principal }

// inScope 是钥匙范围（§14.2）：observer 只读与提建议；worker 自己的事项；delegate 与运行凭据全部；Web 本人全部。
func inScope(r Request) bool {
	if r.Level == LevelWeb {
		return true
	}
	switch r.Action {
	case ActionRead, ActionCreateProposed:
		return true
	case ActionWebRead:
		return false
	case ActionReassign, ActionMoveIntoProject, ActionRuleCreateChild, ActionRunnerTask:
		return true // 只是 §14.7 的校验点，范围由发起它们的命令判定
	}
	switch r.Level {
	case LevelWorker:
		switch r.Action {
		case ActionUpdateOwn, ActionDelegate, ActionAnswerDecision:
			return own(r)
		case ActionReviseMaterial, ActionSubmitPlugin, ActionEditCheckout:
			return true
		}
	case LevelDelegate, LevelRun:
		return true
	}
	return false
}

// decideChannel：本人经已配对渠道回答。回答人在名单内即是本人，不看权限卡；名单为空等于这个渠道不能回答。
func decideChannel(p policy.Policy, r Request) Decision {
	if r.Sender == "" || !contains(p.ChannelAnswerers[r.Channel], r.Sender) {
		return reject(domain.CodePermissionDenied, domain.ReasonOutOfScope, "sender is not an allowed answerer for this channel", "ask the owner to answer in the Web UI", r.WebURL)
	}
	return allow
}

// checkExecutor 是 §14.7 的硬约束，对所有调用者（含 Web）生效；只按执行者 id 名单。
func checkExecutor(r Request) Decision {
	switch r.Action {
	case ActionDelegate, ActionReassign, ActionMoveIntoProject, ActionRuleCreateChild, ActionRunnerTask:
	default:
		return allow
	}
	pr := r.Object.ProjectInfo
	if pr == nil || pr.ExecutorAllowed(domain.ExecutorRef{ID: r.Object.Executor}, r.Owner) {
		return allow
	}
	e := domain.NewError(domain.CodeExecutorNotAllowed, "executor is not allowed in this project", "pick one of the candidates or an allowed executor, or turn the item to needs_input")
	e.Reason = domain.ReasonOutOfScope
	e.Allowed = append([]string{}, pr.AllowedExecutors...)
	e.Candidates = append([]string{}, r.Object.Candidates...)
	return Decision{Verdict: Deny, Err: e}
}

// ForwardAllowed 是信任的发件人名单（§14.6）：发送者缺失或未核实，按不在名单处理。
func ForwardAllowed(p policy.Policy, source, sender string, verified bool) bool {
	return sender != "" && verified && contains(p.ForwardAllowlist[source], sender)
}

// RejectionEvent 构造应记账的拒绝事件（§6.7）：只有本人（20）、权限卡没开（23）、钥匙范围外（4，限范围判定）、
// 项目不允许的执行者（21），一条留痕路径。再输口令（reauth_required）是本人还没完成的操作，不记。
// 判定本身无副作用，由调用方在自己的事务里追加。
func RejectionEvent(r Request, d Decision) (domain.Event, bool) {
	if d.Err == nil {
		return domain.Event{}, false
	}
	switch {
	case d.Err.Code == domain.CodeWebOnly, d.Err.Code == domain.CodeTierClosed, d.Err.Code == domain.CodeExecutorNotAllowed:
	case d.Err.Code == domain.CodePermissionDenied && d.Err.Reason == domain.ReasonOutOfScope:
	default:
		return domain.Event{}, false
	}
	// tier 键沿用旧名，取值是组键（附录 B.6）；web_url 供"被拦明细"与 apa why denied 给出本人直接做这件事的地方。
	payload, _ := json.Marshal(map[string]any{"action": r.Action, "level": r.Level, "tier": string(r.Group), "code": d.Err.Code, "reason": d.Err.Reason,
		"exit": d.ExitCode(), "why": d.Err.Why, "agent": r.Agent, "web_url": d.Err.WebURL})
	return domain.Event{Type: domain.EvCommandRejected, Object: r.Object.ID, Actor: r.Actor, Reason: d.Err.Why, Payload: payload}, true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}
