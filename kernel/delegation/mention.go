package delegation

// 讨论区的 @（§6.3、§7.5，决定 100）：delegate.mention 在一个事务里写一条发言，并为每个被 @ 的执行者在讨论所在的事项下
// 建一条委派子事项（origin = mention:<发言 id>），由 kernel/background 按声明托管拉起（与 apa delegate --start 同一条链路）；
// @ 主会话（domain.MentionMain）不建子事项，由触达发一条唤醒。被 @ 的对象由入口结构化给出（执行者 id），内核不读正文猜名字。
// 任何一个被 @ 的叫不动，整条不生效（发言也不写）。执行者的回执写回讨论时挂在这条发言下面（parentProgress）。

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// MaxMentions 是一条发言最多 @ 几个对象。
const MaxMentions = 3

// OriginMention 是由 @ 建出的委派子事项的 origin.source；origin.ref 是发起的那条发言。
const OriginMention = "mention"

// 叫不动的类别码（候选列表的 reason 与命令错误共用，Web 按词表说明）。
const (
	BlockItemPhase   = "item_phase"    // 事项不在进行中或等待：只能发言，不能 @ 执行者
	BlockUnknown     = "unknown"       // 名录里没有这个执行者
	BlockNotStarted  = "not_startable" // 不是由已确认的无头运行声明登记的，APA 不知道怎么拉起
	BlockUnavailable = "unavailable"   // 声明所属的 Agent 现在拉不起（停用、权限卡档位不够等）
	BlockNotAllowed  = "not_allowed"   // 不在项目允许的执行者名单里
	BlockBusy        = "busy"          // 这个执行者在这件事上还有一次没跑完
	BlockSelf        = "self"          // 不能 @ 自己
)

type mentionIn struct {
	ID       string   `json:"id"` // 讨论所在的事项
	Text     string   `json:"text"`
	On       *string  `json:"on"`
	ReplyTo  string   `json:"reply_to"`
	Mentions []string `json:"mentions"`
	// Overrides 只在恰好 @ 一个执行者时可带：拉起方从命令输入读它，对这条命令建的每个子事项都生效。
	Overrides map[string]string `json:"overrides,omitempty"`
}

func parseMention(in json.RawMessage) (mentionIn, []string, error) {
	var v mentionIn
	if err := decode(in, &v); err != nil {
		return v, nil, err
	}
	if blank(v.ID) {
		return v, nil, invalid("id (the item) is required")
	}
	if len(v.Mentions) == 0 || len(v.Mentions) > MaxMentions {
		return v, nil, invalid("mention between 1 and 3 executors (for a comment without @, use item.progress)")
	}
	var execs []string
	for i, m := range v.Mentions {
		if blank(m) || slices.Contains(v.Mentions[:i], m) {
			return v, nil, invalid("mentions must be distinct, non-empty executor ids")
		}
		if m != domain.MentionMain {
			execs = append(execs, m)
		}
	}
	if len(v.Overrides) > 0 && (len(execs) != 1 || len(v.Mentions) != 1) {
		return v, nil, invalid("overrides (model, thinking) only apply when exactly one executor is mentioned")
	}
	return v, execs, nil
}

