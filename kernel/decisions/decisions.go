// Package decisions 是决策模块（设计 §7.2、§14.3）：决策请求、回答、接受或丢弃建议。
// 事项投影归事项中心，本包经 items.Revise 修改、经命令模型落事件，不直写其表。
// 授权由命令管线统一判定：提问是一档；回答与处理建议是二档（Agent 的档位放开才行）。问题永不超时代答。
// 只认第一次回答：后到的回答返回退出码 6。
//
// 本包不决定何时提醒、不处理材料修订（revision.confirm / reject）、不处理渠道转达。
package decisions

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 决定入口（Resolution.ResolvedVia，§6.3）：web 是本人在 Web；agent 是档位放开的 Agent 替本人答；渠道回答记渠道名。
// chat（凭本人原话代答）、timeout（超时默认）、grant（授权单）是旧模型的取值，不再产生，只为读旧记录保留。
const (
	ViaWeb     = "web"
	ViaAgent   = "agent"
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
	Batch  string `json:"batch,omitempty"` // 批量处理时由 inbound.Batch 填（pb_ 加 12 位十六进制），记在事件载荷里
}

var batchID = regexp.MustCompile(`^pb_[0-9a-f]{12}$`)

type ref struct {
	ID string `json:"id"`
}

// Register 在命令执行器上登记 decision.request、decision.answer、proposal.accept、proposal.dismiss。
//
// 档位（§14.3）：提交决策请求是一档；回答、接受、丢弃是二档，AGENTS.md 漂移问题的回答是 3a（改以后同步写进仓库的内容）。
// observer 得到 4（范围外）。
func Register(x *command.Executor) error {
	for _, sp := range []command.Spec{request(), answer(), proposal("proposal.accept", domain.PhaseOpen, "proposal accepted"),
		proposal("proposal.dismiss", domain.PhaseDropped, "proposal dismissed")} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

// object 解析授权对象：全部字段来自账本，不来自输入。
func object(tx *store.Tx, id string) (authz.Object, domain.Item, error) {
	if blank(id) {
		return authz.Object{}, domain.Item{}, invalid("item id is required")
	}
	it, err := items.Get(tx, id)
	if err != nil {
		return authz.Object{}, it, err
	}
	return authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Project: it.Project}, it, nil
}

// viaOf：Web 上的本人记 web，其余（档位放开的 Agent、内核）记 agent：Agent 替本人答的每一条都可见、可按次撤回（§14.3）。
func viaOf(c *command.Ctx) string {
	if c.Caller.Level == authz.LevelWeb {
		return ViaWeb
	}
	return ViaAgent
}

func reasonOr(r, def string) string {
	if blank(r) {
		return def
	}
	return r
}

// ---- decision.request ----

func request() command.Spec {
	parse := func(in json.RawMessage) (requestIn, error) {
		var v requestIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "decision.request", Action: authz.ActionUpdateOwn, Tier: authz.TierOrganize,
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
			return checkAsk(v.Ask, it)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			if (v.Ask.Code != "" || v.Ask.Params != nil) && c.Caller.Actor.Type != domain.ActorSystem && c.Caller.Level != authz.LevelWeb {
				return nil, invalid("ask.code and ask.params are written by the kernel only")
			}
			_, it, snap, err := items.Revise(c.Tx, v.ID, "decision.request", c.Now(), func(it *domain.Item) error {
				it.Ask = &domain.Ask{AskInput: v.Ask}
				it.Phase, it.WaitingOn = domain.PhaseNeedsInput, nil
				return nil
			})
			if err != nil {
				return nil, err
			}
			if err := cancelTimer(c.Tx, it.ID); err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: domain.EvDecisionRequested, Object: it.ID, Reason: reasonOr(v.Reason, "decision requested"), Payload: snap})
			return ref{it.ID}, err
		}}
}

// checkAsk 校验决策请求自身：问题、选项、推荐与下一阶段。问题只有本人或档位放开的 Agent 能答，没有超时默认。
func checkAsk(a domain.AskInput, it domain.Item) error {
	if e := domain.CheckAsk(a); e != nil { // 问题的最小语义：至少两个真实选项或允许写别的想法（§6.14 第 20 条）
		return e
	}
	ids := map[string]bool{}
	for _, o := range a.Options {
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
		return invalid("default_on_timeout is no longer accepted: a question waits for an answer and never resolves itself")
	}
	return nil
}

// ---- decision.answer ----

