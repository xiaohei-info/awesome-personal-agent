package connections

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/conn"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// record 是一行登记（不含现状与凭据）。
type record struct {
	ID, Direction, Type, Name string
	Config                    map[string]string
	Enabled                   bool
	CreatedAt, UpdatedAt      time.Time
}

func b2i(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func insert(tx *store.Tx, r record) error {
	cfg, err := json.Marshal(r.Config)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO connections_connection (id, direction, type, name, config, enabled, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.Direction, r.Type, r.Name, string(cfg), b2i(r.Enabled), r.CreatedAt.UnixNano(), r.UpdatedAt.UnixNano())
	return err
}

func update(tx *store.Tx, r record) error {
	cfg, err := json.Marshal(r.Config)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE connections_connection SET name = ?, config = ?, enabled = ?, updated_at = ? WHERE id = ?`,
		r.Name, string(cfg), b2i(r.Enabled), r.UpdatedAt.UnixNano(), r.ID)
	return err
}

func list(tx *store.Tx, where string, args ...any) ([]record, error) {
	rs, err := tx.Query(`SELECT id, direction, type, name, config, enabled, created_at, updated_at FROM connections_connection `+where+` ORDER BY created_at, id`, args...)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []record
	for rs.Next() {
		var (
			r      record
			cfg    string
			en     bool
			ca, ua int64
		)
		if err := rs.Scan(&r.ID, &r.Direction, &r.Type, &r.Name, &cfg, &en, &ca, &ua); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(cfg), &r.Config); err != nil {
			return nil, err
		}
		r.Enabled, r.CreatedAt, r.UpdatedAt = en, time.Unix(0, ca).UTC(), time.Unix(0, ua).UTC()
		out = append(out, r)
	}
	return out, rs.Err()
}

func get(tx *store.Tx, id string) (record, error) {
	rs, err := list(tx, "WHERE id = ?", id)
	if err != nil {
		return record{}, err
	}
	if len(rs) == 0 {
		return record{}, notFound(id)
	}
	return rs[0], nil
}

func isNotFound(err error) bool {
	e, ok := err.(*domain.Error)
	return ok && e.Code == domain.CodeNotFound
}

type status struct {
	problem, detail string
	ok, failed      int64
}

func readStatus(tx *store.Tx, id string) (status, error) {
	var s status
	rs, err := tx.Query(`SELECT problem, detail, last_ok_at, last_error_at FROM connections_status WHERE connection = ?`, id)
	if err != nil {
		return s, err
	}
	defer rs.Close()
	if rs.Next() {
		err = rs.Scan(&s.problem, &s.detail, &s.ok, &s.failed)
	}
	if err != nil {
		return s, err
	}
	return s, rs.Err()
}

func at(ns int64) *time.Time {
	if ns == 0 {
		return nil
	}
	t := time.Unix(0, ns).UTC()
	return &t
}

// view 把登记、现状与凭据可用性折成读接口的形状。必填凭据不可用时问题就是 credentials_missing（主密钥丢失或没填）。
func (s *Service) view(tx *store.Tx, r record) (Connection, error) {
	usable, err := s.v.Usable(tx, r.ID)
	if err != nil {
		return Connection{}, err
	}
	st, err := readStatus(tx, r.ID)
	if err != nil {
		return Connection{}, err
	}
	c := Connection{ID: r.ID, Direction: r.Direction, Type: r.Type, Name: r.Name, Config: r.Config, Secrets: map[string]bool{},
		Enabled: r.Enabled, Problem: st.problem, Detail: st.detail, LastOKAt: at(st.ok), LastErrorAt: at(st.failed), CreatedAt: r.CreatedAt, UpdatedAt: r.UpdatedAt}
	if d, ok := s.decl(r.Type); ok {
		for _, f := range d.Fields {
			if f.Kind != conn.KindSecret {
				continue
			}
			c.Secrets[f.Key] = usable[f.Key]
			if f.Required && !usable[f.Key] {
				c.Problem, c.Detail = conn.ProblemCredentialsMissing, ""
			}
		}
	} else {
		c.Problem, c.Detail = conn.ProblemConfigInvalid, "type not available in this build"
	}
	switch {
	case !c.Enabled:
		c.State = StateDisabled
	case c.Problem != "":
		c.State = StateProblem
	default:
		c.State = StateOK
	}
	return c, nil
}

// List 返回某方向的全部连接（direction 为空返回全部），按创建顺序。
func (s *Service) List(tx *store.Tx, direction string) ([]Connection, error) {
	where, args := "", []any{}
	if direction != "" {
		where, args = "WHERE direction = ?", []any{direction}
	}
	rs, err := list(tx, where, args...)
	if err != nil {
		return nil, err
	}
	out := make([]Connection, 0, len(rs))
	for _, r := range rs {
		c, err := s.view(tx, r)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// Get 按 id 读一个连接。
func (s *Service) Get(tx *store.Tx, id string) (Connection, error) {
	r, err := get(tx, id)
	if err != nil {
		return Connection{}, err
	}
	return s.view(tx, r)
}

// Target 是调用插件所需的连接事实（非凭据配置）。
func Target(c Connection) conn.Target {
	return conn.Target{ID: c.ID, Type: c.Type, Settings: c.Config}
}

// Deliverable 判断投递路由能否交给消费方：指向渠道连接（channel:ch-…）而连接已停用或已删除时为假，
// 投递直接落 Web 未读，不等无人领取（§2.3 的收紧）。其他路由（inject、wake、ext 渠道名）一律为真。
func Deliverable(tx *store.Tx, route string) (bool, error) {
	id, ok := strings.CutPrefix(route, "channel:")
	if !ok || !strings.HasPrefix(id, IDPrefix) {
		return true, nil
	}
	r, err := get(tx, id)
	if isNotFound(err) {
		return false, nil
	}
	return err == nil && r.Enabled, err
}

// SetStatus 覆盖写连接现状：problem 为空表示成功（清掉问题、记最近成功），否则记问题代码与不含凭据的细节。
// 不记 Event（与 doctor_seen 同类）；连接已删除时什么也不写。供渠道发送器在自己的事务里调用。
func SetStatus(tx *store.Tx, id, problem, detail string, now time.Time) error {
	if _, err := get(tx, id); isNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	if len(detail) > 500 {
		detail = strings.ToValidUTF8(detail[:500], "")
	}
	if problem == "" {
		_, err := tx.Exec(`INSERT INTO connections_status (connection, last_ok_at) VALUES (?, ?)
			ON CONFLICT(connection) DO UPDATE SET problem = '', detail = '', last_ok_at = excluded.last_ok_at`, id, now.UnixNano())
		return err
	}
	_, err := tx.Exec(`INSERT INTO connections_status (connection, problem, detail, last_error_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(connection) DO UPDATE SET problem = excluded.problem, detail = excluded.detail, last_error_at = excluded.last_error_at`,
		id, problem, detail, now.UnixNano())
	return err
}
