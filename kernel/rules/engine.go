package rules

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

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Call 是交给动作处理器的一次执行。Do 以该规则的身份走命令模型：授权依据、链路深度继承与幂等键都已带好。
type Call struct {
	Rule   string
	Action packs.Action
	Item   *domain.Item // 触发事项；时钟规则或事件不涉及事项时为空
	Seq    int64        // 触发事件的序号；时钟规则为 0
	Depth  int          // 触发事件的链路深度；拉起新运行的动作据此加一（§14.9）
	Do     func(cmd string, input any) (command.Result, error)
	// Instance 是到期的能力包实例；只有实例驱动的时钟规则才有（附录 C.10）。
	Instance *Fired
	// Bound 为真表示 Item 是本次执行里 create_item 建的事项（实例驱动的规则），后续动作以它为对象。
	Bound bool
	// Requested 为真表示这次是本人在 Web 点了“现在开工”（item.start_requested）：动作不再看规则模式，其余复核照旧（§13.6）。
	Requested bool
	bind      func(id string) error
}

// Ask 以规则的身份把事项转 needs_input 交本人：只用于"跑完了但没有结论"、要本人决定怎么收尾的情形（§8.7）；
// 动作没能完成（故障、没拉起、被拦下）返回错误即可，规则引擎记告警，规则建的事项转 dropped。
func (c Call) Ask(itemID, reason string) error { return askOwner(c.Do, itemID, reason) }

// askOwner 的选项不含"重新运行"：回答不会再次执行规则动作，给这个选项只会让事项回到 open 后无人处理。
func askOwner(do func(string, any) (command.Result, error), itemID, reason string) error {
	q := map[string]any{"question": reason, "code": domain.AskRuleActionFailed, "params": map[string]string{"detail": reason}, "options": domain.HandleOrDrop()}
	if _, err := do("item.update", map[string]any{"id": itemID, "ask": q, "reason": reason}); err != nil {
		return err
	}
	_, err := do("item.transition", map[string]any{"id": itemID, "to": domain.PhaseNeedsInput, "reason": reason})
	return err
}

// Fired 是一次实例到期：哪个实例、参数、项目、计划到期时刻。
type Fired struct {
	ID      string          `json:"id"`
	Pack    string          `json:"pack"`
	Project string          `json:"project,omitempty"`
	Params  json.RawMessage `json:"params"`
	Due     time.Time       `json:"due"`
}

// Handler 执行一个枚举动作。set_phase、add_tag、create_item、create_timer 由本包内置；
// start_background_run 由后台运行模块、call_agent 由专属 agent 的装配处登记。没有登记的动作按未知动作失败，
// 不伪造成功。
type Handler func(ctx context.Context, c Call) error

// Options 配置 Engine。
type Options struct {
	Store *store.Store
	Exec  *command.Executor
	Packs PackSource
	// Handlers 登记内置之外的动作处理器，键是动作名。
	Handlers map[string]Handler
	// Owner 是本人的标识，作为规则动作的 on_behalf_of 与执行者校验的主人。
	Owner string
	Now   func() time.Time
	// OnError 接收提交后自动消费失败的错误（OnCommit 回调没有返回值）；可为空。
	OnError func(error)
	// Reminder 是触达的提醒入口（*attention.Attention 满足它）：create_timer 建的 Timer 到期经它提醒。
	// 只在用到 create_timer 时需要；为空时这类 Timer 到期返回错误，不静默丢弃。
	Reminder Reminder
	// Gate 在执行实例驱动的规则前调用：返回非空原因时这次到期整次跳过，只记一条 rule.skipped，不执行任何动作，
	// 也不补跑（§7.3）。为空时不检查。
	Gate func(ctx context.Context, pack string, do []packs.Action, params json.RawMessage) (skip string, err error)
}

// Reminder 在调用方事务里产出一条提醒；同一 (kind, 事项, due) 只提醒一次。
type Reminder interface {
	Remind(tx *store.Tx, it domain.Item, kind string, due time.Time, title, summary string) error
}

// Engine 消费账本事件与到期的时钟规则，按模式执行规则。
type Engine struct {
	o     Options
	mu    sync.Mutex
	again atomic.Bool
}

