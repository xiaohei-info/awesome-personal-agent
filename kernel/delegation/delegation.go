// Package delegation 是任务委派模块（设计 §7.5、§14.7）：记录与跟踪委派，不调用任何执行者。
//
//	delegate.create  建子事项（kind=delegation，assignee 为执行者 id，project 继承父事项），父事项转 waiting
//	delegate.report  回执：进展、done、阻塞，可带证据（commit、PR、分支、链接）和消息确认游标
//
// 项目允许的执行者是硬约束：名单外的 assignee 在命令管线的授权步骤被拒绝（退出码 21，带允许的 id 列表，
// 不写任何子事项，管线记 command.rejected）。assignee 不必先在执行者名录里；请求带 executor 时，
// 同一事务里随委派登记或更新名录条目（§6.17），被拒的请求不登记。
// 回执只接受事项 assignee 绑定的 worker 凭据、delegate 凭据、Web 本人，或范围内的运行凭据（任务型运行对自己的目标事项回执，附录 C.13），
// 其他 worker 凭据是退出码 4（授权行"更新自己的事项"：绑定了执行者的凭据，其操作者就是执行者 id）。
//
// 父事项回 open：全部委派子事项进入终态（done、dropped）才回；子事项 needs_input 时父事项保持 waiting。
// 回执把子事项推到 done 时在同一事务里完成；子事项经 item.transition 等命令被丢弃或完成时，由提交后的
// 订阅者经 delegate.settle 命令补上，失败交给 Options.OnError，不吞错。
//
// 回执里的 PR 登记为关联键（delegation_link），供跟进消息挂靠（kernel-followmsg）按 Lookup 查事项。
// 本包不写别人的表：事项投影经 items.Create / items.Revise，事件经命令管线。
package delegation

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 回执结果。
const (
	OutcomeProgress = "progress"
	OutcomeDone     = "done"
	OutcomeBlocked  = "blocked"
)

// Module 是本模块在存储层登记的迁移：PR 关联键。
var Module = store.Module{Name: "delegation", Migrations: []store.Migration{{
	Version: 1, Name: "create-link",
	SQL: `CREATE TABLE delegation_link (
	ref TEXT PRIMARY KEY,
	item TEXT NOT NULL
) WITHOUT ROWID;`,
}, dispatchMigration, dispatchRunMigration}}

// Options 注入本模块读取但不拥有的东西。
type Options struct {
	// Owner 是本人的执行者 id；指派给本人不受项目名单限制，也不必在执行者清单里（§14.7）。
	Owner string
	// OnError 接收提交后补做父事项状态失败的错误，必填：失败时父事项会停在 waiting，不能静默。
	OnError func(commit command.Commit, err error)
}

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

// Evidence 是回执证据。Link 是第一个链接（旧字段，含义不变）；给了链接时 Links 是全部链接（含第一个）。
// 输入里的 link 可以是字符串或字符串数组（§7.5、附录 B.2）。
type Evidence struct {
	Commit string   `json:"commit,omitempty"`
	PR     string   `json:"pr,omitempty"`
	Branch string   `json:"branch,omitempty"`
	Link   string   `json:"link,omitempty"`
	Links  []string `json:"links,omitempty"`
}

