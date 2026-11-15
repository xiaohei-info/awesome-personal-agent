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
}

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

type dispatchIn struct {
	ID        string          `json:"id"` // 子事项
	Session   string          `json:"session"`
	Resumable bool            `json:"resumable"`
	Cwd       string          `json:"cwd"`
	Repo      string          `json:"repo"`
	Handle    json.RawMessage `json:"handle"`
	Continues string          `json:"continues"`
}

type dispatchUpdateIn struct {
	ID        string `json:"id"` // 委派记录
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

const dispatchCols = "id, item, executor, harness, session, resumable, cwd, repo, handle, continues, state, unattended, started_at, ended_at"

func scanDispatch(r *store.Rows) (Dispatch, error) {
	var d Dispatch
	var handle string
	var started, ended int64
	if err := r.Scan(&d.ID, &d.Item, &d.Executor, &d.Harness, &d.Session, &d.Resumable, &d.Cwd, &d.Repo, &handle,
		&d.Continues, &d.State, &d.Unattended, &started, &ended); err != nil {
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
	_, err := tx.Exec(`INSERT INTO delegation_dispatch (`+dispatchCols+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET resumable = excluded.resumable, state = excluded.state, ended_at = excluded.ended_at`,
		d.ID, d.Item, d.Executor, d.Harness, d.Session, d.Resumable, d.Cwd, d.Repo, string(d.Handle), d.Continues, d.State,
		d.Unattended, d.StartedAt.UnixNano(), ended)
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
	return authz.Object{ID: it.ID, Assignee: it.Assignee, Kind: string(it.Kind), Tags: it.Tags, Project: it.Project}
}

func (o Options) dispatch() command.Spec {
	parse := func(in json.RawMessage) (dispatchIn, error) {
		var v dispatchIn
		if err := decode(in, &v); err != nil {
			return v, err
		}
		return v, v.check()
	}
	return command.Spec{Name: "delegate.dispatch", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Bound: bound, Receipt: true, KeepVersion: true,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
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
			id, err := newDispatchID()
			if err != nil {
				return nil, err
			}
			d := Dispatch{ID: id, Item: it.ID, Executor: it.Assignee, Harness: harness, Session: v.Session, Resumable: v.Resumable,
				Cwd: v.Cwd, Repo: v.Repo, Handle: v.Handle, Continues: v.Continues, State: DispatchRunning,
				Unattended: c.Caller.Level == authz.LevelRun || it.Unattended, StartedAt: c.Now()}
			if err := putDispatch(c.Tx, d); err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: EvDispatched, Object: it.ID, Reason: "dispatched to " + it.Assignee, Payload: dispatchPayload(d)})
			return d, err
		}}
}

func (o Options) dispatchUpdate() command.Spec {
	parse := func(in json.RawMessage) (dispatchUpdateIn, error) {
		var v dispatchUpdateIn
		if err := decode(in, &v); err != nil {
			return v, err
		}
		if v.State != "" && v.State != DispatchEnded && v.State != DispatchLost {
			return v, invalid("state must be ended or lost")
		}
		if v.State == "" && v.Resumable == nil {
			return v, invalid("give state and/or resumable")
		}
		return v, nil
	}
	return command.Spec{Name: "delegate.dispatch_update", Action: authz.ActionUpdateOwn, Tier: authz.TierDecide, Bound: bound, Receipt: true, KeepVersion: true,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			d, err := GetDispatch(tx, v.ID)
			if err != nil {
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
			d, err := GetDispatch(tx, v.ID)
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
			d, err := GetDispatch(c.Tx, v.ID)
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
			_, err = c.Emit(domain.Event{Type: EvDispatchUpdated, Object: d.Item, Reason: "dispatch " + d.State, Payload: dispatchPayload(d)})
			return d, err
		}}
}

type dispatchListIn struct {
	Executor  string `json:"executor"`
	Item      string `json:"item"`
	Resumable *bool  `json:"resumable"`
	Limit     int    `json:"limit"`
}

// dispatchList 是只读命令：列委派记录（Web 的执行者视图、apa 读取）。运行凭据只能按范围内的事项读。
func dispatchList() command.Spec {
	return command.Spec{Name: "delegate.dispatch_list", Action: authz.ActionRead,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			var v dispatchListIn
			return authz.Object{}, decode(in, &v)
		},
		Apply: func(c *command.Ctx) (any, error) {
			var v dispatchListIn
			_ = decode(c.Input, &v)
			if c.Caller.Level == authz.LevelRun {
				ok := false
				if v.Item != "" && c.Caller.RunScope != nil {
					var err error
					if ok, _, err = c.Caller.RunScope(c.Tx, authz.Object{ID: v.Item}); err != nil {
						return nil, err
					}
				}
				if !ok {
					return nil, domain.NewError(domain.CodePermissionDenied, "a run can only read dispatches of items in its scope", "pass item= with the run's target or one of its children")
				}
			}
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
