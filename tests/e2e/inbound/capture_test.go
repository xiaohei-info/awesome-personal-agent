//go:build e2e

package inbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

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

func TestFlowCapture(t *testing.T) {
	k := newKit(t)
	open := k.Projects["acc-open"]

	k.step("ACT-CAP-01 关掉项目的捕获后不捕获", func(t *testing.T) {
		before := len(k.proposed())
		// 对话捕获对大项目默认开启（§6.5，默认值迁移见 §16.5）：要验关掉时不捕获，先显式关掉。
		k.webOK("PATCH", "/v1/projects/"+open, `{"capture":false}`, 200)
		k.clear()
		code, out := k.capture(k.WRK, open, "下周我要把合同发给小王")
		if code != http.StatusOK || out.Captured || out.Reason != "capture_disabled" {
			t.Fatalf("关掉捕获后应返回 200 {captured:false, reason:capture_disabled}: %d %+v", code, out)
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
	k.step("ACT-CAP-03 对话增量与其他来源走同一条线，批处理用运行凭据读到对话原文", func(t *testing.T) {
		seen := filepath.Join(k.Root, "capture-read.txt")
		k.BatchAgent(fmt.Sprintf(`"$APA" source event "$EV" >> %q
"$APA" source propose "$EV" >/dev/null`, seen))
		defer k.BatchAgent("")
		before := len(k.proposed())
		k.Cls.PYes(keyOf("cap-two"), 0.90)
		k.clear()
		code, out := k.capture(k.WRK, open, cap2)
		if code != 200 || !out.Captured {
			t.Fatalf("开启后应捕获: %d %+v", code, out)
		}
		ev := k.waitCapture(open, map[string]bool{})
		eq(t, "disposition", ev.Disposition, "proposed")
		it := k.item(ev.Item)
		eq(t, "phase", it.Phase, "proposed")
		eq(t, "project", it.Project, open)
		eq(t, "建议流新增", len(k.proposed()), before+1)
		if b, err := os.ReadFile(seen); err != nil || !strings.Contains(string(b), "订会议室") {
			t.Errorf("运行凭据应能读到对话原文: %v %s", err, b)
		}
		if k.claim() != http.StatusNoContent {
			t.Error("CHAN 不应领到投递")
		}
	})

	k.step("ACT-CAP-04 判为不用处理的对话不进队列", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.PYes(keyOf("cap-chat"), 0.03)
		k.clear()
		k.capture(k.WRK, open, "cap-chat 好的谢谢，今天天气不错")
		ev := k.waitCapture(open, seen)
		eq(t, "disposition", ev.Disposition, "ignored")
		if len(k.clsAbout("cap-chat")) == 0 {
			t.Error("CLS_LOG 应有这道题")
		}
		eq(t, "建议流条数", len(k.proposed()), before)
		if strings.Contains(k.BatchPrompts(), ev.ID) {
			t.Error("忽略的对话不应进批处理")
		}
	})

	k.step("ACT-CAP-07 判断端点不可用时由对话端点回答同一题", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.SetFault(modelstub.ClsFault{Status: 503})
		defer k.Cls.SetFault(modelstub.ClsFault{})
		k.chatJudge("cap-nocls", "yes", true)
		k.clear()
		k.capture(k.WRK, open, "cap-nocls 明天交报销单")
		ev := k.waitCapture(open, seen)
		eq(t, "disposition", ev.Disposition, "proposed")
		eq(t, "建议流新增", len(k.proposed()), before+1)
	})

	k.step("ACT-CAP-08 两个端点都不可用时捕获的增量记未判定，不丢", func(t *testing.T) {
		before, seen := len(k.proposed()), k.captureIDs(open)
		k.Cls.SetFault(modelstub.ClsFault{Status: 503})
		defer k.Cls.SetFault(modelstub.ClsFault{})
		k.ModelStub.Judge(modelstub.ChatJudge{Key: keyOf("cap-down"), Status: 500})
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
		eq(t, "第二次 cap-two 判断请求数", len(k.clsAbout("cap-two")), 0)
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

	// 2026-11-26 改写（权限卡，§14.6）：对话捕获与其他来源一样，Web 登录与内部受众的钥匙可读，外部受众的钥匙读不到。
	k.step("ACT-CAP-11 捕获的原文对内部受众可见，外部钥匙读不到", func(t *testing.T) {
		path := "/v1/sources/capture:" + open + "/events"
		eq(t, "OBS（内部钥匙）读内部来源", k.APIRaw("GET", path, "", k.OBS).Code, 200)
		eq(t, "CHAN（外部 ext 密钥）读内部来源", k.APIRaw("GET", path, "", k.CHAN).Code, 403)
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
