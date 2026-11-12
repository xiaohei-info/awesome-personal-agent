// Package wake 是唤醒投递与租约（设计 §13.6 "唤醒真实主会话"、§7.4）。
//
// 唤醒真实主会话时，内核只做两件事，且都不外呼、不向本人发消息：
//  1. 产出 class=wake 的投递（路由 wake，带运行 id、不带凭据明文）。它不是提醒本人，所以不经打扰控制，
//     直接写投递队列；网关插件领取后自行开回合。
//  2. 网关插件换取运行凭据时，为换取所用的 delegate token（即真实主会话手里的那个）开租约：
//     到运行结束（回执）或运行超时为止，该 token 的代答一律拒绝（退出码 4）。本人在 Web 的决定不受影响。
//
// 唤醒运行的进程不在本机（proc.QueueExternal）：目标事项离开 open 即为回执，由 Settle 按成功落账；
// 到期由调度器按超时落账。
//
// 租约不靠吊销：Identity 在每次认证时读租约与运行状态，所以运行进入终态、显式 Release、到期都立即恢复，
// 不依赖调用方记得收尾。被租约约束的 delegate 按运行凭据级（LevelRun，范围为空）处理——决策、接受、丢弃、
// 改任意事项都是退出码 4，且决策类拒绝由授权层记 command.rejected。
// shortcut: 租约期间该 token 的其他 delegate 能力（如管理项目）也一并拒绝，需要按命令区分时再扩展授权层。
//
// Run（launch=wake）由后台触发方写入；本包以运行 id 为入口，不建 Run，只经外呼调用器的写入接口落终态。
package wake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 本模块追加的审计记录类型（内部记录，`apa why` 可见）。
const (
	EvOpened   = "wake.lease_opened"
	EvReleased = "wake.lease_released"
)

// DeliveryID 是运行 id 对应的唤醒投递 id（幂等键）。
func DeliveryID(runID string) string { return "wake_" + runID }

// Module 是本模块在存储层登记的迁移：一行一个（运行，delegate 凭据）租约。
var Module = store.Module{Name: "wake", Migrations: []store.Migration{{
	Version: 1, Name: "create-lease",
	SQL: `CREATE TABLE wake_lease (
	run_id TEXT NOT NULL,
	credential TEXT NOT NULL,
	expires_at INTEGER NOT NULL,
	released_at INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (run_id, credential)
) WITHOUT ROWID;
CREATE INDEX wake_lease_credential ON wake_lease(credential);`,
}}}

// Options 配置 Service。Delivery 必须给出。
type Options struct {
	Delivery *delivery.Queue
	Now      func() time.Time
}

// Service 无状态，所有状态在账本里。
type Service struct{ o Options }

// New 返回 Service。
func New(o Options) (*Service, error) {
	if o.Delivery == nil {
		return nil, errors.New("wake: Delivery is required")
	}
	if o.Now == nil {
		o.Now = clock.Now
	}
	return &Service{o}, nil
}

var systemActor = domain.Actor{Type: domain.ActorSystem, Principal: "kernel:wake", Via: domain.ViaClock}

// wakeRun 取未结束的 launch=wake 后台运行；不是则拒绝。
func wakeRun(tx *store.Tx, runID string) (domain.Run, error) {
	rec, err := proc.Get(tx, runID)
	if err != nil {
		return domain.Run{}, err
	}
	r := rec.Run
	switch {
	case r.Category != domain.RunBackground || r.Launch != runidentity.LaunchWake:
		return r, domain.NewError(domain.CodeConflict, "run "+runID+" is not a wake run", "only launch=wake background runs get a wake delivery")
	case r.State.Terminal():
		return r, domain.NewError(domain.CodeConflict, "run "+runID+" has already finished", "start a new run")
	case r.Target == "":
		return r, domain.NewError(domain.CodeInvalidRequest, "wake run has no target item", "fix the wake request")
	}
	return r, nil
}

// Open 在调用方事务内为一次 launch=wake 的后台运行产出唤醒投递（与写 Run 同一事务，§13.6 拉起流程第 3、4 步）。
// 运行必须是未结束的 launch=wake 后台运行，否则拒绝，不写任何东西。重复调用幂等：同一投递不重复产出。
// 投递的版本不随事项变化而作废。租约不在这里开：要等网关插件换取凭据时才知道是哪个 token（Exchange）。
func (s *Service) Open(tx *store.Tx, runID string) (domain.Delivery, error) {
	r, err := wakeRun(tx, runID)
	if err != nil {
		return domain.Delivery{}, err
	}
	d, _, err := s.o.Delivery.Create(tx, domain.Delivery{ID: DeliveryID(runID), Item: r.Target, Run: runID,
		Class: domain.DeliveryWake, Route: delivery.RouteWake, Version: delivery.AllVersions,
		Content: domain.DeliveryContent{Title: "wake " + runID}})
	return d, err
}