func answer() command.Spec {
	parse := func(in json.RawMessage) (answerIn, error) {
		var v answerIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "decision.answer", Action: authz.ActionAnswerDecision, Tier: authz.TierDecide, Page: "/decisions",
		TierFor: func(tx *store.Tx, _ command.Caller, o authz.Object, _ json.RawMessage) (authz.Tier, error) {
			it, err := items.Get(tx, o.ID)
			if err == nil && it.Ask != nil && it.Ask.Code == domain.AskAgentsMDDrift {
				return authz.TierRules, nil // 这个回答改的是以后同步写进仓库的 AGENTS.md（§14.3）
			}
			return authz.TierDecide, err
		},
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

// Answer 在命令事务里落下决定：写结果、移出 needs_input、记 decision.resolved。
// via 是决定入口（Resolution.ResolvedVia），by 是回答人。已有结果或事项不在等待决定返回退出码 6，只认第一次。
// 授权由调用方的命令管线负责；渠道回答（channel.answer）经它落账，决策的写入仍只有本模块。
func Answer(c *command.Ctx, id, choice, text, via, by, reason string) error {
	res := domain.Resolution{Choice: choice, Text: text, ResolvedVia: via, AnsweredBy: by, ResolvedAt: c.Now()}
	it, err := settle(c.Tx, answerIn{ID: id, Choice: choice, Text: text}, res)
	if err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: domain.EvDecisionResolved, Object: it.id, Reason: reasonOr(reason, "decision answered"),
		Payload: it.snap, ResolvedVia: res.ResolvedVia})
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

// settle 写入决定结果、移出 needs_input，并清掉旧模型可能留下的超时 Timer。
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
	return command.Spec{Name: name, Action: authz.ActionAnswerDecision, Tier: authz.TierDecide, Page: "/proposals",
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
			if v.Batch != "" && !batchID.MatchString(v.Batch) {
				return invalid("batch must look like pb_ followed by 12 hex digits")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			_, it, snap, err := items.Revise(c.Tx, v.ID, name, c.Now(), func(it *domain.Item) error {
				it.Phase = to
				return nil
			})
			if err == nil && v.Batch != "" {
				snap, err = items.TagBatch(snap, v.Batch)
			}
			if err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: domain.EvItemPhaseChanged, Object: it.ID, Reason: reasonOr(v.Reason, def),
				Payload: snap, ResolvedVia: viaOf(c)})
			return ref{it.ID}, err
		}}
}

// ---- 依据不足不静默（§14.5） ----
//
// 退出码 23 的代理写入不生效、阶段不变，不产出问题：拒绝本身已在同一事务记为 command.rejected，
// 事项详情、事项列表、决策待办流与建议流读它标明"谁的什么操作被拒、事项未被修改"（api/items.LastRejection）。
// 早先的版本把 open、waiting 的事项转 needs_input 并挂一个只有"继续"的内核问题；那不是决定，见 WithdrawProxyRejectionAsks。

// ProxyRejectionActor 是早先版本为被拒的代理写入代写问题的操作者；只用来认出这些升级前留下的问题。
const ProxyRejectionActor = "system:proxy-rejection"

// withdrawActor 是撤回这些问题的内核身份。
var withdrawActor = domain.Actor{Type: domain.ActorSystem, Principal: "system:ask-withdraw", Via: domain.ViaClock}

