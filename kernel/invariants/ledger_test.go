package invariants_test

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// 不变量 1（ACT-LED-02）：账本事实变更与它的 Event 在同一事务提交。
// 成功命令：版本加一，恰好多一条事件，投影与事件折叠一致；中途失败的命令：事件与版本都不留痕。
func TestInv1LedgerChangeAndEventCommitTogether(t *testing.T) {
	e := newLedger(t)
	err := e.x.Register(command.Spec{
		Name: "invariants.emit_then_fail", Action: authz.ActionEditAnyItem,
		Object: func(_ *store.Tx, in json.RawMessage) (authz.Object, error) {
			var p struct{ ID string }
			_ = json.Unmarshal(in, &p)
			return authz.Object{ID: p.ID}, nil
		},
		Apply: func(c *command.Ctx) (any, error) {
			if _, err := c.Emit(domain.Event{Type: domain.EvItemUpdated, Object: c.Object.ID, Payload: json.RawMessage(`{}`)}); err != nil {
				return nil, err
			}
			return nil, errors.New("boom after the event was appended")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	id := e.newItem(t, nil)
	if v, n := e.version(t, id), len(e.events(t, id)); v != 1 || n != 1 {
		t.Fatalf("create: version %d, events %d", v, n)
	}
	e.must(t, owner, "item.update", map[string]any{"id": id, "title": "renamed"})
	if v, n := e.version(t, id), len(e.events(t, id)); v != 2 || n != 2 {
		t.Fatalf("update: version %d, events %d", v, n)
	}

	headBefore := len(e.events(t, ""))
	if _, err := e.do(owner, "invariants.emit_then_fail", "", map[string]any{"id": id}); err == nil {
		t.Fatal("failing command reported success")
	}
	if got := len(e.events(t, "")); got != headBefore {
		t.Fatalf("a failed command left %d Event(s) behind", got-headBefore)
	}
	if v := e.version(t, id); v != 2 {
		t.Fatalf("a failed command moved the version to %d", v)
	}
	e.verify(t)
}

// 不变量 2（ACT-LED-03）：proposed 事项不产生投递。对照组：同样带过期截止的 open 事项会产生提醒。
func TestInv2ProposedItemsProduceNoDelivery(t *testing.T) {
	e := newLedger(t)
	deadline := e.now.Add(10 * time.Second)
	proposed := e.newItem(t, map[string]any{"phase": "proposed", "deadline": deadline})
	open := e.newItem(t, map[string]any{"deadline": deadline})
	e.tick(t)
	e.tick(t)
	if ds := e.deliveriesFor(t, open); len(ds) == 0 {
		t.Fatal("control failed: the open item with a passed deadline produced no delivery, so the proposed check proves nothing")
	}
	if ds := e.deliveriesFor(t, proposed); len(ds) != 0 {
		t.Fatalf("proposed item produced deliveries: %+v", ds)
	}
	// 提案不被当作承诺：按"进行中"阶段筛选看不到它。
	open2 := e.newItem(t, map[string]any{"kind": "commitment"})
	prop2 := e.newItem(t, map[string]any{"kind": "commitment", "phase": "proposed"})
	var ids []string
	if err := e.st.View(bg, func(tx *store.Tx) error { return listPhase(tx, domain.PhaseOpen, &ids) }); err != nil {
		t.Fatal(err)
	}
	var seenOpen, seenProp bool
	for _, id := range ids {
		seenOpen = seenOpen || id == open2
		seenProp = seenProp || id == prop2
	}
	if !seenOpen || seenProp {
		t.Fatalf("open commitment listed=%v, proposed commitment listed=%v", seenOpen, seenProp)
	}
}

// 不变量 3：waiting 必有 waiting_on；needs_input 必有 ask；delegation 进 done 必带证据。
func TestInv3PhaseEntryConstraints(t *testing.T) {
	e := newLedger(t)
	task := e.newItem(t, nil)
	dlg := e.newItem(t, map[string]any{"kind": "delegation"})
	tr := func(id string, in map[string]any) error {
		in["id"] = id
		_, err := e.do(owner, "item.transition", "", in)
		return err
	}
	for name, c := range map[string]struct {
		id string
		in map[string]any
	}{
		"waiting without waiting_on":   {task, map[string]any{"to": "waiting", "reason": "r"}},
		"needs_input without ask":      {task, map[string]any{"to": "needs_input", "reason": "r"}},
		"delegation done w/o evidence": {dlg, map[string]any{"to": "done", "reason": "r"}},
		"blank evidence is none":       {dlg, map[string]any{"to": "done", "reason": "r", "evidence": "  "}},
	} {
		before := len(e.events(t, c.id))
		if err := tr(c.id, c.in); exit(err) != domain.ExitUsage {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
		if got := e.get(t, c.id).Phase; got != domain.PhaseOpen {
			t.Errorf("%s: phase moved to %s", name, got)
		}
		if len(e.events(t, c.id)) != before {
			t.Errorf("%s: refused transition wrote an Event", name)
		}
	}
	if err := tr(task, map[string]any{"to": "waiting", "reason": "r", "waiting_on": map[string]any{"kind": "human", "ref": "bob"}}); err != nil {
		t.Fatal(err)
	}
	if it := e.get(t, task); it.WaitingOn == nil || it.WaitingOn.Ref != "bob" {
		t.Fatalf("waiting_on not recorded: %+v", it)
	}
	e.must(t, owner, "item.update", map[string]any{"id": task, "ask": map[string]any{"question": "ship?"}})
	if err := tr(task, map[string]any{"to": "needs_input", "reason": "r"}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, "item.update", "", map[string]any{"id": task, "ask": nil}); exit(err) != domain.ExitUsage {
		t.Fatalf("clearing ask while needs_input must be refused: %v", err)
	}
	if err := tr(dlg, map[string]any{"to": "done", "reason": "r", "evidence": "receipt #1"}); err != nil {
		t.Fatalf("delegation done with evidence: %v", err)
	}
	e.verify(t)
}

// 不变量 4：web_only 由策略规则给出，初筛只升不降，创建时的字段不能降低门槛，初筛返回前不投递、不接受代答。
func TestInv4WebOnlyOnlyEscalates(t *testing.T) {
	t.Run("pending screening blocks delivery and delegate answers, then escalation sticks", func(t *testing.T) {
		e := newLedger(t)
		e.cls.SetDefaultPYes(0.95)
		e.cls.SetFault(modelstub.ClsFault{DelayMS: 300})
		id := e.newItem(t, nil)
		e.must(t, worker, "decision.request", map[string]any{"id": id, "ask": ask(map[string]any{"default_on_timeout": "A", "deadline": t0.Add(time.Hour)})})
		if it := e.get(t, id); !it.Ask.ScreeningPending {
			t.Fatalf("request must commit with screening pending: %+v", it.Ask)
		}
		_, err := e.do(delegate, "decision.answer", "owner said A", map[string]any{"id": id, "choice": "A"})
		if exit(err) != domain.ExitScreeningPending {
			t.Fatalf("answer before screening returns: want 22, got %v", err)
		}
		if ds := e.deliveriesFor(t, id); len(ds) != 0 {
			t.Fatalf("delivery while screening pending: %+v", ds)
		}
		e.scr.Wait()
		it := e.get(t, id)
		if it.Ask.RiskClass != domain.RiskClassWebOnly || it.Ask.ScreeningPending || it.Ask.DefaultOnTimeout != "" {
			t.Fatalf("escalation: %+v", it.Ask)
		}
		_, err = e.do(delegate, "decision.answer", "owner said A", map[string]any{"id": id, "choice": "A"})
		if exit(err) != domain.ExitWebOnly {
			t.Fatalf("answer after escalation: want 20, got %v", err)
		}
		for _, d := range e.deliveriesFor(t, id) {
			if !d.Content.WebOnly || len(d.Content.Options) != 0 {
				t.Fatalf("escalated delivery must only link to Web: %+v", d)
			}
		}
	})
	t.Run("a rule-given class is never downgraded by a calm model", func(t *testing.T) {
		e := newLedger(t)
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			_, err := policy.Update(tx, owner.Actor, "rule", func(p *policy.Policy) error {
				p.WebOnly.Rules = append(p.WebOnly.Rules, policy.WebOnlyRule{Tag: "inv-finance", Category: policy.CategoryFunds})
				return nil
			})
			return err
		}); err != nil {
			t.Fatal(err)
		}
		id := e.newItem(t, map[string]any{"tags": []string{"inv-finance"}})
		e.must(t, worker, "decision.request", map[string]any{"id": id, "ask": ask(nil)})
		e.scr.Wait()
		if it := e.get(t, id); it.Ask.RiskClass != policy.CategoryFunds || it.Ask.ScreeningPending {
			t.Fatalf("rule class must stay: %+v", it.Ask)
		}
		_, err := e.do(delegate, "decision.answer", "owner said A", map[string]any{"id": id, "choice": "A"})
		if exit(err) != domain.ExitWebOnly {
			t.Fatalf("want 20, got %v", err)
		}
	})
	t.Run("fields written at creation cannot lower the bar", func(t *testing.T) {
		e := newLedger(t)
		for _, k := range []string{"risk_class", "screening_pending"} {
			in := map[string]any{"title": "t", "kind": "task", "phase": "open", "ask": map[string]any{"question": "q", k: "none"}}
			if k == "screening_pending" {
				in["ask"] = map[string]any{"question": "q", k: false}
			}
			if _, err := e.do(owner, "item.create", "", in); exit(err) != domain.ExitUsage {
				t.Fatalf("%s at creation must be refused, got %v", k, err)
			}
		}
	})
}

// 不变量 5：项目设置了 allowed_executors 时，任何入口都不能把事项指派给名单外的执行者。
func TestInv5AllowedExecutorsBindEveryEntry(t *testing.T) {
	e := newLedger(t)
	var p struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": "P"}).Value, &p)
	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": p.ID, "executors": []string{"bob"}})
	inside := e.newItem(t, map[string]any{"project": p.ID, "assignee": "bob"})
	loose := e.newItem(t, map[string]any{"assignee": "alice"})

	for name, c := range map[string]struct {
		id  command.Identity
		cmd string
		in  map[string]any
	}{
		"create (owner)":             {owner, "item.create", map[string]any{"kind": "task", "title": "t", "project": p.ID, "assignee": "alice"}},
		"reassign (owner)":           {owner, "item.update", map[string]any{"id": inside, "assignee": "alice"}},
		"reassign (delegate)":        {delegate, "item.update", map[string]any{"id": inside, "assignee": "alice"}},
		"move an outsider's item in": {owner, "item.update", map[string]any{"id": loose, "project": p.ID}},
		"create proposed (worker)":   {worker, "item.create", map[string]any{"kind": "task", "title": "t", "phase": "proposed", "project": p.ID, "assignee": "alice"}},
	} {
		before := len(e.events(t, ""))
		_, err := e.do(c.id, c.cmd, "owner said so", c.in)
		if exit(err) != domain.ExitExecutorNotAllowed {
			t.Errorf("%s: want exit 21, got %v", name, err)
		}
		// 拒绝只留一条 command.rejected，账本事实不变。
		es := e.events(t, "")
		if len(es) != before+1 || es[len(es)-1].Type != domain.EvCommandRejected {
			t.Errorf("%s: want exactly one command.rejected, got %d new events", name, len(es)-before)
		}
	}
	if it := e.get(t, inside); it.Assignee != "bob" {
		t.Fatalf("assignee changed: %+v", it)
	}
	if it := e.get(t, loose); it.Project != "" {
		t.Fatalf("item moved into the project: %+v", it)
	}
	e.must(t, owner, "item.update", map[string]any{"id": inside, "assignee": "owner"}) // 本人始终允许
	e.verify(t)
}

func listPhase(tx *store.Tx, ph domain.Phase, ids *[]string) error {
	its, err := items.List(tx, items.Filter{Phase: ph})
	for _, it := range its {
		*ids = append(*ids, it.ID)
	}
	return err
}
