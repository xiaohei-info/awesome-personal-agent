// Package decisions 是决策模块（设计 §7.2、§14.4、§14.5）：决策请求、回答、接受或丢弃建议，
// 以及到期按默认值决定。事项投影归事项中心，本包经 items.Revise 修改、经命令模型落事件，不直写其表。
// 授权由命令管线统一判定：web_only 拒绝（退出码 20）、初筛未返回（22）、运行身份（4）以及代答证据，
// 被拒绝的各类都由管线记 command.rejected。只认第一次回答：后到的回答返回退出码 6。
//
// 本包不做判断型初筛、不决定何时提醒、不处理材料修订（revision.confirm / reject）、不处理渠道转达。
package decisions

import (
	"bytes"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 决定入口（Resolution.ResolvedVia，§6.3）。
const (
	ViaWeb     = "web"
	ViaChat    = "chat"
	ViaTimeout = "timeout"
	ViaGrant   = domain.ResolvedViaGrant
)

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func conflict(why, next string) error { return domain.NewError(domain.CodeConflict, why, next) }

func decode(in json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

type requestIn struct {
	ID     string          `json:"id"`
	Ask    domain.AskInput `json:"ask"`
	Reason string          `json:"reason"`
}

type answerIn struct {
	ID     string `json:"id"`
	Choice string `json:"choice"`
	Text   string `json:"text"`
	Reason string `json:"reason"`
}

type proposalIn struct {
	ID     string `json:"id"`
	Reason string `json:"reason"`
}

type ref struct {
	ID string `json:"id"`
}

// Register 在命令执行器上登记 decision.request、decision.answer、proposal.accept、proposal.dismiss。
//
// 授权（§14.3）：提交决策请求按"更新自己的事项"；回答、接受、丢弃按代答：Web 本人可，delegate 必须附本人原话，
// web_only 得到 20、初筛未返回得到 22，观察者、worker 与运行凭据得到 4。
func Register(x *command.Executor) error { return RegisterWith(x, nil) }

// Screening 是 web_only 初筛模块在 decision.request 事务内的两个接入点（§14.4）。
// 判断与模型调用都不在这里：Needed 只据账本回答"这条请求要不要初筛"，Begin 在同一事务写下判断型 Run 的起点。
type Screening interface {
	Needed(tx *store.Tx, it domain.Item) (bool, error)
	Begin(c *command.Ctx, it domain.Item) error
}

// RegisterWith 同 Register；s 非空时，决策请求按 s.Needed 在同一事务置 screening_pending 并调 s.Begin。
func RegisterWith(x *command.Executor, s Screening) error {
	x.OnReject(onReject)
	for _, sp := range []command.Spec{request(s), answer(), proposal("proposal.accept", domain.PhaseOpen, "proposal accepted"),
		proposal("proposal.dismiss", domain.PhaseDropped, "proposal dismissed")} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

// object 解析授权对象：全部字段来自账本（含内核维护的风险类别与初筛状态），不来自输入。
func object(tx *store.Tx, id string) (authz.Object, domain.Item, error) {
	if blank(id) {
		return authz.Object{}, domain.Item{}, invalid("item id is required")
	}
	it, err := items.Get(tx, id)
	if err != nil {
		return authz.Object{}, it, err
	}
	o := authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Tags: it.Tags, Project: it.Project}
	if a := it.Ask; a != nil {
		o.Category, o.ScreeningPending = a.RiskClass, a.ScreeningPending
	}
	return o, it, nil
}

// viaOf：Web 上的本人记 web，依据授权单的记 grant，其余（附证据的 delegate）记 chat（§14.5）。
func viaOf(c *command.Ctx) string {
	if c.Caller.Level == authz.LevelWeb {
		return ViaWeb
	}
	if c.Grant != "" {
		return ViaGrant
	}
	return ViaChat
}

func reasonOr(r, def string) string {
	if blank(r) {
		return def
	}
	return r
}

// ---- decision.request ----

// riskClass 按策略规则定出风险类别（§14.4）：取第一条命中的规则；判定与调用方写的任何字段无关。
func riskClass(p policy.Policy, it domain.Item) string {
	for _, r := range p.WebOnly.Rules {
		if (r.Kind == "" || r.Kind == string(it.Kind)) && (r.Tag == "" || slices.Contains(it.Tags, r.Tag)) &&
			(r.Project == "" || r.Project == it.Project) {
			return r.Category
		}
	}
	return ""
}

func request(scr Screening) command.Spec {
	parse := func(in json.RawMessage) (requestIn, error) {
		var v requestIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "decision.request", Action: authz.ActionUpdateOwn, Tier: authz.TierWork,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			o, _, err := object(tx, v.ID)
			return o, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			_, it, err := object(tx, v.ID)
			if err != nil {
				return err
			}
			switch it.Phase {
			case domain.PhaseOpen, domain.PhaseWaiting:
			case domain.PhaseNeedsInput:
				return conflict("item already has an open decision request", "wait for the answer; one question at a time")
			default:
				return conflict("decision requests can only be made on open or waiting items", "re-read the item")
			}
			if err := checkAsk(v.Ask, it); err != nil {
				return err
			}
			if v.Ask.DefaultOnTimeout != "" {
				_, pol, err := policy.Current(tx)
				if err != nil {
					return err
				}
				cls := riskClass(pol, it)
				if authz.WebOnly(pol, authz.Object{Kind: string(it.Kind), Tags: it.Tags, Project: it.Project, Category: cls}) {
					return invalid("web_only decisions cannot have default_on_timeout")
				}
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			_, pol, err := policy.Current(c.Tx)
			if err != nil {
				return nil, err
			}
			screen := false
			_, it, snap, err := items.Revise(c.Tx, v.ID, "decision.request", c.Now(), func(it *domain.Item) error {
				it.Ask = &domain.Ask{AskInput: v.Ask, AskKernelState: domain.AskKernelState{RiskClass: riskClass(pol, *it)}}
				it.Phase, it.WaitingOn = domain.PhaseNeedsInput, nil
				if scr != nil {
					var err error
					if screen, err = scr.Needed(c.Tx, *it); err != nil {
						return err
					}
					it.Ask.ScreeningPending = screen
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			if err := cancelTimer(c.Tx, it.ID); err != nil {
				return nil, err
			}
			if v.Ask.DefaultOnTimeout != "" {
				if _, err := clock.Add(c.Tx, domain.Timer{ID: TimerID(it.ID), Target: it.ID, Kind: domain.TimerDue, At: *v.Ask.Deadline}); err != nil {
					return nil, err
				}
			}
			if _, err = c.Emit(domain.Event{Type: domain.EvDecisionRequested, Object: it.ID, Reason: reasonOr(v.Reason, "decision requested"), Payload: snap}); err != nil {
				return nil, err
			}
			if screen {
				err = scr.Begin(c, it)
			}
			return ref{it.ID}, err
		}}
}

// checkAsk 校验决策请求自身：问题、选项、推荐、下一阶段与超时默认值。
func checkAsk(a domain.AskInput, it domain.Item) error {
	if blank(a.Question) {
		return invalid("ask.question is required")
	}
	if len(a.Options) == 0 && !a.FreeText {
		return invalid("ask needs options or free_text")
	}
	ids := map[string]bool{}
	for _, o := range a.Options {
		switch {
		case blank(o.ID) || blank(o.Label):
			return invalid("every option needs an id and a label")
		case ids[o.ID]:
			return invalid("duplicate option id " + o.ID)
		}
		ids[o.ID] = true
		switch o.NextPhase() {
		case domain.PhaseOpen, domain.PhaseDropped:
		case domain.PhaseDone:
			if it.Kind == domain.KindDelegation { // 委派进 done 必须带证据（§6.14），决定本身给不出
				return invalid("a delegation cannot be completed by a decision option")
			}
		default:
			return invalid("option next must be open, done or dropped")
		}
	}
	if a.Recommend != "" && !ids[a.Recommend] {
		return invalid("recommend must be an option id")
	}
	if a.DefaultOnTimeout != "" {
		if !ids[a.DefaultOnTimeout] {
			return invalid("default_on_timeout must be an option id")
		}
		if a.Deadline == nil || a.Deadline.IsZero() {
			return invalid("default_on_timeout requires a deadline")
		}
	}
	return nil
}

// ---- decision.answer ----

func answer() command.Spec {
	parse := func(in json.RawMessage) (answerIn, error) {
		var v answerIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "decision.answer", Action: authz.ActionAnswerDecision, Tier: authz.TierDecide,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			o, _, err := object(tx, v.ID)
			return o, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			_, it, err := object(tx, v.ID)
			if err != nil {
				return err
			}
			_, err = resolve(it, v)
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			return ref{v.ID}, Answer(c, v.ID, v.Choice, v.Text, viaOf(c), c.Caller.Actor.Principal, v.Reason)
		}}
}

// Answer 在命令事务里落下决定：写结果、移出 needs_input、撤销超时 Timer、记 decision.resolved。
// via 是决定入口（Resolution.ResolvedVia），by 是回答人。已有结果或事项不在等待决定返回退出码 6，只认第一次。
// 授权由调用方的命令管线负责；渠道回答（channel.answer）经它落账，决策的写入仍只有本模块。
func Answer(c *command.Ctx, id, choice, text, via, by, reason string) error {
	res := domain.Resolution{Choice: choice, Text: text, ResolvedVia: via, AnsweredBy: by, Evidence: c.Evidence, Grant: c.Grant, ResolvedAt: c.Now()}
	it, err := settle(c.Tx, answerIn{ID: id, Choice: choice, Text: text}, res)
	if err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: domain.EvDecisionResolved, Object: it.id, Reason: reasonOr(reason, "decision answered"),
		Payload: it.snap, ResolvedVia: res.ResolvedVia, Evidence: res.Evidence})
	return err
}

// resolve 校验回答并给出决定后的阶段。已有结果或事项不在等待决定：退出码 6，只认第一次回答。
func resolve(it domain.Item, v answerIn) (domain.Phase, error) {
	if it.Phase != domain.PhaseNeedsInput || it.Ask == nil || it.Ask.Resolution != nil {
		return "", conflict("the decision was already resolved or is not waiting for an answer", "read the item's ask.resolution; the first answer stands")
	}
	switch {
	case v.Choice != "" && v.Text != "":
		return "", invalid("give either choice or text, not both")
	case v.Choice == "" && blank(v.Text):
		return "", invalid("choice or text is required")
	case v.Choice != "":
		for _, o := range it.Ask.Options {
			if o.ID == v.Choice {
				return o.NextPhase(), nil
			}
		}
		return "", invalid("choice is not one of the options")
	case !it.Ask.FreeText:
		return "", invalid("this decision does not accept free text")
	}
	return domain.PhaseOpen, nil
}

type settled struct {
	id   string
	snap json.RawMessage
}

// settle 写入决定结果、移出 needs_input，并撤销超时 Timer。
func settle(tx *store.Tx, v answerIn, res domain.Resolution) (settled, error) {
	_, it, snap, err := items.Revise(tx, v.ID, "decision.answer", res.ResolvedAt, func(it *domain.Item) error {
		next, err := resolve(*it, v)
		if err != nil {
			return err
		}
		a := *it.Ask
		a.Resolution = &res
		it.Ask, it.Phase = &a, next
		return nil
	})
	if err != nil {
		return settled{}, err
	}
	return settled{it.ID, snap}, cancelTimer(tx, it.ID)
}

// ---- proposal.accept / proposal.dismiss ----

func proposal(name string, to domain.Phase, def string) command.Spec {
	parse := func(in json.RawMessage) (proposalIn, error) {
		var v proposalIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: name, Action: authz.ActionAnswerDecision, Tier: authz.TierDecide,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			o, _, err := object(tx, v.ID)
			return o, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			_, it, err := object(tx, v.ID)
			if err != nil {
				return err
			}
			if it.Phase != domain.PhaseProposed {
				return conflict("the item is not a pending proposal", "re-read the item; the first answer stands")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			_, it, snap, err := items.Revise(c.Tx, v.ID, name, c.Now(), func(it *domain.Item) error {
				it.Phase = to
				return nil
			})
			if err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: domain.EvItemPhaseChanged, Object: it.ID, Reason: reasonOr(v.Reason, def),
				Payload: snap, ResolvedVia: viaOf(c), Evidence: c.Evidence})
			return ref{it.ID}, err
		}}
}

