package items

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func decode(in json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(in))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

// put 把 JSON 值写进字段；null 清空为零值。
func put[T any](raw json.RawMessage, dst *T) error {
	var v T
	if string(bytes.TrimSpace(raw)) != "null" {
		if err := decode(raw, &v); err != nil {
			return err
		}
	}
	*dst = v
	return nil
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

type createIn struct {
	domain.ItemInput
	Phase    domain.Phase `json:"phase"` // 缺省 proposed：agent 的产出先进建议流
	Reason   string       `json:"reason"`
	Evidence string       `json:"evidence"`
}

type transitionIn struct {
	ID        string            `json:"id"`
	To        domain.Phase      `json:"to"`
	Reason    string            `json:"reason"`
	Evidence  string            `json:"evidence"`
	WaitingOn *domain.WaitingOn `json:"waiting_on"`
}

// relateIn 的边读作"From <type> To"；parent 的 From 是子、To 是父（与 derived_from 同向）。
type relateIn struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Type   string `json:"type"`
	Remove bool   `json:"remove"`
	Reason string `json:"reason"`
}

type revertIn struct {
	ID     string `json:"id"`
	Seq    int64  `json:"event"`
	Reason string `json:"reason"`
}

type ref struct {
	ID string `json:"id"`
}

func (c createIn) phase() domain.Phase {
	if c.Phase == "" {
		return domain.PhaseProposed
	}
	return c.Phase
}

func (c createIn) item(id string) domain.Item {
	in := c.ItemInput
	it := domain.Item{ID: id, Phase: c.phase(), Kind: in.Kind, Title: in.Title, Summary: in.Summary, Body: in.Body,
		StatusLabel: in.StatusLabel, Priority: in.Priority, Project: in.Project, Tags: in.Tags, Assignee: in.Assignee,
		WaitingOn: in.WaitingOn, Deadline: in.Deadline, PlannedFor: in.PlannedFor, Origin: in.Origin, Spec: in.Spec, Ext: in.Ext}
	if it.Priority == "" {
		it.Priority = domain.PriorityNormal
	}
	if in.Ask != nil {
		it.Ask = &domain.Ask{AskInput: *in.Ask}
	}
	return it
}

// parsePatch 拆出 update 输入：id、reason 之外的键都是要写的字段。
func parsePatch(in json.RawMessage) (id, reason string, patch map[string]json.RawMessage, err error) {
	if err = decode(in, &patch); err != nil {
		return
	}
	for key, dst := range map[string]*string{"id": &id, "reason": &reason} {
		if raw, ok := patch[key]; ok {
			if err = put(raw, dst); err != nil {
				return
			}
			delete(patch, key)
		}
	}
	if id == "" {
		err = invalid("item id is required")
	}
	return
}

// applyPatch 在 cur 的副本上写入给定字段；phase、kind、id 与内核字段不在可写集合内。
func applyPatch(cur domain.Item, patch map[string]json.RawMessage) (domain.Item, error) {
	if len(patch) == 0 {
		return cur, invalid("nothing to update")
	}
	it := cur
	for k, raw := range patch {
		var err error
		switch k {
		case "title":
			err = put(raw, &it.Title)
		case "summary":
			err = put(raw, &it.Summary)
		case "body":
			err = put(raw, &it.Body)
		case "status_label":
			err = put(raw, &it.StatusLabel)
		case "priority":
			err = put(raw, &it.Priority)
		case "project":
			err = put(raw, &it.Project)
		case "tags":
			err = put(raw, &it.Tags)
		case "assignee":
			err = put(raw, &it.Assignee)
		case "waiting_on":
			err = put(raw, &it.WaitingOn)
		case "deadline":
			err = put(raw, &it.Deadline)
		case "planned_for":
			err = put(raw, &it.PlannedFor)
		case "origin":
			err = put(raw, &it.Origin)
		case "spec":
			err = put(raw, &it.Spec)
		case "ext":
			err = put(raw, &it.Ext)
		case "ask":
			var a *domain.AskInput
			if err = put(raw, &a); err == nil {
				it.Ask = nil
				if a != nil { // 内核维护的风险类别、初筛标记与决定结果不因调用方改写问题而丢
					it.Ask = &domain.Ask{AskInput: *a}
					if cur.Ask != nil {
						it.Ask.AskKernelState, it.Ask.Resolution = cur.Ask.AskKernelState, cur.Ask.Resolution
					}
				}
			}
		default:
			err = invalid("field " + k + " cannot be updated")
		}
		if err != nil {
			return cur, err
		}
	}
	return it, nil
}

