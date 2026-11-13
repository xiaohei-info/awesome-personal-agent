//go:build e2e

package automation

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const accWatchPack = `name: acc-watch
version: 1.0.0
description: 条件监视与知会验收
source_rules:
  - id: metric-over-limit
    match: {source_type: acc-metric, event_type: [metric.sampled]}
    where:
      - {field: metrics.value, op: crosses_above, value: 90}
    then: alert
  - id: metric-quiet
    match: {source_type: acc-metric}
    then: ignore
  - id: acc-newsletter
    match: {source_type: acc-news, sender: ["news@example.com"]}
    then: digest
`

const accBadAlertPack = `name: acc-badalert
version: 1.0.0
description: alert 没有字段条件必须被拒绝
source_rules:
  - id: any-mail-alert
    match: {source_type: acc-news}
    then: alert
`

func metric(id string, v int) map[string]any {
	return map[string]any{"id": id, "type": "metric.sampled", "subject": "acc 队列长度", "occurred_at": now(),
		"link": "https://example.invalid/queue", "correlation_keys": []string{"acc-queue"}, "metrics": map[string]any{"value": v}}
}

func news(id string, n int) map[string]any {
	return map[string]any{"id": id, "type": "mail.received", "subject": fmt.Sprintf("资讯 %d", n), "summary": fmt.Sprintf("资讯摘要 %d", n),
		"sender": "news@example.com", "occurred_at": now(), "link": fmt.Sprintf("https://example.invalid/news/%d", n)}
}

