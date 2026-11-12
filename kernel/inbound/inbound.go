// Package inbound 是入站流水线（设计 §9.3、§5.6）：把落盘后 disposition 为 pending 的来源事件依次送过
// 确定性规则（能力包 source_rules）、判断型（必要时级联生成型起标题和摘要），最后落成 proposed 事项。
//
//   - 规则 then: proposed 直接映射 kind 与项目，不调模型；then: ignore 只记 ignored；then: judge 以及没有任何规则
//     命中的事件交判断型。
//   - 判断型模板约定为规则所在包（无规则命中取 DefaultPack）里的 Template。choice 题的枚举是 kind 名，
//     "ignore" 表示不值得；yes_no 题的 probability 低于 0.5 表示不值得，kind 取规则的 kind（缺省 task）。
//     级联到生成型时，输出里的 title、summary 用作事项标题与摘要。判断为不值得的只记 ignored。
//   - 模型未配置、持续不可用，或判断型在级联后仍未定，记 unjudged 并记下原因，留在来源页，不进建议流，也不当作失败；
//     其他错误（模板缺失、输出不合 schema）原样返回，事件保持 pending。
//   - 因模型不可用而未判定的事件在模型恢复后自动重判（backlog.go）：放回 pending 再走一遍本流水线；超出时限的转 expired。
//   - accept: auto 的规则：observe 档事项照常以 proposed 进入，只记一条"本来会自动接受"；auto 档（只在 Web 切换）
//     直接进 open，Event 记策略版本与规则 id；off 档按普通规则处理。
//   - 事项与处置都经命令模型写入，每个来源事件的写入幂等，崩溃后重跑不产生重复事项。
//   - 积压（unjudged、expired）由本人手动转建议（propose.go，只走"进入 proposed"这一步，不再判断、不自动接受）或忽略（backlog.go）。
//
// 跟进消息归属（§9.4）不在本包：它在判断之前拦截事件，由 kernel-followmsg 负责，经 Options.Intercept 接入；
// 命中的挂靠后结束，没命中但有关联线索的，线索随判断输入与事项理由带走。
// 本包不起子进程、不调网络；模型调用只经注入的 Judge。
package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	// CmdDispose 把一条来源事件的处置结果记入账本。
	CmdDispose = "inbound.dispose"
	// EvDisposed 是处置的审计记录，不在对外稳定的事件类型表里。
	EvDisposed = "inbound.disposed"

	// DefaultPack 与 Template 是没有规则命中时判断型取用的包与模板名。
	DefaultPack = "inbound-triage"
	Template    = "triage"

	// ChoiceIgnore 是 choice 题里表示"不值得建议"的枚举。
	ChoiceIgnore = "ignore"

	defaultKind = domain.Kind("task")
	systemPrinc = "kernel:inbound"
)

var system = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: systemPrinc, Via: domain.ViaSource}, Level: authz.LevelWorker}

// Judge 是专属 agent 的判断型入口（*direct.Service 满足它）。
type Judge interface {
	Do(ctx context.Context, c direct.Call) (direct.Outcome, error)
}

// PackSource 取已加载的能力包（packs.Loader 满足它）。
type PackSource interface {
	List() []*packs.Pack
}

// Intercept 在规则与判断之前看一条 pending 事件。attached 为真表示已挂靠到原事项，本流水线不再处理；
// 否则 hint 非空时是关联线索（命中已关闭或多个未关闭事项而没有挂靠），随判断输入与事项理由带走。
type Intercept func(ctx context.Context, ev domain.SourceEvent) (attached bool, hint string, err error)

// Options 配置 Service。
type Options struct {
	Store *store.Store
	Exec  *command.Executor
	Packs PackSource
	Judge Judge
	// Owner 是本人的标识，自动接受时作为 on_behalf_of。
	Owner string
	// OnError 接收提交后自动处理失败的错误（OnCommit 回调没有返回值）；可为空。
	OnError func(error)
	// Intercept 是跟进消息归属（§9.4）；可为空。
	Intercept Intercept
	// Now 是时钟，缺省 time.Now；自动重判按它算事件年龄与退避。
	Now func() time.Time
	// Rejudge 是自动重判的上限与节流，零值取 DefaultRejudge。
	Rejudge RejudgeLimits
}

