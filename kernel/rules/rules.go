// Package rules 是主动服务里的规则执行（设计 §7.3、§13.3、§14.8）：
// 能力包的时钟规则和事件规则按 off / observe / auto 三档运行。
//
//   - off 跳过；observe 只记一条"本来会做什么"的事件，不写业务状态、不外呼；auto 才执行枚举动作。
//   - 动作经命令模型执行，授权依据（策略版本与规则 id）记入 Event；本包不直写其他模块的数据。
//   - 规则动作产生的 Event 不再触发规则；间接回环由命令管线的链路深度兜底。
//   - 规则内容 hash 变化后回到 observe，并产出一条健康类事件；包文件不能声明 auto，auto 只能在 Web 切换。
//
// 规则状态（当前模式与已审阅的内容 hash）归本包所有，是账本之外唯一的规则持久状态。
package rules

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 规则模式。
const (
	ModeOff     = "off"
	ModeObserve = "observe"
	ModeAuto    = "auto"
)

// 规则自己追加的记录类型。它们是内部审计记录，不在对外开放的事件类型表里（订阅看不到，`apa why` 可见）。
const (
	EvObserved     = "rule.observed"      // observe 档：本来会做什么
	EvTimerCreated = "rule.timer_created" // create_timer 动作建出的 Timer
)

const (
	groupClock  = "clock_rules"
	groupEvent  = "event_rules"
	groupSource = "source_rules"
	// timerPrefix 标识规则自己的周期 Timer（id = timerPrefix + 规则键的摘要）。
	timerPrefix = "rule_"
	// actionTimerPrefix 标识 create_timer 动作为触发事项建的 Timer（id = actionTimerPrefix + 随机串），到期经触达提醒。
	actionTimerPrefix = "ruletm_"
	// remindKind 是规则提醒在触达里的 kind：同一事项同一到期时刻只提醒一次。
	remindKind = "rule"
)

// Module 是本模块在存储层登记的迁移：规则状态、事件游标、待执行的时钟规则。
var Module = store.Module{Name: "rules", Migrations: []store.Migration{{
	Version: 1, Name: "create-rules",
	SQL: `CREATE TABLE rules_state (
	key TEXT PRIMARY KEY,
	pack TEXT NOT NULL,
	grp TEXT NOT NULL,
	id TEXT NOT NULL,
	mode TEXT NOT NULL,
	hash TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE rules_cursor (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
);
CREATE TABLE rules_fire (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	rule TEXT NOT NULL,
	due INTEGER NOT NULL
);`,
}}}

