package inbound

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 对话候选捕获（设计 §9.3、§11.2）：插件在回合结束时提交对话增量，这里把它转成内部来源事件，
// 之后与其他来源事件走同一条流水线（规则之后的判断型 → 生成型），产出 proposed 事项。
// 每个项目一个内部来源 "capture:<项目 id>"，不进来源登记表；事项归属该项目。
const (
	// CmdCapture 提交一段对话增量。
	CmdCapture = "inbound.capture"
	// EvCaptured 是增量已收下的审计记录；只记事件 id 与项目，不含对话内容。
	EvCaptured = "inbound.captured"

	// CaptureKind 是内部来源的语义类型，CaptureEvent 是事件类型（能力包规则可据此匹配）。
	CaptureKind  = "capture"
	CaptureEvent = "conversation.delta"

	capturePrefix = "capture:"
)

// 未收下的原因（响应里的 reason）。
const (
	ReasonRunIdentity     = "run_identity"
	ReasonNoProject       = "no_project"
	ReasonCaptureDisabled = "capture_disabled"
)

type captureIn struct {
	Project string `json:"project,omitempty"`
	Path    string `json:"path,omitempty"`
	Host    string `json:"host,omitempty"`
	Text    string `json:"text"`
}

// CaptureOut 是 CmdCapture 的结果；Captured 为假时不写账本，Reason 说明原因（只写机制）。
type CaptureOut struct {
	Captured bool   `json:"captured"`
	Reason   string `json:"reason,omitempty"`
	Event    string `json:"event,omitempty"`
	Project  string `json:"project,omitempty"`
}

// IsCaptureSource 报告 id 是否是对话捕获的内部来源；这种来源不进登记表，内容是对话原文（§9.3）。
func IsCaptureSource(id string) bool {
	_, ok := captureProject(id)
	return ok
}

func captureProject(sourceID string) (string, bool) {
	id, ok := strings.CutPrefix(sourceID, capturePrefix)
	return id, ok && id != ""
}

func captureSource(id string) domain.Source { return domain.Source{ID: id, SourceKind: CaptureKind} }

func (s *Service) registerCapture(x *command.Executor) error {
	return x.Register(command.Spec{Name: CmdCapture, Action: authz.ActionCreateProposed, Tier: authz.TierWork, Receipt: true,
		Object: func(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{}, nil },
		Apply: func(c *command.Ctx) (any, error) {
			var in captureIn
			dec := json.NewDecoder(strings.NewReader(string(c.Input)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&in); err != nil {
				return nil, invalid("bad input: " + err.Error())
			}
			if strings.TrimSpace(in.Text) == "" {
				return nil, invalid("text is required")
			}
			if c.Caller.Level == authz.LevelObserver || c.Caller.Kind == auth.Observer {
				return nil, domain.NewError(domain.CodePermissionDenied, "credential level does not allow this action", "use a worker or higher credential")
			}
			// 运行身份下不生效（§7.7）：钩子照常触发，但不入账，防止"运行产出对话 → 捕获 → 再起运行"的回环。
			if c.Caller.Level == authz.LevelRun {
				return CaptureOut{Reason: ReasonRunIdentity}, nil
			}
			id, err := projects.Resolve(c.Tx, projects.Scope{Flag: in.Project, Cwd: in.Path, Host: in.Host})
			if err != nil {
				return nil, err
			}
			if id == "" {
				return CaptureOut{Reason: ReasonNoProject}, nil
			}
			p, err := projects.Get(c.Tx, id)
			if err != nil {
				return nil, err
			}
			if !p.Capture {
				return CaptureOut{Reason: ReasonCaptureDisabled, Project: id}, nil
			}
			// 事件 id 由内容决定：同一会话重复提交同一段增量不产生第二条来源事件。
			sum := sha256.Sum256([]byte(id + "\x00" + c.Caller.Actor.Session + "\x00" + in.Text))
			evID := "cap_" + hex.EncodeToString(sum[:12])
			ev := domain.SourceEvent{ID: evID, Type: CaptureEvent, Subject: "conversation delta", Detail: in.Text, OccurredAt: c.Now()}
			if err := runner.AppendInternal(c.Tx, captureSource(capturePrefix+id), ev); err != nil {
				return nil, err
			}
			pl, _ := json.Marshal(map[string]string{"event": evID, "project": id})
			if _, err := c.Emit(domain.Event{Type: EvCaptured, Object: evID, Reason: "conversation delta captured", Payload: pl}); err != nil {
				return nil, err
			}
			return CaptureOut{Captured: true, Event: evID, Project: id}, nil
		}})
}
