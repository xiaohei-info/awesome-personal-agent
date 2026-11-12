// Package inbound 是入站流水线（设计 §9.3、§5.6）：把落盘后 disposition 为 pending 的来源事件依次送过
// 确定性规则（能力包 source_rules）、初筛（判断型是非题）、起草（生成型），最后落成 proposed 事项。
//
//   - 规则 then: proposed 直接映射 kind 与项目，不调模型；then: ignore 只记 ignored；then: digest 只记 digest；
//     then: screen（旧名 judge）以及没有任何规则命中的事件交初筛；then: extract 先预筛再抽取多条候选。
//   - 初筛是一道是非题（默认模板 triage-screen，规则的 screen 可换）：分类端点先答，不确定带内由对话端点重答同一题，
//     各级都未定取保守默认"是"并标"把握低"（级联在 kernel/agents/direct）。否：忽略并留痕，不起草；是：起草。
//   - 起草（默认模板 triage-draft，规则的 draft 可换）给全标题、摘要、类别、项目、日期和推荐处置。"补到已有事项"只能从内核给的
//     候选清单（同项目或同发送者、标题相近的未关闭事项）里选，清单外的 id、未登记的项目、不合 schema 的输出都不采纳；
//     起草失败（端点不可用、输出不合 schema）不挡路：标题取来源事件的标题，摘要留空。
//   - 两个端点都不可用（未配置或持续失败）、规则又判不了的事件记 unjudged 并记下原因，留在来源页，不进建议流，也不当作失败；
//     其他错误（模板缺失）原样返回，事件保持 pending。
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
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/metrics"
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
	// Template 是初筛的默认模板，DraftTemplate、PrescreenTemplate、ExtractTemplate 是起草、对话预筛与抽取的默认模板（规则的 screen、draft、prescreen、extract 可换）。
	Template          = "triage-screen"
	DraftTemplate     = "triage-draft"
	PrescreenTemplate = "capture-prescreen"
	ExtractTemplate   = "triage-extract"

	// ChoiceIgnore 是 verdict 里表示"不值得建议"的取值（选择题的枚举 ignore 同义）。
	ChoiceIgnore = "ignore"

	defaultKind = domain.Kind("task")
	systemPrinc = "kernel:inbound"
)

var system = command.Trusted{Actor: domain.Actor{Type: domain.ActorSystem, Principal: systemPrinc, Via: domain.ViaSource}, Level: authz.LevelWorker}

// Judge 是专属 agent 的入口（*direct.Service 满足它）：Judge 答初筛与预筛的是非题（级联），Do 跑起草与抽取的生成型模板。
type Judge interface {
	Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error)
	Do(ctx context.Context, c direct.Call) (direct.Outcome, error)
}

// PackSource 取已加载的能力包（packs.Loader 满足它）。
type PackSource interface {
	List() []*packs.Pack
}

// Intercept 在规则与判断之前看一条 pending 事件。attached 为真表示已挂靠到原事项，本流水线不再处理；
// 否则 hint 非空时是关联线索（命中已关闭或多个未关闭事项而没有挂靠），随判断输入与事项理由带走。
// onUpdate 是命中该事件的来源规则声明的 on_update（refresh、attach 或空），没有规则命中时为空。
type Intercept func(ctx context.Context, ev domain.SourceEvent, onUpdate string) (attached bool, hint string, err error)

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
	// Now 是时钟，缺省 clock.Now；自动重判按它算事件年龄与退避。
	Now func() time.Time
	// Rejudge 是自动重判的上限与节流，零值取 DefaultRejudge。
	Rejudge RejudgeLimits
	// BacklogReleasePerDay 是积压放开重判时每天最多进入建议流的条数（backlog_release_per_day，§9.3）；0 不限。
	BacklogReleasePerDay int
	// Series 是变化类字段条件（changed、crosses_*）取同一序列上一次取值的来源；可为空（此时这类条件恒不成立）。
	Series rules.Series
	// Extract 可替换 then: extract 的内置处理（测试用）；Alert 产出 then: alert 的提醒（kernel/metrics）。
	// Alert 没注入时命中 alert 出口的事件保持 pending 并报错，不假装处理。
	Extract Exit
	Alert   *metrics.Alerter
}