func transit(cur domain.Item, in transitionIn) (domain.Item, error) {
	next := cur
	next.Phase = in.To
	switch {
	case in.To != domain.PhaseWaiting && in.WaitingOn != nil:
		return cur, invalid("waiting_on only applies to the waiting phase")
	case in.To != domain.PhaseWaiting:
		next.WaitingOn = nil
	case in.WaitingOn != nil:
		next.WaitingOn = in.WaitingOn
	}
	return next, nil
}

type svc struct{ kinds *Kinds }

// check 校验事项自身：字段、项目存在、waiting_on / ask 的形状、阶段不变量（§6.14 第 3 条）、kind 的 spec schema。
func (s *svc) check(tx *store.Tx, it domain.Item) error {
	switch {
	case blank(string(it.Kind)) || strings.ContainsAny(string(it.Kind), " \t\r\n"):
		return invalid("kind is required and must not contain whitespace")
	case blank(it.Title):
		return invalid("title is required")
	case it.Priority != "" && !it.Priority.Valid():
		return invalid("unknown priority " + string(it.Priority))
	case slices.ContainsFunc(it.Tags, blank):
		return invalid("tags must not be blank")
	case it.Origin != nil && blank(it.Origin.Source):
		return invalid("origin.source is required")
	case it.Ask != nil && blank(it.Ask.Question):
		return invalid("ask.question is required")
	case it.Deadline != nil && it.Deadline.IsZero():
		return invalid("deadline must not be the zero time")
	case it.PlannedFor != "":
		if err := checkDate("planned_for", it.PlannedFor); err != nil {
			return err
		}
	case it.Phase == domain.PhaseWaiting && it.WaitingOn == nil:
		return invalid("waiting phase requires waiting_on")
	case it.Phase == domain.PhaseNeedsInput && it.Ask == nil:
		return invalid("needs_input phase requires ask")
	}
	if w := it.WaitingOn; w != nil {
		if !w.Kind.Valid() || blank(w.Ref) {
			return invalid("waiting_on needs a kind (human, executor, external, time) and a ref")
		}
		if w.Kind == domain.WaitingTime {
			if _, err := time.Parse(time.RFC3339, w.Ref); err != nil {
				return invalid("waiting_on.ref for kind time must be an RFC 3339 timestamp")
			}
		}
	}
	if it.Project != "" {
		if _, err := projectOf(tx, it.Project); err != nil {
			return err
		}
	}
	return s.kinds.check(it.Kind, it.Spec)
}

// projectOf 读项目；引用不存在的项目是请求错误，不是 not_found。
func projectOf(tx *store.Tx, id string) (domain.Project, error) {
	p, err := projects.Get(tx, id)
	if isNotFound(err) {
		return p, invalid("project " + id + " does not exist")
	}
	return p, err
}

// object 解析授权对象。Assignee 是当前执行者（判定"自己的事项"）；Executor 与 ProjectInfo 是写入后的
// 执行者与所属项目，交给 §14.7 的名单校验（两者都有才校验）。
func object(tx *store.Tx, cur, next domain.Item) (authz.Object, error) {
	o := authz.Object{ID: cur.ID, Assignee: cur.Assignee, Kind: string(cur.Kind), Tags: cur.Tags, Project: cur.Project, Executor: next.Assignee}
	if a := cur.Ask; a != nil { // 内核维护的风险类别与初筛状态进入代答判定（§14.4）
		o.Category, o.ScreeningPending = a.RiskClass, a.ScreeningPending
	}
	if next.Project != cur.Project {
		o.ToProject = &next.Project
	}
	if next.Assignee != "" && next.Project != "" {
		p, err := projectOf(tx, next.Project)
		if err != nil {
			return o, err
		}
		o.ProjectInfo = &p
	}
	return o, nil
}

