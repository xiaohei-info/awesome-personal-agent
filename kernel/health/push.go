package health

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 推送哪些事（§7.4 "推送哪些事"）：本人在设置 › 提醒与打扰里逐类开关的推送，由这里统一产出；
// 各模块只记事件（source.failed、run.finished、item.created），不自建推送。投递 id 由类别与对象确定，重放不重复。
const (
	runCooldown      = time.Hour     // 同一事项的运行失败 1 小时内只推一次
	proposalCooldown = 2 * time.Hour // 两次"新建议攒多了"之间至少隔 2 小时
)

// templateKinds 是模板型运行的事项：成功就是结果本身，失败要本人知道"这一份没出来"。
var templateKinds = map[domain.Kind]bool{"brief": true, "routine": true}

// Pusher 按事件日志产出推送。游标只在内存：启动时从账本当前末尾开始，不补推升级前与停机前的旧事件
// （事件都由本进程写入，停机期间没有新事件）。
// shortcut: 进程在写完事件、推送入队之前崩溃时那几条不补推；需要严格补推时把游标落账（加一步迁移）。
type Pusher struct {
	q       *delivery.Queue
	mu      sync.Mutex
	started bool
	after   int64
	// fresh 是上次"新建议攒多了"推送以来新增的建议数与推送时刻。
	fresh    int
	lastPush time.Time
}

// NewPusher 返回 Pusher；q 必填。
func NewPusher(q *delivery.Queue) *Pusher { return &Pusher{q: q} }

// Sync 在自己的事务里处理游标之后的事件；失败不推进游标，下一次重做（投递按 id 幂等）。
func (p *Pusher) Sync(ctx context.Context, st *store.Store) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	after, fresh, last := p.after, p.fresh, p.lastPush
	err := st.Update(ctx, func(tx *store.Tx) error {
		if !p.started {
			head, err := eventlog.Head(tx)
			after = head
			return err
		}
		_, pol, err := policy.Current(tx)
		if err != nil {
			return err
		}
		for {
			es, err := eventlog.Query(tx, eventlog.Filter{}, after, 500)
			if err != nil {
				return err
			}
			if len(es) == 0 {
				return nil
			}
			for _, en := range es {
				after = en.Event.Seq
				if en.Tombstone {
					continue
				}
				d, ok, err := p.delivery(tx, pol, en.Event, &fresh, &last)
				if err != nil {
					return err
				}
				if ok {
					t := delivery.Target{}
					if d.Class == domain.DeliveryHealth && strings.HasPrefix(d.ID, "push:source:") {
						t.Source = d.Item
					}
					if _, _, err := p.q.CreateGated(tx, pol, d, t); err != nil {
						return err
					}
				}
			}
		}
	})
	if err == nil {
		p.started, p.after, p.fresh, p.lastPush = true, after, fresh, last
	}
	return err
}

// route 是一类推送的去向：设置里选了渠道就用它，否则沿用"需要你决定"的路由。
func pushRoute(pol policy.Policy, r policy.PushRule) string {
	if r.Route != "" {
		return r.Route
	}
	if d := pol.Disturb.Routes[string(domain.DeliveryDecision)]; d != "" {
		return d
	}
	return delivery.RouteWeb
}

// delivery 把一条事件换成推送；不属于任何一类、类别关着、或已不需要本人知道时 ok 为假。
func (p *Pusher) delivery(tx *store.Tx, pol policy.Policy, e domain.Event, fresh *int, last *time.Time) (domain.Delivery, bool, error) {
	switch e.Type {
	case domain.EvSourceFailed:
		return sourcePush(tx, pol, e)
	case domain.EvRunFinished:
		return runPush(tx, pol, e)
	case domain.EvItemCreated:
		r := pol.Disturb.PushRuleFor(policy.PushNewProposals)
		var pl struct {
			Item struct {
				Phase domain.Phase `json:"phase"`
			} `json:"item"`
		}
		if json.Unmarshal(e.Payload, &pl) != nil || pl.Item.Phase != domain.PhaseProposed {
			return domain.Delivery{}, false, nil
		}
		*fresh++
		if !r.On || *fresh < r.Min || (!last.IsZero() && e.At.Sub(*last) < proposalCooldown) {
			return domain.Delivery{}, false, nil
		}
		n := *fresh
		*fresh, *last = 0, e.At
		return domain.Delivery{ID: fmt.Sprintf("push:proposals:%d", e.Seq), Class: domain.DeliveryReminder, Version: 1,
			Route:   pushRoute(pol, r),
			Origin:  &domain.DeliveryOrigin{SourceEvent: fmt.Sprintf("seq:%d", e.Seq), Rule: "push/" + policy.PushNewProposals},
			Content: domain.DeliveryContent{Title: fmt.Sprintf("新建议攒了 %d 条", n), Summary: "有空时去建议页看一眼：接受、补到已有事项或忽略。", WebURL: "/proposals"}}, true, nil
	}
	return domain.Delivery{}, false, nil
}

