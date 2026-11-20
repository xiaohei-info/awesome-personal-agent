package policy

import (
	"net/http"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 策略段按档判（§14.3）：Agent 档位放开到 3a 才能改 3a 段，没放开是 23；3b 段对任何 Agent 都是硬线 20，不读档位。
// 档位本身只有 Web 本人能设。
func TestSettingsSectionsFollowAgentTier(t *testing.T) {
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

	persona := `{"persona":"terse"}`
	if rec := e.do("PUT", "/v1/settings/policy", persona, agent); rec.Code != http.StatusForbidden || errCode(t, rec) != domain.CodeTierClosed {
		t.Fatalf("3a while organize only: want 23 tier_closed, got %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/agents/agent_a/tier", `{"tier":"rules"}`, agent); errCode(t, rec) != domain.CodeWebOnly {
		t.Fatalf("an Agent must not set its own tier: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/agents/agent_a/tier", `{"tier":"rules"}`, e.web); rec.Code != http.StatusOK {
		t.Fatalf("owner sets tier: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/policy", persona, agent); rec.Code != http.StatusOK {
		t.Fatalf("3a once opened: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/access-limits", `{"permission_cap":{"default":"full","by_harness":{}}}`, agent); errCode(t, rec) != domain.CodeWebOnly {
		t.Fatalf("3b stays a hard line even at the top tier: %d %s", rec.Code, rec.Body)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		_, p, err := kpolicy.Stored(tx)
		if p.Persona != "terse" {
			t.Fatalf("persona not saved: %q", p.Persona)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// refused：设置路由对没放开档位的钥匙的三种拒绝口径之一。
func refused(c domain.Code) bool {
	return c == domain.CodeWebOnly || c == domain.CodeTierClosed || c == domain.CodePermissionDenied
}
