package inbound

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// sameObject 造两条合并键相同、都在建议里的建议（线上就是这样来的：第一条被忽略后同一对象又来了消息，另起一条，之后第一条被改回建议）。
func (e *env) sameObject(key, a, b string) (string, string) {
	e.t.Helper()
	x := e.propose("src_t", key+"-1", a, map[string]any{"merge_key": key})
	e.must("proposal.dismiss", map[string]any{"id": x})
	y := e.propose("src_t", key+"-2", b, map[string]any{"merge_key": key})
	e.must("item.transition", map[string]any{"id": x, "to": "proposed", "reason": "back"})
	if e.item(x).Proposal.MergeKey != key || e.item(y).Proposal.MergeKey != key || x == y {
		e.t.Fatalf("fixture: %s and %s should be two proposals sharing %s", x, y, key)
	}
	return x, y
}

func (e *env) needsYou(title string) string {
	e.t.Helper()
	in, _ := json.Marshal(map[string]any{"kind": "decision", "title": title, "phase": "proposed", "reason": "test"})
	r, err := e.x.Do(bg, command.Request{Identity: system, Command: "item.create", Input: in, Owner: "owner"})
	if err != nil {
		e.t.Fatal(err)
	}
	var v struct{ ID string }
	_ = json.Unmarshal(r.Value, &v)
	return v.ID
}

func (e *env) batchLogs(batch string) (handled, undone []domain.Event) {
	for _, ev := range e.eventsOf(batch) {
		switch ev.Type {
		case domain.EvProposalBatchHandled:
			handled = append(handled, ev)
		case domain.EvProposalBatchUndone:
			undone = append(undone, ev)
		}
	}
	return
}

func resultOf(rs []BatchResult, id string) BatchResult {
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	return BatchResult{}
}

func heldBy(e *env, item string) (ids []string) {
	e.view(func(tx *store.Tx) {
		evs, err := runner.Held(tx, item, nil)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, ev := range evs {
			ids = append(ids, ev.ID)
		}
	})
	return
}

