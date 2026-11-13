//go:build e2e

package workbench

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// 日期按本机时区取（验收文档 ACT-WBN-01）。
func day(offset int) string { return time.Now().AddDate(0, 0, offset).Format("2006-01-02") }

// newKit 建好 flow-env-baseline 的替身环境，并让两个模型端点替身在整个工作台流程里"从未被请求"。
func newKit(t *testing.T) *kit {
	t.Helper()
	e := baseline.Setup(t)
	e.WireEndpoints()
	return &kit{Env: e, t: t}
}

// TestFlowWebWorkbench 走完 flow-web-workbench（场景十二）：不装宿主、不配模型，本人在 Web 和 CLI 上记事、安排、推进和回顾。
func TestFlowWebWorkbench(t *testing.T) {
	k := newKit(t)
	var wbA, wbB, wbC, wbD, wbE, wbF, wbG, wbNew, wbF2 string
	TODAY, YDAY, TMRW := day(0), day(-1), day(1)

	k.step("ACT-WBN-01 准备工作台用例数据", func(t *testing.T) {
		wbA = k.webItem(map[string]any{"kind": "task", "title": "WB-A", "planned_for": YDAY})
		wbB = k.webItem(map[string]any{"kind": "task", "title": "WB-B", "planned_for": TODAY})
		wbC = k.webItem(map[string]any{"kind": "task", "title": "WB-C", "planned_for": TMRW})
		wbD = k.webItem(map[string]any{"kind": "task", "title": "WB-D",
			"deadline": time.Now().Format("2006-01-02") + "T23:00:00" + time.Now().Format("-07:00")})
		wbE = k.webItem(map[string]any{"kind": "task", "title": "WB-E", "planned_for": TODAY})
		k.webDone(wbE)
		var out struct {
			ID string `json:"id"`
		}
		jsonOf(t, k.apa(k.WRK, 0, "item", "add", "--kind", "task", "--title", "WB-F", "--assignee", "work-host.cli", "--project", k.Projects["acc-work"]).Stdout, &out)
		wbF = out.ID
		wbG = k.AskDecision("WB-G")

		eq(t, "WB-A planned_for", k.planned(wbA), YDAY)
		eq(t, "WB-B planned_for", k.planned(wbB), TODAY)
		eq(t, "WB-C planned_for", k.planned(wbC), TMRW)
		if k.item(wbD)["deadline"] == nil {
			t.Error("WB-D 没有 deadline")
		}
		eq(t, "WB-E phase", k.phase(wbE), "done")
		eq(t, "WB-A phase", k.phase(wbA), "open")
		eq(t, "WB-G phase", k.phase(wbG), "needs_input")
	})

	k.step("ACT-WBN-02 本人在 Web 新建事项，直接进 open 并可同时设项目、截止与计划日期", func(t *testing.T) {
		body := "## 行程\n- 周五出发\n[航司](https://example.com)\n<script>window.__wbn=1</script>\n![外图](https://example.org/a.png)"
		deadline := time.Now().AddDate(0, 0, 2).Format("2006-01-02") + "T18:00:00" + time.Now().Format("-07:00")
		wbNew = k.webItem(map[string]any{"kind": "task", "title": "WB-NEW 订机票", "project": k.Projects["acc-home"],
			"deadline": deadline, "planned_for": TMRW, "body": body})
		it := k.item(wbNew)
		eq(t, "phase", it["phase"], "open")
		eq(t, "project", it["project"], k.Projects["acc-home"])
		eq(t, "planned_for", it["planned_for"], TMRW)
		if it["deadline"] == nil {
			t.Error("deadline 没有写入")
		}
		evs := k.events(wbNew, "item.created")
		if len(evs) != 1 {
			t.Fatalf("item.created 事件数 = %d，期望 1", len(evs))
		}
		byOwner(t, "item.created", evs[0])
	})

	k.step("ACT-WBN-03 事项详情按 Markdown 渲染并净化正文（ego-browser）", func(t *testing.T) {
		needBrowser(t)
		defer k.closeBrowser("gt315-wbn-md")
		r := k.browse("gt315-wbn-md", fmt.Sprintf(`
await login();
await page.goto(base + "/items/" + %q);
await page.waitForSelector(".markdown", { state: "visible" });
const probe = () => page.evaluate(() => ({
  h2: [...document.querySelectorAll(".markdown h2")].map((e) => e.textContent),
  li: [...document.querySelectorAll(".markdown li")].map((e) => e.textContent),
  links: [...document.querySelectorAll(".markdown a")].map((a) => ({ t: a.textContent, href: a.getAttribute("href"), rel: a.rel })),
  scripts: document.querySelectorAll(".markdown script").length,
  wbn: typeof window.__wbn,
  imgs: document.querySelectorAll(".markdown img").length,
  ext: performance.getEntriesByType("resource").filter((r) => r.name.includes("example.org")).length,
  literal: document.querySelector(".markdown").textContent.includes("<script>"),
}));
out.before = await probe();
await page.click("loc=role:button[name*='外图']");
await page.waitForSelector(".markdown img", { state: "attached" });
out.after = await probe();
out.imgSrc = await page.evaluate(() => document.querySelector(".markdown img").getAttribute("src"));
`, wbNew))
		before, after := r["before"].(map[string]any), r["after"].(map[string]any)
		eq(t, "二级标题", fmt.Sprint(before["h2"]), "[行程]")
		eq(t, "列表项", fmt.Sprint(before["li"]), "[周五出发]")
		links, _ := before["links"].([]any)
		if len(links) != 1 {
			t.Fatalf("链接数 = %d，期望 1", len(links))
		}
		l := links[0].(map[string]any)
		eq(t, "链接文字", l["t"], "航司")
		eq(t, "链接地址", l["href"], "https://example.com")
		if !strings.Contains(fmt.Sprint(l["rel"]), "noopener") {
			t.Errorf("外链没有 noopener: %v", l["rel"])
		}
		eq(t, "正文里的 <script> 元素数", before["scripts"], float64(0))
		eq(t, "window.__wbn", before["wbn"], "undefined")
		eq(t, "<script> 作为文本显示", before["literal"], true)
		eq(t, "打开详情时的图片数", before["imgs"], float64(0))
		eq(t, "点开前发往 example.org 的资源请求", before["ext"], float64(0))
		eq(t, "点开后图片数", after["imgs"], float64(1))
		eq(t, "点开后图片地址", r["imgSrc"], "https://example.org/a.png")
		eq(t, "点开后脚本仍未执行", after["wbn"], "undefined")
	})

	k.step("ACT-WBN-04 在 Web 编辑标题与正文", func(t *testing.T) {
		before := k.item(wbNew)
		ver := before["version"].(float64)
		body := before["body"].(string) + "\n- 周日返回"
		b, _ := json.Marshal(map[string]any{"title": "WB-NEW 订往返机票", "body": body})
		r := k.Web("PATCH", fmt.Sprintf("/v1/items/%s", wbNew), string(b))
		if r.Code != 200 {
			t.Fatalf("编辑被拒 HTTP %d: %s", r.Code, r.Body)
		}
		after := k.item(wbNew)
		eq(t, "title", after["title"], "WB-NEW 订往返机票")
		if !strings.HasSuffix(after["body"].(string), "- 周日返回") {
			t.Errorf("body 末尾没有追加: %q", after["body"])
		}
		eq(t, "version", after["version"], ver+1)
		byOwner(t, "item.updated", k.lastEvent(wbNew, "item.updated"))
	})

	k.step("ACT-WBN-05 CLI 读写计划日期，计划日期不产生定时与提醒", func(t *testing.T) {
		k.apa(k.WRK, 0, "item", "update", wbF, "--planned-for", TMRW)
		eq(t, "planned_for", k.planned(wbF), TMRW)
		why := k.apa(k.OBS, 0, "why", wbF).Stdout
		if strings.Contains(strings.ToLower(why), "timer") && !strings.Contains(why, `"timers":[]`) && !strings.Contains(why, `"timers":null`) {
			// why 的 Timer 列表不为空才算"因计划日期建了 Timer"
			var w struct {
				Timers []any `json:"timers"`
			}
			if json.Unmarshal([]byte(why), &w) == nil && len(w.Timers) > 0 {
				t.Errorf("计划日期创建了 Timer: %s", why)
			}
		}
		r := k.APIRaw("PATCH", "/v1/items/"+wbF, `{"planned_for": null}`, k.WRK)
		if r.Code != 200 {
			t.Fatalf("PATCH null: HTTP %d %s", r.Code, r.Body)
		}
		if v := k.planned(wbF); v != nil {
			t.Errorf("清除后 GET 仍返回 planned_for=%v", v)
		}
		var out struct {
			ID string `json:"id"`
		}
		jsonOf(t, k.apa(k.WRK, 0, "item", "add", "--kind", "task", "--title", "WB-F2", "--assignee", "work-host.cli", "--planned-for", TMRW).Stdout, &out)
		wbF2 = out.ID
		eq(t, "WB-F2 planned_for", k.planned(wbF2), TMRW)
	})

	k.step("ACT-WBN-06 非法计划日期被拒绝", func(t *testing.T) {
		ver := k.version(wbF)
		for _, bad := range []string{"2026-13-40", "2026-11-12T10:00:00Z"} {
			r := k.apa(k.WRK, exitUsage, "item", "update", wbF, "--planned-for", bad)
			if !strings.Contains(r.Stdout+r.Stderr, "YYYY-MM-DD") {
				t.Errorf("%s 的错误没有说明只接受 YYYY-MM-DD: %s%s", bad, r.Stdout, r.Stderr)
			}
		}
		eq(t, "version 不变", k.version(wbF), ver)
		if v := k.planned(wbF); v != nil {
			t.Errorf("非法值被写入: %v", v)
		}
	})

	k.step("ACT-WBN-07 \"今天\"清单四组取材正确（CLI 与 HTTP）", func(t *testing.T) {
		td := k.today()
		if !has(td.ids("planned"), wbA) || !has(td.ids("planned"), wbB) {
			t.Errorf("planned 组缺 WB-A/WB-B: %v", td.ids("planned"))
		}
		if has(td.ids("planned"), wbC) || has(td.ids("planned"), wbE) {
			t.Errorf("planned 组混入 WB-C/WB-E: %v", td.ids("planned"))
		}
		if !has(td.ids("due"), wbD) {
			t.Errorf("due 组缺 WB-D: %v", td.ids("due"))
		}
		if !has(td.ids("pending"), wbG) {
			t.Errorf("pending 组缺 WB-G: %v", td.ids("pending"))
		}
		eq(t, "WB-A 不自动改期", k.planned(wbA), YDAY)
	})

	k.step("ACT-WBN-08 Web\"今天\"视图四组与操作", func(t *testing.T) {
		cli, web := k.today(), k.webToday()
		for _, g := range []string{"planned", "due", "pending", "stale"} {
			eq(t, "Web 与 CLI 的 "+g+" 组", strings.Join(sortedCopy(web.ids(g)), ","), strings.Join(sortedCopy(cli.ids(g)), ","))
		}
		// "改计划日期" / "写进展" / "完成" 调的就是这三个接口。
		k.webOK("PATCH", "/items/"+wbA, fmt.Sprintf(`{"planned_for":%q}`, TMRW), 200)
		k.webOK("POST", "/items/"+wbB+"/progress", `{"text":"已订酒店"}`, 201)
		k.webDone(wbB)

		if has(k.webToday().ids("planned"), wbA) {
			t.Error("改期后 WB-A 仍在 planned 组")
		}
		eq(t, "WB-A planned_for", k.planned(wbA), TMRW)
		pr := k.progress(wbB)
		if len(pr) != 1 || pr[0]["text"] != "已订酒店" {
			t.Errorf("WB-B 进展不符: %v", pr)
		}
		eq(t, "WB-B phase", k.phase(wbB), "done")
		byOwner(t, "改期 item.updated", k.lastEvent(wbA, "item.updated"))
		byOwner(t, "进展 item.progress_added", k.lastEvent(wbB, "item.progress_added"))
		byOwner(t, "完成 item.phase_changed", k.lastEvent(wbB, "item.phase_changed"))
	})

	k.step("ACT-WBN-09 CLI 追加进展，不改阶段，记 item.progress_added", func(t *testing.T) {
		phase0 := k.phase(wbF)
		before := len(k.events(wbF, "item.progress_added"))
		k.apa(k.WRK, 0, "item", "progress", wbF, "--text", "联系了供应商", "--on", YDAY)
		eq(t, "phase", k.phase(wbF), phase0)
		pr := k.progress(wbF)
		if len(pr) == 0 {
			t.Fatal("progress[] 为空")
		}
		last := pr[len(pr)-1]
		eq(t, "text", last["text"], "联系了供应商")
		eq(t, "on", last["on"], YDAY)
		if last["at"] == nil || last["actor"] == nil || last["actor"] == "" {
			t.Errorf("进展缺记录时刻或操作者: %v", last)
		}
		// HTTP 路径返回同样的 progress[]
		var got struct {
			Progress []map[string]any `json:"progress"`
		}
		jsonOf(t, k.APIRaw("GET", "/v1/items/"+wbF, "", k.OBS).Body, &got)
		if n := len(got.Progress); n == 0 || got.Progress[n-1]["text"] != "联系了供应商" {
			t.Errorf("GET /items/{id} 的 progress[] 末尾不是这条进展: %v", got.Progress)
		}
		if n := len(k.events(wbF, "item.progress_added")); n != before+1 {
			t.Errorf("item.progress_added 事件数 %d，期望 %d", n, before+1)
		}
		// 事件流（apa events）里也看得到
		ev := k.apa(k.OBS, 0, "events", "--limit", "500").Stdout
		if !strings.Contains(ev, "item.progress_added") {
			t.Error("apa events 没有 item.progress_added")
		}
	})

	k.step("ACT-WBN-10 进展的所指日期不能晚于今天", func(t *testing.T) {
		n := len(k.progress(wbF))
		k.apa(k.WRK, exitUsage, "item", "progress", wbF, "--text", "未来", "--on", TMRW)
		r := k.APIRaw("POST", "/v1/items/"+wbF+"/progress", fmt.Sprintf(`{"text":"未来","on":%q}`, TMRW), k.WRK)
		eq(t, "HTTP", r.Code, 400)
		eq(t, "progress 条数", len(k.progress(wbF)), n)
	})

	k.step("ACT-WBN-11 进展的权限边界", func(t *testing.T) {
		n, nb := len(k.progress(wbF)), len(k.progress(wbB))
		k.apa(k.WRK2, exitDenied, "item", "progress", wbF, "--text", "x")
		r := k.apa(k.DEL, exitBasis, "item", "progress", wbB, "--text", "x")
		if !strings.Contains(r.Stdout+r.Stderr, "evidence") && !strings.Contains(r.Stdout+r.Stderr, "依据") {
			t.Errorf("第二条没有说明缺依据: %s%s", r.Stdout, r.Stderr)
		}
		eq(t, "WB-F 进展不变", len(k.progress(wbF)), n)
		eq(t, "WB-B 进展不变", len(k.progress(wbB)), nb)
		k.apa(k.DEL, 0, "item", "progress", wbB, "--text", "本人说已订", "--evidence", "已订好了")
		eq(t, "WB-B 进展落账", len(k.progress(wbB)), nb+1)
		// 缺依据（退出码 23）的拒绝由命令管线记 command.rejected；越权（退出码 4）在授权层被挡，不落账。
		basis := false
		for _, e := range k.events(wbB, "") {
			if e.Type == "command.rejected" && e.Payload["code"] == "basis_required" {
				basis = true
			}
		}
		if !basis {
			t.Error("审计页没有 WB-B 缺依据的 command.rejected")
		}
	})

	k.step("ACT-WBN-12 全文搜索覆盖标题、摘要、正文与进展", func(t *testing.T) {
		k.webOK("PATCH", "/items/"+wbC, `{"body":"护照续签"}`, 200)
		k.webOK("POST", "/items/"+wbD+"/progress", `{"text":"等护照照片"}`, 201)
		vC, vD := k.version(wbC), k.version(wbD)
		var cli, web, http struct {
			Items []map[string]any `json:"items"`
		}
		jsonOf(t, k.apa(k.OBS, 0, "item", "list", "--q", "护照").Stdout, &cli)
		jsonOf(t, k.APIRaw("GET", "/v1/items?q=%E6%8A%A4%E7%85%A7", "", k.OBS).Body, &http)
		jsonOf(t, k.webOK("GET", "/items?q=%E6%8A%A4%E7%85%A7", "", 200), &web)
		want := sortedCopy([]string{wbC, wbD})
		for name, l := range map[string][]map[string]any{"CLI": cli.Items, "HTTP": http.Items, "Web": web.Items} {
			var ids []string
			for _, it := range l {
				ids = append(ids, it["id"].(string))
			}
			eq(t, name+" 搜索结果", strings.Join(sortedCopy(ids), ","), strings.Join(want, ","))
		}
		eq(t, "WB-C version 不变", k.version(wbC), vC)
		eq(t, "WB-D version 不变", k.version(wbD), vD)
	})

	k.step("ACT-WBN-13 列表按截止、计划日期、优先级、更新时间排序，按计划日期筛选", func(t *testing.T) {
		type list struct {
			Items []map[string]any `json:"items"`
		}
		order := func(l list) string {
			var ids []string
			for _, it := range l.Items {
				ids = append(ids, it["id"].(string))
			}
			return strings.Join(ids, ",")
		}
		for _, sort := range []string{"deadline", "planned_for", "priority", "updated"} {
			var cli, http, web list
			jsonOf(t, k.apa(k.OBS, 0, "item", "list", "--sort", sort).Stdout, &cli)
			jsonOf(t, k.APIRaw("GET", "/v1/items?sort="+sort, "", k.OBS).Body, &http)
			jsonOf(t, k.webOK("GET", "/items?sort="+sort, "", 200), &web)
			eq(t, "排序 "+sort+" CLI 与 HTTP", order(cli), order(http))
			eq(t, "排序 "+sort+" Web 与 HTTP", order(web), order(http))
			switch sort {
			case "planned_for", "deadline":
				seenUnset := false
				prev := ""
				for _, it := range cli.Items {
					v, _ := it[sort].(string)
					if v == "" {
						seenUnset = true
						continue
					}
					if seenUnset {
						t.Errorf("排序 %s：未设该字段的事项没有排在末尾", sort)
					}
					if v < prev {
						t.Errorf("排序 %s 不单调: %s 在 %s 之后", sort, v, prev)
					}
					prev = v
				}
			case "updated":
				prev := ""
				for _, it := range cli.Items {
					v, _ := it["updated_at"].(string)
					if prev != "" && v > prev {
						t.Errorf("排序 updated 不单调（应从新到旧）: %s 在 %s 之后", v, prev)
					}
					prev = v
				}
			}
		}
		var f list
		jsonOf(t, k.apa(k.OBS, 0, "item", "list", "--planned-for", TMRW).Stdout, &f)
		var got []string
		for _, it := range f.Items {
			eq(t, "筛选结果的 planned_for", it["planned_for"], TMRW)
			got = append(got, it["id"].(string))
		}
		for _, id := range []string{wbA, wbC, wbNew, wbF2} {
			if !has(got, id) {
				t.Errorf("--planned-for %s 漏了 %s: %v", TMRW, id, got)
			}
		}
		eq(t, "筛选条数", len(got), 4)
	})

	k.step("ACT-WBN-14 v0.1 的列表参数与输出不变", func(t *testing.T) {
		k.v01CompatList(t)
	})

	var proposals []SrcEvent
	var clsLines, modelLines int
	k.step("ACT-WBN-15 为手机走查准备建议并停掉全部替身", func(t *testing.T) {
		// 验收文档写了 source_rule then: proposed，但规则直接成为建议不调模型、没有推荐（flow-run-tiers 的 ACT-TIER-04 正依赖这点）；
		// 要带推荐就得走默认的初筛与起草（内置 inbound-triage 包），所以这里不写规则。
		k.Cls.SetDefaultPYes(0.99)
		src := k.registerSource("src_wb", "acc-wb")
		var evs []map[string]any
		for i := 1; i <= 12; i++ {
			subject := fmt.Sprintf("WB-P%02d", i)
			d := map[string]any{"title": "起草：" + subject, "summary": "摘要 " + subject, "kind": "task", "reason": "一句话理由"}
			switch {
			case i == 1:
				d["recommend"], d["attach_to"] = "attach", wbC
			case i == 2:
				d["recommend"], d["attach_to"] = "attach", wbD
			case i <= 7:
				d["recommend"] = "accept"
			default:
				d["recommend"] = "dismiss"
			}
			k.draft(subject, d)
			evs = append(evs, map[string]any{"id": "wb-" + strings.ToLower(subject), "type": "acc.wb", "subject": subject})
		}
		src.emit(k, evs...)
		proposals = k.waitEvents("src_wb", 12)
		for _, e := range proposals {
			if e.Disposition != "proposed" || e.Item == "" {
				t.Fatalf("事件 %s 应为 proposed 且有事项: %+v", e.Subject, e)
			}
		}
		if !k.WaitFor(60*time.Second, func() bool {
			for _, e := range proposals {
				p, _ := k.item(e.Item)["proposal"].(map[string]any)
				if p == nil || p["recommend"] == nil || p["recommend"] == "" {
					return false
				}
			}
			return true
		}) {
			t.Fatal("12 条建议没有都带上推荐")
		}
		// 停掉全部替身：之后的页面操作若还需要模型就会失败。STUB_HARNESS 在本流程里从未启动，进程表里本来就没有。
		k.Cls.Close()
		k.ModelStub.Close()
		clsLines, modelLines = len(k.Cls.Batches()), len(k.ModelStub.Requests())
		if _, err := http.Get(k.Cls.URL()); err == nil {
			t.Error("CLS_STUB 仍在监听")
		}
		if _, err := http.Get(k.ModelStub.URL()); err == nil {
			t.Error("MODEL_STUB 仍在监听")
		}
	})

	k.step("ACT-WBN-16 360 像素宽度下走完场景十二并逐页走查（ego-browser）", func(t *testing.T) {
		needBrowser(t)
		defer k.closeBrowser("gt315-wbn-mobile")
		seq0 := int64(0)
		for _, e := range k.events("", "") {
			seq0 = max(seq0, e.Seq)
		}
		pages := []string{"today", "decisions", "proposals", "items", "projects", "materials", "delegation", "briefs", "sources", "judgment", "health", "agent-changes", "settings", "audit"}
		pagesJSON, _ := json.Marshal(pages)
		td := k.webToday()
		wantGroups := fmt.Sprintf("[计划今天或更早（%d） 今天到期（%d） 等你决定（%d） 久未推进（%d）]", len(td.Planned), len(td.Due), len(td.Pending), len(td.Stale))
		r := k.browse("gt315-wbn-mobile", fmt.Sprintf(`
await login();
await page.cdp("Emulation.setDeviceMetricsOverride", { width: 360, height: 780, deviceScaleFactor: 2, mobile: true });
await page.cdp("Emulation.setTouchEmulationEnabled", { enabled: true, maxTouchPoints: 5 });
// ① 今天
await page.goto(base + "/today");
await page.waitForSelector("main section h2", { state: "visible" });
out.groups = await page.evaluate(() => [...document.querySelectorAll("main section h2")].map((h) => h.textContent));
// ② 新建 WB-MOB
await page.goto(base + "/items/new");
await page.waitForSelector("form input", { state: "visible" });
await page.fill("loc=role:textbox[name*='标题']", "WB-MOB 交房租");
await page.evaluate((d) => {
  const el = document.querySelector("input[type=date]");
  Object.getOwnPropertyDescriptor(HTMLInputElement.prototype, "value").set.call(el, d);
  el.dispatchEvent(new Event("input", { bubbles: true }));
}, %q);
await page.click("loc=role:button[name='创建']");
await page.waitForFunction(() => /\/items\/itm_/.test(location.pathname));
out.mobPath = await page.evaluate(() => location.pathname);
// ③ WB-D 写进展
await page.goto(base + "/items/" + %q);
await page.waitForSelector("loc=role:button[name='写进展']", { state: "visible" });
await page.fill("loc=role:textbox[name*='进展']", "已交照片");
await page.click("loc=role:button[name='写进展']");
await page.waitForFunction(() => document.body.textContent.includes("已交照片"));
// ④ 按推荐处理
await page.goto(base + "/proposals");
await page.waitForSelector("loc=role:button[name='按推荐处理']", { state: "visible" });
await page.click("loc=role:button[name='按推荐处理']");
await page.click("loc=role:button[name='确认按推荐处理']");
await page.waitForFunction(() => /已处理 \d+ 条/.test(document.body.textContent), undefined, { timeout: 30000 });
out.outcome = await page.evaluate(() => (document.body.textContent.match(/已处理 \d+ 条[^。]*/) || [""])[0]);
// 逐页走查
out.widths = {};
out.empty = [];
for (const p of %s) {
  await page.goto(base + "/" + p);
  await page.waitForSelector("nav", { state: "visible" });
  await page.waitForTimeout(300);
  out.widths[p] = await page.evaluate(() => document.scrollingElement.scrollWidth);
  if (out.widths[p] > 360) out.wide = (out.wide || []).concat([[p, await page.evaluate(() => [...document.querySelectorAll("body *")].filter((e) => !e.closest("nav") && e.getBoundingClientRect().right > 361).slice(0, 6).map((e) => e.tagName + "." + e.className + ":" + Math.round(e.getBoundingClientRect().right)))]]);

  // 主区里的控件都必须落在视口宽度内（页面本身没有控件，如空状态，不算）。
  const off = await page.evaluate(() => [...document.querySelectorAll("main button, main input, main select, main textarea, main a")].filter((e) => { const r = e.getBoundingClientRect(); return r.width > 0 && (r.right > 361 || r.left < -1); }).length);
  if (off > 0) out.empty.push(p + "(" + off + " 个控件超出视口)");
}
`, TMRW, wbD, pagesJSON))
		eq(t, "今天页四组", fmt.Sprint(r["groups"]), wantGroups)
		for p, w := range r["widths"].(map[string]any) {
			if w.(float64) > 360 {
				t.Errorf("页面 %s 的 scrollWidth = %v，超出 360", p, w)
			}
		}
		if w := r["wide"]; w != nil {
			t.Logf("宽于视口的元素（不含导航）: %v", w)
		}
		if e := r["empty"].([]any); len(e) > 0 {
			t.Errorf("这些页面在 360 宽度下有控件超出视口: %v", e)
		}
		// ② 的结果
		mob := k.findItem("WB-MOB 交房租")
		eq(t, "WB-MOB planned_for", k.planned(mob), TMRW)
		// ③
		wrote := false
		for _, p := range k.progress(wbD) {
			wrote = wrote || p["text"] == "已交照片"
		}
		if !wrote {
			t.Error("WB-D 没有进展\"已交照片\"")
		}
		// ④ 12 条建议各有一条 Event；其中 2 条 proposal.attached，成为 WB-C、WB-D 的挂靠消息
		for _, e := range proposals {
			var mine []event
			for _, ev := range k.events(e.Item, "") {
				if ev.Seq > seq0 && ev.Type != "item.updated" {
					mine = append(mine, ev)
				}
			}
			if len(mine) == 0 {
				t.Errorf("建议 %s 没有处置 Event", e.Subject)
				continue
			}
			byOwner(t, "建议 "+e.Subject+" 的 "+mine[0].Type, mine[0])
		}
		if n := len(k.events("", "proposal.attached")); n < 2 {
			t.Errorf("proposal.attached 事件 %d 条，期望 2 条", n)
		}
		for _, it := range []string{wbC, wbD} {
			found := false
			for _, ev := range k.events(it, "proposal.attached") {
				if ev.Seq > seq0 {
					found = true
				}
			}
			if !found {
				t.Errorf("事项 %s 没有挂靠消息（proposal.attached）", it)
			}
		}
		// 全程不需要模型与宿主
		eq(t, "CLS_LOG 行数", len(k.Cls.Batches()), clsLines)
		eq(t, "MODEL_LOG 行数", len(k.ModelStub.Requests()), modelLines)
	})

	k.step("ACT-WBN-17 恢复替身", func(t *testing.T) {
		k.Cls, k.ModelStub = nil, nil
		k.WireEndpoints()
		clsBefore, modelBefore := len(k.Cls.Batches()), len(k.ModelStub.Requests())
		k.probeStubs()
		if len(k.Cls.Batches()) != clsBefore+1 || len(k.ModelStub.Requests()) != modelBefore+1 {
			t.Errorf("恢复后的替身没有记录测试请求: CLS %d→%d，MODEL %d→%d", clsBefore, len(k.Cls.Batches()), modelBefore, len(k.ModelStub.Requests()))
		}
	})
}
