//go:build e2e

package importwb

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestFlowImportApply 是 flow-import-apply（验收文档 ACT-IMP-01..11，OMAC-NOTES §3 I5 的导入部分）：
// 本人在 Web 登录下导入带原始时间线的项目与事项，时间线与输入一致，重放新增 0 条，预检不写账本。
func TestFlowImportApply(t *testing.T) {
	f := setup(t)
	const src = "acc-imp"
	plan := f.plan()
	p := f.planAt()
	var base map[string]int
	var seq0 int64
	var batch string
	var t1, t2 string

	f.step("ACT-IMP-01 计划文件四行，每行是合法 JSON", func(t *testing.T) {
		lines := strings.Split(strings.TrimSuffix(plan, "\n"), "\n")
		if len(lines) != 4 {
			t.Fatalf("计划文件应为 4 行，得到 %d", len(lines))
		}
		for i, l := range lines {
			decode[map[string]any](t, l)
			_ = i
		}
		base, seq0 = f.tableCounts(), f.maxSeq()
	})

	f.step("ACT-IMP-02 预检只报结果、不写账本", func(t *testing.T) {
		r := f.importWeb(src, "dry_run=1", plan, nil)
		if r.Code != 200 {
			t.Fatalf("预检: HTTP %d %s", r.Code, r.Body)
		}
		out := decode[importResult](t, r.Body)
		if out.Batch != "" {
			t.Errorf("预检不应有批次: %q", out.Batch)
		}
		want := []string{"created", "existing", "created", "created"}
		for i, l := range out.Results {
			if l.Outcome != want[i] {
				t.Errorf("预检第 %d 行 outcome=%s，期望 %s", i+1, l.Outcome, want[i])
			}
		}
		if got := out.Results[1].Object; got != f.Projects["acc-home"] {
			t.Errorf("② 应绑定已有项目 acc-home(%s)，得到 %q", f.Projects["acc-home"], got)
		}
		if out.Created != 3 || out.Existing != 1 || out.Invalid != 0 {
			t.Errorf("计数 = %+v", out)
		}
		if after := f.tableCounts(); !reflect.DeepEqual(after, base) || f.maxSeq() != seq0 {
			t.Errorf("预检写了账本: %v", diffCounts(base, after))
		}
		if bs := f.batches(); len(bs) != 0 {
			t.Errorf("预检不应建批次: %+v", bs)
		}
	})

	f.step("ACT-IMP-03 本人在 Web 登录下导入，不需要授权单", func(t *testing.T) {
		out := f.mustImport(src, "", plan)
		batch = out.Batch
		if !strings.HasPrefix(batch, "imp_") {
			t.Fatalf("批次号 %q", batch)
		}
		want := []string{"created", "existing", "created", "created"}
		for i, l := range out.Results {
			if l.Outcome != want[i] {
				t.Errorf("第 %d 行 outcome=%s，期望 %s", i+1, l.Outcome, want[i])
			}
		}
		t1, t2 = out.Results[2].Object, out.Results[3].Object
		bs := f.batches()
		if len(bs) != 1 || bs[0].ID != batch || bs[0].Source != src || bs[0].Actor != "owner" || bs[0].Grant != "" ||
			bs[0].Created != 3 || bs[0].Existing != 1 || bs[0].Invalid != 0 {
			t.Errorf("GET /imports = %+v", bs)
		}
	})

	f.step("ACT-IMP-04 时间线与输入一致", func(t *testing.T) {
		got := f.byOrigin(src, "T-0001")
		if len(got) != 1 || got[0].ID != t1 {
			t.Fatalf("按 origin 取 T-0001: %+v", got)
		}
		s := f.show(t1)
		it := s.Item
		if it.Origin == nil || it.Origin.Source != "import:"+src || it.Origin.Ref != "T-0001" {
			t.Errorf("origin = %+v", it.Origin)
		}
		if it.Imported == nil || it.Imported.Batch != batch || it.Imported.At.IsZero() {
			t.Errorf("imported = %+v", it.Imported)
		}
		if it.Phase != "done" || !it.CreatedAt.Equal(p.t0) || !it.UpdatedAt.Equal(p.t1Done) {
			t.Errorf("phase=%s created_at=%s updated_at=%s，期望 done %s %s", it.Phase, it.CreatedAt, it.UpdatedAt, p.t0, p.t1Done)
		}
		if len(s.Progress) != 5 {
			t.Fatalf("进展 %d 条，期望 5", len(s.Progress))
		}
		for i, pr := range s.Progress {
			if !pr.At.Equal(p.progressAt[i]) || pr.On != p.progressOn[i] || (pr.Precision == "day") != (i < 3) {
				t.Errorf("进展 %d = %+v，期望 at=%s on=%s precision day=%v", i+1, pr, p.progressAt[i], p.progressOn[i], i < 3)
			}
		}
		// 该事项的全部 Event：9 条，at 等于输入时刻并按 at 排列，每条带导入批次与写入时刻；precision 为 day 的载荷带标记。
		wantAt := []time.Time{p.t0, p.progressAt[0], p.progressAt[1], p.progressAt[2], p.progressAt[3], p.progressAt[4], p.t0.AddDate(0, 0, 6), p.t0.AddDate(0, 0, 7), p.t1Done}
		wantType := []string{"item.created", "item.progress_added", "item.progress_added", "item.progress_added", "item.progress_added", "item.progress_added",
			"item.phase_changed", "item.phase_changed", "item.phase_changed"}
		wantDay := []bool{false, true, true, true, false, false, false, false, true}
		evs := f.audit("imported=" + batch + "&object=" + t1)
		if len(evs) != 9 {
			t.Fatalf("事项的导入事件 %d 条，期望 9", len(evs))
		}
		for i, e := range evs {
			if e.Type != wantType[i] || !e.At.Equal(wantAt[i]) || e.Imported != batch || e.RecordedAt == "" {
				t.Errorf("事件 %d = %+v，期望 %s at=%s", i+1, e, wantType[i], wantAt[i])
			}
			if i > 0 && e.At.Before(evs[i-1].At) {
				t.Errorf("事件 %d 的 at 早于前一条", i+1)
			}
			if day := strings.Contains(string(e.Payload), `"precision":"day"`); day != wantDay[i] {
				t.Errorf("事件 %d precision=day 为 %v，期望 %v: %s", i+1, day, wantDay[i], e.Payload)
			}
		}
		if phases := phaseTargets(evs); !reflect.DeepEqual(phases, []string{"dropped", "open", "done"}) {
			t.Errorf("三次阶段转移 = %v", phases)
		}
	})

	f.step("ACT-IMP-05 项目记录按对照、名字绑定或新建", func(t *testing.T) {
		type proj struct {
			ID             string   `json:"id"`
			Name           string   `json:"name"`
			Status         string   `json:"status"`
			AllowedExec    []string `json:"allowed_executors"`
			CaptureOut     any      `json:"capture_outbound"`
			Repos          []any    `json:"repos"`
			AllowedPresent bool
		}
		list := decode[struct {
			Projects []proj `json:"projects"`
		}](t, f.apa(f.OBS, 0, "project", "list").Stdout).Projects
		var alpha *proj
		names := map[string]int{}
		for i, pj := range list {
			names[strings.ToLower(strings.TrimSpace(pj.Name))]++
			if pj.Name == "Alpha Imp" {
				alpha = &list[i]
			}
		}
		if alpha == nil {
			t.Fatalf("没有新建项目 Alpha Imp: %+v", list)
		}
		if alpha.AllowedExec != nil || alpha.CaptureOut != nil || len(alpha.Repos) != 0 {
			t.Errorf("Alpha Imp 不应带 allowed_executors、repos 与对话捕获: %+v", alpha)
		}
		if names["acc-home"] != 1 {
			t.Errorf("绑定而不是新建：名为 acc-home 的项目应只有一个，得到 %d", names["acc-home"])
		}
		home := decode[proj](t, f.apa(f.OBS, 0, "project", "show", f.Projects["acc-home"]).Stdout)
		if home.Name != "acc-home" || home.Status != "active" {
			t.Errorf("已有项目 acc-home 被改动: %+v", home)
		}
		if got := f.show(t2).Item.Project; got != f.Projects["acc-home"] {
			t.Errorf("T-0002 应归在 acc-home(%s)，得到 %s", f.Projects["acc-home"], got)
		}
		created := f.audit("imported=" + batch + "&object=" + alpha.ID)
		if len(created) != 1 || created[0].Type != "project.updated" || !created[0].At.Equal(p.alphaAt) || !strings.Contains(string(created[0].Payload), `"create"`) {
			t.Errorf("Alpha Imp 的创建事件 = %+v，期望 at=%s", created, p.alphaAt)
		}
		if len(f.audit("object="+f.Projects["acc-home"]+"&imported="+batch)) != 0 {
			t.Errorf("绑定已有项目不写事件")
		}
	})

	f.step("ACT-IMP-06 原样重放新增 0 条", func(t *testing.T) {
		domainEvents := `type NOT IN ('auth.login')`
		items0, ev0 := f.tableCounts()["items"], f.eventCount(domainEvents)
		out := f.mustImport(src, "", plan)
		if out.Created != 0 || out.Existing != 4 || out.Invalid != 0 {
			t.Errorf("重放 = %+v", out)
		}
		for i, l := range out.Results {
			if l.Outcome != "existing" {
				t.Errorf("重放第 %d 行 = %s", i+1, l.Outcome)
			}
		}
		if got := f.tableCounts()["items"]; got != items0 {
			t.Errorf("事项数 %d -> %d", items0, got)
		}
		if got := f.eventCount(domainEvents); got != ev0+1 {
			t.Errorf("Event 数 %d -> %d，期望只多一条 import.completed", ev0, got)
		}
	})

	f.step("ACT-IMP-11 每次真实导入各有一条 import.completed 进对外事件流，没有导入事件", func(t *testing.T) {
		feed := decode[struct {
			Events []struct {
				Seq    int64  `json:"seq"`
				Type   string `json:"type"`
				Object string `json:"object"`
			} `json:"events"`
		}](t, f.api(f.OBS, "GET", fmt.Sprintf("/v1/events?cursor=%d&limit=500", seq0), 200))
		var done []string
		for _, e := range feed.Events {
			switch e.Type {
			case "item.created", "item.progress_added", "item.phase_changed", "project.updated":
				t.Errorf("对外事件流里不应有导入的事件: %+v", e)
			}
			if e.Type == "import.completed" {
				done = append(done, e.Object)
			}
		}
		if len(done) != 2 || done[0] != batch || done[1] == batch {
			t.Errorf("import.completed 应为两条（两次真实导入各一，预检不计），得到 %v", done)
		}
		raw := f.queryStrings(`SELECT payload FROM eventlog_event WHERE type = 'import.completed' AND object = ?`, batch)
		if len(raw) != 1 || !strings.Contains(raw[0], `"source":"acc-imp"`) || !strings.Contains(raw[0], `"created":3`) || !strings.Contains(raw[0], `"existing":1`) {
			t.Errorf("import.completed 载荷 = %v", raw)
		}
		if strings.Contains(f.api(f.OBS, "GET", fmt.Sprintf("/v1/events?cursor=%d&limit=500", seq0), 200), `"imported"`) {
			t.Errorf("订阅流里出现了带 imported 的事件")
		}
	})

	f.step("ACT-IMP-10 按 origin 查询：HTTP 与 CLI 一致", func(t *testing.T) {
		httpIDs := func(q string) []string {
			var ids []string
			for _, it := range decode[struct {
				Items []itemView `json:"items"`
			}](t, f.api(f.OBS, "GET", "/v1/items?"+q, 200)).Items {
				ids = append(ids, it.ID)
			}
			return ids
		}
		cliIDs := func(ref string) []string {
			var ids []string
			for _, it := range f.byOrigin(src, ref) {
				ids = append(ids, it.ID)
			}
			return ids
		}
		if all := httpIDs("origin_source=import:" + src); len(all) != 2 {
			t.Errorf("只给来源应返回 acc-imp 的全部事项（2 条）: %v", all)
		} else if !reflect.DeepEqual(all, cliIDs("")) {
			t.Errorf("CLI 与 HTTP 不一致: %v vs %v", cliIDs(""), all)
		}
		one := httpIDs("origin_source=import:" + src + "&origin_ref=T-0002")
		if len(one) != 1 || one[0] != t2 || !reflect.DeepEqual(one, cliIDs("T-0002")) {
			t.Errorf("加 ref 后应只有 T-0002(%s)：HTTP %v CLI %v", t2, one, cliIDs("T-0002"))
		}
	})

	f.step("ACT-IMP-07 不合法的记录逐行报 invalid，其余照常", func(t *testing.T) {
		now := f.now().In(cst)
		at := rfc(now.Add(-72 * time.Hour).Truncate(time.Second))
		day := now.Add(-72 * time.Hour).Format("2006-01-02")
		created := fmt.Sprintf(`{"at":%q,"event":"created","phase":"open"}`, at)
		item := func(id, body, hist string) string {
			return fmt.Sprintf(`{"type":"item","external_id":%q,"item":{%s},"history":[%s]}`, id, body, hist)
		}
		long := []string{created}
		for i := 0; i < 500; i++ {
			long = append(long, fmt.Sprintf(`{"at":%q,"event":"progress","text":"p","on":%q}`, at, day))
		}
		cases := []struct{ rule, line string }{
			{"history_first", item("b1", `"kind":"task","title":"x"`, fmt.Sprintf(`{"at":%q,"event":"progress","text":"p","on":%q}`, at, day))},
			{"history_future", item("b2", `"kind":"task","title":"x"`, fmt.Sprintf(`{"at":%q,"event":"created","phase":"open"}`, rfc(now.Add(time.Hour))))},
			{"progress_on_late", item("b3", `"kind":"task","title":"x"`, created+fmt.Sprintf(`,{"at":%q,"event":"progress","text":"p","on":%q}`, at, now.Add(24*time.Hour).Format("2006-01-02")))},
			{"kind_not_allowed", item("b4", `"kind":"delegation","title":"x"`, created)},
			{"kind_not_allowed", item("b5", `"kind":"brief","title":"x"`, created)},
			{"needs_input_not_allowed", item("b6", `"kind":"task","title":"x"`, fmt.Sprintf(`{"at":%q,"event":"created","phase":"needs_input"}`, at))},
			{"waiting_on_missing", item("b7", `"kind":"task","title":"x"`, fmt.Sprintf(`{"at":%q,"event":"created","phase":"waiting"}`, at))},
			{"reason_missing", item("b8", `"kind":"task","title":"x"`, created+fmt.Sprintf(`,{"at":%q,"event":"transition","to":"dropped"}`, at))},
			{"assignee_not_allowed", item("b9", `"kind":"task","title":"x","assignee":{"kind":"executor","ref":"work-host.cli"}`, created)},
			{"history_too_long", item("b10", `"kind":"task","title":"x"`, strings.Join(long, ","))},
			{"", item("ok1", `"kind":"task","title":"合法"`, created)},
		}
		var b strings.Builder
		for _, c := range cases {
			b.WriteString(c.line + "\n")
		}
		out := f.mustImport("acc-imp2", "", b.String())
		if len(out.Results) != 11 || out.Created != 1 || out.Invalid != 10 {
			t.Fatalf("结果 = %+v", out)
		}
		for i, c := range cases[:10] {
			if l := out.Results[i]; l.Outcome != "invalid" || l.Code != c.rule || l.Why == "" {
				t.Errorf("第 %d 行 = %+v，期望 invalid/%s", i+1, l, c.rule)
			}
		}
		if l := out.Results[10]; l.Outcome != "created" {
			t.Errorf("合法那行 = %+v", l)
		}
		if n := len(f.byOrigin("acc-imp2", "")); n != 1 {
			t.Errorf("acc-imp2 只应有 1 条事项（不合法行没有写入任何事件），得到 %d", n)
		}
		if n := len(f.audit("imported=" + out.Batch)); n != 1 {
			t.Errorf("该批次只应有合法那条的 1 个创建事件，得到 %d", n)
		}
		// 不合法的来源名：400，不写账本；CLI 为退出码 2。
		before := f.tableCounts()
		for _, bad := range []string{"Acc_Imp", strings.Repeat("a", 33)} {
			if r := f.importWeb(bad, "", f.simpleItems("x", 1), nil); r.Code != 400 {
				t.Errorf("来源名 %q: HTTP %d %s，期望 400", bad, r.Code, r.Body)
			}
			plan := t.TempDir() + "/plan.jsonl"
			writeFile(t, plan, f.simpleItems("x", 1))
			f.apa(f.DEL, 2, "import", "--file", plan, "--source", bad, "--grant", "grt_none")
		}
		if after := f.tableCounts(); !reflect.DeepEqual(before, after) {
			t.Errorf("不合法的来源名写了账本: %v", diffCounts(before, after))
		}
	})

	f.step("ACT-IMP-08 超限返回 413，多批沿用批次号，批次号不能跨来源名", func(t *testing.T) {
		before := f.tableCounts()
		if r := f.importWeb("acc-imp8", "", strings.Repeat("{}\n", 501), nil); r.Code != 413 {
			t.Errorf("501 行: HTTP %d，期望 413", r.Code)
		}
		if r := f.importWeb("acc-imp8", "", strings.Repeat("x", 9<<20), nil); r.Code != 413 {
			t.Errorf("9 MiB: HTTP %d，期望 413", r.Code)
		}
		if after := f.tableCounts(); !reflect.DeepEqual(before, after) {
			t.Errorf("被拒的请求写了账本: %v", diffCounts(before, after))
		}
		all := strings.Split(strings.TrimSuffix(f.simpleItems("big", 600), "\n"), "\n")
		first := f.mustImport("acc-imp8", "", strings.Join(all[:500], "\n"))
		second := f.mustImport("acc-imp8", "batch="+first.Batch, strings.Join(all[500:], "\n"))
		if second.Batch != first.Batch || first.Created != 500 || second.Created != 100 {
			t.Errorf("拆分后应属于同一批次：%+v %+v", first, second)
		}
		var got *batchView
		for _, b := range f.batches() {
			if b.ID == first.Batch {
				b := b
				got = &b
			}
		}
		if got == nil || got.Created != 600 || got.Source != "acc-imp8" {
			t.Errorf("GET /imports 该批次 = %+v，期望 created 600", got)
		}
		before = f.tableCounts()
		r := f.importWeb("acc-other8", "batch="+first.Batch, f.simpleItems("o", 1), nil)
		if r.Code < 400 || r.Code >= 500 || !strings.Contains(r.Body, "batch") {
			t.Errorf("另一来源名带同一批次号应被拒（4xx，why 写明批次不属于该来源名）: HTTP %d %s", r.Code, r.Body)
		}
		if after := f.tableCounts(); !reflect.DeepEqual(before, after) {
			t.Errorf("被拒的请求写了账本: %v", diffCounts(before, after))
		}
	})

	f.step("ACT-IMP-09 中途失败后重跑从断点继续", func(t *testing.T) {
		const n = 300
		body := f.tripleItems("crash", n)
		done := make(chan resp, 1)
		go func() {
			r, err := f.importHTTPErr("acc-crash", "", body, "", nil)
			if err != nil {
				r = resp{Code: -1, Body: err.Error()} // 内核被杀，连接断开
			}
			done <- r
		}()
		written := func() int {
			var c int
			_ = f.ledger().QueryRow(`SELECT COUNT(*) FROM item_import_map WHERE source = 'acc-crash'`).Scan(&c)
			return c
		}
		f.until("导入写到一半", func() bool { return written() >= 100 })
		if err := syscall.Kill(f.ServePID(), syscall.SIGKILL); err != nil { // 内核被强杀：测试目录里的故障注入
			t.Fatal(err)
		}
		select {
		case r := <-done:
			if r.Code == 200 {
				t.Fatalf("导入在被杀前已全部完成（%d 条），没有制造出中途失败", written())
			}
		case <-time.After(30 * time.Second):
			t.Fatal("被杀后请求没有返回")
		}
		f.StopKernel()
		f.StartKernelAgain()
		f.relogin()
		w := written()
		if w < 100 || w >= n {
			t.Fatalf("中途失败时已写入 %d 条，期望在 [100,%d) 内", w, n)
		}
		// 已写入的事项各自完整：每条都是 created + progress + transition 三个事件，没有只写了一半的。
		for _, id := range f.queryStrings(`SELECT object_id FROM item_import_map WHERE source = 'acc-crash' AND record_type = 'item'`) {
			if c := f.eventCount(`object = ? AND import_batch <> ''`, id); c != 3 {
				t.Errorf("事项 %s 有 %d 个导入事件，期望 3（半条记录）", id, c)
			}
		}
		out := f.mustImport("acc-crash", "", body)
		if out.Existing != w || out.Created != n-w || out.Invalid != 0 {
			t.Errorf("重跑 = %+v，期望 existing=%d created=%d", out, w, n-w)
		}
		if c := len(f.byOrigin("acc-crash", "")); c != n {
			t.Errorf("事项总数 %d，期望 %d", c, n)
		}
		for _, id := range f.queryStrings(`SELECT object_id FROM item_import_map WHERE source = 'acc-crash' AND record_type = 'item'`) {
			if c := f.eventCount(`object = ? AND import_batch <> ''`, id); c != 3 {
				t.Errorf("重跑后事项 %s 有 %d 个导入事件，期望 3", id, c)
			}
		}
	})
}

func phaseTargets(evs []auditEv) []string {
	var out []string
	for _, e := range evs {
		if e.Type != "item.phase_changed" {
			continue
		}
		var p struct {
			Item struct {
				Phase string `json:"phase"`
			} `json:"item"`
		}
		_ = json.Unmarshal(e.Payload, &p)
		out = append(out, p.Item.Phase)
	}
	return out
}

// diffCounts 列出两次行数统计不同的表。
func diffCounts(a, b map[string]int) map[string][2]int {
	out := map[string][2]int{}
	for k, v := range b {
		if a[k] != v {
			out[k] = [2]int{a[k], v}
		}
	}
	return out
}