// 批量接受：合并键相同的两条先合成一条再接受，只建一个事项；指向别的对象的一条单独接受；需要拍板的不进批量。
// 一次批量一条审计事件；同一批重发全部按"本批已处理"跳过，不多写一条事件；撤销后四条都回到原样（含合并前的归类记录与消息归属），再撤一次是 0 条。
func TestBatchSelectedAcceptMergesSameObjectAndUndoes(t *testing.T) {
	e := attachEnv(t)
	a, b := e.sameObject("mk-1", "PR 12 评审请求", "PR 12 评审提醒")
	c := e.proposalOf("c-1", "另一件事")
	d := e.needsYou("要不要接这个邀约")
	aBefore, bBefore, bMsgs := e.item(a), e.item(b), heldBy(e, b)
	req := command.Request{Identity: owner, Owner: "owner", IdemKey: "sel-1"}
	sel := BatchRequest{Mode: BatchSelected, Do: "accept", Filter: BatchFilter{IDs: []string{a, b, c, d}}}

	res, err := Batch(bg, e.x, e.st, req, sel)
	if err != nil || len(res) != 4 {
		t.Fatalf("selected accept: %+v %v", res, err)
	}
	ra, rb, rc, rd := resultOf(res, a), resultOf(res, b), resultOf(res, c), resultOf(res, d)
	if !ra.OK || ra.Do != "accept" || !rb.OK || rb.Do != "merge" || rb.Into != a || !rc.OK || rc.Do != "accept" || rd.OK || rd.Skip != SkipNeedsYou {
		t.Fatalf("results: %+v", res)
	}
	batch := ra.Batch
	if !batchRe.MatchString(batch) || rb.Batch != batch || ra.UndoUntil == nil {
		t.Fatalf("batch tags: %+v", res)
	}
	if e.item(a).Phase != domain.PhaseOpen || e.item(c).Phase != domain.PhaseOpen || e.item(b).Phase != domain.PhaseDropped || e.item(d).Phase != domain.PhaseProposed {
		t.Fatalf("phases: a=%s b=%s c=%s d=%s", e.item(a).Phase, e.item(b).Phase, e.item(c).Phase, e.item(d).Phase)
	}
	if sw := e.item(b).Proposal.Sweep; sw.By != domain.SweepByBulk || sw.Action != domain.SweepMerge || sw.Target != a || sw.Batch != batch || sw.Prev == nil || sw.Prev.State != "pending" {
		t.Fatalf("b's fold record: %+v", sw)
	}
	pa := e.item(a).Proposal
	if len(pa.Absorbed) != 1 || pa.Absorbed[0].Item != b || !slices.Contains(pa.Sources, bMsgs[0]) || len(heldBy(e, b)) != 0 {
		t.Fatalf("a should now carry b and its messages: %+v held(b)=%v", pa, heldBy(e, b))
	}
	h, _ := e.batchLogs(batch)
	if len(h) != 1 {
		t.Fatalf("one audit event per batch, got %d", len(h))
	}
	var l batchLog
	_ = json.Unmarshal(h[0].Payload, &l)
	if l.Mode != BatchSelected || l.Do != "accept" || l.Requested != 4 || len(l.Results) != 4 || l.By.Principal != owner.Actor.Principal || l.UndoUntil == nil {
		t.Fatalf("audit payload: %+v", l)
	}

	// 同一批重发：一条都不再处理，结果标为本批已处理；不多写任何事件。
	n := len(e.eventsOf(a)) + len(e.eventsOf(b)) + len(e.eventsOf(c)) + len(e.eventsOf(batch))
	again, err := Batch(bg, e.x, e.st, req, sel)
	if err != nil || len(again) != 4 {
		t.Fatalf("replay: %+v %v", again, err)
	}
	for _, id := range []string{a, b, c} {
		if r := resultOf(again, id); r.OK || r.Skip != SkipHandledHere {
			t.Errorf("replay %s: %+v", id, r)
		}
	}
	if m := len(e.eventsOf(a)) + len(e.eventsOf(b)) + len(e.eventsOf(c)) + len(e.eventsOf(batch)); m != n {
		t.Fatalf("a replay must not write: %d -> %d events", n, m)
	}
	// 另一次提交（不同批）碰到已处理的：按"别处已处理"跳过。
	other, err := Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner"}, BatchRequest{Mode: BatchSelected, Do: "dismiss", Filter: BatchFilter{IDs: []string{c}}})
	if err != nil || len(other) != 1 || other[0].Skip != SkipHandled {
		t.Fatalf("handled elsewhere: %+v %v", other, err)
	}

	out, err := Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner", IdemKey: "undo-1"}, BatchRequest{Mode: BatchUndo, Batch: batch})
	if err != nil || len(out) != 3 || slices.ContainsFunc(out, func(r BatchResult) bool { return !r.OK }) {
		t.Fatalf("undo: %+v %v", out, err)
	}
	for _, id := range []string{a, b, c} {
		if e.item(id).Phase != domain.PhaseProposed {
			t.Errorf("%s not back to proposed: %s", id, e.item(id).Phase)
		}
	}
	if got := e.item(b).Proposal; got.Sweep == nil || got.Sweep.State != "pending" || got.Sweep.By != "" || slices.Contains(got.NotSame, a) {
		t.Fatalf("b's record must be restored, without not_same: %+v", got)
	}
	if got := e.item(a).Proposal; len(got.Absorbed) != 0 || !slices.Equal(got.Sources, aBefore.Proposal.Sources) || slices.Contains(got.NotSame, b) {
		t.Fatalf("a must be restored: %+v want sources %v", got, aBefore.Proposal.Sources)
	}
	if !slices.Equal(heldBy(e, b), bMsgs) || e.item(b).Title != bBefore.Title {
		t.Fatalf("b's messages must come back: %v want %v", heldBy(e, b), bMsgs)
	}
	if _, u := e.batchLogs(batch); len(u) != 1 {
		t.Fatalf("one undo audit event, got %d", len(u))
	}
	if again, err := Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner"}, BatchRequest{Mode: BatchUndo, Batch: batch}); err != nil || len(again) != 0 {
		t.Fatalf("a second undo changes nothing: %+v %v", again, err)
	}
	if _, u := e.batchLogs(batch); len(u) != 1 {
		t.Fatal("an empty undo must not be logged")
	}
	e.verify()
}

