package kcontext

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// agent 是主会话里的 Agent：带会话身份（APA_SESSION）的调用。
var agent = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Harness: "h", Session: "main"}, Level: authz.LevelDelegate}

// 工作台概览（§7.6）：不属于任何项目的会话从头读，按组列出全部项目里的事，每组至多 5 行，多出的给条数与查询命令；
// 本会话写过的事项进"本会话在跟"。
func TestWorkbenchOverviewGroupsAndTracking(t *testing.T) {
	e := setup(t)
	p := e.project(t, "biz", "https://git.example/org/biz", "h", "/repo/biz")
	for i := 0; i < 7; i++ {
		e.item(t, map[string]any{"title": fmt.Sprintf("biz %d", i), "project": p})
	}
	mine := e.id(t, agent, "item.create", map[string]any{"kind": "task", "title": "主会话建的", "phase": "proposed"})
	r := e.build(t, Request{Session: "main~1764200000000000000"}) // 压缩后插件换的游标键：会话仍是 main
	groups := map[string]OverviewGroup{}
	for _, g := range r.Overview {
		groups[g.Key+g.Project] = g
	}
	if g := groups[GroupProject+p]; len(g.Items) != OverviewGroupRows || g.More != 2 || g.Query != "apa item list --phase open --project "+p {
		t.Fatalf("project group: %+v", g)
	}
	if g := groups[GroupDecide]; len(g.Items) != 1 || g.Items[0].ID != mine {
		t.Fatalf("decide group: %+v", g)
	}
	if _, dup := groups[GroupTracking]; dup {
		t.Fatal("an item is listed only in its first group")
	}
	out := r.Inject()
	for _, want := range []string{"## 等你决定", "## 项目 biz（" + p + "）未结", "另有 2 条未列出：apa item list --phase open --project " + p} {
		if !strings.Contains(out, want) {
			t.Fatalf("lacks %q:\n%s", want, out)
		}
	}
	if ts, err := trackingOf(e, "main"); err != nil || len(ts) != 1 || ts[0] != mine {
		t.Fatalf("tracking = %v %v", ts, err)
	}
}

func trackingOf(e *env, session string) ([]string, error) {
	var ids []string
	err := e.st.View(bg, func(tx *store.Tx) error {
		its, err := Tracking(tx, session, 0)
		for _, it := range its {
			ids = append(ids, it.ID)
		}
		return err
	})
	return ids, err
}

// 增量（§7.6）：事件变了但注入里那一行没变的事项不带；只改普通字段的事项，工作台视图里只对本会话在跟的给出。
func TestWorkbenchIncrementSkipsInvisibleChanges(t *testing.T) {
	e := setup(t)
	a := e.item(t, map[string]any{"title": "A"})
	b := e.id(t, agent, "item.create", map[string]any{"kind": "task", "title": "B"}) // 默认卡：Agent 建的是建议
	e.build(t, Request{Session: "main"})
	e.do(t, owner, "item.update", map[string]any{"id": a, "body": "正文改了，行没变"})
	if r := e.build(t, Request{Session: "main"}); len(r.Items) != 0 || r.Inject() != "" {
		t.Fatalf("an invisible change must not be injected: %v %q", ids(r), r.Inject())
	}
	e.do(t, owner, "item.update", map[string]any{"id": a, "title": "A2"}) // 普通字段，A 不在本会话在跟
	e.do(t, owner, "item.update", map[string]any{"id": b, "title": "B2"}) // B 是本会话建的
	if r := e.build(t, Request{Session: "main"}); !eq(ids(r), []string{b}) {
		t.Fatalf("field updates only for tracked items: %v", ids(r))
	}
	e.do(t, owner, "item.progress", map[string]any{"id": a, "text": "有了新进展"})
	r := e.build(t, Request{Session: "main"})
	if !eq(ids(r), []string{a}) || r.Items[0].Progress != "有了新进展" || !strings.Contains(r.Inject(), "  进展：有了新进展") {
		t.Fatalf("progress is an item-level change and is shown: %+v", r.Items)
	}
}

// 未挂事项的派活（§6.17）在记下它的会话里每轮都提示；整份注入不超过 InjectBudget，截掉的事项行写明另有几条。
func TestUnattachedDispatchEveryTurnAndInjectBudget(t *testing.T) {
	e := setup(t)
	if err := executors.Register(e.x, executors.Options{Owner: "owner"}); err != nil {
		t.Fatal(err)
	}
	if err := delegation.Register(e.x, delegation.Options{Owner: "owner", OnError: func(command.Commit, error) {}}); err != nil {
		t.Fatal(err)
	}
	parent := e.item(t, map[string]any{"title": "发布"})
	e.do(t, agent, "delegate.dispatch", map[string]any{"id": parent, "assignee": "x.cli", "session": "agt_1"})
	for turn := 0; turn < 2; turn++ {
		out := e.build(t, Request{Session: "main"}).Inject()
		if !strings.Contains(out, "未挂事项的派活") || !strings.Contains(out, "--parent "+parent) {
			t.Fatalf("turn %d lacks the unattached dispatch:\n%s", turn, out)
		}
	}
	if out := e.build(t, Request{Session: "other"}).Inject(); strings.Contains(out, "未挂事项的派活") {
		t.Fatalf("another session must not be reminded:\n%s", out)
	}

	long := strings.Repeat("很长的标题", 30)
	var many []Entry
	for i := 0; i < 60; i++ {
		many = append(many, Entry{ID: fmt.Sprintf("itm_%04d", i), Title: long, Phase: domain.PhaseOpen, Action: "apa item show x"})
	}
	out := Result{Items: many}.Inject()
	if n := len([]rune(out)); n > InjectBudget+80 || !strings.Contains(out, "条未列出：apa context --scope all") || !strings.HasSuffix(out, envelopeClose+"\n") {
		t.Fatalf("budget: %d runes\n%s", n, out[len(out)-200:])
	}
}
