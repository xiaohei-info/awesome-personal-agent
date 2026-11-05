//go:build e2e

package ledger

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// TestFlowItemLifecycle 按验收文档 flow-item-lifecycle 顺序执行 ACT-ITEM-01..13：
// 本人（Web 会话）和 agent（CLI/HTTP 凭据）经两个入口操作同一事项，阶段约束由内核强制。
func TestFlowItemLifecycle(t *testing.T) {
	e := baseline.Setup(t)
	var a, dg, dropped string // item-a、委派事项、被丢弃的事项

	t.Run("ACT-ITEM-01 CLI 建的事项在 Web 可见", func(t *testing.T) {
		a = idOf(t, e.Apa(e.DEL, "", "item", "add", "--kind", "task", "--phase", "open", "--title", "item-a",
			"--project", project(e, "acc-open"), "--evidence", "owner: track item-a"), "item add")
		r := e.Web("GET", "/v1/items/"+a, "")
		if r.Code != 200 {
			t.Fatalf("Web 读事项: %d %s", r.Code, r.Body)
		}
		it, _ := parse(t, r.Body)["item"].(map[string]any)
		if it["title"] != "item-a" || it["phase"] != "open" {
			t.Errorf("Web 应显示 item-a / open: %s", r.Body)
		}
		steps := webSteps(t, e, a)
		if len(steps) != 1 || steps[0].Type != "item.created" || steps[0].Actor.Type != "agent" {
			t.Errorf("时间线应只有一条 agent 的创建记录: %+v", steps)
		}
	})

	t.Run("ACT-ITEM-02 Web 的修改在 CLI 可见", func(t *testing.T) {
		before := version(show(t, e, a))
		if r := e.Web("PATCH", "/v1/items/"+a, `{"status_label":"review"}`); r.Code != 200 {
			t.Fatalf("Web 改 status_label: %d %s", r.Code, r.Body)
		}
		it := show(t, e, a)
		if it["status_label"] != "review" || version(it) != before+1 {
			t.Errorf("CLI 应见 status_label=review、version=%d: %v", before+1, it)
		}
		_, steps := why(t, e, e.OBS, a)
		last := steps[len(steps)-1]
		if last.Type != "item.updated" || last.Actor.Type != "human" || last.Actor.Via != "web" {
			t.Errorf("最后一条 Event 应是本人经 web 的更新: %+v", last)
		}
	})

	t.Run("ACT-ITEM-03 waiting 必须有 waiting_on", func(t *testing.T) {
		r := e.Apa(e.DEL, "", "item", "transition", a, "--to", "waiting", "--reason", "wait for vendor", "--evidence", "owner: wait")
		if r.Code == 0 {
			t.Fatal("没有 waiting_on 也进入了 waiting")
		}
		fail(t, r, r.Code, "waiting 无 waiting_on")
		if p := show(t, e, a)["phase"]; p != "open" {
			t.Fatalf("失败后阶段应不变，实际 %v", p)
		}
		ok(t, e.Apa(e.DEL, "", "item", "transition", a, "--to", "waiting", "--reason", "wait for vendor",
			"--waiting-on", "external:报价单", "--evidence", "owner: wait"), "带 waiting_on 重试")
		it := show(t, e, a)
		wo, _ := it["waiting_on"].(map[string]any)
		if it["phase"] != "waiting" || wo["kind"] != "external" || wo["ref"] != "报价单" {
			t.Errorf("应进入 waiting 且 waiting_on={external,报价单}: %v", it)
		}
	})

	t.Run("ACT-ITEM-04 needs_input 必须有 ask", func(t *testing.T) {
		r := e.Apa(e.DEL, "", "item", "transition", a, "--to", "needs_input", "--reason", "need input", "--evidence", "owner: ask me")
		if r.Code == 0 {
			t.Fatal("没有 ask 也进入了 needs_input")
		}
		fail(t, r, r.Code, "needs_input 无 ask")
		if p := show(t, e, a)["phase"]; p != "waiting" {
			t.Fatalf("失败后阶段应不变(waiting)，实际 %v", p)
		}
	})

	t.Run("ACT-ITEM-05 dropped 必须有 reason", func(t *testing.T) {
		dropped = addTask(t, e, "to-drop")
		r := e.Apa(e.DEL, "", "item", "transition", dropped, "--to", "dropped", "--evidence", "owner: drop it")
		if r.Code == 0 {
			t.Fatal("没有 reason 也进入了 dropped")
		}
		fail(t, r, r.Code, "dropped 无 reason")
		if p := show(t, e, dropped)["phase"]; p != "open" {
			t.Fatalf("失败后阶段应不变，实际 %v", p)
		}
		ok(t, e.Apa(e.DEL, "", "item", "transition", dropped, "--to", "dropped", "--reason", "no longer needed", "--evidence", "owner: drop it"), "带 reason 重试")
		if p := show(t, e, dropped)["phase"]; p != "dropped" {
			t.Errorf("应进入 dropped 且仍可查询，实际 %v", p)
		}
	})

	t.Run("ACT-ITEM-06 delegation 进 done 必须带证据", func(t *testing.T) {
		parent := addTask(t, e, "dg-parent", "--project", project(e, "acc-work"), "--assignee", "work-host.cli")
		dg = idOf(t, e.Apa(e.WRK, "", "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", "dg",
			"--reason", "hand the work to the executor"), "新建 delegation")
		if k := show(t, e, dg)["kind"]; k != "delegation" {
			t.Fatalf("委派子事项 kind 应是 delegation，实际 %v", k)
		}
		r := e.Apa(e.WRK, "", "item", "transition", dg, "--to", "done", "--reason", "finished")
		if r.Code == 0 {
			t.Fatal("没有证据也完成了 delegation")
		}
		fail(t, r, r.Code, "delegation 无证据 done")
		if p := show(t, e, dg)["phase"]; p != "open" {
			t.Fatalf("失败后阶段应不变，实际 %v", p)
		}
		ok(t, e.Apa(e.WRK, "", "report", dg, "--outcome", "done", "--summary", "shipped", "--pr", "https://github.com/acc/test-repo/pull/1"), "report done")
		if p := show(t, e, dg)["phase"]; p != "done" {
			t.Errorf("带证据后应进入 done，实际 %v", p)
		}
	})

	t.Run("ACT-ITEM-07 终态可以重开", func(t *testing.T) {
		for _, id := range []string{dg, dropped} {
			ok(t, e.Apa(e.DEL, "", "item", "transition", id, "--to", "open", "--reason", "reopen "+id, "--evidence", "owner: reopen"), "重开 "+id)
			if p := show(t, e, id)["phase"]; p != "open" {
				t.Errorf("%s 应回到 open，实际 %v", id, p)
			}
			_, steps := why(t, e, e.DEL, id)
			last := steps[len(steps)-1]
			if last.Type != "item.phase_changed" || last.Reason != "reopen "+id {
				t.Errorf("%s 应记一条带 reason 的阶段变更 Event: %+v", id, last)
			}
		}
	})

	t.Run("ACT-ITEM-08 并发修改由版本号裁决", func(t *testing.T) {
		b := addTask(t, e, "race")
		n := version(show(t, e, b))
		var wg sync.WaitGroup
		codes := make([]int, 2)
		for i := range codes {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				r := req(t, e, "PATCH", "/v1/items/"+b, fmt.Sprintf(`{"title":"racer-%d"}`, i), e.DEL,
					map[string]string{"If-Match": fmt.Sprint(n), "X-APA-Evidence": "owner: rename"})
				codes[i] = r.Code
			}(i)
		}
		wg.Wait()
		wins, conflicts, winner := 0, 0, -1
		for i, c := range codes {
			switch c {
			case 200:
				wins, winner = wins+1, i
			case 409:
				conflicts++
			}
		}
		if wins != 1 || conflicts != 1 {
			t.Fatalf("应恰好一次成功、一次 409，实际 %v", codes)
		}
		it := show(t, e, b)
		if it["title"] != fmt.Sprintf("racer-%d", winner) || version(it) != n+1 {
			t.Errorf("事项应只反映成功的那次: %v", it)
		}
		fail(t, e.Apa(e.DEL, "", "item", "update", b, "--title", "stale", "--version", fmt.Sprint(n), "--reason", "r", "--evidence", "e"), 6, "CLI 过期版本")
	})

	t.Run("ACT-ITEM-09 幂等键防止重复写入", func(t *testing.T) {
		body := `{"kind":"task","title":"idem-item","phase":"open","evidence":"owner: once"}`
		h := map[string]string{"Idempotency-Key": "acc-key-1"}
		r1 := req(t, e, "POST", "/v1/items", body, e.DEL, h)
		r2 := req(t, e, "POST", "/v1/items", body, e.DEL, h)
		if r1.Code != 201 || (r2.Code != 200 && r2.Code != 201) {
			t.Fatalf("两次 POST: %d %s / %d %s", r1.Code, r1.Body, r2.Code, r2.Body)
		}
		id1, id2 := parse(t, r1.Body)["id"], parse(t, r2.Body)["id"]
		if id1 == "" || id1 != id2 {
			t.Fatalf("两次应返回同一事项 id: %v %v", id1, id2)
		}
		if parse(t, r2.Body)["replayed"] != true {
			t.Errorf("第二次应标记 replayed: %s", r2.Body)
		}
		list := parse(t, e.Web("GET", "/v1/items", "").Body)["items"].([]any)
		n := 0
		for _, x := range list {
			if x.(map[string]any)["title"] == "idem-item" {
				n++
			}
		}
		if c := len(ofType(audit(t, e, "object="+fmt.Sprint(id1)), "item.created")); n != 1 || c != 1 {
			t.Errorf("账本应只多一个事项和一条 item.created，实际事项 %d 条、item.created %d 条", n, c)
		}
	})

	t.Run("ACT-ITEM-10 调用方不能写内核维护的字段", func(t *testing.T) {
		c := addTask(t, e, "kernel-fields")
		ok(t, e.Apa(e.DEL, "", "item", "update", c, "--ask", `{"question":"q?","options":[{"id":"a","label":"A"}]}`, "--reason", "prep", "--evidence", "owner: prep"), "准备 ask")
		snap := func() [3]any {
			it := show(t, e, c)
			ask, _ := it["ask"].(map[string]any)
			return [3]any{it["chain_depth"], it["unattended"], ask["risk_class"]}
		}
		before := snap()
		for _, body := range []string{`{"chain_depth":99}`, `{"unattended":true}`, `{"ask":{"question":"q?","risk_class":"none"}}`} {
			r := req(t, e, "PATCH", "/v1/items/"+c, body, e.DEL, map[string]string{"X-APA-Evidence": "owner: tamper"})
			t.Logf("PATCH %s → %d", body, r.Code)
			if after := snap(); after != before {
				t.Errorf("%s 改写了内核字段: %v → %v", body, before, after)
			}
		}
	})

	t.Run("ACT-ITEM-11 按 kind 校验 spec，ext 原样透传", func(t *testing.T) {
		count := func() int { return len(parse(t, e.Web("GET", "/v1/items?kind=brief", "").Body)["items"].([]any)) }
		n := count()
		r := e.Apa(e.DEL, "", "item", "add", "--kind", "brief", "--phase", "open", "--title", "bad-brief", "--spec", `{"period":5}`, "--evidence", "owner: brief")
		fail(t, r, 2, "非法 brief spec")
		if count() != n {
			t.Error("被拒绝的 brief 不应入账")
		}
		spec := `{"anything":[1,{"k":"v"}],"n":null}`
		ext := `{"github":{"pr":88}}`
		u := idOf(t, e.Apa(e.DEL, "", "item", "add", "--kind", "acc-unregistered", "--phase", "open", "--title", "unregistered", "--evidence", "owner: x",
			"--spec", spec), "未注册 kind")
		if r := req(t, e, "PATCH", "/v1/items/"+u, `{"ext":`+ext+`}`, e.DEL, map[string]string{"X-APA-Evidence": "owner: ext"}); r.Code != 200 {
			t.Fatalf("写 ext: %d %s", r.Code, r.Body)
		}
		var wantSpec, wantExt any
		json.Unmarshal([]byte(spec), &wantSpec)
		json.Unmarshal([]byte(ext), &wantExt)
		for name, it := range map[string]map[string]any{"CLI": show(t, e, u), "Web": parse(t, e.Web("GET", "/v1/items/"+u, "").Body)["item"].(map[string]any)} {
			if !sameJSON(it["spec"], wantSpec) || !sameJSON(it["ext"], wantExt) {
				t.Errorf("%s 应原样返回 spec 与 ext: %v", name, it)
			}
		}
	})

	t.Run("ACT-ITEM-12 关联事项并按标签筛选", func(t *testing.T) {
		x, y := addTask(t, e, "rel-x"), addTask(t, e, "rel-y")
		ok(t, e.Apa(e.DEL, "", "item", "update", x, "--tags", "alpha,beta", "--reason", "tag", "--evidence", "owner: tag"), "加 tag")
		ok(t, e.Apa(e.DEL, "", "item", "relate", x, "--to", y, "--type", "relates", "--evidence", "owner: relate"), "relate")
		rels, _ := ok(t, e.Apa(e.OBS, "", "item", "show", x), "show")["relations"].([]any)
		if len(rels) != 1 || rels[0].(map[string]any)["to"] != y || rels[0].(map[string]any)["type"] != "relates" {
			t.Errorf("item show 应看到 relates 边: %v", rels)
		}
		// Web 事项页的 tag 筛选读 GET /v1/items 并按 tags 过滤，这里按同一规则复算。
		var got []string
		for _, x := range parse(t, e.Web("GET", "/v1/items", "").Body)["items"].([]any) {
			it := x.(map[string]any)
			tags, _ := it["tags"].([]any)
			for _, tg := range tags {
				if tg == "beta" {
					got = append(got, it["title"].(string))
				}
			}
		}
		if len(got) != 1 || got[0] != "rel-x" {
			t.Errorf("按 beta 筛选应只得 rel-x，实际 %v", got)
		}
	})

	t.Run("ACT-ITEM-13 读取不存在的对象返回退出码 5", func(t *testing.T) {
		b := fail(t, e.Apa(e.OBS, "", "item", "show", "item_not_exist"), 5, "item show 不存在")
		if b["code"] != "not_found" {
			t.Errorf("错误码应为 not_found: %v", b)
		}
	})
}
