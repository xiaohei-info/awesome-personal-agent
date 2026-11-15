package kcontext

import (
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 主会话：delegate 级，报告 harness "hermes"。
var host = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Harness: "hermes"}, Level: authz.LevelDelegate}

func (e *env) withDelegation(t *testing.T) {
	t.Helper()
	if err := executors.Register(e.x, executors.Options{Owner: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(_ command.Commit, err error) { t.Error(err) }}); err != nil {
		t.Fatal(err)
	}
}

func (e *env) dispatch(t *testing.T, in map[string]any) string {
	t.Helper()
	return e.id(t, host, "delegate.dispatch", in)
}

// 委派历史（§6.17）：同一子事项、同一父事项下的子事项、经 continues 接上的记录；own 随调用方的 harness；
// 注入只多一行、不带句柄；发起委派的凭据对子事项负责。
func TestDispatchHistory(t *testing.T) {
	e := setup(t)
	e.withDelegation(t)
	parent := e.item(t, map[string]any{"title": "login timeout"})
	exec := map[string]any{"kind": "remote-agent", "host": "mbp", "label": "Paseo · quick @ mbp", "handle": map[string]any{"mech": "paseo", "profile": "quick"}}
	c1 := e.id(t, host, "delegate.create", map[string]any{"parent": parent, "assignee": "hermes.paseo.mbp.quick", "executor": exec})
	d1 := e.dispatch(t, map[string]any{"id": c1, "session": "agt_1", "resumable": true, "handle": map[string]any{"mech": "paseo", "agent_id": "agt_1"}})
	time.Sleep(2 * time.Millisecond)
	c2 := e.id(t, host, "delegate.create", map[string]any{"parent": parent, "assignee": "someone"})
	d2 := e.dispatch(t, map[string]any{"id": c2})
	// 别的事项上的记录，经 continues 接续 d1。
	other := e.item(t, map[string]any{"title": "follow-up"})
	c3 := e.id(t, host, "delegate.create", map[string]any{"parent": other, "assignee": "hermes.paseo.mbp.quick"})
	time.Sleep(2 * time.Millisecond)
	d3 := e.dispatch(t, map[string]any{"id": c3, "session": "agt_1", "continues": d1})
	unrelated := e.item(t, map[string]any{"title": "unrelated"})
	c4 := e.id(t, host, "delegate.create", map[string]any{"parent": unrelated, "assignee": "someone"})
	d4 := e.dispatch(t, map[string]any{"id": c4})

	r := e.build(t, Request{ItemScope: true, Item: c1, Reader: Reader{Level: authz.LevelDelegate, Principal: "dlg", Harness: "hermes"}})
	if len(r.History) != 1 || r.History[0].Item != c1 {
		t.Fatalf("history %+v", r.History)
	}
	got := map[string]string{}
	for _, h := range r.History[0].Dispatches {
		got[h.ID] = h.Via
		if !h.Own {
			t.Errorf("%s was written by this harness", h.ID)
		}
	}
	if got[d1] != "item" || got[d2] != "parent" || got[d3] != "continues" || got[d4] != "" || len(got) != 3 {
		t.Fatalf("sequence %v (d1 %s d2 %s d3 %s)", got, d1, d2, d3)
	}
	first := r.History[0].Dispatches[0]
	if first.ID != d3 || first.Executor.Label != "Paseo · quick @ mbp" || string(first.Executor.Handle) != `{"mech":"paseo","profile":"quick"}` {
		t.Fatalf("newest first with the executor's registration: %+v", first)
	}

	// 别的 harness 读：own 为假，注入标"仅供参考"，且不带句柄。
	r = e.build(t, Request{ItemScope: true, Item: parent, Reader: Reader{Level: authz.LevelDelegate, Principal: "dlg", Harness: "other"}})
	if len(r.History) != 1 || r.History[0].Dispatches[0].Own {
		t.Fatalf("foreign reader: %+v", r.History)
	}
	inj := r.Inject()
	if !strings.Contains(inj, "上次：") || !strings.Contains(inj, "来自 hermes，仅供参考") || strings.Contains(inj, "agent_id") {
		t.Fatalf("inject: %s", inj)
	}

	// 负责者：发起委派的凭据对子事项负责；别的凭据不负责；运行身份对目标的委派子事项负责。
	var mine, foreign, run bool
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		it, err := items.Get(tx, c1)
		if err != nil {
			return err
		}
		if mine, err = (Reader{Principal: "dlg"}).responsible(tx, it); err != nil {
			return err
		}
		if foreign, err = (Reader{Principal: "someone-else"}).responsible(tx, it); err != nil {
			return err
		}
		rec, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Runner: "main_session", Launch: "headless",
			Target: parent, Harness: "h", Access: "read", TimeoutSec: 60}, Spec: proc.Spec{Command: []string{"true"}}}, time.Now())
		if err != nil {
			return err
		}
		run, err = (Reader{Level: authz.LevelRun, Run: rec.ID}).responsible(tx, it)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !mine || foreign || !run {
		t.Fatalf("responsible: initiator %v, other %v, run on parent %v", mine, foreign, run)
	}
}

// 同一例行任务实例建的事项算同一序列（via: instance）；别的实例的不算。
func TestDispatchHistoryAcrossInstanceRuns(t *testing.T) {
	e := setup(t)
	e.withDelegation(t)
	week := func(inst, title string) string {
		return e.item(t, map[string]any{"title": title, "origin": map[string]string{"source": domain.OriginInstance, "ref": inst}})
	}
	w1 := week("inst_weekly", "周报 W1")
	c1 := e.id(t, host, "delegate.create", map[string]any{"parent": w1, "assignee": "writer"})
	d1 := e.dispatch(t, map[string]any{"id": c1, "session": "agt_w1", "resumable": true})
	other := week("inst_daily", "日报")
	co := e.id(t, host, "delegate.create", map[string]any{"parent": other, "assignee": "writer"})
	e.dispatch(t, map[string]any{"id": co, "session": "agt_d"})
	w2 := week("inst_weekly", "周报 W2")

	r := e.build(t, Request{ItemScope: true, Item: w2, Reader: Reader{Level: authz.LevelDelegate, Principal: "dlg", Harness: "hermes"}})
	if len(r.History) != 1 || len(r.History[0].Dispatches) != 1 {
		t.Fatalf("history %+v", r.History)
	}
	if d := r.History[0].Dispatches[0]; d.ID != d1 || d.Via != "instance" {
		t.Fatalf("last week's dispatch via instance: %+v", d)
	}
}
