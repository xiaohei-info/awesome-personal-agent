package health

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 登记健康告警的消费游标。游标从 chain 模块的旧游标接手（模块名排序保证 chain 先迁移），
// 已有账本里的历史告警不会被补发。
var Module = store.Module{Name: "health", Migrations: []store.Migration{{
	Version: 1, Name: "create-health-cursor",
	SQL: `CREATE TABLE health_cursor (
	id INTEGER PRIMARY KEY CHECK (id = 1),
	seq INTEGER NOT NULL
);
INSERT INTO health_cursor (id, seq) SELECT id, seq FROM chain_cursor;`,
}}}

// ScreeningKind 是 web_only 初筛失败 health.alert 的 payload.kind。初筛失败按策略规则继续、逐事项发生，
// 设计（§14.4）只要求进健康视图，不产出投递；这是唯一不出投递的健康告警。
const ScreeningKind = "screening"

// DraftKind 是 decision-card 包的卡片文案、阈值判定与摘要起草失败的 payload.kind。与初筛失败同理：
// 逐事项发生、按规则继续，只进健康视图（"模型不可用"），不出投递。
const DraftKind = "decision-card"

// Notifier 是"健康告警 → 健康类投递"的唯一出口（设计 §15.2）：按游标读 health.alert，
// 每条以事件序号为幂等键产出一条 class=health 的投递，重复处理不会重复投递。
// 各模块只记 health.alert，不自建健康投递；本包只写自己的游标与投递队列。
type Notifier struct{ q *delivery.Queue }

// NewNotifier 返回 Notifier；q 必填。
func NewNotifier(q *delivery.Queue) *Notifier { return &Notifier{q} }

// Sync 在调用方事务内处理游标之后的全部 health.alert。
func (n *Notifier) Sync(tx *store.Tx) error {
	var after int64
	r, err := tx.Query(`SELECT seq FROM health_cursor WHERE id = 1`)
	if err != nil {
		return err
	}
	if r.Next() {
		err = r.Scan(&after)
	}
	if err == nil {
		err = r.Err()
	}
	r.Close()
	if err != nil {
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
	for {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, after, 500)
		if err != nil {
			return err
		}
		if len(es) == 0 {
			break
		}
		for _, en := range es {
			after = en.Event.Seq
			d, ok := alertDelivery(en, route(pol))
			if !ok {
				continue
			}
			if _, _, err := n.q.Create(tx, d); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(`INSERT INTO health_cursor (id, seq) VALUES (1, ?) ON CONFLICT(id) DO UPDATE SET seq = excluded.seq`, after)
	return err
}

// alertDelivery 把一条 health.alert 格式化成健康类投递；初筛与起草失败不出投递。
func alertDelivery(en eventlog.Entry, route string) (domain.Delivery, bool) {
	e := en.Event
	var p struct {
		Kind string `json:"kind"`
	}
	if len(e.Payload) > 0 {
		_ = json.Unmarshal(e.Payload, &p) // 墓碑化后没有内容：按通用格式出投递
	}
	if p.Kind == ScreeningKind || p.Kind == DraftKind {
		return domain.Delivery{}, false
	}
	d := domain.Delivery{
		ID: fmt.Sprintf("health:%d", e.Seq), Item: e.Object, Class: domain.DeliveryHealth, Version: 1, Route: route,
		Content: domain.DeliveryContent{
			Title:   "健康告警：" + e.Object,
			Summary: e.Reason + "。修复入口见健康视图。",
			WebURL:  "/health",
		},
	}
	if t, ok := chain.TripOf(en); ok {
		d.Content.Title = "链路熔断：" + t.Object
		d.Content.Summary = fmt.Sprintf("自动链路深度 %d 超过阈值 %d，已只记录、不再派发。需要继续时请在 Web 重新委派，新链路从头计数。",
			t.Depth, t.Threshold)
		if t.Object != t.Command { // 没有对象的命令拦下时，Object 是命令名，不是事项
			d.Content.WebURL = "/items/" + t.Object
		}
	}
	return d, true
}

// route 取策略里健康类的路由，未配置走 Web。
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

// PackRejected 记一条能力包加载失败的 health.alert，作为 packs.Options.OnReject 的落账函数；
// 加载器自己不写账本。同一包的同一内容只通知一次由加载器保证。
func PackRejected(tx *store.Tx, r packs.Rejection) error {
	p, _ := json.Marshal(map[string]string{"kind": "pack", "pack": r.Pack, "source": string(r.Source), "hash": r.Hash, "reason": r.Reason})
	_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: "pack:" + r.Pack, Payload: p,
		Actor:  domain.Actor{Type: domain.ActorSystem, Principal: "kernel:packs", Via: domain.ViaCLI},
		Reason: "capability pack " + r.Pack + " rejected, previous valid version kept: " + r.Reason})
	return err
}

// Attach 让命令提交后立即追赶健康告警。失败不丢告警：游标没推进，下一次 Sync 会重做。
// 不经命令提交写账本的模块（后台运行、规则、跟进、初筛、能力包加载）由组装处在落账后另调 Sync。
func (n *Notifier) Attach(x *command.Executor, st *store.Store, onErr func(error)) {
	x.OnCommit(func(command.Commit) {
		if err := st.Update(context.Background(), n.Sync); err != nil && onErr != nil {
			onErr(err)
		}
	})
}
