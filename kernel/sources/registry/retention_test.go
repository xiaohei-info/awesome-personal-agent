package registry

import (
	"errors"
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// ACT-RET-01：保留天数只有 Web 能设并记 Event；null 读回为空（不清理）；普通更新不碰它。
func TestSetContentRetention(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegister, pull("src_ret")); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "src_ret").ContentRetentionDays; got != nil {
		t.Fatalf("default must be no clearing, got %d", *got)
	}

	if _, err := e.do(owner, CmdSetRetention, m{"id": "src_ret", "days": 1}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "src_ret").ContentRetentionDays; got == nil || *got != 1 {
		t.Fatalf("days = %v, want 1", got)
	}
	if n := slices.Index(e.types(t), EvUpdated); n < 0 {
		t.Fatalf("setting retention must record an Event: %v", e.types(t))
	}

	// 同一个值再设不记第二条事件
	before := len(e.types(t))
	if _, err := e.do(owner, CmdSetRetention, m{"id": "src_ret", "days": 1}); err != nil {
		t.Fatal(err)
	}
	if len(e.types(t)) != before {
		t.Fatal("unchanged value recorded an Event")
	}

	// 来源的普通更新不改保留天数
	if _, err := e.do(owner, CmdUpdate, m{"id": "src_ret", "schedule": "30m"}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "src_ret").ContentRetentionDays; got == nil || *got != 1 {
		t.Fatalf("update changed retention: %v", got)
	}

	if _, err := e.do(owner, CmdSetRetention, m{"id": "src_ret", "days": nil}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "src_ret").ContentRetentionDays; got != nil {
		t.Fatalf("null must read back as no clearing, got %d", *got)
	}
}

func TestSetContentRetentionOnlyWebAndValidInput(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegister, pull("src_ret")); err != nil {
		t.Fatal(err)
	}
	// 保留期是 3a：档位只整理的 delegate 得 23（档位未放开）；内容不变
	if _, err := e.do(e.bearer(t, auth.Delegate), CmdSetRetention, m{"id": "src_ret", "days": 5}); exit(err) != 23 {
		t.Fatalf("delegate exit = %d (%v), want 23", exit(err), err)
	}
	if e.get(t, "src_ret").ContentRetentionDays != nil {
		t.Fatal("delegate changed retention")
	}
	for name, in := range map[string]m{
		"zero":     {"id": "src_ret", "days": 0},
		"negative": {"id": "src_ret", "days": -3},
		"huge":     {"id": "src_ret", "days": MaxRetentionDays + 1},
		"fraction": {"id": "src_ret", "days": 1.5},
		"string":   {"id": "src_ret", "days": "7"},
		"missing":  {"id": "src_ret"},
		"unknown":  {"id": "src_ret", "days": 3, "extra": 1},
	} {
		if _, err := e.do(owner, CmdSetRetention, in); exit(err) != 2 {
			t.Errorf("%s: exit = %d (%v), want invalid_request", name, exit(err), err)
		}
	}
	if _, err := e.do(owner, CmdSetRetention, m{"id": "src_none", "days": 3}); exit(err) != 5 {
		t.Errorf("unknown source: exit = %d (%v)", exit(err), err)
	}
}

// 处理方式只有 Web 本人能改：开放了"改日常规则"的 Agent 得 20（§9.3）。
func TestSetHandlingModeOwnerOnly(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegister, pull("src_mode")); err != nil {
		t.Fatal(err)
	}
	agent := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "a", Via: domain.ViaCLI}, Level: authz.LevelDelegate, CredentialAgentID: "agent_a"}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "card", func(p *policy.Policy) error {
			p.AgentCards = map[string]policy.Card{"agent_a": {Groups: map[string]policy.GroupGrant{policy.GroupRules: {}}, Tool: policy.PermRead}}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(agent, CmdSetHandling, m{"id": "src_mode", "mode": domain.HandlingRemindOnly}); exit(err) != 20 {
		t.Fatalf("agent: %v", err)
	}
	if got := e.get(t, "src_mode").HandlingMode; got != domain.HandlingNormal {
		t.Fatalf("mode = %q", got)
	}
}

// 直接立项（§9.3）：处理来源的 Agent 的权限卡没开"替我拍板"时不能选，带类别码拒绝，处理方式不变。
func TestCreateItemNeedsDecideOnTheHandlingAgentsCard(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegister, pull("src_mode")); err != nil {
		t.Fatal(err)
	}
	_, err := e.do(owner, CmdSetHandling, m{"id": "src_mode", "mode": domain.HandlingCreateItem})
	var de *domain.Error
	if !errors.As(err, &de) || de.Reason != ReasonCreateItemNeedsDecide {
		t.Fatalf("want %s, got %v", ReasonCreateItemNeedsDecide, err)
	}
	if got := e.get(t, "src_mode").HandlingMode; got != domain.HandlingNormal {
		t.Fatalf("mode = %q", got)
	}
}