// UnmarshalJSON 接受 link 为字符串或数组；读回已存事件时 links 也照收。
func (e *Evidence) UnmarshalJSON(b []byte) error {
	var raw struct {
		Commit string          `json:"commit"`
		PR     string          `json:"pr"`
		Branch string          `json:"branch"`
		Link   json.RawMessage `json:"link"`
		Links  []string        `json:"links"`
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	links := raw.Links
	if len(links) == 0 && len(raw.Link) > 0 && string(raw.Link) != "null" { // 已存的事件两个键都有，以 links 为准
		var one string
		var many []string
		switch err := json.Unmarshal(raw.Link, &one); {
		case err == nil:
			links = []string{one}
		case json.Unmarshal(raw.Link, &many) == nil:
			links = many
		default:
			return invalid("link must be a string or an array of strings")
		}
	}
	*e = Evidence{Commit: raw.Commit, PR: raw.PR, Branch: raw.Branch}
	for _, l := range links {
		if l = strings.TrimSpace(l); l != "" {
			e.Links = append(e.Links, l)
		}
	}
	if len(e.Links) > 0 {
		e.Link = e.Links[0]
	}
	return nil
}

func (e Evidence) empty() bool {
	return blank(e.Commit) && blank(e.PR) && blank(e.Branch) && blank(e.Link)
}

// Report 是一次回执的内容，随 delegation.reported 事件保存。
type Report struct {
	Outcome  string   `json:"outcome"`
	Summary  string   `json:"summary"`
	Evidence Evidence `json:"evidence"`
	Cursor   string   `json:"cursor,omitempty"` // 消息确认游标：之前的挂靠消息算已处理
}

type createIn struct {
	Parent   string          `json:"parent"`
	Assignee string          `json:"assignee"`
	Title    string          `json:"title"`
	Summary  string          `json:"summary"`
	Body     string          `json:"body"`
	Priority domain.Priority `json:"priority"`
	Project  string          `json:"project"` // 只能缺省或等于父事项的 project
	Reason   string          `json:"reason"`
	// Executor 随委派登记或更新名录条目（id 取 Assignee，harness 取调用方），可缺省。
	Executor *executors.Enrollment `json:"executor,omitempty"`
	// Start 为真时由 APA 按执行者的无头运行声明拉起这次委派（路径 A，§7.5、§13.6）；拉起与它的前置校验在
	// kernel/background 的同事务回调里，不通过则整条命令不生效。Overrides 是这一次覆盖的可覆盖参数（只能取声明列出的值）。
	Start     bool              `json:"start,omitempty"`
	Overrides map[string]string `json:"overrides,omitempty"`
}

// StartInput 是 delegate.create 里交给拉起方的部分：kernel/background 从命令输入读它。
type StartInput struct {
	Start     bool              `json:"start"`
	Overrides map[string]string `json:"overrides"`
}

type reportIn struct {
	ID string `json:"id"`
	// Session 是不带事项 id 的回执：按调用方 harness 记下的会话句柄找到委派记录所在的子事项（§6.17）。
	Session string `json:"session,omitempty"`
	Report
	Reason string `json:"reason"`
}

type settleIn struct {
	ID string `json:"id"` // 子事项
}

type created struct {
	ID     string `json:"id"`
	Parent string `json:"parent"`
}

// settlers 是其终态变化可能让父事项回 open 的命令。
var settlers = map[string]bool{"item.transition": true, "decision.answer": true, "item.revert": true}

// Register 在命令执行器上登记 delegate.create、delegate.report、delegate.settle，并订阅提交后补做父事项状态。
func Register(x *command.Executor, o Options) error {
	if o.OnError == nil {
		return invalid("delegation: Options.OnError is required")
	}
	for _, s := range []command.Spec{o.create(), o.attach(), report(), settle(), o.dispatch(), o.dispatchUpdate(), dispatchList()} {
		if err := x.Register(s); err != nil {
			return err
		}
	}
	x.InTx(closeDispatches)
	x.OnCommit(func(c command.Commit) {
		if !settlers[c.Command] || c.Object == "" {
			return
		}
		in, _ := json.Marshal(settleIn{ID: c.Object})
		_, err := x.Do(context.Background(), command.Request{Identity: system, Command: "delegate.settle", Input: in,
			Chain: command.Chain{Mode: command.ChainKeep, From: c.Object}})
		if err != nil {
			o.OnError(c, err)
		}
	})
	return nil
}

// system 是提交后补做父事项状态时的内部身份。
var system = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "kernel:delegation", Via: domain.ViaClock}, Level: authz.LevelWorker}

// ---- delegate.create ----

