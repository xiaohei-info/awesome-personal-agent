//go:build e2e

package outbound

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const wait = 60 * time.Second

func TestOutboundScope(t *testing.T) {
	top := t
	k := newKit(t)
	var (
		eventsBefore int // 建议流（proposed 事项）条数
		rssID        = "src_rss"
	)

	k.step("ACT-OBD-01 两个端点都声明为外部端点", func(t *testing.T) {
		k.endpoints("external", "external")
		v := k.endpointView()
		if v.Judge.Locality != "external" || v.Generate.Locality != "external" {
			t.Fatalf("位置未保存: %+v", v)
		}
		k.clear()
		k.cases("OBD-")
	})

	k.step("ACT-OBD-02 登记来源：外部范围没有缺省值，本机范围缺省全文", func(t *testing.T) {
		k.registerObd()
		show := k.sourceShow(obdSrc)
		if show.Outbound.Local != "full" || show.Outbound.External != nil {
			t.Fatalf("登记后应是 local=full、external 未选择: %+v", show.Outbound)
		}
		// 外部范围只能经 Web 设置接口写；凭据登记不能带 outbound（见 ACT-OBD-10）。
		k.scope(obdSrc, "full", "full")
		show = k.sourceShow(obdSrc)
		if show.Outbound.Local != "full" || show.Outbound.External == nil || *show.Outbound.External != "full" {
			t.Fatalf("选定后应是 local=full、external=full: %+v", show.Outbound)
		}
	})

	k.step("ACT-OBD-03 外部范围 full：外部端点收到全文，判断记录记端点位置", func(t *testing.T) {
		k.clear()
		k.emit(1)
		ev := k.waitEvent(obdSrc, "OBD-1", wait)
		if ev.Disposition != "proposed" {
			t.Fatalf("OBD-1 应进建议流: %+v", ev)
		}
		cls := requestsWith(k.clsLog(), "OBD-1")
		k.contains("CLS_STUB", cls, "HEAD-MARK", "MID-MARK", "TAIL-MARK")
		k.lacks("CLS_STUB", cls, `"material_truncated":true`)
		k.contains("MODEL_STUB triage-draft", k.draftRequests("OBD-1"), "HEAD-MARK", "MID-MARK", "TAIL-MARK")
		j := k.judgment(ev.Judgment)
		if len(j.Levels) == 0 || j.Levels[0].Locality != "external" {
			t.Fatalf("判断记录的 levels[] 应记下端点位置 external: %+v", j)
		}
	})

	k.step("ACT-OBD-04 外部范围 excerpt：只送元信息、摘要与正文前 500 字符", func(t *testing.T) {
		k.scope(obdSrc, "full", "excerpt")
		k.clear()
		k.emit(2)
		k.waitEvent(obdSrc, "OBD-2", wait)
		cls, draft := requestsWith(k.clsLog(), "OBD-2"), k.draftRequests("OBD-2")
		for label, b := range map[string][]string{"CLS_STUB": cls, "MODEL_STUB triage-draft": draft} {
			k.contains(label, b, "SUM-MARK", "HEAD-MARK")
			k.lacks(label, b, "MID-MARK", "TAIL-MARK")
		}
	})

	k.step("ACT-OBD-05 外部范围 meta：只送类型、标题、发送者、时间与链接域名", func(t *testing.T) {
		k.scope(obdSrc, "full", "meta")
		k.clear()
		k.emit(3)
		k.waitEvent(obdSrc, "OBD-3", wait)
		cls, draft := requestsWith(k.clsLog(), "OBD-3"), k.draftRequests("OBD-3")
		for label, b := range map[string][]string{"CLS_STUB": cls, "MODEL_STUB triage-draft": draft} {
			k.contains(label, b, "obd.message", "OBD-3 外发测试", sender, "occurred_at", "docs.example.org")
			k.lacks(label, b, "/path/secret-123", "SUM-MARK", "HEAD-MARK", "MID-MARK", "TAIL-MARK")
		}
	})

	k.step("ACT-OBD-06 对所有可用端点都为 none：不外发，事件改走摘要出口", func(t *testing.T) {
		eventsBefore = k.proposals()
		if eventsBefore == 0 {
			t.Fatal("OBD-1..3 已进建议流，建议流不应为空（读取建议流的方式有误）")
		}
		k.scope(obdSrc, "full", "none")
		k.clear()
		k.emit(4)
		ev := k.waitEvent(obdSrc, "OBD-4", wait)
		time.Sleep(3 * time.Second) // 给"万一外发了"留出时间：两个替身都不应出现 OBD-4
		if n := len(requestsWith(k.clsLog(), "OBD-4")) + len(requestsWith(k.modelLog(), "OBD-4")); n != 0 {
			t.Fatalf("两个外部端点不应收到 OBD-4，实际 %d 个请求", n)
		}
		if ev.Disposition != "digest" || ev.Item != "" {
			t.Fatalf("OBD-4 应走摘要出口、不建事项: %+v", ev)
		}
		var digest []SrcEvent
		for _, e := range k.events(obdSrc, "&disposition=digest") {
			if strings.Contains(e.Subject, "OBD-4") {
				digest = append(digest, e)
			}
		}
		if len(digest) != 1 {
			t.Fatalf("OBD-4 应留在来源页的 digest 列表里: %+v", digest)
		}
		if n := k.proposals(); n != eventsBefore {
			t.Fatalf("建议流不应多出 OBD-4 的建议: %d → %d", eventsBefore, n)
		}
	})

	k.step("ACT-OBD-07 按端点位置分别取范围：本机端点全文，外部端点 meta", func(t *testing.T) {
		k.endpoints("local", "external")
		k.scope(obdSrc, "full", "meta")
		k.clear()
		k.emit(5)
		k.waitEvent(obdSrc, "OBD-5", wait)
		k.contains("CLS_STUB（本机）", requestsWith(k.clsLog(), "OBD-5"), "HEAD-MARK", "MID-MARK", "TAIL-MARK")
		draft := k.draftRequests("OBD-5")
		k.contains("MODEL_STUB（外部）", draft, "OBD-5 外发测试", "docs.example.org")
		k.lacks("MODEL_STUB（外部）", draft, "SUM-MARK", "HEAD-MARK", "MID-MARK", "TAIL-MARK")
	})

	k.step("ACT-OBD-08 某端点范围为 none：该端点不可用，级联交下一级", func(t *testing.T) {
		k.endpoints("external", "local")
		k.scope(obdSrc, "full", "none")
		k.clear()
		k.emit(6)
		ev := k.waitEvent(obdSrc, "OBD-6", wait)
		if n := len(requestsWith(k.clsLog(), "OBD-6")); n != 0 {
			t.Fatalf("外部判断端点不应收到 OBD-6，实际 %d 个请求", n)
		}
		k.contains("MODEL_STUB triage-screen", k.screenRequests("OBD-6"), "HEAD-MARK", "MID-MARK", "TAIL-MARK")
		if ev.Disposition == "ignored" || ev.Disposition == "unjudged" {
			t.Fatalf("OBD-6 不应被忽略或记未判定: %+v", ev)
		}
		j := k.judgment(ev.Judgment)
		if j.DecidedBy != "chat" || len(j.Levels) == 0 || j.Levels[0].Backend != "classifier" || j.Levels[0].Outcome != "skipped" {
			t.Fatalf("分类端点那一级应 skipped、由 chat 定案: %+v", j)
		}
	})

	k.step("ACT-OBD-09 端点位置未声明时按 external 处理", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"judge_endpoint":       map[string]any{"url": k.Cls.URL(), "api_key": "stub-key", "protocol": "probability-judgment/1"},
			"generate_endpoint":    map[string]any{"url": k.ModelStub.URL(), "api_key": "stub-key", "model": "acc-model"},
			"screening_timeout_ms": 2000,
		})
		k.webOK("PUT", "/v1/settings/model-endpoints", string(body), 200)
		v := k.endpointView()
		if v.Judge.Locality != "external" || v.Generate.Locality != "external" {
			t.Fatalf("未声明的位置应返回 external: %+v", v)
		}
		k.clear()
		k.emit(7)
		ev := k.waitEvent(obdSrc, "OBD-7", wait)
		time.Sleep(3 * time.Second)
		if n := len(requestsWith(k.clsLog(), "OBD-7")) + len(requestsWith(k.modelLog(), "OBD-7")); n != 0 {
			t.Fatalf("按 external（none）处理，两个替身不应出现 OBD-7，实际 %d", n)
		}
		if ev.Disposition != "digest" {
			t.Fatalf("OBD-7 应走摘要出口: %+v", ev)
		}
	})

	k.step("ACT-OBD-10 外发范围不能经 API 用凭据写入", func(t *testing.T) {
		before := k.sourceShow(obdSrc)
		n0 := k.sourceUpdates()
		for _, c := range []struct{ method, path, body string }{
			{"PUT", "/v1/settings/sources/" + obdSrc + "/outbound", `{"local":"full","external":"full"}`},
			{"PATCH", "/v1/sources/" + obdSrc, `{"outbound":{"external":"full"}}`},
			{"PUT", "/v1/settings/projects/" + k.Projects["acc-open"] + "/capture-outbound", `{"value":"full"}`},
		} {
			if r := k.APIRaw(c.method, c.path, c.body, k.DEL); r.Code != 403 {
				t.Fatalf("%s %s 应返回 403，实际 %d %s", c.method, c.path, r.Code, r.Body)
			}
		}
		after := k.sourceShow(obdSrc)
		if after.Outbound.Local != before.Outbound.Local || *after.Outbound.External != *before.Outbound.External {
			t.Fatalf("来源的外发范围被改变: %+v → %+v", before.Outbound, after.Outbound)
		}
		if v := k.captureOutbound(k.Projects["acc-open"]); v != nil {
			t.Fatalf("项目的 capture_outbound 被改变: %v", *v)
		}
		if n1 := k.sourceUpdates(); n1 != n0 {
			t.Fatalf("事件流里不应有新的来源更新事件: %d → %d", n0, n1)
		}
	})

	k.step("ACT-OBD-11 CLI 登记的来源可带默认项目，外部范围为未选择", func(t *testing.T) {
		feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/rss+xml")
			fmt.Fprintf(w, `<?xml version="1.0"?><rss version="2.0"><channel><title>obd</title><item><title>OBD-RSS-1</title><link>https://feed.example.org/1</link><guid>obd-rss-1</guid><pubDate>%s</pubDate><description>RSS 条目 OBD-RSS-1 的正文</description></item></channel></rss>`,
				time.Now().UTC().Format(time.RFC1123Z))
		}))
		top.Cleanup(feed.Close) // 订阅服务要活到整条流程结束，不随本步骤的子测试退出
		k.installRSS()
		cfg, _ := json.Marshal(map[string]any{"url": feed.URL + "/feed.xml"})
		k.apa(k.DEL, 0, "source", "add", "--id", rssID, "--type", "pull", "--command", "rss-source", "--schedule", "5s", "--timeout", "30",
			"--config", string(cfg), "--default-project", k.Projects["acc-home"], "--evidence", "登记 RSS 来源，默认归入 acc-home")
		s := k.sourceShow(rssID)
		if s.DefaultProject != k.Projects["acc-home"] || s.Outbound.Local != "full" || s.Outbound.External != nil {
			t.Fatalf("show 应是 default_project=acc-home、local=full、external 未选择: %+v", s)
		}
	})

	k.step("ACT-OBD-12 外部范围未选择且端点都在外部：记 outbound_unset，不外发", func(t *testing.T) {
		k.endpoints("external", "external")
		k.clear()
		var ev SrcEvent
		if !k.WaitFor(wait, func() bool {
			for _, e := range k.events(rssID, "&disposition=unjudged") {
				if strings.Contains(e.Subject, "OBD-RSS-1") {
					ev = e
					return true
				}
			}
			return false
		}) {
			t.Fatalf("OBD-RSS-1 没有记为未判定: %+v\n%s\n%s", k.events(rssID, ""), k.apa(k.OBS, 0, "source", "show", rssID).Stdout, k.ServiceLog())
		}
		if ev.Unjudged != "outbound_unset" || ev.Item != "" {
			t.Fatalf("应是 unjudged / outbound_unset、不进建议流: %+v", ev)
		}
		if n := len(requestsWith(k.clsLog(), "OBD-RSS-1")) + len(requestsWith(k.modelLog(), "OBD-RSS-1")); n != 0 {
			t.Fatalf("外部端点不应收到 OBD-RSS-1，实际 %d", n)
		}
	})

	k.step("ACT-OBD-13 健康视图、来源页与 apa doctor 提示去选外发范围", func(t *testing.T) {
		for name, raw := range map[string]string{
			"apa health":     k.apa(k.OBS, 0, "health").Stdout,
			"GET /v1/health": k.APIRaw("GET", "/v1/health", "", k.OBS).Body,
		} {
			sig := healthSignal(t, raw, "unjudged_backlog")
			if sig.OK || sig.Severity != "warning" || sig.outboundUnset() < 1 || !strings.Contains(sig.Next, "source page: choose the external outbound scope") {
				t.Fatalf("%s: unjudged_backlog 应是 warning、outbound_unset ≥ 1、next 指向来源页: %+v", name, sig)
			}
		}
		if h := healthHealthy(t, k.APIRaw("GET", "/v1/health", "", k.OBS).Body); !h {
			t.Fatalf("提醒级信号不应影响 healthy")
		}
		s := k.sourceShow(rssID) // 来源页读的就是这份登记：external 为空 = "未选择"并有去选的入口（PUT /settings/sources/{id}/outbound）
		if s.Outbound.External != nil {
			t.Fatalf("来源页应显示外部范围未选择: %+v", s.Outbound)
		}
		d := k.doctorSource(rssID)
		if d.Status != "warning" || !strings.Contains(d.Why, "outbound scope is not chosen") {
			t.Fatalf("doctor 应对 %s 给出"+"外发范围未选"+"的提醒级结论: %+v", rssID, d)
		}
	})

	k.step("ACT-OBD-14 选定外发范围后自动重判", func(t *testing.T) {
		k.scope(rssID, "full", "excerpt")
		k.clear()
		var ev SrcEvent
		advanced := false
		if !k.WaitFor(wait, func() bool {
			for _, e := range k.events(rssID, "") {
				if strings.Contains(e.Subject, "OBD-RSS-1") && e.Disposition != "unjudged" && e.Disposition != "pending" {
					ev = e
					return true
				}
			}
			if !advanced && len(k.clsLog()) == 0 { // 后台维护按测试时钟到期：推进一次，不需要本人手动触发
				k.TCAdvance(600)
				advanced = true
			}
			return false
		}) {
			t.Fatalf("OBD-RSS-1 没有自动重判\n%s", k.ServiceLog())
		}
		if ev.Disposition != "proposed" || ev.Item == "" {
			t.Fatalf("重判后应按初筛结果进建议流: %+v", ev)
		}
		if !k.requeuedBySystem(ev.ID) {
			t.Fatalf("事件流应有一条操作者为 system 的 inbound.requeued")
		}
		cls := requestsWith(k.clsLog(), "OBD-RSS-1")
		k.contains("CLS_STUB", cls, "OBD-RSS-1")
		k.lacks("CLS_STUB", cls, "feed.example.org/1")
	})

	k.step("ACT-OBD-15 来源页显示两个范围，apa doctor 报告向外部端点发送全文", func(t *testing.T) {
		const note = "full text of this source is being sent to an external model endpoint"
		k.scope(obdSrc, "full", "full")
		for _, s := range []string{obdSrc, rssID} {
			if o := k.sourceShow(s).Outbound; o.Local == "" {
				t.Fatalf("来源页应显示 %s 的 local 范围", s)
			}
		}
		if d := k.doctorSource(obdSrc); !strings.Contains(d.Why, note) {
			t.Fatalf("external=full 时 doctor 应点名 %s: %+v", obdSrc, d)
		}
		if d := k.doctorSource(rssID); strings.Contains(d.Why, note) {
			t.Fatalf("excerpt 的来源不应被点名: %+v", d)
		}
		k.scope(obdSrc, "full", "excerpt")
		if d := k.doctorSource(obdSrc); strings.Contains(d.Why, note) {
			t.Fatalf("改为 excerpt 后这一条应消失: %+v", d)
		}
	})

	open := func() string { return k.Projects["acc-open"] }
	k.step("ACT-OBD-16 项目的 capture_outbound 未选择：对话增量不送外部端点", func(t *testing.T) {
		k.webOK("PATCH", "/v1/projects/"+open(), `{"capture":true}`, 200)
		k.clear()
		k.capture(open(), "CAP-MARK 下周把合同发给小王")
		// 捕获是异步处理：先等这段对话在内部来源 capture:<项目> 里被处置（按 outbound_unset 留作未判定），再断言替身没有收到它。
		var ev SrcEvent
		if !k.WaitFor(wait, func() bool {
			for _, e := range k.events("capture:"+open(), "") {
				if e.Disposition == "unjudged" {
					ev = e
					return true
				}
			}
			return false
		}) {
			t.Fatalf("对话增量没有被记为未判定\n%s", k.ServiceLog())
		}
		if ev.Unjudged != "outbound_unset" {
			t.Fatalf("应因 capture_outbound 未选择而未判定: %+v", ev)
		}
		if n := len(requestsWith(k.clsLog(), "CAP-MARK")) + len(requestsWith(k.modelLog(), "CAP-MARK")); n != 0 {
			t.Fatalf("对话原文不应送往外部端点，实际 %d 个请求", n)
		}
	})

	k.step("ACT-OBD-17 对话捕获按项目 capture_outbound 裁剪", func(t *testing.T) {
		k.webOK("PUT", "/v1/settings/projects/"+open()+"/capture-outbound", `{"value":"excerpt"}`, 200)
		k.Cls.PYes(modelstub.Key{Contains: "CAP2-"}, 0.9)
		yes := true
		k.ModelStub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "CAP2-"}, Answer: "yes", Sufficient: &yes})
		k.ModelStub.When("CAP2-", modelstub.Reply{Content: `{"candidates":[{"title":"CAP2 候选","summary":"从对话抽出","kind":"task"}]}`})
		k.clear()
		k.capture(open(), body2000("CAP2-HEAD", "CAP2-MID", "CAP2-TAIL"))
		if !k.WaitFor(wait, func() bool {
			return len(requestsWith(k.clsLog(), "CAP2-HEAD")) > 0 && len(k.extractRequests("CAP2-HEAD")) > 0
		}) {
			t.Fatalf("预筛或抽取请求没有出现\nCLS=%d MODEL=%d\n%s", len(k.clsLog()), len(k.modelLog()), k.ServiceLog())
		}
		k.contains("capture-prescreen", requestsWith(k.clsLog(), "CAP2-"), "CAP2-HEAD")
		k.contains("triage-extract", k.extractRequests("CAP2-"), "CAP2-HEAD")
		k.lacks("capture-prescreen", requestsWith(k.clsLog(), "CAP2-"), "CAP2-MID", "CAP2-TAIL")
		k.lacks("triage-extract", k.extractRequests("CAP2-"), "CAP2-MID", "CAP2-TAIL")
	})

	k.step("ACT-OBD-18 v0.1 账本升级后已有来源：外部范围未选择、默认项目为空", func(t *testing.T) {
		fx := k.LedgerFixture(2)
		base := k.serveFixture(fx.Home)
		obs := fx.M.Tokens["observer"]
		var list struct {
			Sources []struct {
				ID string `json:"id"`
			} `json:"sources"`
		}
		r := k.fixtureGet(base, "/v1/sources", obs)
		if err := json.Unmarshal([]byte(r), &list); err != nil || len(list.Sources) == 0 {
			t.Fatalf("夹具应有来源: %v %s", err, r)
		}
		for _, s := range list.Sources {
			var sh struct {
				DefaultProject string `json:"default_project"`
				Outbound       struct {
					Local    string  `json:"local"`
					External *string `json:"external"`
				} `json:"outbound"`
			}
			if err := json.Unmarshal([]byte(k.fixtureGet(base, "/v1/sources/"+s.ID, obs)), &sh); err != nil {
				t.Fatal(err)
			}
			if sh.DefaultProject != "" || sh.Outbound.Local != "full" || sh.Outbound.External != nil {
				t.Fatalf("来源 %s 升级后应是 default_project 空、local=full、external 未选择: %+v", s.ID, sh)
			}
		}
	})

	k.step("ACT-OBD-19 恢复缺省端点接线", func(t *testing.T) {
		k.endpoints("local", "local")
		k.webOK("PATCH", "/v1/projects/"+open(), `{"capture":false}`, 200)
		for _, s := range []string{obdSrc, rssID} {
			k.apa(k.DEL, 0, "source", "disable", s, "--evidence", "流程结束，停用验收来源")
		}
		v := k.endpointView()
		if v.Judge.Locality != "local" || v.Generate.Locality != "local" {
			t.Fatalf("两个端点应恢复为 local: %+v", v)
		}
	})
}
