package executors

import (
	"bytes"
	"context"
	"encoding/json"
	"os"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 事件类型（只增不改）。
const (
	EvDeclared  = "executor.declared"  // 插件提交声明，待确认
	EvConfirmed = "executor.confirmed" // 本人确认声明，或直接登记
	EvRejected  = "executor.rejected"  // 本人拒绝声明
	EvChanged   = "executor.changed"   // 本人增改清单项
	EvRemoved   = "executor.removed"   // 本人移除清单项
)

// Options 注入本模块读取但不拥有的东西。
type Options struct {
	// Owner 是本人的执行者 id；指派给本人不受项目名单限制（§14.7）。
	Owner string
	// Project 按 id 读项目（项目模块的只读接口）；为空时 executor.list 不支持 project。
	Project func(tx *store.Tx, id string) (domain.Project, error)
}

// Register 在命令模型上登记本模块的命令：
//
//	executor.list      列出清单；带 project 时标明是否允许
//	executor.declare   插件提交声明，进入待确认（worker、delegate 或本人）
//	executor.pending   查看声明及其状态
//	executor.confirm / executor.reject   本人在 Web 处理待确认声明
//	executor.register  本人直接登记声明（主机配置文件走这里），立即生效
//	executor.put / executor.remove       本人维护清单项
//
// 登记、确认、维护都按"改设置"授权：只有 Web 上的本人可做，delegate 得到退出码 20，CLI 凭据得到退出码 4（§14.12）。
func Register(x *command.Executor, o Options) error {
	for _, s := range []command.Spec{
		{Name: "executor.list", Action: authz.ActionRead, Object: noObject, Apply: o.list},
		{Name: "executor.declare", Action: authz.ActionSubmitPlugin, Tier: authz.TierWork, Object: declObject, Apply: apply(declare)},
		{Name: "executor.pending", Action: authz.ActionSubmitPlugin, Object: noObject, Apply: pending},
		{Name: "executor.confirm", Action: authz.ActionChangeSettings, Object: refObject, Apply: apply2(confirm)},
		{Name: "executor.reject", Action: authz.ActionChangeSettings, Object: refObject, Apply: apply2(reject)},
		{Name: "executor.register", Action: authz.ActionChangeSettings, Object: declObject, Apply: apply(register)},
		{Name: "executor.put", Action: authz.ActionChangeSettings, Object: putObject, Apply: put},
		{Name: "executor.remove", Action: authz.ActionChangeSettings, Object: idObject, Apply: remove},
	} {
		if err := x.Register(s); err != nil {
			return err
		}
	}
	return nil
}

// RegisterHostFile 读取主机上的声明文件并直接登记（本人在主机上写的配置，§14.12）。
// 相同内容重复登记（例如每次启动）是空操作，不产生新版本；声明有误或登记被拒绝时返回原错误。
func RegisterHostFile(ctx context.Context, x *command.Executor, id command.Identity, path string) (command.Result, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return command.Result{}, err
	}
	return x.Do(ctx, command.Request{Identity: id, Command: "executor.register", Input: data})
}

// ---- 授权对象与解析 ----

func noObject(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{}, nil }

func declObject(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var d struct {
		Harness string `json:"harness"`
	}
	_ = json.Unmarshal(in, &d) // 解析错误由 Apply 里的严格解码报告
	if d.Harness == "" {
		return authz.Object{}, nil
	}
	return authz.Object{ID: "executors:declaration:" + d.Harness}, nil
}

type ref struct {
	Harness string `json:"harness"`
	Rev     int64  `json:"rev"`
}

func refObject(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var r ref
	if err := decode(in, &r); err != nil {
		return authz.Object{}, err
	}
	if r.Harness == "" || r.Rev < 1 {
		return authz.Object{}, invalid("harness and rev are required")
	}
	return authz.Object{ID: "executors:declaration:" + r.Harness}, nil
}

type idIn struct {
	ID string `json:"id"`
}

func idObject(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var r idIn
	if err := decode(in, &r); err != nil {
		return authz.Object{}, err
	}
	if r.ID == "" {
		return authz.Object{}, invalid("id is required")
	}
	return authz.Object{ID: "executors:" + r.ID}, nil
}

func putObject(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var e Executor
	if err := decode(in, &e); err != nil {
		return authz.Object{}, err
	}
	if err := e.validate(); err != nil {
		return authz.Object{}, err
	}
	return authz.Object{ID: "executors:" + e.ID}, nil
}

func payload(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // 只用于固定结构
	}
	return b
}

// ---- 命令 ----

func apply(f func(*command.Ctx, Declaration) (any, error)) func(*command.Ctx) (any, error) {
	return func(c *command.Ctx) (any, error) {
		d, err := ParseDeclaration(c.Input)
		if err != nil {
			return nil, err
		}
		return f(c, d)
	}
}

func apply2(f func(*command.Ctx, ref) (any, error)) func(*command.Ctx) (any, error) {
	return func(c *command.Ctx) (any, error) {
		var r ref
		if err := decode(c.Input, &r); err != nil {
			return nil, err
		}
		return f(c, r)
	}
}

func declare(c *command.Ctx, d Declaration) (any, error) {
	if err := checkCollisions(c.Tx, d.Harness, d.Executors); err != nil {
		return nil, err
	}
	rev, err := insertDeclaration(c.Tx, d, StatePending)
	if err != nil {
		return nil, err
	}
	_, err = c.Emit(domain.Event{Type: EvDeclared, Object: c.Object.ID, Payload: payload(Record{d.Harness, rev, StatePending, d})})
	return ref{d.Harness, rev}, err
}

