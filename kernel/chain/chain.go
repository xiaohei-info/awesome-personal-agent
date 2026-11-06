// Package chain 是链路深度熔断的读出（设计 §14.9）。
//
// 深度的继承与阈值判断在命令管线与后台运行里完成：超阈值时它们只记一条 health.alert（Reason 为 ExceededReason），
// 不派发。规则拉起新运行（后台、任务型、直接调用）共用本包的 Next 与 Record：深度的继承与熔断记录只在这里和命令管线。
// 本包还负责认出这类事件并按序列出（Trips，健康视图的数据来源）。健康类投递由 kernel/health 对所有健康告警统一产出。
package chain

import (
	"context"
	"encoding/json"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ExceededReason 是熔断事件的 Reason；命令管线与后台运行都用它。
const ExceededReason = "chain depth exceeded"

// Next 是规则拉起新运行的链路深度：触发事件的深度加一（§14.9）。
func Next(trigger int) int { return trigger + 1 }

// Record 记一条熔断 health.alert：规则 rule 想在 object 上拉起深度为 depth 的运行，超过 threshold，只记录不派发。
func Record(ctx context.Context, st *store.Store, now time.Time, rule, object string, depth, threshold int) error {
	body, _ := json.Marshal(map[string]any{"rule": rule, "reason": ExceededReason, "chain_depth": depth, "threshold": threshold})
	return st.Update(ctx, func(tx *store.Tx) error {
		pv, _, err := policy.Current(tx)
		if err != nil {
			return err
		}
		_, err = eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: object,
			Actor: domain.Actor{Type: domain.ActorAgent, Principal: "rule:" + rule, Via: domain.ViaRule},
			Basis: &domain.Basis{PolicyVersion: pv, RuleID: rule}, Reason: ExceededReason, Payload: body, ChainDepth: depth, At: now})
		return err
	})
}

// Module 保留历史迁移：旧账本已登记过它，去掉会让启动时判定库比程序新；其游标由 health 模块的迁移接手。
var Module = store.Module{Name: "chain", Migrations: []store.Migration{{
	Version: 1, Name: "create-chain-cursor",
	SQL: `CREATE TABLE chain_cursor (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
);`,
}}}

// Trip 是一次熔断：哪个对象（或命令）在什么深度被拦下。
type Trip struct {
	Seq       int64
	Object    string // 被拦下的事项；没有对象的命令则是命令名
	Command   string // 命令管线拦下时的命令名
	Rule      string // 后台运行拦下时的规则 id
	Depth     int
	Threshold int
	At        time.Time
}

// TripOf 认出熔断事件；不是熔断返回 false。
func TripOf(en eventlog.Entry) (Trip, bool) {
	e := en.Event
	if e.Type != domain.EvHealthAlert || e.Reason != ExceededReason {
		return Trip{}, false
	}
	var p struct {
		Command   string `json:"command"`
		Rule      string `json:"rule"`
		Depth     int    `json:"chain_depth"`
		Threshold int    `json:"threshold"`
	}
	if len(e.Payload) > 0 {
		_ = json.Unmarshal(e.Payload, &p) // 墓碑化后的事件没有内容，深度退回事件自带的值
	}
	if p.Depth == 0 {
		p.Depth = e.ChainDepth
	}
	return Trip{Seq: e.Seq, Object: e.Object, Command: p.Command, Rule: p.Rule, Depth: p.Depth, Threshold: p.Threshold, At: e.At}, true
}

// Trips 按发生顺序返回全部熔断，是健康视图的数据来源。
// shortcut: 每次读全部健康事件，熔断很多时给视图加分页。
func Trips(tx *store.Tx) ([]Trip, error) {
	var out []Trip
	var after int64
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, after, 500)
		if err != nil || len(es) == 0 {
			return out, err
		}
		for _, en := range es {
			after = en.Event.Seq
			if t, ok := TripOf(en); ok {
				out = append(out, t)
			}
		}
	}
}