// Exit 处理一个出口：命中规则 m 的事件 ev 由它写完处置（经命令模型）。
type Exit func(ctx context.Context, src domain.Source, ev domain.SourceEvent, rule string) error

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
		o.Now = clock.Now
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
	mergeFields
	// UnjudgedReason 是 unjudged 的原因（domain.Unjudged*）。
	UnjudgedReason string `json:"unjudged_reason,omitempty"`
	// Judgment 是这次处置依据的初筛判断记录 id；Items 是 extract 出口一次生成的全部事项（Item 为其中第一个）。
	Judgment string   `json:"judgment,omitempty"`
	Items    []string `json:"items,omitempty"`
	// alert 出口（§9.3）：Conditions 是命中的数值条件及两次取值，回放用；Alert 为真时在同一事务内产出提醒投递；
	// WouldAlert 为真表示规则在 observe 档，只记 rule.observed“本来会提醒”。
	Conditions []metrics.Condition `json:"conditions,omitempty"`
	Alert      bool                `json:"alert,omitempty"`
	WouldAlert bool                `json:"would_alert,omitempty"`
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
			if err := runner.SetOutcome(c.Tx, v.Event, v.Judgment, v.Items); err != nil {
				return nil, err
			}
			if err := applyMerge(c, v.Event, v.Item, v.mergeFields); err != nil {
				return nil, err
			}
			payload := map[string]any{"event": v.Event, "disposition": v.Disposition, "item": v.Item, "rule": v.Rule,
				"unjudged_reason": v.UnjudgedReason, "judgment": v.Judgment, "merged": v.Merge}
			if len(v.Conditions) > 0 {
				payload["conditions"] = v.Conditions
			}
			if v.Alert {
				ev, err := runner.Event(c.Tx, v.Event)
				if err != nil {
					return nil, err
				}
				if payload["delivery"], err = s.o.Alert.Deliver(c.Tx, ev, v.Rule); err != nil {
					return nil, err
				}
			}
			p, _ := json.Marshal(payload)
			if _, err := c.Emit(domain.Event{Type: EvDisposed, Object: v.Event, Reason: v.Reason, Payload: p}); err != nil {
				return nil, err
			}
			if v.WouldAlert {
				return nil, s.observeAlert(c, v)
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
	if err := RegisterAttach(x); err != nil {
		return err
	}
	if err := s.registerBacklog(x); err != nil {
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
	locs    []string    // 入站初筛与起草实际会调用的端点的位置：外发范围按它们取
	open    []candidate // 未关闭的事项：起草时"补到已有事项"的候选来源
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
		if it.Phase == domain.PhaseOpen || it.Phase == domain.PhaseWaiting || it.Phase == domain.PhaseNeedsInput {
			c := candidate{ID: it.ID, Title: it.Title, Project: it.Project}
			if it.Origin != nil {
				c.Sender = it.Origin.Actor
			}
			sn.open = append(sn.open, c)
		}
		var x struct {
			SourceEvent string `json:"source_event"`
		}
		if len(it.Ext) > 0 && json.Unmarshal(it.Ext, &x) == nil && x.SourceEvent != "" {
			sn.made[x.SourceEvent] = it.ID
		}
	}
	var pol policy.Policy
	sn.policy, pol, err = policy.Current(tx)
	sn.locs = endpointLocalities(pol)
	return err
}

