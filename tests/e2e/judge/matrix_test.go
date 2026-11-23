//go:build e2e

package judge

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// §8.9 表逐格验收：每个用途（模板）× 低于下限 / 不确定带内 / 高于上限 / 各级未定 / 端点都不可用，
// 各一条用例，都经真实触发路径（捕获、决策请求）走真实二进制，不测内核内部函数。
// 投递推迟（delivery-defer）与跟进推迟（follow-up-defer）两个模板已取消（2026-11-23），它们的格随之删除。
// 表里没有的格（只写一端的模板没有"带"）在用例名里注明取值：带内即 p 恰等于该端的值。

const (
	capMark   = "cap-matrix"
	extractOK = `{"candidates":[{"title":"matrix 抽取出的事项","kind":"task"}]}`
)

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

// ---- capture-prescreen：下限 0.10，没有上限 ----

func TestMatrixCapturePrescreen(t *testing.T) {
	f := setup(t)
	open := f.Projects["acc-open"]
	f.webOK("PATCH", "/projects/"+open, `{"capture":true}`, 200)
	f.webOK("PUT", "/settings/projects/"+open+"/capture-outbound", `{"value":"full"}`, 200)
	capEvents := func() []srcEvent {
		var out struct {
			Events []srcEvent `json:"events"`
		}
		must(t, json.Unmarshal([]byte(f.webOK("GET", "/sources/capture:"+open+"/events?limit=500", "", 200)), &out))
		return out.Events
	}
	// run 提交一段对话，返回新事件与生成端点收到的抽取请求数（含 mark 的非判断题请求）。
	run := func(mark string, genOK bool) (srcEvent, int) {
		seen := map[string]bool{}
		for _, e := range capEvents() {
			seen[e.ID] = true
		}
		if genOK {
			f.ModelStub.When(mark, reply(extractOK))
		}
		body, _ := json.Marshal(map[string]any{"text": mark + " 记得周五给房东回电话", "project": open, "host": "acc-stub"})
		if r := f.APIRaw("POST", "/v1/capture", string(body), f.WRK); r.Code != 200 || !strings.Contains(r.Body, `"captured":true`) {
			t.Fatalf("capture: %d %s", r.Code, r.Body)
		}
		var ev srcEvent
		f.waitFor("捕获事件处置", 60*time.Second, func() bool {
			for _, e := range capEvents() {
				if !seen[e.ID] && e.Disposition != "" && e.Disposition != "pending" {
					ev = e
					return true
				}
			}
			return false
		})
		n := 0
		for _, g := range f.genRequests() {
			if strings.Contains(g, mark) {
				n++
			}
		}
		return ev, n
	}
	cell := func(name string, setup func(mark string), wantDisp string, wantExtract int) {
		f.step(t, name, func(t *testing.T) {
			f.reset()
			mark := capMark + "-" + strings.Fields(name)[0]
			setup(mark)
			ev, n := run(mark, true)
			eq(t, "处置", ev.Disposition, wantDisp)
			eq(t, "抽取请求数", n, wantExtract)
		})
	}
	cell("low 低于下限 0.03：不抽取", func(m string) { f.cls(m, 0.03) }, "ignored", 0)
	cell("band 带内（p 恰等于下限 0.10，对话端点回是）：抽取", func(m string) { f.cls(m, 0.10); f.chat(m, "yes", true) }, "proposed", 1)
	cell("high 高于上限（无上限，p=0.90）：抽取", func(m string) { f.cls(m, 0.90) }, "proposed", 1)
	cell("undecided 各级未定（带内且对话端点资料不足）：抽取", func(m string) { f.cls(m, 0.10); f.chat(m, "no", false) }, "proposed", 1)
	// 验收文档 ACT-CAP 的口径：判断端点不可用时对话端点接手，抽取照常；两个端点都不可用才记未判定。
	cell("unavail 判断端点不可用（对话端点接手回是）：抽取照常", func(m string) {
		f.Cls.SetFault(modelstub.ClsFault{Status: 503})
		f.chat(m, "yes", true)
	}, "proposed", 1)
	f.step(t, "unavail2 两个端点都不可用：记未判定，不丢", func(t *testing.T) {
		f.reset()
		f.down()
		ev, _ := run(capMark+"-both", false)
		eq(t, "处置", ev.Disposition, "unjudged")
	})
}
