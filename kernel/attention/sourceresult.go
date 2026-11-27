package attention

import (
	"strings"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// NothingToTell 是批处理说明约定的"本批没有需要本人知道的事"整段输出（source-batch 模板）。
const NothingToTell = "无需告知"

// BatchResultID 是来源处理线批处理结论的投递 id（§9.3）：按运行 id 唯一，同一次运行只投一次。
func BatchResultID(runID string) string { return "batch-result:" + runID }

// SourceResult 把一段来源批处理结论作为知会送出去（§9.3）：按投递矩阵 `source_result` 行路由（默认推给 Agent 马上），
// 受静默约束、不合并（每次批处理一条）。首个非空行作标题；整段只写 NothingToTell 时不投递；
// 空 summary 由调用方换成固定说明（故障不吞）。在调用方的事务里运行。
func (a *Attention) SourceResult(tx *store.Tx, runID, summary string) error {
	_, pol, err := policy.Current(tx)
	if err != nil {
		return err
	}
	summary = strings.TrimSpace(summary)
	if summary == "" || strings.Trim(summary, " 。.*`#「」") == NothingToTell {
		return nil
	}
	title, body := "来源处理结果", summary
	head, rest, _ := strings.Cut(summary, "\n")
	if head = strings.Trim(head, " #*"); head != "" && utf8.RuneCountInString(head) <= 100 {
		title, body = head, strings.TrimSpace(rest)
	}
	// 摘要过长时截断，保留开头的整体结论（§9.3：结论过长或为空的风险）。
	if rs := []rune(body); len(rs) > 2000 {
		body = string(rs[:2000]) + "\n…（结论较长，其余略）"
	}
	content := domain.DeliveryContent{Title: title, Summary: body}
	// 来源处理结果不属于任何事项，也没有单一来源事件与规则（一次批处理覆盖多条）：出处记批处理运行本身。
	origin := &domain.DeliveryOrigin{SourceEvent: "batch:" + runID, Rule: "source_batch"}
	return a.offer(tx, pol, held{id: BatchResultID(runID), class: domain.DeliveryReminder,
		priority: domain.PriorityNormal, version: 1, content: content, origin: origin})
}
