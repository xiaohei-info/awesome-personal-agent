package delegation

// 委派记录（设计 §6.17）：一次派出一条，挂在委派子事项上。宿主插件在派出工具调用后自动捕获写入，
// 认不出的派法由主会话或运行者显式写入；内核只存取、限长、校验结构，不解释、不执行句柄。
//
//	delegate.dispatch         写一条委派记录；同一子事项上已有 running、harness 与 session 都相同的记录时不新增，返回已有记录
//	delegate.dispatch_update  推进状态（ended、lost）或改 resumable；lost 同时置 resumable 为假，终态不可改
//
// 授权与回执同一口径：子事项绑定的凭据（assignee 绑定的 worker、发起该委派的凭据）是第一档，
// 范围内的运行凭据可写，其他 delegate 凭据按第二档。子事项进入 done、dropped 时，同一事务里把仍在运行的记录转 ended。

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 委派记录的状态。
const (
	DispatchRunning = "running"
	DispatchEnded   = "ended"
	DispatchLost    = "lost"
)

// 事件类型（内部，不在附录 B.4 的稳定类型里；对象是委派子事项）。
const (
	EvDispatched      = "delegation.dispatched"
	EvDispatchUpdated = "delegation.dispatch_updated"
)

// MaxDispatchesPerItem 是每个子事项的委派记录上限。
const MaxDispatchesPerItem = 50

// Dispatch 是一条委派记录。
type Dispatch struct {
	ID         string          `json:"id"`
	Item       string          `json:"item"`
	Executor   string          `json:"executor"`
	Harness    string          `json:"harness"`
	Session    string          `json:"session,omitempty"`
	Resumable  bool            `json:"resumable"`
	Cwd        string          `json:"cwd,omitempty"`
	Repo       string          `json:"repo,omitempty"`
	Handle     json.RawMessage `json:"handle,omitempty"`
	Continues  string          `json:"continues,omitempty"`
	State      string          `json:"state"`
	Unattended bool            `json:"unattended,omitempty"`
	StartedAt  time.Time       `json:"started_at"`
	EndedAt    *time.Time      `json:"ended_at,omitempty"`
	// Run 是 APA 自己拉起这次派出时的运行 id（路径 A）；harness 自己派出（路径 B）为空。
	Run string `json:"run,omitempty"`
	// Origin 是记下这次派出的会话（调用方的 APA_SESSION）；未挂事项的派活只在这个会话的注入里提示。
	Origin string `json:"origin,omitempty"`
	// About 是派活说明里提到、但不是委派子事项的事项 id：未挂事项时作为"挂到哪里"的提示，内核不据它挂靠。
	About string `json:"about,omitempty"`
}

// Attached 报告这次派出是否已挂在委派子事项上；未挂的派活 Item 为空（§6.17）。
func (d Dispatch) Attached() bool { return d.Item != "" }

var dispatchMigration = store.Migration{
	// 账本结构版本 5（§6.17）：委派记录。
	Version: 2, Ledger: 5, Name: "ledger-v5-dispatch",
	SQL: `CREATE TABLE delegation_dispatch (
	id TEXT PRIMARY KEY,
	item TEXT NOT NULL,
	executor TEXT NOT NULL,
	harness TEXT NOT NULL,
	session TEXT NOT NULL,
	resumable INTEGER NOT NULL,
	cwd TEXT NOT NULL,
	repo TEXT NOT NULL,
	handle TEXT NOT NULL,
	continues TEXT NOT NULL,
	state TEXT NOT NULL,
	unattended INTEGER NOT NULL,
	started_at INTEGER NOT NULL,
	ended_at INTEGER NOT NULL
) WITHOUT ROWID;
CREATE INDEX delegation_dispatch_item ON delegation_dispatch(item, started_at);
CREATE INDEX delegation_dispatch_executor ON delegation_dispatch(executor, started_at);`,
}

