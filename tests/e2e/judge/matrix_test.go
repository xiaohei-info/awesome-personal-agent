//go:build e2e

package judge

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// §8.9 表逐格验收：每个用途（模板）× 低于下限 / 不确定带内 / 高于上限 / 各级未定 / 端点都不可用，
// 各一条用例，都经真实触发路径（捕获、决策请求、决策投递、跟进到期）走真实二进制，不测内核内部函数。
// 表里没有的格（只写一端的模板没有"带"）在用例名里注明取值：带内即 p 恰等于该端的值。

const (
	capMark   = "cap-matrix"
	deferQ    = "这条决策可以晚点送吗"
	fuDeferQ  = "这一轮提醒可以晚点提吗"
	screenQ   = baseline.ScreeningPhrase
	extractOK = `{"candidates":[{"title":"matrix 抽取出的事项","kind":"task"}]}`
)

func (f *fx) workTask(title string) string {
	f.t.Helper()
	r := f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--assignee", "work-host.cli",
		"--project", f.Projects["acc-work"], "--evidence", "建任务")
	return jsonMap(f.t, r.Stdout)["id"].(string)
}

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

func (f *fx) ask(id string) {
	f.t.Helper()
	f.apa(f.WRK, 0, "ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A")
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

// ---- web-only-screening：下限 0.20，single_shot ----

func TestMatrixWebOnlyScreening(t *testing.T) {
	f := setup(t)
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"inject","decision":"channel:acc-chan","health":"web","reminder":"inject","wake":"web"},"merge_window_sec":0,"remind_on_deadline":true}}`, 200)
	cell := func(name string, setup func(), wantWebOnly bool) {
		f.step(t, name, func(t *testing.T) {
			f.reset()
			setup()
			id := f.workTask("scr-" + strings.Fields(name)[0])
			f.ask(id)
			f.waitFor("初筛结束", 60*time.Second, func() bool {
				p, _ := sub(f.webItem(id), "ask")["screening_pending"].(bool)
				return !p
			})
			got := sub(f.webItem(id), "ask")["risk_class"] == "web_only"
			eq(t, "升级为 web_only", got, wantWebOnly)
			if n := len(f.judgeRequests()); n != 0 && wantWebOnly && f.judgeConfigured() {
				t.Errorf("single_shot 不升级到对话端点，MODEL_LOG 判断题 = %d", n)
			}
		})
	}
	cell("low 低于下限 0.05：不升级", func() { f.cls(screenQ, 0.05) }, false)
	cell("band 带内（p 恰等于下限 0.20）：升级", func() { f.cls(screenQ, 0.20) }, true)
	cell("high 高于上限（无上限，p=0.90）：升级", func() { f.cls(screenQ, 0.90) }, true)
	cell("undecided 各级未定（只配对话端点且自报资料不足，取保守默认是）：升级", func() {
		f.webOK("PUT", "/settings/model-endpoints", `{"judge_endpoint": null}`, 200)
		f.chat(screenQ, "no", false)
	}, true)
	cell("unavail 端点都不可用：按规则的结果继续，不升级也不等待", func() { f.down() }, false)
}

// ---- delivery-defer：上限 0.85，single_shot，默认关闭 ----

func TestMatrixDeliveryDefer(t *testing.T) {
	f := setup(t)
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"inject","decision":"channel:acc-chan","health":"web","reminder":"inject","wake":"web"},"merge_window_sec":120,"remind_on_deadline":true,"threshold_judge":true}}`, 200)
	claim := func(wait string) int { return f.APIRaw("GET", "/v1/deliveries/next?wait="+wait, "", f.CHAN).Code }
	for claim("0") == 200 { // 清掉此前的投递
	}
	cell := func(name string, setup func(), wantDeferred bool) {
		f.step(t, name, func(t *testing.T) {
			f.reset()
			f.cls(screenQ, 0.05) // 决策本身不是 web_only
			setup()
			id := f.workTask("dfr-" + strings.Fields(name)[0])
			f.ask(id)
			code := claim("3")
			if wantDeferred {
				eq(t, "立即领取", code, 204)
				f.TCAdvance(130)
				eq(t, "窗口结束后领取", claim("10"), 200)
			} else {
				eq(t, "立即领取", code, 200)
			}
			if wantDeferred && len(f.judgeRequests()) != 0 {
				t.Error("single_shot 不升级到对话端点")
			}
		})
	}
	cell("low 低于上限 0.50：按规则路由", func() { f.cls(deferQ, 0.50) }, false)
	cell("band 带内（p 恰等于上限 0.85）：按规则路由", func() { f.cls(deferQ, 0.85) }, false)
	cell("high 高于上限 0.95：推迟到合并窗口结束后单独投递", func() { f.cls(deferQ, 0.95) }, true)
	cell("undecided 各级未定（按题错误取保守默认否）：按规则路由", func() { f.clsErr(deferQ, "internal") }, false)
	cell("unavail 端点都不可用：按规则路由", func() { f.down() }, false)
}

