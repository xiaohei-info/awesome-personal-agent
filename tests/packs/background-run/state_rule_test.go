package backgroundrun

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/background"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// handle-labelled 是状态规则（trigger: state，C.6）：按事项的当前状态求值，同一事项自动只开工一次。

func (e *env) runsFor(id string) []proc.Record {
	var out []proc.Record
	for _, r := range e.runs_() {
		if r.Run.Target == id {
			out = append(out, r)
		}
	}
	return out
}

// events 返回挂在事项上、类型为 typ 的事件载荷。
func (e *env) events(id, typ string) []map[string]any {
	e.t.Helper()
	var out []map[string]any
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, id)
		for _, x := range es {
			if x.Event.Type == typ {
				var p map[string]any
				_ = json.Unmarshal(x.Event.Payload, &p)
				out = append(out, p)
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

// createOpen 是 Web“新建事项”直接建成 open 并勾了“交给 Agent”：没有任何 item.phase_changed。
func (e *env) createOpen(title string, tags []string) string {
	e.t.Helper()
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", map[string]any{"kind": "task", "title": title, "phase": "open", "tags": tags}), &out)
	return out.ID
}

func (e *env) drain(rounds int) {
	e.t.Helper()
	for range rounds {
		if err := e.eng.Drain(bg); err != nil {
			e.t.Fatal(err)
		}
	}
}

// 新建即 open 开工一次且只一次；之后连跑多轮扫描、事项再被改动，都不重复起。
func TestStateRuleNewOpenItemStartsOnce(t *testing.T) {
	e := newEnv(t, "")
	e.auto(mainRule)
	id := e.createOpen("新建就交出去", []string{"agent-ok"})
	rs := e.runsFor(id)
	if len(rs) != 1 || rs[0].Run.Basis == nil || rs[0].Run.Basis.RuleID != mainRule {
		t.Fatalf("runs = %+v", rs)
	}
	e.finish(rs[0].Run.ID) // 跑完没回执：事项转 needs_input
	e.drain(5)
	e.must(owner, "item.update", map[string]any{"id": id, "title": "改个标题", "reason": "edit"})
	e.drain(5)
	if n := len(e.runsFor(id)); n != 1 {
		t.Fatalf("runs after repeated scans = %d", n)
	}
	if f := e.events(id, rules.EvFired); len(f) != 1 || f[0]["trigger"] != "state" {
		t.Fatalf("rule.fired = %+v", f)
	}
}

// 自动跑完没标完成：事项转 needs_input；本人把它放回 open 也不会被自动再开工（连带后果三），
// 本人点“现在开工”能再起一次，且记录写明是手动。
func TestStateRuleManualStartRunsAgainAfterAuto(t *testing.T) {
	e := newEnv(t, "")
	e.auto(mainRule)
	id := e.createOpen("跑完没收尾", []string{"agent-ok"})
	first := e.runsFor(id)
	if len(first) != 1 {
		t.Fatalf("runs = %d", len(first))
	}
	e.finish(first[0].Run.ID)
	if got := e.item(id).Phase; got != domain.PhaseNeedsInput {
		t.Fatalf("phase after a run that left it open = %s", got)
	}
	e.must(owner, "item.transition", map[string]any{"id": id, "to": "open", "reason": "接着处理"})
	e.drain(3)
	if n := len(e.runsFor(id)); n != 1 {
		t.Fatalf("back to open restarted it automatically: runs = %d", n)
	}
	e.must(owner, "item.start", map[string]any{"id": id})
	if n := len(e.runsFor(id)); n != 2 {
		t.Fatalf("start now: runs = %d", n)
	}
	fired := e.events(id, rules.EvFired)
	if len(fired) != 2 || fired[1]["trigger"] != "manual" {
		t.Fatalf("rule.fired = %+v", fired)
	}
	launched := e.events(id, background.EvLaunched)
	if len(launched) != 2 || launched[0]["manual"] != false || launched[1]["manual"] != true {
		t.Fatalf("background.launched = %+v", launched)
	}
}

// 事项已有在途运行：不再起，也不占掉幂等记录。
func TestStateRuleSkipsItemWithRunInFlight(t *testing.T) {
	e := newEnv(t, "")
	e.auto(mainRule)
	id := e.createOpen("别人在跑", nil)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: id, TimeoutSec: 60}, External: true}, t0)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	e.must(owner, "item.update", map[string]any{"id": id, "tags": []string{"agent-ok"}, "reason": "交给 Agent"})
	e.drain(3)
	if n := len(e.runsFor(id)); n != 1 {
		t.Fatalf("runs = %d, want only the one already in flight", n)
	}
	if f := e.events(id, rules.EvFired); len(f) != 0 {
		t.Fatalf("rule.fired = %+v, a busy item must not use up its once", f)
	}
}

