package runner

import (
	"encoding/json"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
)

// CmdIngest 把推送型来源（webhook）转换出的标准事件记入同一张来源事件表，与拉取结果走同一条去重与流水线（§9.5）。
// 不动游标与统计；事件按 (去重域, 事件 id) 去重，重放不产生重复。
const CmdIngest = "source.ingest"

// IngestEvent 是标准事件（§9.2 的字段）。
type IngestEvent = parsedEvent

// IngestInput 是 CmdIngest 的输入；RunID 记录事件的来由（接收记录 id 或转换脚本的 Run）。
type IngestInput struct {
	SourceID string        `json:"source_id"`
	RunID    string        `json:"run_id"`
	Events   []IngestEvent `json:"events"`
}

// ParseEvents 解析转换脚本的 stdout：每行一个标准事件，不含游标行。
// truncated 为真表示输出超出外呼调用器保留的末段，开头的事件已丢，调用方不应当作成功。
func ParseEvents(stdout []byte) (events []IngestEvent, rejects []Reject, truncated bool) {
	p := parse(stdout)
	return p.events, p.rejects, p.truncated
}

// ValidEvent 校验事件满足必填字段（映射产出的事件与脚本输出同一标准）。
func ValidEvent(ev IngestEvent) error {
	b, _ := json.Marshal(ev)
	_, err := asEvent(b)
	return err
}

func (r *Runner) applyIngest(c *command.Ctx) (any, error) {
	var in IngestInput
	if err := json.Unmarshal(c.Input, &in); err != nil {
		return nil, domain.NewError(domain.CodeInvalidRequest, "bad input: "+err.Error(), "fix the request and retry")
	}
	src, err := registry.Get(c.Tx, in.SourceID)
	if err != nil {
		return nil, err
	}
	if src.Type != domain.SourceWebhook {
		return nil, domain.NewError(domain.CodeInvalidRequest, "source "+src.ID+" is not a webhook source", "ingest only targets webhook sources")
	}
	for _, ev := range in.Events {
		if err := ValidEvent(ev); err != nil {
			return nil, domain.NewError(domain.CodeInvalidRequest, "event "+ev.ID+": "+err.Error(), "fix the mapping or script")
		}
		if err := insertEvent(c.Tx, src, in.RunID, ev); err != nil {
			return nil, err
		}
	}
	return map[string]any{"source_id": src.ID, "events": len(in.Events)}, nil
}

// Internal 是内核内部模块（webhook 接收）发 CmdIngest 的身份：来源的调度与落账归内核自己维护。
var Internal = system
