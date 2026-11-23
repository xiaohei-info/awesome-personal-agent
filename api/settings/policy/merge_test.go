package policy

import (
	"strings"
	"testing"
)

// 设置 › 跨来源合并：缺段的提示词 400 且不生效；合格的生效并记下保存时刻；空串恢复内置默认；执行合并的 Agent 只认 same 与 off。
func TestMergeSettingsValidateAndRestore(t *testing.T) {
	e := setup(t)
	good := "## 任务\n归类。\n## 四种结论\nattach merge duplicate keep\n## 判定依据\n同一件事。\n## 不许做的事\n不编造。"
	if rec := e.do("PUT", "/v1/settings/merge", `{"merge":{"prompt":"## 任务\n只有一段 attach merge duplicate keep"}}`, e.web); rec.Code != 400 {
		t.Fatalf("bad prompt: %d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.Merge.Prompt != "" {
		t.Fatal("a rejected prompt must not take effect")
	}
	body := `{"merge":{"exclude_sources":["src_mail"],"skip_existing":true,"batch_size":10,"wait_minutes":60,"prompt":` + jsonString(good) + `}}`
	if rec := e.do("PUT", "/v1/settings/merge", body, e.web); rec.Code != 200 {
		t.Fatalf("good prompt: %d %s", rec.Code, rec.Body)
	}
	_, p := e.current()
	if p.Merge.Prompt != good || p.Merge.PromptAt == nil || p.Merge.BatchSize != 10 || !p.Merge.SkipExisting || p.Merge.ExcludeSources[0] != "src_mail" {
		t.Fatalf("saved %+v", p.Merge)
	}
	if rec := e.do("PUT", "/v1/settings/merge", `{"merge":{"prompt":""}}`, e.web); rec.Code != 200 {
		t.Fatalf("restore: %d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.Merge.Prompt != "" || p.Merge.PromptAt == nil {
		t.Fatalf("restore default keeps when it changed: %+v", p.Merge)
	}
	if rec := e.do("PUT", "/v1/settings/merge", `{"merge_judge":"judge"}`, e.web); rec.Code != 400 {
		t.Fatalf("judge is no longer an option: %d", rec.Code)
	}
	if rec := e.do("GET", "/v1/settings/merge", "", e.web); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"merge"`) {
		t.Fatalf("get: %d %s", rec.Code, rec.Body)
	}
}

func jsonString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s) + `"`
}

// 一批资料上限（budget_tokens）：0、负数、过小、过大都 400 并写明可用范围，不生效；合法值保存后读回；不带这个键恢复默认。
func TestMergeBudgetTokensValidated(t *testing.T) {
	e := setup(t)
	for _, n := range []string{"0", "-1", "1999", "500001"} {
		rec := e.do("PUT", "/v1/settings/merge", `{"merge":{"budget_tokens":`+n+`}}`, e.web)
		if rec.Code != 400 || !strings.Contains(rec.Body.String(), "use 2000 to 500000") {
			t.Fatalf("budget %s: %d %s", n, rec.Code, rec.Body)
		}
		if _, p := e.current(); p.Merge.BudgetTokens != nil {
			t.Fatalf("a rejected budget must not take effect: %d", *p.Merge.BudgetTokens)
		}
	}
	if rec := e.do("PUT", "/v1/settings/merge", `{"merge":{"budget_tokens":500000}}`, e.web); rec.Code != 200 {
		t.Fatalf("good budget: %d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.Merge.Budget() != 500000 {
		t.Fatalf("saved %+v", p.Merge)
	}
	if rec := e.do("GET", "/v1/settings/merge", "", e.web); !strings.Contains(rec.Body.String(), `"budget_tokens":500000`) {
		t.Fatalf("get: %s", rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/merge", `{"merge":{}}`, e.web); rec.Code != 200 {
		t.Fatalf("back to default: %d %s", rec.Code, rec.Body)
	}
	if _, p := e.current(); p.Merge.BudgetTokens != nil {
		t.Fatalf("without the key the budget is the default again: %+v", p.Merge)
	}
}