func (o Options) create() command.Spec {
	parse := func(in json.RawMessage) (createIn, error) {
		var v createIn
		if err := decode(in, &v); err != nil {
			return v, err
		}
		if (v.Start || len(v.Overrides) > 0) && v.Assignee == o.Owner {
			return v, invalid("start and overrides need an executor; work assigned to the owner is not started by APA")
		}
		if len(v.Overrides) > 0 && !v.Start {
			return v, invalid("overrides only apply when APA starts the work (start)")
		}
		if v.Executor != nil && v.Assignee != o.Owner {
			return v, v.Executor.Check(v.Assignee)
		}
		return v, nil
	}
	return command.Spec{Name: "delegate.create", Action: authz.ActionDelegate, Group: authz.GroupOrganize,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			if blank(v.Parent) || blank(v.Assignee) {
				return authz.Object{}, invalid("parent and assignee are required")
			}
			parent, err := items.Get(tx, v.Parent)
			if err != nil {
				return authz.Object{}, err
			}
			if v.Project != "" && v.Project != parent.Project {
				return authz.Object{}, invalid("a delegated item inherits its parent's project and cannot choose another")
			}
			obj := authz.Object{ID: parent.ID, Assignee: parent.Assignee, Kind: string(parent.Kind),
				Project: parent.Project, Executor: v.Assignee}
			if parent.Project != "" && v.Assignee != o.Owner {
				p, err := projects.Get(tx, parent.Project)
				if err != nil {
					return authz.Object{}, err
				}
				obj.ProjectInfo = &p
				if p.Restricted() { // 名单只认执行者 id（§14.7）
					if obj.Candidates, err = executors.Candidates(tx, p, o.Owner); err != nil {
						return authz.Object{}, err
					}
				}
			}
			return obj, nil
		},
		Validate: func(tx *store.Tx, obj authz.Object, _ json.RawMessage) error {
			parent, err := items.Get(tx, obj.ID)
			if err != nil {
				return err
			}
			if parent.Phase != domain.PhaseOpen && parent.Phase != domain.PhaseWaiting {
				return conflict("only an open or waiting item can be delegated from", "re-read the item")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			return o.createChild(c, v)
		}}
}

// createChild 建委派子事项并让父事项转 waiting（delegate.create 与把未挂的派活挂上共用）。
func (o Options) createChild(c *command.Ctx, v createIn) (created, error) {
	parent, err := items.Get(c.Tx, v.Parent)
	if err != nil {
		return created{}, err
	}
	if v.Executor != nil && v.Assignee != o.Owner {
		if err := executors.Enroll(c, v.Assignee, *v.Executor); err != nil {
			return created{}, err
		}
	}
	title := v.Title
	if blank(title) {
		title = parent.Title
	}
	prio := v.Priority
	if prio == "" {
		prio = domain.PriorityNormal
	}
	child := domain.Item{Phase: domain.PhaseOpen, Kind: domain.KindDelegation, Title: title, Summary: v.Summary, Body: v.Body,
		Priority: prio, Project: parent.Project, Assignee: v.Assignee,
		// 后台运行建立的子事项带 unattended 并沿链路继承；本人显式重新委派开始新链路（§7.5）。
		ItemKernelState: domain.ItemKernelState{ChainDepth: c.ChainDepth,
			Unattended: c.Caller.Level == authz.LevelRun || (parent.Unattended && c.Caller.Level != authz.LevelWeb)}}
	child, snap, err := items.Create(c.Tx, child, parent.ID, c.Now())
	if err != nil {
		return created{}, err
	}
	if _, err := command.Bump(c.Tx, child.ID); err != nil { // 新对象不是管线的授权对象，版本自己加
		return created{}, err
	}
	reason := reasonOr(v.Reason, "delegated to "+v.Assignee)
	if _, err := c.Emit(domain.Event{Type: domain.EvDelegationCreated, Object: child.ID, Reason: reason, Payload: snap}); err != nil {
		return created{}, err
	}
	// 父事项（管线的授权对象，版本由管线加）转 waiting，等这个执行者。
	before, _, psnap, err := items.Revise(c.Tx, parent.ID, "delegate.create", c.Now(), func(it *domain.Item) error {
		it.Phase, it.WaitingOn = domain.PhaseWaiting, &domain.WaitingOn{Kind: domain.WaitingExecutor, Ref: v.Assignee}
		return nil
	})
	if err != nil {
		return created{}, err
	}
	typ := domain.EvItemUpdated
	if before.Phase != domain.PhaseWaiting {
		typ = domain.EvItemPhaseChanged
	}
	_, err = c.Emit(domain.Event{Type: typ, Object: parent.ID, Reason: "waiting on " + v.Assignee, Payload: psnap})
	return created{ID: child.ID, Parent: parent.ID}, err
}

