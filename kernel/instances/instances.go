// Package instances 是能力包实例（设计 §6.12、附录 C.10）：用户建的具体定时或提醒，一条账本记录。
// instance.create/update/toggle 按包声明的 instance_params（JSON Schema）校验参数，
// 并在同一事务里把 `schedule` 落为 Timer（§6.8）；到期的系统命令经 HandleTimer 记成 instance.fired 事件，
// 由主动服务的规则执行消费（kernel/rules：按包里实例驱动的时钟规则执行动作）。同一周期只触发一次、错过只补最近一次由时钟保证。
package instances

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。
var Module = store.Module{Name: "instances", Migrations: []store.Migration{{
	Version: 1, Name: "create-instance",
	SQL: `CREATE TABLE instances_instance (
	id TEXT PRIMARY KEY,
	pack TEXT NOT NULL,
	project TEXT NOT NULL,
	params TEXT NOT NULL,
	enabled INTEGER NOT NULL
) WITHOUT ROWID;`,
}}}

// Instance 是账本里的一条实例。
type Instance struct {
	ID      string          `json:"id"`
	Pack    string          `json:"pack"`
	Project string          `json:"project,omitempty"`
	Params  json.RawMessage `json:"params"`
	Enabled bool            `json:"enabled"`
}

// 实例事件类型。它们写进账本（不变量 1），但不在 B.4 的对外稳定集合里，订阅方看不到；
// 要对外开放须先改附录 B.4 与 domain.RegisteredEventTypes。
const (
	EvCreated = "instance.created"
	EvUpdated = "instance.updated"
	EvToggled = "instance.toggled"
	EvFired   = "instance.fired"
)

// Packs 是实例读取包声明的来源（packs.Loader 满足）。
type Packs interface {
	Get(name string) (*packs.Pack, bool)
}

func invalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "fix the request and retry")
}

func isCode(err error, c domain.Code) bool {
	var de *domain.Error
	return errors.As(err, &de) && de.Code == c
}

func newID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "ins_" + hex.EncodeToString(b), nil
}

// timerID 是实例 Timer 的 id；一个实例至多一个 Timer。
func timerID(instance string) string { return "instance_" + instance }

// ---- 读 ----

// Get 读一条实例，不存在返回 not_found。
func Get(tx *store.Tx, id string) (Instance, error) {
	l, err := query(tx, `SELECT id, pack, project, params, enabled FROM instances_instance WHERE id = ?`, id)
	if err != nil {
		return Instance{}, err
	}
	if len(l) == 0 {
		return Instance{}, domain.NewError(domain.CodeNotFound, "instance "+id+" does not exist", "list instances and retry")
	}
	return l[0], nil
}

// List 按 id 排序返回全部实例。
func List(tx *store.Tx) ([]Instance, error) {
	return query(tx, `SELECT id, pack, project, params, enabled FROM instances_instance ORDER BY id`)
}

