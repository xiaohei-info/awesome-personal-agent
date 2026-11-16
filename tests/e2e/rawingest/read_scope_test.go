//go:build e2e

package rawingest

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// scopeSources 是四种外发范围的来源（local 都为 full）。
var scopeSources = []struct{ id, external string }{
	{"src_acc_raw", "full"}, {"src_acc_raw_ex", "excerpt"}, {"src_acc_raw_meta", "meta"}, {"src_acc_raw_none", "none"},
}

// TestFlowRawReadScope 是验收 flow-raw-read-scope（ACT-RSC-01…07）：本人在 Web 读到全文，token 读到的原文不超过该来源对外部端点的外发范围，
// 运行凭据与 ext 密钥读不到；升级前落盘的事件（结构版本 7 夹具升级后）完整程度为 legacy 且照常可读。
func TestFlowRawReadScope(t *testing.T) {
	srcStubBin(t)
	e := baseline.Setup(t)
	e.EnableStubRuns()
	k := newKit(t, e)
	ids := map[string]string{} // 来源 id → 其 mail-like 事件 id
	var mail map[string]any

	k.step("ACT-RSC-01 准备四种外发范围的来源", func(t *testing.T) {
		mail = k.stubOut("mail-like")[0]
		for _, s := range scopeSources {
			k.addStub(s.id, "mail", "full", s.external) // 初始 case 即 mail-like
			ids[s.id] = k.eventByStubID(s.id, "mail-like-1").ID
		}
		if r := k.APIRaw("GET", "/v1/today", "", k.OBS); r.Code != 200 {
			t.Fatalf("OBS 应能调 GET /v1/today: %d", r.Code)
		}
	})

	k.step("ACT-RSC-02 Web 登录读到全文", func(t *testing.T) {
		got := k.readEvent("", ids["src_acc_raw_none"], "", 200)
		eq(t, "scope", got.Scope, "full")
		if got.Body == nil || *got.Body != asString(mail["detail"]) || got.Original == nil || got.Original.Text != asString(mail["original"].(map[string]any)["text"]) ||
			len(got.Headers) == 0 || len(got.Attachments) == 0 {
			t.Errorf("Web 登录应读到 body、original、headers、attachments 全文: %+v", got)
		}
		if got.Trimmed != nil {
			t.Errorf("Web 登录不应有 trimmed: %+v", got.Trimmed)
		}
	})

	k.step("ACT-RSC-03 token 按 excerpt 范围读到摘录", func(t *testing.T) {
		// excerpt 要有被裁的量：给 acc-raw-ex 再来一条 big-body 事件。
		k.setCase("src_acc_raw_ex", "big-body")
		big := k.eventByStubID("src_acc_raw_ex", "big-big-body").ID
		wantBody := asString(k.stubOut("big-body")[0]["detail"])
		ex := k.readEvent(k.OBS, big, "", 200)
		eq(t, "scope", ex.Scope, "excerpt")
		if ex.Body == nil || utf8.RuneCountInString(*ex.Body) != 500 || *ex.Body != wantBody[:500] {
			t.Errorf("excerpt 只回正文前 500 个字符，得到 %d", lenOf(ex.Body))
		}
		if ex.Original != nil || len(ex.Headers) != 0 || len(ex.Messages) != 0 {
			t.Errorf("excerpt 不回 original、headers、messages: %+v", ex)
		}
		if ex.Trimmed == nil || ex.Trimmed.TotalChars != 204800 || ex.Trimmed.ReturnedChars != 500 {
			t.Errorf("trimmed 应写明 total_chars=204800、returned_chars=500: %+v", ex.Trimmed)
		}
		if ex.Event.Summary == "" && ex.Event.Subject == "" {
			t.Error("excerpt 应回摘要")
		}
		// 邮件事件（短正文）同样不回被排除的部分。
		m := k.readEvent(k.OBS, ids["src_acc_raw_ex"], "", 200)
		eq(t, "mail-like 的 scope", m.Scope, "excerpt")
		if m.Original != nil || len(m.Headers) != 0 {
			t.Errorf("excerpt 不回 original、headers: %+v", m)
		}
		// 全文范围的来源：全部部分。
		full := k.readEvent(k.OBS, ids["src_acc_raw"], "", 200)
		eq(t, "acc-raw 的 scope", full.Scope, "full")
		if full.Body == nil || full.Original == nil || len(full.Headers) == 0 || len(full.Attachments) == 0 || full.Trimmed != nil {
			t.Errorf("full 范围应回全部部分且无 trimmed: %+v", full)
		}
		// 经 HTTP 头直接读一次（等价于验收里的 curl）。
		if r := k.APIRaw("GET", "/v1/source-events/"+big, "", k.OBS); r.Code != 200 || !strings.Contains(r.Body, `"scope":"excerpt"`) {
			t.Errorf("curl 等价读取: %d %.200s", r.Code, r.Body)
		}
	})

	k.step("ACT-RSC-04 token 在 meta、none 与未选择时只读到元信息", func(t *testing.T) {
		metaOnly := func(what string, got read) {
			t.Helper()
			eq(t, what+" 的 scope", got.Scope, "meta")
			if got.Body != nil || got.Original != nil || len(got.Headers) != 0 || len(got.Attachments) != 0 || len(got.Messages) != 0 {
				t.Errorf("%s 只应回元信息与 content 清单: %+v", what, got)
			}
			if got.Event.ID == "" || got.Content.State == "" || len(got.Content.Parts) == 0 {
				t.Errorf("%s 应回 event 元信息与 content 清单: %+v", what, got)
			}
		}
		for _, s := range []string{"src_acc_raw_meta", "src_acc_raw_none"} {
			metaOnly(s, k.readEvent(k.OBS, ids[s], "", 200))
			metaOnly(s+" ?part=body", k.readEvent(k.OBS, ids[s], "body", 200))
		}
		// 未选择外部范围的来源：结构版本 7 夹具升级后的 v0.1 来源。
		fx := newKit(t, baseline.New(t))
		f := fx.LedgerFixture(7)
		fx.ServeFixture(f, map[string]string{"unjudged_rejudge_days": "0"})
		src := f.M.ExternalUnselectedSource
		var out struct {
			Outbound struct {
				External *string `json:"external"`
			} `json:"outbound"`
		}
		if err := json.Unmarshal([]byte(fx.apa(fx.OBS, 0, "source", "show", src).Stdout), &out); err != nil || out.Outbound.External != nil {
			t.Fatalf("夹具来源 %s 的外部外发范围应是未选择: %+v %v", src, out, err)
		}
		evs := fx.events(src)
		if len(evs) == 0 {
			t.Fatalf("夹具来源 %s 没有事件", src)
		}
		metaOnly("未选择外部范围的来源", fx.readEvent(fx.OBS, evs[0].ID, "", 200))

		// 升级前落盘的事件：完整程度 legacy，照常可读（正文一字不变）。
		if len(f.M.Mail16K) == 0 {
			t.Fatal("夹具没有 16 KiB 邮件事件")
		}
		for _, m := range f.M.Mail16K {
			got := fx.readEvent("", m.ID, "", 200)
			eq(t, m.ID+" 的 content.state", got.Content.State, "legacy")
			if got.Body == nil || len(*got.Body) != 16384 || sha([]byte(*got.Body)) != m.BodySHA256 {
				t.Errorf("legacy 事件 %s 的正文应照常读回 16384 字节且哈希与升级前一致", m.ID)
			}
		}
	})

	k.step("ACT-RSC-05 运行凭据与 ext 密钥不能读来源原文", func(t *testing.T) {
		_, _, env := k.StubRun("hang") // 运行期间凭据有效：在超时（4 秒）之前用完
		tok := env["APA_TOKEN"]
		if tok == "" {
			t.Fatalf("取不到运行凭据: %v", env)
		}
		if r := k.APIRaw("GET", "/v1/today", "", tok); r.Code != 200 {
			t.Fatalf("运行凭据应能调 today: %d", r.Code)
		}
		id := ids["src_acc_raw"]
		eq(t, "运行凭据读来源事件的 HTTP 状态", k.APIRaw("GET", "/v1/source-events/"+id, "", tok).Code, 403)
		eq(t, "ext 密钥读来源事件的 HTTP 状态", k.APIRaw("GET", "/v1/source-events/"+id, "", k.CHAN).Code, 403)
		eq(t, "apa source event 的退出码（运行凭据）", k.Apa(tok, "", "source", "event", id).Code, exitDenied)
	})

	k.step("ACT-RSC-06 已接受事项上的挂靠消息按挂靠消息权限读，不按外发范围裁剪", func(t *testing.T) {
		src := "src_acc_raw_none"
		item := ""
		for _, ev := range k.events(src) {
			if ev.ID == ids[src] {
				item = ev.Item
			}
		}
		if item == "" { // 未判定的事件由本人转为建议；规则已直接转成建议的省掉这一步
			k.web("POST", "/v1/source-events/"+ids[src]+"/propose", `{}`, 200)
			for _, ev := range k.events(src) {
				if ev.ID == ids[src] {
					item = ev.Item
				}
			}
		}
		if item == "" {
			t.Fatal("转为建议后事件没有事项")
		}
		k.web("POST", "/v1/items/"+item+"/accept", `{}`, 200)
		reply := map[string]any{}
		for kk, v := range mail {
			reply[kk] = v
		}
		reply["id"], reply["subject"], reply["occurred_at"] = "mail-like-reply", "Re: Quarterly report", "2026-11-10T09:30:00Z"
		reply["detail"] = "Reply with the full text that must not be trimmed to metadata."
		k.setJSONL(src, []map[string]any{reply}, "reply-batch")
		var rid string
		k.until("回信挂靠到已接受的事项", func() bool {
			for _, ev := range k.events(src) {
				if ev.DedupeKey.EventID == "mail-like-reply" && ev.Disposition == "attached" && ev.Item == item {
					rid = ev.ID
					return true
				}
			}
			return false
		})
		got := k.readEvent(k.OBS, rid, "", 200)
		eq(t, "挂靠消息的 scope", got.Scope, "full")
		if got.Body == nil || *got.Body != reply["detail"] {
			t.Errorf("挂靠消息应回正文全文（不因 none 被裁成元信息）: %+v", got)
		}
	})

	k.step("ACT-RSC-07 对话捕获的内部来源原文只对 Web 登录开放", func(t *testing.T) {
		open := e.Projects["acc-open"]
		k.web("PATCH", "/v1/projects/"+open, `{"capture":true}`, 200)
		body, _ := json.Marshal(map[string]any{"text": "rsc-07 下周一提交报销单", "project": open})
		if r := k.APIRaw("POST", "/v1/capture", string(body), k.WRK); r.Code != 200 {
			t.Fatalf("提交对话增量: %d %s", r.Code, r.Body)
		}
		var id string
		k.until("出现对话捕获事件", func() bool {
			var out struct {
				Events []listed `json:"events"`
			}
			r := k.Web("GET", "/v1/sources/capture:"+open+"/events", "")
			if r.Code != 200 || json.Unmarshal([]byte(r.Body), &out) != nil || len(out.Events) == 0 {
				return false
			}
			id = out.Events[0].ID
			return true
		})
		web := k.readEvent("", id, "", 200)
		if web.Body == nil || !strings.Contains(*web.Body, "rsc-07 下周一提交报销单") || web.Scope != "full" {
			t.Errorf("Web 登录应读到捕获全文: %+v", web)
		}
		k.readEvent(k.OBS, id, "", 403)
	})
}
