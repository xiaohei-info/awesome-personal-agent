package invariants_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 投影由 Event 重建且结果一致（§6.7、§20.1 M1）：跑一组覆盖项目、事项、决策请求的命令，
// 事项（含 ask）与项目的每个字段都和事件折叠逐字段比较；再丢弃事项投影重建，重建结果与原投影相同。
func TestProjectionRebuildFromEventsMatchesAllFields(t *testing.T) {
	e := newLedger(t)
	var p struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": "P", "intro": "i", "tags": []string{"a"}}).Value, &p)
	e.must(t, owner, "project.update", map[string]any{"id": p.ID, "name": "P2", "status": "paused", "capture": true})
	e.must(t, delegate, "project.repo.add", map[string]any{"id": p.ID, "remote": "https://git.example/x/app.git"})
	e.must(t, owner, "project.checkout.set", map[string]any{"id": p.ID, "remote": "https://git.example/x/app.git", "host": "h1", "path": "/tmp/app"})
	e.must(t, owner, "project.capture_outbound.set", map[string]any{"id": p.ID, "value": "excerpt"})
	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": p.ID, "executors": []string{"wrk"}})
	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": p.ID, "executors": []string{}})
	other := struct{ ID string }{}
	_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": "Q"}).Value, &other)

	e.newItem(t, map[string]any{"project": p.ID, "assignee": "owner"})
	id := e.newItem(t, nil)
	e.must(t, worker, "decision.request", map[string]any{"id": id, "ask": ask(nil)})
	asked := e.get(t, id)
	if asked.Ask == nil {
		t.Fatal("decision request not recorded")
	}
	e.must(t, owner, "decision.answer", map[string]any{"id": id, "choice": "A"})
	e.must(t, owner, "item.update", map[string]any{"id": id, "title": "renamed"})
	e.newItem(t, nil)

	// 初筛在提交后异步返回并再写一条 Event；等它落定，两次快照之间才没有新写入。
	for end := time.Now().Add(10 * time.Second); e.get(t, id).Ask.ScreeningPending; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("screening never returned")
		}
	}

	if err := e.st.View(bg, projects.Verify); err != nil {
		t.Fatalf("project projection diverges from events: %v", err)
	}
	e.verify(t)

	// 篡改任一投影字段都能被发现。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE projects SET intro = 'drifted' WHERE id = ?", p.ID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.View(bg, projects.Verify); err == nil {
		t.Fatal("project drift went undetected")
	}

	// 事项投影重建后与原投影逐字段相同。
	var before, after []domain.Item
	list := func(dst *[]domain.Item) func(tx *store.Tx) error {
		return func(tx *store.Tx) (err error) { *dst, err = items.List(tx, items.Filter{}); return }
	}
	if err := e.st.View(bg, list(&before)); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Update(bg, items.Rebuild); err != nil {
		t.Fatal(err)
	}
	if err := e.st.View(bg, list(&after)); err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 || !reflect.DeepEqual(before, after) {
		t.Fatalf("rebuilt items differ \nbefore %+v\nafter  %+v", before, after)
	}
}