// Service 消费 pending 的来源事件。
type Service struct {
	o     Options
	mu    sync.Mutex
	again atomic.Bool
	// proposing 串行化手动转建议；不与 mu 共用，mu 在模型调用期间一直被占着。
	proposing sync.Mutex
	// rejudging 让同一时刻只有一轮自动重判；probe 是模型仍不可用时的退避（只在内存，重启后立即再探一次）。
	rejudging sync.Mutex
	probe     probeState
	// captures 跟踪捕获触发的后台处理；测试在关账本前等它们退出，否则提交后钩子会撞上已关闭的连接。
	captures sync.WaitGroup
}

// New 返回 Service。Store、Exec、Packs、Judge 必须给出。
func New(o Options) (*Service, error) {
	if o.Store == nil || o.Exec == nil || o.Packs == nil || o.Judge == nil {
		return nil, errors.New("inbound: Store, Exec, Packs and Judge are required")
	}
	if o.Owner == "" {
		o.Owner = "owner"
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Rejudge == (RejudgeLimits{}) {
		o.Rejudge = DefaultRejudge
	}
	return &Service{o: o}, nil
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the rule or the prompt template")
}

type disposeIn struct {
	Event       string             `json:"event"`
	Disposition domain.Disposition `json:"disposition"`
	Item        string             `json:"item,omitempty"`
	Rule        string             `json:"rule,omitempty"`
	Reason      string             `json:"reason,omitempty"`
	// WouldAccept 为真表示 accept: auto 的规则在 observe 档，只记录本来会自动接受。
	WouldAccept bool `json:"would_accept,omitempty"`
	// UnjudgedReason 是 unjudged 的原因（domain.Unjudged*）。
	UnjudgedReason string `json:"unjudged_reason,omitempty"`
}

func parseDispose(in json.RawMessage) (disposeIn, error) {
	var v disposeIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, invalid("bad input: " + err.Error())
	}
	return v, nil
}

// Register 登记本模块的命令，并在每次来源拉取结果入账后立即处理新事件。
func (s *Service) Register(x *command.Executor) error {
	err := x.Register(command.Spec{Name: CmdDispose, Action: authz.ActionUpdateOwn,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parseDispose(in)
			if err != nil {
				return authz.Object{}, err
			}
			// 内核自己维护来源事件的处置：执行者记为内核身份，使写入属于"改自己的对象"。
			return authz.Object{ID: v.Event, Assignee: systemPrinc}, nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseDispose(c.Input)
			if err := runner.SetDisposition(c.Tx, v.Event, v.Disposition, v.Item, v.UnjudgedReason); err != nil {
				return nil, err
			}
			p, _ := json.Marshal(map[string]any{"event": v.Event, "disposition": v.Disposition, "item": v.Item, "rule": v.Rule,
				"unjudged_reason": v.UnjudgedReason})
			if _, err := c.Emit(domain.Event{Type: EvDisposed, Object: v.Event, Reason: v.Reason, Payload: p}); err != nil {
				return nil, err
			}
			if v.WouldAccept {
				p, _ := json.Marshal(map[string]any{"rule": v.Rule, "item": v.Item, "would_do": "accept"})
				_, err := c.Emit(domain.Event{Type: rules.EvObserved, Object: v.Item, Reason: "observe: would auto-accept by " + v.Rule, Payload: p})
				return nil, err
			}
			return nil, nil
		}})
	if err != nil {
		return err
	}
	if err := s.registerCapture(x); err != nil {
		return err
	}
	if err := registerPropose(x); err != nil {
		return err
	}
	if err := registerBacklog(x); err != nil {
		return err
	}
	x.OnCommit(func(c command.Commit) {
		switch {
		case c.Command == runner.CmdRunResult || c.Command == runner.CmdIngest:
			s.process()
		case c.Command == CmdCapture && len(c.Seqs) > 0:
			s.captures.Add(1)
			go func() { // 捕获发生在插件钩子的时限内，模型调用不能占着请求
				defer s.captures.Done()
				s.process()
			}()
		}
	})
	return nil
}