// 合并键相同但本人拆开过的两条：接受时各建一个事项，不合并。
func TestBatchSelectedAcceptRespectsNotSame(t *testing.T) {
	e := attachEnv(t)
	a, b := e.sameObject("mk-2", "同一链接的两件事 A", "同一链接的两件事 B")
	// 本人在建议上撤销过 b 与 a 的合并（"不是同一件"）：两边互记 not_same。
	if _, err := e.do(owner, CmdMergeProposal, "", mergeIn{ID: b, Into: a, Batch: "pb_000000000001"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdUnmerge, "", unmergeIn{Item: b}); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(e.item(a).Proposal.NotSame, b) || e.item(b).Phase != domain.PhaseProposed {
		t.Fatalf("fixture: not_same %+v", e.item(a).Proposal)
	}
	g := Groups([]domain.Item{e.item(a), e.item(b)}, map[string]Origin{}, "")
	if len(g) != 1 || len(g[0].Items) != 2 || len(g[0].Bundles) != 0 {
		t.Fatalf("same merge key must be one group, but no bundle when split: %+v", g)
	}
	res, err := Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner"}, BatchRequest{Mode: BatchSelected, Do: "accept", Filter: BatchFilter{IDs: []string{a, b}}})
	if err != nil || len(res) != 2 || resultOf(res, a).Do != "accept" || resultOf(res, b).Do != "accept" || !resultOf(res, b).OK {
		t.Fatalf("not_same pair must be accepted one by one: %+v %v", res, err)
	}
	if e.item(a).Phase != domain.PhaseOpen || e.item(b).Phase != domain.PhaseOpen {
		t.Fatal("both must be open items")
	}
	e.verify()
}

// 批量执行期间别处先处理了：留下的那条被别处忽略时，已合进去的撤回，余下的另起一个事项；成员被别处处理时跳过它。都不中断、都计入结果。
func TestBatchSelectedConflictsDuringRunAreSkipped(t *testing.T) {
	e := attachEnv(t)
	a, b := e.sameObject("mk-3", "对象 A 第一条", "对象 A 第二条")
	c := e.proposalOf("c-3", "第三件")
	d := e.proposalOf("d-3", "第四件")
	fired := false
	e.x.OnCommit(func(cm command.Commit) {
		if fired || cm.Command != CmdMergeProposal {
			return
		}
		fired = true
		// 合并 b 进 a 刚落账：别处先把 a 忽略了，也把 d 忽略了。
		for _, id := range []string{a, d} {
			in, _ := json.Marshal(map[string]any{"id": id, "reason": "别处"})
			if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "proposal.dismiss", Input: in, Owner: "owner"}); err != nil {
				t.Errorf("dismiss elsewhere: %v", err)
			}
		}
	})
	res, err := Batch(bg, e.x, e.st, command.Request{Identity: owner, Owner: "owner"}, BatchRequest{Mode: BatchSelected, Do: "accept", Filter: BatchFilter{IDs: []string{a, b, c, d}}})
	if err != nil {
		t.Fatalf("a conflict during the run must not abort: %v", err)
	}
	if r := resultOf(res, a); r.OK || r.Skip != SkipHandled {
		t.Fatalf("a: %+v", res)
	}
	if r := resultOf(res, b); !r.OK || r.Do != "accept" || e.item(b).Phase != domain.PhaseOpen || e.item(b).Proposal.Sweep.By == domain.SweepByBulk {
		t.Fatalf("b should be pulled back out of a and accepted on its own: %+v %+v", r, e.item(b).Proposal.Sweep)
	}
	if r := resultOf(res, d); r.OK || r.Skip != SkipHandled || !resultOf(res, c).OK {
		t.Fatalf("d handled elsewhere is skipped, c goes on: %+v", res)
	}
	if len(e.item(a).Proposal.Absorbed) != 0 {
		t.Fatal("nothing may stay merged into an item the batch did not accept")
	}
	e.verify()
}

