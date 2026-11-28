package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdProposeUnjudged 把积压的来源事件（unjudged、expired）或简报"知会"里的 digest 事件记为 proposed（§9.3、§3.12）。输入不带 item 时只过授权与状态校验，不写入。
const CmdProposeUnjudged = "inbound.propose_unjudged"

func registerPropose(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdProposeUnjudged, Action: authz.ActionManageSource, Group: authz.GroupSourceEvents,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseDispose(in)
			return authz.Object{ID: v.Event}, err
		},
		Validate: func(tx *store.Tx, o authz.Object, _ json.RawMessage) error {
			ev, err := runner.Event(tx, o.ID)
			if err != nil {
				return err
			}
			d := ev.Disposition
			if d != domain.DispositionUnjudged && d != domain.DispositionExpired && d != domain.DispositionDigest && d != domain.DispositionReminded && !runner.Running(ev) {
				return domain.NewError(domain.CodeConflict, "source event "+o.ID+" is "+string(d)+", not unjudged, digest, reminded or running in queue", "re-read the source event")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseDispose(c.Input)
			cur, err := runner.Event(c.Tx, v.Event)
			if err != nil {
				return nil, err
			}
			// 建事项之前的预检（不带 item）就在这里拦下，被拒的调用者不会留下事项。
			if err := registry.CheckProposal(c.Tx, c.Caller.Level, cur.SourceID); err != nil {
				return nil, err
			}
			if v.Item == "" {
				return nil, nil
			}
			ev, err := runner.Event(c.Tx, v.Event)
			if err != nil {
				return nil, err
			}
			if err := runner.Resolve(c.Tx, v.Event, domain.DispositionProposed, v.Item); err != nil {
				return nil, err
			}
			if err := applyMerge(c, v.Event, v.Item, v.mergeFields); err != nil {
				return nil, err
			}
			if v.WouldAccept {
				p, _ := json.Marshal(map[string]any{"rule": v.Rule, "item": v.Item, "would_do": "accept"})
				if _, err := c.Emit(domain.Event{Type: rules.EvObserved, Object: v.Item, Reason: "observe: would auto-accept by " + v.Rule, Payload: p}); err != nil {
					return nil, err
				}
			}
			payload := map[string]any{"event": v.Event, "disposition": domain.DispositionProposed, "item": v.Item, "merged": v.Merge}
			if runner.Running(ev) && ev.Judgment != "" { // 批处理建出的建议：判断一行跟着事项走（§15.1）
				if err := addJudgmentRef(c.Tx, payload, ev.Judgment); err != nil {
					return nil, err
				}
			}
			p, _ := json.Marshal(payload)
			_, err = c.Emit(domain.Event{Type: EvDisposed, Object: v.Event, Reason: v.Reason, Payload: p})
			return nil, err
		}})
}

// ProposeUnjudged 以 id 的身份把未判定事件转为 proposed 事项，返回更新后的来源事件。kind 与项目沿用命中的规则，
// 缺省 task、不归项目。事项与处置是两条命令：中途失败时事件仍在积压里，重试接回已建事项。
func (s *Service) ProposeUnjudged(ctx context.Context, id command.Identity, event string) (domain.SourceEvent, error) {
	return s.ProposeInto(ctx, id, event, "")
}