// BoundTo 是 §14.5 第一档的对象绑定，只据账本：事项的 assignee 等于该凭据，或这个委派子事项由该凭据发起
// （delegate.create 事件的操作者）。输入里的任何声明都不算。
func BoundTo(tx *store.Tx, principal string, o authz.Object) (bool, error) {
	if principal == "" {
		return false, nil
	}
	if o.Assignee == principal {
		return true, nil
	}
	es, err := eventlog.ReadObject(tx, o.ID)
	if err != nil {
		return false, err
	}
	for _, e := range es {
		if e.Event.Type == domain.EvDelegationCreated {
			return e.Event.Actor.Principal == principal, nil
		}
	}
	return false, nil
}

// bound 供命令登记 Spec.Bound 用。
func bound(tx *store.Tx, c command.Caller, o authz.Object, _ json.RawMessage) (bool, error) {
	return BoundTo(tx, c.Actor.Principal, o)
}

func boundOr(tx *store.Tx, c command.Caller, o authz.Object) bool {
	ok, _ := BoundTo(tx, c.Actor.Principal, o) // 查不到按未绑定，落到更严的第二档
	return ok
}

// editActions：改自己的事项（运行凭据：范围内的事项）为 update_own，改他人的为 edit_any_item（§14.3）；
// 写了 assignee 或 project 再加名单校验点，使 21 号拒绝走管线留痕。
func editActions(c command.Caller, o authz.Object, own, wroteAssignee, wroteProject bool, first ...authz.Action) []authz.Action {
	as := first
	if len(as) == 0 {
		if own || (c.Level == authz.LevelRun && o.InRunScope) {
			as = []authz.Action{authz.ActionUpdateOwn}
		} else {
			as = []authz.Action{authz.ActionEditAnyItem}
		}
	}
	switch {
	case wroteAssignee:
		as = append(as, authz.ActionReassign)
	case wroteProject:
		as = append(as, authz.ActionMoveIntoProject)
	}
	return as
}

// marshalPayload 序列化事件 payload；版本与时间不进快照，折叠时按事件补上。
func marshalPayload(p payload) (json.RawMessage, error) {
	p.Item = bare(p.Item)
	if p.Before != nil {
		b := bare(*p.Before)
		p.Before = &b
	}
	return json.Marshal(p)
}

func emit(c *command.Ctx, typ, reason, evidence string, p payload) error {
	b, err := marshalPayload(p)
	if err != nil {
		return err
	}
	if blank(evidence) {
		evidence = c.Evidence // 第二档：请求头带的本人原话也要记进事件
	}
	_, err = c.Emit(domain.Event{Type: typ, Object: p.Item.ID, Reason: reason, Evidence: evidence, Payload: b})
	return err
}

func waitTimerID(item string) string { return "wait_" + item }

// dueTimerID 是事项截止 Timer 的 id；决策超时 Timer 另有自己的 id（decisions.TimerID），两者并存互不覆盖。
func dueTimerID(item string) string { return "due_" + item }

// dueAt 是事项此刻应有的截止 Timer 时刻：已接受且未结束才计时（§6.8），其余阶段保留 deadline 字段但不计时。
func dueAt(it domain.Item) *time.Time {
	switch it.Phase {
	case domain.PhaseOpen, domain.PhaseWaiting, domain.PhaseNeedsInput:
		return it.Deadline
	}
	return nil
}

// syncDueTimer 只在应有时刻变化时重排：已到期的一次性 Timer 不因无关改动重新触发。
func syncDueTimer(tx *store.Tx, cur, next domain.Item) error {
	was, at := dueAt(cur), dueAt(next)
	if was == at || (was != nil && at != nil && was.Equal(*at)) {
		return nil
	}
	id := dueTimerID(next.ID)
	if err := clock.Cancel(tx, id); err != nil && !isNotFound(err) {
		return err
	}
	if at == nil {
		return nil
	}
	_, err := clock.Add(tx, domain.Timer{ID: id, Target: next.ID, Kind: domain.TimerDue, At: *at})
	return err
}