// State 是一条规则的当前状态。Key 形如 "<包>/<组>/<id>"，同时是 Event.Basis.RuleID。
type State struct {
	Key   string `json:"key"`
	Pack  string `json:"pack"`
	Group string `json:"group"` // clock_rules | event_rules
	ID    string `json:"id"`
	Mode  string `json:"mode"`
	Hash  string `json:"hash"`
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func key(pack, group, id string) string { return pack + "/" + group + "/" + id }

// ClockKey 是时钟规则的规则键：自己消费时钟规则的模块（如承接跟进）据它读当前模式。
func ClockKey(pack, id string) string { return key(pack, groupClock, id) }

// SourceKey 是 accept: auto 的来源规则的规则键，也是它在规则状态里的键。
func SourceKey(pack, id string) string { return key(pack, groupSource, id) }

func objectOf(k string) string { return "rule:" + k }

func ruleActor(k string) domain.Actor {
	return domain.Actor{Type: domain.ActorSystem, Principal: "rule:" + k, Via: domain.ViaRule}
}

var systemActor = domain.Actor{Type: domain.ActorSystem, Principal: "system", Via: domain.ViaClock}

func scanState(r *store.Rows) (State, error) {
	var s State
	err := r.Scan(&s.Key, &s.Pack, &s.Group, &s.ID, &s.Mode, &s.Hash)
	return s, err
}

const stateCols = `key, pack, grp, id, mode, hash`

func loadState(tx *store.Tx, k string) (State, bool, error) {
	r, err := tx.Query(`SELECT `+stateCols+` FROM rules_state WHERE key = ?`, k)
	if err != nil {
		return State{}, false, err
	}
	defer r.Close()
	if !r.Next() {
		return State{}, false, r.Err()
	}
	s, err := scanState(r)
	return s, err == nil, err
}

// List 返回全部规则状态，按键排序。
func List(tx *store.Tx) ([]State, error) {
	r, err := tx.Query(`SELECT ` + stateCols + ` FROM rules_state ORDER BY key`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []State
	for r.Next() {
		s, err := scanState(r)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, r.Err()
}

// ---- 命令 ----

type setModeIn struct {
	Rule string `json:"rule"`
	Mode string `json:"mode"`
}

type timerIn struct {
	Target string    `json:"target"`
	Kind   string    `json:"kind"` // remind | recur
	At     time.Time `json:"at"`
	Every  string    `json:"every,omitempty"`
	Reason string    `json:"reason,omitempty"`
}

func decode(in json.RawMessage, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

// Register 登记本模块的命令：
//   - rule.set_mode：切换规则模式。属策略类设置（change_settings），只有 Web 上的本人能调用，
//     其他入口得到退出码 20；token、运行身份不能切到 auto，也不能切其他档。
//   - rule.create_timer：规则的 create_timer 动作用它经命令模型写 Timer。
func Register(x *command.Executor) error {
	for _, sp := range []command.Spec{setMode(), createTimer()} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

func setMode() command.Spec {
	parse := func(in json.RawMessage) (setModeIn, error) {
		var v setModeIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "rule.set_mode", Action: authz.ActionChangeSettings,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			return authz.Object{ID: objectOf(v.Rule)}, nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			switch v.Mode {
			case ModeOff, ModeObserve, ModeAuto:
			default:
				return invalid("mode must be off, observe or auto")
			}
			cur, ok, err := loadState(tx, v.Rule)
			if err != nil {
				return err
			}
			if !ok {
				return domain.NewError(domain.CodeNotFound, "rule "+v.Rule+" does not exist", "list rules and retry")
			}
			if cur.Mode == v.Mode {
				return invalid("rule is already in mode " + v.Mode)
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			cur, _, err := loadState(c.Tx, v.Rule)
			if err != nil {
				return nil, err
			}
			if _, err := c.Tx.Exec(`UPDATE rules_state SET mode = ? WHERE key = ?`, v.Mode, v.Rule); err != nil {
				return nil, err
			}
			p, _ := json.Marshal(map[string]string{"rule": v.Rule, "from": cur.Mode, "to": v.Mode})
			_, err = c.Emit(domain.Event{Type: domain.EvRuleModeChanged, Object: objectOf(v.Rule), Reason: "mode set", Payload: p})
			return State{Key: cur.Key, Pack: cur.Pack, Group: cur.Group, ID: cur.ID, Mode: v.Mode, Hash: cur.Hash}, err
		}}
}

func createTimer() command.Spec {
	parse := func(in json.RawMessage) (timerIn, error) {
		var v timerIn
		return v, decode(in, &v)
	}
	return command.Spec{Name: "rule.create_timer", Action: authz.ActionUpdateOwn,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			return authz.Object{ID: v.Target}, nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			if v.Target == "" || v.At.IsZero() || (v.Kind != "remind" && v.Kind != "recur") {
				return invalid("create_timer needs a target, a time, and kind remind or recur")
			}
			_, err := items.Get(tx, v.Target) // 到期要提醒的是这个事项
			return err
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			kind, every := domain.TimerRemind, ""
			if v.Kind == "recur" {
				kind, every = domain.TimerRecur, v.Every
			}
			b := make([]byte, 8)
			if _, err := rand.Read(b); err != nil {
				return nil, err
			}
			id, err := clock.Add(c.Tx, domain.Timer{ID: actionTimerPrefix + hex.EncodeToString(b), Target: v.Target, Kind: kind, At: v.At, Recurrence: every})
			if err != nil {
				return nil, err
			}
			p, _ := json.Marshal(map[string]any{"timer": id, "kind": kind, "at": v.At, "every": every})
			_, err = c.Emit(domain.Event{Type: EvTimerCreated, Object: v.Target, Reason: "timer created by rule", Payload: p})
			return map[string]string{"timer": id}, err
		}}
}

// ---- 与能力包同步 ----

// PackSource 取已加载的能力包（packs.Loader 满足它）。
type PackSource interface {
	List() []*packs.Pack
}

type ruleDef struct {
	State
	// State.Mode 在这里是包里写的 mode（可能为空）；初始模式由类别决定（sync）。
	Reminder bool   // 提醒类：follow_up、stale 时钟规则（§14.8）；类别由规则类型推定，包不能自报
	Type     string // 时钟规则类型
	When     map[string]any
	Do       []packs.Action
}

func defs(src PackSource) []ruleDef {
	var out []ruleDef
	for _, p := range src.List() {
		for _, r := range p.ClockRules {
			k := key(p.Name, groupClock, r.ID)
			out = append(out, ruleDef{State: State{Key: k, Pack: p.Name, Group: groupClock, ID: r.ID, Mode: r.Mode, Hash: p.RuleHashes[groupClock+"/"+r.ID]},
				Reminder: r.Type == "follow_up" || r.Type == "stale", Type: r.Type, Do: r.Do, When: map[string]any{"schedule": r.Schedule}})
		}
		for _, r := range p.EventRules {
			k := key(p.Name, groupEvent, r.ID)
			out = append(out, ruleDef{State: State{Key: k, Pack: p.Name, Group: groupEvent, ID: r.ID, Mode: r.Mode, Hash: p.RuleHashes[groupEvent+"/"+r.ID]},
				When: r.When, Do: r.Do})
		}
	}
	// 只有 accept: auto 与 then: alert 的来源规则会自动产生动作，需要模式；其余来源规则是确定性映射，没有模式。
	for _, p := range src.List() {
		for _, r := range p.SourceRules {
			if r.Accept != "auto" && r.Then != "alert" {
				continue
			}
			k := key(p.Name, groupSource, r.ID)
			out = append(out, ruleDef{State: State{Key: k, Pack: p.Name, Group: groupSource, ID: r.ID, Mode: r.Mode, Hash: p.RuleHashes[groupSource+"/"+r.ID]}})
		}
	}
	return out
}

func timerID(k string) string { return timerPrefix + fmt.Sprintf("%x", shortSum(k)) }

func shortSum(s string) []byte {
	// FNV 已够区分规则键；Timer id 只需稳定且互不相同，不做安全用途。
	h := uint64(1469598103934665603)
	for i := 0; i < len(s); i++ {
		h = (h ^ uint64(s[i])) * 1099511628211
	}
	return []byte{byte(h >> 56), byte(h >> 48), byte(h >> 40), byte(h >> 32), byte(h >> 24), byte(h >> 16), byte(h >> 8), byte(h)}
}

// Reconcile 让规则状态与当前生效的能力包一致；启动时调一次，之后每次能力包重新加载（热加载）都调。
//   - 第一次见到的规则：取包声明的初始模式；省略时提醒类为 auto，动作类为 observe（§14.8）。
//   - 内容 hash 变化：动作类回到 observe，记 rule.mode_changed 与一条 health.alert（原模式已是 observe 时只更新 hash）；提醒类保持本人上一次的选择。
//   - v0.1 升级：未动过的内置提醒类规则切到 auto（upgradeReminders）。
//   - 周期型时钟规则（recur、check）在第一次见到或内容变化时建一个周期 Timer；schedule 含实例占位符的规则由实例建 Timer。
//
// 无法落成 Timer 的 schedule（既不是时长也不是 cron）不伪装成功：规则状态仍登记，错误在返回值里逐条列出。
func (e *Engine) Reconcile(ctx context.Context) error {
	var problems []error
	err := e.o.Store.Update(ctx, func(tx *store.Tx) error {
		problems = nil
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		ds := defs(e.o.Packs)
		if err := e.upgradeReminders(tx, pv, ds); err != nil {
			return err
		}
		for _, d := range ds {
			if err := e.sync(tx, pv, d, &problems); err != nil {
				return err
			}
		}
		_, err = e.cursor(tx)
		return err
	})
	if err != nil {
		return err
	}
	return errors.Join(problems...)
}

func (e *Engine) sync(tx *store.Tx, pv int, d ruleDef, problems *[]error) error {
	cur, ok, err := loadState(tx, d.Key)
	if err != nil {
		return err
	}
	if ok && cur.Hash == d.Hash {
		return nil
	}
	mode := d.Mode
	if mode == "" {
		mode = ModeObserve
		if d.Reminder {
			mode = ModeAuto
		}
	}
	basis := &domain.Basis{PolicyVersion: pv, RuleID: d.Key}
	if !ok {
		if _, err := tx.Exec(`INSERT INTO rules_state (key, pack, grp, id, mode, hash) VALUES (?, ?, ?, ?, ?, ?)`,
			d.Key, d.Pack, d.Group, d.ID, mode, d.Hash); err != nil {
			return err
		}
	} else if d.Reminder {
		if _, err := tx.Exec(`UPDATE rules_state SET hash = ? WHERE key = ?`, d.Hash, d.Key); err != nil {
			return err
		}
	} else {
		if _, err := tx.Exec(`UPDATE rules_state SET mode = ?, hash = ? WHERE key = ?`, ModeObserve, d.Hash, d.Key); err != nil {
			return err
		}
		if cur.Mode != ModeObserve {
			p, _ := json.Marshal(map[string]string{"rule": d.Key, "from": cur.Mode, "to": ModeObserve, "why": "content changed"})
			for _, typ := range []string{domain.EvRuleModeChanged, domain.EvHealthAlert} {
				reason := "rule content changed, back to observe"
				if _, err := eventlog.Append(tx, domain.Event{Type: typ, Object: objectOf(d.Key), Actor: ruleActor(d.Key), Basis: basis,
					Reason: reason, Payload: p, At: e.now()}); err != nil {
					return err
				}
			}
		}
		if d.Group == groupClock {
			if err := clock.Cancel(tx, timerID(d.Key)); err != nil && !isNotFound(err) {
				return err
			}
		}
	}
	if d.Group == groupClock && (d.Type == "recur" || d.Type == "check") {
		if err := e.addTimer(tx, d); err != nil {
			*problems = append(*problems, err)
		}
	}
	return nil
}

// upgradeReminders 是 v0.1 → v0.2.0 的规则模式迁移（§16.5）：账本里仍是初始 observe、包里没写 mode、
// 且从没有过 rule.mode_changed（本人没切过）的提醒类规则切到 auto，逐条记 rule.mode_changed（操作者 system），
// 并合并成一条健康类说明。包里显式写 observe/off 的、本人切换过的不动；切完即是 auto，再次对账不重复。
func (e *Engine) upgradeReminders(tx *store.Tx, pv int, ds []ruleDef) error {
	var switched []string
	for _, d := range ds {
		if !d.Reminder || d.Mode != "" {
			continue
		}
		cur, ok, err := loadState(tx, d.Key)
		if err != nil {
			return err
		}
		if !ok || cur.Mode != ModeObserve {
			continue
		}
		prior, err := eventlog.Query(tx, eventlog.Filter{Object: objectOf(d.Key), Type: domain.EvRuleModeChanged}, 0, 1)
		if err != nil {
			return err
		}
		if len(prior) > 0 {
			continue
		}
		if _, err := tx.Exec(`UPDATE rules_state SET mode = ? WHERE key = ?`, ModeAuto, d.Key); err != nil {
			return err
		}
		p, _ := json.Marshal(map[string]string{"rule": d.Key, "from": ModeObserve, "to": ModeAuto, "why": "reminder rules default to auto"})
		if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvRuleModeChanged, Object: objectOf(d.Key), Actor: systemActor,
			Basis: &domain.Basis{PolicyVersion: pv, RuleID: d.Key}, Reason: "upgrade: reminder rule default is now auto", Payload: p, At: e.now()}); err != nil {
			return err
		}
		switched = append(switched, d.Key)
	}
	if len(switched) == 0 {
		return nil
	}
	p, _ := json.Marshal(map[string]any{"kind": "rules_upgrade", "rules": switched})
	_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: "rules:upgrade", Actor: systemActor,
		Reason:  fmt.Sprintf("升级：%d 条提醒类规则按新默认值切到 auto（%s）；不想要时在 Web 设回 observe 或 off", len(switched), strings.Join(switched, "、")),
		Payload: p, At: e.now()})
	return err
}

