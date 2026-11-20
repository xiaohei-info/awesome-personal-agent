package decisions

import (
	"encoding/json"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 二档档位未放开（退出码 23）：被拒的写入不生效，open、waiting 的事项阶段与版本不变，不产出问题，只多一条 command.rejected（§14.5）。
// 2026-11-20 改写：此前断言的是"转 needs_input 并挂单选项「继续」的内核问题"，该做法已取消（问题只给需要本人决定的事，§6.14 第 20 条）。
func TestBasisRequiredTurnsOpenAndWaitingItemsToNeedsInput(t *testing.T) {
	e := setup(t)
	open := e.item(t, map[string]any{})
	waiting := e.item(t, map[string]any{"phase": "waiting", "waiting_on": map[string]any{"kind": "human", "ref": "bob"}})
	for _, id := range []string{open, waiting} {
		before := e.get(t, id)
		nEv := len(e.events(t, id))
		if _, err := e.do(delegate, "proposal.accept", "", map[string]any{"id": id}); exit(err) != domain.ExitBasisRequired {
			t.Fatalf("%s: want 23, got %v", id, err)
		}
		it := e.get(t, id)
		if it.Phase != before.Phase || it.Ask != nil || it.Version != before.Version || (before.WaitingOn != nil) != (it.WaitingOn != nil) {
			t.Fatalf("%s changed by a rejected write: %+v -> %+v", id, before, it)
		}
		es := e.events(t, id)
		if len(es) != nEv+1 || es[len(es)-1].Type != domain.EvCommandRejected || es[len(es)-1].Actor.Principal != "dlg" {
			t.Fatalf("%s events: %+v", id, es[nEv:])
		}
	}
	e.verify(t)
}

// 升级前留下的单选项内核问题（system:proxy-rejection 写的「继续」）降级为通知：回到挂问题前的阶段，不写回答（不替本人选），
// 拒绝记录保留；幂等。本人自己（或 agent）挂的问题不动。
func TestKernelAskIsWebOnlyAndAnswerReturnsToOpen(t *testing.T) {
	e := setup(t)
	legacy := func(phase map[string]any) string {
		id := e.item(t, phase)
		if err := e.st.Update(bg, func(tx *store.Tx) error { // 按旧版 onReject 的写法造出存量数据
			// 新不变量不让写单选项问题：先写两个选项，再像旧数据一样把第二个从投影与快照里去掉。
			_, _, snap, err := items.Revise(tx, id, "decision.request", e.now, func(it *domain.Item) error {
				it.Ask = &domain.Ask{AskInput: domain.AskInput{Question: "legacy", Options: []domain.AskOption{{ID: "continue", Label: "继续"}, {ID: "x", Label: "x"}}},
					AskKernelState: domain.AskKernelState{RiskClass: domain.RiskClassWebOnly}}
				it.Phase, it.WaitingOn = domain.PhaseNeedsInput, nil
				return nil
			})
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE items SET data = json_remove(data, '$.ask.options[1]') WHERE id = ?`, id); err != nil {
				return err
			}
			var m map[string]any
			_ = json.Unmarshal(snap, &m)
			ask := m["item"].(map[string]any)["ask"].(map[string]any)
			ask["options"] = ask["options"].([]any)[:1]
			snap, _ = json.Marshal(m)
			if _, err := command.Bump(tx, id); err != nil {
				return err
			}
			_, err = eventlog.Append(tx, domain.Event{Type: domain.EvDecisionRequested, Object: id, Reason: "proxy write rejected: basis required",
				Actor: domain.Actor{Type: domain.ActorSystem, Principal: ProxyRejectionActor, Via: domain.ViaClock}, Payload: snap, At: e.now})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	open := legacy(map[string]any{})
	waiting := legacy(map[string]any{"phase": "waiting", "waiting_on": map[string]any{"kind": "human", "ref": "bob"}})
	mine := e.asked(t, nil) // 普通问题：不撤
	var got []string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { got, err = WithdrawProxyRejectionAsks(tx, e.now); return }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("withdrawn = %v", got)
	}
	if it := e.get(t, open); it.Phase != domain.PhaseOpen || it.Ask != nil {
		t.Fatalf("open legacy: %+v", it)
	}
	if it := e.get(t, waiting); it.Phase != domain.PhaseWaiting || it.WaitingOn == nil || it.WaitingOn.Ref != "bob" || it.Ask != nil {
		t.Fatalf("waiting legacy: %+v", it)
	}
	if it := e.get(t, mine); it.Phase != domain.PhaseNeedsInput || it.Ask == nil {
		t.Fatalf("an ordinary question was touched: %+v", it)
	}
	if ev := e.last(t, open, domain.EvItemPhaseChanged); ev.Actor.Type != domain.ActorSystem || ev.ResolvedVia != "" {
		t.Fatalf("withdraw event: %+v", ev)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { got, err = WithdrawProxyRejectionAsks(tx, e.now); return }); err != nil || len(got) != 0 {
		t.Fatalf("second withdraw: %v %v", got, err)
	}
	e.verify(t)
}

// §6.14 第 20 条：只有一个选项又不能写别的想法的问题不能构造；零选项也不行；两个选项、或单选项加别的想法、或只写别的想法可以。
func TestAskNeedsARealChoice(t *testing.T) {
	e := setup(t)
	id := e.item(t, map[string]any{"assignee": "wrk"})
	for name, a := range map[string]map[string]any{
		"one option":   {"question": "q", "options": []map[string]any{{"id": "A", "label": "a"}}},
		"no options":   {"question": "q"},
		"blank option": {"question": "q", "options": []map[string]any{{"id": "A", "label": "a"}, {"id": "B", "label": " "}}},
	} {
		if _, err := e.do(worker, "decision.request", "", map[string]any{"id": id, "ask": a}); exit(err) != domain.ExitUsage {
			t.Fatalf("%s: want usage error, got %v", name, err)
		}
	}
	if _, err := e.do(worker, "item.update", "", map[string]any{"id": id, "ask": map[string]any{"question": "q", "options": []map[string]any{{"id": "A", "label": "a"}}}}); exit(err) != domain.ExitUsage {
		t.Fatalf("item.update with a single-option ask: %v", err)
	}
	// 只有内核身份（及 Web 上的本人）能写问题的类别码。
	if _, err := e.do(worker, "decision.request", "", map[string]any{"id": id, "ask": map[string]any{"question": "q", "free_text": true, "code": "task_incomplete"}}); exit(err) != domain.ExitUsage {
		t.Fatalf("worker wrote ask.code: %v", err)
	}
	for _, a := range []map[string]any{
		{"question": "q", "free_text": true, "options": []map[string]any{{"id": "A", "label": "a"}}},
	} {
		e.must(t, worker, "decision.request", "", map[string]any{"id": id, "ask": a})
	}
	e.verify(t)
}

func TestBasisRequiredLeavesOtherObjectsUnchanged(t *testing.T) {
	e := setup(t)
	asked := e.asked(t, nil)
	proposed := e.item(t, map[string]any{"phase": "proposed"})
	done := e.item(t, map[string]any{})
	e.must(t, owner, "item.transition", "", map[string]any{"id": done, "to": "done", "reason": "x"})
	for _, tc := range []struct {
		id, cmd string
		in      map[string]any
		phase   domain.Phase
	}{
		{asked, "decision.answer", map[string]any{"choice": "A"}, domain.PhaseNeedsInput},
		{proposed, "proposal.accept", nil, domain.PhaseProposed},
		{done, "proposal.accept", nil, domain.PhaseDone},
	} {
		in := map[string]any{"id": tc.id}
		for k, v := range tc.in {
			in[k] = v
		}
		before := e.get(t, tc.id)
		nEv := len(e.events(t, tc.id))
		if _, err := e.do(delegate, tc.cmd, "", in); exit(err) != domain.ExitBasisRequired {
			t.Fatalf("%s: want 23, got %v", tc.id, err)
		}
		after := e.get(t, tc.id)
		if after.Phase != tc.phase || after.Version != before.Version || (before.Ask == nil) != (after.Ask == nil) {
			t.Fatalf("%s changed: %+v -> %+v", tc.id, before, after)
		}
		// 只多一条 command.rejected：不产生 ask、不产生别的事件。
		if es := e.events(t, tc.id); len(es) != nEv+1 || es[len(es)-1].Type != domain.EvCommandRejected {
			t.Fatalf("%s events: %+v", tc.id, es[nEv:])
		}
	}
	// 建项目是一档，不需要任何附加依据。
	if _, err := e.do(delegate, "project.create", "", map[string]any{"name": "p1"}); err != nil {
		t.Fatalf("project.create: %v", err)
	}
	e.verify(t)
}
