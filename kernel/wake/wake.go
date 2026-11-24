// Package wake 是唤醒投递与租约记录（设计 §13.6 "唤醒真实主会话"、§7.4）。
//
// 唤醒真实主会话时，内核只做两件事，且都不外呼、不向本人发消息：
//  1. 产出 class=wake 的投递（路由 wake，带运行 id、不带凭据明文）。它不是提醒本人，所以不经打扰控制，
//     直接写投递队列；网关插件领取后自行开回合。
//  2. 网关插件换取运行凭据时，为换取所用的 delegate token 记一条租约：哪次运行、哪把钥匙、运行截止时刻、
//     何时结束。租约只供观测与统计，不参与授权判定。
//
// 2026-11-24 取消"租约期间钥匙降级"（本人决定）：此前认证层见到租约内的 delegate 就把整把钥匙降成范围为空的
// 运行级身份，本机 CLI 与主会话在唤醒期间对任何事项的写入都是退出码 4。现在运行身份只由调用方是否携带运行凭据
// （apa_run_…，runidentity.Identity）决定；持 delegate 钥匙的调用一律按钥匙本身的级别与 Agent 档位判定。
//
// 唤醒运行的进程不在本机（proc.QueueExternal）：目标事项离开 open 即为回执，由 Settle 按成功落账；
// 到期由调度器按超时落账。无论哪种终态（回执、失败、超时），Settle 都把该运行的租约标记释放。
//
// Run（launch=wake）由后台触发方写入；本包以运行 id 为入口，不建 Run，只经外呼调用器的写入接口落终态。
package wake

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
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
		Class: domain.DeliveryWake, Route: delivery.WakeRoute(r.InstanceID()), Version: delivery.AllVersions,
		Content: domain.DeliveryContent{Title: "wake " + runID}})
	return d, err
}

// Exchange 是 POST /runs/{id}/credential 的内核入口：经 runidentity.Exchange 换出运行凭据（谁能换、换什么由它判定），
// 并在同一事务为换取所用的 delegate token 记租约（只供观测，不影响授权）；worker 换取不记。
// 租约截止时刻就是运行的截止时刻（与运行凭据同一个值：运行开始加运行超时）；同一 token 再次换取不重复记。
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
	// delegate 没有执行者绑定，auth.Credential.Actor 的 Principal 就是凭据 id。
	n, err := tx.Exec(`INSERT INTO wake_lease (run_id, credential, expires_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
		runID, c.Actor.Principal, expires.UnixNano())
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
// 运行按成功落账，再逐个交给 onEnd。运行超时由调度器落账，不在这里。
// 同一事务里把所有已到终态（回执、失败、超时）的运行上未释放的租约标记释放。
// 每次重新从账本判定，所以漏掉的提交（如重启前）会在下次调用时补上；与超时落账在写事务里互斥，只结束一次。
func (s *Service) Settle(ctx context.Context, st *store.Store, onEnd func(proc.Result)) error {
	var has bool
	if err := st.View(ctx, func(tx *store.Tx) error {
		recs, err := proc.External(tx)
		if err != nil || len(recs) > 0 {
			has = len(recs) > 0
			return err
		}
		open, err := openLeases(tx)
		has = len(open) > 0
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
			got, err := proc.Get(tx, rec.Run.ID)
			if err != nil {
				return err
			}
			done = append(done, proc.Result{Run: got.Run})
		}
		return s.releaseEnded(tx)
	})
	if err != nil {
		return err
	}
	for _, res := range done {
		onEnd(res)
	}
	return nil
}

// openLeases 返回还有未释放租约的运行 id。
func openLeases(tx *store.Tx) ([]string, error) {
	rows, err := tx.Query(`SELECT DISTINCT run_id FROM wake_lease WHERE released_at = 0`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var runs []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		runs = append(runs, id)
	}
	return runs, rows.Err()
}

// releaseEnded 释放运行已到终态的租约。
func (s *Service) releaseEnded(tx *store.Tx) error {
	runs, err := openLeases(tx)
	if err != nil {
		return err
	}
	for _, id := range runs {
		rec, err := proc.Get(tx, id)
		if err != nil {
			return err
		}
		if rec.Run.State.Terminal() {
			if err := s.Release(tx, id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Release 把运行的租约标记释放；运行不存在或没有未释放的租约不报错（幂等）。事件带运行终态，供统计。
func (s *Service) Release(tx *store.Tx, runID string) error {
	now := s.o.Now()
	n, err := tx.Exec(`UPDATE wake_lease SET released_at = ? WHERE run_id = ? AND released_at = 0`, now.UnixNano(), runID)
	if err != nil || n == 0 {
		return err
	}
	rec, err := proc.Get(tx, runID)
	if err != nil {
		return err
	}
	_, err = eventlog.Append(tx, domain.Event{Type: EvReleased, Object: rec.Run.Target, Actor: systemActor,
		Reason: "wake run " + runID, ChainDepth: rec.Run.ChainDepth, At: now, Payload: []byte(fmt.Sprintf(`{"run_id":%q,"state":%q}`, runID, rec.Run.State))})
	return err
}
