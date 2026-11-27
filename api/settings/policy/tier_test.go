package policy

import (
	"net/http"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	kpolicy "github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 策略段按组判（§14.3）：Agent 的权限卡开放"改日常规则与提醒"才能改这一组的段，没开是 23；
// 只有本人的段（权限卡、渠道回答人）对任何 Agent 都是 20，不读权限卡。权限卡本身只有 Web 本人能设。
func TestSettingsSectionsFollowPermissionCard(t *testing.T) {
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

	change := `{"background_run_timeout_sec":1800}`
	rules := `{"groups":{"view":{},"organize":{},"source_events":{},"rules":{}},"tool":"read"}`
	if rec := e.do("PUT", "/v1/settings/policy", change, agent); rec.Code != http.StatusForbidden || errCode(t, rec) != domain.CodeTierClosed {
		t.Fatalf("rules while closed: want 23 tier_closed, got %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/agents/agent_a/card", rules, agent); errCode(t, rec) != domain.CodeWebOnly {
		t.Fatalf("an Agent must not set its own card: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/agents/agent_a/card", rules, e.web); rec.Code != http.StatusOK {
		t.Fatalf("owner sets the card: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/policy", change, agent); rec.Code != http.StatusOK {
		t.Fatalf("rules once opened: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/access-limits", `{"agent_cards":{}}`, agent); errCode(t, rec) != domain.CodeWebOnly {
		t.Fatalf("the cards stay owner-only even with every group open: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("PUT", "/v1/settings/agents/agent_a/card", `{"groups":{"purge":{}}}`, e.web); rec.Code != http.StatusBadRequest {
		t.Fatalf("purge without an expiry is refused: %d %s", rec.Code, rec.Body)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		_, p, err := kpolicy.Stored(tx)
		if p.BackgroundRunTimeoutSec != 1800 {
			t.Fatalf("change not saved: %d", p.BackgroundRunTimeoutSec)
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