// syncTimer 让事项的 Timer 与从 cur 到 next 的变更一致（新建时 cur 为零值）：wait_until 只属于等时间的
// waiting，改时间即重排（§6.4、§6.8）；截止 Timer 见 syncDueTimer。
func syncTimer(tx *store.Tx, cur, it domain.Item) error {
	if err := syncDueTimer(tx, cur, it); err != nil {
		return err
	}
	id := waitTimerID(it.ID)
	if err := clock.Cancel(tx, id); err != nil && !isNotFound(err) {
		return err
	}
	if it.Phase != domain.PhaseWaiting || it.WaitingOn == nil || it.WaitingOn.Kind != domain.WaitingTime {
		return nil
	}
	at, err := time.Parse(time.RFC3339, it.WaitingOn.Ref)
	if err != nil {
		return err
	}
	_, err = clock.Add(tx, domain.Timer{ID: id, Target: it.ID, Kind: domain.TimerWaitUntil, At: at})
	return err
}

// Register 在命令执行器上登记事项命令。kinds 为 nil 表示没有登记任何 kind schema。
//
// 授权（§14.3）：以 proposed 新建所有级别可；其余写入改自己的事项为 update_own、改他人的为 edit_any_item；
// 离开 proposed（接受或丢弃建议）与离开 needs_input（决定）按代答（answer_decision）判定，使 worker、
// 运行身份不能自己确认；item.revert 只在 Web（change_settings）。assignee 与 project 的写入都过名单校验。
func Register(x *command.Executor, kinds *Kinds) error {
	s := &svc{kinds: kinds}
	for _, sp := range []command.Spec{s.create(), s.update(), s.transition(), s.relate(), s.revert(), s.progress()} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

func (s *svc) create() command.Spec {
	parse := func(in json.RawMessage) (createIn, error) {
		var v createIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "item.create", Action: authz.ActionCreateProposed, Tier: authz.TierDecide,
		Bound: func(_ *store.Tx, _ command.Caller, _ authz.Object, in json.RawMessage) (bool, error) {
			v, err := parse(in)
			return v.phase() == domain.PhaseProposed, err
		},
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			id, err := newID() // 新事项的 id 先定下来，授权与拒绝事件才有对象
			if err != nil {
				return authz.Object{}, err
			}
			it := v.item(id)
			return object(tx, it, it)
		},
		Actions: func(_ *store.Tx, c command.Caller, o authz.Object, in json.RawMessage) []authz.Action {
			v, _ := parse(in)
			own := o.Assignee != "" && o.Assignee == c.Actor.Principal
			if v.phase() == domain.PhaseProposed {
				return editActions(c, o, own, v.Assignee != "", v.Project != "", authz.ActionCreateProposed)
			}
			return editActions(c, o, own, v.Assignee != "", v.Project != "")
		},
		Validate: func(tx *store.Tx, o authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			it := v.item(o.ID)
			reason := v.Reason
			if reason == "" {
				reason = "created"
			}
			if e := domain.CheckEntry(it, it.Phase, reason, !blank(v.Evidence)); e != nil {
				return e
			}
			return s.check(tx, it)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			it := stamped(v.item(c.Object.ID), c.Now())
			it.ChainDepth = c.ChainDepth
			if err := save(c.Tx, it); err != nil {
				return nil, err
			}
			if err := syncTimer(c.Tx, domain.Item{}, it); err != nil {
				return nil, err
			}
			reason := v.Reason
			if reason == "" {
				reason = "created"
			}
			return ref{it.ID}, emit(c, domain.EvItemCreated, reason, v.Evidence, payload{Op: "create", Item: it})
		}}
}