// ProposeInto 把处于处理中、未判定或 digest 的来源事件转为建议；into 非空时并入该已有建议并保留来源留痕（§9.3）。
func (s *Service) ProposeInto(ctx context.Context, id command.Identity, event, into string) (domain.SourceEvent, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	// 先过授权：item.create 对任何凭据开放，放到后面会让被拒绝的调用者留下事项。
	if _, err := s.do(ctx, id, "", "", CmdProposeUnjudged, disposeIn{Event: event}, command.Chain{}); err != nil {
		return domain.SourceEvent{}, err
	}
	var (
		item     string
		mf       mergeFields
		ev       domain.SourceEvent
		autoRule string
		autoMode string
		err      error
	)
	if strings.TrimSpace(into) != "" {
		into = strings.TrimSpace(into)
		if err = s.o.Store.View(ctx, func(tx *store.Tx) error {
			if ev, err = runner.Event(tx, event); err != nil {
				return err
			}
			it, err := items.Get(tx, into)
			if err != nil {
				return err
			}
			if it.Phase != domain.PhaseProposed {
				return domain.NewError(domain.CodeConflict, "item "+into+" is "+string(it.Phase)+", not proposed", "re-read the item")
			}
			return nil
		}); err != nil {
			return domain.SourceEvent{}, err
		}
		item = into
		mf = mergeFields{
			Merge: true,
			Trace: &domain.ProposalMerge{
				Title:   orElse(ev.Subject, ev.Type),
				Summary: ev.Summary,
				By:      "agent",
			},
		}
	} else {
		if item, mf, autoRule, autoMode, err = s.draftProposal(ctx, id, event, true); err != nil {
			return ev, err
		}
	}
	reason := "manually proposed from the source page"
	if strings.TrimSpace(into) != "" {
		reason = "merged into proposal " + into
	}
	d := disposeIn{Event: event, Item: item, Rule: autoRule, WouldAccept: autoRule != "" && autoMode == rules.ModeObserve && !mf.Merge, Reason: reason, mergeFields: mf}
	if _, err := s.do(ctx, id, autoRule, "", CmdProposeUnjudged, d, command.Chain{}); err != nil {
		return ev, err
	}
	// 直接立项（§9.3）：来源的处理方式由本人设为 create_item 时，新建的建议以处理它的调用者身份直接转进行中，
	// 授权照常走权限卡（离开建议属"替我拍板"）。卡后来被收窄时转不过去，留下建议与拒绝记录，不静默。
	if !mf.Merge {
		var mode string
		if err := s.o.Store.View(ctx, func(tx *store.Tx) error {
			cur, err := runner.Event(tx, event)
			if err == nil {
				mode, err = registry.HandlingMode(tx, cur.SourceID)
			}
			return err
		}); err != nil {
			return ev, err
		}
		if mode == domain.HandlingCreateItem {
			_, err := s.do(ctx, id, "", "inbound:create_item:"+event, "item.transition",
				map[string]any{"id": item, "to": domain.PhaseOpen, "reason": "source handling: create item directly"}, command.Chain{})
			var de *domain.Error
			if err != nil && !(errors.As(err, &de) && de.Code == domain.CodeTierClosed) {
				return ev, err
			}
		}
	}
	if autoRule != "" && autoMode == rules.ModeAuto && !mf.Merge {
		rule := command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "rule:" + autoRule, Via: domain.ViaRule, OnBehalfOf: s.o.Owner}, Level: authz.LevelDelegate}
		if _, err := s.do(ctx, rule, autoRule, "inbound:accept:"+event, "item.transition",
			map[string]any{"id": item, "to": domain.PhaseOpen, "reason": "rule " + autoRule + " auto accept"},
			command.Chain{Mode: command.ChainInherit, From: item}); err != nil {
			return ev, err
		}
	}
	err = s.o.Store.View(ctx, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, event); return })
	return ev, err
}

// DraftProposal 为来源事件建 proposed 事项并返回它的 id；这个事件已经建过就沿用，重试不会重复建。
// 只建事项，不改事件的处置：处置由调用方的命令落账（积压转建议、抽检"该看"）。
func (s *Service) DraftProposal(ctx context.Context, id command.Identity, event string) (string, error) {
	s.proposing.Lock()
	defer s.proposing.Unlock()
	// 调用方的命令不带合并信息，所以这里不并入已有建议，只新建。
	item, _, _, _, err := s.draftProposal(ctx, id, event, false)
	return item, err
}