type attachIn struct {
	ID       string          `json:"id"` // 未挂事项的委派记录
	Parent   string          `json:"parent"`
	Title    string          `json:"title"`
	Summary  string          `json:"summary"`
	Body     string          `json:"body"`
	Priority domain.Priority `json:"priority"`
	Reason   string          `json:"reason"`
}

// attach 是 delegate.attach：把还没挂到事项上的派活挂到父事项下——与 delegate.create 同一路径建委派子事项（项目名单、
// 父事项转 waiting），再把这条委派记录挪到子事项上（§6.17）。
func (o Options) attach() command.Spec {
	parse := func(tx *store.Tx, in json.RawMessage) (attachIn, Dispatch, error) {
		var v attachIn
		if err := decode(in, &v); err != nil {
			return v, Dispatch{}, err
		}
		if blank(v.ID) || blank(v.Parent) {
			return v, Dispatch{}, invalid("id (the dispatch) and parent are required")
		}
		d, err := GetDispatch(tx, v.ID)
		if err != nil {
			return v, d, err
		}
		if d.Attached() || d.State != DispatchRunning {
			return v, d, conflict("the dispatch is already attached or no longer tracked", "list the item's dispatches")
		}
		return v, d, nil
	}
	create := o.create()
	asCreate := func(v attachIn, d Dispatch) json.RawMessage {
		b, _ := json.Marshal(createIn{Parent: v.Parent, Assignee: d.Executor, Title: v.Title, Summary: v.Summary, Body: v.Body,
			Priority: v.Priority, Reason: reasonOr(v.Reason, "attached dispatch "+d.ID)})
		return b
	}
	return command.Spec{Name: "delegate.attach", Action: authz.ActionDelegate, Group: authz.GroupOrganize,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, d, err := parse(tx, in)
			if err != nil {
				return authz.Object{}, err
			}
			return create.Object(tx, asCreate(v, d))
		},
		Validate: create.Validate,
		Apply: func(c *command.Ctx) (any, error) {
			v, d, err := parse(c.Tx, c.Input)
			if err != nil {
				return nil, err
			}
			var cv createIn
			_ = json.Unmarshal(asCreate(v, d), &cv)
			out, err := o.createChild(c, cv)
			if err != nil {
				return nil, err
			}
			d.Item = out.ID
			if err := putDispatch(c.Tx, d); err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: EvDispatchUpdated, Object: out.ID, Reason: "dispatch attached", Payload: dispatchPayload(d)})
			return out, err
		}}
}

func reasonOr(r, def string) string {
	if blank(r) {
		return def
	}
	return r
}

// ---- delegate.report ----