// ---- 依据不足不静默（§14.5） ----

// rejectActor 是内核代写 ask 的操作者。
var rejectActor = domain.Actor{Type: domain.ActorSystem, Principal: "system:proxy-rejection", Via: domain.ViaClock}

// onReject 在记 command.rejected 的同一事务里处理退出码 23：open、waiting 的事项转 needs_input 并附内核生成的 ask；
// needs_input、proposed、已终结的事项与不对应事项的命令不变。ask 只写操作者、命令名和拒绝原因，不带 agent 自述的内容，
// risk_class 固定 web_only（不初筛，只有本人在 Web 能答），选项只有"继续"，回答后回 open，不重放被拒绝的修改。
func onReject(tx *store.Tx, r command.Rejection) error {
	if r.Exit != domain.ExitBasisRequired || r.Object == "" {
		return nil
	}
	it, err := items.Get(tx, r.Object)
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeNotFound {
		return nil // 对象不是事项（项目、来源、实例）
	}
	if err != nil {
		return err
	}
	if it.Phase != domain.PhaseOpen && it.Phase != domain.PhaseWaiting {
		return nil
	}
	depth, err := items.ChainDepth(tx, it.ID)
	if err != nil {
		return err
	}
	q := "代理 " + r.Actor.Principal + " 执行 " + r.Command + " 被拒绝：" + r.Why + "。同意请在 Web 直接修改，或签发授权单后让代理重试。"
	_, _, snap, err := items.Revise(tx, it.ID, "decision.request", r.At, func(it *domain.Item) error {
		it.Ask = &domain.Ask{AskInput: domain.AskInput{Question: q, Options: []domain.AskOption{{ID: "continue", Label: "继续"}}},
			AskKernelState: domain.AskKernelState{RiskClass: domain.RiskClassWebOnly}}
		it.Phase, it.WaitingOn = domain.PhaseNeedsInput, nil
		return nil
	})
	if err != nil {
		return err
	}
	if err := cancelTimer(tx, it.ID); err != nil {
		return err
	}
	if _, err := command.Bump(tx, it.ID); err != nil {
		return err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: domain.EvDecisionRequested, Object: it.ID, Actor: rejectActor, ChainDepth: depth,
		Reason: "proxy write rejected: basis required", Payload: snap, At: r.At})
	return err
}