// WithdrawProxyRejectionAsks 把升级前为被拒的代理写入挂上的单选项问题降级为通知（§16.5）：仍未回答、且最近一次
// decision.requested 由 ProxyRejectionActor 写下的事项，回到挂问题之前的阶段（open，或带原 waiting_on 的 waiting），
// 去掉问题，记 item.phase_changed。它不替本人选任何选项（不写 resolution）；拒绝记录照旧，事项详情仍能看到。
// 幂等：撤回过的事项已不在 needs_input。返回撤回的事项 id。
func WithdrawProxyRejectionAsks(tx *store.Tx, now time.Time) ([]string, error) {
	its, err := items.List(tx, items.Filter{Phase: domain.PhaseNeedsInput})
	if err != nil {
		return nil, err
	}
	var done []string
	for _, it := range its {
		if it.Ask == nil || it.Ask.Resolution != nil {
			continue
		}
		es, err := eventlog.ReadObject(tx, it.ID)
		if err != nil {
			return nil, err
		}
		var req *domain.Event
		for i := len(es) - 1; i >= 0; i-- {
			if es[i].Event.Type == domain.EvDecisionRequested {
				req = &es[i].Event
				break
			}
		}
		if req == nil || req.Actor.Principal != ProxyRejectionActor {
			continue
		}
		var snap struct {
			Before *domain.Item `json:"before"`
		}
		if json.Unmarshal(req.Payload, &snap) != nil || snap.Before == nil {
			continue
		}
		prev := snap.Before
		depth, err := items.ChainDepth(tx, it.ID)
		if err != nil {
			return nil, err
		}
		_, _, body, err := items.Revise(tx, it.ID, "transition", now, func(x *domain.Item) error {
			x.Ask, x.Phase, x.WaitingOn = nil, domain.PhaseOpen, nil
			if prev.Phase == domain.PhaseWaiting && prev.WaitingOn != nil {
				x.Phase, x.WaitingOn = domain.PhaseWaiting, prev.WaitingOn
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := cancelTimer(tx, it.ID); err != nil {
			return nil, err
		}
		if _, err := command.Bump(tx, it.ID); err != nil {
			return nil, err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemPhaseChanged, Object: it.ID, Actor: withdrawActor, ChainDepth: depth,
			Reason: "withdrew a kernel question that was not a decision: a proxy write was rejected and nothing was changed", Payload: body, At: now}); err != nil {
			return nil, err
		}
		done = append(done, it.ID)
	}
	return done, nil
}

// ClearLegacyAskState 是权限模型切换的一步（§16.5）：未答问题上旧模型留下的初筛标记、风险类别与超时默认一律清掉，
// 超时 Timer 撤掉，问题照旧等本人。每个改动记一条 item.updated（操作者 authz-switch）。返回改过的事项 id。
func ClearLegacyAskState(tx *store.Tx, actor domain.Actor, now time.Time) ([]string, error) {
	ids, err := LegacyAskItems(tx)
	if err != nil {
		return nil, err
	}
	done := []string{}
	for _, id := range ids {
		it, err := items.Get(tx, id)
		if err != nil {
			return nil, err
		}
		depth, err := items.ChainDepth(tx, it.ID)
		if err != nil {
			return nil, err
		}
		_, _, body, err := items.Revise(tx, it.ID, "authz-switch", now, func(x *domain.Item) error {
			x.Ask.DefaultOnTimeout, x.Ask.RiskClass, x.Ask.ScreeningPending = "", "", false
			return nil
		})
		if err != nil {
			return nil, err
		}
		if err := cancelTimer(tx, it.ID); err != nil {
			return nil, err
		}
		if _, err := command.Bump(tx, it.ID); err != nil {
			return nil, err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvItemUpdated, Object: it.ID, Actor: actor, ChainDepth: depth,
			Reason: "permission model switch: legacy screening and timeout default cleared", Payload: body, At: now}); err != nil {
			return nil, err
		}
		done = append(done, it.ID)
	}
	return done, nil
}

// LegacyAskItems 是还带旧模型状态（初筛标记、风险类别、超时默认）的未答问题。
func LegacyAskItems(tx *store.Tx) ([]string, error) {
	its, err := items.List(tx, items.Filter{Phase: domain.PhaseNeedsInput})
	if err != nil {
		return nil, err
	}
	var out []string
	for _, it := range its {
		if a := it.Ask; a != nil && a.Resolution == nil && (a.DefaultOnTimeout != "" || a.RiskClass != "" || a.ScreeningPending) {
			out = append(out, it.ID)
		}
	}
	return out, nil
}

// ---- 旧模型的超时 Timer ----

// TimerID 是旧模型决策超时 Timer 的 id（"ask_"+事项）。超时默认已取消（2026-11-20，§14.3）：不再新建这种 Timer。
func TimerID(item string) string { return "ask_" + item }

// Owns 判断时钟交来的命令是否是旧模型留下的决策超时 Timer。组装处据它在处理器间分派。
func Owns(c clock.Command) bool { return c.Kind == domain.TimerDue && c.TimerID == TimerID(c.Target) }

func cancelTimer(tx *store.Tx, item string) error {
	err := clock.Cancel(tx, TimerID(item))
	var de *domain.Error
	if err != nil && !(errors.As(err, &de) && de.Code == domain.CodeNotFound) {
		return err
	}
	return nil
}

// HandleTimer 处理旧模型留下的决策超时 Timer：什么也不决定，返回 nil 让时钟清掉它，问题继续等回答。
func HandleTimer(_ *store.Tx, c clock.Command) error {
	if !Owns(c) {
		return invalid("timer " + c.TimerID + " is not a decision timeout")
	}
	return nil
}
