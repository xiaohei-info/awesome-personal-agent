//go:build e2e

package upgrade

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
	ledgerfixture "github.com/xiaohei-info/awesome-personal-agent/tests/fixtures/ledger"
)

// ACT-UPG-10：v0.1 账本升到最新结构后第一次启动（规则内容 hash 在 v0.1 与 v0.2 的算法下对不上的那一次）：
// 本人切换过的、默认 off 的、用户包显式写 observe 的规则模式不变且没有对应 rule.mode_changed；动作类规则不变。
func TestUpgradeChain(t *testing.T) {
	latest, err := ledgerfixture.Latest()
	if err != nil {
		t.Fatal(err)
	}
	e := baseline.New(t)
	fx := e.LedgerFixture(latest)
	e.ServeFixture(fx, map[string]string{"unjudged_rejudge_days": "0"})

	var rs struct {
		Rules []struct{ Key, Mode string }
	}
	e.WebLogin()
	if r := e.Web("GET", "/v1/rules", ""); r.Code != 200 || json.Unmarshal([]byte(r.Body), &rs) != nil {
		t.Fatalf("GET /rules = %d %s", r.Code, r.Body)
	}
	got := map[string]string{}
	for _, x := range rs.Rules {
		got[x.Key] = x.Mode
	}
	for k, want := range fx.M.RuleModes.Current {
		if got[k] != want {
			t.Errorf("rule %s mode after upgrade = %q, want %q (unchanged)", k, got[k], want)
		}
	}

	var ev struct {
		Events []struct{ Type, Object string }
	}
	if r := e.APIRaw("GET", "/v1/events?limit=500", "", e.OBS); r.Code != 200 || json.Unmarshal([]byte(r.Body), &ev) != nil {
		t.Fatalf("GET /events = %d %s", r.Code, r.Body)
	}
	var changed []string
	for _, x := range ev.Events {
		if x.Type == "rule.mode_changed" {
			changed = append(changed, x.Object)
		}
	}
	if len(changed) != len(fx.M.RuleModes.OwnerSwitched) {
		t.Errorf("rule.mode_changed on %v, want only the owner's own switches %v", changed, fx.M.RuleModes.OwnerSwitched)
	}
}