var dispatchRunMigration = store.Migration{
	// 账本结构版本 14（§6.17）：APA 拉起的运行、记下派出的会话、未挂事项时说明里提到的事项。只增列，已有记录取空值。
	Version: 3, Ledger: 14, Name: "ledger-v14-dispatch-run",
	SQL: `ALTER TABLE delegation_dispatch ADD COLUMN run TEXT NOT NULL DEFAULT '';
ALTER TABLE delegation_dispatch ADD COLUMN origin TEXT NOT NULL DEFAULT '';
ALTER TABLE delegation_dispatch ADD COLUMN about TEXT NOT NULL DEFAULT '';
CREATE INDEX delegation_dispatch_session ON delegation_dispatch(session);`,
}

type dispatchIn struct {
	ID        string          `json:"id"` // 子事项；为空是未挂事项的派活，须带 assignee
	Session   string          `json:"session"`
	Resumable bool            `json:"resumable"`
	Cwd       string          `json:"cwd"`
	Repo      string          `json:"repo"`
	Handle    json.RawMessage `json:"handle"`
	Continues string          `json:"continues"`
	// Assignee、Executor 与 About 只用于未挂事项的派活：派给谁（随派活登记名录条目，同 delegate.create）、说明里提到的事项。
	Assignee string                `json:"assignee,omitempty"`
	Executor *executors.Enrollment `json:"executor,omitempty"`
	About    string                `json:"about,omitempty"`
}

type dispatchUpdateIn struct {
	ID string `json:"id"` // 委派记录
	// Session 代替 id：调用方 harness 用这个会话句柄最近记下的那条（宿主只知道句柄，比如续派时发现会话已失效）。
	Session   string `json:"session,omitempty"`
	State     string `json:"state"`
	Resumable *bool  `json:"resumable"`
}

func (v *dispatchIn) check() error {
	for _, f := range []struct {
		path, val string
		max       int
	}{{"session", v.Session, 128}, {"cwd", v.Cwd, 256}, {"repo", v.Repo, 256}} {
		if err := executors.CheckValue(f.path, f.val, f.max); err != nil {
			return err
		}
	}
	if v.Resumable && blank(v.Session) {
		return invalid("a resumable dispatch needs its session")
	}
	if blank(v.ID) && blank(v.Assignee) {
		return invalid("give id (the delegated item) or, for a dispatch not yet attached to an item, assignee (with optional executor and about)")
	}
	for _, id := range []string{v.ID, v.About} {
		if id != "" && !strings.HasPrefix(id, "itm_") {
			return invalid("id and about must be item ids")
		}
	}
	if v.Executor != nil {
		if err := v.Executor.Check(v.Assignee); err != nil {
			return err
		}
	}
	h, err := executors.ValidateHandle("handle", v.Handle)
	v.Handle = h
	return err
}

func newDispatchID() (string, error) {
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "dsp_" + hex.EncodeToString(b), nil
}

const dispatchCols = "id, item, executor, harness, session, resumable, cwd, repo, handle, continues, state, unattended, started_at, ended_at, run, origin, about"

func scanDispatch(r *store.Rows) (Dispatch, error) {
	var d Dispatch
	var handle string
	var started, ended int64
	if err := r.Scan(&d.ID, &d.Item, &d.Executor, &d.Harness, &d.Session, &d.Resumable, &d.Cwd, &d.Repo, &handle,
		&d.Continues, &d.State, &d.Unattended, &started, &ended, &d.Run, &d.Origin, &d.About); err != nil {
		return d, err
	}
	if handle != "" {
		d.Handle = json.RawMessage(handle)
	}
	d.StartedAt = time.Unix(0, started).UTC()
	if ended > 0 {
		t := time.Unix(0, ended).UTC()
		d.EndedAt = &t
	}
	return d, nil
}