// 批量忽略：可选填原因，每条记批次；撤销不限时。批量接受的撤销只在窗口内、且事项此后没有别的改动时生效。
func TestBatchSelectedDismissAndUndoLimits(t *testing.T) {
	e := attachEnv(t)
	p, q := e.proposalOf("p-4", "订阅邮件一"), e.proposalOf("q-4", "订阅邮件二")
	req := command.Request{Identity: owner, Owner: "owner"}
	res, err := Batch(bg, e.x, e.st, req, BatchRequest{Mode: BatchSelected, Do: "dismiss", Reason: "营销邮件", Filter: BatchFilter{IDs: []string{p, q}}})
	if err != nil || len(res) != 2 || !res[0].OK || !res[1].OK {
		t.Fatalf("dismiss: %+v %v", res, err)
	}
	ds := e.dismissals(p, q)
	if ds[p].Batch != res[0].Batch || ds[p].Reason != "营销邮件" || ds[q].Batch != res[0].Batch {
		t.Fatalf("dismissals: %+v", ds)
	}
	e.now = e.now.Add(48 * time.Hour)
	if out, err := Batch(bg, e.x, e.st, req, BatchRequest{Mode: BatchUndo, Batch: res[0].Batch}); err != nil || len(out) != 2 || !out[0].OK {
		t.Fatalf("undoing an ignore has no window: %+v %v", out, err)
	}

	x, y := e.proposalOf("x-4", "接受一"), e.proposalOf("y-4", "接受二")
	acc, err := Batch(bg, e.x, e.st, req, BatchRequest{Mode: BatchSelected, Do: "accept", Filter: BatchFilter{IDs: []string{x, y}}})
	if err != nil || len(acc) != 2 {
		t.Fatalf("accept: %+v %v", acc, err)
	}
	e.must("item.update", map[string]any{"id": y, "title": "接受二（之后改过）", "reason": "edit"})
	e.now = e.now.Add(UndoWindow - time.Minute)
	out, err := Batch(bg, e.x, e.st, req, BatchRequest{Mode: BatchUndo, Batch: acc[0].Batch})
	if err != nil || !resultOf(out, x).OK || resultOf(out, y).Skip != SkipChanged || e.item(y).Phase != domain.PhaseOpen {
		t.Fatalf("changed after accept must stay: %+v %v", out, err)
	}

	z := e.proposalOf("z-4", "接受三")
	acc, _ = Batch(bg, e.x, e.st, req, BatchRequest{Mode: BatchSelected, Do: "accept", Filter: BatchFilter{IDs: []string{z}}})
	e.now = e.now.Add(UndoWindow + time.Minute)
	out, err = Batch(bg, e.x, e.st, req, BatchRequest{Mode: BatchUndo, Batch: acc[0].Batch})
	if err != nil || len(out) != 1 || out[0].Skip != SkipExpired || e.item(z).Phase != domain.PhaseOpen {
		t.Fatalf("past the window: %+v %v", out, err)
	}
	e.verify()
}

// 形态错误整批不生效；权限沿用单条命令：运行凭据一律 4，档位没放开拍板的 Agent 整批被拒，什么都不改。
func TestBatchSelectedShapeAndPermissions(t *testing.T) {
	e := attachEnv(t)
	a := e.proposalOf("s-5", "一条")
	req := command.Request{Identity: owner, Owner: "owner"}
	for name, b := range map[string]BatchRequest{
		"no do":          {Mode: BatchSelected, Filter: BatchFilter{IDs: []string{a}}},
		"attach":         {Mode: BatchSelected, Do: "attach", Filter: BatchFilter{IDs: []string{a}}},
		"no ids":         {Mode: BatchSelected, Do: "accept"},
		"dup ids":        {Mode: BatchSelected, Do: "accept", Filter: BatchFilter{IDs: []string{a, a}}},
		"reason accept":  {Mode: BatchSelected, Do: "accept", Reason: "x", Filter: BatchFilter{IDs: []string{a}}},
		"source":         {Mode: BatchSelected, Do: "dismiss", Filter: BatchFilter{IDs: []string{a}, Source: "src_t"}},
		"do in explicit": {Mode: BatchExplicit, Do: "accept", Actions: []BatchAction{{ID: a, Do: "accept"}}},
		"undo with do":   {Mode: BatchUndo, Do: "accept", Batch: "pb_000000000000"},
	} {
		if _, err := Batch(bg, e.x, e.st, req, b); exitOf(err) != domain.ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
	}
	sel := BatchRequest{Mode: BatchSelected, Do: "accept", Filter: BatchFilter{IDs: []string{a}}}
	if res, err := Batch(bg, e.x, e.st, command.Request{Identity: runIdent, Owner: "owner"}, sel); err != nil || len(res) != 1 || res[0].Code != domain.CodeTierClosed {
		t.Errorf("run credential follows its Agent: %+v %v", res, err)
	}
	if res, err := Batch(bg, e.x, e.st, command.Request{Identity: delegate, Owner: "owner"}, sel); err != nil || len(res) != 1 || res[0].OK || res[0].Code != domain.CodeTierClosed {
		t.Errorf("delegate without the decide tier is refused per item, as a single accept would be: %+v %v", res, err)
	}
	if e.item(a).Phase != domain.PhaseProposed {
		t.Fatal("a refused batch must change nothing")
	}
	e.decideFor()
	if res, err := Batch(bg, e.x, e.st, command.Request{Identity: delegate, Owner: "owner"}, sel); err != nil || !res[0].OK {
		t.Fatalf("delegate with the decide tier: %+v %v", res, err)
	}
	e.verify()
}