// source 取事件所属来源；对话增量的内部来源不在登记表里。
func (sn *snapshot) source(id string) (domain.Source, error) {
	if src, ok := sn.sources[id]; ok {
		return src, nil
	}
	if proj, isCapture := captureProject(id); isCapture {
		src := captureSource(id)
		src.Outbound = captureOutbound(sn.projs, proj)
		return src, nil
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
			if err == nil && ok {
				ok, err = rules.Where(r.Where, src, ev, s.o.Series)
			}
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
	// 以下由起草或初筛给出，手动转建议与规则直出时为零值。
	project    string
	deadline   *time.Time
	plannedFor string
	rec        *domain.Proposal
	judgment   string // 初筛的判断记录 id
}

func (s *Service) one(ctx context.Context, sn snapshot, ev domain.SourceEvent) error {
	src, err := sn.source(ev.SourceID)
	if err != nil {
		return err
	}
	var hint string
	m, merr := s.match(src, ev)
	if s.o.Intercept != nil {
		onUpdate := ""
		if m != nil {
			onUpdate = m.rule.OnUpdate
		}
		attached, h, err := s.o.Intercept(ctx, ev, onUpdate)
		if err != nil || attached {
			return err
		}
		hint = h
	}
	if merr != nil {
		return merr
	}
	exit := rules.ExitScreen // 没有规则命中按 screen 处理
	switch {
	case m != nil:
		exit = rules.Exit(m.rule)
	case IsCaptureSource(src.ID): // 对话增量固定走 extract（§9.3）
		exit = rules.ExitExtract
	}
	if id, ok := sn.made[ev.ID]; ok && exit != rules.ExitExtract { // 上一轮建好了事项却没记完处置：接着做，不再判断、不再建
		return s.finish(ctx, sn, m, ev, id, "resumed after partial failure", "", nil, s.resumeFields(ctx, ev))
	}
	var (
		prop    proposal
		project string
	)
	if exit == rules.ExitScreen || exit == rules.ExitProposed {
		if project, err = s.project(sn, src, ev, m); err != nil {
			return err
		}
	}
	switch exit {
	case rules.ExitIgnore:
		return s.dispose(ctx, m, ev, disposeIn{Event: ev.ID, Disposition: domain.DispositionIgnored, Reason: "rule " + m.key() + " ignores this event"})
	case rules.ExitDigest:
		return s.dispose(ctx, m, ev, disposeIn{Event: ev.ID, Disposition: domain.DispositionDigest, Reason: "rule " + m.key() + " digests this event"})
	case rules.ExitAlert:
		return s.alert(ctx, sn, ev, m)
	case rules.ExitExtract:
		if s.o.Extract != nil {
			return s.o.Extract(ctx, src, ev, m.key())
		}
		return s.extract(ctx, sn, src, ev, m)
	case rules.ExitProposed:
		prop = proposal{kind: defaultKind, title: orElse(ev.Subject, ev.Type), summary: ev.Summary, reason: "rule " + m.key(),
			rec: &domain.Proposal{Sources: []string{ev.ID}}}
		if m.rule.Kind != "" {
			prop.kind = domain.Kind(m.rule.Kind)
		}
	default: // screen（含旧名 judge），或没有规则命中
		var verdict string
		prop, verdict, err = s.screenAndDraft(ctx, sn, src, ev, m, hint, project)
		if err != nil {
			return err
		}
		if verdict != "" {
			return s.dispose(ctx, m, ev, withdrawn(ev, prop, verdict))
		}
	}
	if hint != "" {
		prop.reason += "; " + hint
	}
	if project == "" {
		project = prop.project
	}
	return s.propose(ctx, sn, src, ev, m, prop, project)
}

// withdrawn 把不建事项的结论（verdict）翻成对来源事件的处置。
func withdrawn(ev domain.SourceEvent, prop proposal, verdict string) disposeIn {
	d := disposeIn{Event: ev.ID, Disposition: domain.DispositionIgnored, Reason: verdict, Judgment: prop.judgment}
	if verdict == ChoiceIgnore {
		d.Reason = "screened out: not worth a suggestion"
		return d
	}
	d.Disposition, d.UnjudgedReason = domain.DispositionUnjudged, domain.UnjudgedUndecided
	switch {
	case strings.HasPrefix(verdict, unavailablePrefix):
		d.UnjudgedReason = domain.UnjudgedModelUnavailable
	case strings.HasPrefix(verdict, outboundUnsetPrefix):
		d.UnjudgedReason = domain.UnjudgedOutboundUnset
	case strings.HasPrefix(verdict, outboundNonePrefix):
		d.Disposition, d.UnjudgedReason = domain.DispositionDigest, ""
	}
	return d
}

// alert 处理 then: alert 的出口（§9.3、§14.8）：auto 档产出没有事项的提醒并记 alerted；
// observe 档（含规则状态还没对账）只记 rule.observed“本来会提醒”，处置为 ignored；off 档什么都不做，处置为 ignored。
// 不调用任何模型。
func (s *Service) alert(ctx context.Context, sn snapshot, ev domain.SourceEvent, m *matched) error {
	if s.o.Alert == nil {
		return fmt.Errorf("source rule %s uses the %s exit but no handler is configured", m.key(), rules.ExitAlert)
	}
	d := disposeIn{Event: ev.ID, Rule: m.key(), Conditions: metrics.Explain(m.rule, ev)}
	switch sn.modes[m.key()] {
	case rules.ModeAuto:
		d.Disposition, d.Alert, d.Reason = domain.DispositionAlerted, true, "rule "+m.key()+" alerts"
	case rules.ModeOff:
		d.Disposition, d.Reason = domain.DispositionIgnored, "rule "+m.key()+" is off"
	default:
		d.Disposition, d.WouldAlert, d.Reason = domain.DispositionIgnored, true, "rule "+m.key()+" is observing: would alert"
	}
	return s.dispose(ctx, m, ev, d)
}

// observeAlert 记 observe 档 alert 规则“本来会提醒”：对象是来源事件，操作者是规则本身，Web 规则设置页按它筛选观察记录。
func (s *Service) observeAlert(c *command.Ctx, v disposeIn) error {
	p, _ := json.Marshal(map[string]any{"rule": v.Rule, "event": v.Event, "would_do": "alert", "conditions": v.Conditions})
	_, err := eventlog.Append(c.Tx, domain.Event{Type: rules.EvObserved, Object: v.Event, Actor: domain.Actor{Type: domain.ActorSystem, Principal: "rule:" + v.Rule, Via: domain.ViaRule},
		Basis: c.Basis, ChainDepth: c.ChainDepth, Reason: "observe: would alert by " + v.Rule, Payload: p, At: s.o.Now()})
	return err
}

// verdict 的前缀：模型不可用而未判定（自动重判）；外发范围未选而未判定（选定后自动重判）；外发范围对可用端点都是 none（改走摘要出口）。
// 其余未判定是判断未定。
const (
	unavailablePrefix   = "model unavailable: "
	outboundUnsetPrefix = "outbound scope not chosen: "
	outboundNonePrefix  = "outbound scope is none: "
)

// endpointLocalities 是此刻已配置的判断端点与生成端点的位置；一个都没配置时为空（调用方自己报不可用，不会发出请求）。
func endpointLocalities(pol policy.Policy) []string {
	var locs []string
	if ep := pol.JudgeEndpoint; ep != nil && ep.URL != "" {
		locs = append(locs, policy.EffectiveLocality(ep.Locality))
	}
	if g := pol.GenerateEndpoint; g.URL != "" && g.Model != "" {
		locs = append(locs, policy.EffectiveLocality(g.Locality))
	}
	return locs
}

// outboundBlock 报告外发控制此刻是否拦下这个来源的内容（registry.BlockUnset、BlockNone 或空）。
func (sn *snapshot) outboundBlock(src domain.Source) string {
	return registry.Decide(src.Outbound, sn.locs...).Block
}

// captureOutbound 是项目对话捕获的外发范围：本机端点收全文，外部范围取项目的 capture_outbound，未选择则外部端点不送（§6.5、§14.14）。
func captureOutbound(projs []domain.Project, project string) domain.Outbound {
	o := domain.Outbound{Local: registry.ScopeFull}
	for _, p := range projs {
		if p.ID == project {
			o.External = p.CaptureOutbound
		}
	}
	return o
}

// freshOutbound 读此刻的外发范围与策略（不走整轮快照：积压很多时本人中途改的范围必须立即生效）。
func (s *Service) freshOutbound(ctx context.Context, src domain.Source) (domain.Outbound, policy.Policy, error) {
	var (
		o   = src.Outbound
		pol policy.Policy
	)
	err := s.o.Store.View(ctx, func(tx *store.Tx) error {
		var err error
		if _, pol, err = policy.Current(tx); err != nil {
			return err
		}
		if proj, isCapture := captureProject(src.ID); isCapture {
			p, err := projects.Get(tx, proj)
			if err != nil {
				return err
			}
			o = captureOutbound([]domain.Project{p}, proj)
			return nil
		}
		cur, err := registry.Get(tx, src.ID)
		o = cur.Outbound
		return err
	})
	return o, pol, err
}

// material 是按端点位置裁剪后的资料正文；ok 为假表示该位置的端点被外发范围排除（§14.14）。
// 聚合事件的消息与正文同等对待：它们也是来源内容。
func material(ev domain.SourceEvent, o domain.Outbound) func(locality string) (string, bool) {
	body := ev
	if len(ev.Messages) > 0 {
		var b strings.Builder
		b.WriteString(ev.Detail)
		for _, m := range ev.Messages {
			fmt.Fprintf(&b, "\n%s: %s", m.Sender, m.Text)
		}
		body.Detail = b.String()
	}
	return func(locality string) (string, bool) {
		t, ok := registry.Trim(body, registry.ScopeFor(o, locality))
		if !ok {
			return "", false
		}
		text := t.Summary + "\n" + t.Detail
		if scope := registry.ScopeFor(o, locality); scope == registry.ScopeFull && t.Link != "" {
			text += "\n" + t.Link // 完整链接只在全文范围内送出，其余范围只有结构化输入里的域名
		}
		return strings.TrimSpace(text), true
	}
}

// eventInput 是送给每一级的结构化输入：只含元信息（类型、标题、发送者、时间、链接域名），正文走资料段按位置裁剪。
func eventInput(src domain.Source, ev domain.SourceEvent, hint string, extra map[string]any) json.RawMessage {
	t, _ := registry.Trim(ev, registry.ScopeMeta)
	in := map[string]any{"source": src.ID, "source_type": src.SourceKind, "event_type": t.Type, "subject": t.Subject, "link": t.Link,
		"sender": t.Sender, "sender_verified": ev.SenderVerified, "occurred_at": t.OccurredAt, "correlation_hint": hint}
	maps.Copy(in, extra)
	b, _ := json.Marshal(in)
	return b
}

// asked 是一道判断题的结论：no 为 true 表示不值得；unavailable 非空是未判定的原因前缀文本。
type asked struct {
	no          bool
	low         bool
	judgment    string
	unavailable string
}

// ask 经级联问一道是非题（初筛、对话预筛）：分类端点先答，不确定带内由对话端点重答，各级未定取模板的保守默认。
func (s *Service) ask(ctx context.Context, src domain.Source, ev domain.SourceEvent, pack, tmpl, hint string, o domain.Outbound) (asked, error) {
	v, err := s.o.Judge.Judge(ctx, direct.JudgeCall{Pack: pack, Template: tmpl, Input: eventInput(src, ev, hint, nil),
		Material: material(ev, o), Subject: ev.ID, Source: src.ID, Scope: direct.Scope{Source: src.ID}, UnjudgedOnUnavailable: true})
	if err != nil {
		return asked{}, err
	}
	if v.Unavailable {
		return asked{unavailable: unavailablePrefix + "no judge endpoint or chat endpoint answered"}, nil
	}
	a := asked{no: v.Answer == "no" || v.Answer == ChoiceIgnore, low: v.LowConfidence}
	if v.Written {
		a.judgment = v.Record.ID
	}
	return a, nil
}

// outboundGate 先看外发控制：可用端点都被排除时不问模型。reason 非空即不可发。
func outboundGate(o domain.Outbound, pol policy.Policy, src domain.Source) string {
	switch registry.Decide(o, endpointLocalities(pol)...).Block {
	case registry.BlockUnset:
		return outboundUnsetPrefix + "the external endpoint has no outbound scope for source " + src.ID
	case registry.BlockNone:
		return outboundNonePrefix + "no endpoint may receive content from source " + src.ID
	}
	return ""
}

// screenAndDraft 走第 4、5 步：初筛，是（或未定标把握低）则起草。返回的 verdict 非空表示不建事项：ChoiceIgnore 是不值得，其余是未判定的原因。
func (s *Service) screenAndDraft(ctx context.Context, sn snapshot, src domain.Source, ev domain.SourceEvent, m *matched, hint, project string) (proposal, string, error) {
	pack, screen, draft := DefaultPack, Template, DraftTemplate
	if m != nil {
		pack = m.pack
		screen, draft = orElse(m.rule.Screen, screen), orElse(m.rule.Draft, draft)
	}
	// 外发控制（§14.14）：范围在每次调用前重读；裁剪后的内容才进请求与输入指纹。
	o, pol, err := s.freshOutbound(ctx, src)
	if err != nil {
		return proposal{}, "", err
	}
	if why := outboundGate(o, pol, src); why != "" {
		return proposal{}, why, nil
	}
	a, err := s.ask(ctx, src, ev, pack, screen, hint, o)
	if err != nil {
		return proposal{}, "", err
	}
	if a.unavailable != "" {
		return proposal{}, a.unavailable, nil
	}
	if a.no {
		return proposal{judgment: a.judgment}, ChoiceIgnore, nil
	}
	kind := defaultKind
	if m != nil && m.rule.Kind != "" {
		kind = domain.Kind(m.rule.Kind)
	}
	reason := "screened in"
	if a.low {
		reason = "screened in with low confidence"
	}
	p := proposal{kind: kind, title: orElse(ev.Subject, ev.Type), reason: reason, judgment: a.judgment, project: project,
		rec: &domain.Proposal{LowConfidence: a.low, Sources: []string{ev.ID}}}
	if err := s.draft(ctx, sn, src, ev, pack, draft, hint, o, pol, &p); err != nil {
		return proposal{}, "", err
	}
	return p, "", nil
}

// draftOut 是 triage-draft 的输出。
type draftOut struct {
	Title      string `json:"title"`
	Summary    string `json:"summary"`
	Kind       string `json:"kind"`
	Project    string `json:"project"`
	Deadline   string `json:"deadline"`
	PlannedFor string `json:"planned_for"`
	Recommend  string `json:"recommend"`
	AttachTo   string `json:"attach_to"`
	Reason     string `json:"reason"`
}

// draft 是第 5 步。起草失败不挡路：保留 p 里来源事件的标题，摘要留空，失败原因记进理由；只有模板缺失这类配置错误和取消才返回错误。
func (s *Service) draft(ctx context.Context, sn snapshot, src domain.Source, ev domain.SourceEvent, pack, tmpl, hint string, o domain.Outbound, pol policy.Policy, p *proposal) error {
	gen := pol.GenerateEndpoint
	locality := policy.EffectiveLocality(gen.Locality)
	text, allowed := material(ev, o)(locality)
	if !allowed {
		p.reason += "; draft skipped: outbound scope excludes the generate endpoint"
		return nil
	}
	cands := sn.candidates(ev, p.project)
	projs := make([]map[string]string, 0, len(sn.projs))
	for _, pr := range sn.projs {
		if pr.Status == domain.ProjectActive {
			projs = append(projs, map[string]string{"id": pr.ID, "name": pr.Name})
		}
	}
	in := eventInput(src, ev, hint, map[string]any{"projects": projs, "candidates": cands, "today": s.o.Now().Format(time.DateOnly)})
	var out direct.Outcome
	var err error
	for attempt := 0; ; attempt++ { // 调用时端点位置变了，Judge 拒绝发送：按新位置重裁剪再来
		out, err = s.o.Judge.Do(ctx, direct.Call{Pack: pack, Template: tmpl, Input: in, Material: text, RequireLocality: locality,
			Scope: direct.Scope{Source: src.ID}, Actor: domain.Actor{Type: domain.ActorAgent, Principal: systemPrinc, Via: domain.ViaSource}})
		if !errors.Is(err, direct.ErrLocalityChanged) || attempt >= 3 {
			break
		}
		if o, pol, err = s.freshOutbound(ctx, src); err != nil {
			return err
		}
		locality = policy.EffectiveLocality(pol.GenerateEndpoint.Locality)
		var ok bool
		if text, ok = material(ev, o)(locality); !ok {
			p.reason += "; draft skipped: outbound scope excludes the generate endpoint"
			return nil
		}
	}
	switch {
	case errors.Is(err, direct.ErrBadTemplate), ctx.Err() != nil:
		if err == nil {
			err = ctx.Err()
		}
		return err
	case err != nil:
		p.reason += "; draft failed: " + err.Error()
		return nil
	case !out.Settled:
		p.reason += "; draft failed: " + orElse(out.Reason, "unsettled")
		return nil
	}
	var d draftOut
	if json.Unmarshal(out.Final().Result, &d) != nil || d.Title == "" || !validKind(d.Kind) {
		p.reason += "; draft failed: output does not fit the draft schema"
		return nil
	}
	p.title, p.summary, p.kind = d.Title, d.Summary, domain.Kind(d.Kind)
	if sn.hasProject(d.Project) {
		p.project = d.Project
	}
	if t, ok := parseDay(d.Deadline); ok {
		end := time.Date(t.Year(), t.Month(), t.Day(), 23, 59, 59, 0, s.o.Now().Location())
		p.deadline = &end
	}
	if _, ok := parseDay(d.PlannedFor); ok {
		p.plannedFor = d.PlannedFor
	}
	switch d.Recommend {
	case "accept", "dismiss":
		p.rec.Recommend = d.Recommend
	case "attach":
		if slices.ContainsFunc(cands, func(c candidate) bool { return c.ID == d.AttachTo }) { // 只能从内核给的清单里选，不采纳编造的 id
			p.rec.Recommend, p.rec.AttachTo = "attach", d.AttachTo
		}
	}
	p.rec.Reason = d.Reason
	return nil
}

// validKind：起草的 kind 只能是内置模板 schema 里列出的值，未知值视同输出不合 schema。
func validKind(k string) bool {
	return slices.Contains([]string{"task", "note", "commitment", "decision"}, k)
}

func parseDay(s string) (time.Time, bool) {
	t, err := time.Parse(time.DateOnly, s)
	return t, err == nil
}

func (sn *snapshot) hasProject(id string) bool {
	return id != "" && slices.ContainsFunc(sn.projs, func(p domain.Project) bool { return p.ID == id && p.Status == domain.ProjectActive })
}

// candidate 是起草时可推荐"补到"的未关闭事项。
type candidate struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Project string `json:"project,omitempty"`
	Sender  string `json:"sender,omitempty"`
	score   float64
}

