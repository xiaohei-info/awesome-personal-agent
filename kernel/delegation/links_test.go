package delegation

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) vaults(t *testing.T, vs ...policy.NoteVault) {
	t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "vaults", func(p *policy.Policy) error { p.NoteVaults = vs; return nil })
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) linked(t *testing.T) (ms []materials.Material) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { ms, err = materials.List(tx, ""); return }); err != nil {
		t.Fatal(err)
	}
	return ms
}

func (e *env) relations(t *testing.T, id string) (rs []domain.Relation) {
	t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { rs, err = items.Relations(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return rs
}

func output(rs []domain.Relation) (out []string) {
	for _, r := range rs {
		if r.Type == domain.RelOutput {
			out = append(out, r.To)
		}
	}
	return out
}

// flow-report-links：done 回执里落在已登记库内的链接，同事务登记为链接型材料并以 output 关联到父事项；库外链接与网址不登记。
func TestDoneReceiptRegistersVaultLinksAsOutputs(t *testing.T) {
	e := setup(t)
	e.vaults(t, policy.NoteVault{Name: "notes", OpenURL: "app://open?v={vault}&p={path}"})
	proj := e.project(t, "acc-work", []string{workHost})
	parent := e.parent(t, proj)
	child := e.delegate(t, e.as("DEL"), parent, workHost)

	e.must(t, e.as("WRK"), "delegate.report", map[string]any{"id": child, "outcome": "done", "summary": "s", "evidence": map[string]any{
		"link": []string{"vault:notes/设计/方案.md", "https://example.com/pr/1", "vault:other/x.md", "vault:notes/../x.md", "vault:notes/设计/方案.md", "vault:notes/纪要.md"}}})

	ms := e.linked(t)
	if len(ms) != 2 {
		t.Fatalf("materials = %+v", ms)
	}
	byLink := map[string]materials.Material{}
	for _, m := range ms {
		byLink[m.Link] = m
		if m.Form != materials.FormLink || m.Kind != "reference" || m.Project != proj {
			t.Fatalf("material = %+v", m)
		}
	}
	plan := byLink["vault:notes/设计/方案.md"]
	if plan.Title != "方案.md" || byLink["vault:notes/纪要.md"].Title != "纪要.md" {
		t.Fatalf("titles come from the file name: %+v", ms)
	}
	if got := output(e.relations(t, parent)); len(got) != 2 || !slices.Contains(got, plan.ID) {
		t.Fatalf("output relations of the parent = %v", got)
	}
	if got := e.item(t, parent); got.Phase != domain.PhaseOpen {
		t.Fatalf("parent phase = %s (reopen still works)", got.Phase)
	}

	// 操作者是 system，Basis 记策略版本与内置规则；证据里 link 仍是第一个，另带 links[]。
	var sawMaterial, sawRelation bool
	for _, ev := range e.events(t) {
		if ev.Actor.Type != domain.ActorSystem || ev.Actor.Principal != "kernel:delegation" {
			continue
		}
		if ev.Basis == nil || ev.Basis.RuleID != "delegate.report.vault_links" || ev.Basis.PolicyVersion == 0 {
			t.Fatalf("basis = %+v", ev.Basis)
		}
		sawMaterial = sawMaterial || (ev.Type == domain.EvMaterialRevised && ev.Object == plan.ID)
		sawRelation = sawRelation || (ev.Type == domain.EvItemUpdated && ev.Object == parent)
	}
	if !sawMaterial || !sawRelation {
		t.Fatalf("system events: material=%v relation=%v", sawMaterial, sawRelation)
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		rs, err := Reports(tx, child)
		if err != nil || len(rs) != 1 || rs[0].Evidence.Link != "vault:notes/设计/方案.md" || len(rs[0].Evidence.Links) != 6 {
			t.Fatalf("evidence = %+v %v", rs, err)
		}
		return nil
	})
	e.consistent(t)

	// 另一个子事项报同一位置：复用已登记的材料，父事项的 output 不重复。
	child2 := e.delegate(t, e.as("DEL"), parent, workHost)
	e.must(t, e.as("WRK"), "delegate.report", rep(child2, "done", Evidence{Link: "vault:notes/设计/方案.md"}))
	if len(e.linked(t)) != 2 || len(output(e.relations(t, parent))) != 2 {
		t.Fatal("the same location is registered once")
	}
	e.consistent(t)

	// ACT-RPL-04：本人移除这条关系，材料保留，关系变化记事件。
	before := countType(e.events(t), domain.EvItemUpdated)
	e.must(t, owner, "item.relate", map[string]any{"from": parent, "to": plan.ID, "type": "output", "remove": true, "reason": "not an output"})
	if got := output(e.relations(t, parent)); slices.Contains(got, plan.ID) || len(got) != 1 {
		t.Fatalf("relation not removed: %v", got)
	}
	if len(e.linked(t)) != 2 {
		t.Fatal("removing the relation must keep the material")
	}
	if countType(e.events(t), domain.EvItemUpdated) != before+1 {
		t.Fatal("removing the relation records an event")
	}
	e.consistent(t)
}

func TestReceiptLinksAreNotRegisteredOutsideTheirRule(t *testing.T) {
	e := setup(t)
	proj := e.project(t, "acc-work", nil)
	link := "vault:notes/a.md"

	// 未登记任何笔记库：没有自动登记，证据照旧。
	parent := e.parent(t, proj)
	child := e.delegate(t, e.as("DEL"), parent, workHost)
	e.must(t, e.as("WRK"), "delegate.report", rep(child, "done", Evidence{Link: link}))
	if len(e.linked(t)) != 0 {
		t.Fatal("no vault registered: nothing to register")
	}

	e.vaults(t, policy.NoteVault{Name: "notes"})
	// progress 与 blocked 的回执不登记。
	p2 := e.parent(t, proj)
	c2 := e.delegate(t, e.as("DEL"), p2, workHost)
	e.must(t, e.as("WRK"), "delegate.report", rep(c2, "progress", Evidence{Link: link}))
	e.must(t, e.as("WRK"), "delegate.report", rep(c2, "blocked", Evidence{Link: link}))
	if len(e.linked(t)) != 0 {
		t.Fatal("progress and blocked receipts do not register links")
	}
	// 父事项没有项目：不登记。
	p3 := e.parent(t, "")
	c3 := e.delegate(t, e.as("DEL"), p3, workHost)
	e.must(t, e.as("WRK"), "delegate.report", rep(c3, "done", Evidence{Link: link}))
	if len(e.linked(t)) != 0 {
		t.Fatal("a parent without a project registers nothing")
	}
	e.consistent(t)
}

func TestReceiptLinkAcceptsStringOrArrayAndKeepsFirstAsLink(t *testing.T) {
	var ev Evidence
	if err := json.Unmarshal([]byte(`{"link":"https://a"}`), &ev); err != nil || ev.Link != "https://a" || !slices.Equal(ev.Links, []string{"https://a"}) {
		t.Fatalf("string: %+v %v", ev, err)
	}
	if err := json.Unmarshal([]byte(`{"link":["https://a"," ","https://b"],"pr":"p"}`), &ev); err != nil || ev.Link != "https://a" || !slices.Equal(ev.Links, []string{"https://a", "https://b"}) || ev.PR != "p" {
		t.Fatalf("array: %+v %v", ev, err)
	}
	for _, bad := range []string{`{"link":3}`, `{"link":[1]}`, `{"nope":1}`} {
		if err := json.Unmarshal([]byte(bad), &ev); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	b, _ := json.Marshal(Evidence{Link: "x", Links: []string{"x", "y"}})
	if string(b) != `{"link":"x","links":["x","y"]}` {
		t.Fatalf("stored shape %s", b)
	}
	if (Evidence{}).empty() != true {
		t.Fatal("empty evidence")
	}
	_ = eventlog.Module
}