func (o Options) mention() command.Spec {
	return command.Spec{Name: "delegate.mention", Action: authz.ActionDelegate, Group: authz.GroupOrganize,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, execs, err := parseMention(in)
			if err != nil {
				return authz.Object{}, err
			}
			it, err := items.Get(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			// 项目名单由管线按 obj.Executor 判（退出码 21 并留痕）：交给它第一个不在名单里的；全在名单里时随便哪个都放行。
			// 只 @ 主会话不是委派给执行者，按本人判（本人不受名单限制）。
			obj := authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Project: it.Project, Executor: o.Owner}
			if len(execs) > 0 {
				obj.Executor = execs[0]
			}
			if it.Project != "" && len(execs) > 0 {
				p, err := projects.Get(tx, it.Project)
				if err != nil {
					return authz.Object{}, err
				}
				obj.ProjectInfo = &p
				if p.Restricted() {
					for _, e := range execs {
						if !p.ExecutorAllowed(domain.ExecutorRef{ID: e}, o.Owner) {
							obj.Executor = e
							break
						}
					}
					if obj.Candidates, err = executors.Candidates(tx, p, o.Owner); err != nil {
						return authz.Object{}, err
					}
				}
			}
			return obj, nil
		},
		// 不能 @ 自己：运行凭据按它这次运行的委派记录认执行者，绑定执行者的凭据按主语认，主 Agent 的凭据不能 @ 主会话。
		Refine: func(tx *store.Tx, c command.Caller, _ *authz.Object, in json.RawMessage) error {
			v, _, err := parseMention(in)
			if err != nil {
				return err
			}
			self := []string{c.Actor.Principal}
			if c.Actor.Run != "" {
				if d, ok, err := ByRun(tx, c.Actor.Run); err != nil {
					return err
				} else if ok {
					self = append(self, d.Executor)
				}
			}
			if slices.Contains(v.Mentions, domain.MentionMain) && c.CredentialAgentID != "" {
				_, pol, err := policy.Current(tx)
				if err != nil {
					return err
				}
				if pol.MainAgentID != nil && *pol.MainAgentID == c.CredentialAgentID {
					self = append(self, domain.MentionMain)
				}
			}
			for _, m := range v.Mentions {
				if slices.Contains(self, m) {
					return blocked(m, BlockSelf)
				}
			}
			return nil
		},
		Validate: func(tx *store.Tx, obj authz.Object, in json.RawMessage) error {
			v, _, err := parseMention(in)
			if err != nil {
				return err
			}
			if err := items.CheckComment(tx, v.ID, v.Text, v.On, v.ReplyTo); err != nil {
				return err
			}
			it, err := items.Get(tx, v.ID)
			if err != nil {
				return err
			}
			_, pol, err := policy.Current(tx)
			if err != nil {
				return err
			}
			for _, m := range v.Mentions {
				why, err := o.blocker(tx, pol, it, obj.ProjectInfo, m)
				if err != nil {
					return err
				}
				if why != "" {
					return blocked(m, why)
				}
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, execs, _ := parseMention(c.Input)
			p := domain.Progress{Text: v.Text, ReplyTo: v.ReplyTo, Mentions: v.Mentions, Actor: c.Caller.Actor.Principal}
			if v.On != nil {
				p.On = *v.On
			}
			w, err := items.AppendComment(c.Tx, v.ID, p, c.Now())
			if err != nil {
				return nil, err
			}
			if _, err := c.Emit(domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, Reason: "mentioned " + strings.Join(v.Mentions, ", ")}); err != nil {
				return nil, err
			}
			var written struct {
				Progress domain.Progress `json:"progress"`
			}
			if err := json.Unmarshal(w.Payload, &written); err != nil {
				return nil, err
			}
			comment := written.Progress.ID
			out := map[string]any{"id": v.ID, "comment": comment}
			var kids []string
			for _, e := range execs {
				ch, err := o.createChild(c, createIn{Parent: v.ID, Assignee: e, Title: mentionTitle(v.Text), Body: v.Text,
					Reason: "mentioned in " + comment, origin: &domain.Origin{Source: OriginMention, Ref: comment}})
				if err != nil {
					return nil, err
				}
				kids = append(kids, ch.ID)
			}
			if len(kids) > 0 {
				out["items"] = kids
			}
			return out, nil
		}}
}

func blocked(target, why string) error {
	e := domain.NewError(domain.CodeConflict, "cannot mention "+target+" ("+why+"); nothing was written",
		"read apa executors --project <the item's project> and the item's phase; mention someone who can be started, or comment without --mention")
	return e
}

// blocker 是一个 @ 对象叫不动的类别码，空为叫得动。不含调用者相关的判定（不能 @ 自己、链路深度），那两项在命令里判。
func (o Options) blocker(tx *store.Tx, pol policy.Policy, it domain.Item, proj *domain.Project, target string) (string, error) {
	if target == domain.MentionMain {
		_, why, err := executors.MainWake(tx, pol)
		return why, err
	}
	if it.Phase != domain.PhaseOpen && it.Phase != domain.PhaseWaiting {
		return BlockItemPhase, nil
	}
	e, ok, err := executors.Get(tx, target)
	switch {
	case err != nil:
		return "", err
	case !ok:
		return BlockUnknown, nil
	case proj != nil && !proj.ExecutorAllowed(domain.ExecutorRef{ID: e.ID, Host: e.Host, Harness: e.Harness}, o.Owner):
		return BlockNotAllowed, nil
	case e.Harness == "":
		return BlockNotStarted, nil
	}
	sel, err := executors.Resolve(tx, pol, executors.Selection{Harness: e.Harness, Runner: "task"})
	if err != nil {
		return "", err
	}
	if sel.AccessReason(sel.Snapshot.PermissionCap) != "" {
		return BlockUnavailable, nil
	}
	busy, err := Busy(tx, it.ID, target)
	if err != nil || busy {
		return BlockBusy, err
	}
	return "", nil
}