const maxCandidates = 8

// candidates 取同项目或同发送者、标题相近的未关闭事项，最像的在前。
func (sn *snapshot) candidates(ev domain.SourceEvent, project string) []candidate {
	var out []candidate
	for _, c := range sn.open {
		sim := similarity(c.Title, ev.Subject)
		sameProject := project != "" && c.Project == project
		sameSender := ev.Sender != "" && c.Sender == ev.Sender
		if sim < 0.3 && !sameProject && !sameSender {
			continue
		}
		c.score = sim
		if sameProject {
			c.score += 0.2
		}
		if sameSender {
			c.score += 0.2
		}
		out = append(out, c)
	}
	slices.SortStableFunc(out, func(a, b candidate) int { return cmp.Compare(b.score, a.score) })
	return out[:min(len(out), maxCandidates)]
}

// similarity 是两个标题按相邻字符对算出的 Dice 系数（0 到 1）。
func similarity(a, b string) float64 {
	pa, pb := bigrams(a), bigrams(b)
	if len(pa) == 0 || len(pb) == 0 {
		return 0
	}
	n := 0
	for g := range pa {
		if pb[g] {
			n++
		}
	}
	return 2 * float64(n) / float64(len(pa)+len(pb))
}

func bigrams(s string) map[[2]rune]bool {
	r := []rune(strings.ToLower(s))
	out := map[[2]rune]bool{}
	for i := 0; i+1 < len(r); i++ {
		out[[2]rune{r[i], r[i+1]}] = true
	}
	return out
}