func (s *Service) process() {
	if err := s.Process(context.Background()); err != nil && s.o.OnError != nil {
		s.o.OnError(err)
	}
}

// snapshot 是一轮处理开始时读到的账本事实。
type snapshot struct {
	events  []domain.SourceEvent
	sources map[string]domain.Source
	modes   map[string]string
	made    map[string]string // 来源事件 id → 已为它建出的事项（上一轮中途失败留下的）
	projs   []domain.Project
	policy  int
}

func (s *Service) load(ctx context.Context) (sn snapshot, err error) {
	err = s.o.Store.View(ctx, func(tx *store.Tx) error {
		if sn.events, err = runner.Pending(tx); err != nil || len(sn.events) == 0 {
			return err
		}
		return sn.loadFacts(tx)
	})
	return sn, err
}

// loadFacts 读事件之外的账本事实。
func (sn *snapshot) loadFacts(tx *store.Tx) error {
	srcs, err := registry.List(tx)
	if err != nil {
		return err
	}
	sn.sources = map[string]domain.Source{}
	for _, x := range srcs {
		sn.sources[x.ID] = x
	}
	all, err := rules.List(tx)
	if err != nil {
		return err
	}
	sn.modes = map[string]string{}
	for _, r := range all {
		sn.modes[r.Key] = r.Mode
	}
	if sn.projs, err = projects.List(tx); err != nil {
		return err
	}
	// shortcut: 每轮扫一遍全部事项找出已建事项；事项量大到拖慢时，给事项加按来源事件的索引。
	its, err := items.List(tx, items.Filter{})
	if err != nil {
		return err
	}
	sn.made = map[string]string{}
	for _, it := range its {
		var x struct {
			SourceEvent string `json:"source_event"`
		}
		if len(it.Ext) > 0 && json.Unmarshal(it.Ext, &x) == nil && x.SourceEvent != "" {
			sn.made[x.SourceEvent] = it.ID
		}
	}
	sn.policy, _, err = policy.Current(tx)
	return err
}

// source 取事件所属来源；对话增量的内部来源不在登记表里。
func (sn *snapshot) source(id string) (domain.Source, error) {
	if src, ok := sn.sources[id]; ok {
		return src, nil
	}
	if _, isCapture := captureProject(id); isCapture {
		return captureSource(id), nil
	}
	return domain.Source{}, fmt.Errorf("source %s is not registered", id)
}

// Process 处理当前所有 pending 的来源事件。单个事件失败不影响其他事件，错误合并返回，失败的事件保持 pending。
// 重入与并发调用只做标记，由正在运行的那次循环接着处理。
func (s *Service) Process(ctx context.Context) error {
	if !s.mu.TryLock() {
		s.again.Store(true)
		return nil
	}
	errs := s.drain(ctx)
	s.mu.Unlock()
	if s.again.Load() { // 释放锁前后之间到达的触发不能丢
		errs = append(errs, s.Process(ctx))
	}
	return errors.Join(errs...)
}

func (s *Service) drain(ctx context.Context) []error {
	var errs []error
	for {
		s.again.Store(false)
		sn, err := s.load(ctx)
		if err != nil {
			return append(errs, err)
		}
		for _, ev := range sn.events {
			if err := s.one(ctx, sn, ev); err != nil {
				if ctx.Err() != nil {
					return append(errs, ctx.Err())
				}
				errs = append(errs, fmt.Errorf("inbound: event %s: %w", ev.ID, err))
			}
		}
		// 失败的事件保持 pending，等下次触发；只有处理期间又有新的触发才再来一轮。
		if !s.again.Load() {
			return errs
		}
	}
}