// ---- follow-up-defer：上限 0.85，默认关闭，至多连续跳过一轮 ----

func TestMatrixFollowupDefer(t *testing.T) {
	f := setup(t)
	dst := filepath.Join(f.Home, "packs", "follow-up")
	must(t, os.CopyFS(dst, os.DirFS("../../../packs/follow-up")))
	p := filepath.Join(dst, "pack.yaml")
	b, err := os.ReadFile(p)
	must(t, err)
	must(t, os.WriteFile(p, []byte(strings.Replace(string(b), "schedule: 72h", "schedule: 1m", 1)), 0o644))
	f.StopKernel()
	f.StartKernelAgain()
	f.Web("PATCH", "/v1/rules/follow-up%2Fclock_rules%2Ffollow-up-interval/mode", `{"mode":"auto"}`) // 已是 auto 时返回 400，无妨
	f.webOK("PUT", "/settings/policy", `{"disturb":{"routes":{"brief":"web","decision":"web","health":"web","reminder":"web","wake":"web"},"merge_window_sec":0,"remind_on_deadline":false,"followup_defer":true}}`, 200)
	cell := func(name string, setup func(), wantDeferred bool) {
		f.step(t, name, func(t *testing.T) {
			f.reset()
			setup()
			id := jsonMap(t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", "fud-"+strings.Fields(name)[0], "--phase", "open",
				"--assignee", "work-host.cli", "--project", f.Projects["acc-work"], "--evidence", "建任务").Stdout)["id"].(string)
			f.apa(f.DEL, 0, "item", "transition", id, "--to", "waiting", "--waiting-on", "external:装修公司报价", "--reason", "等报价", "--evidence", "等报价")
			f.TCAdvance(65)
			f.waitFor("第一次到期有结论", 60*time.Second, func() bool {
				return len(f.audit(id, "followup.deferred"))+len(f.audit(id, "followup.reminded")) > 0
			})
			eq(t, "被推迟一轮", len(f.audit(id, "followup.deferred")) == 1, wantDeferred)
			eq(t, "第一轮已提醒", len(f.audit(id, "followup.reminded")) == 1, !wantDeferred)
		})
	}
	cell("low 低于上限 0.50：照常提醒", func() { f.cls(fuDeferQ, 0.50) }, false)
	cell("band 带内（p 恰等于上限 0.85，对话端点回否）：照常提醒", func() { f.cls(fuDeferQ, 0.85); f.chat(fuDeferQ, "no", true) }, false)
	cell("high 高于上限 0.95：跳过这一轮", func() { f.cls(fuDeferQ, 0.95) }, true)
	cell("undecided 各级未定（带内且资料不足，保守默认否）：照常提醒", func() { f.cls(fuDeferQ, 0.85); f.chat(fuDeferQ, "yes", false) }, false)
	cell("unavail 端点都不可用：照常提醒", func() { f.down() }, false)
}