func report() command.Spec {
	// parse 读输入；按会话句柄回执时把找到的子事项填进 ID，三个阶段看到的是同一个对象。
	parse := func(tx *store.Tx, in json.RawMessage) (reportIn, error) {
		var v reportIn
		if err := decode(in, &v); err != nil {
			return v, err
		}
		if blank(v.ID) == blank(v.Session) {
			return v, invalid("give the item id or the dispatch session handle, not both")
		}
		if v.ID != "" {
			return v, nil
		}
		d, ok, err := BySession(tx, "", v.Session)
		switch {
		case err != nil:
			return v, err
		case !ok:
			return v, domain.NewError(domain.CodeNotFound, "no dispatch was recorded with this session handle", "report on the item id, or record the dispatch first")
		case !d.Attached():
			return v, conflict("the dispatch "+d.ID+" is not attached to an item yet", "attach it with apa dispatch attach "+d.ID+" --parent <item>, then report")
		}
		v.ID = d.Item
		return v, nil
	}
	return command.Spec{Name: "delegate.report", Action: authz.ActionUpdateOwn, Group: authz.GroupOrganize, Receipt: true,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(tx, in)
			if err != nil {
				return authz.Object{}, err
			}
			child, err := reportable(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			return authz.Object{ID: child.ID, Assignee: child.Assignee, Kind: string(child.Kind), Project: child.Project}, nil
		},
		// 按句柄回执只认调用方 harness 自己写的记录：句柄是那个 harness 的会话 id，别的 harness 不能借用。
		Refine: func(tx *store.Tx, c command.Caller, _ *authz.Object, in json.RawMessage) error {
			var v reportIn
			if err := decode(in, &v); err != nil || v.Session == "" {
				return err
			}
			if _, ok, err := BySession(tx, c.Actor.Harness, v.Session); err != nil || !ok {
				if err == nil {
					err = domain.NewError(domain.CodeNotFound, "no dispatch of your harness was recorded with this session handle", "report on the item id")
				}
				return err
			}
			return nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, err := parse(tx, in)
			if err != nil {
				return err
			}
			child, err := reportable(tx, v.ID)
			if err != nil {
				return err
			}
			return checkReport(tx, child, v)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Tx, c.Input)
			label := v.Summary
			if v.Outcome == OutcomeBlocked {
				label = "blocked: " + v.Summary
			}
			_, child, snap, err := items.Revise(c.Tx, v.ID, "delegate.report", c.Now(), func(it *domain.Item) error {
				it.StatusLabel = label
				if v.Outcome == OutcomeDone {
					it.Phase, it.WaitingOn = domain.PhaseDone, nil
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
			if snap, err = withReport(snap, v.Report); err != nil {
				return nil, err
			}
			if pr := strings.TrimSpace(v.Evidence.PR); pr != "" {
				if _, err := c.Tx.Exec("INSERT OR IGNORE INTO delegation_link (ref, item) VALUES (?, ?)", pr, child.ID); err != nil {
					return nil, err
				}
			}
			ev, _ := json.Marshal(v.Evidence)
			// 只有委派子事项的回执是 delegation.reported（委派核验等规则按它触发）；运行目标事项走通用事项事件。
			typ := domain.EvDelegationReported
			if child.Kind != domain.KindDelegation {
				typ = domain.EvItemUpdated
				if v.Outcome == OutcomeDone {
					typ = domain.EvItemPhaseChanged
				}
			}
			if _, err := c.Emit(domain.Event{Type: typ, Object: child.ID, Reason: reasonOr(v.Reason, "reported "+v.Outcome),
				Evidence: string(ev), Payload: snap}); err != nil {
				return nil, err
			}
			if child.Kind == domain.KindDelegation {
				if err := parentProgress(c, child, label, v.Evidence); err != nil {
					return nil, err
				}
			}
			if v.Outcome == OutcomeDone && child.Kind == domain.KindDelegation {
				if err := registerVaultLinks(c, child, v.Evidence.Links); err != nil {
					return nil, err
				}
				if parent, ok, err := reopenable(c.Tx, child.ID); err != nil {
					return nil, err
				} else if ok {
					if _, err := command.Bump(c.Tx, parent); err != nil { // 父事项不是本命令的授权对象
						return nil, err
					}
					if err := reopen(c, parent); err != nil {
						return nil, err
					}
				}
			}
			return map[string]string{"id": child.ID}, nil
		}}
}

// parentProgress 把委派子事项的回执同时写成父事项的一条进展（§7.5）：本人看事项看的是父事项，回报不能只留在子事项里。
// 父事项已结束时不写。
func parentProgress(c *command.Ctx, child domain.Item, label string, ev Evidence) error {
	parent, ok, err := items.ParentOf(c.Tx, child.ID)
	if err != nil || !ok {
		return err
	}
	p, err := items.Get(c.Tx, parent)
	if err != nil || p.Phase.Terminal() {
		return err
	}
	text := child.Title + "：" + label
	for _, e := range []string{ev.PR, ev.Commit, ev.Branch, ev.Link} {
		if e = strings.TrimSpace(e); e != "" {
			text += " " + e
		}
	}
	w, err := items.AppendProgress(c.Tx, parent, text, c.Caller.Actor.Principal, c.Now())
	if err != nil {
		return err
	}
	if _, err := command.Bump(c.Tx, parent); err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, Reason: "reported by " + child.Assignee})
	return err
}