// matched 是命中的来源规则及其所在的包。
type matched struct {
	pack string
	rule packs.SourceRule
}

func (m *matched) key() string {
	if m == nil {
		return ""
	}
	return rules.SourceKey(m.pack, m.rule.ID)
}

// match 按包名、声明顺序取第一条命中的规则；没有命中返回 nil。
func (s *Service) match(src domain.Source, ev domain.SourceEvent) (*matched, error) {
	for _, p := range s.o.Packs.List() {
		for _, r := range p.SourceRules {
			ok, err := matchFlat(r.Match, src, ev)
			if err != nil {
				return nil, fmt.Errorf("source rule %s/%s: %w", p.Name, r.ID, err)
			}
			if ok {
				return &matched{pack: p.Name, rule: r}, nil
			}
		}
	}
	return nil, nil
}

// matchFlat：每个键的值是标量（相等）或标量列表（成员），全部满足才命中。
// 条件键：source_type（来源登记的语义类型）、event_type、source（来源 id）、sender。
func matchFlat(m map[string]any, src domain.Source, ev domain.SourceEvent) (bool, error) {
	for k, v := range m {
		var got string
		switch k {
		case "source_type":
			got = src.SourceKind
		case "event_type":
			got = ev.Type
		case "source":
			got = src.ID
		case "sender":
			got = ev.Sender
		default:
			return false, fmt.Errorf("condition %q is not supported (source_type, event_type, source, sender)", k)
		}
		want := []string{fmt.Sprint(v)}
		if l, ok := v.([]any); ok {
			want = want[:0]
			for _, x := range l {
				want = append(want, fmt.Sprint(x))
			}
		}
		if !slices.Contains(want, got) {
			return false, nil
		}
	}
	return true, nil
}

// proposal 是要建成事项的内容。
type proposal struct {
	kind           domain.Kind
	title, summary string
	reason         string
}

func (s *Service) one(ctx context.Context, sn snapshot, ev domain.SourceEvent) error {
	src, err := sn.source(ev.SourceID)
	if err != nil {
		return err
	}
	var hint string
	if s.o.Intercept != nil {
		attached, h, err := s.o.Intercept(ctx, ev)
		if err != nil || attached {
			return err
		}
		hint = h
	}
	m, err := s.match(src, ev)
	if err != nil {
		return err
	}
	if id, ok := sn.made[ev.ID]; ok { // 上一轮建好了事项却没记完处置：接着做，不再判断、不再建
		return s.finish(ctx, sn, m, ev, id, "resumed after partial failure")
	}
	var prop proposal
	switch {
	case m != nil && m.rule.Then == "ignore":
		return s.dispose(ctx, m, ev, disposeIn{Event: ev.ID, Disposition: domain.DispositionIgnored, Reason: "rule " + m.key() + " ignores this event"})
	case m != nil && (m.rule.Then == "" || m.rule.Then == "proposed"):
		if m.rule.Kind == "" {
			return invalid("source rule " + m.key() + " maps directly but declares no kind")
		}
		prop = proposal{kind: domain.Kind(m.rule.Kind), title: orElse(ev.Subject, ev.Type), summary: ev.Summary, reason: "rule " + m.key()}
	default: // then: judge，或没有规则命中
		var verdict string
		prop, verdict, err = s.judge(ctx, sn, src, ev, m, hint)
		if err != nil {
			return err
		}
		if verdict != "" {
			d := disposeIn{Event: ev.ID, Disposition: domain.DispositionIgnored, Reason: verdict}
			if verdict != ChoiceIgnore {
				d.Disposition, d.UnjudgedReason = domain.DispositionUnjudged, domain.UnjudgedUndecided
				if strings.HasPrefix(verdict, unavailablePrefix) {
					d.UnjudgedReason = domain.UnjudgedModelUnavailable
				}
			}
			return s.dispose(ctx, m, ev, d)
		}
	}
	if hint != "" {
		prop.reason += "; " + hint
	}
	project, err := s.project(sn, src, ev, m)
	if err != nil {
		return err
	}
	return s.propose(ctx, sn, src, ev, m, prop, project)
}