// draftProposal 的 merge 为真时按合并键找已有建议（§9.3），返回的 mergeFields 由调用方带进处置命令。
func (s *Service) draftProposal(ctx context.Context, id command.Identity, event string, merge bool) (string, mergeFields, string, string, error) {
	var (
		sn snapshot
		ev domain.SourceEvent
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) (err error) {
		if ev, err = runner.Event(tx, event); err != nil {
			return err
		}
		return sn.loadFacts(tx, s.o.Now())
	})
	if err != nil {
		return "", mergeFields{}, "", "", err
	}
	if item, made := sn.made[event]; made {
		return item, s.resumeFields(ctx, ev), "", "", nil
	}
	return s.proposeItem(ctx, id, sn, ev, merge)
}

func (s *Service) proposeItem(ctx context.Context, id command.Identity, sn snapshot, ev domain.SourceEvent, merge bool) (string, mergeFields, string, string, error) {
	src, err := sn.source(ev.SourceID)
	if err != nil {
		return "", mergeFields{}, "", "", err
	}
	m, err := s.match(src, ev)
	if err != nil {
		return "", mergeFields{}, "", "", err
	}
	project, err := s.project(sn, src, ev, m)
	if err != nil {
		return "", mergeFields{}, "", "", err
	}
	if project == "" {
		project = sn.inboxID
	}
	kind := defaultKind
	if m != nil && m.rule.Kind != "" {
		kind = domain.Kind(m.rule.Kind)
	}
	var autoRule, autoMode string
	if m != nil && m.rule.Accept == rules.ModeAuto {
		autoRule = m.key()
		autoMode = sn.modes[autoRule]
		if autoMode == "" {
			autoMode = rules.ModeObserve
		}
	}
	p := proposal{kind: kind, title: orElse(ev.Subject, ev.Type), summary: ev.Summary, reason: "manually proposed from the source page"}
	if !merge {
		item, err := s.createItem(ctx, id, "", src, ev, p, project)
		return item, mergeFields{}, autoRule, autoMode, err
	}
	item, mf, err := s.place(ctx, id, "", src, ev, p, project)
	return item, mf, autoRule, autoMode, err
}

// ---- 合并键（§9.3 第 6 步）：同一件事从两个来源来时只成为一条建议 ----

// mergeWindow 是建议接收合并的期限：从建议创建起算。
const mergeWindow = 7 * 24 * time.Hour

// mergeFields 附在会把来源事件记为 proposed 的命令输入上，让建议信息（proposal）只经入站的处置命令写入，
// 不开放给 item.create（附录 B：proposal 只读）：MergeKey 记到来源事件；
// Merge 为真时事项 Item 是已有建议，事件并入它（记入 proposal.sources）；
// New 为真时 Item 是刚为该事件建出的建议，写入它的 proposal（来源、合并键，命中已接受事项时 AttachTo 推荐补到它）。
// Trace 是合并键命中并入时的留痕（候选内容与依据，§9.3）。
type mergeFields struct {
	MergeKey string                `json:"merge_key,omitempty"`
	Merge    bool                  `json:"merge,omitempty"`
	New      bool                  `json:"new,omitempty"`
	AttachTo string                `json:"attach_to,omitempty"`
	Trace    *domain.ProposalMerge `json:"trace,omitempty"`
}

