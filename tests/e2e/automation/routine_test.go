//go:build e2e

package automation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlowRoutine 按验收文档 flow-routine（场景十一）执行 ACT-RTN-01..12：
// 例行任务按日程只建事项或由任务型执行，结果写回事项，不依赖宿主的定时任务。
// 验收文档的"每分钟"周期与 2~5 分钟等待用测试时钟推进（tc_advance），到期判定仍由内核时钟完成。
func TestFlowRoutine(t *testing.T) {
	k := newKit(t)
	e := k.Env
	var h *accHarness
	home := e.Projects["acc-home"]

	// tick 让测试时钟走过一个周期（61 秒），之后等内核处理完到期。
	tick := func(minutes int) {
		for i := 0; i < minutes; i++ {
			e.TCAdvance(61)
			time.Sleep(1500 * time.Millisecond)
		}
	}
	routineItems := func() map[string]ledgerItem {
		m := map[string]ledgerItem{}
		for _, it := range k.items("routine") {
			m[it.ID] = it
		}
		return m
	}
	newInstance := func(params map[string]any) (id string, status int, body string) {
		b, _ := json.Marshal(map[string]any{"pack": "routine", "project": home, "params": params})
		r := k.Web("POST", "/v1/instances", string(b))
		var out struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(r.Body), &out)
		return out.ID, r.Code, r.Body
	}
	runsOf := func(item string) int {
		var out struct {
			Runs []json.RawMessage `json:"runs"`
		}
		jsonOf(t, k.apa(e.OBS, 0, "run", "list", "--target", item).Stdout, &out)
		return len(out.Runs)
	}
	waitNew := func(before map[string]ledgerItem, what string, pred func(ledgerItem) bool) ledgerItem {
		t.Helper()
		var found ledgerItem
		if !k.WaitFor(45*time.Second, func() bool {
			for id, it := range routineItems() {
				if _, old := before[id]; !old && pred(it) {
					found = it
					return true
				}
			}
			return false
		}) {
			t.Fatalf("%s：45 秒内没有出现新的 routine 事项\n%s", what, e.ServiceLog())
		}
		return found
	}
	disable := func(id string) {
		t.Helper()
		k.apa(e.DEL, 0, "instance", "disable", id)
	}
	var instances []string

	k.step("ACT-RTN-01 准备日常执行者与替身", func(t *testing.T) {
		h = k.declareHarness("acc-stub-rtn", "routine")
		k.webOK("PUT", "/v1/settings/policy", `{"default_executor":"acc-stub-rtn","main_executor":"acc-stub-rtn"}`, 200)
		k.webOK("PUT", "/v1/settings/agents/acc-stub-rtn/card", `{"groups":{"view":{},"organize":{},"source_events":{}},"tool":"read"}`, 200)
		if p := packList(t, e); p["routine"].Name == "" {
			t.Fatalf("pack list 应含内置 routine 包: %v", p)
		}
		if pol := k.webOK("GET", "/v1/settings/policy", "", 200) + k.webOK("GET", "/v1/settings/access-limits", "", 200); !contains(strings.ReplaceAll(pol, " ", ""), `"default_executor":"acc-stub-rtn"`, `"acc-stub-rtn":{"groups"`) {
			t.Errorf("Web 设置应显示日常执行者为 acc-stub-rtn、权限卡上限 read: %s", pol)
		}
		k.markModelBaseline()
	})

	var ci string // create_item 实例
	k.step("ACT-RTN-02 create_item 模式到点只建一条 open 事项", func(t *testing.T) {
		k.alignClock()
		before := routineItems()
		var status int
		var body string
		ci, status, body = newInstance(map[string]any{"schedule": "* * * * *", "mode": "create_item", "title": "acc 例行提醒 {date}"})
		if status != 201 {
			t.Fatalf("建 create_item 实例: %d %s", status, body)
		}
		instances = append(instances, ci)
		tick(2)
		now := routineItems()
		if len(now)-len(before) != 2 {
			t.Fatalf("两个周期应各建一条 routine 事项，实际新增 %d: %+v", len(now)-len(before), now)
		}
		for id, it := range now {
			if it.Phase != "open" || it.Project != home || !strings.HasPrefix(it.Title, "acc 例行提醒 ") || strings.Contains(it.Title, "{date}") || !strings.Contains(it.Title, "-") {
				t.Errorf("事项应为 open、项目 acc-home、标题带日期: %+v", it)
			}
			if n := runsOf(id); n != 0 {
				t.Errorf("create_item 不应为事项 %s 创建 Run，实际 %d", id, n)
			}
		}
		k.noModelCallsBeyondItemSummaries("ACT-RTN-02", "routine")
	})

	k.step("ACT-RTN-03 create_item 模式不需要宿主", func(t *testing.T) {
		k.webOK("PUT", "/v1/settings/policy", `{"default_executor":"","main_executor":""}`, 200)
		before := routineItems()
		tick(2)
		if n := len(routineItems()) - len(before); n != 2 {
			t.Errorf("没有任何 harness 时仍应按周期建事项，新增 %d", n)
		}
		if hl := k.apa(e.OBS, 0, "health").Stdout; strings.Contains(strings.ToLower(hl), "harness") {
			t.Errorf("健康视图不应因缺 harness 产生告警: %s", hl)
		}
		k.webOK("PUT", "/v1/settings/policy", `{"default_executor":"acc-stub-rtn","main_executor":"acc-stub-rtn"}`, 200)
	})

	k.step("ACT-RTN-04 同一周期只生成一份，停机后只补最近一次", func(t *testing.T) {
		before := len(routineItems())
		tick(3)
		if n := len(routineItems()) - before; n != 3 {
			t.Errorf("3 个周期应恰好 3 条（每周期至多一条），实际 %d", n)
		}
		before = len(routineItems())
		k.StopKernel()
		e.TCAdvance(5 * 61) // 停机 5 个周期
		k.StartKernelAgain()
		time.Sleep(5 * time.Second)
		if n := len(routineItems()) - before; n != 1 {
			t.Errorf("停机期间错过的周期只补最近一次，实际新增 %d", n)
		}
	})

	k.step("ACT-RTN-05 停用实例即停止", func(t *testing.T) {
		disable(ci)
		before := len(routineItems())
		tick(3)
		if n := len(routineItems()) - before; n != 0 {
			t.Errorf("停用后不应再建 routine 事项，实际新增 %d", n)
		}
		if l := k.apa(e.OBS, 0, "instance", "list").Stdout; !contains(l, ci) || len(stillEnabled(t, l, ci)) > 0 {
			t.Errorf("instance list 应显示该实例已停用: %s", l)
		}
	})

	k.step("ACT-RTN-06 实例参数不合法时拒绝", func(t *testing.T) {
		before := k.apa(e.OBS, 0, "instance", "list").Stdout
		r := e.Apa(e.DEL, "", "instance", "add", "routine", "--param", "schedule=* * * * *", "--param", "mode=sometimes", "--param", "title=x")
		if r.Code == 0 || !strings.Contains(r.Stderr, "mode") {
			t.Errorf("mode 不合法应被拒绝并指出 mode: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		r = e.Apa(e.DEL, "", "instance", "add", "routine", "--param", "mode=create_item", "--param", "title=x")
		if r.Code == 0 || !strings.Contains(r.Stderr, "schedule") {
			t.Errorf("缺 schedule 应被拒绝并指出 schedule: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		if after := k.apa(e.OBS, 0, "instance", "list").Stdout; after != before {
			t.Errorf("被拒绝后不应有新实例:\n%s\n→\n%s", before, after)
		}
	})

	var run string // ACT-RTN-07 的运行 id
	var rtnItem ledgerItem
	var rtnInst string
	k.step("ACT-RTN-07 run 模式按任务说明运行，说明经文件给出", func(t *testing.T) {
		before := routineItems()
		var status int
		var body string
		rtnInst, status, body = newInstance(map[string]any{"schedule": "* * * * *", "mode": "run", "title": "acc 周报 {date}",
			"instruction": "按本周完成的事项和进展写一份周报草稿 ACC-INSTR-7", "access": "read", "output": "item_and_material"})
		if status != 201 {
			t.Fatalf("建 run 实例: %d %s", status, body)
		}
		instances = append(instances, rtnInst)
		tick(1)
		rtnItem = waitNew(before, "run 模式到期", func(it ledgerItem) bool { return strings.HasPrefix(it.Title, "acc 周报 ") })
		if !k.WaitFor(45*time.Second, func() bool { return len(h.RunRecords()) == 1 }) {
			t.Fatalf("应起一个任务型 Run（harness acc-stub-rtn），实际 %v\n%s", h.RunRecords(), e.ServiceLog())
		}
		run = h.RunRecords()[0]
		prompt := h.File(run, "prompt.txt")
		if !contains(prompt, "完成一次例行任务", rtnItem.ID) || strings.Contains(prompt, "ACC-INSTR-7") {
			t.Errorf("提示词应是 run-routine 固定模板且不含任务说明正文: %s", prompt)
		}
		if !strings.Contains(prompt, filepath.Join(e.Home, "runs", run, "instruction.txt")) {
			t.Errorf("提示词应含 instruction_path 指向运行目录内的文件: %s", prompt)
		}
		if got := h.File(run, "instruction.txt"); !strings.Contains(got, "ACC-INSTR-7") {
			t.Errorf("instruction.txt 应含任务说明: %q", got)
		}
		if _, err := os.Stat(filepath.Join(e.Home, "runs", run, "instruction.txt")); err != nil {
			t.Errorf("任务说明文件应在这次运行的运行目录内: %v", err)
		}
		mat := h.File(run, "materials.json")
		if !contains(mat, "ledger_snapshot", "instance_params", "ACC-INSTR-7") {
			t.Errorf("资料文件应带账本快照与实例参数: %s", mat)
		}
		if w := k.why(rtnItem.ID); !strings.Contains(w, run) {
			t.Errorf("apa why 应能追到这次 Run %s: %s", run, w)
		}
	})

	k.step("ACT-RTN-08 run 模式写回结果后事项转 done", func(t *testing.T) {
		var it ledgerItem
		var raw string
		if !k.WaitFor(45*time.Second, func() bool { it, raw = k.show(rtnItem.ID); return it.Phase == "done" }) {
			t.Fatalf("事项应在写回结果后转 done: %s", raw)
		}
		if !strings.Contains(string(it.Spec), "周报草稿 "+run) {
			t.Errorf("结果字段应已写回: %s", it.Spec)
		}
		var w struct {
			Steps []struct {
				Seq   int    `json:"seq"`
				Type  string `json:"type"`
				Actor struct {
					Type      string `json:"type"`
					Principal string `json:"principal"`
				} `json:"actor"`
			} `json:"steps"`
		}
		jsonOf(t, k.why(rtnItem.ID), &w)
		wrote, doneBy := 0, ""
		for _, st := range w.Steps {
			if st.Type == "item.updated" && st.Actor.Type == "agent" {
				wrote = st.Seq
			}
			if st.Type == "item.phase_changed" && wrote != 0 && st.Seq > wrote {
				doneBy = st.Actor.Principal
			}
		}
		if wrote == 0 || doneBy != "kernel:agents-task" {
			t.Errorf("apa why 应显示运行者写回结果后由内核转 done（写回 seq=%d，转 done 的主体 %q）: %+v", wrote, doneBy, w.Steps)
		}
		var found bool
		k.WaitFor(30*time.Second, func() bool {
			found = strings.Contains(k.apa(e.OBS, 0, "material", "list", "--project", home).Stdout, "例行任务纪要 "+run)
			return found
		})
		if !found {
			t.Errorf("acc-home 下应多一条纪要型材料: %s", k.apa(e.OBS, 0, "material", "list", "--project", home).Stdout)
		}
	})

	// 2026-11-26 改写（权限卡）：实例的 access 已取消，工具档位取权限卡（这里是只读）；实例写了 edit 也按只读运行。
	k.step("ACT-RTN-09 run 模式的工具档位取权限卡", func(t *testing.T) {
		disable(rtnInst)
		runsBefore := len(h.RunRecords())
		id, status, body := newInstance(map[string]any{"schedule": "* * * * *", "mode": "run", "title": "acc 越权 {date}",
			"instruction": "越权", "access": "edit", "output": "item"})
		if status != 201 {
			t.Fatalf("建立实例: HTTP %d %s", status, body)
		}
		instances = append(instances, id)
		tick(1)
		if !k.WaitFor(45*time.Second, func() bool { return len(h.RunRecords()) > runsBefore }) {
			t.Fatal("到期后应拉起运行")
		}
		for _, r := range h.RunRecords()[runsBefore:] {
			if a := h.File(r, "args.txt"); !strings.Contains(a, "--access=read") || strings.Contains(a, "--access=edit") || strings.Contains(a, "--access=full") {
				t.Errorf("应按权限卡的只读运行: %s", a)
			}
		}
		disable(id)
	})

	// 以下两步复用一个新的 run 实例：每次到期换一次替身模式。
	var failInst string
	k.step("ACT-RTN-10 run 模式运行失败只记事件、不转 needs_input、不重试", func(t *testing.T) {
		var status int
		var body string
		failInst, status, body = newInstance(map[string]any{"schedule": "* * * * *", "mode": "run", "title": "acc 失败 {date}",
			"instruction": "失败", "access": "read", "output": "item"})
		if status != 201 {
			t.Fatalf("建 run 实例: %d %s", status, body)
		}
		instances = append(instances, failInst)
		h.Mode("exit1")
		before := routineItems()
		tick(1)
		it := waitNew(before, "exit1 到期", func(it ledgerItem) bool { return strings.HasPrefix(it.Title, "acc 失败 ") })
		// 2026-11-20 改写：运行故障是事件不是问题——事项不转 needs_input、不挂问题，运行记录带类别码 exit_nonzero。
		var runs string
		if !k.WaitFor(45*time.Second, func() bool {
			runs = k.apa(e.OBS, 0, "run", "list", "--target", it.ID).Stdout
			return strings.Contains(runs, `"state":"failed"`)
		}) {
			t.Fatalf("运行应以失败结束: %s", runs)
		}
		if !contains(runs, `"code":"exit_nonzero"`) {
			t.Errorf("失败的运行应带类别码: %s", runs)
		}
		cur, raw := k.show(it.ID)
		t.Logf("运行失败后的事项: %s", raw)
		if cur.Phase != "open" || strings.Contains(raw, `"ask"`) {
			t.Errorf("运行故障不应把事项转 needs_input 或挂问题: %s", raw)
		}
		time.Sleep(5 * time.Second)
		if n := runsOf(it.ID); n != 1 {
			t.Errorf("该事项应只有一个 Run、没有重试，实际 %d", n)
		}
	})

	k.step("ACT-RTN-11 run 模式没有写全结果时转 needs_input", func(t *testing.T) {
		h.Mode("noop")
		before := routineItems()
		tick(1)
		it := waitNew(before, "noop 到期", func(it ledgerItem) bool { return strings.HasPrefix(it.Title, "acc 失败 ") })
		var cur ledgerItem
		var raw string
		if !k.WaitFor(45*time.Second, func() bool { cur, raw = k.show(it.ID); return cur.Phase == "needs_input" }) {
			t.Fatalf("没有写全结果应转 needs_input，不得 done: %s", raw)
		}
		t.Logf("没写全结果后的事项: %s", raw)
		if !contains(raw, "任务型运行未完成", "没写全", "spec.result") {
			t.Errorf("原因应写明声明的结果字段没有写全: %s", raw)
		}
	})

	k.step("ACT-RTN-12 记录 SPIKE-ROUTINE 开发验收结论并清理", func(t *testing.T) {
		// 结论：create_item 零模型调用（ACT-RTN-02 已断言）、instruction_path 文件写入且可读（ACT-RTN-07 已断言）；
		// run 模式在真实宿主里可读由本机升级验证 L12 承接。
		for _, id := range instances {
			e.Apa(e.DEL, "", "instance", "disable", id) // 已停用的再停用不算错
		}
		h.Mode("routine")
		if l := k.apa(e.OBS, 0, "instance", "list").Stdout; len(stillEnabled(t, l, instances...)) > 0 {
			t.Errorf("本流程的实例都应已停用: %s", l)
		}
	})
}

// stillEnabled 返回 ids 里在 apa instance list 输出中仍启用的；开箱默认安排（§7.3）不属于任何流程，不在其中。
func stillEnabled(t *testing.T, list string, ids ...string) (out []string) {
	t.Helper()
	var l struct {
		Instances []struct {
			ID      string `json:"id"`
			Enabled bool   `json:"enabled"`
		} `json:"instances"`
	}
	if err := json.Unmarshal([]byte(list), &l); err != nil {
		t.Fatalf("instance list: %v: %s", err, list)
	}
	for _, in := range l.Instances {
		for _, id := range ids {
			if in.ID == id && in.Enabled {
				out = append(out, id)
			}
		}
	}
	return out
}
