package inbound

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// B5 验收：三个面向 Agent 的去向命令（propose + --into 并入、ignore、attach + handback 撤回）。
func TestEventDispositionCommandsProposeIntoIgnoreAttachAndHandback(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	fm, err := followmsg.New(followmsg.Options{Store: e.st, Exec: e.x, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Register(e.x); err != nil {
		t.Fatal(err)
	}

	dlg := command.Trusted{
		Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent_1", Via: domain.ViaCLI},
		Level: authz.LevelDelegate,
	}

	// 准备 4 条处于队列 running 状态的来源事件
	for _, id := range []string{"ev_p1", "ev_p2", "ev_ign", "ev_att"} {
		ingestOne(t, e, "src_mail", "mail", id, "mail.received", "subject "+id)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for _, id := range []string{"ev_p1", "ev_p2", "ev_ign", "ev_att"} {
			if err := runner.Enqueue(tx, id, runner.LevelNormal); err != nil {
				return err
			}
		}
		_, err := runner.Claim(tx, "run_b5", 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// 1) propose 新建建议（不带 into）
	ev1, err := e.svc.ProposeInto(bg, dlg, "ev_p1", "")
	if err != nil {
		t.Fatalf("propose new: %v", err)
	}
	if ev1.Disposition != domain.DispositionProposed || ev1.Item == "" || ev1.Queue != runner.QueueDone {
		t.Fatalf("ev_p1 after propose: %+v", ev1)
	}
	propID := ev1.Item

	// 2) propose --into 并入已有建议：该建议的 proposal.sources 多一条，merges 有记录
	ev2, err := e.svc.ProposeInto(bg, dlg, "ev_p2", propID)
	if err != nil {
		t.Fatalf("propose into: %v", err)
	}
	if ev2.Disposition != domain.DispositionProposed || ev2.Item != propID || ev2.Queue != runner.QueueDone {
		t.Fatalf("ev_p2 after propose into: %+v", ev2)
	}
	prop := e.item(propID)
	if prop.Proposal == nil || !slices.Contains(prop.Proposal.Sources, "ev_p1") || !slices.Contains(prop.Proposal.Sources, "ev_p2") {
		t.Fatalf("proposal sources should contain both events: %+v", prop.Proposal)
	}
	if len(prop.Proposal.Merges) == 0 || prop.Proposal.Merges[0].Event != "ev_p2" {
		t.Fatalf("proposal merges should trace ev_p2: %+v", prop.Proposal.Merges)
	}

	// 3) ignore 忽略处于 running 的事件
	raw, _ := json.Marshal(map[string]any{"event": "ev_ign", "reason": "noise"})
	if _, err := e.x.Do(bg, command.Request{Identity: dlg, Command: CmdIgnoreUnjudged, Input: raw}); err != nil {
		t.Fatalf("ignore running: %v", err)
	}
	var evIgn domain.SourceEvent
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { evIgn, err = runner.Event(tx, "ev_ign"); return })
	if evIgn.Disposition != domain.DispositionIgnored || evIgn.Queue != runner.QueueDone {
		t.Fatalf("ev_ign after ignore: %+v", evIgn)
	}

	// 4) attach 挂到已有进行中事项，再用 followmsg.handback 交回撤销
	var openItem struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "已有事项", "phase": "open"}), &openItem)
	evAtt, err := e.svc.AttachEvent(bg, dlg, "ev_att", openItem.ID, "属于这条已有事项")
	if err != nil {
		t.Fatalf("attach event: %v", err)
	}
	if evAtt.Disposition != domain.DispositionAttached || evAtt.Item != openItem.ID || evAtt.AttachState != runner.AttachPending || evAtt.Queue != runner.QueueDone {
		t.Fatalf("ev_att after attach: %+v", evAtt)
	}

	// 用 followmsg.handback 交回：事件回到 pending、队列清空、attach_state 为 handed_back
	hb, _ := json.Marshal(map[string]any{"message": "ev_att", "reason": "挂错了，另行处理"})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: followmsg.CmdHandback, Input: hb}); err != nil {
		t.Fatalf("handback attached event: %v", err)
	}
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { evAtt, err = runner.Event(tx, "ev_att"); return })
	if evAtt.Disposition != domain.DispositionPending || evAtt.AttachState != runner.AttachHandedBack || evAtt.Queue != "" {
		t.Fatalf("ev_att after handback: %+v", evAtt)
	}

	// 校验关闭的事项拒绝挂靠
	var closedItem struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "已关事项", "phase": "open"}), &closedItem)
	_ = e.must("item.transition", map[string]any{"id": closedItem.ID, "to": string(domain.PhaseDone), "reason": "done"})
	ingestOne(t, e, "src_mail", "mail", "ev_att_closed", "mail.received", "to closed")
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := runner.Enqueue(tx, "ev_att_closed", runner.LevelNormal); err != nil {
			return err
		}
		_, err := runner.Claim(tx, "run_b5_2", 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.svc.AttachEvent(bg, dlg, "ev_att_closed", closedItem.ID, "nope"); err == nil {
		t.Fatal("expected conflict attaching to closed item")
	}
	var _ items.Write // keep import clean if needed
}