// unavailablePrefix 开头的 verdict 表示因模型不可用而未判定（会自动重判），其余未判定是判断未定。
const unavailablePrefix = "model unavailable: "

// judge 返回要建的事项；verdict 非空表示不建：ChoiceIgnore 是不值得，其余是未判定的原因。
func (s *Service) judge(ctx context.Context, sn snapshot, src domain.Source, ev domain.SourceEvent, m *matched, hint string) (proposal, string, error) {
	pack := DefaultPack
	if m != nil {
		pack = m.pack
	}
	in, _ := json.Marshal(map[string]any{"source": src.ID, "source_type": src.SourceKind, "event_type": ev.Type,
		"subject": ev.Subject, "summary": ev.Summary, "link": ev.Link, "sender": ev.Sender, "sender_verified": ev.SenderVerified, "correlation_hint": hint})
	out, err := s.o.Judge.Do(ctx, direct.Call{Pack: pack, Template: Template, Input: in, Material: ev.Detail,
		Scope: direct.Scope{Source: src.ID},
		Actor: domain.Actor{Type: domain.ActorAgent, Principal: systemPrinc, Via: domain.ViaSource},
		Basis: &domain.Basis{PolicyVersion: sn.policy, RuleID: m.key()}})
	if errors.Is(err, model.ErrNotConfigured) || errors.Is(err, direct.ErrUnavailable) {
		return proposal{}, unavailablePrefix + err.Error(), nil
	}
	if err != nil {
		return proposal{}, "", err
	}
	if !out.Settled {
		why := out.Reason
		if out.HandoffTask != "" {
			why = "needs task agent " + out.HandoffTask
		}
		return proposal{}, "judge undecided: " + why, nil
	}
	var j direct.Judgement
	var gen struct{ Title, Summary string }
	for _, st := range out.Steps {
		switch st.Category {
		case "judge":
			j = direct.Judgement{}
			if err := json.Unmarshal(st.Result, &j); err != nil {
				return proposal{}, "", err
			}
		case "generate":
			if err := json.Unmarshal(st.Result, &gen); err != nil {
				return proposal{}, "", err
			}
		}
	}
	kind := defaultKind
	if m != nil && m.rule.Kind != "" {
		kind = domain.Kind(m.rule.Kind)
	}
	switch {
	case j.Choice == ChoiceIgnore, j.Probability != nil && *j.Probability < 0.5:
		return proposal{}, ChoiceIgnore, nil
	case j.Choice != "":
		kind = domain.Kind(j.Choice)
	case j.Probability == nil:
		return proposal{}, "", invalid("inbound triage prompt " + pack + "/" + Template + " must be a choice or yes_no judge")
	}
	return proposal{kind: kind, title: orElse(gen.Title, ev.Subject, ev.Type), summary: orElse(gen.Summary, ev.Summary),
		reason: fmt.Sprintf("judged %s (confidence %.2f)", kind, j.Confidence)}, "", nil
}