// 规则关掉（off）时完全不动作：不拉起、不留观察记录、不留触发记录。
func TestStateRuleOffDoesNothing(t *testing.T) {
	e := newEnv(t, "")
	e.must(owner, "rule.set_mode", map[string]string{"rule": mainRule, "mode": "off"})
	id := e.createOpen("关着", []string{"agent-ok"})
	e.drain(3)
	if n, o, f := len(e.runs_()), len(e.events(id, rules.EvObserved)), len(e.events(id, rules.EvFired)); n+o+f != 0 {
		t.Fatalf("off rule acted: runs=%d observed=%d fired=%d", n, o, f)
	}
}

// observe 档：每件只记一条“本来会做什么”，多轮扫描不重复；切到 auto 后再开工。
func TestStateRuleObserveRecordsOnceThenAutoStarts(t *testing.T) {
	e := newEnv(t, "")
	id := e.createOpen("先看看", []string{"agent-ok"})
	e.drain(3)
	if n, o := len(e.runs_()), len(e.events(id, rules.EvObserved)); n != 0 || o != 1 {
		t.Fatalf("observe: runs=%d observed=%d", n, o)
	}
	e.auto(mainRule)
	if n := len(e.runsFor(id)); n != 1 {
		t.Fatalf("after switching to auto: runs = %d", n)
	}
}

// 从等待转回进行中会开工（等待期间不开工）。
func TestStateRuleWaitingBackToOpenStarts(t *testing.T) {
	e := newEnv(t, "")
	e.auto(mainRule)
	var out struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "item.create", map[string]any{"kind": "task", "title": "等人", "phase": "waiting", "tags": []string{"agent-ok"},
		"waiting_on": map[string]any{"kind": "external", "ref": "x"}}), &out)
	if n := len(e.runsFor(out.ID)); n != 0 {
		t.Fatalf("waiting item started: runs = %d", n)
	}
	e.must(owner, "item.transition", map[string]any{"id": out.ID, "to": "open", "reason": "等到了"})
	if n := len(e.runsFor(out.ID)); n != 1 {
		t.Fatalf("back to open: runs = %d", n)
	}
}

// 选中的 Agent 不可用（权限卡上的工具档位不在它的声明里）时不开工、不留记录；可用之后下一轮扫描开工。
func TestStateRuleWaitsForUsableAgent(t *testing.T) {
	e := newEnv(t, "")
	setCap := func(c string) {
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			_, err := policy.Update(tx, owner.Actor, "t", func(p *policy.Policy) error { p.AgentCards["hx"] = policy.DefaultCard().WithTool(c); return nil })
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	setCap(policy.PermFull) // 声明只有 read、edit
	e.auto(mainRule)
	id := e.createOpen("Agent 还不行", []string{"agent-ok"})
	e.drain(2)
	if n, f := len(e.runsFor(id)), len(e.events(id, rules.EvFired)); n != 0 || f != 0 {
		t.Fatalf("not ready: runs=%d fired=%d", n, f)
	}
	setCap(policy.PermEdit)
	e.drain(1)
	if n := len(e.runsFor(id)); n != 1 {
		t.Fatalf("agent usable again: runs = %d", n)
	}
}
