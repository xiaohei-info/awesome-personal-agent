// Package stale 是久未推进询问（设计 §7.3、附录 C.4 的 stale 规则）：
// open 的任务与承诺超过 stale_after 没有任何变更、也没有未来的计划日期时，列入"今天"的久未推进一组；
// stale 时钟规则按 schedule 检查，把当时的一批合并成一条提醒，让本人批量选继续、改期或放弃。
//
//   - 只读事项、不调模型、不自动改任何事项的状态；继续、改期、放弃都是本人经已有命令做的。
//   - 阈值与周期、模式都取自能力包的 stale 规则，热加载后 Sync 重排 Timer；阈值同时供"今天"取材（After）。
//   - 提醒是提醒类：auto 才产出，observe 只记 rule.observed（不提醒），off 什么都不做。
//   - 同一批事项（事项 id 集合相同）只提醒一次；批里有事项被处理或新增后才会是新的一条。
//   - 提醒经触达这个唯一出口，受静默与合并约束。
package stale

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const (
	ruleType = "stale"
	timerID  = "stale_check"
	// TodayURL 是提醒指向的 Web 入口：今天页的久未推进一组。
	TodayURL = "/today#stale"
)

// Reminder 是触达的合并提醒入口（*attention.Attention 满足它）。
type Reminder interface {
	RemindGroup(tx *store.Tx, anchor domain.Item, key, title, summary, webURL string) error
}

// PackSource 取已加载的能力包（packs.Loader 满足它）。
type PackSource interface {
	Get(name string) (*packs.Pack, bool)
}

// Options 配置久未推进检查；Packs、Pack、Reminder 必填。
type Options struct {
	Packs    PackSource
	Pack     string // 声明 stale 时钟规则的能力包名
	Reminder Reminder
	Now      func() time.Time
}

// Service 无内存状态：Timer 在时钟里，规则模式在规则状态里。
type Service struct{ o Options }

