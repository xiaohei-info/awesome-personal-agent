// Package authz 是统一授权判定（设计 §7.7、§14.3）：输入调用者、动作、档位、对象，
// 输出允许或拒绝（带退出码与原因）。纯函数，无 I/O，不写任何数据。
//
// 判定顺序固定：硬线（3b，不读档位）→ 钥匙范围 → 档位 → 项目允许的执行者。
// 钥匙只决定能碰哪些对象；档位只决定能做到哪一档。调用方自报的原话、会话类型、宿主名都不是输入。
package authz

import (
	"encoding/json"

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

// Tier 是动作按后果固定的档（§14.3）。零值无效：每条命令必须登记一档。
type Tier int

const (
	TierUnset    Tier = iota
	TierRead          // 只读
	TierOrganize      // 一档 整理：永远开，可撤回
	TierDecide        // 二档 拍板：Agent 档位 ≥ decide 且对象项目在范围内
	TierRules         // 3a 改日常：Agent 档位 = rules
	TierPower         // 3b 造新权力：永不放开，只有 Web 本人且刚再输过口令
	TierInternal      // 系统内部：只有内核自己（与 Web 本人）能调，Agent 一律硬线
)

var tierNames = map[Tier]string{TierRead: "read", TierOrganize: "organize", TierDecide: "decide", TierRules: "rules", TierPower: "power", TierInternal: "internal"}

func (t Tier) String() string { return tierNames[t] }

// Object 是动作的对象。全部字段来自账本或内核，不来自调用方。
type Object struct {
	ID       string
	Assignee string // 事项当前的执行者

	Kind    string
	Project string // 二档的项目限定按它比对

	// 执行者校验点（§14.7）。ProjectInfo 为 nil 表示无项目，不校验。
	ProjectInfo *domain.Project
	Executor    string
	// Candidates 是名录里当前在名单内的执行者 id，只随拒绝返回。
	Candidates []string

	// ToProject 非 nil 表示命令要把对象移入该项目（空串为移出）：二档的项目限定要求来源与目标都在范围内。
	ToProject *string

	// Creator 仅供调用方留痕；判定从不读取它。
	Creator domain.Actor
}

// Request 是一次判定的全部输入。
type Request struct {
	Actor  domain.Actor
	Level  Level
	Action Action
	Tier   Tier
	Object Object
	// Fresh：Web 会话刚再输过一次口令（3b 要求，§14.3）。只由内核的会话层给出。
	Fresh bool
	// Agent 与 Allowance：调用者所属的 Agent（内核记录的凭据归属或运行记录）与它的档位；没有归属时为只整理。
	Agent     string
	Allowance policy.AgentAllowance
	Owner     string // 本人的执行者 id，指派给本人始终允许
	WebURL    string // 对象在 Web 的页面，二档拒绝时返回
	Page      string // 动作所在的设置页，3a/3b 拒绝时返回；空则用 WebURL
	Channel   string // ActionChannelAnswer：渠道
	Sender    string // ActionChannelAnswer：回答人
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

func pick(page, url string) string {
	if page != "" {
		return page
	}
	return url
}

// Decide 对 r 给出判定。p 是当前生效的策略版本（只用到渠道回答人名单）。
func Decide(p policy.Policy, r Request) Decision {
	if !validLevel(r) {
		return outOfScope()
	}
	if r.Action == ActionChannelAnswer {
		return decideChannel(p, r)
	}
	if r.Tier == TierUnset {
		return reject(domain.CodeInvalidRequest, "", "command has no tier", "report this as a kernel bug", "")
	}
	// 1 硬线：3b 不读档位，任何设置都放不开。
	if r.Tier == TierPower {
		switch {
		case r.Level == LevelWeb && r.Fresh:
		case r.Level == LevelWeb:
			return reject(domain.CodeReauthRequired, domain.ReasonReauthRequired, "this action creates new authority and needs the password entered again",
				"enter the password again and retry", "")
		default:
			return reject(domain.CodeWebOnly, domain.ReasonHardLine, "only the owner can do this, in the Web UI; no setting opens it to an Agent",
				"give the Web link to the owner and stop; do not retry", pick(r.Page, r.WebURL))
		}
	}
	// 2 钥匙范围。
	if !inScope(r) {
		return outOfScope()
	}
	// 3 档位：Web 本人与内核自己（规则、时钟，依据是本人开启的规则模式）不看档位。
	if r.Level != LevelWeb && r.Actor.Type != domain.ActorSystem {
		switch r.Tier {
		case TierDecide:
			if !decideOpen(r) {
				return reject(domain.CodeTierClosed, domain.ReasonTierClosed, "this Agent may not decide this for the owner",
					"do not retry; hand the Web link to the owner, or ask the question with apa ask", r.WebURL)
			}
		case TierRules:
			if r.Allowance.Tier != policy.AllowRules {
				return reject(domain.CodeTierClosed, domain.ReasonTierClosed, "this Agent may not change everyday rules",
					"do not retry; give the Web link to the owner", pick(r.Page, r.WebURL))
			}
		case TierInternal: // 只给内核自己：外部钥匙调不到，也不能被档位放开
			return reject(domain.CodeWebOnly, domain.ReasonHardLine, "this command is run by the kernel itself",
				"do not call it", "")
		}
	}
	return checkExecutor(r)
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

// decideOpen：档位放开二档，且设了项目限定时要移入的项目也在名单内。
func decideOpen(r Request) bool {
	return DecideOpen(r.Allowance, r.Object.Project) && (r.Object.ToProject == nil || DecideOpen(r.Allowance, *r.Object.ToProject))
}

// DecideOpen 报告档位 a 是否在项目 project 上放开了二档（替本人拍板）；设了项目限定时对象的项目须在名单内。
// 材料"改已有直接生效还是留待确认"也按它（§12.4）。
func DecideOpen(a policy.AgentAllowance, project string) bool {
	if a.Tier != policy.AllowDecide && a.Tier != policy.AllowRules {
		return false
	}
	return len(a.Projects) == 0 || contains(a.Projects, project)
}

// decideChannel：本人经已配对渠道回答。回答人在名单内即是本人，不看档位；名单为空等于这个渠道不能回答。
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

// RejectionEvent 构造应记账的拒绝事件（§6.7）：硬线（20）、档位未放开（23）、钥匙范围外（4，限范围判定）、
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
	payload, _ := json.Marshal(map[string]any{"action": r.Action, "level": r.Level, "tier": r.Tier.String(), "code": d.Err.Code, "reason": d.Err.Reason, "exit": d.ExitCode(), "why": d.Err.Why, "agent": r.Agent})
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
