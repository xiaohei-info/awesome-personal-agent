package inbound

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventfeed"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	delegate = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
	runIdent = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "run", Via: domain.ViaCLI, Run: "run_1"}, Level: authz.LevelRun}
)

// attachEnv 在入站环境上接入建议处置命令与跟进消息归属（后续同线程消息据它回到目标事项）。
func attachEnv(t *testing.T) *env {
	t.Helper()
	e := newEnv(t, false)
	if err := decisions.Register(e.x); err != nil {
		t.Fatal(err)
	}
	fm, err := followmsg.New(followmsg.Options{Store: e.st, Exec: e.x, OnError: func(err error) { t.Errorf("followmsg: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := fm.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.svc.o.Intercept = fm.Intercept
	return e
}

// proposalOf 让一条 ticket 事件经规则进建议流，返回建议 id。
func (e *env) proposalOf(id, subject string, keys ...string) string {
	e.t.Helper()
	extra := map[string]any{"summary": subject + "摘要"}
	if len(keys) > 0 {
		extra["correlation_keys"] = keys
	}
	e.emit("src_t", "ticket", ev(id, "ticket.opened", subject, extra))
	for _, se := range e.events("src_t") {
		if se.DedupeKey.EventID == id && se.Item != "" {
			return se.Item
		}
	}
	e.t.Fatalf("event %s produced no proposal", id)
	return ""
}

func (e *env) openItem(title string) string {
	e.t.Helper()
	var made struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": title, "phase": "open"}), &made)
	return made.ID
}

func (e *env) do(who command.Trusted, cmd, evidence string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: who, Command: cmd, Input: b, Owner: "owner", Evidence: evidence})
}

func (e *env) eventsOf(obj string) (out []domain.Event) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, obj)
		for _, en := range es {
			out = append(out, en.Event)
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func countType(es []domain.Event, typ string) (n int) {
	for _, e := range es {
		if e.Type == typ {
			n++
		}
	}
	return
}

func (e *env) view(f func(tx *store.Tx)) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error { f(tx); return nil }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) verify() {
	e.t.Helper()
	if err := e.st.View(bg, items.Verify); err != nil {
		e.t.Fatalf("projection differs from event fold: %v", err)
	}
}

// ACT-PRH-08、09、18：delegate 附本人原话把建议补到已有事项；之后同一线程的消息直接回到目标事项。
func TestAttachProposalToExistingItem(t *testing.T) {
	e := attachEnv(t)
	pa := e.proposalOf("att-1", "合同第二版意见", "thread-9")
	t3 := e.openItem("合同谈判")

	if _, err := e.do(delegate, CmdAttach, "", map[string]any{"id": pa, "to": t3}); exitOf(err) != domain.ExitBasisRequired {
		t.Fatalf("delegate without the owner's words must get exit 23: %v", err)
	}
	if _, err := e.do(runIdent, CmdAttach, "", map[string]any{"id": pa, "to": t3}); exitOf(err) != domain.ExitPermissionDenied {
		t.Fatalf("run identity must get exit 4: %v", err)
	}
	if got := e.item(pa); got.Phase != domain.PhaseProposed {
		t.Fatalf("rejected attach changed the proposal: %+v", got)
	}

	if _, err := e.do(delegate, CmdAttach, "这条补到合同谈判", map[string]any{"id": pa, "to": t3}); err != nil {
		t.Fatal(err)
	}
	if got := e.item(pa); got.Phase != domain.PhaseDropped {
		t.Fatalf("proposal must be dropped, not deleted: %+v", got)
	}
	e.view(func(tx *store.Tx) {
		rels, _ := items.Relations(tx, pa)
		if !slices.Contains(rels, domain.Relation{Type: domain.RelRelates, From: pa, To: t3}) {
			t.Errorf("proposal must relate to the target: %+v", rels)
		}
		msgs, _ := runner.Attachments(tx, t3)
		if len(msgs) != 1 || msgs[0].DedupeKey.EventID != "att-1" || msgs[0].AttachState != runner.AttachProcessed {
			t.Errorf("source event must become a processed message of the target: %+v", msgs)
		}
		prog, _ := items.Progress(tx, t3)
		if len(prog) != 1 || !strings.Contains(prog[0].Text, "合同第二版意见") || !strings.Contains(prog[0].Text, "合同第二版意见摘要") {
			t.Errorf("progress must carry the proposal title and summary: %+v", prog)
		}
		keys, _ := items.Correlations(tx, t3)
		if len(keys) != 1 || keys[0].Key != "thread-9" || keys[0].Domain != "src_t" {
			t.Errorf("correlation key must be registered on the target: %+v", keys)
		}
	})

	// proposal.attached 以被补到的事项为对象（附录 B.4）；建议自己的变更是一条 item.phase_changed。
	att := e.eventsOf(t3)
	last := att[len(att)-1]
	if last.Type != domain.EvProposalAttached || last.Actor.OnBehalfOf != "owner" || last.Actor.SessionKind != "direct" || last.Evidence != "这条补到合同谈判" ||
		!strings.Contains(last.Reason, pa) && !strings.Contains(string(last.Payload), pa) {
		t.Fatalf("proposal.attached event: %+v", last)
	}
	pe := e.eventsOf(pa)
	if p := pe[len(pe)-1]; p.Type != domain.EvItemPhaseChanged || p.Evidence != last.Evidence || !strings.Contains(p.Reason, t3) {
		t.Fatalf("proposal change event: %+v", p)
	}
	if !domain.IsRegisteredEventType(domain.EvProposalAttached) || !domain.IsRegisteredEventType(domain.EvItemProgressAdded) {
		t.Fatal("proposal.attached and item.progress_added must be stable subscribable types")
	}
	// ACT-PRH-18：两类新事件在事件订阅里可见，proposal.attached 的标题是被补到的事项。
	var page eventfeed.Page
	e.view(func(tx *store.Tx) { page, _ = eventfeed.Read(tx, 0, 1000) })
	seen := map[string]eventfeed.Envelope{}
	for _, env := range page.Envelopes {
		seen[env.Type] = env
	}
	if got := seen[domain.EvProposalAttached]; got.Object != t3 || got.Title != "合同谈判" || seen[domain.EvItemProgressAdded].Object != t3 {
		t.Fatalf("subscription must expose proposal.attached and item.progress_added: %+v", seen)
	}
	if n := countType(e.eventsOf(t3), domain.EvItemProgressAdded); n != 1 {
		t.Fatalf("target must have one item.progress_added, got %d", n)
	}
	e.verify()

	// 同一线程的下一条消息回到目标事项，不再生成建议。
	e.emit("src_t", "ticket", ev("att-2", "ticket.opened", "合同第三版意见", map[string]any{"correlation_keys": []string{"thread-9"}}))
	for _, se := range e.events("src_t") {
		if se.DedupeKey.EventID == "att-2" && (se.Disposition != domain.DispositionAttached || se.Item != t3) {
			t.Fatalf("follow-up must attach to the target: %+v", se)
		}
	}
	for _, it := range e.allItems() {
		if it.Phase == domain.PhaseProposed {
			t.Fatalf("follow-up created a new proposal: %+v", it)
		}
	}
	// 重复补到同一建议：只认第一次。
	if _, err := e.do(delegate, CmdAttach, "再来一次", map[string]any{"id": pa, "to": t3}); exitOf(err) != domain.ExitConflict {
		t.Fatalf("second attach must conflict: %v", err)
	}
}

// ACT-PRH-11：目标不合法时拒绝，建议、来源事件、事件日志都不变。
func TestAttachRejectsInvalidTargetsAtomically(t *testing.T) {
	e := attachEnv(t)
	pc := e.proposalOf("att-4", "会议纪要", "thread-4")
	other := e.proposalOf("att-5", "另一条建议")
	done := e.openItem("已完成")
	e.must("item.transition", map[string]any{"id": done, "to": "done", "reason": "finished"})

	for name, c := range map[string]struct {
		to   string
		exit int
	}{
		"closed": {done, domain.ExitConflict}, "missing": {"itm-not-exist", domain.ExitNotFound},
		"proposed": {other, domain.ExitConflict}, "self": {pc, domain.ExitUsage},
	} {
		if _, err := e.do(owner, CmdAttach, "", map[string]any{"id": pc, "to": c.to}); exitOf(err) != c.exit {
			t.Errorf("%s target: exit %d, want %d (%v)", name, exitOf(err), c.exit, err)
		}
	}
	if got := e.item(pc); got.Phase != domain.PhaseProposed {
		t.Fatalf("proposal changed: %+v", got)
	}
	for _, id := range []string{pc, done} {
		if n := countType(e.eventsOf(id), domain.EvProposalAttached); n != 0 {
			t.Fatalf("rejected attach left %d proposal.attached events on %s", n, id)
		}
	}
	first := e.events("src_t")[0].ID
	e.view(func(tx *store.Tx) {
		ev, _ := runner.Event(tx, first)
		if ev.Disposition != domain.DispositionProposed || ev.Item != pc {
			t.Errorf("source event moved: %+v", ev)
		}
		if p, _ := items.Progress(tx, done); len(p) != 0 {
			t.Errorf("progress written on a rejected target: %+v", p)
		}
	})
	e.verify()
}

// ACT-PRH-15、16、17：处置清单批量，每条各自成败、各有 Event、可逐条撤销；缺依据或超上限整批不生效。
func TestBatchExplicitPerItemOutcomesAndRevert(t *testing.T) {
	e := attachEnv(t)
	e1, e2, e3 := e.proposalOf("e-1", "一"), e.proposalOf("e-2", "二"), e.proposalOf("e-3", "三")
	t3 := e.openItem("合同谈判")
	req := command.Request{Identity: delegate, Evidence: "这几条按我说的处理", Owner: "owner"}
	plan := BatchRequest{Mode: BatchExplicit, Actions: []BatchAction{
		{ID: e1, Do: "accept"}, {ID: e2, Do: "dismiss", Reason: "不需要"}, {ID: e3, Do: "attach", To: t3}, {ID: "itm-not-exist", Do: "accept"}}}

	// 缺依据：整批不生效，退出码 23。
	if _, err := Batch(bg, e.x, e.st, command.Request{Identity: delegate, Owner: "owner"}, plan); exitOf(err) != domain.ExitBasisRequired {
		t.Fatalf("missing basis: %v", err)
	}
	// 运行身份：退出码 4。
	if _, err := Batch(bg, e.x, e.st, command.Request{Identity: runIdent, Owner: "owner"}, plan); exitOf(err) != domain.ExitPermissionDenied {
		t.Fatalf("run identity: %v", err)
	}
	// 超上限：整批不生效。
	big := BatchRequest{Mode: BatchExplicit}
	for range MaxBatch + 1 {
		big.Actions = append(big.Actions, BatchAction{ID: e1, Do: "accept"})
	}
	if _, err := Batch(bg, e.x, e.st, req, big); exitOf(err) != domain.ExitUsage {
		t.Fatalf("over the limit: %v", err)
	}
	for _, id := range []string{e1, e2, e3} {
		if got := e.item(id); got.Phase != domain.PhaseProposed {
			t.Fatalf("rejected batch processed %s: %+v", id, got)
		}
	}

	res, err := Batch(bg, e.x, e.st, req, plan)
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 4 || !res[0].OK || !res[1].OK || !res[2].OK || res[3].OK || res[3].Code != domain.CodeNotFound || res[3].Why == "" {
		t.Fatalf("per-item results: %+v", res)
	}
	if e.item(e1).Phase != domain.PhaseOpen || e.item(e2).Phase != domain.PhaseDropped || e.item(e3).Phase != domain.PhaseDropped {
		t.Fatal("dispositions do not match the plan")
	}
	for _, id := range []string{e1, e2, e3} {
		es := e.eventsOf(id)
		last := es[len(es)-1]
		if last.Type != domain.EvItemPhaseChanged || last.Evidence != "这几条按我说的处理" || last.Actor.OnBehalfOf != "owner" || last.Actor.SessionKind != "direct" {
			t.Errorf("%s: event %+v", id, last)
		}
	}
	if !strings.Contains(lastReason(e.eventsOf(e2)), "不需要") {
		t.Errorf("dismiss reason lost: %+v", e.eventsOf(e2))
	}
	e.view(func(tx *store.Tx) {
		if m, _ := runner.Attachments(tx, t3); len(m) != 1 {
			t.Errorf("attach in batch did not move the message: %+v", m)
		}
	})

	// 逐条撤销：只撤 e2 的丢弃，e1、e3 不动。
	es := e.eventsOf(e2)
	e.must("item.revert", map[string]any{"id": e2, "event": es[len(es)-1].Seq})
	if e.item(e2).Phase != domain.PhaseProposed || e.item(e1).Phase != domain.PhaseOpen || e.item(e3).Phase != domain.PhaseDropped {
		t.Fatal("revert of one batch entry must not touch the others")
	}
	after := e.eventsOf(e2)
	if len(after) != len(es)+1 || after[len(es)-1].Type != domain.EvItemPhaseChanged {
		t.Fatalf("revert must append an event and keep the original: %+v", after)
	}
	if tes := e.eventsOf(t3); tes[len(tes)-1].Type != domain.EvProposalAttached || tes[len(tes)-1].Evidence != "这几条按我说的处理" {
		t.Errorf("attach in a batch must record proposal.attached on the target: %+v", tes[len(tes)-1])
	}
	// 补到事项的那条不可逐条撤销（来源事件已改挂，单撤阶段会留下不一致）。
	es3 := e.eventsOf(e3)
	if _, err := e.do(owner, "item.revert", "", map[string]any{"id": e3, "event": es3[len(es3)-1].Seq}); err == nil {
		t.Fatal("reverting an attach must be refused")
	}
	e.verify()
}

func lastReason(es []domain.Event) string { return es[len(es)-1].Reason }

// 按推荐批量：只处理带推荐的建议；忽略取推荐理由；目标已关闭的跳过并说明，其余照常，各记一条 Event。
func TestBatchRecommendedSkipsClosedTargetAndUnrecommended(t *testing.T) {
	e := attachEnv(t)
	acc, dis, att, skip, none := e.proposalOf("r-1", "接受"), e.proposalOf("r-2", "忽略"), e.proposalOf("r-3", "补到"), e.proposalOf("r-4", "补到已关闭"), e.proposalOf("r-5", "无推荐")
	live, shut := e.openItem("在办"), e.openItem("已关")
	e.must("item.transition", map[string]any{"id": shut, "to": "done", "reason": "finished"})
	e.recommend(acc, `{"recommend":"accept","reason":"需要跟进"}`)
	e.recommend(dis, `{"recommend":"dismiss","reason":"广告"}`)
	e.recommend(att, `{"recommend":"attach","attach_to":"`+live+`","reason":"同一件事"}`)
	e.recommend(skip, `{"recommend":"attach","attach_to":"`+shut+`"}`)

	res, err := Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner"}, BatchRequest{Mode: BatchRecommended})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]BatchResult{}
	for _, r := range res {
		got[r.ID] = r
	}
	if len(res) != 4 || !got[acc].OK || !got[dis].OK || !got[att].OK || got[skip].OK || got[skip].Why == "" {
		t.Fatalf("results: %+v", res)
	}
	if e.item(acc).Phase != domain.PhaseOpen || e.item(dis).Phase != domain.PhaseDropped || e.item(att).Phase != domain.PhaseDropped ||
		e.item(skip).Phase != domain.PhaseProposed || e.item(none).Phase != domain.PhaseProposed {
		t.Fatal("dispositions do not follow the recommendations")
	}
	if r := lastReason(e.eventsOf(dis)); r != "广告" {
		t.Errorf("dismiss reason must be the recommendation's: %q", r)
	}
	// 范围：只处理 filter 内的。
	again := e.proposalOf("r-6", "又一条")
	e.recommend(again, `{"recommend":"accept"}`)
	other := e.proposalOf("r-7", "范围外")
	e.recommend(other, `{"recommend":"accept"}`)
	res, err = Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner"}, BatchRequest{Mode: BatchRecommended, Filter: BatchFilter{IDs: []string{again}}})
	if err != nil || len(res) != 1 || e.item(other).Phase != domain.PhaseProposed {
		t.Fatalf("filter ignored: %+v %v", res, err)
	}
	e.verify()
}

// recommend 把推荐写进事项的 proposal 列：推荐由入站起草流程写，这里只提供读取所需的夹具数据。
func (e *env) recommend(id, js string) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE items SET proposal = ? WHERE id = ?`, js, id)
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// 没有可处理的建议时，无权的调用者也得不到成功：身份与依据对整批一视同仁。
func TestBatchGateRejectsUnauthorizedEvenWhenNothingToDo(t *testing.T) {
	e := attachEnv(t)
	b := BatchRequest{Mode: BatchRecommended}
	if _, err := Batch(bg, e.x, e.st, command.Request{Identity: delegate, Owner: "owner"}, b); exitOf(err) != domain.ExitBasisRequired {
		t.Fatalf("delegate without evidence: %v", err)
	}
	if _, err := Batch(bg, e.x, e.st, command.Request{Identity: runIdent, Owner: "owner"}, b); exitOf(err) != domain.ExitPermissionDenied {
		t.Fatalf("run identity: %v", err)
	}
	res, err := Batch(bg, e.x, e.st, command.Request{Identity: delegate, Evidence: "按推荐", Owner: "owner"}, b)
	if err != nil || len(res) != 0 {
		t.Fatalf("authorized empty batch: %+v %v", res, err)
	}
}

func TestBatchRejectsBadShape(t *testing.T) {
	e := attachEnv(t)
	req := command.Request{Identity: owner, Owner: "owner"}
	for name, b := range map[string]BatchRequest{
		"no mode": {}, "bad mode": {Mode: "all"}, "bad do": {Mode: BatchExplicit, Actions: []BatchAction{{ID: "x", Do: "delete"}}},
		"mixed": {Mode: BatchRecommended, Actions: []BatchAction{{ID: "x", Do: "accept"}}},
		"empty": {Mode: BatchExplicit},
	} {
		if _, err := Batch(bg, e.x, e.st, req, b); exitOf(err) != domain.ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func exitOf(err error) int {
	if err == nil {
		return 0
	}
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

// 按推荐的范围可按来源收窄；同一线程的第二条建议补到同一事项时，已登记的关联键不重复登记。
func TestBatchRecommendedSourceFilterAndRepeatedKey(t *testing.T) {
	e := attachEnv(t)
	a := e.proposalOf("s-1", "来自 src_t", "thread-1")
	e.emit("src_o", "ticket", ev("s-2", "ticket.opened", "来自 src_o", map[string]any{"correlation_keys": []string{"thread-1"}}))
	var b string
	for _, se := range e.events("src_o") {
		b = se.Item
	}
	target := e.openItem("目标")
	e.recommend(a, `{"recommend":"attach","attach_to":"`+target+`"}`)
	e.recommend(b, `{"recommend":"attach","attach_to":"`+target+`"}`)

	res, err := Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner"}, BatchRequest{Mode: BatchRecommended, Filter: BatchFilter{Source: "src_t"}})
	if err != nil || len(res) != 1 || res[0].ID != a || !res[0].OK || e.item(b).Phase != domain.PhaseProposed {
		t.Fatalf("source filter: %+v %v", res, err)
	}
	if _, err := e.do(owner, CmdAttach, "", map[string]any{"id": b, "to": target}); err != nil {
		t.Fatal(err)
	}
	e.view(func(tx *store.Tx) {
		keys, _ := items.Correlations(tx, target)
		if len(keys) != 2 { // 两个去重域各登记一次；同域的键不重复
			t.Errorf("keys: %+v", keys)
		}
		if p, _ := items.Progress(tx, target); len(p) != 2 || p[1].ID == p[0].ID {
			t.Errorf("progress: %+v", p)
		}
	})
	e.verify()
}