func (s *svc) update() command.Spec {
	plan := func(tx *store.Tx, in json.RawMessage) (cur, next domain.Item, reason string, patch map[string]json.RawMessage, err error) {
		var id string
		if id, reason, patch, err = parsePatch(in); err != nil {
			return
		}
		if cur, err = Get(tx, id); err != nil {
			return
		}
		next, err = applyPatch(cur, patch)
		return
	}
	return command.Spec{Name: "item.update", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Bound: bound,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			cur, next, _, _, err := plan(tx, in)
			if err != nil {
				return authz.Object{}, err
			}
			return object(tx, cur, next)
		},
		Actions: func(tx *store.Tx, c command.Caller, o authz.Object, in json.RawMessage) []authz.Action {
			_, _, _, patch, _ := plan(tx, in)
			_, a := patch["assignee"]
			_, p := patch["project"]
			return editActions(c, o, boundOr(tx, c, o), a, p)
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			cur, next, _, _, err := plan(tx, in)
			if err != nil {
				return err
			}
			if next.Project != cur.Project && cur.Kind == domain.KindDelegation {
				// 委派子事项的 project 继承父事项，移走即可绕过允许的执行者名单（§7.5）。
				if _, has, err := ParentOf(tx, cur.ID); err != nil {
					return err
				} else if has {
					return invalid("a delegated item inherits its parent's project and cannot be moved")
				}
			}
			return s.check(tx, next)
		},
		Apply: func(c *command.Ctx) (any, error) {
			cur, next, reason, _, err := plan(c.Tx, c.Input)
			if err != nil {
				return nil, err
			}
			next = stamped(next, c.Now())
			if err := save(c.Tx, next); err != nil {
				return nil, err
			}
			if err := syncTimer(c.Tx, cur, next); err != nil {
				return nil, err
			}
			return ref{next.ID}, emit(c, domain.EvItemUpdated, reason, c.Evidence, payload{Op: "update", Before: &cur, Item: next})
		}}
}

func (s *svc) transition() command.Spec {
	plan := func(tx *store.Tx, in json.RawMessage) (cur, next domain.Item, v transitionIn, err error) {
		if err = decode(in, &v); err != nil {
			return
		}
		if cur, err = Get(tx, v.ID); err != nil {
			return
		}
		next, err = transit(cur, v)
		return
	}
	return command.Spec{Name: "item.transition", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Bound: bound,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			cur, next, _, err := plan(tx, in)
			if err != nil {
				return authz.Object{}, err
			}
			return object(tx, cur, next)
		},
		Actions: func(tx *store.Tx, c command.Caller, o authz.Object, in json.RawMessage) []authz.Action {
			cur, _, v, _ := plan(tx, in)
			as := editActions(c, o, boundOr(tx, c, o), false, false)
			if (cur.Phase == domain.PhaseProposed || cur.Phase == domain.PhaseNeedsInput) && v.To != cur.Phase {
				as = append(as, authz.ActionAnswerDecision)
			}
			return as
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			cur, next, v, err := plan(tx, in)
			if err != nil {
				return err
			}
			if cur.Phase == v.To {
				return invalid("item is already in phase " + string(v.To))
			}
			if e := domain.CheckEntry(next, v.To, v.Reason, !blank(v.Evidence)); e != nil {
				return e
			}
			return s.check(tx, next)
		},
		Apply: func(c *command.Ctx) (any, error) {
			cur, next, v, err := plan(c.Tx, c.Input)
			if err != nil {
				return nil, err
			}
			next = stamped(next, c.Now())
			if err := save(c.Tx, next); err != nil {
				return nil, err
			}
			if err := syncTimer(c.Tx, cur, next); err != nil {
				return nil, err
			}
			return ref{next.ID}, emit(c, domain.EvItemPhaseChanged, v.Reason, v.Evidence, payload{Op: "transition", Before: &cur, Item: next})
		}}
}