// applyMerge 在处置命令的事务里落下合并键与建议信息，并追加对应的 item.updated。
func applyMerge(c *command.Ctx, event, item string, m mergeFields) error {
	if m.MergeKey != "" {
		if err := runner.SetMergeKey(c.Tx, event, m.MergeKey); err != nil {
			return err
		}
	}
	if !m.Merge && !m.New {
		return nil
	}
	ev, err := runner.Event(c.Tx, event)
	if err != nil {
		return err
	}
	var w items.Write
	if m.Merge {
		var changed bool
		if w, changed, err = mergeInto(c, item, ev, m.Trace); err != nil || !changed {
			return err
		}
	} else {
		cur, err := items.Get(c.Tx, item)
		if err != nil {
			return err
		}
		p := domain.Proposal{} // 起草（W2）可能已随事项写入推荐，保留
		if cur.Proposal != nil {
			p = *cur.Proposal
		}
		if !slices.Contains(p.Sources, ev.ID) {
			p.Sources = append(p.Sources, ev.ID)
		}
		p.MergeKey = m.MergeKey
		if m.AttachTo != "" { // 命中已接受事项：推荐固定为补到它，覆盖起草的推荐
			if _, err := items.Get(c.Tx, m.AttachTo); err != nil {
				return err
			}
			p.Recommend, p.AttachTo = "attach", m.AttachTo
		}
		if w, err = items.SetProposal(c.Tx, item, p, nil, c.Now()); err != nil { // 新建事项时已按 applyEventTime 写过时间
			return err
		}
	}
	if _, err := command.Bump(c.Tx, w.Object); err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: w.Type, Object: w.Object, Payload: w.Payload, Reason: "merged source event " + ev.ID})
	return err
}

// mergeInto 把来源事件并入已有建议：记入 proposal.sources，并追加一条合并留痕（同一事件、同一候选标题只记一次）；
// 事件带来的结构化时间补到事项上缺的字段（§9.3），已有的值不覆盖。
// 建议已不在 proposed（本人刚处理掉）时报冲突，处置命令失败后下一轮按新账本重新找去处。
func mergeInto(c *command.Ctx, item string, ev domain.SourceEvent, trace *domain.ProposalMerge) (items.Write, bool, error) {
	st := ev.Schedule(time.Local)
	if trace == nil {
		return items.AddProposalSource(c.Tx, item, ev.ID, &st, c.Now())
	}
	cur, err := items.Get(c.Tx, item)
	if err != nil {
		return items.Write{}, false, err
	}
	if cur.Phase != domain.PhaseProposed {
		return items.Write{}, false, domain.NewError(domain.CodeConflict, "item "+item+" is "+string(cur.Phase)+", not proposed", "re-read the item")
	}
	p := domain.Proposal{}
	if cur.Proposal != nil {
		p = *cur.Proposal
	}
	t := *trace
	t.Event, t.Source, t.At = ev.ID, ev.SourceID, c.Now()
	changed := false
	if !slices.Contains(p.Sources, ev.ID) {
		p.Sources, changed = append(slices.Clone(p.Sources), ev.ID), true
	}
	if !slices.ContainsFunc(p.Merges, func(x domain.ProposalMerge) bool { return x.Event == t.Event && x.Title == t.Title }) {
		p.Merges, changed = append(slices.Clone(p.Merges), t), true
	}
	if !changed && !missingTime(cur, st) {
		return items.Write{}, false, nil
	}
	w, err := items.SetProposal(c.Tx, item, p, &st, c.Now())
	return w, err == nil, err
}

// missingTime 报告来源事件带来了事项上还缺的时间字段：这些字段要补，即使 proposal 没有别的变化。
func missingTime(it domain.Item, st domain.EventSchedule) bool {
	return st.Deadline != nil && it.Deadline == nil || st.PlannedFor != "" && it.PlannedFor == ""
}

// mergeKeyOf 算来源事件的合并键：来源自带的优先；否则取规范化后的链接；没有链接取规范化后的标题加发送者。
// 链接与标题都没有时没有可靠的键，返回空：宁可多一条建议，也不把不相干的事件并成一条。
func mergeKeyOf(ev domain.SourceEvent) string {
	if ev.MergeKey != "" {
		return ev.MergeKey
	}
	// 对话捕获与按会话聚合的消息，标题是固定占位或会话名，推不出"同一件事"：只认来源给的键，并不并由处理它的 Agent 决定。
	if IsCaptureSource(ev.SourceID) || len(ev.Messages) > 0 {
		return ""
	}
	if l := normalizeLink(ev.Link); l != "" {
		return "link:" + l
	}
	if t := normalizeText(ev.Subject); t != "" {
		return titleKey(ev.Subject, ev.Sender)
	}
	return ""
}