// reportable 读出回执的目标事项：委派子事项，或任务型运行的目标事项（附录 C.13 的"写回事项"）。
// 谁能写回由授权决定：绑定的执行者凭据改自己的事项，运行凭据改范围内的事项；这里不再限定 kind。
func reportable(tx *store.Tx, id string) (domain.Item, error) {
	if blank(id) {
		return domain.Item{}, invalid("item id is required")
	}
	return items.Get(tx, id)
}

func checkReport(tx *store.Tx, child domain.Item, v reportIn) error {
	switch v.Outcome {
	case OutcomeProgress, OutcomeDone, OutcomeBlocked:
	default:
		return invalid("outcome must be progress, done or blocked")
	}
	if blank(v.Summary) {
		return invalid("summary is required")
	}
	switch child.Phase {
	case domain.PhaseOpen, domain.PhaseWaiting:
	case domain.PhaseNeedsInput:
		if v.Outcome == OutcomeDone {
			return conflict("the item is waiting for the owner's decision and cannot be completed", "wait for the answer")
		}
	default:
		return conflict("the item is already "+string(child.Phase)+"; reports are only accepted on live items", "re-read the item")
	}
	if v.Outcome == OutcomeDone {
		if e := domain.CheckEntry(child, domain.PhaseDone, "reported done", !v.Evidence.empty()); e != nil {
			return e
		}
		if v.Evidence.empty() { // 任何 kind 报 done 都要结果证据（§6.14 第 3 条）；CheckEntry 只管 delegation 的进入约束
			return domain.NewError(domain.CodeInvalidRequest, "reporting done needs result evidence (pr, commit, branch or link)", "report progress or blocked instead, or attach the result")
		}
	}
	if pr := strings.TrimSpace(v.Evidence.PR); pr != "" {
		if owner, ok, err := Lookup(tx, pr); err != nil {
			return err
		} else if ok && owner != child.ID {
			return conflict("this PR is already registered for another item", "report the PR on the item that owns it")
		}
	}
	return nil
}

// withReport 把回执内容并入事项事件的 payload；折叠只读事项快照，多出的键不影响重建。
func withReport(snap json.RawMessage, r Report) (json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(snap, &m); err != nil {
		return nil, err
	}
	b, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	m["report"] = b
	return json.Marshal(m)
}

// ---- 父事项回 open ----

// reopenable：child 的父事项正等执行者，且全部委派子事项都已终态。
func reopenable(tx *store.Tx, child string) (parent string, ok bool, err error) {
	parent, has, err := items.ParentOf(tx, child)
	if err != nil || !has {
		return "", false, err
	}
	p, err := items.Get(tx, parent)
	if err != nil {
		return "", false, err
	}
	if p.Phase != domain.PhaseWaiting || p.WaitingOn == nil || p.WaitingOn.Kind != domain.WaitingExecutor {
		return "", false, nil
	}
	kids, err := items.Children(tx, parent)
	if err != nil {
		return "", false, err
	}
	for _, k := range kids {
		if k.Kind == domain.KindDelegation && k.Phase != domain.PhaseDone && k.Phase != domain.PhaseDropped {
			return "", false, nil
		}
	}
	return parent, true, nil
}

