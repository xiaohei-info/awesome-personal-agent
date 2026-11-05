// Package wake 是唤醒投递与租约（设计 §13.6 "唤醒真实主会话"、§7.4）。
//
// 唤醒真实主会话时，内核只做两件事，且都不外呼、不向本人发消息：
//  1. 产出 class=wake 的投递（路由 wake，带运行 id、不带凭据明文）。它不是提醒本人，所以不经打扰控制，
//     直接写投递队列；网关插件领取后自行开回合。
//  2. 为真实主会话手里的 delegate token 开租约：到运行结束（回执）或运行超时为止，该 token 的代答一律拒绝（退出码 4）。
//     本人在 Web 的决定不受影响。
//
// 租约不靠吊销：Identity 在每次认证时读租约与运行状态，所以运行进入终态、显式 Release、到期都立即恢复，
// 不依赖调用方记得收尾。被租约约束的 delegate 按运行凭据级（LevelRun，范围为空）处理——决策、接受、丢弃、
// 改任意事项都是退出码 4，且决策类拒绝由授权层记 command.rejected。
// shortcut: 租约期间该 token 的其他 delegate 能力（如管理项目）也一并拒绝，需要按命令区分时再扩展授权层。
//
// Run（launch=wake）由后台触发方写入；本包以运行 id 为入口，不建 Run、不改其他模块的数据。
package wake

import (
	"errors"
	"fmt"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
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

// Holders 返回 harness 的真实主会话所持 delegate 凭据的 id（auth.Credential.ID）。
// 由装配处提供：内核不认识具体 harness，凭据与 harness 的对应是登记数据。
type Holders func(tx *store.Tx, harness string) ([]string, error)

// Options 配置 Service。Delivery 与 Holders 必须给出。
type Options struct {
	Delivery *delivery.Queue
	Holders  Holders
	Now      func() time.Time
}

// Service 无状态，所有状态在账本里。
type Service struct{ o Options }

// New 返回 Service。
func New(o Options) (*Service, error) {
	if o.Delivery == nil || o.Holders == nil {
		return nil, errors.New("wake: Delivery and Holders are required")
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return &Service{o}, nil
}

var systemActor = domain.Actor{Type: domain.ActorSystem, Principal: "kernel:wake", Via: domain.ViaClock}

// Open 在调用方事务内为一次 launch=wake 的后台运行产出唤醒投递并开租约。
// 运行必须是未结束的 launch=wake 后台运行，否则拒绝，不写任何东西。重复调用幂等：同一投递、租约不重复。
// 租约期限取运行超时（未设时与凭据换取一致，1 小时），投递的版本不随事项变化而作废。
func (s *Service) Open(tx *store.Tx, runID string) (domain.Delivery, error) {
	rec, err := proc.Get(tx, runID)
	if err != nil {
		return domain.Delivery{}, err
	}
	r := rec.Run
	switch {
	case r.Category != domain.RunBackground || r.Launch != runidentity.LaunchWake:
		return domain.Delivery{}, domain.NewError(domain.CodeConflict, "run "+runID+" is not a wake run", "only launch=wake background runs get a wake delivery")
	case r.State.Terminal():
		return domain.Delivery{}, domain.NewError(domain.CodeConflict, "run "+runID+" has already finished", "start a new run")
	case r.Target == "":
		return domain.Delivery{}, domain.NewError(domain.CodeInvalidRequest, "wake run has no target item", "fix the wake request")
	}
	holders, err := s.o.Holders(tx, r.Harness)
	if err != nil {
		return domain.Delivery{}, fmt.Errorf("wake: resolve holders of %q: %w", r.Harness, err)
	}
	now := s.o.Now()
	ttl := runidentity.DefaultExchangeTTL
	if r.TimeoutSec > 0 {
		ttl = time.Duration(r.TimeoutSec) * time.Second
	}
	for _, cred := range holders {
		if _, err := tx.Exec(`INSERT INTO wake_lease (run_id, credential, expires_at) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`,
			runID, cred, now.Add(ttl).UnixNano()); err != nil {
			return domain.Delivery{}, err
		}
	}
	d, created, err := s.o.Delivery.Create(tx, domain.Delivery{ID: DeliveryID(runID), Item: r.Target, Run: runID,
		Class: domain.DeliveryWake, Route: delivery.RouteWake, Version: delivery.AllVersions,
		Content: domain.DeliveryContent{Title: "wake " + runID}})
	if err != nil {
		return d, err
	}
	if created {
		_, err = eventlog.Append(tx, domain.Event{Type: EvOpened, Object: r.Target, Actor: systemActor,
			Reason: "wake run " + runID, ChainDepth: r.ChainDepth, At: now,
			Payload: []byte(fmt.Sprintf(`{"run_id":%q,"leased":%d}`, runID, len(holders)))})
	}
	return d, err
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