func query(tx *store.Tx, sql string, args ...any) ([]Instance, error) {
	r, err := tx.Query(sql, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	var out []Instance
	for r.Next() {
		var (
			in     Instance
			params string
			en     int64
		)
		if err := r.Scan(&in.ID, &in.Pack, &in.Project, &params, &en); err != nil {
			return nil, err
		}
		in.Params, in.Enabled = json.RawMessage(params), en != 0
		out = append(out, in)
	}
	return out, r.Err()
}

func save(tx *store.Tx, in Instance) error {
	en := 0
	if in.Enabled {
		en = 1
	}
	_, err := tx.Exec(`INSERT INTO instances_instance (id, pack, project, params, enabled) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET project = excluded.project, params = excluded.params, enabled = excluded.enabled`,
		in.ID, in.Pack, in.Project, string(in.Params), en)
	return err
}

// ---- 校验与落 Timer ----

type svc struct{ packs Packs }

// check 按包的 instance_params 校验参数；包不存在、包没声明实例参数、参数不符都拒绝。
func (s *svc) check(tx *store.Tx, in Instance) error {
	p, ok := s.packs.Get(in.Pack)
	if !ok {
		return invalid("pack " + in.Pack + " is not loaded")
	}
	if p.InstanceParams == nil {
		return invalid("pack " + in.Pack + " declares no instance_params")
	}
	var v any
	if err := json.Unmarshal(in.Params, &v); err != nil {
		return invalid("params is not valid JSON")
	}
	if err := items.ValidateAgainst(p.InstanceParams, v); err != nil {
		return invalid("params do not match instance_params of pack " + in.Pack + ": " + err.Error())
	}
	if _, _, err := plan(in, time.Time{}); err != nil {
		return err
	}
	if in.Project != "" {
		if _, err := projects.Get(tx, in.Project); err != nil {
			if isCode(err, domain.CodeNotFound) {
				return invalid("project " + in.Project + " does not exist")
			}
			return err
		}
	}
	return nil
}

// plan 把 params.schedule 译成 Timer 的形状：Go 时长（如 "24h"）或 5 段 cron 是周期定时，从 now 起算；
// RFC3339 时刻是一次性提醒。没有 schedule 则不落 Timer。
func plan(in Instance, now time.Time) (t domain.Timer, ok bool, err error) {
	s, ok, err := schedule(in.Params)
	if err != nil || !ok {
		return t, false, err
	}
	if at, perr := time.Parse(time.RFC3339, s); perr == nil {
		return domain.Timer{Kind: domain.TimerRemind, At: at}, true, nil
	}
	at, perr := clock.Start(s, now)
	if perr != nil {
		return t, false, invalid("schedule " + s + " must be a positive duration like 24h, a 5-field cron expression like 0 8 * * 1-5, or an RFC3339 time")
	}
	return domain.Timer{Kind: domain.TimerRecur, At: at, Recurrence: s}, true, nil
}

// schedule 取 params.schedule；没有返回 ok=false，存在但不是字符串是错误（否则实例会静默没有 Timer）。
func schedule(params json.RawMessage) (s string, ok bool, err error) {
	var p struct {
		Schedule json.RawMessage `json:"schedule"`
	}
	if json.Unmarshal(params, &p) != nil || len(p.Schedule) == 0 {
		return "", false, nil // params 不是对象或没有 schedule：校验另有人管
	}
	if json.Unmarshal(p.Schedule, &s) != nil {
		return "", false, invalid("schedule must be a string")
	}
	return s, true, nil
}

// rearm 在 tx 内按实例当前状态重排 Timer：先撤旧的，启用且有 schedule 才建新的。
// fresh=false 用于重新启用：已过去的一次性时刻不再重建，否则已经触发过的提醒会再触发一次。
func rearm(tx *store.Tx, in Instance, now time.Time, fresh bool) error {
	if err := clock.Cancel(tx, timerID(in.ID)); err != nil && !isCode(err, domain.CodeNotFound) {
		return err
	}
	if !in.Enabled {
		return nil
	}
	t, ok, err := plan(in, now)
	if err != nil || !ok || (!fresh && t.Kind == domain.TimerRemind && !t.At.After(now)) {
		return err
	}
	t.ID, t.Target = timerID(in.ID), in.ID
	_, err = clock.Add(tx, t)
	return err
}

func emit(c *command.Ctx, typ, reason string, in Instance) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: typ, Object: in.ID, Reason: reason, Payload: b})
	return err
}

func decode(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return invalid("bad input: " + err.Error())
	}
	return nil
}

// ---- 命令 ----

type createIn struct {
	Pack    string          `json:"pack"`
	Params  json.RawMessage `json:"params"`
	Project string          `json:"project"`
	Enabled *bool           `json:"enabled"` // 缺省启用
}

type updateIn struct {
	ID      string          `json:"id"`
	Params  json.RawMessage `json:"params"`
	Project *string         `json:"project"` // 空串清除
}

type toggleIn struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled"`
}

type ref struct {
	ID string `json:"id"`
}

// Register 在命令执行器上登记实例命令。三条都按 manage_source 授权（§14.3）：本人与 delegate 可，
// worker、observer、运行身份不可；ext 与 webhook 凭据默认不能调用。
func Register(x *command.Executor, p Packs) error {
	s := &svc{packs: p}
	for _, sp := range []command.Spec{s.create(), s.update(), s.toggle()} {
		if err := x.Register(sp); err != nil {
			return err
		}
	}
	return nil
}

func existing(tx *store.Tx, id string) (authz.Object, error) {
	if id == "" {
		return authz.Object{}, invalid("instance id is required")
	}
	if _, err := Get(tx, id); err != nil {
		return authz.Object{}, err
	}
	return authz.Object{ID: id}, nil
}

func (s *svc) create() command.Spec {
	parse := func(raw json.RawMessage) (createIn, error) {
		var v createIn
		return v, decode(raw, &v)
	}
	instance := func(id string, v createIn) Instance {
		return Instance{ID: id, Pack: v.Pack, Project: v.Project, Params: v.Params, Enabled: v.Enabled == nil || *v.Enabled}
	}
	return command.Spec{Name: "instance.create", Action: authz.ActionManageSource, Tier: authz.TierWords,
		Object: func(_ *store.Tx, raw json.RawMessage) (authz.Object, error) {
			if _, err := parse(raw); err != nil {
				return authz.Object{}, err
			}
			id, err := newID() // 先定 id，授权与拒绝事件才有对象
			return authz.Object{ID: id}, err
		},
		Validate: func(tx *store.Tx, o authz.Object, raw json.RawMessage) error {
			v, _ := parse(raw)
			if len(bytes.TrimSpace(v.Params)) == 0 {
				return invalid("params is required")
			}
			return s.check(tx, instance(o.ID, v))
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			in := instance(c.Object.ID, v)
			if err := save(c.Tx, in); err != nil {
				return nil, err
			}
			if err := rearm(c.Tx, in, c.Now(), true); err != nil {
				return nil, err
			}
			return ref{in.ID}, emit(c, EvCreated, "created", in)
		}}
}

