package inbound

// 补救旧级联误丢的事件（§9.3）：把时间段内由 v0.1 级联依判断结论定案为 ignored、没有判断记录的来源事件放回积压
// （unjudged，原因 restored），之后只经积压放开的四步重判。预览只读；执行不调模型、不建事项，每条各记一条 inbound.disposed，
// 操作者是调用者。整次放回在一个事务里：命中集合由账本现状决定，已放回的不再是 ignored，所以同一时间段再执行是 0 条。

import (
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// CmdBacklogRestore 把旧级联忽略的事件放回积压。只接受本人原话（Web 或 delegate 附原话），不接受授权单。
const CmdBacklogRestore = "inbound.backlog_restore"

// SourceCount 是某来源命中的条数。
type SourceCount struct {
	Source string `json:"source"`
	Count  int    `json:"count"`
}

// RestorePreview 是预览：命中的总条数与按来源的分布（来源 id 升序）。
type RestorePreview struct {
	Matched int           `json:"matched"`
	Sources []SourceCount `json:"sources"`
}

// RestoreResult 是执行的结果：放回的条数。
type RestoreResult struct {
	Restored int `json:"restored"`
}

type restoreIn struct {
	From   time.Time `json:"from"`
	To     time.Time `json:"to"`
	Source string    `json:"source,omitempty"`
}

func parseRestore(in json.RawMessage) (restoreIn, error) {
	var v restoreIn
	dec := json.NewDecoder(strings.NewReader(string(in)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil {
		return v, restoreInvalid("bad input: " + err.Error())
	}
	return v, v.check()
}

func (v restoreIn) check() error {
	if v.From.IsZero() || v.To.IsZero() {
		return restoreInvalid("from and to are required")
	}
	if !v.From.Before(v.To) {
		return restoreInvalid("from must be earlier than to")
	}
	return nil
}

func restoreInvalid(why string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, "give from and to as RFC 3339 times with from earlier than to")
}

// RestoreWindow 校验预览的时间段与来源：from、to 必填且 from 早于 to（400），来源给了但没登记是 404。
func RestoreWindow(tx *store.Tx, from, to time.Time, source string) error {
	if err := (restoreIn{From: from, To: to}).check(); err != nil {
		return err
	}
	if source != "" {
		_, err := registry.Get(tx, source)
		return err
	}
	return nil
}

type restorableEvent struct{ id, source string }

// restorable 返回命中的事件，按落盘顺序。命中条件同时满足（§9.3）：处置为 ignored；发生在 [from, to)；没有判断记录（升级前判的）；
// 最近一次处置是内核依判断结论写入的，不是规则出口（载荷带规则）、也不是调用者的忽略（操作者不是内核）。
// shortcut: 来源事件没有单独的落盘时间列，时间段按事件的发生时间（occurred_at）取，与重判窗口同一把尺；
// shortcut: 逐条读判断记录与事件流，积压到数万条拖慢预览时再给判断记录与事件流加批量查询。
func restorable(tx *store.Tx, from, to time.Time, source string) ([]restorableEvent, error) {
	q := `SELECT id, source_id FROM sources_runner_event WHERE disposition = 'ignored' AND judgment = '' AND occurred_at >= ? AND occurred_at < ?`
	args := []any{from.UnixNano(), to.UnixNano()}
	if source != "" {
		q, args = q+` AND source_id = ?`, append(args, source)
	}
	rows, err := tx.Query(q+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	var cand []restorableEvent
	for rows.Next() {
		var c restorableEvent
		if err := rows.Scan(&c.id, &c.source); err != nil {
			rows.Close()
			return nil, err
		}
		cand = append(cand, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []restorableEvent
	for _, c := range cand {
		if rs, _, err := record.List(tx, record.Filter{Subject: c.id, Limit: 1}); err != nil || len(rs) > 0 {
			if err != nil {
				return nil, err
			}
			continue
		}
		ok, err := ignoredByOldCascade(tx, c.id)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, c)
		}
	}
	return out, nil
}

// ignoredByOldCascade 报告这条事件最近一次处置是不是内核依判断结论写成 ignored 的。
func ignoredByOldCascade(tx *store.Tx, id string) (bool, error) {
	es, err := eventlog.ReadObject(tx, id)
	if err != nil {
		return false, err
	}
	for _, e := range slices.Backward(es) {
		if e.Tombstone || e.Event.Type != EvDisposed {
			continue
		}
		var p struct {
			Disposition domain.Disposition `json:"disposition"`
			Rule        string             `json:"rule"`
		}
		if json.Unmarshal(e.Event.Payload, &p) != nil {
			return false, nil
		}
		a := e.Event.Actor
		return p.Disposition == domain.DispositionIgnored && p.Rule == "" && a.Type == domain.ActorSystem && a.Principal == systemPrinc, nil
	}
	return false, nil
}

// PreviewRestore 是只读预览：命中条数与按来源的分布。调用方先用 RestoreWindow 校验参数。
func PreviewRestore(tx *store.Tx, from, to time.Time, source string) (RestorePreview, error) {
	evs, err := restorable(tx, from, to, source)
	if err != nil {
		return RestorePreview{}, err
	}
	by := map[string]int{}
	for _, e := range evs {
		by[e.source]++
	}
	out := RestorePreview{Matched: len(evs), Sources: []SourceCount{}}
	for s, n := range by {
		out.Sources = append(out.Sources, SourceCount{Source: s, Count: n})
	}
	slices.SortFunc(out.Sources, func(a, b SourceCount) int { return strings.Compare(a.Source, b.Source) })
	return out, nil
}

func restoreSpec() command.Spec {
	return command.Spec{Name: CmdBacklogRestore, Action: authz.ActionManageSource, Tier: authz.TierWords,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			_, err := parseRestore(in)
			return authz.Object{}, err
		},
		Validate: func(tx *store.Tx, _ authz.Object, in json.RawMessage) error {
			v, _ := parseRestore(in)
			return RestoreWindow(tx, v.From, v.To, v.Source)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parseRestore(c.Input)
			evs, err := restorable(c.Tx, v.From, v.To, v.Source)
			if err != nil {
				return nil, err
			}
			for _, e := range evs {
				if err := runner.RestoreIgnored(c.Tx, e.id); err != nil {
					return nil, err
				}
				p, _ := json.Marshal(map[string]any{"event": e.id, "disposition": domain.DispositionUnjudged, "unjudged_reason": domain.UnjudgedRestored,
					"previous": domain.DispositionIgnored, "from": v.From, "to": v.To})
				if _, err := c.Emit(domain.Event{Type: EvDisposed, Object: e.id, Reason: "restored to the backlog: ignored by the v0.1 cascade", Payload: p}); err != nil {
					return nil, err
				}
			}
			return RestoreResult{Restored: len(evs)}, nil
		}}
}