func (e *Engine) addTimer(tx *store.Tx, d ruleDef) error {
	sched, _ := d.When["schedule"].(string)
	if strings.Contains(sched, "{") {
		return nil // 实例参数覆盖：Timer 由实例创建
	}
	at, err := clock.Start(sched, e.now())
	if err != nil {
		return fmt.Errorf("rules: %s: %w", d.Key, err)
	}
	_, err = clock.Add(tx, domain.Timer{ID: timerID(d.Key), Target: d.Key, Kind: domain.TimerRecur, At: at, Recurrence: sched})
	return err
}

func isNotFound(err error) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == domain.CodeNotFound
}

// Owns 判断时钟交来的命令是否归本模块：规则自己的周期 Timer，以及 create_timer 动作建的 Timer。组装处据它在处理器间分派。
func Owns(c clock.Command) bool {
	return (c.Kind == domain.TimerRecur && strings.HasPrefix(c.TimerID, timerPrefix)) || strings.HasPrefix(c.TimerID, actionTimerPrefix)
}

// HandleTimer 是时钟处理器里归规则的部分，在时钟交来的事务里运行：
//   - 规则自己的周期 Timer 到期，只排一条待执行记录。动作要走命令模型（自己开写事务），不能在时钟的事务里嵌套执行；
//     排好之后由 Drain 执行。
//   - create_timer 建的 Timer 到期，经触达对目标事项产出一条提醒（§7.3），受静默与合并约束，同一到期时刻只一次。
//     事项是 proposed 时不提醒（§6.14）；事项已结束或已删除时撤销 Timer。
//
// 不是规则的 Timer 返回错误，由组装处分派给对应模块。
func (e *Engine) HandleTimer(tx *store.Tx, c clock.Command) error {
	if !Owns(c) {
		return invalid("timer " + c.TimerID + " is not handled by the rules module")
	}
	if !strings.HasPrefix(c.TimerID, actionTimerPrefix) {
		_, err := tx.Exec(`INSERT INTO rules_fire (rule, due) VALUES (?, ?)`, c.Target, c.Due.UnixNano())
		return err
	}
	it, err := items.Get(tx, c.Target)
	if isNotFound(err) || (err == nil && (it.Phase == domain.PhaseDone || it.Phase == domain.PhaseDropped)) {
		if err := clock.Cancel(tx, c.TimerID); err != nil && !isNotFound(err) {
			return err
		}
		return nil
	}
	if err != nil || it.Phase == domain.PhaseProposed {
		return err
	}
	if e.o.Reminder == nil {
		return errors.New("rules: a create_timer timer fired but no Reminder is configured")
	}
	return e.o.Reminder.Remind(tx, it, remindKind, c.Due, "提醒："+it.Title, "规则设定的提醒时间已到")
}
