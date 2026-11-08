//go:build e2e

package decisions

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// grantFor 由本人在 Web 签发授权单并返回 id；ttl 是有效期，projects 是范围。
func (f *fx) grantFor(cred string, ttl time.Duration, projects ...string) string {
	f.t.Helper()
	ps, _ := json.Marshal(projects)
	body := `{"grantee":{"credential":"` + cred + `"},"scope":{"projects":` + string(ps) + `},"actions":["decision.answer","proposal.accept"],"expires_at":"` +
		time.Now().Add(ttl).UTC().Format(time.RFC3339) + `","reason":"away"}`
	var g struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(f.webOK("POST", "/settings/grants", body, 201)), &g); err != nil || g.ID == "" {
		f.t.Fatalf("签发授权单没有返回 id: %v", err)
	}
	return g.ID
}

// delID 取 DEL 凭据的 id（授权单的 grantee）。
func (f *fx) delID() string {
	f.t.Helper()
	var out struct {
		Credentials []struct{ ID, Label string } `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(f.webOK("GET", "/settings/tokens", "", 200)), &out); err != nil {
		f.t.Fatal(err)
	}
	for _, c := range out.Credentials {
		if c.Label == "DEL" {
			return c.ID
		}
	}
	f.t.Fatal("找不到 DEL 凭据")
	return ""
}

// TestFlowGrantBasis 对应 flow-authz-matrix、flow-decision-answer 的授权单依据（ACT-AUTH-03、ACT-DEC-03、ACT-CHAT-01/03、ACT-REV-01）：
// 全程经 apa 二进制的全局 --grant 走 X-APA-Grant。
func TestFlowGrantBasis(t *testing.T) {
	f := setup(t)
	del := f.delID()
	work := f.Projects["acc-work"]
	g := f.grantFor(del, time.Hour, work)

	t.Run("授权单代答生效，resolved_via=grant，apa why 与审计可见", func(t *testing.T) {
		id := f.task("grant-ok", "acc-work")
		f.ask(id)
		f.apa(f.DEL, 0, "decide", id, "--option", "A", "--grant", g)
		eq(t, "resolved_via", f.resolution(id)["resolved_via"], "grant")
		if why := f.apa(f.OBS, 0, "why", id).Stdout; !strings.Contains(why, g) {
			t.Errorf("apa why 应带授权单 id %s: %s", g, why)
		}
		if got := f.last(id, "decision.resolved"); got.Actor.OnBehalfOf == "" {
			t.Errorf("Event 应带 on_behalf_of: %+v", got)
		}
		if es := f.events("", "grant.issued"); len(es) == 0 {
			t.Error("审计缺 grant.issued")
		}
	})

	t.Run("Web 撤销后同一授权单代答为 23，事项留在 needs_input 且拒绝可见", func(t *testing.T) {
		id := f.task("grant-revoked", "acc-work")
		f.ask(id)
		f.webOK("POST", "/settings/grants/"+g+"/revoke", `{"reason":"back home"}`, 200)
		r := f.apa(f.DEL, 23, "decide", id, "--option", "A", "--grant", g)
		for _, want := range []string{`"code":"basis_required"`, `"why"`, `"next"`} {
			if !strings.Contains(r.Stderr, want) {
				t.Errorf("退出码 23 的输出缺 %s: %s", want, r.Stderr)
			}
		}
		eq(t, "阶段", f.phase(id), "needs_input")
		if f.resolution(id) != nil {
			t.Error("决策不应生效")
		}
		if rej := f.last(id, "command.rejected"); rej.Payload["code"] != "basis_required" {
			t.Errorf("应记 command.rejected: %+v", rej)
		}
		if !strings.Contains(f.webOK("GET", "/items/"+id, "", 200), `"last_rejection"`) {
			t.Error("事项应暴露 last_rejection")
		}
	})

	t.Run("已过期的授权单为 23", func(t *testing.T) {
		id := f.task("grant-expired", "acc-work")
		f.ask(id)
		short := f.grantFor(del, 2*time.Second, work)
		time.Sleep(3 * time.Second)
		f.apa(f.DEL, 23, "decide", id, "--option", "A", "--grant", short)
		eq(t, "阶段", f.phase(id), "needs_input")
	})

	t.Run("范围不符的授权单为 23", func(t *testing.T) {
		id := f.task("grant-scope", "acc-work")
		f.ask(id)
		other := f.grantFor(del, time.Hour, f.Projects["acc-open"])
		f.apa(f.DEL, 23, "decide", id, "--option", "A", "--grant", other)
		eq(t, "阶段", f.phase(id), "needs_input")
	})

	live := f.grantFor(del, time.Hour, work)

	t.Run("--evidence 与 --grant 同带为 2", func(t *testing.T) {
		id := f.task("grant-both", "acc-work")
		f.ask(id)
		f.apa(f.DEL, 2, "decide", id, "--option", "A", "--grant", live, "--evidence", "同意")
		if f.resolution(id) != nil {
			t.Error("决策不应生效")
		}
	})

	t.Run("授权单碰 web_only 决策为 20", func(t *testing.T) {
		f.webOK("PUT", "/settings/policy", `{"web_only":{"categories":`+defaultCategories+`,"rules":[{"tag":"acc-finance","category":"funds"}]}}`, 200)
		id := f.task("grant-fin", "acc-work", "acc-finance")
		f.ask(id)
		f.apa(f.DEL, 20, "decide", id, "--option", "A", "--grant", live)
		if f.resolution(id) != nil {
			t.Error("决策不应生效")
		}
	})

	t.Run("运行身份带 --grant 为 4", func(t *testing.T) {
		id := f.task("grant-run", "acc-work")
		f.ask(id)
		f.apa(f.WRK, 4, "decide", id, "--option", "A", "--grant", live)
		if f.resolution(id) != nil {
			t.Error("决策不应生效")
		}
	})

	t.Run("授权单接受建议，且 apa grant list 只列有效单", func(t *testing.T) {
		p := f.proposal("grant-prop")
		open := f.grantFor(del, time.Hour, f.Projects["acc-open"])
		f.apa(f.DEL, 0, "accept", p, "--grant", open)
		eq(t, "阶段", f.phase(p), "open")
		out := f.apa(f.DEL, 0, "grant", "list").Stdout
		if !strings.Contains(out, live) || strings.Contains(out, g) {
			t.Errorf("grant list 应含有效单、不含已撤销单: %s", out)
		}
	})
}