// New 返回 Engine 并登记为命令提交后的消费者：每条命令提交后立即处理新事件。
func New(o Options) (*Engine, error) {
	if o.Store == nil || o.Exec == nil || o.Packs == nil {
		return nil, errors.New("rules: Store, Exec and Packs are required")
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	e := &Engine{o: o}
	o.Exec.OnCommit(func(command.Commit) {
		if err := e.Drain(context.Background()); err != nil && e.o.OnError != nil {
			e.o.OnError(err)
		}
	})
	return e, nil
}

func (e *Engine) now() time.Time { return e.o.Now() }

// cursor 返回已处理到的事件序号；第一次使用时定在账本头部，不回放历史事件（历史上的事件不应让 auto 规则补做动作）。
func (e *Engine) cursor(tx *store.Tx) (int64, error) {
	r, err := tx.Query(`SELECT seq FROM rules_cursor WHERE id = 1`)
	if err != nil {
		return 0, err
	}
	var seq int64
	found := r.Next()
	if found {
		err = r.Scan(&seq)
	} else {
		err = r.Err()
	}
	if cerr := r.Close(); err == nil {
		err = cerr
	}
	if err != nil || found {
		return seq, err
	}
	if seq, err = eventlog.Head(tx); err != nil {
		return 0, err
	}
	_, err = tx.Exec(`INSERT INTO rules_cursor (id, seq) VALUES (1, ?)`, seq)
	return seq, err
}

// Drain 处理待执行的时钟规则和新事件，直到没有新的为止。重入（动作的提交又触发消费）与并发调用只做标记，
// 由正在运行的那次循环接着处理，保证规则动作串行。
func (e *Engine) Drain(ctx context.Context) error {
	if !e.mu.TryLock() {
		e.again.Store(true)
		return nil
	}
	defer e.mu.Unlock()
	for {
		e.again.Store(false)
		n, err := e.pass(ctx)
		if err != nil {
			return err
		}
		if n == 0 && !e.again.Load() {
			return nil
		}
	}
}

const batch = 200

func (e *Engine) pass(ctx context.Context) (int, error) {
	var (
		fires  []fire
		events []eventlog.Entry
		modes  map[string]string
	)
	err := e.o.Store.Update(ctx, func(tx *store.Tx) error {
		cur, err := e.cursor(tx)
		if err != nil {
			return err
		}
		if events, err = eventlog.ReadLive(tx, cur, batch); err != nil {
			return err
		}
		if fires, err = loadFires(tx); err != nil {
			return err
		}
		all, err := List(tx)
		if err != nil {
			return err
		}
		modes = map[string]string{}
		for _, s := range all {
			modes[s.Key] = s.Mode
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	rs := defs(e.o.Packs)
	for _, f := range fires {
		if err := e.fire(ctx, rs, modes, f); err != nil {
			return 0, err
		}
	}
	for _, en := range events {
		if err := e.event(ctx, rs, modes, en); err != nil {
			return 0, err
		}
	}
	return len(fires) + len(events), nil
}

type fire struct {
	id   int64
	rule string
	due  time.Time
}

func loadFires(tx *store.Tx) ([]fire, error) {
	r, err := tx.Query(`SELECT id, rule, due FROM rules_fire ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []fire
	for r.Next() {
		var f fire
		var due int64
		if err := r.Scan(&f.id, &f.rule, &due); err != nil {
			return nil, err
		}
		f.due = time.Unix(0, due).UTC()
		out = append(out, f)
	}
	return out, r.Err()
}

// ---- 到期的时钟规则 ----

func (e *Engine) fire(ctx context.Context, rs []ruleDef, modes map[string]string, f fire) error {
	for _, d := range rs {
		if d.Key != f.rule || d.Group != groupClock {
			continue
		}
		t := &trigger{object: objectOf(d.Key), ref: fmt.Sprintf("f%d", f.id)}
		if err := e.run(ctx, d, modes[d.Key], t); err != nil {
			return err
		}
	}
	// 包已被删除的规则也清掉待执行记录。
	return e.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`DELETE FROM rules_fire WHERE id = ?`, f.id)
		return err
	})
}

// ---- 事件 ----

// trigger 是一次规则命中的上下文。
type trigger struct {
	entry     eventlog.Entry
	instance  *Fired
	bound     bool
	item      *domain.Item
	before    *domain.Item
	object    string // 链路深度的来源对象，也是观察记录挂靠的对象
	requested bool   // 本人“现在开工”
	ref       string // 幂等键里的触发标识
	depth     int
}

func (e *Engine) event(ctx context.Context, rs []ruleDef, modes map[string]string, en eventlog.Entry) error {
	if !en.Tombstone && en.Event.Type == instances.EvFired {
		if err := e.instance(ctx, rs, en); err != nil {
			return err
		}
	}
	if !en.Tombstone && en.Event.Type == items.EvStartRequested {
		if err := e.startNow(ctx, rs, en); err != nil {
			return err
		}
	}
	// 规则动作（含其被拒绝的尝试）产生的 Event 不再触发规则（§7.3）。
	if !en.Tombstone && en.Event.Actor.Via != domain.ViaRule {
		var t *trigger
		for _, d := range rs {
			if d.Group != groupEvent || modes[d.Key] == "" || modes[d.Key] == ModeOff {
				continue
			}
			if t == nil {
				var err error
				if t, err = e.trigger(ctx, en); err != nil {
					return err
				}
			}
			ok, err := match(d.When, t)
			if err != nil {
				if err := e.fail(ctx, d, t, "match", err); err != nil {
					return err
				}
				continue
			}
			if ok {
				if err := e.run(ctx, d, modes[d.Key], t); err != nil {
					return err
				}
			}
		}
	}
	return e.o.Store.Update(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE rules_cursor SET seq = ? WHERE id = 1 AND seq < ?`, en.Event.Seq, en.Event.Seq)
		return err
	})
}

// instance 执行实例所属包里由实例驱动的时钟规则（schedule 含 {instance.}）。实例是本人显式建的，
// 规则只写回自身事项，所以不走模式，直接执行（附录 C.13）。触发标识取 (实例, 到期时刻)：
// 时钟每个周期只交来一次、错过只补最近一次，重处理同一事件时命令按幂等键返回首次结果，同周期只一份。
func (e *Engine) instance(ctx context.Context, rs []ruleDef, en eventlog.Entry) error {
	var f Fired
	if err := json.Unmarshal(en.Event.Payload, &f); err != nil || f.ID == "" {
		return fmt.Errorf("rules: instance.fired %d has no instance: %v", en.Event.Seq, err)
	}
	ran := false
	for _, d := range rs {
		if d.Pack != f.Pack || d.Group != groupClock || !instanceDriven(d) {
			continue
		}
		ran = true
		t := &trigger{entry: en, instance: &f, object: f.ID, depth: en.Event.ChainDepth,
			ref: fmt.Sprintf("i%s:%d", f.ID, f.Due.UnixNano())}
		if e.o.Gate != nil {
			skip, err := e.o.Gate(ctx, d.Key, d.Do, f.Params)
			if err != nil {
				return err
			}
			if skip != "" {
				if err := e.skipped(ctx, d, t, f, skip); err != nil {
					return err
				}
				continue
			}
		}
		if err := e.execute(ctx, d, t); err != nil {
			return err
		}
	}
	if ran {
		return nil
	}
	// 包被删除或覆盖后不再有实例驱动的规则：实例还在到期，不能什么都不留。
	return e.fail(ctx, ruleDef{State: State{Key: f.Pack + "/" + groupClock}}, &trigger{entry: en, object: f.ID, depth: en.Event.ChainDepth},
		"instance", invalid("instance "+f.ID+" fired, but pack "+f.Pack+" has no instance-driven clock rule"))
}

// skipped 记一条跳过；重处理同一个 instance.fired 时不重复记。
func (e *Engine) skipped(ctx context.Context, d ruleDef, t *trigger, f Fired, why string) error {
	return e.o.Store.Update(ctx, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, f.ID)
		if err != nil {
			return err
		}
		for _, x := range es {
			if x.Event.Type == EvSkipped && x.Event.Seq > t.entry.Event.Seq {
				var p struct {
					Rule    string `json:"rule"`
					Trigger int64  `json:"trigger_seq"`
				}
				if json.Unmarshal(x.Event.Payload, &p) == nil && p.Rule == d.Key && p.Trigger == t.entry.Event.Seq {
					return nil
				}
			}
		}
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		b, _ := json.Marshal(map[string]any{"rule": d.Key, "trigger_seq": t.entry.Event.Seq, "due": f.Due, "reason": why})
		_, err = eventlog.Append(tx, domain.Event{Type: EvSkipped, Object: f.ID, Actor: ruleActor(d.Key),
			Basis: &domain.Basis{PolicyVersion: pv, RuleID: d.Key}, Reason: why, Payload: b, ChainDepth: t.depth, At: e.now()})
		return err
	})
}

