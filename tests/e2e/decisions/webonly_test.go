//go:build e2e

package decisions

import (
	"net/http"
	"strings"
	"testing"
)

const defaultCategories = `["external_commitment","funds","irreversible","permission_change","spec_activation"]`

// TestFlowWebOnly 对应 flow-web-only（ACT-WEB-01..07）。
func TestFlowWebOnly(t *testing.T) {
	f := setup(t)
	var fin string

	t.Run("ACT-WEB-01 配置高风险策略", func(t *testing.T) {
		before := f.webOK("GET", "/settings/policy", "", 200)
		if !strings.Contains(before, defaultCategories) {
			t.Fatalf("默认类别缺失: %s", before)
		}
		n := len(f.events("policy", "policy.changed"))
		f.webOK("PUT", "/settings/policy", `{"web_only":{"categories":`+defaultCategories+`,"rules":[{"tag":"acc-finance","category":"funds"}]}}`, 200)
		after := f.webOK("GET", "/settings/policy", "", 200)
		if !strings.Contains(after, `"tag":"acc-finance"`) || !strings.Contains(after, `"category":"funds"`) {
			t.Errorf("规则未生效: %s", after)
		}
		if got := len(f.events("policy", "policy.changed")); got != n+1 {
			t.Errorf("policy.changed 应新增一条: %d → %d", n, got)
		}
	})

	t.Run("ACT-WEB-02 CLI 代答 web_only 决策返回 20", func(t *testing.T) {
		fin = f.task("fin-1", "acc-work", "acc-finance")
		f.ask(fin)
		r := f.apa(f.DEL, 20, "decide", fin, "--option", "A", "--evidence", "同意")
		for _, want := range []string{`"code":"web_only"`, `"why"`, `"next"`, "/items/" + fin} {
			if !strings.Contains(r.Stderr, want) {
				t.Errorf("退出码 20 的输出缺 %s: %s", want, r.Stderr)
			}
		}
		eq(t, "阶段", f.phase(fin), "needs_input")
		eq(t, "risk_class", f.riskClass(fin), "funds")
		if f.resolution(fin) != nil {
			t.Error("决策不应生效")
		}
	})

	t.Run("ACT-WEB-03 接受 web_only 建议同样返回 20", func(t *testing.T) {
		p := f.proposal("fin-prop", "acc-finance")
		f.apa(f.DEL, 20, "accept", p, "--evidence", "同意")
		eq(t, "阶段", f.phase(p), "proposed")
	})

	t.Run("ACT-WEB-04 创建者不能降低门槛", func(t *testing.T) {
		id := f.task("fin-2", "acc-work", "acc-finance")
		body := `{"ask":{"question":"付款？","options":[{"id":"A","label":"是"},{"id":"B","label":"否"}],"risk_class":"none"}}`
		// 以 worker 身份经 HTTP 提交；risk_class 属于内核字段，必须被拒绝，且不得降低门槛。
		if r := f.APIRaw("POST", "/v1/items/"+id+"/ask", body, f.WRK); r.Code < 400 || r.Code >= 500 {
			t.Errorf("带 risk_class 的创建者请求应被拒绝: %d %s", r.Code, r.Body)
		}
		f.ask(id)
		eq(t, "risk_class", f.riskClass(id), "funds")
		f.apa(f.DEL, 20, "decide", id, "--option", "A", "--evidence", "同意")
		eq(t, "阶段", f.phase(id), "needs_input")
	})

	t.Run("ACT-WEB-05 web_only 不允许超时默认值", func(t *testing.T) {
		id := f.task("fin-3", "acc-work", "acc-finance")
		r := f.Apa(f.WRK, "", "ask", id, "--question", "付款？", "--option", "A=是", "--option", "B=否",
			"--deadline", "2099-01-01T00:00:00Z", "--default-on-timeout", "A")
		if r.Code == 0 {
			// 接受请求时必须已清除默认值。
			if a, _ := f.item(id)["ask"].(map[string]any); a["default_on_timeout"] != nil {
				t.Errorf("默认值未清除: %v", a)
			}
		} else if r.Code != 2 {
			t.Errorf("被拒绝应是 invalid_request(2)，实际 %d %s", r.Code, r.Stderr)
		}
		if got := f.phase(id); got == "open" && r.Code != 2 {
			t.Errorf("请求既未被拒绝也未进入 needs_input: %s", got)
		}
		if r.Code != 0 {
			eq(t, "被拒后阶段", f.phase(id), "open")
		}
	})

	t.Run("ACT-WEB-06 本人在 Web 确认", func(t *testing.T) {
		f.webOK("POST", "/items/"+fin+"/decide", `{"choice":"A"}`, http.StatusOK)
		r := f.resolution(fin)
		eq(t, "resolved_via", r["resolved_via"], "web")
		eq(t, "choice", r["choice"], "A")
		eq(t, "阶段", f.phase(fin), "open")
	})

	t.Run("ACT-WEB-07 被拒绝的命令可追溯", func(t *testing.T) {
		why := f.apa(f.OBS, 0, "why", fin).Stdout
		if !strings.Contains(why, `"command.rejected"`) {
			t.Errorf("apa why 没有拒绝记录: %s", why)
		}
		var rej *step
		for _, s := range f.events(fin, "command.rejected") {
			s := s
			if s.Payload["exit"] == float64(20) {
				rej = &s
			}
		}
		if rej == nil {
			t.Fatal("审计页查不到退出码 20 的 command.rejected")
		}
		eq(t, "发起者类型", rej.Actor.Type, "agent")
		if rej.Actor.Principal == "" || rej.Actor.OnBehalfOf != "owner" {
			t.Errorf("缺发起凭据: %+v", rej.Actor)
		}
		eq(t, "code", rej.Payload["code"], "web_only")
	})
}