// Busy 报告执行者 executor 在事项 item 下是否还有一次没跑完：没结束的委派子事项上有在途的委派记录。
func Busy(tx *store.Tx, item, executor string) (bool, error) {
	kids, err := items.Children(tx, item)
	if err != nil {
		return false, err
	}
	for _, k := range kids {
		if k.Assignee != executor || k.Phase.Terminal() {
			continue
		}
		ds, err := Dispatches(tx, k.ID)
		if err != nil {
			return false, err
		}
		for _, d := range ds {
			if d.State == DispatchRunning {
				return true, nil
			}
		}
	}
	return false, nil
}

// mentionTitle 是由 @ 建出的子事项的标题：发言的第一行，过长截断。
func mentionTitle(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	if r := []rune(line); len(r) > 80 {
		line = string(r[:80]) + "…"
	}
	return line
}

// Candidate 是讨论里能不能 @ 的一项（GET /items/{id}/mentionable）：名录里的每个执行者（含不在 APA 主机上的）与主会话。
// OK 为假时 Reason 是叫不动的类别码；Overridable 是这个执行者的声明允许按次覆盖的参数与取值。
type Candidate struct {
	ID          string                        `json:"id"`
	Kind        string                        `json:"kind"` // executor | main
	Label       string                        `json:"label,omitempty"`
	Host        string                        `json:"host,omitempty"`
	Remote      bool                          `json:"remote,omitempty"`
	OK          bool                          `json:"ok"`
	Reason      string                        `json:"reason,omitempty"`
	Overridable map[string]executors.Override `json:"overridable,omitempty"`
}

// mentionable 是 delegate.mentionable：读事项的 @ 候选（GET /items/{id}/mentionable），与读事项同一组。
func (o Options) mentionable() command.Spec {
	return command.Spec{Name: "delegate.mentionable", Action: authz.ActionRead, Group: authz.GroupView,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v struct {
				ID string `json:"id"`
			}
			if err := decode(in, &v); err != nil {
				return authz.Object{}, err
			}
			it, err := items.Get(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			return authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Project: it.Project}, nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			cs, err := o.Mentionable(c.Tx, c.Object.ID)
			return map[string]any{"candidates": cs}, err
		}}
}

// Mentionable 列出事项 item 的讨论里能 @ 谁、叫不动的为什么。本人的执行者 id（指派给本人的工作 APA 不拉起）不在列。只读。
func (o Options) Mentionable(tx *store.Tx, item string) ([]Candidate, error) {
	it, err := items.Get(tx, item)
	if err != nil {
		return nil, err
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return nil, err
	}
	var proj *domain.Project
	if it.Project != "" {
		p, err := projects.Get(tx, it.Project)
		if err != nil {
			return nil, err
		}
		proj = &p
	}
	es, err := executors.List(tx)
	if err != nil {
		return nil, err
	}
	out := []Candidate{}
	main := Candidate{ID: domain.MentionMain, Kind: "main"}
	if main.Reason, err = o.blocker(tx, pol, it, proj, domain.MentionMain); err != nil {
		return nil, err
	}
	out = append(out, main)
	for _, e := range es {
		if e.ID == o.Owner {
			continue
		}
		c := Candidate{ID: e.ID, Kind: "executor", Label: e.Label, Host: e.Host, Remote: executors.Remote(e, o.Host)}
		if c.Reason, err = o.blocker(tx, pol, it, proj, e.ID); err != nil {
			return nil, err
		}
		if e.Harness != "" {
			if c.Overridable, err = executors.Overridable(tx, e.Harness); err != nil {
				return nil, err
			}
		}
		out = append(out, c)
	}
	for i := range out {
		out[i].OK = out[i].Reason == ""
	}
	return out, nil
}
