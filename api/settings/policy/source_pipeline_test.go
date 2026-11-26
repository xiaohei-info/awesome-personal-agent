package policy

import (
	"strings"
	"testing"
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
