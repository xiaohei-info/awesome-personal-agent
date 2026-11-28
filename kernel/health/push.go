package health

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 推送哪些事（§7.4 投递矩阵 D、E、F 组）：本人逐类开关的推送（push.*.on），由这里识别并交给触达（attention.Notify）；
// 送到哪、静默与合并由触达按矩阵的行统一处理，这里不选路由。各模块只记事件（source.failed、run.finished、item.created），
// 不自建推送。投递 id 由类别与对象确定，重放不重复。
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
	att     *attention.Attention
	mu      sync.Mutex
	started bool
	after   int64
	// fresh 是上次"新建议攒多了"推送以来新增的建议数与推送时刻。
	fresh    int
	lastPush time.Time
}

// NewPusher 返回 Pusher；att 必填。
func NewPusher(att *attention.Attention) *Pusher { return &Pusher{att: att} }

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
				n, ok, err := p.notice(tx, pol, en.Event, &fresh, &last)
				if err != nil {
					return err
				}
				if ok {
					if err := p.att.Notify(tx, n); err != nil {
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

// notice 把一条事件换成推送；不属于任何一类、类别关着、或已不需要本人知道时 ok 为假。
func (p *Pusher) notice(tx *store.Tx, pol policy.Policy, e domain.Event, fresh *int, last *time.Time) (attention.Notice, bool, error) {
	switch e.Type {
	case domain.EvSourceFailed:
		return sourcePush(tx, pol, e)
	case domain.EvRunFinished:
		return runPush(tx, pol, e)
	case domain.EvItemCreated, domain.EvItemUpdated, domain.EvItemPhaseChanged:
		// 攒的条数按合并之后计（§7.4、§9.3）：建立时加一，被建议合并折叠时减一；建议更新时也再看一次攒够没有。
		r := pol.Disturb.PushRuleFor(policy.PushNewProposals)
		var pl struct {
			Op   string `json:"op"`
			Item struct {
				Phase    domain.Phase `json:"phase"`
				Proposal *struct {
					Sweep *struct {
						State string `json:"state"`
					} `json:"sweep"`
				} `json:"proposal"`
			} `json:"item"`
		}
		if json.Unmarshal(e.Payload, &pl) != nil {
			return attention.Notice{}, false, nil
		}
		settled := pl.Item.Phase == domain.PhaseProposed && pl.Item.Proposal != nil && (pl.Item.Proposal.Sweep == nil || pl.Item.Proposal.Sweep.State == "done")
		switch {
		case e.Type == domain.EvItemCreated && pl.Item.Phase == domain.PhaseProposed:
			*fresh++
		case e.Type == domain.EvItemPhaseChanged && pl.Op == "merge.fold":
			*fresh = max(*fresh-1, 0)
			return attention.Notice{}, false, nil
		case e.Type == domain.EvItemUpdated && settled:
		default:
			return attention.Notice{}, false, nil
		}
		if !r.On || *fresh < r.Min || (!last.IsZero() && e.At.Sub(*last) < proposalCooldown) {
			return attention.Notice{}, false, nil
		}
		n := *fresh
		*fresh, *last = 0, e.At
		return attention.Notice{ID: fmt.Sprintf("push:proposals:%d", e.Seq), Class: domain.DeliveryReminder,
			Origin:  &domain.DeliveryOrigin{SourceEvent: fmt.Sprintf("seq:%d", e.Seq), Rule: "push/" + policy.PushNewProposals},
			Content: domain.DeliveryContent{Title: fmt.Sprintf("新建议攒了 %d 条", n), Summary: "有空时去建议页看一眼：接受、补到已有事项或忽略。", WebURL: "/proposals"}}, true, nil
	}
	return attention.Notice{}, false, nil
}

// sourcePush：连续失败到提醒线（高频第 3 次，低频第 1 次，runner.AlertNow）、认证或配置失败、永久失败。同一串失败只推一次：投递 id 带上一次成功的时刻；
// 来源已经恢复的不再推。
func sourcePush(tx *store.Tx, pol policy.Policy, e domain.Event) (attention.Notice, bool, error) {
	r := pol.Disturb.PushRuleFor(policy.PushSourceFailing)
	var pl struct {
		Kind   string `json:"kind"`
		Reason string `json:"reason"`
		Chance string `json:"chance_period"`
	}
	if !r.On || json.Unmarshal(e.Payload, &pl) != nil {
		return attention.Notice{}, false, nil
	}
	src, err := registry.Get(tx, e.Object)
	if err != nil {
		return attention.Notice{}, false, nil // 来源已删除：不再推
	}
	st, err := runner.StatsOf(src)
	if err != nil || st.ConsecutiveFailures == 0 {
		return attention.Notice{}, false, err
	}
	name := orElse(src.Name, src.ID)
	title, step := "来源「"+name+"」连续失败", "去来源页看失败原因；常见是本机工具没在跑或网络不通。"
	summary := fmt.Sprintf("已连续失败 %d 次。", st.ConsecutiveFailures)
	if pl.Kind == "streak" && st.ConsecutiveFailures < runner.StreakFailures { // 低频：失败一次就提醒
		title = "来源「" + name + "」这次没读到"
		summary = "这类内容每 " + orElse(pl.Chance, src.Schedule) + " 才有一次机会，所以失败一次就提醒。"
	}
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
	return attention.Notice{ID: fmt.Sprintf("push:source:%s:%d:%s", src.ID, st.LastSuccessAt.Unix(), suffix), Item: src.ID, Class: domain.DeliveryHealth,
		Content: domain.DeliveryContent{Title: title, Summary: summary + step, WebURL: "/sources/" + src.ID}}, true, nil
}

// runPush：任务型与后台运行失败或超时。模板型（简报、例行任务）归"简报或定时任务没出来"，其余归"Agent 运行失败"；
// 事项已经关掉的不推（本人已处理）。同一事项 1 小时内只推一次。
func runPush(tx *store.Tx, pol policy.Policy, e domain.Event) (attention.Notice, bool, error) {
	var pl struct {
		Category string `json:"category"`
		State    string `json:"state"`
		RunID    string `json:"run_id"`
	}
	if json.Unmarshal(e.Payload, &pl) != nil || (pl.Category != "task" && pl.Category != "background") ||
		(pl.State != string(domain.RunFailed) && pl.State != string(domain.RunTimeout)) {
		return attention.Notice{}, false, nil
	}
	it, err := items.Get(tx, e.Object)
	if err != nil || it.Phase == domain.PhaseDone || it.Phase == domain.PhaseDropped {
		return attention.Notice{}, false, nil
	}
	if started, err := delegation.Started(tx, it.ID); err != nil || started { // 结局只经委派结果通知告诉发起方
		return attention.Notice{}, false, err
	}
	cat, title := policy.PushRunFailed, "Agent 没做完：「"+it.Title+"」"
	if templateKinds[it.Kind] {
		cat, title = policy.PushTemplateRun, "「"+it.Title+"」没出来"
	}
	r := pol.Disturb.PushRuleFor(cat)
	if !r.On {
		return attention.Notice{}, false, nil
	}
	summary := "Agent 没跑成 —— 去事项里看原因，决定重跑还是放弃。"
	if pl.State == string(domain.RunTimeout) {
		// 超时不是失败：这条运行到点停止，事项状态未变，已写的进展保留（§15.2）。不露秒数与内部状态词。
		summary = "超过时限停下了，事项状态未变 —— 重跑还是放弃？"
	}
	return attention.Notice{ID: fmt.Sprintf("push:run:%s:%d", it.ID, e.At.Truncate(runCooldown).Unix()), Item: it.ID, Class: domain.DeliveryHealth,
		Content: domain.DeliveryContent{Title: title, Summary: summary, WebURL: "/items/" + it.ID + "#agent"}}, true, nil
}
