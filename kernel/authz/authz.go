// Package authz 是统一授权判定（设计 §7.7、§14.3、§14.4）：输入 Actor、动作、对象，
// 输出允许、拒绝（带退出码与原因）或需要 Web。纯函数，无 I/O，不写任何数据。
// web_only 的判断型初筛不在这里（那是初筛模块的事）；初筛的结果经 Object 传入。
package authz

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
)

// Level 是调用者的信任级（§14.2）。LevelWeb 只授予 Web 上的本人。
type Level string

const (
	LevelObserver Level = "observer"
	LevelWorker   Level = "worker"
	LevelDelegate Level = "delegate"
	LevelRun      Level = "run" // 运行凭据：worker 级，绑定一次运行，没有代答能力
	LevelWeb      Level = "web"
)

// Action 对应 §14.3 的一行（或一个校验点）。
type Action string

const (
	ActionRead            Action = "read"
	ActionCreateProposed  Action = "create_proposed"
	ActionUpdateOwn       Action = "update_own" // 更新自己的事项、回执、建子事项与 Timer、提交决策请求
	ActionReviseMaterial  Action = "revise_material"
	ActionDelegate        Action = "delegate"
	ActionEditCheckout    Action = "edit_checkout"
	ActionSubmitPlugin    Action = "submit_plugin"
	ActionEditAnyItem     Action = "edit_any_item"
	ActionAnswerDecision  Action = "answer_decision" // 含接受或丢弃建议
	ActionManageProject   Action = "manage_project"  // 建项目、改项目信息、增删 repos 远程地址
	ActionManageSource    Action = "manage_source"   // 来源、webhook 密钥、能力包实例等
	ActionChangeSettings  Action = "change_settings" // 策略、信任级、token、白名单、允许的执行者等
	ActionChannelAnswer   Action = "channel_answer"  // 渠道里的回答，凭渠道回答人白名单
	ActionReassign        Action = "reassign"
	ActionMoveIntoProject Action = "move_into_project"
	ActionRuleCreateChild Action = "rule_create_child"
	ActionRunnerTask      Action = "runner_task"
)

// Tier 是 delegate 凭据代理写入的档位（§14.5）。第三档（高风险与设置类）由动作本身判定，不在这里。
type Tier int

const (
	TierNone   Tier = iota // 不是代理写命令
	TierWork               // 一：agent 自己的工作，凭身份和对象绑定放行
	TierDecide             // 二：替本人做决定，要本人原话加会话引用
	TierWords              // 二中只接受本人原话的管理命令（项目、来源、能力包实例）
)

// Object 是动作的对象。全部字段来自账本或内核，没有任何字段可由事项创建者声明来降低门槛。
type Object struct {
	ID       string
	Assignee string // 事项当前的执行者
	// 运行凭据的范围由内核据账本解析：对象是否为目标事项或其子事项 / 是否属于目标事项所属项目。
	InRunScope   bool
	InRunProject bool

	// web_only 匹配（§14.4）：kind、tag、项目由账本给出；Category 由内核归类；
	// ScreenedWebOnly 是判断型初筛的单调升级结果；ScreeningPending 表示初筛未返回。
	Kind             string
	Tags             []string
	Project          string
	Category         string
	ScreenedWebOnly  bool
	ScreeningPending bool

	// 执行者校验点（§14.7）。ProjectInfo 为 nil 表示无项目，不校验。
	ProjectInfo *domain.Project
	Executor    string

	// ToProject 非 nil 表示命令要把对象移入该项目（空串为移出）：授权单要求来源和目标都在范围内（§14.5）。
	ToProject *string

	// Creator 仅供调用方留痕；判定从不读取它（§14.4 判定不来自事项创建者）。
	Creator domain.Actor
}

