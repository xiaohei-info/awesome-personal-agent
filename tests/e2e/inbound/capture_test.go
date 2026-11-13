//go:build e2e

package inbound

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

type captureResp struct {
	Captured bool   `json:"captured"`
	Reason   string `json:"reason"`
}

// capture 以 token 提交一段对话增量（POST /v1/capture），返回状态码与响应。
func (k *kit) capture(token, project, text string) (int, captureResp) {
	k.t.Helper()
	body, _ := json.Marshal(map[string]any{"text": text, "project": project, "host": "acc-stub"})
	r := k.APIRaw("POST", "/v1/capture", string(body), token)
	var out captureResp
	_ = json.Unmarshal([]byte(r.Body), &out)
	return r.Code, out
}

// captureEvents 是内部来源 capture:<项目> 的事件（只对 Web 登录可见）。
func (k *kit) captureEvents(project string) []SrcEvent { return k.events("capture:"+project, "") }

// captureIDs 是内部来源现有事件的 id 集合，用来在下一次提交后认出新增的那一条。
func (k *kit) captureIDs(project string) map[string]bool {
	ids := map[string]bool{}
	for _, e := range k.captureEvents(project) {
		ids[e.ID] = true
	}
	return ids
}

// waitCapture 等内部来源里出现一条不在 seen 里、且已处置的新事件并返回它。
func (k *kit) waitCapture(project string, seen map[string]bool) SrcEvent {
	k.t.Helper()
	var ev SrcEvent
	if !k.WaitFor(wait, func() bool {
		for _, e := range k.captureEvents(project) {
			if !seen[e.ID] && e.Disposition != "pending" && e.Disposition != "" {
				ev = e
				return true
			}
		}
		return false
	}) {
		k.t.Fatalf("内部来源 capture:%s 在 %v 内没有新的已处置事件: %+v\n%s", project, wait, k.captureEvents(project), k.ServiceLog())
	}
	return ev
}

// deliveries 用 CHAN 领取一次渠道投递（wait=1），返回状态码。
func (k *kit) claim() int {
	return k.APIRaw("GET", "/v1/deliveries/next?wait=1", "", k.CHAN).Code
}

// extract 是 triage-extract 的合法输出：{"candidates":[…]}，每项必有 title、kind（内置包的 output_schema）。
func extract(cands ...map[string]any) string {
	for _, c := range cands {
		c["kind"] = "task"
	}
	b, _ := json.Marshal(map[string]any{"candidates": cands})
	return string(b)
}

