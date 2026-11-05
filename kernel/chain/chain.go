// Package chain 是链路深度熔断的出口（设计 §14.9）。
//
// 深度的继承与阈值判断在命令管线与后台运行里完成：超阈值时它们只记一条 health.alert（Reason 为 ExceededReason），
// 不派发。本包补上"产出一条健康类投递"和"健康视图能看到链路熔断"这两半：
// 读账本里的熔断事件，每条恰好产出一条健康投递（Sync），并按序列出（Trips）。
// 深度本身只由内核写进事件，本包不写深度、不改任何业务状态，只写自己的游标与投递队列。
package chain

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ExceededReason 是熔断事件的 Reason；命令管线与后台运行都用它。
const ExceededReason = "chain depth exceeded"

// Module 登记事件游标。
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

// Options 配置 Breaker；Queue 必填。
type Options struct {
	Queue  *delivery.Queue
	WebURL func(item string) string // 投递里的 Web 链接，默认 /items/<id>
}

// Breaker 无状态，游标在账本里。
type Breaker struct{ o Options }

// New 返回 Breaker。
func New(o Options) *Breaker {
	if o.WebURL == nil {
		o.WebURL = func(id string) string { return "/items/" + id }
	}
	return &Breaker{o}
}

func parse(en eventlog.Entry) (Trip, bool) {
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

func each(tx *store.Tx, after int64, fn func(Trip) error) (int64, error) {
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, after, 500)
		if err != nil {
			return after, err
		}
		if len(es) == 0 {
			return after, nil
		}
		for _, en := range es {
			after = en.Event.Seq
			if t, ok := parse(en); ok {
				if err := fn(t); err != nil {
					return after, err
				}
			}
		}
	}
}

// Trips 按发生顺序返回全部熔断，是健康视图的数据来源。
// shortcut: 每次读全部健康事件，熔断很多时给视图加分页。
func Trips(tx *store.Tx) ([]Trip, error) {
	var out []Trip
	_, err := each(tx, 0, func(t Trip) error { out = append(out, t); return nil })
	return out, err
}

// Sync 读游标之后的熔断事件，每条产出一条健康投递（以事件序号为幂等键，重复处理不会重复投递）。
func (b *Breaker) Sync(tx *store.Tx) error {
	var after int64
	if err := scan(tx, func(r *store.Rows) error { return r.Scan(&after) }, `SELECT seq FROM chain_cursor WHERE id = 1`); err != nil {
		return err
	}
	head, err := eventlog.Head(tx)
	if err != nil {
		return err
	}
	if after > head { // 账本被恢复到较早状态：从头来，宁可重算也不漏（投递幂等）
		after = 0
	}
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	after, err = each(tx, after, func(t Trip) error { return b.deliver(tx, pol, t) })
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO chain_cursor (id, seq) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET seq = excluded.seq`, after)
	return err
}

func (b *Breaker) deliver(tx *store.Tx, pol policy.Policy, t Trip) error {
	d := domain.Delivery{
		ID: fmt.Sprintf("chain_trip:%d", t.Seq), Item: t.Object, Class: domain.DeliveryHealth, Version: 1, Route: route(pol),
		Content: domain.DeliveryContent{
			Title: "链路熔断：" + t.Object,
			Summary: fmt.Sprintf("自动链路深度 %d 超过阈值 %d，已只记录、不再派发。需要继续时请在 Web 重新委派，新链路从头计数。",
				t.Depth, t.Threshold),
		},
	}
	if t.Object != t.Command { // 没有对象的命令拦下时，Object 是命令名，不是事项
		d.Content.WebURL = b.o.WebURL(t.Object)
	}
	_, _, err := b.o.Queue.Create(tx, d)
	return err
}

func route(pol policy.Policy) string {
	r := pol.Disturb.Routes[string(domain.DeliveryHealth)]
	switch {
	case r == "":
		return delivery.RouteWeb
	case r == delivery.RouteInject || r == delivery.RouteWake || r == delivery.RouteWeb || strings.HasPrefix(r, "channel:"):
		return r
	}
	return delivery.ChannelRoute(r)
}

// Attach 让命令提交后立即追赶熔断事件。失败不丢事件：游标没推进，下一次 Sync 会重做。
// 后台运行直接写账本、不经命令提交，所以组装处还要在它落账后调用 Sync。
func (b *Breaker) Attach(x *command.Executor, st *store.Store, onErr func(error)) {
	x.OnCommit(func(command.Commit) {
		if err := st.Update(context.Background(), b.Sync); err != nil && onErr != nil {
			onErr(err)
		}
	})
}

func scan(tx *store.Tx, fn func(*store.Rows) error, sql string, args ...any) error {
	r, err := tx.Query(sql, args...)
	if err != nil {
		return err
	}
	defer r.Close()
	if r.Next() {
		if err := fn(r); err != nil {
			return err
		}
	}
	return r.Err()
}
