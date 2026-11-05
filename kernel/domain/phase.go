// Package domain 是全部内核模块共用的领域类型与错误契约（设计 §6、§17.3、附录 B）。
// 只有类型、枚举、纯校验与映射表；不含持久化与命令逻辑。
package domain

// Phase 是事项的六个固定阶段（§6.4）。
type Phase string

const (
	PhaseProposed   Phase = "proposed"
	PhaseOpen       Phase = "open"
	PhaseWaiting    Phase = "waiting"
	PhaseNeedsInput Phase = "needs_input"
	PhaseDone       Phase = "done"
	PhaseDropped    Phase = "dropped"
)

// Phases 按生命周期顺序列出全部阶段。
var Phases = []Phase{PhaseProposed, PhaseOpen, PhaseWaiting, PhaseNeedsInput, PhaseDone, PhaseDropped}

func (p Phase) Valid() bool {
	for _, x := range Phases {
		if p == x {
			return true
		}
	}
	return false
}

// Terminal 为终态（可重开，不删除）。
func (p Phase) Terminal() bool { return p == PhaseDone || p == PhaseDropped }
