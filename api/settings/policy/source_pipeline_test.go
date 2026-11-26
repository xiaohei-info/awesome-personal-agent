package policy

import (
	"net/http"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func TestSourcePipelineEndpoint(t *testing.T) {
	e := setup(t)
	// 默认读取成功。
	if rec := e.do("GET", "/v1/settings/source-pipeline", "", e.web); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"source_pipeline"`) {
		t.Fatalf("GET = %d %s", rec.Code, rec.Body)
	}
	// 合法更新保存并去空白。
	body := `{"source_pipeline":{"agent":"main","batch_size":5,"wait_minutes":15,"urgent_immediate":true,"max_batch":12,"max_retries":3,"daily_batch_limit":20,"judge_prompt":"  重要邮件优先  ","batch_prompt":"  保持简洁  "}}`
	if rec := e.do("PUT", "/v1/settings/source-pipeline", body, e.web); rec.Code != 200 {
		t.Fatalf("PUT valid = %d %s", rec.Code, rec.Body)
	}
	rec := e.do("GET", "/v1/settings/source-pipeline", "", e.web)
	for _, want := range []string{`"agent":"main"`, `"batch_size":5`, `"wait_minutes":15`, `"urgent_immediate":true`, `"max_batch":12`, `"max_retries":3`, `"daily_batch_limit":20`, `"judge_prompt":"重要邮件优先"`, `"batch_prompt":"保持简洁"`} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Errorf("GET missing %s: %s", want, rec.Body)
		}
	}
	// 越界参数返回 400。
	if rec := e.do("PUT", "/v1/settings/source-pipeline", `{"source_pipeline":{"batch_size":999}}`, e.web); rec.Code != 400 {
		t.Fatalf("out of range batch_size = %d %s", rec.Code, rec.Body)
	}
	// 未知字段返回 400。
	if rec := e.do("PUT", "/v1/settings/source-pipeline", `{"source_pipeline":{"unknown_field":1}}`, e.web); rec.Code != 400 {
		t.Fatalf("unknown field = %d %s", rec.Code, rec.Body)
	}
	// 旧的 /v1/settings/merge 路由已删除，返回 404。
	if rec := e.do("GET", "/v1/settings/merge", "", e.web); rec.Code != 404 {
		t.Fatalf("old /settings/merge = %d, want 404", rec.Code)
	}
}

// 来源处理段决定谁来跑批处理、用什么指令，归"Agent 与执行者配置"：只开规则组的 Agent 改不了，开了 Agent 组的可以。
func TestSourcePipelineNeedsAgentsGroup(t *testing.T) {
	e := setup(t)
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: auth.Delegate, AgentID: "agent_a", Harness: "h"})
		secret = s
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent := func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }
	card := func(groups string) {
		t.Helper()
		if rec := e.do("PUT", "/v1/settings/agents/agent_a/card", `{"groups":{`+groups+`},"tool":"read"}`, e.web); rec.Code != http.StatusOK {
			t.Fatalf("set card: %d %s", rec.Code, rec.Body)
		}
	}
	body := `{"source_pipeline":{"batch_prompt":"x"}}`
	card(`"view":{},"rules":{}`)
	if rec := e.do("PUT", "/v1/settings/source-pipeline", body, agent); !refused(errCode(t, rec)) {
		t.Fatalf("rules group must not change source_pipeline: %d %s", rec.Code, rec.Body)
	}
	card(`"view":{},"agents":{}`)
	if rec := e.do("PUT", "/v1/settings/source-pipeline", body, agent); rec.Code != http.StatusOK {
		t.Fatalf("agents group may change source_pipeline: %d %s", rec.Code, rec.Body)
	}
}
