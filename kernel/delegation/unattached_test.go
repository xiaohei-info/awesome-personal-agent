package delegation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 路径 B（§6.17）：派了没挂的活先记下（未挂事项），挂上时与 apa delegate 同一路径建子事项并把记录挪过去；
// 之后执行者的结果按会话句柄回执，内核找到子事项，回报同时成为父事项的进展；标为不跟踪后不再在跟。
func TestUnattachedDispatchAttachAndReportByHandle(t *testing.T) {
	e := setup(t)
	parent := e.id(t, e.must(t, owner, "item.create", map[string]any{"kind": "task", "title": "发布 0.1", "phase": "open"}))

	var d Dispatch
	r := e.must(t, e.as("DEL"), "delegate.dispatch", map[string]any{"id": parent, "assignee": workHost, "session": "agt_1", "resumable": true})
	_ = json.Unmarshal(r.Value, &d)
	if d.Attached() || d.About != parent || d.Origin != "s1" || d.Executor != workHost {
		t.Fatalf("a dispatch naming a non-delegated item is recorded unattached with that item as a hint: %+v", d)
	}
	// 同一会话再来一次（插件与模型手动补记撞上）只留一条。
	e.must(t, e.as("DEL"), "delegate.dispatch", map[string]any{"assignee": workHost, "session": "agt_1"})
	var open []Dispatch
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { open, err = Unattached(tx); return })
	if len(open) != 1 {
		t.Fatalf("unattached = %+v", open)
	}
	// 没挂之前按句柄回执：明确拒绝，告诉它先挂上。
	if de := e.wantExit(t, 6, e.as("DEL"), "delegate.report", map[string]any{"session": "agt_1", "outcome": "progress", "summary": "x"}); !strings.Contains(de.Next, "attach") {
		t.Fatalf("next = %q", de.Next)
	}

	var c created
	_ = json.Unmarshal(e.must(t, e.as("DEL"), "delegate.attach", map[string]any{"id": d.ID, "parent": parent, "title": "README"}).Value, &c)
	var got Dispatch
	var prog []domain.Progress
	read := func() {
		t.Helper()
		if err := e.st.View(bg, func(tx *store.Tx) (err error) {
			if got, err = GetDispatch(tx, d.ID); err != nil {
				return err
			}
			prog, err = items.Progress(tx, parent)
			return err
		}); err != nil {
			t.Fatal(err)
		}
	}
	read()
	if got.Item != c.ID || c.Parent != parent {
		t.Fatalf("attach moves the record to the new child: %+v %+v", got, c)
	}
	e.must(t, e.as("DEL"), "delegate.report", map[string]any{"session": "agt_1", "outcome": "done", "summary": "README 已重写",
		"evidence": map[string]string{"pr": "https://git.example/o/r/pull/3"}})
	read()
	if len(prog) != 1 || !strings.Contains(prog[0].Text, "README 已重写") || !strings.Contains(prog[0].Text, "pull/3") {
		t.Fatalf("the report is also progress on the parent: %+v", prog)
	}
	var p domain.Item
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { p, err = items.Get(tx, parent); return })
	if p.Phase != domain.PhaseOpen {
		t.Fatalf("all children done: parent back to open, got %s", p.Phase)
	}

	// 研究类的派活：标为不跟踪，之后不在未挂事项里。
	_ = json.Unmarshal(e.must(t, e.as("DEL"), "delegate.dispatch", map[string]any{"assignee": workHost, "session": "agt_2"}).Value, &d)
	e.must(t, e.as("DEL"), "delegate.dispatch_update", map[string]any{"id": d.ID, "state": "ended"})
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { open, err = Unattached(tx); return })
	if len(open) != 0 {
		t.Fatalf("untracked dispatch still listed: %+v", open)
	}
}