// titleKey 是不带链接的合并键：规范化后的标题加发送者。
func titleKey(title, sender string) string {
	return "title:" + normalizeText(title) + "\x1f" + normalizeText(sender)
}

func normalizeText(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

// normalizeLink 小写 scheme 与主机、去掉片段与路径末尾的斜杠；解析不了的链接按规范化文本比较。
func normalizeLink(l string) string {
	l = strings.TrimSpace(l)
	u, err := url.Parse(l)
	if err != nil || u.Host == "" {
		return normalizeText(l)
	}
	u.Scheme, u.Host, u.Fragment = strings.ToLower(u.Scheme), strings.ToLower(u.Host), ""
	u.Path = strings.TrimRight(u.Path, "/")
	u.RawPath = ""
	return u.String()
}

// planMerge 找合并键命中的事项：7 天内仍在 proposed 的建议（into，取最新的一条）接收合并；
// 否则命中已接受、未关闭的事项（attachTo，取最新的一条）只用于推荐"补到该事项"，内核不自动并入。
// proposed 以外的已处置建议（dropped 等）与已关闭的事项都不参与。
func (s *Service) planMerge(ctx context.Context, key string) (into, attachTo string, err error) {
	if key == "" {
		return "", "", nil
	}
	var hit []domain.Item
	if err = s.o.Store.View(ctx, func(tx *store.Tx) (err error) { hit, err = items.ByMergeKey(tx, key); return }); err != nil {
		return "", "", err
	}
	slices.SortStableFunc(hit, func(a, b domain.Item) int { return b.CreatedAt.Compare(a.CreatedAt) }) // 新的在前
	now := s.o.Now()
	for _, it := range hit {
		switch it.Phase {
		case domain.PhaseProposed:
			if into == "" && now.Sub(it.CreatedAt) <= mergeWindow {
				into = it.ID
			}
		case domain.PhaseOpen, domain.PhaseWaiting, domain.PhaseNeedsInput:
			if attachTo == "" {
				attachTo = it.ID
			}
		}
	}
	if into != "" {
		attachTo = ""
	}
	return into, attachTo, nil
}

// place 为来源事件找到它的建议：合并键命中可并入的建议就返回那条（并标记 Merge），否则新建，
// 命中已接受事项时新建的建议推荐补到它。语义相近的不在这里判断，由处理它的后台 Agent 决定。调用方把返回的 mergeFields 带进处置命令。
func (s *Service) place(ctx context.Context, id command.Identity, rule string, src domain.Source, ev domain.SourceEvent, p proposal, project string) (string, mergeFields, error) {
	key := mergeKeyOf(ev)
	into, attachTo, err := s.planMerge(ctx, key)
	if err != nil {
		return "", mergeFields{}, err
	}
	keyTrace := &domain.ProposalMerge{Title: p.title, Summary: p.summary, Kind: p.kind, Project: project, By: "merge_key"}
	if into != "" {
		return into, mergeFields{MergeKey: key, Merge: true, Trace: keyTrace}, nil
	}
	item, err := s.createItem(ctx, id, rule, src, ev, p, project)
	return item, mergeFields{MergeKey: key, New: true, AttachTo: attachTo}, err
}

// resumeFields 是上一轮建好事项却没记完处置时的合并信息：接着写建议信息；推荐补到哪里按当前账本重新找。
func (s *Service) resumeFields(ctx context.Context, ev domain.SourceEvent) mergeFields {
	key := mergeKeyOf(ev)
	_, attachTo, _ := s.planMerge(ctx, key) // 读不到时只是少一条推荐，不影响处置
	return mergeFields{MergeKey: key, New: true, AttachTo: attachTo}
}