// sourcePush：连续失败（第 3 次）、认证或配置失败、永久失败。同一串失败只推一次：投递 id 带上一次成功的时刻；
// 来源已经恢复的不再推。
func sourcePush(tx *store.Tx, pol policy.Policy, e domain.Event) (domain.Delivery, bool, error) {
	r := pol.Disturb.PushRuleFor(policy.PushSourceFailing)
	var pl struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
	}
	if !r.On || json.Unmarshal(e.Payload, &pl) != nil {
		return domain.Delivery{}, false, nil
	}
	src, err := registry.Get(tx, e.Object)
	if err != nil {
		return domain.Delivery{}, false, nil // 来源已删除：不再推
	}
	st, err := runner.StatsOf(src)
	if err != nil || st.ConsecutiveFailures == 0 {
		return domain.Delivery{}, false, err
	}
	name := orElse(src.Name, src.ID)
	title, step := "来源「"+name+"」连续失败", "去来源页看失败原因；常见是本机工具没在跑或网络不通。"
	switch pl.Kind {
	case "auth_or_config":
		title, step = "来源「"+name+"」要重新登录或改配置", "去来源页按提示重新登录或补全配置，修好之前它不会再读到新内容。"
	case "permanent":
		title, step = "来源「"+name+"」已停用", "它遇到无法自动恢复的失败，已停用；去来源页看原因，修好后重新启用。"
	}
	suffix := "fail"
	if pl.Kind == "permanent" {
		suffix = "off"
	}
	return domain.Delivery{ID: fmt.Sprintf("push:source:%s:%d:%s", src.ID, st.LastSuccessAt.Unix(), suffix), Item: src.ID, Class: domain.DeliveryHealth, Version: 1,
		Route:   pushRoute(pol, r),
		Content: domain.DeliveryContent{Title: title, Summary: fmt.Sprintf("已连续失败 %d 次。%s", st.ConsecutiveFailures, step), WebURL: "/sources/" + src.ID}}, true, nil
}

// runPush：任务型与后台运行失败或超时。模板型（简报、例行任务）归"简报或定时任务没出来"，其余归"Agent 运行失败"；
// 事项已经关掉的不推（本人已处理）。同一事项 1 小时内只推一次。
func runPush(tx *store.Tx, pol policy.Policy, e domain.Event) (domain.Delivery, bool, error) {
	var pl struct {
		Category string `json:"category"`
		State    string `json:"state"`
		RunID    string `json:"run_id"`
		Error    string `json:"error"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil || (pl.Category != "task" && pl.Category != "background") ||
		(pl.State != string(domain.RunFailed) && pl.State != string(domain.RunTimeout)) {
		return domain.Delivery{}, false, nil
	}
	it, err := items.Get(tx, e.Object)
	if err != nil || it.Phase == domain.PhaseDone || it.Phase == domain.PhaseDropped {
		return domain.Delivery{}, false, nil
	}
	cat, title := policy.PushRunFailed, "Agent 没做完：「"+it.Title+"」"
	if templateKinds[it.Kind] {
		cat, title = policy.PushTemplateRun, "「"+it.Title+"」没出来"
	}
	r := pol.Disturb.PushRuleFor(cat)
	if !r.On {
		return domain.Delivery{}, false, nil
	}
	why := "运行失败"
	if pl.State == string(domain.RunTimeout) {
		why = "运行超时"
	}
	if pl.Error != "" {
		why += "：" + firstLine(pl.Error)
	}
	return domain.Delivery{ID: fmt.Sprintf("push:run:%s:%d", it.ID, e.At.Truncate(runCooldown).Unix()), Item: it.ID, Class: domain.DeliveryHealth, Version: 1,
		Route:   pushRoute(pol, r),
		Content: domain.DeliveryContent{Title: title, Summary: why + "。去事项里看原因，决定重跑还是放弃。", WebURL: "/items/" + it.ID + "#agent"}}, true, nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if r := []rune(s); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return s
}