func queryDispatches(tx *store.Tx, where string, args ...any) ([]Dispatch, error) {
	r, err := tx.Query("SELECT "+dispatchCols+" FROM delegation_dispatch "+where, args...)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := []Dispatch{}
	for r.Next() {
		d, err := scanDispatch(r)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, r.Err()
}

// GetDispatch 读一条委派记录。
func GetDispatch(tx *store.Tx, id string) (Dispatch, error) {
	ds, err := queryDispatches(tx, "WHERE id = ?", id)
	if err != nil {
		return Dispatch{}, err
	}
	if len(ds) == 0 {
		return Dispatch{}, domain.NewError(domain.CodeNotFound, "dispatch "+id+" does not exist", "list the item's dispatches and retry")
	}
	return ds[0], nil
}

// Dispatches 返回事项上的委派记录，按开始时间倒序。
func Dispatches(tx *store.Tx, item string) ([]Dispatch, error) {
	return queryDispatches(tx, "WHERE item = ? ORDER BY started_at DESC, id", item)
}

// Continuing 返回接续 id 的委派记录（continues = id）。
func Continuing(tx *store.Tx, id string) ([]Dispatch, error) {
	return queryDispatches(tx, "WHERE continues = ? ORDER BY started_at DESC, id", id)
}

// DispatchFilter 是 ListDispatches 的条件；空字段不过滤。
type DispatchFilter struct {
	Executor  string
	Item      string
	Resumable *bool
	Limit     int
}

// ListDispatches 按条件返回委派记录，按开始时间倒序；Limit 缺省 50、至多 200。
func ListDispatches(tx *store.Tx, f DispatchFilter) ([]Dispatch, error) {
	where, args := []string{}, []any{}
	if f.Executor != "" {
		where, args = append(where, "executor = ?"), append(args, f.Executor)
	}
	if f.Item != "" {
		where, args = append(where, "item = ?"), append(args, f.Item)
	}
	if f.Resumable != nil {
		where, args = append(where, "resumable = ?"), append(args, *f.Resumable)
	}
	limit := f.Limit
	if limit <= 0 {
		limit = 50
	}
	limit = min(limit, 200)
	q := ""
	if len(where) > 0 {
		q = "WHERE " + strings.Join(where, " AND ") + " "
	}
	return queryDispatches(tx, q+"ORDER BY started_at DESC, id LIMIT ?", append(args, limit)...)
}

func putDispatch(tx *store.Tx, d Dispatch) error {
	var ended int64
	if d.EndedAt != nil {
		ended = d.EndedAt.UnixNano()
	}
	_, err := tx.Exec(`INSERT INTO delegation_dispatch (`+dispatchCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET item = excluded.item, session = excluded.session, resumable = excluded.resumable,
		state = excluded.state, ended_at = excluded.ended_at`,
		d.ID, d.Item, d.Executor, d.Harness, d.Session, d.Resumable, d.Cwd, d.Repo, string(d.Handle), d.Continues, d.State,
		d.Unattended, d.StartedAt.UnixNano(), ended, d.Run, d.Origin, d.About)
	return err
}

func dispatchPayload(d Dispatch) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"dispatch": d})
	return b
}

// dispatchable 读出可以写委派记录的子事项：委派子事项、未关闭、assignee 不是本人。
func (o Options) dispatchable(tx *store.Tx, id string) (domain.Item, error) {
	if blank(id) {
		return domain.Item{}, invalid("item id is required")
	}
	it, err := items.Get(tx, id)
	if err != nil {
		return it, err
	}
	if it.Kind != domain.KindDelegation {
		return it, invalid("dispatches are recorded on delegated items; run apa delegate first")
	}
	if it.Assignee == o.Owner {
		return it, invalid("the item is assigned to the owner; nothing is dispatched")
	}
	switch it.Phase {
	case domain.PhaseOpen, domain.PhaseWaiting, domain.PhaseNeedsInput:
		return it, nil
	}
	return it, conflict("the item is already "+string(it.Phase)+"; dispatches are only recorded on live items", "re-read the item")
}

func itemObject(it domain.Item) authz.Object {
	return authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Project: it.Project}
}

func (o Options) dispatch() command.Spec {
	parse := func(in json.RawMessage) (dispatchIn, error) {
		var v dispatchIn
		if err := decode(in, &v); err != nil {
			return v, err
		}
		return v, v.check()
	}
	return command.Spec{Name: "delegate.dispatch", Action: authz.ActionUpdateOwn, Group: authz.GroupOrganize, Receipt: true, KeepVersion: true,
		// 未挂事项的派活没有授权对象：只是记下一次已经发生的派出，与提建议同一口径（§6.17）。
		Actions: func(_ *store.Tx, _ command.Caller, o authz.Object, _ json.RawMessage) []authz.Action {
			if o.ID == "" {
				return []authz.Action{authz.ActionCreateProposed}
			}
			return []authz.Action{authz.ActionUpdateOwn}
		},
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			if v, err = o.target(tx, v); err != nil || v.ID == "" {
				return authz.Object{}, err
			}
			it, err := o.dispatchable(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			return itemObject(it), nil
		},
		Validate: func(tx *store.Tx, obj authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			if v.Continues == "" {
				return nil
			}
			prev, err := GetDispatch(tx, v.Continues)
			if err != nil {
				if de, ok := err.(*domain.Error); ok && de.Code == domain.CodeNotFound {
					return invalid("continues refers to a dispatch that does not exist")
				}
				return err
			}
			if !prev.Attached() || v.ID == "" {
				return invalid("continues only links dispatches recorded on delegated items")
			}
			pit, err := items.Get(tx, prev.Item)
			if err != nil {
				return err
			}
			if pit.Project != obj.Project {
				return invalid("continues must refer to a dispatch in the same project")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			harness := c.Caller.Actor.Harness
			if harness == "" {
				return nil, invalid("recording a dispatch needs the caller's harness (APA_HARNESS / X-APA-Harness)")
			}
			v, err := o.target(c.Tx, v)
			if err != nil {
				return nil, err
			}
			// 同一会话再派一次（续派）：接到它已挂的事项上，记成上一次的接续（§6.17）。
			if v.ID == "" && v.Session != "" {
				if prev, ok, err := BySession(c.Tx, harness, v.Session); err != nil {
					return nil, err
				} else if ok && prev.Attached() {
					v.ID, v.Continues = prev.Item, prev.ID
				}
			}
			if v.ID == "" {
				return o.unattached(c, v, harness)
			}
			it, err := items.Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			have, err := Dispatches(c.Tx, it.ID)
			if err != nil {
				return nil, err
			}
			// 同会话去重：插件自动捕获与模型手动补记撞在同一次派出上时只留一条（§6.17）。
			for _, d := range have {
				if d.State == DispatchRunning && d.Harness == harness && d.Session == v.Session {
					return d, nil
				}
			}
			if len(have) >= MaxDispatchesPerItem {
				return nil, conflict("the item already has the maximum number of dispatches", "update an existing dispatch instead of recording a new one")
			}
			d := Dispatch{Item: it.ID, Executor: it.Assignee, Harness: harness, Session: v.Session, Resumable: v.Resumable,
				Cwd: v.Cwd, Repo: v.Repo, Handle: v.Handle, Continues: v.Continues, State: DispatchRunning,
				Unattended: c.Caller.Level == authz.LevelRun || it.Unattended, Origin: c.Caller.Actor.Session}
			return d, Record(c, &d)
		}}
}

// target 定这次派出记在哪里：id 是未关闭的委派子事项就记在它上面；否则（宿主从派活说明里认出的是父事项或别的事项）
// 带 assignee 时按未挂事项的派活记，id 留作"说明里提到的事项"。内核不据它挂靠，挂到哪里由 Agent 定。
func (o Options) target(tx *store.Tx, v dispatchIn) (dispatchIn, error) {
	if v.ID == "" || v.Assignee == "" {
		return v, nil
	}
	it, err := items.Get(tx, v.ID)
	if de, ok := err.(*domain.Error); ok && de.Code == domain.CodeNotFound {
		v.ID = ""
		return v, nil
	}
	if err != nil {
		return v, err
	}
	if it.Kind == domain.KindDelegation && it.Assignee == v.Assignee && !it.Phase.Terminal() {
		return v, nil
	}
	v.About, v.ID = v.ID, ""
	return v, nil
}

// Record 在调用方的命令事务里写一条新的委派记录并记 delegation.dispatched（插件捕获、显式补记、APA 自己拉起的运行共用）。
// d.ID 与 StartedAt 由这里填。
func Record(c *command.Ctx, d *Dispatch) error {
	id, err := newDispatchID()
	if err != nil {
		return err
	}
	d.ID, d.StartedAt = id, c.Now()
	if err := putDispatch(c.Tx, *d); err != nil {
		return err
	}
	object := d.Item
	if object == "" {
		object = unattachedObject
	}
	_, err = c.Emit(domain.Event{Type: EvDispatched, Object: object, Reason: "dispatched to " + d.Executor, Payload: dispatchPayload(*d)})
	return err
}

// unattachedObject 是未挂事项的派活事件的对象：它们不属于任何事项，挂上之后的事件对象是子事项。
const unattachedObject = "delegation:unattached"

// MaxUnattached 是同时在跟的未挂事项派活上限：超过说明插件在把每次调用都当派活，拒绝而不是无限堆积提示。
const MaxUnattached = 50

// unattached 记一次还没挂到事项上的派出（§6.17）：之后每轮注入在记下它的会话里提示，直到挂上或标为不跟踪。
func (o Options) unattached(c *command.Ctx, v dispatchIn, harness string) (any, error) {
	if v.Assignee == o.Owner {
		return nil, invalid("the assignee is the owner; nothing is dispatched")
	}
	running, err := queryDispatches(c.Tx, "WHERE item = '' AND state = ?", DispatchRunning)
	if err != nil {
		return nil, err
	}
	for _, d := range running {
		if d.Harness == harness && d.Session == v.Session && d.Executor == v.Assignee && (v.Session != "" || d.Origin == c.Caller.Actor.Session) {
			return d, nil
		}
	}
	if len(running) >= MaxUnattached {
		return nil, conflict("too many dispatches are waiting to be attached to an item", "attach them with apa dispatch attach, or mark them untracked")
	}
	if v.Executor != nil {
		if err := executors.Enroll(c, v.Assignee, *v.Executor); err != nil {
			return nil, err
		}
	}
	d := Dispatch{Executor: v.Assignee, Harness: harness, Session: v.Session, Resumable: v.Resumable, Cwd: v.Cwd, Repo: v.Repo,
		Handle: v.Handle, State: DispatchRunning, Unattended: c.Caller.Level == authz.LevelRun, Origin: c.Caller.Actor.Session, About: v.About}
	return d, Record(c, &d)
}

// ByRun 返回 APA 拉起的运行对应的委派记录；不是委派即开工的运行 ok 为假。
func ByRun(tx *store.Tx, runID string) (Dispatch, bool, error) {
	ds, err := queryDispatches(tx, "WHERE run = ? LIMIT 1", runID)
	if err != nil || len(ds) == 0 {
		return Dispatch{}, false, err
	}
	return ds[0], true, nil
}

// FinishRun 在 APA 拉起的运行结束时收尾它的委派记录：补上运行结束才知道的会话句柄，能续接时标可续接，
// 仍在运行的转 ended（进程没了，这一次派出就结束了），并记 delegation.dispatch_updated。由 kernel/background 在自己的收尾事务里调用。
func FinishRun(tx *store.Tx, runID, session string, resumable bool, at time.Time, actor domain.Actor, depth int) error {
	ds, err := queryDispatches(tx, "WHERE run = ?", runID)
	if err != nil {
		return err
	}
	for _, d := range ds {
		before := d
		if session != "" && d.Session == "" {
			d.Session = session
		}
		if resumable && d.Session != "" && d.State != DispatchLost {
			d.Resumable = true
		}
		if d.State == DispatchRunning {
			d.State, d.EndedAt = DispatchEnded, &at
		}
		if d.Session == before.Session && d.Resumable == before.Resumable && d.State == before.State {
			continue
		}
		if err := putDispatch(tx, d); err != nil {
			return err
		}
		if _, err := eventlog.Append(tx, domain.Event{Type: EvDispatchUpdated, Object: d.Item, Actor: actor, Reason: "run " + runID + " finished",
			Payload: dispatchPayload(d), ChainDepth: depth, At: at}); err != nil {
			return err
		}
	}
	return nil
}

// Unattached 返回仍在跟、还没挂到事项上的派活，按开始时间倒序。
func Unattached(tx *store.Tx) ([]Dispatch, error) {
	return queryDispatches(tx, "WHERE item = '' AND state = ? ORDER BY started_at DESC, id", DispatchRunning)
}

// BySession 返回某会话句柄最近一次派出的记录（harness 为空不限）；回执按句柄查事项用它。
func BySession(tx *store.Tx, harness, session string) (Dispatch, bool, error) {
	ds, err := queryDispatches(tx, "WHERE session = ? AND (? = '' OR harness = ?) ORDER BY started_at DESC, id LIMIT 1", session, harness, harness)
	if err != nil || len(ds) == 0 {
		return Dispatch{}, false, err
	}
	return ds[0], true, nil
}

func (o Options) dispatchUpdate() command.Spec {
	parse := func(in json.RawMessage) (dispatchUpdateIn, error) {
		var v dispatchUpdateIn
		if err := decode(in, &v); err != nil {
			return v, err
		}
		if blank(v.ID) == blank(v.Session) {
			return v, invalid("give the dispatch id or its session handle")
		}
		if v.State != "" && v.State != DispatchEnded && v.State != DispatchLost {
			return v, invalid("state must be ended or lost")
		}
		if v.State == "" && v.Resumable == nil {
			return v, invalid("give state and/or resumable")
		}
		return v, nil
	}
	return command.Spec{Name: "delegate.dispatch_update", Action: authz.ActionUpdateOwn, Group: authz.GroupOrganize, Receipt: true, KeepVersion: true,
		// 未挂事项的派活：标为不跟踪（state ended）或失效，与记下它同一口径。
		Actions: func(_ *store.Tx, _ command.Caller, o authz.Object, _ json.RawMessage) []authz.Action {
			if o.ID == "" {
				return []authz.Action{authz.ActionCreateProposed}
			}
			return []authz.Action{authz.ActionUpdateOwn}
		},
		// 按句柄只认调用方 harness 自己写的记录。
		Refine: func(tx *store.Tx, c command.Caller, _ *authz.Object, in json.RawMessage) error {
			v, err := parse(in)
			if err != nil || v.Session == "" {
				return err
			}
			if _, ok, err := BySession(tx, c.Actor.Harness, v.Session); err != nil || !ok {
				if err == nil {
					err = domain.NewError(domain.CodeNotFound, "no dispatch of your harness was recorded with this session handle", "use the dispatch id")
				}
				return err
			}
			return nil
		},
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			d, err := updateTarget(tx, v)
			if err != nil || !d.Attached() {
				return authz.Object{}, err
			}
			it, err := items.Get(tx, d.Item)
			if err != nil {
				return authz.Object{}, err
			}
			return itemObject(it), nil
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parse(in)
			d, err := updateTarget(tx, v)
			if err != nil {
				return err
			}
			if d.State != DispatchRunning && v.State != "" && v.State != d.State {
				return conflict("the dispatch is already "+d.State, "record a new dispatch instead")
			}
			if d.State == DispatchLost && v.Resumable != nil && *v.Resumable {
				return conflict("a lost session cannot become resumable", "record a new dispatch instead")
			}
			if v.Resumable != nil && *v.Resumable && blank(d.Session) {
				return invalid("a resumable dispatch needs its session")
			}
			return nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			d, err := updateTarget(c.Tx, v)
			if err != nil {
				return nil, err
			}
			before := d
			if v.Resumable != nil {
				d.Resumable = *v.Resumable
			}
			if v.State != "" && d.State == DispatchRunning {
				now := c.Now()
				d.State, d.EndedAt = v.State, &now
			}
			if d.State == DispatchLost {
				d.Resumable = false
			}
			if d.State == before.State && d.Resumable == before.Resumable {
				return d, nil
			}
			if err := putDispatch(c.Tx, d); err != nil {
				return nil, err
			}
			object, reason := d.Item, "dispatch "+d.State
			if !d.Attached() {
				object, reason = unattachedObject, "dispatch untracked"
			}
			_, err = c.Emit(domain.Event{Type: EvDispatchUpdated, Object: object, Reason: reason, Payload: dispatchPayload(d)})
			return d, err
		}}
}

// updateTarget 是 dispatch_update 要改的那条：按 id，或按会话句柄取最近一条。
func updateTarget(tx *store.Tx, v dispatchUpdateIn) (Dispatch, error) {
	if v.ID != "" {
		return GetDispatch(tx, v.ID)
	}
	d, ok, err := BySession(tx, "", v.Session)
	if err == nil && !ok {
		err = domain.NewError(domain.CodeNotFound, "no dispatch was recorded with this session handle", "list the item's dispatches and retry")
	}
	return d, err
}

type dispatchListIn struct {
	Executor  string `json:"executor"`
	Item      string `json:"item"`
	Resumable *bool  `json:"resumable"`
	Limit     int    `json:"limit"`
}

// dispatchList 是只读命令：列委派记录（Web 的执行者视图、apa 读取）。
func dispatchList() command.Spec {
	return command.Spec{Name: "delegate.dispatch_list", Action: authz.ActionRead, Group: authz.GroupView,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v dispatchListIn
			return authz.Object{}, decode(in, &v)
		},
		Apply: func(c *command.Ctx) (any, error) {
			var v dispatchListIn
			_ = decode(c.Input, &v)
			ds, err := ListDispatches(c.Tx, DispatchFilter(v))
			return map[string]any{"dispatches": ds}, err
		}}
}

// closeDispatches 在同一事务里把终态子事项仍在运行的委派记录转 ended（§6.17）。由 InTx 回调对本命令写到的事项调用。
func closeDispatches(c *command.Ctx) error {
	seen := map[string]bool{}
	for _, e := range c.Events() {
		if seen[e.Object] || !strings.HasPrefix(e.Object, "itm_") {
			continue
		}
		seen[e.Object] = true
		running, err := queryDispatches(c.Tx, "WHERE item = ? AND state = ?", e.Object, DispatchRunning)
		if err != nil || len(running) == 0 {
			if err != nil {
				return err
			}
			continue
		}
		it, err := items.Get(c.Tx, e.Object)
		if err != nil {
			return err
		}
		if it.Phase != domain.PhaseDone && it.Phase != domain.PhaseDropped {
			continue
		}
		for _, d := range running {
			now := c.Now()
			d.State, d.EndedAt = DispatchEnded, &now
			if err := putDispatch(c.Tx, d); err != nil {
				return err
			}
			if _, err := c.Emit(domain.Event{Type: EvDispatchUpdated, Object: d.Item, Reason: "item " + string(it.Phase), Payload: dispatchPayload(d)}); err != nil {
				return err
			}
		}
	}
	return nil
}

// Purge 删除事项的委派记录与 PR 关联键（物理删除，§6.15）；事件由调用方墓碑化。
func Purge(tx *store.Tx, item string) error {
	if _, err := tx.Exec("DELETE FROM delegation_dispatch WHERE item = ?", item); err != nil {
		return err
	}
	_, err := tx.Exec("DELETE FROM delegation_link WHERE item = ?", item)
	return err
}
