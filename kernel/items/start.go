package items

import (
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// EvStartRequested 是本人在 Web 点了"现在开工"：对象是事项，载荷带事项快照（§7.1、§13.6）。
// 它是内部记录，不在对外稳定的事件类型表里；规则引擎据它执行能为该事项拉起后台运行的规则，与"立即跑一次"同一思路。
// 它不改事项，所以不计入事项的版本。
const EvStartRequested = "item.start_requested"

// start 是 item.start（POST /items/{id}/start）：只有本人在 Web 能发起（change_settings），事项须是 open。
// 命令只落一条 item.start_requested；拉起与否、用谁、能用什么由后台运行模块照常判定（权限上限、项目允许的执行者、链路深度、在途运行），
// 不看规则的模式——这是本人的显式指令，规则模式只管自动触发。
func (s *svc) start() command.Spec {
	parse := func(in json.RawMessage) (ref, error) {
		var v ref
		return v, decode(in, &v)
	}
	return command.Spec{Name: "item.start", Action: authz.ActionChangeSettings,
		Object: func(tx *store.Tx, in json.RawMessage) (authz.Object, error) {
			v, err := parse(in)
			if err != nil {
				return authz.Object{}, err
			}
			cur, err := Get(tx, v.ID)
			if err != nil {
				return authz.Object{}, err
			}
			return object(tx, cur, cur)
		},
		Apply: func(c *command.Ctx) (any, error) {
			v, _ := parse(c.Input)
			cur, err := Get(c.Tx, v.ID)
			if err != nil {
				return nil, err
			}
			if cur.Phase != domain.PhaseOpen {
				return nil, conflict("item "+cur.ID+" is "+string(cur.Phase)+", not open", "only an open item can be started now")
			}
			b, err := json.Marshal(payload{Op: "start", Item: cur})
			if err != nil {
				return nil, err
			}
			_, err = c.Emit(domain.Event{Type: EvStartRequested, Object: cur.ID, Reason: "start now", Payload: b})
			return ref{cur.ID}, err
		}}
}