func TestFlowCapture(t *testing.T) {
	k := newKit(t)
	open := k.Projects["acc-open"]
	var capTwoItems []string

	k.step("ACT-CAP-01 默认关闭时不捕获", func(t *testing.T) {
		before := len(k.proposed())
		k.clear()
		code, out := k.capture(k.WRK, open, "下周我要把合同发给小王")
		if code != http.StatusOK || out.Captured || out.Reason == "" {
			t.Fatalf("应返回 200 {captured:false, reason}: %d %+v", code, out)
		}
		quiet()
		eq(t, "CLS_LOG 新请求", len(k.Cls.Batches())-k.clsOff, 0)
		eq(t, "MODEL_LOG 新请求", len(k.ModelStub.Requests())-k.modelOff, 0)
		eq(t, "建议流条数", len(k.proposed()), before)
	})

	k.step("ACT-CAP-02 开启 acc-open 的捕获", func(t *testing.T) {
		k.webOK("PATCH", "/v1/projects/"+open, `{"capture":true}`, 200)
		var p struct {
			Capture bool `json:"capture"`
		}
		k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "project", "show", open).Stdout), &p))
		if !p.Capture {
			t.Fatalf("apa project show 应显示捕获已开启: %s", k.apa(k.OBS, 0, "project", "show", open).Stdout)
		}
	})

	cap2 := "cap-two 下周三前把合同发给小王；另外周五要订会议室"
	k.step("ACT-CAP-03 一段含两件事的对话先预筛再抽取，得到两条建议", func(t *testing.T) {
		before := len(k.proposed())
		k.Cls.PYes(keyOf("cap-two"), 0.90)
		wed := weekday(time.Now().AddDate(0, 0, 1), time.Wednesday).Format("2006-01-02")
		k.ModelStub.When("cap-two", replyJSON(extract(
			map[string]any{"title": "把合同发给小王", "deadline": wed}, map[string]any{"title": "周五订会议室"})))
		k.clear()
		code, out := k.capture(k.WRK, open, cap2)
		if code != 200 || !out.Captured {
			t.Fatalf("开启后应捕获: %d %+v", code, out)
		}
		ev := k.waitCapture(open, map[string]bool{})
		eq(t, "disposition", ev.Disposition, "proposed")
		if len(ev.Items) != 2 {
			t.Fatalf("内部来源事件的 items 应含两条事项 id: %+v", ev)
		}
		capTwoItems = ev.Items
		titles := map[string]bool{}
		for _, id := range ev.Items {
			it := k.item(id)
			eq(t, "phase", it.Phase, "proposed")
			eq(t, "project", it.Project, open)
			titles[it.Title] = true
		}
		if !titles["把合同发给小王"] || !titles["周五订会议室"] {
			t.Errorf("两条建议的标题不符: %v", titles)
		}
		eq(t, "建议流新增", len(k.proposed()), before+2)
		if n := len(k.clsAbout("cap-two")); n != 1 {
			t.Errorf("CLS_LOG 应先有一道 capture-prescreen 题: %d", n)
		}
		if n := len(k.modelAbout("cap-two", false)); n != 1 {
			t.Errorf("MODEL_LOG 应有一次 triage-extract 请求: %d", n)
		}
		if n := len(k.modelAbout("cap-two", true)); n != 0 {
			t.Errorf("预筛已定案，不应有同题重答: %d", n)
		}
		if k.claim() != http.StatusNoContent {
			t.Error("CHAN 不应领到投递")
		}
	})

	k.step("ACT-CAP-04 预筛为否时生成型零调用", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.PYes(keyOf("cap-chat"), 0.03)
		k.clear()
		k.capture(k.WRK, open, "cap-chat 好的谢谢，今天天气不错")
		ev := k.waitCapture(open, seen)
		eq(t, "disposition", ev.Disposition, "ignored")
		if len(k.clsAbout("cap-chat")) == 0 {
			t.Error("CLS_LOG 应有这道预筛题")
		}
		quiet()
		eq(t, "MODEL_LOG 新请求", len(k.ModelStub.Requests())-k.modelOff, 0)
		eq(t, "建议流条数", len(k.proposed()), before)
	})

	k.step("ACT-CAP-05 预筛落在下限之上即抽取（宁可多抽）", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.PYes(keyOf("cap-maybe"), 0.30)
		k.ModelStub.When("cap-maybe", replyJSON(extract(map[string]any{"title": "cap-maybe 回电话给房东"})))
		k.clear()
		k.capture(k.WRK, open, "cap-maybe 回头记得给房东回电话")
		ev := k.waitCapture(open, seen)
		eq(t, "disposition", ev.Disposition, "proposed")
		eq(t, "抽取请求数", len(k.modelAbout("cap-maybe", false)), 1)
		eq(t, "同题重答数", len(k.modelAbout("cap-maybe", true)), 0)
		eq(t, "建议流新增", len(k.proposed()), before+1)
		eq(t, "标题", k.item(ev.Items[0]).Title, "cap-maybe 回电话给房东")
	})

	k.step("ACT-CAP-06 抽取出零条候选时不建建议", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.PYes(keyOf("cap-zero"), 0.90)
		k.ModelStub.When("cap-zero", replyJSON(`{"candidates":[]}`))
		k.clear()
		k.capture(k.WRK, open, "cap-zero 我们讨论了一下架构")
		ev := k.waitCapture(open, seen)
		eq(t, "disposition", ev.Disposition, "ignored")
		eq(t, "抽取请求数", len(k.modelAbout("cap-zero", false)), 1)
		eq(t, "建议流条数", len(k.proposed()), before)
	})

	k.step("ACT-CAP-07 判断端点不可用时直接交生成型抽取", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.SetFault(modelstub.ClsFault{Status: 503})
		defer k.Cls.SetFault(modelstub.ClsFault{})
		k.ModelStub.When("cap-nocls", replyJSON(extract(map[string]any{"title": "cap-nocls 交报销单"})))
		k.clear()
		k.capture(k.WRK, open, "cap-nocls 明天交报销单")
		ev := k.waitCapture(open, seen)
		eq(t, "disposition", ev.Disposition, "proposed")
		eq(t, "建议流新增", len(k.proposed()), before+1)
		eq(t, "标题", k.item(ev.Items[0]).Title, "cap-nocls 交报销单")
	})

	k.step("ACT-CAP-08 两个端点都不可用时捕获的增量记未判定，不丢", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.SetFault(modelstub.ClsFault{Status: 503})
		defer k.Cls.SetFault(modelstub.ClsFault{})
		k.ModelStub.When("cap-down", failReply(500))
		k.capture(k.WRK, open, "cap-down 下周一体检")
		ev := k.waitCapture(open, seen)
		eq(t, "disposition", ev.Disposition, "unjudged")
		eq(t, "unjudged_reason", ev.Unjudged, "model_unavailable")
		eq(t, "建议流条数", len(k.proposed()), before)
	})

	k.step("ACT-CAP-09 同一段对话重复提交不产生重复候选", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.clear()
		code, _ := k.capture(k.WRK, open, cap2)
		eq(t, "HTTP 状态", code, 200)
		quiet()
		eq(t, "内部来源事件数", len(k.captureEvents(open)), len(seen))
		eq(t, "建议流条数", len(k.proposed()), before)
		eq(t, "第二次 cap-two 抽取请求数", len(k.modelAbout("cap-two", false)), 0)
		if len(capTwoItems) != 2 {
			t.Error("ACT-CAP-03 应已得到两条建议")
		}
	})

	k.step("ACT-CAP-10 运行身份下不生效", func(t *testing.T) {
		k.EnableStubRuns()
		_, _, env := k.StubRun("hang")
		tok, run := env["APA_TOKEN"], env["APA_RUN"]
		if tok == "" || run == "" {
			t.Fatalf("取不到运行凭据: %v", env)
		}
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.clear()
		body, _ := json.Marshal(map[string]any{"text": "cap-run 记一下要买票", "project": open})
		r := k.APIRaw("POST", "/v1/capture", string(body), tok)
		var out captureResp
		_ = json.Unmarshal([]byte(r.Body), &out)
		if r.Code != 200 || out.Captured || out.Reason == "" {
			t.Fatalf("运行身份应返回 200 {captured:false, reason}: %d %s", r.Code, r.Body)
		}
		quiet()
		eq(t, "内部来源事件数", len(k.captureEvents(open)), len(seen))
		eq(t, "建议流条数", len(k.proposed()), before)
		eq(t, "CLS_LOG 含 cap-run 的批次", len(k.clsAbout("cap-run")), 0)
		eq(t, "MODEL_LOG 含 cap-run 的请求", k.modelAny("cap-run"), 0)
	})

	k.step("ACT-CAP-11 捕获的原文只对 Web 登录可见", func(t *testing.T) {
		path := "/v1/sources/capture:" + open + "/events"
		eq(t, "OBS 读内部来源", k.APIRaw("GET", path, "", k.OBS).Code, 404)
		w := k.Web("GET", path, "")
		if w.Code != 200 || !strings.Contains(w.Body, "events") {
			t.Errorf("Web 登录应读到事件列表: %d %s", w.Code, w.Body)
		}
		list := k.APIRaw("GET", "/v1/sources", "", k.OBS).Body
		if strings.Contains(list, "capture:") {
			t.Errorf("GET /v1/sources 不应含 capture 内部来源: %s", list)
		}
	})

	k.step("ACT-CAP-12 恢复 acc-open 的捕获设置", func(t *testing.T) {
		k.webOK("PATCH", "/v1/projects/"+open, `{"capture":false}`, 200)
		var p struct {
			Capture bool `json:"capture"`
		}
		k.must(json.Unmarshal([]byte(k.apa(k.OBS, 0, "project", "show", open).Stdout), &p))
		if p.Capture {
			t.Error("apa project show 应显示捕获已关闭")
		}
	})
}