func (s *svc) relate() command.Spec {
	parse := func(in json.RawMessage) (relateIn, error) {
		var v relateIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "item.relate", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Bound: bound,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			cur, err := Get(tx, v.From)
			if err != nil {
				return authz.Object{}, err
			}
			return object(tx, cur, cur)
		},
		Actions: func(tx *store.Tx, c command.Caller, o authz.Object, _ json.RawMessage) []authz.Action {
			return editActions(c, o, boundOr(tx, c, o), false, false)
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			rel := domain.Relation{Type: v.Type, From: v.From, To: v.To}
			switch {
			case blank(v.Type) || strings.ContainsAny(v.Type, " \t\r\n"):
				return invalid("relation type is required and must not contain whitespace")
			case v.From == v.To:
				return invalid("an item cannot be related to itself")
			}
			if _, err := Get(tx, v.To); err != nil {
				// 事项与材料同样用类型化关系（§6.3）：终点可以是材料，但材料不能当父事项。
				if !isNotFound(err) {
					return err
				}
				if _, merr := materials.Get(tx, v.To); merr != nil {
					return domain.NewError(domain.CodeNotFound, v.To+" is neither an item nor a material", "list items or materials and retry")
				}
				if v.Type == domain.RelParent {
					return invalid("a material cannot be a parent; use basis, output or minutes")
				}
			}
			has, err := hasRelation(tx, rel)
			if err != nil {
				return err
			}
			switch {
			case v.Remove && !has:
				return domain.NewError(domain.CodeNotFound, "relation does not exist", "list the item's relations")
			case !v.Remove && has:
				return conflict("relation already exists", "nothing to do")
			case !v.Remove && v.Type == domain.RelParent:
				return noParentCycle(tx, v.From, v.To)
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			rel := domain.Relation{Type: v.Type, From: v.From, To: v.To}
			cur, err := Get(c.Tx, v.From)
			if err != nil {
				return nil, err
			}
			if err := setRelation(c.Tx, rel, v.Remove); err != nil {
				return nil, err
			}
			cur = stamped(cur, c.Now()) // 关系变更也加版本、也是一条事项事件
			if err := save(c.Tx, cur); err != nil {
				return nil, err
			}
			return ref{cur.ID}, emit(c, domain.EvItemUpdated, v.Reason, c.Evidence, payload{Op: "relate", Item: cur, Relation: &rel, Remove: v.Remove})
		}}
}

// noParentCycle：把 child 挂到 parent 下，不能让 child 成为 parent 的祖先。
func noParentCycle(tx *store.Tx, child, parent string) error {
	for seen, cur := map[string]bool{}, parent; cur != ""; {
		if cur == child {
			return invalid("parent relation would create a cycle")
		}
		if seen[cur] {
			return nil
		}
		seen[cur] = true
		up, err := allRelations(tx, "WHERE type = ? AND from_id = ? ", domain.RelParent, cur)
		if err != nil {
			return err
		}
		cur = ""
		if len(up) > 0 {
			cur = up[0].To
		}
	}
	return nil
}

// revertPlan 是一次撤销要做的事：恢复 next，或把一条关系变更取反。
type revertPlan struct {
	cur, next domain.Item
	rel       *domain.Relation
	remove    bool
}

// revertible 是可撤销的事件类型；decision.resolved 的快照带变更前的事项，恢复它即恢复未回答的 ask（§6.4）。
// 外部副作用（已发出的消息等）不在账本里，撤销不碰。
var revertible = []string{domain.EvItemUpdated, domain.EvItemPhaseChanged, domain.EvDecisionResolved}

// rearmAskTimeout：撤销把已回答的决定恢复成未回答时，补回回答时被撤掉的超时 Timer，
// 否则带 default_on_timeout 的请求永不自动决定。id 与 kind 必须同 decisions.TimerID（"ask_"+事项）；
// 不能 import decisions（它依赖本包），Timer 到期仍由 decisions 处理。
func rearmAskTimeout(tx *store.Tx, it domain.Item) error {
	a := it.Ask
	if it.Phase != domain.PhaseNeedsInput || a == nil || a.Resolution != nil || a.DefaultOnTimeout == "" || a.Deadline == nil {
		return nil
	}
	id := "ask_" + it.ID
	if err := clock.Cancel(tx, id); err != nil && !isNotFound(err) {
		return err
	}
	_, err := clock.Add(tx, domain.Timer{ID: id, Target: it.ID, Kind: domain.TimerDue, At: *a.Deadline})
	return err
}