// extractOut 是 triage-extract 的输出。
type extractOut struct {
	Candidates []struct {
		Title   string `json:"title"`
		Kind    string `json:"kind"`
		Summary string `json:"summary"`
	} `json:"candidates"`
}

const maxExtracted = 20

// extract 是 extract 出口：先预筛"要不要抽取"，再由生成型抽出零到多条候选，每条候选各自成为 proposed 事项。
// 事项的幂等键带候选序号，崩溃后重跑不产生重复事项（模型结果按输入指纹复用）。
func (s *Service) extract(ctx context.Context, sn snapshot, src domain.Source, ev domain.SourceEvent, m *matched) error {
	pack, prescreen, extract := DefaultPack, PrescreenTemplate, ExtractTemplate
	if m != nil {
		pack = m.pack
		prescreen, extract = orElse(m.rule.Prescreen, prescreen), orElse(m.rule.Extract, extract)
	}
	o, pol, err := s.freshOutbound(ctx, src)
	if err != nil {
		return err
	}
	if why := outboundGate(o, pol, src); why != "" {
		return s.dispose(ctx, m, ev, withdrawn(ev, proposal{}, why))
	}
	a, err := s.ask(ctx, src, ev, pack, prescreen, "", o)
	if err != nil {
		return err
	}
	if a.unavailable != "" {
		return s.dispose(ctx, m, ev, withdrawn(ev, proposal{}, a.unavailable))
	}
	if a.no {
		return s.dispose(ctx, m, ev, withdrawn(ev, proposal{judgment: a.judgment}, ChoiceIgnore))
	}
	locality := policy.EffectiveLocality(pol.GenerateEndpoint.Locality)
	text, allowed := material(ev, o)(locality)
	if !allowed {
		return s.dispose(ctx, m, ev, withdrawn(ev, proposal{judgment: a.judgment}, outboundUnsetPrefix+"the generate endpoint is excluded by the outbound scope"))
	}
	out, err := s.o.Judge.Do(ctx, direct.Call{Pack: pack, Template: extract, Input: eventInput(src, ev, "", nil), Material: text, RequireLocality: locality,
		Scope: direct.Scope{Source: src.ID}, Actor: domain.Actor{Type: domain.ActorAgent, Principal: systemPrinc, Via: domain.ViaSource}})
	if errors.Is(err, model.ErrNotConfigured) || errors.Is(err, direct.ErrUnavailable) {
		return s.dispose(ctx, m, ev, withdrawn(ev, proposal{judgment: a.judgment}, unavailablePrefix+err.Error()))
	}
	if err != nil {
		return err
	}
	var x extractOut
	if !out.Settled || json.Unmarshal(out.Final().Result, &x) != nil {
		return fmt.Errorf("extract template %s/%s gave no usable candidates", pack, extract)
	}
	project, err := s.project(sn, src, ev, m)
	if err != nil {
		return err
	}
	var ids []string
	for i, c := range x.Candidates {
		if i == maxExtracted {
			break
		}
		if strings.TrimSpace(c.Title) == "" || !validKind(c.Kind) {
			continue
		}
		p := proposal{kind: domain.Kind(c.Kind), title: c.Title, summary: c.Summary, reason: "extracted from " + ev.ID, judgment: a.judgment,
			rec: &domain.Proposal{LowConfidence: a.low, Sources: []string{ev.ID}}}
		id, err := s.createItemKeyed(ctx, system, m.key(), fmt.Sprintf("inbound:item:%s:%d", ev.ID, i), src, ev, p, project)
		if err != nil {
			return err
		}
		ids = append(ids, id)
	}
	d := disposeIn{Event: ev.ID, Rule: m.key(), Judgment: a.judgment}
	if len(ids) == 0 {
		d.Disposition, d.Reason = domain.DispositionIgnored, "extract found no candidate"
		return s.dispose(ctx, m, ev, d)
	}
	d.Disposition, d.Item, d.Items, d.Reason = domain.DispositionProposed, ids[0], ids, fmt.Sprintf("extracted %d candidate(s)", len(ids))
	return s.dispose(ctx, m, ev, d)
}

