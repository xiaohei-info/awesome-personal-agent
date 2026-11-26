package exec

import (
	"net/http"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 限制清单：权限卡加七项设置与改不了的规则；最近 7 天的拒绝只算这个 Agent 的，带权限卡页面。
func TestLimitsListCardSettingsAndDenials(t *testing.T) {
	e := setup(t)
	now := time.Now()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := policy.Update(tx, domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "t", func(p *policy.Policy) error {
			p.AgentCards["agent_a"] = policy.DefaultCard().With(policy.GroupDecide)
			p.Limits.ChainDepth = 5
			return nil
		}); err != nil {
			return err
		}
		for _, agent := range []string{"agent_a", "agent_b"} {
			if _, err := eventlog.Append(tx, domain.Event{Type: domain.EvCommandRejected, Object: "itm_1", At: now,
				Actor:   domain.Actor{Type: domain.ActorAgent, Principal: "p", Via: domain.ViaCLI},
				Payload: []byte(`{"action":"change_settings","tier":"purge","code":"tier_closed","reason":"tier_closed","exit":23,"agent":"` + agent + `","web_url":"/purge"}`)}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	var l Limits
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		l, err = limits(tx, "agent_a", Fallback{Long: 4, Short: 2, Chain: 8}, now)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if !l.Card.Allows(policy.GroupDecide, now) || l.ChainDepth != 5 || l.LongRuns != 4 || l.ShortRuns != 2 || !l.TrustedOnly || len(l.FixedRules) != 3 || l.RunTimeoutSec != 4*3600 {
		t.Fatalf("limits: %+v", l)
	}
	if l.DeniedWeek != 1 || len(l.Denied) != 1 || l.Denied[0].Group != "purge" || l.Denied[0].SettingsURL != "/settings/agents/agent_a#permissions" || l.Denied[0].WebURL != "/purge" {
		t.Fatalf("denials: %+v", l.Denied)
	}
}

// 外部凭据（ext、webhook）读不到限制清单；本人与 Agent 自己的钥匙照常。
func TestLimitsRouteRefusesExternalCredentials(t *testing.T) {
	e := setup(t)
	var hook string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		_, hook, err = e.au.Issue(tx, auth.Spec{Kind: auth.Webhook, Scope: "src_1"})
		return
	}); err != nil {
		t.Fatal(err)
	}
	for name, mod := range map[string]func(*http.Request){"ext": e.token(auth.Ext), "webhook": func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+hook) }} {
		if r := e.do("GET", "/v1/agents/me/limits", "", mod); r.Code != 403 || code(t, r) != domain.CodePermissionDenied {
			t.Errorf("%s reads limits: %d %s", name, r.Code, r.Body)
		}
	}
	if r := e.do("GET", "/v1/agents/me/limits", "", e.token(auth.Delegate)); r.Code != 200 {
		t.Errorf("delegate reads its own limits: %d %s", r.Code, r.Body)
	}
	if r := e.web("GET", "/v1/agents/agent_a/limits", ""); r.Code != 200 {
		t.Errorf("owner reads any limits: %d %s", r.Code, r.Body)
	}
}