// ---- 超时默认值 ----

// TimerID 是事项决策超时 Timer 的 id；一个事项同时只有一个未决请求，所以一个 id 足够。
func TimerID(item string) string { return "ask_" + item }

// Owns 判断时钟交来的命令是否归本模块：到期的决策超时。组装处据它在处理器间分派。
func Owns(c clock.Command) bool { return c.Kind == domain.TimerDue && c.TimerID == TimerID(c.Target) }

func cancelTimer(tx *store.Tx, item string) error {
	err := clock.Cancel(tx, TimerID(item))
	var de *domain.Error
	if err != nil && !(errors.As(err, &de) && de.Code == domain.CodeNotFound) {
		return err
	}
	return nil
}

// HandleTimer 处理决策超时：按默认值决定，resolved_via 为 timeout，actor 为 system（时钟交来的身份）。
// 它在时钟交来的事务里工作，不经 command.Do（Do 自己开写事务会死锁）；版本仍走 command.Bump。
//
// 已被回答、离开 needs_input、没有默认值或事项已不在时，Timer 已过期：返回 nil 让时钟清掉它，事项继续等本人。
// 现在属于 web_only（策略后来变了）同样不自动决定。初筛未返回时返回错误，时钟保留 Timer 下次重试，
// 初筛结束后再判定（§14.4）。
// shortcut: 初筛未返回时靠时钟每次扫描重试，初筛超时默认 30 秒，量大时改为初筛结束后主动唤醒。
func HandleTimer(tx *store.Tx, c clock.Command) error {
	if !Owns(c) {
		return invalid("timer " + c.TimerID + " is not a decision timeout")
	}
	it, err := items.Get(tx, c.Target)
	var de *domain.Error
	if errors.As(err, &de) && de.Code == domain.CodeNotFound {
		return nil
	}
	if err != nil {
		return err
	}
	a := it.Ask
	if it.Phase != domain.PhaseNeedsInput || a == nil || a.Resolution != nil || a.DefaultOnTimeout == "" {
		return nil
	}
	if a.ScreeningPending {
		return conflict("web_only screening has not returned; the timeout default waits for it", "retry after screening ends")
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	if authz.WebOnly(pol, authz.Object{Kind: string(it.Kind), Tags: it.Tags, Project: it.Project, Category: a.RiskClass}) {
		return nil
	}
	v := answerIn{ID: it.ID, Choice: a.DefaultOnTimeout}
	res := domain.Resolution{Choice: v.Choice, ResolvedVia: ViaTimeout, AnsweredBy: c.Actor.Principal, ResolvedAt: c.Due}
	s, err := settle(tx, v, res)
	if err != nil {
		return err
	}
	if _, err := command.Bump(tx, it.ID); err != nil {
		return err
	}
	depth, err := items.ChainDepth(tx, it.ID)
	if err != nil {
		return err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: domain.EvDecisionResolved, Object: it.ID, Actor: c.Actor, Reason: "decision deadline passed; default applied",
		Payload: s.snap, ChainDepth: depth, ResolvedVia: ViaTimeout, At: c.Due})
	return err
}