func reopen(c *command.Ctx, parent string) error {
	_, _, snap, err := items.Revise(c.Tx, parent, "delegate.settle", c.Now(), func(it *domain.Item) error {
		it.Phase, it.WaitingOn = domain.PhaseOpen, nil
		return nil
	})
	if err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: domain.EvItemPhaseChanged, Object: parent, Reason: "all delegated items finished", Payload: snap})
	return err
}

// settleNoop 是无事可做时的占位对象：被拒绝的命令要留痕，留痕需要对象；它只占一行版本，不是事项。
const settleNoop = "delegation:settle"

// settle 让父事项与子事项终态一致：对象是要回 open 的父事项（版本由管线加），无事可做时是占位对象。
// 它只做"全部终态则回 open"这一件由账本决定的事，所以幂等，也无须额外权限；
// 只有内部身份（系统、时钟入口）按只读判定，其余调用者要改设置（Web 本人）。
func settle() command.Spec {
	parse := func(in json.RawMessage) (settleIn, error) {
		var v settleIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "delegate.settle", Action: authz.ActionChangeSettings, Group: authz.GroupInternal,
		Actions: func(_ *store.Tx, c command.Caller, _ authz.Object, _ json.RawMessage) []authz.Action {
			if c.Actor.Type == domain.ActorSystem && c.Actor.Via == domain.ViaClock {
				return []authz.Action{authz.ActionRead}
			}
			return []authz.Action{authz.ActionChangeSettings}
		},
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			// 订阅者对每个事项命令都会来问：不是委派子事项（或已不存在）就是无事可做，不是错误。
			if it, err := items.Get(tx, v.ID); err != nil {
				if de, ok := err.(*domain.Error); ok && de.Code == domain.CodeNotFound {
					return authz.Object{ID: settleNoop}, nil
				}
				return authz.Object{}, err
			} else if it.Kind != domain.KindDelegation {
				return authz.Object{ID: settleNoop}, nil
			}
			parent, ok, err := reopenable(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			if !ok {
				return authz.Object{ID: settleNoop}, nil
			}
			return authz.Object{ID: parent}, nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			if c.Object.ID == settleNoop {
				return map[string]bool{"reopened": false}, nil
			}
			return map[string]bool{"reopened": true}, reopen(c, c.Object.ID)
		}}
}

// ---- 读 ----

// Lookup 按 PR 关联键查登记它的委派子事项；跟进消息据此挂靠。
func Lookup(tx *store.Tx, pr string) (item string, ok bool, err error) {
	r, err := tx.Query("SELECT item FROM delegation_link WHERE ref = ?", strings.TrimSpace(pr))
	if err != nil {
		return "", false, err
	}
	defer r.Close()
	if !r.Next() {
		return "", false, r.Err()
	}
	err = r.Scan(&item)
	return item, err == nil, err
}

// Entry 是一次已记录的回执。
type Entry struct {
	Seq   int64
	At    time.Time
	Actor domain.Actor
	Report
}

// Reports 按时间顺序返回子事项的全部回执（进展、阻塞原因、证据、游标）。
func Reports(tx *store.Tx, id string) ([]Entry, error) {
	es, err := eventlog.ReadObject(tx, id)
	if err != nil {
		return nil, err
	}
	var out []Entry
	for _, e := range es {
		if e.Tombstone || e.Event.Type != domain.EvDelegationReported {
			continue
		}
		var p struct {
			Report Report `json:"report"`
		}
		if err := json.Unmarshal(e.Event.Payload, &p); err != nil {
			return nil, err
		}
		out = append(out, Entry{Seq: e.Event.Seq, At: e.Event.At, Actor: e.Event.Actor, Report: p.Report})
	}
	return out, nil
}
