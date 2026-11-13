//go:build e2e

package judge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// callsOf 读 GET /judgments/stats 里某用途的调用次数合计（reused 不计入）。
func (f *fx) callsOf(use string) int {
	f.t.Helper()
	var out struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.Unmarshal([]byte(f.APIRaw("GET", "/v1/judgments/stats?use="+use, "", f.OBS).Body), &out); err != nil {
		f.t.Fatal(err)
	}
	n := 0
	for _, r := range out.Rows {
		if c, ok := r["calls"].(float64); ok {
			n += int(c)
		}
	}
	return n
}

// assertNoChatLogprobsOption：chat-logprobs 在 SPIKE-LOGPROB 结论出来前不开放（ACT-JDP-11）。
// Web 设置页的"协议"下拉框只列出 API 与前端构建里存在的选项；这里核对前端产物与设置 API 两处。
func assertNoChatLogprobsOption(t *testing.T, f *fx) {
	t.Helper()
	page := f.Web("GET", "/", "")
	if page.Code != 200 {
		t.Fatalf("Web 首页 HTTP %d", page.Code)
	}
	for _, asset := range assetPaths(page.Body) {
		r := f.Web("GET", asset, "")
		if r.Code == 200 && strings.Contains(r.Body, "chat-logprobs") {
			t.Errorf("前端资源 %s 出现 chat-logprobs 选项", asset)
		}
	}
	if strings.Contains(f.webOK("GET", "/settings/model-endpoints", "", 200), "chat-logprobs") {
		t.Error("设置 API 的形状里出现 chat-logprobs")
	}
}

func assetPaths(html string) (out []string) {
	for _, part := range strings.Split(html, `"`) {
		if strings.HasPrefix(part, "/assets/") {
			out = append(out, part)
		}
	}
	return
}

// assertChatLogprobs（ACT-JDP-12）：若 API 层接受 chat-logprobs，在替身上走换算；否则（实现按 SPIKE-LOGPROB 口径拒绝）
// 以内核一侧的换算单元测试为准，由 TestChatLogprobsConversion 承接。这里只验证拒绝是干净的 4xx 且配置不变。
func assertChatLogprobs(t *testing.T, f *fx) {
	t.Helper()
	f.reset()
	b, _ := json.Marshal(map[string]any{"judge_endpoint": map[string]any{"url": f.ModelStub.URL(), "protocol": "chat-logprobs", "model": "acc-model", "locality": "local"}})
	r := f.Web("PUT", "/v1/settings/model-endpoints", string(b))
	if r.Code >= 400 && r.Code < 500 {
		after := f.webOK("GET", "/settings/model-endpoints", "", 200)
		if strings.Contains(after, "chat-logprobs") {
			t.Errorf("被拒绝的请求不应改动配置: %s", after)
		}
		return
	}
	if r.Code != 200 {
		t.Fatalf("PUT chat-logprobs: HTTP %d %s", r.Code, r.Body)
	}
	defer f.setJudge(nil)
	f.ModelStub.Judge(modelstub.ChatJudge{Key: key("jdp-lp-a"), PYes: pf(0.8320)})
	f.ModelStub.Judge(modelstub.ChatJudge{Key: key("jdp-lp-b"), PYes: pf(0.5)})
	a := f.write(ev{Subject: "jdp-lp-a", ID: "jdp-lp-a"})[0]
	_, ja := f.judgmentOf(a)
	eq(t, "probability_source", ja.Levels[0].ProbabilitySource, "logprobs")
	if ja.Levels[0].PYes == nil || *ja.Levels[0].PYes < 0.82 || *ja.Levels[0].PYes > 0.84 {
		t.Errorf("p_yes = %v，期望约 0.832", ja.Levels[0].PYes)
	}
}