func orElse(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// project 按 rules.Project 的优先级解析归属项目：规则的 project_by，其次来源的默认项目，都没有则不归项目。
func (s *Service) project(sn snapshot, src domain.Source, ev domain.SourceEvent, m *matched) (string, error) {
	if src.SourceKind == CaptureKind { // 对话增量属于提交它的项目，不看规则
		id, _ := captureProject(src.ID)
		return id, nil
	}
	if m == nil {
		return src.DefaultProject, nil
	}
	repo, err := s.repoProject(sn, src, ev, m.rule.ProjectBy)
	return rules.Project(m.rule.ProjectBy, repo, src.DefaultProject), err
}

// repoProject 按 repo_remote 解析项目：远程地址取来源登记 config 里的 remote，否则看事件链接落在哪个项目的仓库之下。
func (s *Service) repoProject(sn snapshot, src domain.Source, ev domain.SourceEvent, by string) (string, error) {
	if by != "repo_remote" {
		return "", nil
	}
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
	return "", nil
}

// propose 建 proposed 事项，按需自动接受，再记处置。每步的幂等键都含来源事件 id。
func (s *Service) propose(ctx context.Context, sn snapshot, src domain.Source, ev domain.SourceEvent, m *matched, p proposal, project string) error {
	item, mf, err := s.place(ctx, system, m.key(), src, ev, p, project)
	if err != nil {
		return err
	}
	return s.finish(ctx, sn, m, ev, item, p.reason, p.judgment, nil, mf)
}

// createItem 以 id 的身份为来源事件建 proposed 事项；ext.source_event 记下来源事件，供重入时找回已建事项。
func (s *Service) createItem(ctx context.Context, id command.Identity, rule string, src domain.Source, ev domain.SourceEvent, p proposal, project string) (string, error) {
	return s.createItemKeyed(ctx, id, rule, "inbound:item:"+ev.ID, src, ev, p, project)
}

// createItemKeyed 同 createItem，幂等键由调用方给出（extract 出口一个事件建多个事项）。
func (s *Service) createItemKeyed(ctx context.Context, id command.Identity, rule, idem string, src domain.Source, ev domain.SourceEvent, p proposal, project string) (string, error) {
	ref := ev.Link
	if ref == "" {
		ref = ev.ID
	}
	in := map[string]any{"kind": p.kind, "title": p.title, "summary": p.summary, "body": ev.Detail, "project": project,
		"origin": domain.Origin{Source: src.ID, Actor: ev.Sender, Ref: ref}, "reason": p.reason,
		"ext": map[string]string{"source_event": ev.ID}}
	if p.deadline != nil {
		in["deadline"] = p.deadline
	}
	if p.plannedFor != "" {
		in["planned_for"] = p.plannedFor
	}
	if p.rec != nil {
		in["proposal"] = p.rec
	}
	res, err := s.do(ctx, id, rule, idem, "item.create", in, command.Chain{})
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
func (s *Service) finish(ctx context.Context, sn snapshot, m *matched, ev domain.SourceEvent, item, reason, judgment string, items []string, mf mergeFields) error {
	d := disposeIn{Event: ev.ID, Disposition: domain.DispositionProposed, Item: item, Rule: m.key(), Reason: reason, Judgment: judgment, Items: items, mergeFields: mf}
	if m != nil && m.rule.Accept == "auto" && !mf.Merge { // 并入已有建议：它的处置早已定过，不再替它自动接受
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
