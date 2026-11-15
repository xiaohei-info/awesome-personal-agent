package delegation

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func dispatchOf(t *testing.T, r command.Result) Dispatch {
	t.Helper()
	var d Dispatch
	if err := json.Unmarshal(r.Value, &d); err != nil {
		t.Fatal(err)
	}
	return d
}

func (e *env) dispatches(t *testing.T, item string) []Dispatch {
	t.Helper()
	var ds []Dispatch
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ds, err = Dispatches(tx, item); return }); err != nil {
		t.Fatal(err)
	}
	return ds
}

// 委派记录（§6.17）：绑定的凭据写入、同会话去重、接续限同一项目、状态推进、子事项完成时收尾。
func TestDispatchRecordLifecycle(t *testing.T) {
	e := setup(t)
	proj := e.project(t, "dsp", nil)
	parent := e.parent(t, proj)
	child := e.delegate(t, e.as("DEL"), parent, workHost)
	add := func(id command.Identity, in map[string]any) (command.Result, error) { return e.do(id, "delegate.dispatch", in) }

	handle := map[string]any{"mech": "paseo", "host": "mbp", "agent_id": "agt_1"}
	d1 := dispatchOf(t, e.must(t, e.as("DEL"), "delegate.dispatch", map[string]any{"id": child, "session": "agt_1", "resumable": true, "cwd": "/w/app", "handle": handle}))
	if d1.Executor != workHost || d1.Harness != "h" || d1.State != DispatchRunning || !d1.Resumable || d1.Item != child || d1.Unattended {
		t.Fatalf("dispatch %+v", d1)
	}
	n := countType(e.events(t), EvDispatched)
	// 同会话去重：自动捕获与手动补记只留一条。
	if again := dispatchOf(t, e.must(t, e.as("DEL"), "delegate.dispatch", map[string]any{"id": child, "session": "agt_1"})); again.ID != d1.ID {
		t.Fatalf("same session must return the running dispatch: %+v", again)
	}
	if countType(e.events(t), EvDispatched) != n {
		t.Fatal("dedup wrote an event")
	}
	// 绑定的执行者凭据也能写；接续本项目的记录。
	d2 := dispatchOf(t, e.must(t, e.as("WRK"), "delegate.dispatch", map[string]any{"id": child, "session": "agt_2", "continues": d1.ID}))
	if d2.ID == d1.ID || d2.Continues != d1.ID || d2.Harness != "h" {
		t.Fatalf("second dispatch %+v", d2)
	}

	// 拒绝：无关的 worker 4；未绑定的 delegate 不带依据 23；没有 harness 2；坏句柄 2；可续接却没有会话 2；接续别的项目 2。
	e.wantExit(t, domain.ExitPermissionDenied, e.as("WRK2"), "delegate.dispatch", map[string]any{"id": child, "session": "x"})
	e.issue(t, "DEL2", auth.Spec{Kind: auth.Delegate})
	if _, err := e.x.Do(bg, command.Request{Identity: e.as("DEL2"), Command: "delegate.dispatch", Input: json.RawMessage(`{"id":"` + child + `","session":"y"}`), Owner: "owner"}); exitOf(err) != domain.ExitBasisRequired {
		t.Fatalf("unbound delegate: %v", err)
	}
	bare := command.Bearer{Auth: e.au, Secret: e.secret["DEL"]}
	e.wantExit(t, domain.ExitUsage, bare, "delegate.dispatch", map[string]any{"id": child, "session": "z"})
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.dispatch", map[string]any{"id": child, "handle": map[string]any{"mech": "x", "command": "rm -rf /"}})
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.dispatch", map[string]any{"id": child, "resumable": true})
	other := e.delegate(t, e.as("DEL"), e.parent(t, e.project(t, "other", nil)), workHost)
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.dispatch", map[string]any{"id": other, "continues": d1.ID})
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.dispatch", map[string]any{"id": child, "continues": "dsp_missing"})
	// 只能写在委派子事项上；交给本人的不记。
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.dispatch", map[string]any{"id": parent})
	mine := e.delegate(t, e.as("DEL"), e.parent(t, proj), "owner")
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.dispatch", map[string]any{"id": mine})
	if _, err := add(e.as("DEL"), map[string]any{"id": child}); err != nil { // 没有会话的派法（不可续接）也能记
		t.Fatal(err)
	}

	// 推进：lost 同时置不可续接，终态不可再改，失效的会话不能改回可续接。
	lost := dispatchOf(t, e.must(t, e.as("DEL"), "delegate.dispatch_update", map[string]any{"id": d1.ID, "state": "lost"}))
	if lost.State != DispatchLost || lost.Resumable || lost.EndedAt == nil {
		t.Fatalf("lost %+v", lost)
	}
	e.wantExit(t, domain.ExitConflict, e.as("DEL"), "delegate.dispatch_update", map[string]any{"id": d1.ID, "state": "ended"})
	e.wantExit(t, domain.ExitConflict, e.as("DEL"), "delegate.dispatch_update", map[string]any{"id": d1.ID, "resumable": true})
	e.wantExit(t, domain.ExitUsage, e.as("DEL"), "delegate.dispatch_update", map[string]any{"id": d2.ID, "state": "running"})

	// 子事项完成：仍在运行的记录在同一事务里转 ended，可续接保持宿主原值。
	e.must(t, e.as("WRK"), "delegate.report", rep(child, "done", Evidence{PR: "https://x/pr/7"}))
	for _, d := range e.dispatches(t, child) {
		if d.State == DispatchRunning {
			t.Fatalf("running dispatch left on a done item: %+v", d)
		}
	}
	// 终态的子事项不再接受新记录。
	e.wantExit(t, domain.ExitConflict, e.as("DEL"), "delegate.dispatch", map[string]any{"id": child, "session": "agt_9"})

	// 列表：按执行者、按事项。
	var list struct{ Dispatches []Dispatch }
	_ = json.Unmarshal(e.must(t, e.as("DEL"), "delegate.dispatch_list", map[string]any{"item": child}).Value, &list)
	if len(list.Dispatches) != 3 || list.Dispatches[0].StartedAt.Before(list.Dispatches[2].StartedAt) {
		t.Fatalf("list %+v", list.Dispatches)
	}
	e.consistent(t)
}