func orElse(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// project 按规则的 project_by 解析归属项目；没有规则、none 或找不到仓库对应的项目时不归项目。
func (s *Service) project(sn snapshot, src domain.Source, ev domain.SourceEvent, m *matched) (string, error) {
	if src.SourceKind == CaptureKind { // 对话增量属于提交它的项目，不看规则
		id, _ := captureProject(src.ID)
		return id, nil
	}
	if m == nil {
		return "", nil
	}
	switch by := m.rule.ProjectBy; {
	case strings.HasPrefix(by, "fixed:"):
		return strings.TrimPrefix(by, "fixed:"), nil
	case by == "repo_remote":
		// 远程地址取来源登记 config 里的 remote，否则看事件链接落在哪个项目的仓库之下。
		var cfg struct {
			Remote string `json:"remote"`
		}
		if len(src.Config) > 0 {
			if err := json.Unmarshal(src.Config, &cfg); err != nil {
				return "", fmt.Errorf("source %s config: %w", src.ID, err)
			}
		}
		if p, _, ok := projects.Find(sn.projs, cfg.Remote); ok {
			return p.ID, nil
		}
		for _, p := range sn.projs {
			for _, r := range p.Repos {
				if projects.RepoHasLink(r.Remote, ev.Link) {
					return p.ID, nil
				}
			}
		}
	}
	return "", nil
}

// propose 建 proposed 事项，按需自动接受，再记处置。每步的幂等键都含来源事件 id。
func (s *Service) propose(ctx context.Context, sn snapshot, src domain.Source, ev domain.SourceEvent, m *matched, p proposal, project string) error {
	item, err := s.createItem(ctx, system, m.key(), src, ev, p, project)
	if err != nil {
		return err
	}
	return s.finish(ctx, sn, m, ev, item, p.reason)
}

// createItem 以 id 的身份为来源事件建 proposed 事项；ext.source_event 记下来源事件，供重入时找回已建事项。
func (s *Service) createItem(ctx context.Context, id command.Identity, rule string, src domain.Source, ev domain.SourceEvent, p proposal, project string) (string, error) {
	ref := ev.Link
	if ref == "" {
		ref = ev.ID
	}
	in := map[string]any{"kind": p.kind, "title": p.title, "summary": p.summary, "body": ev.Detail, "project": project,
		"origin": domain.Origin{Source: src.ID, Actor: ev.Sender, Ref: ref}, "reason": p.reason,
		"ext": map[string]string{"source_event": ev.ID}}
	res, err := s.do(ctx, id, rule, "inbound:item:"+ev.ID, "item.create", in, command.Chain{})
	if err != nil {
		return "", err
	}
	var item struct {
		ID string `json:"id"`
	}
	err = json.Unmarshal(res.Value, &item)
	return item.ID, err
}

// finish 在事项建好之后按需自动接受，再记处置；对同一事件可重复执行。
func (s *Service) finish(ctx context.Context, sn snapshot, m *matched, ev domain.SourceEvent, item, reason string) error {
	d := disposeIn{Event: ev.ID, Disposition: domain.DispositionProposed, Item: item, Rule: m.key(), Reason: reason}
	if m != nil && m.rule.Accept == "auto" {
		mode := sn.modes[m.key()]
		if mode == "" {
			mode = rules.ModeObserve // 规则状态还没对账：按默认的观察档，不自动接受
		}
		switch mode {
		case rules.ModeAuto:
			rule := command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: "rule:" + m.key(), Via: domain.ViaRule, OnBehalfOf: s.o.Owner}, Level: authz.LevelDelegate}
			if _, err := s.do(ctx, rule, m.key(), "inbound:accept:"+ev.ID, "item.transition",
				map[string]any{"id": item, "to": domain.PhaseOpen, "reason": "rule " + m.key() + " auto accept"},
				command.Chain{Mode: command.ChainInherit, From: item}); err != nil {
				return err
			}
		case rules.ModeObserve:
			d.WouldAccept = true
		}
	}
	return s.dispose(ctx, m, ev, d)
}

func (s *Service) dispose(ctx context.Context, m *matched, ev domain.SourceEvent, d disposeIn) error {
	if d.Rule == "" {
		d.Rule = m.key()
	}
	key := "inbound:dispose:" + d.Event
	if ev.Rejudges > 0 { // 每次重判是一次新的处置，不能重放上一次的结果
		key += fmt.Sprintf(":rejudge-%d", ev.Rejudges)
	}
	_, err := s.do(ctx, system, d.Rule, key, CmdDispose, d, command.Chain{})
	return err
}

func (s *Service) do(ctx context.Context, id command.Identity, rule, idem, cmd string, input any, chain command.Chain) (command.Result, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return command.Result{}, err
	}
	return s.o.Exec.Do(ctx, command.Request{Identity: id, Command: cmd, Input: b, IdemKey: idem, Chain: chain,
		RuleID: rule, Evidence: orElse(rule, "inbound"), Owner: s.o.Owner})
}
