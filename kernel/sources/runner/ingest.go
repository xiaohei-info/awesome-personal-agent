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
	// Rejects 是转换脚本输出里没能成为事件的行（含超过单条上限的），与事件同一事务留痕。
	Rejects []Reject `json:"rejects,omitempty"`
}

// ParseEvents 解析转换脚本的 stdout：每行一个标准事件，不含游标行；原文总量超过 maxEvent 字节的事件整行拒收。
func ParseEvents(stdout []byte, maxEvent int) (events []IngestEvent, rejects []Reject) {
	p := parse(stdout, maxEvent)
	return p.events, p.rejects
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
	rejects := in.Rejects
	accepted := 0
	for i, ev := range in.Events {
		if err := ValidEvent(ev); err != nil {
			return nil, domain.NewError(domain.CodeInvalidRequest, "event "+ev.ID+": "+err.Error(), "fix the mapping or script")
		}
		if reason := tooLarge(ev, r.maxEvent()); reason != "" { // 整条拒收并留痕，不截取一段入账
			raw, _ := json.Marshal(map[string]string{"id": ev.ID})
			rejects = append(rejects, Reject{Line: i + 1, Reason: reason, Raw: string(raw)})
			continue
		}
		if err := insertEvent(c.Tx, src, in.RunID, r.capSender(src, ev), c.Now()); err != nil {
			return nil, err
		}
		accepted++
	}
	if err := writeRejects(c.Tx, src.ID, in.RunID, r.o.Now(), rejects); err != nil {
		return nil, err
	}
	return IngestResult{SourceID: src.ID, Events: accepted, Rejected: len(rejects)}, nil
}

// IngestResult 是 CmdIngest 的返回：落账的事件数与整行拒收的行数。
type IngestResult struct {
	SourceID string `json:"source_id"`
	Events   int    `json:"events"`
	Rejected int    `json:"rejected"`
}

// Internal 是内核内部模块（webhook 接收）发 CmdIngest 的身份：来源的调度与落账归内核自己维护。
var Internal = system
