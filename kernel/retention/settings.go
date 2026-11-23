package retention

import (
	"bytes"
	"encoding/json"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块的迁移：三项保留天数（只存本人改过的，没改过的走配置与默认值）和最近一次清理的报数。
var Module = store.Module{Name: "retention", Migrations: []store.Migration{{
	Version: 1, Ledger: 9, Name: "ledger-v9-retention-settings",
	SQL: `CREATE TABLE retention_setting (
	key TEXT PRIMARY KEY,
	days INTEGER NOT NULL
) WITHOUT ROWID;
CREATE TABLE retention_sweep (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	at INTEGER NOT NULL,
	report TEXT NOT NULL
);`,
}}}

const (
	// CmdSet 改保留天数（只 Web）；CmdReport 记一次清理的报数（维护循环）。
	CmdSet    = "retention.set"
	CmdReport = "retention.report"
	// EvChanged、EvSwept 是它们的审计事件（不在对外事件类型表里）。
	EvChanged = "retention.changed"
	EvSwept   = "retention.swept"
	object    = "retention"

	maxDays                        = 3650
	minJudgmentDays                = 30
	keyRun, keyIngest, keyJudgment = "run_dirs", "ingest_bodies", "judgments"
)

// Settings 是三项保留天数，0 表示不清理。
type Settings struct {
	RunDays      int `json:"run_days"`      // 运行目录：到期整个删除
	IngestDays   int `json:"ingest_days"`   // 请求体：到期只清正文，留元数据
	JudgmentDays int `json:"judgment_days"` // 判断记录：到期整条删除
}

// overrides 返回本人在设置里改过的项（键 → 天数）。
func overrides(tx *store.Tx) (map[string]int, error) {
	r, err := tx.Query(`SELECT key, days FROM retention_setting`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	out := map[string]int{}
	for r.Next() {
		var k string
		var d int64
		if err := r.Scan(&k, &d); err != nil {
			return nil, err
		}
		out[k] = int(d)
	}
	return out, r.Err()
}

// Effective 返回生效的保留天数：账本里本人改过的优先，其余用 def（配置或默认值）。
func Effective(tx *store.Tx, def Settings) (Settings, error) {
	ov, err := overrides(tx)
	if err != nil {
		return def, err
	}
	for k, p := range map[string]*int{keyRun: &def.RunDays, keyIngest: &def.IngestDays, keyJudgment: &def.JudgmentDays} {
		if d, ok := ov[k]; ok {
			*p = d
		}
	}
	return def, nil
}

// Last 返回最近一次清理的报数；从没清理过返回 nil。
func Last(tx *store.Tx) (*Report, error) {
	r, err := tx.Query(`SELECT report FROM retention_sweep WHERE id = 1`)
	if err != nil {
		return nil, err
	}
	defer r.Close()
	if !r.Next() {
		return nil, r.Err()
	}
	var raw string
	if err := r.Scan(&raw); err != nil {
		return nil, err
	}
	var rep Report
	if err := json.Unmarshal([]byte(raw), &rep); err != nil {
		return nil, err
	}
	return &rep, nil
}

// setIn 的字段缺省表示不改这一项。
type setIn struct {
	RunDays      *int `json:"run_days"`
	IngestDays   *int `json:"ingest_days"`
	JudgmentDays *int `json:"judgment_days"`
}

func invalid(why, fix string) error { return domain.NewError(domain.CodeInvalidRequest, why, fix) }

func decodeSet(raw json.RawMessage) (setIn, error) {
	var in setIn
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return in, invalid("input must be {\"run_days\", \"ingest_days\", \"judgment_days\"} (any subset of integers)", "fix the request and retry")
	}
	if in.RunDays == nil && in.IngestDays == nil && in.JudgmentDays == nil {
		return in, invalid("nothing to change", "send at least one of run_days, ingest_days, judgment_days")
	}
	for _, c := range []struct {
		name string
		v    *int
		min  int
	}{{"run_days", in.RunDays, 1}, {"ingest_days", in.IngestDays, 1}, {"judgment_days", in.JudgmentDays, minJudgmentDays}} {
		if c.v != nil && *c.v != 0 && (*c.v < c.min || *c.v > maxDays) {
			return in, invalid(c.name+" must be 0 (never clear) or between "+itoa(c.min)+" and "+itoa(maxDays), "fix the value and retry")
		}
	}
	return in, nil
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }

// Register 登记 retention.set 与 retention.report；def 是没改过时生效的值（配置或默认）。
func Register(x *command.Executor, blobDir string, def Settings) error {
	if err := registerContent(x, blobDir); err != nil {
		return err
	}
	if err := x.Register(command.Spec{
		Name: CmdSet, Action: authz.ActionChangeSettings, Tier: authz.TierNone,
		Object:   func(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{ID: object}, nil },
		Validate: func(_ *store.Tx, _ authz.Object, in json.RawMessage) error { _, err := decodeSet(in); return err },
		Apply: func(c *command.Ctx) (any, error) {
			in, err := decodeSet(c.Input)
			if err != nil {
				return nil, err
			}
			for k, v := range map[string]*int{keyRun: in.RunDays, keyIngest: in.IngestDays, keyJudgment: in.JudgmentDays} {
				if v == nil {
					continue
				}
				if _, err := c.Tx.Exec(`INSERT INTO retention_setting (key, days) VALUES (?, ?)
					ON CONFLICT(key) DO UPDATE SET days = excluded.days`, k, *v); err != nil {
					return nil, err
				}
			}
			now, err := Effective(c.Tx, def)
			if err != nil {
				return nil, err
			}
			payload, _ := json.Marshal(now)
			if _, err := c.Emit(domain.Event{Type: EvChanged, Object: object, Payload: payload}); err != nil {
				return nil, err
			}
			return now, nil
		},
	}); err != nil {
		return err
	}
	return x.Register(command.Spec{
		Name: CmdReport, Action: authz.ActionUpdateOwn,
		Object: func(*store.Tx, json.RawMessage) (authz.Object, error) {
			return authz.Object{ID: object, Assignee: system.Actor.Principal}, nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			if !json.Valid(c.Input) {
				return nil, invalid("report is not valid JSON", "send the sweep report")
			}
			if _, err := c.Tx.Exec(`INSERT INTO retention_sweep (id, at, report) VALUES (1, ?, ?)
				ON CONFLICT(id) DO UPDATE SET at = excluded.at, report = excluded.report`, c.Now().UnixNano(), string(c.Input)); err != nil {
				return nil, err
			}
			_, err := c.Emit(domain.Event{Type: EvSwept, Object: object, Payload: c.Input})
			return nil, err
		},
	})
}

func days(n int) time.Duration { return time.Duration(n) * 24 * time.Hour }

// Defaults 把配置里的时长换成天数；judgment 为 0 即不清理。
func Defaults(run, ingest, judgment time.Duration) Settings {
	return Settings{RunDays: int(run / days(1)), IngestDays: int(ingest / days(1)), JudgmentDays: int(judgment / days(1))}
}
