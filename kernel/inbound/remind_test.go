package inbound

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// "只口头提醒"来源（§9.3）：照常进队列、只有它的事件也拉起批处理、提示词标模式；Agent 建建议被内核拒绝且不留事项，
// 挂靠与记为已口头提醒可以；本人事后能补建。
func TestRemindOnlySource(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	fm, err := followmsg.New(followmsg.Options{Store: e.st, Exec: e.x, Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Register(e.x); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"ev_a", "ev_b"} {
		ingestOne(t, e, "src_todo", "reminder", id, "reminder.due", "subject "+id)
	}
	agent := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent_1", Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	// 本人才能改处理方式：开放了权限组的 Agent 切不了，否则能先切回常规处理再建建议。
	mode := map[string]any{"id": "src_todo", "mode": domain.HandlingRemindOnly}
	if _, err := e.do(agent, registry.CmdSetHandling, "", mode); err == nil {
		t.Fatal("an agent must not change how a source is handled")
	}
	e.must(registry.CmdSetHandling, mode)

	_, runsDir := registerBatchAgent(t, e, policy.SourcePipeline{BatchSize: 2, BatchPrompt: "自定义批处理说明"})
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	run := latestBatchRun(t, e)
	prompt, err := os.ReadFile(filepath.Join(runsDir, run.Run.ID, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(prompt), "| mode=remind_only") != 2 || !strings.Contains(string(prompt), "不得建建议") {
		t.Fatalf("prompt must mark remind-only events even with a custom instruction:\n%s", prompt)
	}

	count := func() (n int) {
		_ = e.st.View(bg, func(tx *store.Tx) error {
			l, err := items.List(tx, items.Filter{})
			n = len(l)
			return err
		})
		return
	}
	runAgent := command.Trusted{Actor: agent.Actor, Level: authz.LevelRun}
	for _, who := range []command.Trusted{runAgent, agent} {
		if _, err := e.svc.ProposeInto(bg, who, "ev_a", ""); err == nil || !strings.Contains(err.Error(), "remind only") {
			t.Fatalf("%s propose must be refused: %v", who.Level, err)
		}
	}
	state := func(id string) (ev domain.SourceEvent) {
		_ = e.st.View(bg, func(tx *store.Tx) (err error) { ev, err = runner.Event(tx, id); return })
		return
	}
	if ev := state("ev_a"); ev.Queue != runner.QueueRunning {
		t.Fatalf("refused propose must leave the event running: %+v", ev)
	}
	if _, err := e.do(runAgent, CmdRemind, "", map[string]any{"event": "ev_a"}); err != nil {
		t.Fatal(err)
	}
	if ev := state("ev_a"); ev.Disposition != domain.DispositionReminded || ev.Queue != runner.QueueDone {
		t.Fatalf("after remind: %+v", ev)
	}
	if _, err := e.svc.ProposeInto(bg, runAgent, "ev_a", ""); err == nil {
		t.Fatal("agent must not turn a reminded event of a remind-only source into a proposal")
	}
	var open struct{ ID string }
	if err := json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "已有事项", "phase": "open"}), &open); err != nil {
		t.Fatal(err)
	}
	before := count()
	if ev, err := e.svc.AttachEvent(bg, runAgent, "ev_b", open.ID, "后续"); err != nil || ev.Disposition != domain.DispositionAttached {
		t.Fatalf("attach is allowed: %+v %v", ev, err)
	}
	ev, err := e.svc.ProposeInto(bg, owner, "ev_a", "")
	if err != nil || ev.Disposition != domain.DispositionProposed || ev.Item == "" || count() != before+1 {
		t.Fatalf("owner makes a proposal afterwards: %+v %v", ev, err)
	}

	// 积压"只跑规则"也不为它自动建建议（规则出口是转建议的事件留在积压里）。
	ingestOne(t, e, "src_todo", "reminder", "ev_c", "reminder.due", "subject c")
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE sources_runner_event SET disposition = 'unjudged' WHERE id = 'ev_c'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	before = count()
	raw, err := e.svc.BacklogRules(bg, command.Request{Identity: owner}, "src_todo")
	if err != nil || raw.Disposed != 0 || raw.Remaining != 1 || count() != before {
		t.Fatalf("backlog rules on a remind-only source: %+v %v", raw, err)
	}
}