// Request 是一次判定的全部输入。
type Request struct {
	Actor    domain.Actor
	Level    Level
	Action   Action
	Object   Object
	Evidence string // 第二档的本人原话；会话引用取 Actor.Session
	Tier     Tier   // 命令管线据命令登记与对象绑定定出的代理写档位（§14.5）；零值不是代理写
	Owner    string // 本人的执行者 id，指派给本人始终允许
	WebURL   string // 对象在 Web 的链接，随 20/22 拒绝返回
	Channel  string // ActionChannelAnswer：渠道
	Sender   string // ActionChannelAnswer：回答人

	// 授权单依据（§14.5 第二档的另一条路）：管线据账本填。GrantID 是请求携带的授权单 id；
	// Grant 为 nil 表示账本里没有这张单。Command 是授权单 actions 里的命令名，Credential 是调用者的凭据 id。
	Command    string
	GrantID    string
	Grant      *policy.Grant
	Credential string
	Now        time.Time
}

type Verdict string

const (
	Allow   Verdict = "allow"
	Deny    Verdict = "deny"
	NeedWeb Verdict = "need_web"
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

func deny(code domain.Code, why, next string) Decision {
	return Decision{Verdict: Deny, Err: domain.NewError(code, why, next)}
}

func denied() Decision {
	return deny(domain.CodePermissionDenied, "credential level does not allow this action", "do not retry; turn the item to needs_input if a decision is needed")
}

func withLink(code domain.Code, why, next, webURL string) *domain.Error {
	e := domain.NewError(code, why, next)
	if webURL != "" {
		e.WebURL = &webURL
	}
	return e
}

// Decide 对 r 给出判定。p 是当前生效的策略版本。
func Decide(p policy.Policy, r Request) Decision {
	if !validLevel(r) {
		return denied()
	}
	if r.Action == ActionChannelAnswer {
		return decideChannel(p, r)
	}
	if r.GrantID != "" {
		switch r.Level {
		case LevelDelegate:
			if strings.TrimSpace(r.Evidence) != "" {
				return deny(domain.CodeInvalidRequest, "give either the owner's words or a grant, not both", "resend with only one basis")
			}
		case LevelWeb: // 本人在 Web 不需要授权单，忽略
		default:
			return denied() // 授权单只对 delegate 凭据生效；运行凭据（含唤醒租约期间的 token）没有代答能力
		}
	}
	var d Decision
	switch r.Action {
	case ActionRead, ActionCreateProposed:
		d = allow
	case ActionUpdateOwn:
		d = byLevel(r, own(r), true, r.Object.InRunScope)
	case ActionReviseMaterial, ActionSubmitPlugin:
		d = byLevel(r, true, true, r.Action == ActionReviseMaterial)
	case ActionEditCheckout:
		d = byLevel(r, true, true, r.Object.InRunProject)
	case ActionDelegate:
		d = byLevel(r, own(r), true, r.Object.InRunScope)
	case ActionEditAnyItem:
		d = byLevel(r, false, true, false)
	case ActionAnswerDecision:
		d = answerDecision(p, r)
	case ActionManageProject, ActionManageSource:
		d = byLevel(r, false, true, false)
	case ActionChangeSettings:
		d = byLevel(r, false, false, false)
		if r.Level == LevelDelegate {
			d = needWeb(r, "this change can only be made by the owner in the Web UI")
		}
	case ActionReassign, ActionMoveIntoProject, ActionRuleCreateChild, ActionRunnerTask:
		d = allow // 这些只是 §14.7 的校验点，权限本身由发起它们的命令判定
	default:
		return deny(domain.CodeInvalidRequest, "unknown action "+string(r.Action), "see apa guide")
	}
	if d.Verdict == Allow && r.Level == LevelDelegate && TierOf(r) >= TierDecide {
		d = needBasis(p, r)
	}
	if d.Verdict != Allow {
		return d
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

// byLevel 把一行矩阵落成判定（observer 在这些行恒不可）：worker/delegate 为是否可；run 为运行凭据的范围条件。Web 本人始终可。
func byLevel(r Request, worker, delegate, run bool) Decision {
	ok := false
	switch r.Level {
	case LevelWeb:
		ok = true
	case LevelWorker:
		ok = worker
	case LevelDelegate:
		ok = delegate
	case LevelRun:
		ok = run
	}
	if !ok {
		return denied()
	}
	return allow
}

// TierOf 是本次判定实际适用的档位：代答与改他人事项的动作本身属于第二档，不论命令登记了什么，
// 所以绑定的执行者借 item.transition 把 proposed、needs_input 事项推出去也要本人原话。
func TierOf(r Request) Tier {
	if r.Action == ActionAnswerDecision || r.Action == ActionEditAnyItem {
		return max(r.Tier, TierDecide)
	}
	return r.Tier
}

// needBasis 校验第二档的依据：本人原话与会话引用都非空（内核不能校验真伪，§21.2）。
func needBasis(p policy.Policy, r Request) Decision {
	if r.GrantID != "" {
		if why := grantMismatch(p, r); why != "" {
			return Decision{Verdict: Deny, Err: withLink(domain.CodeBasisRequired, why, "do not retry; give the Web link to the owner so they can decide or issue a grant", r.WebURL)}
		}
		return allow // 授权单是本人事先划定的，不需要会话引用
	}
	switch {
	case strings.TrimSpace(r.Evidence) == "":
		return basisMissing(r, "this action decides for the owner and needs the owner's own words as evidence")
	// 会话引用是 harness 里的 agent 才有的；规则与内核服务（system）的依据是自己带的证据与规则登记。
	case r.Actor.Type == domain.ActorAgent && strings.TrimSpace(r.Actor.Session) == "":
		return basisMissing(r, "the owner's own words need a session reference (APA_SESSION)")
	}
	return allow
}

// grantMismatch 按 §14.5 的顺序逐项校验授权单，返回第一个不符的原因；空串表示放行。
func grantMismatch(p policy.Policy, r Request) string {
	g, o := r.Grant, r.Object
	switch {
	case g == nil:
		return "grant " + r.GrantID + " does not exist"
	case g.Grantee.Credential != r.Credential:
		return "grant was issued to another credential"
	case g.Grantee.Harness != "" && g.Grantee.Harness != r.Actor.Harness:
		return "grant is limited to another harness"
	case !g.RevokedAt.IsZero():
		return "grant was revoked"
	case !r.Now.Before(g.ExpiresAt):
		return "grant has expired"
	case !contains(g.Actions, r.Command):
		return "grant does not cover " + r.Command
	case !inScope(g.Scope, o.Kind, o.Project, o.ID) || (o.ToProject != nil && !inScope(g.Scope, o.Kind, *o.ToProject, o.ID)):
		return "object is outside the grant scope"
	case g.RiskCeiling == policy.RiskNormal && (WebOnly(p, o) || o.ScreeningPending):
		return "object risk exceeds the grant's risk ceiling"
	}
	return ""
}

// inScope：scope 的非空项都命中才算在范围内。授权单不继承到子事项，所以只比对象自己的字段。
func inScope(s policy.Scope, kind, project, id string) bool {
	return (len(s.Projects) == 0 || contains(s.Projects, project)) &&
		(len(s.Kinds) == 0 || contains(s.Kinds, kind)) &&
		(len(s.Items) == 0 || contains(s.Items, id))
}

func basisMissing(r Request, why string) Decision {
	return Decision{Verdict: Deny, Err: withLink(domain.CodeBasisRequired, why, "do not retry with other wording; give the Web link to the owner and wait", r.WebURL)}
}

func needWeb(r Request, why string) Decision {
	return Decision{Verdict: NeedWeb, Err: withLink(domain.CodeWebOnly, why, "give the Web link to the owner and stop; do not continue", r.WebURL)}
}

func answerDecision(p policy.Policy, r Request) Decision {
	if r.Level == LevelWeb {
		return allow
	}
	if r.Level != LevelDelegate {
		return denied() // observer/worker/运行凭据：没有代答能力（运行凭据为退出码 4）
	}
	if WebOnly(p, r.Object) {
		return needWeb(r, "this decision is web_only and can only be answered by the owner in the Web UI")
	}
	if r.Object.ScreeningPending {
		return Decision{Verdict: Deny, Err: withLink(domain.CodeRiskScreeningPending, "web_only screening has not returned; delegate answers are not accepted yet", "do not treat the answer as in effect; retry later or hand the Web link to the owner", r.WebURL)}
	}
	return allow
}

func decideChannel(p policy.Policy, r Request) Decision {
	if r.Sender == "" || !contains(p.ChannelAnswerers[r.Channel], r.Sender) {
		return deny(domain.CodePermissionDenied, "sender is not an allowed answerer for this channel", "ask the owner to answer in the Web UI")
	}
	if WebOnly(p, r.Object) {
		return needWeb(r, "this decision is web_only and cannot be answered from a channel")
	}
	if r.Object.ScreeningPending {
		return Decision{Verdict: Deny, Err: withLink(domain.CodeRiskScreeningPending, "web_only screening has not returned; channel answers are not accepted yet", "retry later or use the Web link", r.WebURL)}
	}
	return allow
}

// checkExecutor 是 §14.7 的硬约束，对所有级别（含 Web）生效。
func checkExecutor(r Request) Decision {
	switch r.Action {
	case ActionDelegate, ActionReassign, ActionMoveIntoProject, ActionRuleCreateChild, ActionRunnerTask:
	default:
		return allow
	}
	pr := r.Object.ProjectInfo
	if pr == nil || pr.ExecutorAllowed(r.Object.Executor, r.Owner) {
		return allow
	}
	e := domain.NewError(domain.CodeExecutorNotAllowed, "executor is not allowed in this project", "pick an executor from the allowed list or turn the item to needs_input")
	e.Allowed = append([]string{}, pr.AllowedExecutors...)
	return Decision{Verdict: Deny, Err: e}
}

// WebOnly 按策略判断决策是否属于 web_only（§14.4）：规则按 kind、tag、项目匹配（非空字段须全部满足），
// 或对象类别在策略的 web_only 类别内，或初筛已升级。只升不降。
func WebOnly(p policy.Policy, o Object) bool {
	if o.ScreenedWebOnly || o.Category == domain.RiskClassWebOnly || (o.Category != "" && contains(p.WebOnly.Categories, o.Category)) {
		return true
	}
	for _, rule := range p.WebOnly.Rules {
		if (rule.Kind == "" || rule.Kind == o.Kind) &&
			(rule.Tag == "" || contains(o.Tags, rule.Tag)) &&
			(rule.Project == "" || rule.Project == o.Project) {
			return true
		}
	}
	return false
}

// ForwardAllowed 是转发白名单（§14.6）：发送者缺失或未核实，按不在白名单处理。
func ForwardAllowed(p policy.Policy, source, sender string, verified bool) bool {
	return sender != "" && verified && contains(p.ForwardAllowlist[source], sender)
}

// RejectionEvent 构造应记账的拒绝事件（§6.7）：退出码 20、21、22、23，以及运行身份被拒绝的决策命令、回答人不在白名单的渠道回答。
// 判定本身无副作用，由调用方在自己的事务里追加。
func RejectionEvent(r Request, d Decision) (domain.Event, bool) {
	if d.Err == nil {
		return domain.Event{}, false
	}
	switch d.ExitCode() {
	case domain.ExitWebOnly, domain.ExitExecutorNotAllowed, domain.ExitScreeningPending, domain.ExitBasisRequired:
	case domain.ExitPermissionDenied:
		if (r.Level != LevelRun || r.Action != ActionAnswerDecision) && r.Action != ActionChannelAnswer && r.GrantID == "" {
			return domain.Event{}, false
		}
	default:
		return domain.Event{}, false
	}
	payload, _ := json.Marshal(map[string]any{"action": r.Action, "level": r.Level, "code": d.Err.Code, "exit": d.ExitCode(), "why": d.Err.Why, "grant": r.GrantID})
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