// TestFlowDigestAlert 按验收文档 flow-digest-alert（场景十）执行 ACT-DGA-01..20：
// 数值来源越过阈值时直接提醒本人，只需知会的来源汇入简报"知会"一节，全程不调模型。
func TestFlowDigestAlert(t *testing.T) {
	k := newKit(t)
	e := k.Env
	packs := filepath.Join(e.Home, "packs")
	var metricSrc, newsSrc *queueSource
	ruleKey := "acc-watch/source_rules/metric-over-limit"
	seq := 0
	sample := func(v int) srcEvent {
		t.Helper()
		seq++
		id := fmt.Sprintf("m-%d", seq)
		metricSrc.emit(metric(id, v))
		return k.waitEvent("src_metric", id)
	}

	k.step("ACT-DGA-01 登记数值来源与资讯来源", func(t *testing.T) {
		metricSrc = k.registerSource("src_metric", "acc-metric")
		newsSrc = k.registerSource("src_news", "acc-news")
		out := k.apa(e.OBS, 0, "source", "list").Stdout
		if !contains(out, "src_metric", "src_news") {
			t.Fatalf("source list 应列出两个来源: %s", out)
		}
		// 提醒走渠道 acc-chan，CHAN 才领得到；日上限先放宽以免干扰
		k.setDisturb(map[string]any{"routes": map[string]string{"decision": "acc-chan", "reminder": "acc-chan", "brief": "web", "health": "web", "wake": "web"},
			"merge_window_sec": 0, "daily_cap": 20})
		k.markModelBaseline()
	})

	k.step("ACT-DGA-02 写入条件监视与摘要规则的用户能力包", func(t *testing.T) {
		writePack(t, packs, "acc-watch", map[string]string{"pack.yaml": accWatchPack})
		k.apa(e.OBS, 0, "pack", "validate", filepath.Join(packs, "acc-watch"))
		k.restart()
		if _, ok := packList(t, e)["acc-watch"]; !ok {
			t.Fatalf("pack list 应含 acc-watch")
		}
		s, ok := rules(t, e)["metric-over-limit"]
		if !ok || s.Mode != "observe" {
			t.Fatalf("metric-over-limit 应已加载且初始为 observe: %+v", s)
		}
	})

	k.step("ACT-DGA-03 没有字段条件的 alert 规则被拒绝", func(t *testing.T) {
		dir := writePack(t, packs, "acc-badalert", map[string]string{"pack.yaml": accBadAlertPack})
		r := e.Apa(e.OBS, "", "pack", "validate", dir)
		if r.Code == 0 || !contains(r.Stdout+r.Stderr, "where") {
			t.Fatalf("validate 应非 0 且指出 alert 缺 where[]: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		k.restart()
		if _, ok := packList(t, e)["acc-badalert"]; ok {
			t.Error("pack list 不应含被拒绝的 acc-badalert")
		}
		h := k.apa(e.OBS, 0, "health").Stdout
		if !contains(h, "pack_rejections", "acc-badalert", "where") {
			t.Errorf("健康视图 pack_rejections 应列出该包与原因: %s", h)
		}
		k.must(os.RemoveAll(dir))
		k.restart()
	})

	k.step("ACT-DGA-04 序列第一次出现与未越过阈值时只留痕", func(t *testing.T) {
		before := len(k.items(""))
		for _, v := range []int{50, 80} {
			if ev := sample(v); ev.Disposition != "ignored" {
				t.Errorf("值 %d 的事件处置应为 ignored，实际 %s", v, ev.Disposition)
			}
		}
		if n := len(k.items("")); n != before {
			t.Errorf("不应建事项: %d → %d", before, n)
		}
		if d := k.claim(5); d != nil {
			t.Errorf("CHAN 不应领到提醒: %s", d.Raw)
		}
		k.noModelCalls("ACT-DGA-04")
	})

	k.step("ACT-DGA-05 alert 规则在 observe 下只记\"本来会提醒\"", func(t *testing.T) {
		ev := sample(95)
		if ev.Disposition == "alerted" {
			t.Fatalf("observe 下处置不应是 alerted: %+v", ev)
		}
		if d := k.claim(5); d != nil {
			t.Errorf("observe 下 CHAN 不应领到提醒: %s", d.Raw)
		}
		if u := k.unread(); len(u) != 0 && strings.Contains(fmt.Sprint(u), ev.ID) {
			t.Errorf("observe 下 Web 未读不应有这条提醒: %+v", u)
		}
		w := k.why(ev.ID)
		if !contains(w, "rule.observed", "metric-over-limit") {
			t.Errorf("apa why 应有 rule.observed 并写明 metric-over-limit: %s", w)
		}
	})

	k.step("ACT-DGA-06 alert 规则只能在 Web 切到 auto", func(t *testing.T) {
		path := "/v1/rules/" + url.PathEscape(ruleKey) + "/mode"
		if r := e.APIRaw("PATCH", path, `{"mode":"auto"}`, e.DEL); r.Code != 403 {
			t.Errorf("DEL 切 auto 应 403，实际 %d %s", r.Code, r.Body)
		}
		if s := rules(t, e)["metric-over-limit"]; s.Mode != "observe" {
			t.Errorf("被拒后应仍为 observe: %+v", s)
		}
		k.webOK("PATCH", path, `{"mode":"auto"}`, 200)
		if s := rules(t, e)["metric-over-limit"]; s.Mode != "auto" {
			t.Errorf("Web 切换后应为 auto: %+v", s)
		}
		ev := k.apa(e.OBS, 0, "events", "--limit", "500").Stdout
		if !contains(ev, "rule.mode_changed", ruleKey) {
			t.Errorf("审计应有 rule.mode_changed: %s", ev)
		}
	})

	var alerted srcEvent // ACT-DGA-07 值 96 的来源事件
	k.step("ACT-DGA-07 切到 auto 后上穿阈值产出无事项的提醒投递", func(t *testing.T) {
		before := len(k.items(""))
		if ev := sample(70); ev.Disposition != "ignored" {
			t.Errorf("值 70 应 ignored，实际 %s", ev.Disposition)
		}
		alerted = sample(96)
		if alerted.Disposition != "alerted" {
			t.Fatalf("值 96 应 alerted，实际 %+v", alerted)
		}
		d := k.claim(10)
		if d == nil {
			t.Fatal("CHAN 应领到一条提醒")
		}
		if d.Class != "reminder" || d.Item != "" {
			t.Errorf("应为 class=reminder 且 item 为空: %s", d.Raw)
		}
		if d.Origin.Event != alerted.ID || !strings.HasSuffix(d.Origin.Rule, "metric-over-limit") {
			t.Errorf("origin 应含来源事件 id %s 与规则 id metric-over-limit: %s", alerted.ID, d.Raw)
		}
		if d.Content.Title != "acc 队列长度" || !strings.Contains(d.Raw, "https://example.invalid/queue") {
			t.Errorf("内容应取来源事件的标题与链接: %s", d.Raw)
		}
		if n := len(k.items("")); n != before {
			t.Errorf("事项数应不变: %d → %d", before, n)
		}
		k.noModelCalls("ACT-DGA-07")
	})

	k.step("ACT-DGA-08 持续高于阈值不重复提醒", func(t *testing.T) {
		if ev := sample(97); ev.Disposition != "ignored" {
			t.Errorf("值 97 应 ignored（96→97 不是上穿），实际 %s", ev.Disposition)
		}
		if d := k.claim(10); d != nil {
			t.Errorf("不应有新提醒: %s", d.Raw)
		}
	})

	var restarted srcEvent // ACT-DGA-09 重启后值 99 的来源事件
	k.step("ACT-DGA-09 上一次取值在重启后仍被保存", func(t *testing.T) {
		if ev := sample(60); ev.Disposition != "ignored" {
			t.Fatalf("值 60（下穿）应 ignored，实际 %s", ev.Disposition)
		}
		k.StopKernel()
		seq++
		id := fmt.Sprintf("m-%d", seq)
		metricSrc.emit(metric(id, 99))
		k.StartKernelAgain()
		restarted = k.waitEvent("src_metric", id)
		if restarted.Disposition != "alerted" {
			t.Fatalf("重启后的第一条样本 99 应与保存的 60 比较判为上穿 alerted，实际 %+v", restarted)
		}
	})

	k.step("ACT-DGA-10 apa why 回放规则、条件、上一次取值与投递", func(t *testing.T) {
		var w struct {
			Alerts []struct {
				Rule        string `json:"rule"`
				Disposition string `json:"disposition"`
				Conditions  []struct {
					Condition string  `json:"condition"`
					Previous  float64 `json:"previous"`
					Current   float64 `json:"current"`
				} `json:"conditions"`
				Delivery struct {
					ID    string `json:"id"`
					State string `json:"state"`
				} `json:"delivery"`
			} `json:"alerts"`
		}
		jsonOf(t, k.why(alerted.ID), &w)
		if len(w.Alerts) != 1 {
			t.Fatalf("apa why 应回放一次提醒判定: %+v", w)
		}
		a := w.Alerts[0]
		if !strings.HasSuffix(a.Rule, "metric-over-limit") || a.Disposition != "alerted" || len(a.Conditions) != 1 {
			t.Fatalf("规则与处置不对: %+v", a)
		}
		if c := a.Conditions[0]; c.Condition != "metrics.value crosses_above 90" || c.Previous != 70 || c.Current != 96 {
			t.Errorf("应回放条件、上一次取值 70、本次取值 96: %+v", c)
		}
		if a.Delivery.ID == "" || a.Delivery.State == "" {
			t.Errorf("应回放产出的投递 id 与状态: %+v", a.Delivery)
		}
	})

	k.step("ACT-DGA-11 随包渠道 ext 能处理没有事项的提醒", func(t *testing.T) {
		stop, bot, logs := k.startExt()
		defer stop()
		k.WaitFor(30*time.Second, func() bool { return len(bot.Sent()) > 0 })
		sent := bot.Sent()
		if len(sent) != 1 || !contains(sent[0].Text, "acc 队列长度", "https://example.invalid/queue") || len(sent[0].Buttons) != 0 {
			t.Fatalf("机器人替身应收到一条含标题与链接、不含选项的消息: %+v", sent)
		}
		if !k.WaitFor(30*time.Second, func() bool { return strings.Contains(k.why(restarted.ID), `"state":"delivered"`) }) {
			t.Errorf("投递状态应转 delivered:\n%s", k.why(restarted.ID))
		}
		// ext 继续领取后续投递：再来一次上穿
		sample(40)
		next := sample(94)
		if next.Disposition != "alerted" {
			t.Fatalf("94 应 alerted: %+v", next)
		}
		if !k.WaitFor(30*time.Second, func() bool { return len(bot.Sent()) == 2 }) {
			t.Errorf("ext 应继续领取后续投递，机器人收到 %d 条\next 日志:\n%s", len(bot.Sent()), logs())
		}
		if l := logs(); strings.Contains(strings.ToLower(l), "error") || strings.Contains(strings.ToLower(l), "panic") {
			t.Errorf("ext 日志不应有错误:\n%s", l)
		}
	})

	k.step("ACT-DGA-12 提醒出口受日上限约束", func(t *testing.T) {
		k.setDisturb(map[string]any{"routes": map[string]string{"decision": "acc-chan", "reminder": "acc-chan", "brief": "web", "health": "web", "wake": "web"},
			"merge_window_sec": 0, "daily_cap": 1, "remind_on_deadline": true})
		defer k.setDisturb(map[string]any{"routes": map[string]string{"decision": "acc-chan", "reminder": "acc-chan", "brief": "web", "health": "web", "wake": "web"},
			"merge_window_sec": 0, "daily_cap": 20})
		// 用掉当天额度：一个即将到期的事项产生普通提醒
		r := e.Apa(e.WRK, "", "item", "add", "--kind", "task", "--title", "acc-cap-deadline", "--summary", "占用当天提醒额度", "--phase", "open", "--assignee", "work-host.cli",
			"--project", e.Projects["acc-work"], "--evidence", "占用额度")
		var created ledgerItem
		if r.Code != 0 {
			t.Fatalf("建事项: %d %s%s", r.Code, r.Stdout, r.Stderr)
		}
		jsonOf(t, r.Stdout, &created)
		k.webOK("PATCH", "/v1/items/"+created.ID, `{"deadline":"`+time.Now().Add(3*time.Second).UTC().Format(time.RFC3339)+`"}`, 200)
		used := k.claim(30)
		if used == nil {
			t.Fatal("到期的普通提醒应经渠道送出并用掉额度")
		}
		sample(50)
		ev := sample(95)
		if ev.Disposition != "alerted" {
			t.Fatalf("来源事件处置仍应为 alerted: %+v", ev)
		}
		if d := k.claim(5); d != nil {
			t.Errorf("超过日上限的提醒 CHAN 不应领到: %s", d.Raw)
		}
		found := false
		for _, u := range k.unread() {
			found = found || u.Origin.Event == ev.ID
		}
		if !found {
			t.Errorf("超过上限的提醒应出现在 Web 未读: %+v", k.unread())
		}
	})

	k.step("ACT-DGA-13 摘要出口不建事项、不提醒、不调模型", func(t *testing.T) {
		items, unread := len(k.items("")), len(k.unread())
		newsSrc.emit(news("n-1", 1), news("n-2", 2))
		for _, id := range []string{"n-1", "n-2"} {
			if ev := k.waitEvent("src_news", id); ev.Disposition != "digest" {
				t.Errorf("%s 应 digest，实际 %s", id, ev.Disposition)
			}
		}
		if inbox := k.apa(e.OBS, 0, "inbox").Stdout; strings.Contains(inbox, "资讯") {
			t.Errorf("建议流不应有 digest 事件: %s", inbox)
		}
		if n := len(k.items("")); n != items {
			t.Errorf("事项数应不变: %d → %d", items, n)
		}
		if d := k.claim(5); d != nil {
			t.Errorf("不应有投递: %s", d.Raw)
		}
		if n := len(k.unread()); n != unread {
			t.Errorf("Web 未读不应增加: %d → %d", unread, n)
		}
		k.noModelCalls("ACT-DGA-13")
	})

	var briefHarness *accHarness
	var briefInstance string
	today := time.Now().Format("2006-01-02")
	tomorrow := time.Now().AddDate(0, 0, 1).Format("2006-01-02")
	k.step("ACT-DGA-14 准备简报实例与计划日期数据", func(t *testing.T) {
		briefHarness = k.declareHarness("acc-stub-brief", "brief")
		k.webOK("PUT", "/v1/settings/policy", `{"default_harness":"acc-stub-brief"}`, 200)
		plan := func(title string, extra ...string) {
			args := append([]string{"item", "add", "--kind", "task", "--title", title, "--summary", title, "--phase", "open", "--assignee", "work-host.cli",
				"--project", e.Projects["acc-home"], "--evidence", "准备计划日期数据"}, extra...)
			k.apa(e.WRK, 0, args...)
		}
		plan("acc-plan-today", "--planned-for", today)
		plan("acc-plan-tomorrow", "--planned-for", tomorrow)
		plan("acc-plan-none")
		r := k.apa(e.DEL, 0, "--evidence", "owner: 建简报实例", "instance", "add", "brief", "--param", "schedule=* * * * *")
		var inst struct {
			ID string `json:"id"`
		}
		jsonOf(t, r.Stdout, &inst)
		briefInstance = inst.ID
		var today0 ledgerItem
		for _, it := range k.items("") {
			if it.Title == "acc-plan-today" {
				today0 = it
			}
		}
		if got, _ := k.show(today0.ID); got.PlannedFor != today {
			t.Errorf("acc-plan-today 的 planned_for 应为 %s: %+v", today, got)
		}
		if l := k.apa(e.OBS, 0, "instance", "list").Stdout; !contains(l, "brief", briefInstance) {
			t.Errorf("instance list 应含 brief 实例: %s", l)
		}
	})

	var first string // 第一份简报的运行 id
	var firstBrief ledgerItem
	k.step("ACT-DGA-15 摘要出口的内容进入下一份简报的\"知会\"一节", func(t *testing.T) {
		e.TCAdvance(61) // 验收文档等 2 分钟；测试时钟让"到期"立即发生
		if !k.WaitFor(60*time.Second, func() bool { return len(briefHarness.RunRecords()) >= 1 }) {
			t.Fatalf("没有生成第一份简报的运行\n%s", e.ServiceLog())
		}
		first = briefHarness.RunRecords()[0]
		mat := briefHarness.File(first, "materials.json")
		if !contains(materialBody(t, mat, "digest"), "资讯 1", "资讯 2", "https://example.invalid/news/1", "https://example.invalid/news/2") {
			t.Errorf("资料文件应有\"知会\"一节列出两条 digest 事件的标题与链接:\n%s", mat)
		}
		for _, it := range k.items("") {
			if strings.HasPrefix(it.Title, "资讯") {
				t.Errorf("digest 事件不应作为事项出现: %+v", it)
			}
		}
		if !k.WaitFor(30*time.Second, func() bool {
			bs := k.items("brief")
			if len(bs) == 0 {
				return false
			}
			firstBrief, _ = k.show(bs[len(bs)-1].ID)
			return contains(string(firstBrief.Spec), "知会", "资讯 1", "资讯 2")
		}) {
			t.Errorf("简报（Web 简报页读的 spec.sections）应显示\"知会\"一节: %+v", firstBrief)
		}
	})

	k.step("ACT-DGA-16 知会内容只汇入一次", func(t *testing.T) {
		e.TCAdvance(61)
		if !k.WaitFor(60*time.Second, func() bool { return len(briefHarness.RunRecords()) >= 2 }) {
			t.Fatalf("没有生成第二份简报的运行\n%s", e.ServiceLog())
		}
		second := briefHarness.RunRecords()[1]
		if mat := briefHarness.File(second, "materials.json"); strings.Contains(materialBody(t, mat, "digest"), "资讯") {
			t.Errorf("第二份简报不应再含已汇入过的两条:\n%s", mat)
		}
	})

	k.step("ACT-DGA-17 简报按计划日期取材", func(t *testing.T) {
		planned := materialBody(t, briefHarness.File(first, "materials.json"), "planned")
		if !strings.Contains(planned, "acc-plan-today") {
			t.Fatalf("按计划日期取的内容应含 acc-plan-today: %s", planned)
		}
		for _, no := range []string{"acc-plan-tomorrow", "acc-plan-none"} {
			if strings.Contains(planned, no) {
				t.Errorf("%s 不应在今天的计划内容里: %s", no, planned)
			}
		}
		if !contains(string(firstBrief.Spec), "acc-plan-today") || strings.Contains(string(firstBrief.Spec), "acc-plan-tomorrow") {
			t.Errorf("简报应写出计划今天的事项且不含明天的: %s", firstBrief.Spec)
		}
	})

	k.step("ACT-DGA-18 知会条目可逐条转为建议", func(t *testing.T) {
		var n1 srcEvent
		for _, ev := range k.events("src_news") {
			switch ev.DedupeKey.EventID {
			case "n-1":
				n1 = ev
			}
		}
		k.webOK("POST", "/v1/source-events/"+n1.ID+"/propose", `{}`, 200)
		for _, ev := range k.events("src_news") {
			switch ev.DedupeKey.EventID {
			case "n-1":
				if ev.Disposition != "proposed" {
					t.Errorf("第一条应转为 proposed: %+v", ev)
				}
			case "n-2":
				if ev.Disposition != "digest" {
					t.Errorf("另一条应仍为 digest: %+v", ev)
				}
			}
		}
		inbox := k.apa(e.OBS, 0, "inbox").Stdout
		if !contains(inbox, "资讯 1") || strings.Contains(inbox, "资讯 2") {
			t.Errorf("建议流应只有第一条: %s", inbox)
		}
		var prop ledgerItem
		for _, it := range k.items("") {
			if strings.Contains(it.Title, "资讯 1") {
				prop = it
			}
		}
		w := k.why(prop.ID)
		t.Logf("建议的 apa why: %s", w)
		if !contains(w, `"type":"human"`) {
			t.Errorf("apa why 应显示操作者为本人: %s", w)
		}
	})

	k.step("ACT-DGA-19 场景十整条路径不调模型、不依赖宿主定时任务", func(t *testing.T) {
		// ACT-DGA-14 之后简报事项自己的一句话摘要（kind=brief）不属于场景十路径，验收文档不计入；其余请求一条都不许有。
		if n := len(k.Cls.Batches()) - k.clsBase; n != 0 {
			t.Errorf("CLS_LOG 比基线多 %d 条请求", n)
		}
		for _, r := range k.ModelStub.Requests()[k.modelBase:] {
			if !strings.Contains(r.BodyText, `\"kind\":\"brief\"`) || strings.Contains(r.BodyText, "资讯") || strings.Contains(r.BodyText, "acc 队列长度") {
				t.Errorf("MODEL_LOG 出现场景十路径上的模型请求: %s", r.BodyText)
			}
		}
	})

	k.step("ACT-DGA-20 清理", func(t *testing.T) {
		for _, id := range []string{"src_metric", "src_news"} {
			k.webOK("PATCH", "/v1/sources/"+id, `{"enabled":false}`, 200)
		}
		k.must(os.RemoveAll(filepath.Join(packs, "acc-watch")))
		k.restart()
		k.apa(e.DEL, 0, "--evidence", "owner: 停用简报实例", "instance", "disable", briefInstance)
		if _, ok := packList(t, e)["acc-watch"]; ok {
			t.Error("acc-watch 不应在 pack list 中")
		}
		for _, id := range []string{"src_metric", "src_news"} {
			if out := k.apa(e.OBS, 0, "source", "show", id).Stdout; !contains(strings.ReplaceAll(out, " ", ""), `"enabled":false`) {
				t.Errorf("来源 %s 应已停用: %s", id, out)
			}
		}
		if l := k.apa(e.OBS, 0, "instance", "list").Stdout; strings.Contains(strings.ReplaceAll(l, " ", ""), `"enabled":true`) {
			t.Errorf("brief 实例应已停用: %s", l)
		}
	})
}