// New 校验配置并返回服务。
func New(o Options) (*Service, error) {
	if o.Packs == nil || o.Pack == "" || o.Reminder == nil {
		return nil, errors.New("stale: Packs, Pack and Reminder are required")
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	return &Service{o}, nil
}

type rule struct {
	key, schedule string
	after         time.Duration
}

// rule 取当前生效的 stale 规则；包未加载或没有这类规则时 ok 为假。
func (s *Service) rule() (rule, bool) {
	p, ok := s.o.Packs.Get(s.o.Pack)
	if !ok {
		return rule{}, false
	}
	for _, cr := range p.ClockRules {
		if cr.Type != ruleType {
			continue
		}
		after, err := parseAfter(cr.StaleAfter)
		if err != nil { // 加载器已校验；走到这里说明包与内核不一致，宁可不运行
			return rule{}, false
		}
		return rule{key: rules.ClockKey(p.Name, cr.ID), schedule: cr.Schedule, after: after}, true
	}
	return rule{}, false
}

// After 是生效的久未推进阈值；没有 stale 规则时取默认的 14 天。"今天"取材用它，与检查用同一个值。
func (s *Service) After() time.Duration {
	if r, ok := s.rule(); ok {
		return r.after
	}
	return items.StaleAfter
}

// parseAfter 解析 stale_after：空为默认 14d，"Nd" 为天数，其余按 Go 时长。
func parseAfter(v string) (time.Duration, error) {
	if v == "" {
		return items.StaleAfter, nil
	}
	if n, ok := strings.CutSuffix(v, "d"); ok {
		var days int
		if _, err := fmt.Sscanf(n, "%d", &days); err == nil && fmt.Sprint(days) == n && days > 0 {
			return time.Duration(days) * 24 * time.Hour, nil
		}
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("stale: bad stale_after %q", v)
	}
	return d, nil
}

// Sync 让检查 Timer 与当前的 stale 规则一致：启动时调一次，之后每次能力包重新加载都调。
// 幂等：Timer 已按同一规则与周期排好就不动（重启不推迟检查，错过的检查由时钟补上）；
// 缺失或规则键、周期变了才重排，规则不在了就撤销。
func (s *Service) Sync(tx *store.Tx) error {
	r, ok := s.rule()
	cur, have, err := clock.Get(tx, timerID)
	if err != nil {
		return err
	}
	if !ok {
		if have {
			return clock.Cancel(tx, timerID)
		}
		return nil
	}
	if have && cur.Target == r.key && cur.Recurrence == r.schedule {
		return nil
	}
	at, err := clock.Start(r.schedule, s.o.Now())
	if err != nil {
		return fmt.Errorf("stale: %s: %w", r.key, err)
	}
	if have {
		if err := clock.Cancel(tx, timerID); err != nil {
			return err
		}
	}
	_, err = clock.Add(tx, domain.Timer{ID: timerID, Target: r.key, Kind: domain.TimerRecur, At: at, Recurrence: r.schedule})
	return err
}

// Owns 判断时钟交来的命令是否归本模块；组装处据它在处理器间分派。
func (s *Service) Owns(c clock.Command) bool { return c.TimerID == timerID }

// HandleTimer 是一次检查，在时钟的事务里运行（只读事项、不外呼）。
func (s *Service) HandleTimer(tx *store.Tx, c clock.Command) error {
	if !s.Owns(c) {
		return fmt.Errorf("stale: timer %s is not mine", c.TimerID)
	}
	r, ok := s.rule()
	if !ok {
		return nil
	}
	m, err := mode(tx, r.key)
	if err != nil || m == rules.ModeOff {
		return err
	}
	list, err := items.TodayItemsAfter(tx, s.o.Now(), r.after)
	if err != nil || len(list.Stale) == 0 {
		return err
	}
	group := list.Stale
	slices.SortFunc(group, func(a, b domain.Item) int { return strings.Compare(a.ID, b.ID) })
	if m == rules.ModeObserve {
		return s.observe(tx, r, group, c.Due)
	}
	ids := make([]string, len(group))
	titles := make([]string, len(group))
	for i, it := range group {
		ids[i], titles[i] = it.ID, "「"+it.Title+"」"
	}
	sum := sha256.Sum256([]byte(strings.Join(ids, ",")))
	title := fmt.Sprintf("%d 件事项很久没有进展", len(group))
	summary := fmt.Sprintf("%s 超过 %s 没有进展。到今天的久未推进一组里选继续、改期或放弃；系统不会替你改状态。",
		strings.Join(titles, "、"), humanDuration(r.after))
	return s.o.Reminder.RemindGroup(tx, group[0], "stale:"+hex.EncodeToString(sum[:8]), title, summary, TodayURL)
}

// observe 只记本来会提醒哪些事项。
func (s *Service) observe(tx *store.Tx, r rule, group []domain.Item, due time.Time) error {
	pv, _, err := policy.Current(tx)
	if err != nil {
		return err
	}
	ids := make([]string, len(group))
	for i, it := range group {
		ids[i] = it.ID
	}
	body, _ := json.Marshal(map[string]any{"rule": r.key, "due": due, "would_do": "remind stale items", "items": ids})
	_, err = eventlog.Append(tx, domain.Event{Type: rules.EvObserved, Object: "rule:" + r.key,
		Actor:  domain.Actor{Type: domain.ActorSystem, Principal: "rule:" + r.key, Via: domain.ViaRule},
		Basis:  &domain.Basis{PolicyVersion: pv, RuleID: r.key},
		Reason: "observe: would remind stale items", Payload: body, At: s.o.Now()})
	return err
}

func humanDuration(d time.Duration) string {
	if d%(24*time.Hour) == 0 {
		return fmt.Sprintf("%d 天", d/(24*time.Hour))
	}
	return d.String()
}

// mode 是规则状态里的当前模式；未登记按 off。
func mode(tx *store.Tx, key string) (string, error) {
	sts, err := rules.List(tx)
	for _, st := range sts {
		if st.Key == key {
			return st.Mode, err
		}
	}
	return rules.ModeOff, err
}