func register(c *command.Ctx, d Declaration) (any, error) {
	if cur, ok, err := record(c.Tx, d.Harness, 0, StateActive); err != nil {
		return nil, err
	} else if ok && sameDoc(cur.Declaration, d) {
		return ref{d.Harness, cur.Rev}, nil
	}
	if err := checkCollisions(c.Tx, d.Harness, d.Executors); err != nil {
		return nil, err
	}
	rev, err := insertDeclaration(c.Tx, d, StateActive)
	if err != nil {
		return nil, err
	}
	if err := retire(c.Tx, d.Harness, rev); err != nil {
		return nil, err
	}
	if err := activate(c.Tx, d); err != nil {
		return nil, err
	}
	_, err = c.Emit(domain.Event{Type: EvConfirmed, Object: c.Object.ID, Payload: payload(Record{d.Harness, rev, StateActive, d})})
	return ref{d.Harness, rev}, err
}

func sameDoc(a, b Declaration) bool { return bytes.Equal(payload(a), payload(b)) }

// retire 让该 harness 之前生效的声明退位。
func retire(tx *store.Tx, harness string, except int64) error {
	_, err := tx.Exec("UPDATE executors_declaration SET state = ? WHERE harness = ? AND state = ? AND rev <> ?", StateSuperseded, harness, StateActive, except)
	return err
}

func pendingRecord(tx *store.Tx, r ref) (Record, error) {
	rec, ok, err := record(tx, r.Harness, r.Rev, "")
	if err != nil {
		return Record{}, err
	}
	if !ok {
		return Record{}, notFound("declaration")
	}
	if rec.State != StatePending {
		return Record{}, conflict("declaration is %s, not pending", rec.State)
	}
	return rec, nil
}

func confirm(c *command.Ctx, r ref) (any, error) {
	rec, err := pendingRecord(c.Tx, r)
	if err != nil {
		return nil, err
	}
	d := rec.Declaration
	if err := checkCollisions(c.Tx, d.Harness, d.Executors); err != nil {
		return nil, err
	}
	if err := retire(c.Tx, d.Harness, r.Rev); err != nil {
		return nil, err
	}
	if _, err := c.Tx.Exec("UPDATE executors_declaration SET state = ? WHERE harness = ? AND rev = ?", StateActive, r.Harness, r.Rev); err != nil {
		return nil, err
	}
	if err := activate(c.Tx, d); err != nil {
		return nil, err
	}
	rec.State = StateActive
	_, err = c.Emit(domain.Event{Type: EvConfirmed, Object: c.Object.ID, Payload: payload(rec)})
	return r, err
}

func reject(c *command.Ctx, r ref) (any, error) {
	rec, err := pendingRecord(c.Tx, r)
	if err != nil {
		return nil, err
	}
	if _, err := c.Tx.Exec("UPDATE executors_declaration SET state = ? WHERE harness = ? AND rev = ?", StateRejected, r.Harness, r.Rev); err != nil {
		return nil, err
	}
	rec.State = StateRejected
	_, err = c.Emit(domain.Event{Type: EvRejected, Object: c.Object.ID, Payload: payload(rec)})
	return r, err
}

func pending(c *command.Ctx) (any, error) {
	var in struct {
		State string `json:"state"`
	}
	if len(bytes.TrimSpace(c.Input)) > 0 {
		if err := decode(c.Input, &in); err != nil {
			return nil, err
		}
	}
	if in.State == "" {
		in.State = StatePending
	}
	recs, err := Records(c.Tx, in.State)
	return map[string]any{"declarations": recs}, err
}

func put(c *command.Ctx) (any, error) {
	var e Executor
	if err := decode(c.Input, &e); err != nil {
		return nil, err
	}
	if _, owner, found, err := entryOwner(c.Tx, e.ID); err != nil {
		return nil, err
	} else if found && owner != "" {
		return nil, conflict("executor %q comes from a declaration; remove it first", e.ID)
	}
	if err := putEntry(c.Tx, e, ""); err != nil {
		return nil, err
	}
	_, err := c.Emit(domain.Event{Type: EvChanged, Object: c.Object.ID, Payload: payload(e)})
	return e, err
}

func remove(c *command.Ctx) (any, error) {
	var r idIn
	if err := decode(c.Input, &r); err != nil {
		return nil, err
	}
	n, err := c.Tx.Exec("DELETE FROM executors_entry WHERE id = ?", r.ID)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, notFound("executor " + r.ID)
	}
	_, err = c.Emit(domain.Event{Type: EvRemoved, Object: c.Object.ID, Payload: payload(r)})
	return r, err
}

// listed 是 executor.list 的一项；Allowed 只在带 project 时出现。
type listed struct {
	Executor
	Allowed *bool `json:"allowed,omitempty"`
}

func (o Options) list(c *command.Ctx) (any, error) {
	var in struct {
		Project string `json:"project"`
	}
	if len(bytes.TrimSpace(c.Input)) > 0 {
		if err := decode(c.Input, &in); err != nil {
			return nil, err
		}
	}
	var proj *domain.Project
	if in.Project != "" {
		if o.Project == nil {
			return nil, invalid("project scoping is not available")
		}
		p, err := o.Project(c.Tx, in.Project)
		if err != nil {
			return nil, err
		}
		proj = &p
	}
	es, err := List(c.Tx)
	if err != nil {
		return nil, err
	}
	out := make([]listed, len(es))
	for i, e := range es {
		out[i].Executor = e
		if proj != nil {
			ok := proj.ExecutorAllowed(e.ID, o.Owner)
			out[i].Allowed = &ok
		}
	}
	return map[string]any{"executors": out}, nil
}