// planRevert 找出要撤销的事件。只撤销 agent 做的、且是该事项最新的一次事项变更：
// 反向命令恢复的是变更前的快照，后面还有别的改动时直接恢复会把它们一并抹掉。
func planRevert(tx *store.Tx, in revertIn) (revertPlan, error) {
	var rp revertPlan
	cur, err := Get(tx, in.ID)
	if err != nil {
		return rp, err
	}
	es, err := eventlog.ReadObject(tx, in.ID)
	if err != nil {
		return rp, err
	}
	var target, last *eventlog.Entry
	for i := range es {
		if es[i].Event.Seq == in.Seq {
			target = &es[i]
		}
		if slices.Contains(itemEvents, es[i].Event.Type) {
			last = &es[i]
		}
	}
	switch {
	case target == nil:
		return rp, domain.NewError(domain.CodeNotFound, "event does not belong to this item", "read the item's timeline")
	case target.Tombstone || !slices.Contains(revertible, target.Event.Type):
		return rp, invalid("only item updates, phase changes and decisions can be reverted")
	case target.Event.Actor.Type != domain.ActorAgent:
		return rp, invalid("only changes made by an agent can be reverted")
	case last == nil || last.Event.Seq != target.Event.Seq:
		return rp, conflict("later changes exist on this item", "revert the later changes first")
	}
	var p payload
	if err := json.Unmarshal(target.Event.Payload, &p); err != nil {
		return rp, invalid("event payload cannot be reverted")
	}
	rp.cur, rp.next = cur, cur
	switch {
	case p.Op == "relate" && p.Relation != nil:
		rp.rel, rp.remove = p.Relation, !p.Remove
	case (p.Op == "update" || p.Op == "transition" || p.Op == "decision.answer") && p.Before != nil:
		rp.next = *p.Before
		rp.next.ItemKernelState = cur.ItemKernelState // 内核字段不回退
	default:
		return rp, invalid("event payload cannot be reverted")
	}
	return rp, nil
}

func (s *svc) revert() command.Spec {
	parse := func(in json.RawMessage) (revertIn, error) {
		var v revertIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "item.revert", Action: authz.ActionChangeSettings,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			rp, err := planRevert(tx, v)
			if err != nil {
				return authz.Object{}, err
			}
			return object(tx, rp.cur, rp.next)
		},
		Actions: func(tx *store.Tx, _ command.Caller, _ authz.Object, in json.RawMessage) []authz.Action {
			as := []authz.Action{authz.ActionChangeSettings}
			v, _ := parse(in)
			if rp, err := planRevert(tx, v); err == nil {
				switch {
				case rp.next.Assignee != rp.cur.Assignee:
					as = append(as, authz.ActionReassign)
				case rp.next.Project != rp.cur.Project:
					as = append(as, authz.ActionMoveIntoProject)
				}
			}
			return as
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			rp, err := planRevert(tx, v)
			if err != nil {
				return err
			}
			return s.check(tx, rp.next)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			rp, err := planRevert(c.Tx, v)
			if err != nil {
				return nil, err
			}
			rp.next = stamped(rp.next, c.Now())
			if err = save(c.Tx, rp.next); err == nil {
				if rp.rel != nil {
					err = setRelation(c.Tx, *rp.rel, rp.remove)
				} else {
					err = syncTimer(c.Tx, rp.cur, rp.next)
				}
				if err == nil {
					err = rearmAskTimeout(c.Tx, rp.next)
				}
			}
			if err != nil {
				return nil, err
			}
			typ := domain.EvItemUpdated
			if rp.next.Phase != rp.cur.Phase {
				typ = domain.EvItemPhaseChanged
			}
			reason := v.Reason
			if reason == "" {
				reason = "revert of event " + strconv.FormatInt(v.Seq, 10)
			}
			return ref{rp.next.ID}, emit(c, typ, reason, c.Evidence, payload{Op: "revert", Before: &rp.cur, Item: rp.next, Relation: rp.rel, Remove: rp.remove, Reverts: v.Seq})
		}}
}