func (s *svc) update() command.Spec {
	parse := func(raw json.RawMessage) (updateIn, error) {
		var v updateIn
		if err := decode(raw, &v); err != nil {
			return v, err
		}
		if len(bytes.TrimSpace(v.Params)) == 0 && v.Project == nil {
			return v, invalid("nothing to update")
		}
		return v, nil
	}
	next := func(tx *store.Tx, v updateIn) (Instance, error) {
		in, err := Get(tx, v.ID)
		if err != nil {
			return in, err
		}
		if len(bytes.TrimSpace(v.Params)) != 0 {
			in.Params = v.Params
		}
		if v.Project != nil {
			in.Project = *v.Project
		}
		return in, nil
	}
	return command.Spec{Name: "instance.update", Action: authz.ActionManageSource, Tier: authz.TierWords,
		Object: func(tx *store.Tx, raw json.RawMessage) (authz.Object, error) {
			v, err := parse(raw)
			if err != nil {
				return authz.Object{}, err
			}
			return existing(tx, v.ID)
		},
		Validate: func(tx *store.Tx, _ authz.Object, raw json.RawMessage) error {
			v, _ := parse(raw)
			in, err := next(tx, v)
			if err != nil {
				return err
			}
			return s.check(tx, in)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			old, err := Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			in, err := next(c.Tx, v)
			if err != nil {
				return nil, err
			}
			if err := save(c.Tx, in); err != nil {
				return nil, err
			}
			// 只有 schedule 变了才重排；改别的参数不能重置周期，也不能让已触发的一次性提醒再触发。
			oldS, _, _ := schedule(old.Params)
			newS, _, _ := schedule(in.Params)
			if oldS != newS {
				if err := rearm(c.Tx, in, c.Now(), true); err != nil {
					return nil, err
				}
			}
			return ref{in.ID}, emit(c, EvUpdated, "updated", in)
		}}
}

func (s *svc) toggle() command.Spec {
	parse := func(raw json.RawMessage) (toggleIn, error) {
		var v toggleIn
		if err := decode(raw, &v); err != nil {
			return v, err
		}
		if v.Enabled == nil {
			return v, invalid("enabled is required")
		}
		return v, nil
	}
	return command.Spec{Name: "instance.toggle", Action: authz.ActionManageSource, Tier: authz.TierWords,
		Object: func(tx *store.Tx, raw json.RawMessage) (authz.Object, error) {
			v, err := parse(raw)
			if err != nil {
				return authz.Object{}, err
			}
			return existing(tx, v.ID)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			in, err := Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			if in.Enabled == *v.Enabled {
				return ref{in.ID}, nil // 状态没变：不重排 Timer，周期不被重置
			}
			in.Enabled = *v.Enabled
			if err := save(c.Tx, in); err != nil {
				return nil, err
			}
			if err := rearm(c.Tx, in, c.Now(), false); err != nil {
				return nil, err
			}
			return ref{in.ID}, emit(c, EvToggled, "toggled", in)
		}}
}

// ---- 时钟 ----

// Owns 判断时钟交来的命令是否归本模块；组装处据它在处理器间分派。
func Owns(c clock.Command) bool { return c.TimerID == timerID(c.Target) }

// HandleTimer 是时钟处理器里归实例的部分：到期记一条 instance.fired（事件时间取计划到期时刻），
// 实例已删除或已停用时 Timer 已过期，返回 nil 让时钟清掉它。不是本模块的 Timer 返回错误，由组装处分派。
// 它在时钟交来的事务里直接写事件，不经 command.Do（Do 自己开写事务，嵌套会死锁）；实例本身不变，所以不加版本。
func HandleTimer(tx *store.Tx, c clock.Command) error {
	if c.TimerID != timerID(c.Target) {
		return invalid("timer " + c.TimerID + " is not handled by the instances module")
	}
	in, err := Get(tx, c.Target)
	if isCode(err, domain.CodeNotFound) || (err == nil && !in.Enabled) {
		return nil
	}
	if err != nil {
		return err
	}
	es, err := eventlog.ReadObject(tx, in.ID)
	if err != nil {
		return err
	}
	depth := 0 // 定时不清零，沿用实例最近一条事件的深度（§14.9）
	for i := len(es) - 1; i >= 0; i-- {
		if es[i].Event.Type != domain.EvCommandRejected {
			depth = es[i].Event.ChainDepth
			break
		}
	}
	b, err := json.Marshal(struct {
		Instance
		Due time.Time `json:"due"`
	}{in, c.Due})
	if err != nil {
		return err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: EvFired, Object: in.ID, Actor: c.Actor,
		Reason: "timer elapsed", Payload: b, ChainDepth: depth, At: c.Due})
	return err
}
