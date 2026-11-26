//go:build e2e

package judge

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// §8.9 表逐格验收：每个用途（模板）× 低于下限 / 不确定带内 / 高于上限 / 各级未定 / 端点都不可用，
// 各一条用例，都经真实触发路径（来源事件、决策请求）走真实二进制，不测内核内部函数。
// 投递推迟（delivery-defer）与跟进推迟（follow-up-defer）两个模板已取消（2026-11-23），它们的格随之删除。
// 表里没有的格（只写一端的模板没有"带"）在用例名里注明取值：带内即 p 恰等于该端的值。

func (f *fx) webItem(id string) map[string]any {
	f.t.Helper()
	m := jsonMap(f.t, f.webOK("GET", "/items/"+id, "", 200))
	if it := sub(m, "item"); it != nil {
		return it
	}
	return m
}

// judgeConfigured 报告判断端点（分类端点）当前是否配置。
func (f *fx) judgeConfigured() bool {
	return sub(jsonMap(f.t, f.webOK("GET", "/settings/model-endpoints", "", 200)), "judge_endpoint") != nil
}

// down 让两个端点对所有请求回 503（对话端点的判断题与生成请求都不可用）。
func (f *fx) down() {
	f.Cls.SetFault(modelstub.ClsFault{Status: 503})
	f.ModelStub.SetDefault(modelstub.Reply{Status: 503})
}

// ---- triage-screen（所有来源共用，对话捕获也走它）：下限 0.15，上限 0.60 ----

func TestMatrixSourceScreen(t *testing.T) {
	f := setup(t)
	cell := func(name string, setup func(mark string), want string) {
		f.step(t, name, func(t *testing.T) {
			f.reset()
			mark := "scr-matrix-" + strings.Fields(name)[0]
			setup(mark)
			e := f.settled(f.write(ev{Subject: mark, ID: mark})[0], 90*time.Second)
			eq(t, "处置", e.Disposition, want)
		})
	}
	cell("low 低于下限 0.03：忽略", func(m string) { f.cls(m, 0.03) }, "ignored")
	cell("band 带内（p 恰等于下限 0.15，对话端点回是）：处理", func(m string) { f.cls(m, 0.15); f.chat(m, "yes", true) }, "proposed")
	cell("high 高于上限 0.90：处理", func(m string) { f.cls(m, 0.90) }, "proposed")
	cell("undecided 各级未定（带内且对话端点资料不足）：按处理", func(m string) { f.cls(m, 0.30); f.chat(m, "no", false) }, "proposed")
	cell("unavail 判断端点不可用（对话端点接手回是）：处理照常", func(m string) {
		f.Cls.SetFault(modelstub.ClsFault{Status: 503})
		f.chat(m, "yes", true)
	}, "proposed")
	f.step(t, "unavail2 两个端点都不可用：记未判定，不丢", func(t *testing.T) {
		f.reset()
		f.down()
		e := f.settled(f.write(ev{Subject: "scr-matrix-both", ID: "scr-matrix-both"})[0], 90*time.Second)
		eq(t, "处置", e.Disposition, "unjudged")
	})
}
