package inbound

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

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
	// EvCaptureFailed 是一次没能捕获的记录，诊断页据它列出最近失败；只记类别与时间，不含对话内容与凭据。
	EvCaptureFailed = domain.EvCaptureFailed

	// CaptureKind 是内部来源的语义类型，CaptureEvent 是事件类型（能力包规则可据此匹配）。
	CaptureKind  = "capture"
	CaptureEvent = "conversation.delta"

	capturePrefix = "capture:"
)

// 未收下的原因（响应里的 reason）。
const (
	ReasonRunIdentity     = "run_identity"
	ReasonCaptureDisabled = "capture_disabled"
)

// 没能捕获的原因类别（EvCaptureFailed 的 class）。第一类内核自己判定；其余由插件在下一次成功连上内核时补报，
// 内核不可达与没有凭据的那几次只有插件知道。
const (
	FailDisabled        = "capture_disabled" // 项目的 capture 被直接调 API 关掉
	FailUnreachable     = "kernel_unreachable"
	FailNoCredentials   = "no_credentials"
	FailRefused         = "kernel_refused" // 内核答了但拒绝（4xx、5xx）
	FailPlugin          = "plugin_error"
	maxReportedFailures = 20
)

var pluginFailClasses = map[string]bool{FailUnreachable: true, FailNoCredentials: true, FailRefused: true, FailPlugin: true}

// reportedFailure 是插件补报的一次失败：发生时间与类别。
type reportedFailure struct {
	At    time.Time `json:"at"`
	Class string    `json:"class"`
}

type captureIn struct {
	Project  string            `json:"project,omitempty"`
	Path     string            `json:"path,omitempty"`
	Host     string            `json:"host,omitempty"`
	Text     string            `json:"text"`
	Failures []reportedFailure `json:"failures,omitempty"`
}

// failed 记一次没能捕获：只有类别与时间。at 为零取当前时刻。
func failed(c *command.Ctx, class, project string, at time.Time) error {
	if at.IsZero() || at.After(c.Now()) {
		at = c.Now()
	}
	pl, _ := json.Marshal(map[string]string{"class": class, "project": project, "at": at.UTC().Format(time.RFC3339)})
	_, err := c.Emit(domain.Event{Type: EvCaptureFailed, Object: "capture-failures", Reason: "conversation delta not captured: " + class, Payload: pl})
	return err
}

// CaptureOut 是 CmdCapture 的结果；Captured 为假时不写来源事件，Reason 说明原因（只写机制）；项目捕获被关掉时另记一条 EvCaptureFailed（只有类别与时间）。
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
	return x.Register(command.Spec{Name: CmdCapture, Action: authz.ActionCreateProposed, Tier: authz.TierOrganize, Receipt: true,
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
			for i, f := range in.Failures {
				if i == maxReportedFailures {
					break
				}
				if !pluginFailClasses[f.Class] {
					f.Class = FailPlugin
				}
				if err := failed(c, f.Class, "", f.At); err != nil {
					return nil, err
				}
			}
			id, _, err := projects.ResolveOrInbox(c.Tx, projects.Scope{Flag: in.Project, Cwd: in.Path, Host: in.Host})
			if err != nil {
				return nil, err
			}
			p, err := projects.Get(c.Tx, id)
			if err != nil {
				return nil, err
			}
			if !p.Capture {
				return CaptureOut{Reason: ReasonCaptureDisabled, Project: id}, failed(c, FailDisabled, id, time.Time{})
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
