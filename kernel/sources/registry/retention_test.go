package registry

import (
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
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
	// delegate（附本人原话）也不行：web_only，退出码 20；内容不变
	if _, err := e.do(e.bearer(t, auth.Delegate), CmdSetRetention, m{"id": "src_ret", "days": 5}); exit(err) != 20 {
		t.Fatalf("delegate exit = %d (%v), want 20", exit(err), err)
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