// startNow 执行本人“现在开工”：只挑事件规则里的 start_background_run 动作，规则条件去掉事件与阶段转移后仍匹配这件事（如 kind、tag）才执行；
// 不看规则模式（本人的显式指令，与“立即跑一次”同理），权限上限、项目允许的执行者、链路深度与在途运行由动作处理器照旧复核。
// 没有一条规则能为它拉起时记一条告警，不假装成功。
func (e *Engine) startNow(ctx context.Context, rs []ruleDef, en eventlog.Entry) error {
	t, err := e.trigger(ctx, en)
	if err != nil {
		return err
	}
	t.ref = fmt.Sprintf("s%d", en.Event.Seq)
	t.requested = true
	ran := false
	for _, d := range rs {
		if d.Group != groupEvent {
			continue
		}
		var do []packs.Action
		for _, a := range d.Do {
			if a["action"] == "start_background_run" {
				do = append(do, a)
			}
		}
		if len(do) == 0 {
			continue
		}
		when := map[string]any{}
		for k, v := range d.When {
			if k != "event" && k != "to_phase" && k != "from_phase" {
				when[k] = v
			}
		}
		if ok, err := match(when, t); err != nil || !ok {
			continue
		}
		ran = true
		d.Do = do
		if err := e.execute(ctx, d, t); err != nil {
			return err
		}
	}
	if ran {
		return nil
	}
	return e.fail(ctx, ruleDef{State: State{Key: "item.start"}}, t, "start", invalid("no rule can start a background run for item "+en.Event.Object))
}

