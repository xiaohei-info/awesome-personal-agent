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
	e.BatchAgent("") // 过了 judge 的事件由批处理替身逐条转为建议
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
		// 正文里链接行紧跟在列表项下、中间没有空行，Markdown 把它当作该列表项的续行（加上换行保留，列表项里是"周五出发<br>航司"），
		// 所以列表项的文字是「周五出发」换行「航司」，而不是只有前者。
		eq(t, "列表项", fmt.Sprint(before["li"]), "[周五出发\n航司]")
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

	k.step("ACT-WBN-05 CLI 读写计划日期，计划日期不产生定时；到点提醒关着（默认）时不产生提醒", func(t *testing.T) {
		timers := func() (n int) {
			for _, e := range k.events("", "") {
				if strings.HasPrefix(e.Type, "timer.") {
					n++
				}
			}
			return
		}
		timers0 := timers()
		k.apa(k.WRK, 0, "item", "update", wbF, "--planned-for", TMRW)
		eq(t, "planned_for", k.planned(wbF), TMRW)
		// 计划日期不新建 Timer（审计里没有新的 timer.* 事件；到点提醒开着时也由触达的周期扫描判定，不建 Timer），\`apa why\` 的步骤里没有 timer.*；
		// 到点提醒默认关，所以也没有该事项的提醒（开着时的提醒点见 kernel/attention/schedule_test.go）。
		eq(t, "timer.* 事件数不变", timers(), timers0)
		var why struct {
			Steps []struct {
				Type string `json:"type"`
			} `json:"steps"`
		}
		jsonOf(t, k.apa(k.OBS, 0, "why", wbF).Stdout, &why)
		if len(why.Steps) == 0 {
			t.Error("apa why 没有任何步骤，断言会落空")
		}
		for _, st := range why.Steps {
			if strings.HasPrefix(st.Type, "timer.") {
				t.Errorf("apa why 里有 %s", st.Type)
			}
		}
		if strings.Contains(k.unreadRaw(), wbF) {
			t.Errorf("Web 未读里出现了 WB-F 的提醒: %s", k.unreadRaw())
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
		k.webOK("POST", "/items/"+wbB+"/comments", `{"text":"已订酒店"}`, 201)
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
		k.apa(k.WRK, 0, "item", "comment", wbF, "--text", "联系了供应商", "--on", YDAY)
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
		k.apa(k.WRK, exitUsage, "item", "comment", wbF, "--text", "未来", "--on", TMRW)
		r := k.APIRaw("POST", "/v1/items/"+wbF+"/comments", fmt.Sprintf(`{"text":"未来","on":%q}`, TMRW), k.WRK)
		eq(t, "HTTP", r.Code, 400)
		eq(t, "progress 条数", len(k.progress(wbF)), n)
	})

	// 2026-11-23 改写（权限模型第二版，决定 84）：本人原话不再是依据，写进展是一档，delegate 写任何事项直接生效，
	// 记在 Agent 名下。仍然必须拒绝的是：钥匙够不着的写入（退出码 4）和档位没放开的拍板（退出码 23）；被拒的都不写入、都留 command.rejected。
	k.step("ACT-WBN-11 进展的权限边界", func(t *testing.T) {
		n, nb := len(k.progress(wbF)), len(k.progress(wbB))
		k.apa(k.WRK2, exitDenied, "item", "comment", wbF, "--text", "x") // worker 只够得着自己的事项
		k.apa(k.OBS, exitDenied, "item", "comment", wbB, "--text", "x")  // observer 只读与提建议
		eq(t, "WB-F 进展不变", len(k.progress(wbF)), n)
		eq(t, "WB-B 进展不变", len(k.progress(wbB)), nb)
		rejected(t, k.events(wbF, "command.rejected"), "permission_denied", "WB-F 越权写进展")
		rejected(t, k.events(wbB, "command.rejected"), "permission_denied", "WB-B observer 写进展")

		k.apa(k.DEL, 0, "item", "comment", wbB, "--text", "本人说已订")
		eq(t, "WB-B 进展落账", len(k.progress(wbB)), nb+1)
		eq(t, "Agent 写的进展记在 Agent 名下", k.lastEvent(wbB, "item.progress_added").Actor.Type, "agent")

		// DEL 的 Agent 档位是定规矩（baseline 里放开的）；DELO 不归属 Agent，只整理。它替本人丢掉一件等本人决定的事
		// （离开 needs_input 是二档）：档位未放开，事项不变。
		r := k.apa(k.DELO, exitTier, "item", "transition", wbG, "--to", "dropped", "--reason", "替本人定了")
		if !strings.Contains(r.Stdout+r.Stderr, "tier_closed") {
			t.Errorf("拒绝没有说明档位未放开: %s%s", r.Stdout, r.Stderr)
		}
		eq(t, "WB-G phase", k.phase(wbG), "needs_input")
		rejected(t, k.events(wbG, "command.rejected"), "tier_closed", "WB-G 档位未放开")
	})

	k.step("ACT-WBN-12 全文搜索覆盖标题、摘要、正文与进展", func(t *testing.T) {
		k.webOK("PATCH", "/items/"+wbC, `{"body":"护照续签"}`, 200)
		k.webOK("POST", "/items/"+wbD+"/comments", `{"text":"等护照照片"}`, 201)
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
			jsonOf(t, k.apa(k.OBS, 0, "item", "list", "--sort", sort, "--full", "--limit", "200").Stdout, &cli)
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
		jsonOf(t, k.apa(k.OBS, 0, "item", "list", "--planned-for", TMRW, "--full", "--limit", "200").Stdout, &f)
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
		// 走默认的 judge 与批处理（内置 inbound-triage 包），所以这里不写规则。
		k.Cls.SetDefaultPYes(0.99)
		src := k.registerSource("src_wb", "acc-wb")
		var evs []map[string]any
		for i := 1; i <= 12; i++ {
			subject := fmt.Sprintf("WB-P%02d", i)
			evs = append(evs, map[string]any{"id": "wb-" + strings.ToLower(subject), "type": "acc.wb", "subject": subject})
		}
		// 同一链接的两条事件当场合成一条建议（合并键取链接）：建议卡上因此有一处「合并自 2 条」，360px 走查要用它。
		evs = append(evs,
			map[string]any{"id": "wb-l1", "type": "acc.wb", "subject": "WB-L", "link": "https://example.test/wb/merged"},
			map[string]any{"id": "wb-l2", "type": "acc.wb", "subject": "WB-L", "link": "https://example.test/wb/merged"})
		src.emit(k, evs...)
		allEvents := k.waitEvents("src_wb", 14)
		proposals = allEvents[:12]
		for _, e := range proposals {
			if e.Disposition != "proposed" || e.Item == "" {
				t.Fatalf("事件 %s 应为 proposed 且有事项: %+v", e.Subject, e)
			}
		}
		var mergedOne map[string]any
		for _, e := range allEvents {
			if e.Subject == "WB-L" && e.Item != "" {
				mergedOne = k.item(e.Item)
				break
			}
		}
		if mergedOne == nil {
			t.Fatal("WB-L 的两条事件没有合成一条建议")
		}
		if pr, _ := mergedOne["proposal"].(map[string]any); pr != nil {
			if srcs, _ := pr["sources"].([]any); len(srcs) != 2 {
				t.Fatalf("WB-L 应合成一条带 2 个来源的建议，实际 %v", pr)
			}
		} else {
			t.Fatalf("WB-L 合成的事项没有建议信息: %v", mergedOne)
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
await fillSafe("loc=role:textbox[name*='标题']", "WB-MOB 交房租");
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
await fillSafe("loc=role:textbox[name*='进展']", "已交照片");
await page.click("loc=role:button[name='写进展']");
await page.waitForFunction(() => document.body.textContent.includes("已交照片"));
// ④ 建议页：页顶没有「按推荐处理」；窄屏卡上只有接受、忽略、更多三个按钮，更多菜单不出视口，「合并自 N 条」展开也不撑宽
await page.goto(base + "/proposals");
await page.waitForSelector("main article", { state: "visible" });
out.proposals = await page.evaluate(() => {
  const cards = [...document.querySelectorAll("main article")];
  const first = cards[0];
  return { cards: cards.length, batchButton: [...document.querySelectorAll("main button")].some((b) => /按推荐处理/.test(b.textContent)),
    buttons: [...first.querySelectorAll(":scope > .actions button")].map((b) => b.textContent),
    merged: [...document.querySelectorAll("main article details > summary")].filter((x) => /^合并自 \d+ 条$/.test(x.textContent)).length,
    width: document.scrollingElement.scrollWidth };
});
await page.evaluate(() => document.querySelector("main article .card-actions .row-more > button").click());
await page.waitForSelector(".card-actions [role='menu']", { state: "visible" });
out.menu = await page.evaluate(() => {
  const m = document.querySelector(".card-actions [role='menu']");
  const r = m.getBoundingClientRect();
  return { items: [...m.querySelectorAll("[role='menuitem']")].map((x) => x.textContent), left: Math.round(r.left), right: Math.round(r.right),
    width: document.scrollingElement.scrollWidth };
});
await page.keyboard.press("Escape");
await page.waitForSelector(".card-actions [role='menu']", { state: "detached" });
if (out.proposals.merged > 0) {
  await page.evaluate(() => [...document.querySelectorAll("main article details > summary")].find((x) => /^合并自 \d+ 条$/.test(x.textContent)).click());
  out.expandedWidth = await page.evaluate(() => document.scrollingElement.scrollWidth);
}
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
// 360px 窄屏行内「更多」菜单：打开后菜单与菜单项不超出视口、不撑宽页面，Esc 关闭。
await page.goto(base + "/items");
await page.waitForSelector("main section:first-of-type li:first-child .row-more button", { state: "visible" });
out.rowButtons = await page.evaluate(() => [...document.querySelectorAll("main section:first-of-type li:first-child .act-row .actions button")].map((b) => ({ text: b.textContent, w: Math.round(b.getBoundingClientRect().width), h: Math.round(b.getBoundingClientRect().height) })));
await page.click("main section:first-of-type li:first-child .row-more button");
await page.waitForSelector(".act-row [role='menu']", { state: "visible" });
out.moreOpen = await page.evaluate(() => {
  const m = document.querySelector(".act-row [role='menu']");
  const r = m.getBoundingClientRect();
  const items = [...m.querySelectorAll("[role='menuitem']")].map((x) => {
    const ir = x.getBoundingClientRect();
    return { text: x.textContent, left: Math.round(ir.left), right: Math.round(ir.right), h: Math.round(ir.height) };
  });
  return { scrollWidth: document.scrollingElement.scrollWidth, left: Math.round(r.left), right: Math.round(r.right), items };
});
await page.keyboard.press("Escape");
await page.waitForSelector(".act-row [role='menu']", { state: "detached" });
`, TMRW, wbD, pagesJSON))
		// 今天页的分组标题（界面设计 §5.3）：计划今天或更早与今天到期合成「今天的事（n）」；久未推进收在折叠的 details 里，不是分组标题。
		groups := fmt.Sprint(r["groups"])
		if want := fmt.Sprintf("今天的事（%d）", len(td.Planned)+len(td.Due)); !strings.Contains(groups, want) {
			t.Errorf("今天页应有分组 %q: %s", want, groups)
		}
		if len(td.Pending) > 0 && !strings.Contains(groups, "要你处理") {
			t.Errorf("有等你决定的事时今天页应有「要你处理」: %s", groups)
		}
		for p, w := range r["widths"].(map[string]any) {
			if w.(float64) > 360 {
				t.Errorf("页面 %s 的 scrollWidth = %v，超出 360", p, w)
			}
		}
		prop := r["proposals"].(map[string]any)
		if prop["cards"].(float64) == 0 {
			t.Error("建议页一张卡都没有，走查无从谈起")
		}
		if prop["batchButton"] == true {
			t.Error("建议页不应再有「按推荐处理」")
		}
		if got := fmt.Sprint(prop["buttons"]); got != "[接受 忽略 更多]" {
			t.Errorf("360px 下建议卡的操作按钮 = %s，期望只有接受、忽略、更多", got)
		}
		menu := r["menu"].(map[string]any)
		if sw := menu["width"].(float64); sw > 360 {
			t.Errorf("展开建议卡「更多」菜单后 scrollWidth = %v，超出 360", sw)
		}
		if left, right := menu["left"].(float64), menu["right"].(float64); left < 0 || right > 360 {
			t.Errorf("建议卡「更多」菜单超出 360 视口: left=%v right=%v", left, right)
		}
		if w, ok := r["expandedWidth"].(float64); ok && w > 360 {
			t.Errorf("展开「合并自 N 条」后 scrollWidth = %v，超出 360", w)
		}
		t.Logf("360px 建议页 cards=%v merged=%v menu=%v expandedWidth=%v", prop["cards"], prop["merged"], menu["items"], r["expandedWidth"])
		if w := r["wide"]; w != nil {
			t.Logf("宽于视口的元素（不含导航）: %v", w)
		}
		if e := r["empty"].([]any); len(e) > 0 {
			t.Errorf("这些页面在 360 宽度下有控件超出视口: %v", e)
		}
		moreOpen := r["moreOpen"].(map[string]any)
		if sw := moreOpen["scrollWidth"].(float64); sw > 360 {
			t.Errorf("展开行内「更多」菜单后 scrollWidth = %v，超出 360", sw)
		}
		if left, right := moreOpen["left"].(float64), moreOpen["right"].(float64); left < 0 || right > 360 {
			t.Errorf("行内「更多」菜单超出 360 视口: left=%v right=%v", left, right)
		}
		t.Logf("360px scrollWidths=%v rowButtons=%v moreOpen=%v", r["widths"], r["rowButtons"], moreOpen)
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
		// ④ 走查之后经批量接口一次处理这批，页面只管摆放与按钮：两条补到 WB-C、WB-D，其余接受或忽略；12 条建议各有一条 Event。
		var actions []map[string]any
		for i, e := range proposals {
			act := map[string]any{"id": e.Item, "do": "accept"}
			switch {
			case i < 2:
				act["do"], act["to"] = "attach", []string{wbC, wbD}[i]
			case i >= 7:
				act["do"] = "dismiss"
			}
			actions = append(actions, act)
		}
		plan, _ := json.Marshal(map[string]any{"mode": "explicit", "actions": actions})
		k.webOK("POST", "/proposals/batch", string(plan), 200)
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
