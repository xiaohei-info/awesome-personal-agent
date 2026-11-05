// Package screening 是 web_only 初筛返回前的读屏蔽（设计 §14.4，附录 B.3）。
//
// 初筛判定、置位与清除、代答 22 / 运行凭据 4 的授权都在内核（kernel/screening、kernel/authz、kernel/command）；
// 投递、`apa context`、审计、`apa why` 的屏蔽也在内核读模型里。事项读取（GET /items、GET /items/{id}，
// `apa inbox` 与 `apa item show` 的数据源）是唯一经 HTTP 层直出事项的路径，所以屏蔽放在这里：
// 只有 Web 本人看得到初筛中决策请求的问题与选项，其他凭据只得到 id、阶段和 screening_pending 标记。
package screening

import (
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// Pending 报告事项是否有 web_only 初筛未返回的决策请求。
func Pending(it domain.Item) bool { return it.Ask != nil && it.Ask.ScreeningPending }

// Redact 返回 level 能看到的事项：Web 或初筛已返回时原样；否则只留 id、阶段、版本、kind 和"初筛中"标记，
// 不含标题、正文、问题、选项、标签、委派人等任何可能带账本内容的字段。
func Redact(it domain.Item, level authz.Level) domain.Item {
	if level == authz.LevelWeb || !Pending(it) {
		return it
	}
	return domain.Item{ID: it.ID, Phase: it.Phase, Version: it.Version, Kind: it.Kind,
		Ask: &domain.Ask{AskKernelState: domain.AskKernelState{ScreeningPending: true}}}
}