func instanceDriven(d ruleDef) bool {
	s, _ := d.When["schedule"].(string)
	return strings.Contains(s, "{instance.")
}

func (e *Engine) trigger(ctx context.Context, en eventlog.Entry) (*trigger, error) {
	t := &trigger{entry: en, object: en.Event.Object, ref: fmt.Sprintf("e%d", en.Event.Seq), depth: en.Event.ChainDepth}
	var p struct {
		Item   domain.Item  `json:"item"`
		Before *domain.Item `json:"before"`
	}
	if json.Unmarshal(en.Event.Payload, &p) == nil && p.Item.ID != "" {
		t.item, t.before = &p.Item, p.Before
		return t, nil
	}
	// 非事项事件：对象若是事项，用它当前的状态匹配 kind、tag 等条件。
	err := e.o.Store.View(ctx, func(tx *store.Tx) error {
		it, err := items.Get(tx, en.Event.Object)
		if err == nil {
			t.item = &it
		}
		if isNotFound(err) {
			return nil
		}
		return err
	})
	return t, err
}

// match 判断平铺条件：每个键的值是标量（相等）或标量列表（成员）；全部满足才命中。不支持表达式。
// 条件键：event、kind、phase、to_phase、from_phase、tag、tag_any、project、assignee、source。
func match(when map[string]any, t *trigger) (bool, error) {
	for k, v := range when {
		want := scalars(v)
		var got []string
		switch k {
		case "event":
			got = []string{t.entry.Event.Type}
		case "kind", "phase", "to_phase", "project", "assignee", "source":
			if t.item == nil {
				return false, nil
			}
			got = []string{itemField(*t.item, k)}
		case "from_phase":
			if t.before == nil {
				return false, nil
			}
			got = []string{string(t.before.Phase)}
		case "tag", "tag_any":
			if t.item == nil {
				return false, nil
			}
			got = t.item.Tags
		default:
			return false, fmt.Errorf("condition %q is not supported (event, kind, phase, to_phase, from_phase, tag, tag_any, project, assignee, source)", k)
		}
		if !slices.ContainsFunc(got, func(g string) bool { return slices.Contains(want, g) }) {
			return false, nil
		}
	}
	return true, nil
}