// 常规来源的事件，Agent 可自行记为已口头提醒，之后仍可按要求补建建议。
func TestAgentMayRemindAndLaterProposeOnNormalSource(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	ingestOne(t, e, "src_mail", "mail", "ev_fyi", "mail.received", "fyi")
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if err := runner.Enqueue(tx, "ev_fyi", runner.LevelNormal); err != nil {
			return err
		}
		_, err := runner.Claim(tx, "run_fyi", 10)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	agent := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent_1", Via: domain.ViaCLI}, Level: authz.LevelRun}
	if _, err := e.do(agent, CmdRemind, "", map[string]any{"event": "ev_fyi"}); err != nil {
		t.Fatal(err)
	}
	if ev, err := e.svc.ProposeInto(bg, agent, "ev_fyi", ""); err != nil || ev.Disposition != domain.DispositionProposed {
		t.Fatalf("propose after remind on a normal source: %+v %v", ev, err)
	}
}

// 对话捕获来源与其他来源同一机制（§9.3）：第一次捕获后即可由本人设"只口头提醒"，提示词标模式，Agent 建不了建议；
// 它仍不是登记的来源。
func TestRemindOnlyCaptureSource(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	var proj struct{ ID string }
	if err := json.Unmarshal(e.must("project.create", map[string]any{"name": "work"}), &proj); err != nil {
		t.Fatal(err)
	}
	var out CaptureOut
	if err := json.Unmarshal(e.must(CmdCapture, map[string]any{"project": proj.ID, "text": "user: 把周报发给团队"}), &out); err != nil || !out.Captured {
		t.Fatalf("capture: %+v %v", out, err)
	}
	src := capturePrefix + out.Project
	e.must(registry.CmdSetHandling, map[string]any{"id": src, "mode": domain.HandlingRemindOnly})
	var again CaptureOut // 后续捕获不改回处理方式
	if err := json.Unmarshal(e.must(CmdCapture, map[string]any{"project": proj.ID, "text": "user: 再提醒我一次"}), &again); err != nil {
		t.Fatal(err)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		if _, err := registry.Get(tx, src); err == nil {
			t.Error("a capture source is not a registered source")
		}
		all, err := registry.List(tx)
		if len(all) != 0 {
			t.Errorf("capture sources must not be listed as registered: %+v", all)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	_, runsDir := registerBatchAgent(t, e, policy.SourcePipeline{BatchSize: 2})
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	var evs []domain.SourceEvent
	for end := time.Now().Add(10 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		evs = e.events(src)
		if len(evs) == 2 && evs[0].Disposition == domain.DispositionUnjudged && evs[1].Disposition == domain.DispositionUnjudged {
			break
		}
		if time.Now().After(end) {
			t.Fatalf("capture events did not settle: %+v", evs)
		}
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error { // 本环境没有判断端点：直接放进队列
		for _, x := range evs {
			if _, err := tx.Exec(`UPDATE sources_runner_event SET disposition = 'pending', unjudged_reason = '' WHERE id = ?`, x.ID); err != nil {
				return err
			}
			if err := runner.Enqueue(tx, x.ID, runner.LevelNormal); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	prompt, err := os.ReadFile(filepath.Join(runsDir, latestBatchRun(t, e).Run.ID, "prompt.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(prompt), "| mode=remind_only") != 2 {
		t.Fatalf("capture events must be marked remind-only:\n%s", prompt)
	}
	agent := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent_1", Via: domain.ViaCLI}, Level: authz.LevelRun}
	if _, err := e.svc.ProposeInto(bg, agent, evs[0].ID, ""); err == nil || !strings.Contains(err.Error(), "remind only") {
		t.Fatalf("agent propose must be refused: %v", err)
	}
}