// Exchange 是 POST /runs/{id}/credential 的内核入口：经 runidentity.Exchange 换出运行凭据（谁能换、换什么由它判定），
// 并在同一事务为换取所用的 delegate token 开租约。网关插件与真实主会话在同一进程、持同一 token，
// 所以换取者就是要被约束的那个 token；worker token 本就没有代答能力，不开租约。
// 租约期限是运行的截止时刻（开始时间加运行超时，未设时 1 小时）；同一 token 再次换取不重复开。
func (s *Service) Exchange(tx *store.Tx, c command.Caller, runID string) (string, time.Time, error) {
	now := s.o.Now()
	secret, expires, err := runidentity.Exchange(tx, c, runID, now)
	if err != nil || c.Kind != auth.Delegate {
		return secret, expires, err
	}
	r, err := wakeRun(tx, runID)
	if err != nil {
		return "", time.Time{}, err
	}
	ttl := runidentity.DefaultExchangeTTL
	if r.TimeoutSec > 0 {
		ttl = time.Duration(r.TimeoutSec) * time.Second
	}
	start := now
	if r.StartedAt != nil {
		start = *r.StartedAt
	}
	// delegate 没有执行者绑定，auth.Credential.Actor 的 Principal 就是凭据 id（与 guard 同一口径）。
	n, err := tx.Exec(`INSERT INTO wake_lease (run_id, credential, expires_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		runID, c.Actor.Principal, start.Add(ttl).UnixNano())
	if err != nil || n == 0 {
		return secret, expires, err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: EvOpened, Object: r.Target, Actor: systemActor,
		Reason: "wake run " + runID, ChainDepth: r.ChainDepth, At: now,
		Payload: []byte(fmt.Sprintf(`{"run_id":%q,"credential":%q}`, runID, c.Actor.Principal))})
	return secret, expires, err
}

// Attach 在每次命令提交后为已有回执的唤醒运行收尾（Settle），结果交给 onEnd（与调度器的 OnResult 同一条路）。
func (s *Service) Attach(x *command.Executor, onEnd func(proc.Result), onErr func(error)) {
	x.OnCommit(func(command.Commit) {
		if err := s.Settle(context.Background(), x.Store(), onEnd); err != nil && onErr != nil {
			onErr(err)
		}
	})
}

// Settle 结束已有回执的唤醒运行：目标事项离开 open（done、waiting、needs_input、dropped）即为回执（§13.6），
// 运行按成功落账并释放租约，再逐个交给 onEnd。运行超时由调度器落账，不在这里。
// 每次重新从账本判定，所以漏掉的提交（如重启前）会在下次调用时补上；与超时落账在写事务里互斥，只结束一次。
func (s *Service) Settle(ctx context.Context, st *store.Store, onEnd func(proc.Result)) error {
	var has bool
	if err := st.View(ctx, func(tx *store.Tx) error {
		recs, err := proc.External(tx)
		has = len(recs) > 0
		return err
	}); err != nil || !has {
		return err
	}
	var done []proc.Result
	err := st.Update(ctx, func(tx *store.Tx) error {
		done = nil
		recs, err := proc.External(tx)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.Run.Launch != runidentity.LaunchWake {
				continue
			}
			it, err := items.Get(tx, rec.Run.Target)
			if err != nil {
				return err
			}
			if it.Phase == domain.PhaseOpen {
				continue
			}
			if err := proc.Settle(tx, rec.Run.ID, domain.RunSucceeded, nil, "", s.o.Now()); err != nil {
				return err
			}
			if err := s.Release(tx, rec.Run.ID); err != nil {
				return err
			}
			got, err := proc.Get(tx, rec.Run.ID)
			if err != nil {
				return err
			}
			done = append(done, proc.Result{Run: got.Run})
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, res := range done {
		onEnd(res)
	}
	return nil
}

// Release 在回执时提前结束租约；运行不存在或没有租约不报错（幂等）。
func (s *Service) Release(tx *store.Tx, runID string) error {
	now := s.o.Now()
	n, err := tx.Exec(`UPDATE wake_lease SET released_at = ? WHERE run_id = ? AND released_at = 0`, now.UnixNano(), runID)
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	rec, err := proc.Get(tx, runID)
	if err != nil {
		return err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: EvReleased, Object: rec.Run.Target, Actor: systemActor,
		Reason: "wake run " + runID, ChainDepth: rec.Run.ChainDepth, At: now, Payload: []byte(fmt.Sprintf(`{"run_id":%q}`, runID))})
	return err
}

// Leased 报告凭据此刻是否在唤醒租约内：存在未释放、未到期、且运行未结束的租约。
func (s *Service) Leased(tx *store.Tx, credential string) (bool, error) {
	rows, err := tx.Query(`SELECT run_id FROM wake_lease WHERE credential = ? AND released_at = 0 AND expires_at > ?`,
		credential, s.o.Now().UnixNano())
	if err != nil {
		return false, err
	}
	var runs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return false, err
		}
		runs = append(runs, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, id := range runs {
		rec, err := proc.Get(tx, id)
		if err != nil {
			return false, err
		}
		if !rec.Run.State.Terminal() {
			return true, nil
		}
	}
	return false, nil
}

// Identity 包装一个认证入口：认证出的 delegate 若在租约内，降为不能代答的运行级身份（范围为空）；
// 其余调用者原样通过。把它放在 command.Bearer 外面使用。
func (s *Service) Identity(inner command.Identity) command.Identity { return guard{s, inner} }

type guard struct {
	s     *Service
	inner command.Identity
}

func (g guard) Authenticate(tx *store.Tx) (command.Caller, error) {
	c, err := g.inner.Authenticate(tx)
	if err != nil || c.Kind != auth.Delegate {
		return c, err
	}
	// delegate 没有执行者绑定，auth.Credential.Actor 的 Principal 就是凭据 id。
	leased, err := g.s.Leased(tx, c.Actor.Principal)
	if err != nil {
		return command.Caller{}, err
	}
	if leased {
		c.Level, c.RunScope = authz.LevelRun, nil
	}
	return c, nil
}
