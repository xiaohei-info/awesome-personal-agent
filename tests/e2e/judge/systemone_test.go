//go:build e2e

package judge

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// TestFlowJudgeSystemOne：判断端点用 system-one 协议（原生分类模型服务的判断接口）时，
// 来源事件经真实二进制走完整条判断路径：设置页获取模型与测试连接、入站初筛逐题取 p_yes、按不确定带定案、判断记录留痕。
func TestFlowJudgeSystemOne(t *testing.T) {
	f := setup(t)
	cfg := map[string]any{"url": f.Cls.SystemOneURL(), "api_key": "stub-key", "protocol": "system-one", "model": "cls-stub-latest", "locality": "local"}
	f.webOK("PUT", "/settings/model-endpoints", mustJSON(map[string]any{"judge_endpoint": cfg}), 200)
	t.Cleanup(f.WireEndpoints)

	f.step(t, "设置页：获取模型、测试连接", func(t *testing.T) {
		m := jsonMap(t, f.webOK("POST", "/settings/model-endpoints/models", mustJSON(map[string]any{
			"endpoint": "judge", "url": cfg["url"], "api_key": "***", "protocol": "system-one"}), 200))
		if m["ok"] != true || len(m["models"].([]any)) != 1 {
			t.Fatalf("models: %v", m)
		}
		d := jsonMap(t, f.webOK("POST", "/settings/model-endpoints/test", mustJSON(map[string]any{"endpoint": "judge", "config": cfg}), 200))
		eq(t, "code", d["code"], "ok")
		eq(t, "probability_source", sub(d, "data")["probability_source"], "native")
	})

	f.step(t, "低于下限：分类端点定案为否，判断记录记 system-one 的答案", func(t *testing.T) {
		f.reset()
		f.cls("so-low", 0.05)
		_, j := f.one("so-low", func(e *ev) { e.Detail = "so-low 资料 SO-MATERIAL" })
		eq(t, "decided_by", j.DecidedBy, "classifier")
		eq(t, "answer", j.Answer, "no")
		eq(t, "levels[0].probability_source", j.Levels[0].ProbabilitySource, "native")
		eq(t, "levels[0].model", j.Levels[0].Model, "cls-stub-1.0")
		b := f.Cls.Batches()
		if len(b) == 0 || !strings.HasSuffix(b[0].Path, "/v1/systemone") || b[0].Header.Get("Authorization") != "Bearer stub-key" {
			t.Fatalf("没有发到 system-one 接口: %+v", b)
		}
		var req struct {
			Model     string                     `json:"model"`
			Questions map[string]json.RawMessage `json:"questions"`
		}
		must(t, json.Unmarshal(b[0].Body, &req))
		eq(t, "model", req.Model, "cls-stub-latest")
		if len(req.Questions) != 1 || !strings.Contains(b[0].BodyText, "SO-MATERIAL") || !strings.Contains(b[0].BodyText, `"type":"noul"`) {
			t.Fatalf("请求体不是一道带资料的 noul 题: %s", b[0].BodyText)
		}
	})

	f.step(t, "漏答：该题交对话端点", func(t *testing.T) {
		f.reset()
		f.Cls.Judge(modelstub.ClsJudge{Key: key("so-omit"), Omit: true})
		f.chat("so-omit", "yes", true)
		_, j := f.one("so-omit")
		eq(t, "levels[0].error_code", j.Levels[0].ErrorCode, "missing_result")
		eq(t, "decided_by", j.DecidedBy, "chat")
	})
}