func itemField(it domain.Item, k string) string {
	switch k {
	case "kind":
		return string(it.Kind)
	case "phase", "to_phase":
		return string(it.Phase)
	case "project":
		return it.Project
	case "assignee":
		return it.Assignee
	}
	if it.Origin != nil {
		return it.Origin.Source
	}
	return ""
}

func scalars(v any) []string {
	if l, ok := v.([]any); ok {
		out := make([]string, 0, len(l))
		for _, x := range l {
			out = append(out, fmt.Sprint(x))
		}
		return out
	}
	return []string{fmt.Sprint(v)}
}

// ---- 按模式执行 ----

func (e *Engine) run(ctx context.Context, d ruleDef, mode string, t *trigger) error {
	switch mode {
	case ModeObserve:
		return e.observe(ctx, d, t)
	case ModeAuto:
		return e.execute(ctx, d, t)
	}
	return nil // off 或未登记：跳过，也不留观察记录
}

// observe 只追加一条"本来会做什么"的记录；不写业务状态，不外呼。
func (e *Engine) observe(ctx context.Context, d ruleDef, t *trigger) error {
	return e.o.Store.Update(ctx, func(tx *store.Tx) error {
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		p, _ := json.Marshal(map[string]any{"rule": d.Key, "trigger_seq": t.entry.Event.Seq, "would_do": d.Do})
		_, err = eventlog.Append(tx, domain.Event{Type: EvObserved, Object: t.object, Actor: ruleActor(d.Key),
			Basis: &domain.Basis{PolicyVersion: pv, RuleID: d.Key}, Reason: "observe: would run " + d.Key,
			Payload: p, ChainDepth: t.depth, At: e.now()})
		return err
	})
}

// execute 依次执行 do[]；某个动作失败就停止，失败记带类别码的 health.alert；create_item 已建好事项时，
// 这一份事项转 dropped 并写明原因（fail）。失败是事件不是问题（§6.14 第 20 条、§8.7）。
func (e *Engine) execute(ctx context.Context, d ruleDef, t *trigger) error {
	for i, a := range d.Do {
		name, _ := a["action"].(string)
		h, ok := e.handler(name)
		var err error
		if !ok {
			err = invalid("action " + name + " has no registered handler")
		} else {
			n := 0
			err = h(ctx, Call{Rule: d.Key, Action: a, Item: t.item, Seq: t.entry.Event.Seq, Depth: t.depth, Instance: t.instance, Bound: t.bound, Requested: t.requested,
				Do: func(cmd string, input any) (command.Result, error) {
					n++
					return e.do(ctx, d.Key, t, fmt.Sprintf("%s:%d:%d", t.ref, i, n), cmd, input)
				},
				bind: func(id string) error { return e.bind(ctx, t, id) }})
		}
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return e.fail(ctx, d, t, name, err)
		}
	}
	return nil
}

// bind 让实例驱动的规则以 create_item 建的事项作为后续动作的对象。
func (e *Engine) bind(ctx context.Context, t *trigger, id string) error {
	return e.o.Store.View(ctx, func(tx *store.Tx) error {
		it, err := items.Get(tx, id)
		t.item, t.bound = &it, err == nil
		return err
	})
}

func (e *Engine) do(ctx context.Context, k string, t *trigger, idem, cmd string, input any) (command.Result, error) {
	b, err := json.Marshal(input)
	if err != nil {
		return command.Result{}, err
	}
	owner := e.o.Owner
	if owner == "" {
		owner = "owner"
	}
	a := ruleActor(k)
	a.OnBehalfOf = owner
	return e.o.Exec.Do(ctx, command.Request{
		Identity: command.Trusted{Actor: a, Level: authz.LevelDelegate},
		Command:  cmd, Input: b, IdemKey: "rule:" + k + ":" + idem,
		Chain:  command.Chain{Mode: command.ChainInherit, From: t.object},
		RuleID: k, Evidence: "rule " + k, Owner: owner,
	})
}

