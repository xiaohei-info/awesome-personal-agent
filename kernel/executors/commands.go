package executors

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"regexp"

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
	EvEnrolled  = "executor.enrolled"  // 宿主随委派登记或更新名录条目（§6.17）
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
//	executor.override  本人给执行者定可覆盖参数的默认值（只能取声明列出的值）
//
// 登记、确认、维护与 Agent 卡片都是 3b（造新权力，§14.3）：只有 Web 上的本人、再输一次口令后可做；
// delegate 得到退出码 20（硬线），其余钥匙得到退出码 4（范围外）。
func Register(x *command.Executor, o Options) error {
	for _, s := range []command.Spec{
		{Name: "agent.configure", Action: authz.ActionChangeSettings, Group: authz.GroupAgents, Page: "/settings/agents", Object: noObject, Apply: saveCard},
		{Name: "executor.list", Action: authz.ActionRead, Group: authz.GroupView, Object: noObject, Apply: o.list},
		{Name: "executor.declare", Action: authz.ActionSubmitPlugin, Group: authz.GroupOrganize, Object: declObject, Apply: apply(declare)},
		{Name: "executor.pending", Action: authz.ActionSubmitPlugin, Group: authz.GroupView, Object: noObject, Apply: pending},
		{Name: "executor.confirm", Action: authz.ActionChangeSettings, Group: authz.GroupAgents, Page: "/settings/agents", Object: refObject, Apply: apply2(confirm)},
		{Name: "executor.reject", Action: authz.ActionChangeSettings, Group: authz.GroupAgents, Page: "/settings/agents", Object: refObject, Apply: apply2(reject)},
		{Name: "executor.register", Action: authz.ActionChangeSettings, Group: authz.GroupAgents, Page: "/settings/agents", Object: declObject, Apply: apply(register)},
		{Name: "executor.put", Action: authz.ActionChangeSettings, Group: authz.GroupAgents, Page: "/executors", Object: putObject, Apply: put},
		{Name: "executor.remove", Action: authz.ActionChangeSettings, Group: authz.GroupAgents, Page: "/executors", Object: idObject, Apply: remove},
		{Name: "executor.override", Action: authz.ActionChangeSettings, Group: authz.GroupAgents, Page: "/executors", Object: overrideObject, Apply: override},
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

func overrideObject(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
	var r overrideIn
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
	// 内容与最新的待确认或已生效版本相同：不产生新版本，本人不必再确认一次（附录 A.2）。
	// 与已生效版本相同时，插件已退回到生效的状态，仍待确认的旧版本随之作废（插件自动同步的开→关→又开回）。
	if cur, ok, err := record(c.Tx, d.Harness, 0, StatePending); err != nil {
		return nil, err
	} else if ok && sameDoc(cur.Declaration, d) {
		return ref{d.Harness, cur.Rev}, nil
	}
	if cur, ok, err := record(c.Tx, d.Harness, 0, StateActive); err != nil {
		return nil, err
	} else if ok && sameDoc(cur.Declaration, d) {
		_, err := c.Tx.Exec("UPDATE executors_declaration SET state = ? WHERE harness = ? AND state = ?", StateSuperseded, d.Harness, StatePending)
		return ref{d.Harness, cur.Rev}, err
	}
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

// ---- 随委派登记（§6.17） ----

var (
	enrollID  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	hostShape = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// Enrollment 是 delegate.create 随委派登记执行者时的输入：id 取 assignee，harness 取调用方报告的 harness。
type Enrollment struct {
	Kind   string          `json:"kind"`
	Host   string          `json:"host,omitempty"`
	Label  string          `json:"label,omitempty"`
	Handle json.RawMessage `json:"handle,omitempty"`
}

// Check 只按形状校验（不读账本），并把句柄规范化；Object 阶段调用，好让名单判定看到将要登记的 host。
func (in *Enrollment) Check(id string) error {
	if !enrollID.MatchString(id) {
		return invalid("executor id must match ^[a-z0-9][a-z0-9._-]{0,63}$ to be registered")
	}
	if !validKind(in.Kind) {
		return invalid("executor.kind must be native-subagent, external-cli, remote-agent or human")
	}
	if in.Host != "" && !hostShape.MatchString(in.Host) {
		return invalid("executor.host must be 1-64 characters of [A-Za-z0-9._-]")
	}
	if err := CheckValue("executor.label", in.Label, 80); err != nil {
		return err
	}
	h, err := ValidateHandle("executor.handle", in.Handle)
	in.Handle = h
	return err
}

// Enroll 在调用方的命令事务里登记或更新名录条目，并记 executor.enrolled。已在名录里时身份字段（harness、kind、host）
// 必须一致，否则冲突；label 与 handle 只有同一 harness 能改。调用方先 Check。
func Enroll(c *command.Ctx, id string, in Enrollment) error {
	harness := c.Caller.Actor.Harness
	if harness == "" {
		return invalid("registering an executor needs the caller's harness (APA_HARNESS / X-APA-Harness)")
	}
	cur, found, err := Get(c.Tx, id)
	if err != nil {
		return err
	}
	if found {
		if cur.Harness != harness || cur.Kind != in.Kind || cur.Host != in.Host {
			return domain.NewError(domain.CodeConflict, "executor "+id+" is already registered with a different harness, kind or host",
				"use another executor id, or ask the owner to remove the entry in Web")
		}
		if cur.Label == in.Label && bytes.Equal(cur.Handle, in.Handle) {
			return nil // 同一派法再次使用：不产生事件
		}
		if _, err := c.Tx.Exec("UPDATE executors_entry SET label = ?, handle = ? WHERE id = ?", in.Label, string(in.Handle), id); err != nil {
			return err
		}
	} else {
		var n int64
		if err := query(c.Tx, func(r *store.Rows) error { return r.Scan(&n) }, "SELECT COUNT(*) FROM executors_entry"); err != nil {
			return err
		}
		if n >= MaxEntries {
			return domain.NewError(domain.CodeConflict, "the executor list is full", "register one executor per way of delegating, not per agent; ask the owner to remove unused entries in Web")
		}
		if _, err := c.Tx.Exec(`INSERT INTO executors_entry (id, kind, scenarios, host, harness, label, handle, created_at) VALUES (?, ?, '[]', ?, ?, ?, ?, ?)`,
			id, in.Kind, in.Host, harness, in.Label, string(in.Handle), c.Now().UnixNano()); err != nil {
			return err
		}
	}
	e, _, err := Get(c.Tx, id)
	if err != nil {
		return err
	}
	_, err = c.Emit(domain.Event{Type: EvEnrolled, Object: "executors:" + id, Payload: payload(e)})
	return err
}

// listed 是 executor.list 的一项；Allowed 只在带 project 时出现。Startable 表示 APA 能按生效的无头运行声明直接拉起它
// （apa delegate --start），Overridable 是那份声明允许按次覆盖的参数与取值。
type listed struct {
	Entry
	Allowed     *bool               `json:"allowed,omitempty"`
	Startable   bool                `json:"startable,omitempty"`
	Overridable map[string]Override `json:"overridable,omitempty"`
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
		out[i].Entry = e
		if e.Harness != "" {
			if rec, ok, err := record(c.Tx, e.Harness, 0, StateActive); err != nil {
				return nil, err
			} else if ok && rec.Declaration.Headless != nil {
				out[i].Startable, out[i].Overridable = true, rec.Declaration.Overrides
			}
		}
		if proj != nil {
			ok := proj.ExecutorAllowed(domain.ExecutorRef{ID: e.ID, Host: e.Host, Harness: e.Harness}, o.Owner)
			out[i].Allowed = &ok
		}
	}
	return map[string]any{"executors": out}, nil
}