func (e *Engine) fail(ctx context.Context, d ruleDef, t *trigger, action string, cause error) error {
	code, params := noticeOf(cause)
	dropped := false
	if t.bound { // 这个事项只有规则知道：规则停在这里，就没有人会再推进它，不留在 open（§8.7）
		var err error
		if dropped, err = e.handBack(ctx, d, t, action, code, cause); err != nil {
			cause = fmt.Errorf("%w; item %s stays open, drop failed: %v", cause, t.item.ID, err)
		}
	}
	switch {
	case dropped || t.bound: // 事项被收掉时一律记告警：拒绝本身的留痕在事项上，事项终结后诊断页要靠这条告诉本人
	case code == domain.NoticeChainTripped:
		return nil // chain.Record 已记熔断，链路熔断信号报告它
	case code == domain.NoticeActionRejected:
		var de *domain.Error
		if errors.As(cause, &de) {
			switch de.ExitCode() {
			case domain.ExitWebOnly, domain.ExitExecutorNotAllowed, domain.ExitScreeningPending:
				return nil // 命令管线已经记了 command.rejected，本人的事项详情里看得到
			}
		}
	}
	item := ""
	if t.item != nil {
		item = t.item.ID
	}
	var ns *domain.NotStarted
	if errors.As(cause, &ns) && item == "" {
		item = ns.Item
	}
	return e.o.Store.Update(ctx, func(tx *store.Tx) error {
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		p, _ := json.Marshal(map[string]any{"rule": d.Key, "action": action, "error": cause.Error(), "code": code, "params": params, "item": item, "dropped": dropped})
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: t.object, Actor: ruleActor(d.Key),
			Basis: &domain.Basis{PolicyVersion: pv, RuleID: d.Key}, Reason: "rule action failed: " + cause.Error(),
			Payload: p, ChainDepth: t.depth, At: e.now()})
		return err
	})
}

// noticeOf 把动作失败归到稳定类别码（domain.NoticeCode）；params 只放内核生成的机制值，不放错误原文（它可能夹着账本内容）。
func noticeOf(cause error) (domain.NoticeCode, map[string]string) {
	var ns *domain.NotStarted
	var de *domain.Error
	switch {
	case errors.As(cause, &ns):
		if ns.Detail == "" {
			return ns.Code, nil
		}
		return ns.Code, map[string]string{"detail": ns.Detail}
	case errors.Is(cause, model.ErrNotConfigured):
		return domain.NoticeModelUnset, nil
	case errors.As(cause, &de) && de.Code == domain.CodeModelsPaused:
		return domain.NoticeModelsPaused, nil
	case errors.As(cause, &de) && de.HTTPStatus() == 403:
		return domain.NoticeActionRejected, map[string]string{"code": string(de.Code)}
	}
	return domain.NoticeActionFailed, nil
}

// handBack 把规则建的、仍在 open 的事项转 dropped 并写明原因（这一份作废，下一周期或"立即跑一次"照常生成，旧的可以重开）；
// 动作已自行让事项离开 open（如回执转 done）时不动它。dropped 报告是否收掉了。
func (e *Engine) handBack(ctx context.Context, d ruleDef, t *trigger, action string, code domain.NoticeCode, cause error) (bool, error) {
	var it domain.Item
	if err := e.o.Store.View(ctx, func(tx *store.Tx) (err error) { it, err = items.Get(tx, t.item.ID); return }); err != nil {
		return false, err
	}
	if it.Phase != domain.PhaseOpen {
		return false, nil
	}
	_, err := e.do(ctx, d.Key, t, t.ref+":fail:1", "item.transition", map[string]any{"id": it.ID, "to": domain.PhaseDropped,
		"reason": "规则动作 " + action + " 没有完成（" + string(code) + "），这一份作废：" + cause.Error()})
	return err == nil, err
}
