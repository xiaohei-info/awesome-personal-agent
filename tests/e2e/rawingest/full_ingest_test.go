//go:build e2e

package rawingest

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestFlowRawFullIngest 是验收 flow-raw-full-ingest（ACT-RAW-01…06）：来源事件的完整原文按部分入账，
// 单条读取逐字节读回，事件列表只带预览与清单，原文不可被任何入口改写。
func TestFlowRawFullIngest(t *testing.T) {
	k := setup(t)
	k.addStub("src_acc_raw", "mail", "full", "full")

	var bigID string
	var bigBody string

	k.step("ACT-RAW-01 200 KiB 正文的事件完整入账并逐字节读回", func(t *testing.T) {
		k.setCase("src_acc_raw", "big-body")
		want := asString(k.stubOut("big-body")[0]["detail"])
		if len(want) != 204800 {
			t.Fatalf("SRC_STUB 的 big-body 正文 %d 字节，期望 204800", len(want))
		}
		bigID = k.eventByStubID("src_acc_raw", "big-big-body").ID
		part := k.readEvent("", bigID, "body", 200)
		if part.Body == nil || len(*part.Body) != 204800 || sha([]byte(*part.Body)) != sha([]byte(want)) || *part.Body != want {
			t.Fatalf("?part=body 没有逐字节读回 200 KiB 正文（得到 %d 字节）", lenOf(part.Body))
		}
		bigBody = *part.Body
		whole := k.readEvent("", bigID, "", 200)
		eq(t, "content.state", whole.Content.State, "full")
		eq(t, "scope", whole.Scope, "full")
		eq(t, "content.bytes 含 body", whole.Content.Bytes >= 204800, true)
		b, h, ok := whole.Content.part("body")
		if !ok || b != 204800 || h != sha([]byte(want)) {
			t.Errorf("content.parts 的 body 项 = (%d, %s, %v)，期望 204800 字节、sha256 %s", b, h, ok, sha([]byte(want)))
		}
		for _, p := range whole.Content.Parts {
			if p.Part == "body" && p.MediaType == "" {
				t.Error("body 项缺 media_type")
			}
		}
		if whole.Body == nil || *whole.Body != want {
			t.Error("无 part 的单条读取没有回全文 body")
		}
		if whole.Trimmed != nil {
			t.Errorf("full 范围不应有 trimmed: %+v", whole.Trimmed)
		}
	})

	k.step("ACT-RAW-02 头部、附件清单、原始正文与逐条消息各自成部分", func(t *testing.T) {
		k.setCase("src_acc_raw", "mail-like")
		mail := k.stubOut("mail-like")[0]
		mailID := k.eventByStubID("src_acc_raw", "mail-like-1").ID
		whole := k.readEvent("", mailID, "", 200)
		eq(t, "mail-like content.state", whole.Content.State, "full")
		jsonEq(t, "headers", whole.Headers, mail["headers"])
		if to, _ := json.Marshal(mail["headers"].(map[string]any)["To"]); !strings.Contains(string(whole.Headers), string(to)) {
			t.Errorf("同名多值头 To 应为数组: %s", whole.Headers)
		}
		var atts []map[string]any
		if err := json.Unmarshal(whole.Attachments, &atts); err != nil || len(atts) != 2 {
			t.Fatalf("attachments = %s（%v）", whole.Attachments, err)
		}
		for i, a := range atts {
			wa := mail["attachments"].([]any)[i].(map[string]any)
			for _, f := range []string{"name", "media_type", "ref"} {
				if a[f] != wa[f] || a[f] == nil {
					t.Errorf("附件 %d 的 %s = %v，期望 %v", i, f, a[f], wa[f])
				}
			}
			if a["size"] != wa["size"] {
				t.Errorf("附件 %d 的 size = %v，期望 %v", i, a["size"], wa["size"])
			}
			if _, has := a["blob"]; has {
				t.Errorf("附件 %d 在取回之前不应有 blob", i)
			}
		}
		orig := k.readEvent("", mailID, "original", 200).Original
		wo := mail["original"].(map[string]any)
		if orig == nil || orig.MediaType != "text/html" || orig.Text != wo["text"] {
			t.Errorf("original 与输出不一致（原样）: %+v", orig)
		}
		for _, name := range []string{"headers", "attachments", "original", "body"} {
			if b, h, ok := whole.Content.part(name); !ok || b == 0 || len(h) != 64 {
				t.Errorf("content.parts 缺 %s 的 bytes/sha256: (%d, %q, %v)", name, b, h, ok)
			}
		}

		k.setCase("src_acc_raw", "chat-two-things")
		chat := k.stubOut("chat-two-things")[0]
		chatID := k.eventByStubID("src_acc_raw", "chat-two-things-1").ID
		msgs := k.readEvent("", chatID, "messages", 200)
		jsonEq(t, "messages", msgs.Messages, chat["items"])
		for _, m := range chat["items"].([]any) { // 不截断：每条文字原样出现
			if !strings.Contains(string(msgs.Messages), asString(m.(map[string]any)["text"])) {
				t.Errorf("消息文字缺失: %v", m)
			}
		}
		if b, h, ok := k.readEvent("", chatID, "", 200).Content.part("messages"); !ok || b == 0 || len(h) != 64 {
			t.Errorf("content.parts 缺 messages: (%d, %q, %v)", b, h, ok)
		}
	})

	k.step("ACT-RAW-03 partial 自报被如实记录", func(t *testing.T) {
		k.setCase("src_acc_raw", "partial")
		out := k.stubOut("partial")[0]
		got := k.readEvent("", k.eventByStubID("src_acc_raw", "partial-1").ID, "", 200)
		eq(t, "content.state", got.Content.State, "partial")
		missing := out["content"].(map[string]any)["missing"].([]any)
		if len(got.Content.Missing) != len(missing) {
			t.Fatalf("content.missing = %+v，期望 %d 项", got.Content.Missing, len(missing))
		}
		for i, m := range missing {
			eq(t, "missing.part", got.Content.Missing[i].Part, asString(m.(map[string]any)["part"]))
			eq(t, "missing.reason", got.Content.Missing[i].Reason, asString(m.(map[string]any)["reason"]))
		}
		if got.Body == nil || *got.Body != asString(out["detail"]) {
			t.Error("partial 事件的其余部分（body）应照常保存")
		}
	})

	var itemID string
	k.step("ACT-RAW-04 事件列表与挂靠消息列表只带 2000 字符预览与原文清单", func(t *testing.T) {
		k.setCase("src_acc_raw", "big-body")
		var big listed
		for _, e := range k.events("src_acc_raw") {
			if e.ID == bigID {
				big = e
			}
		}
		checkPreview := func(where string, detail string, c content) {
			t.Helper()
			if n := utf8.RuneCountInString(detail); n == 0 || n > 2000 || !strings.HasPrefix(bigBody, detail) {
				t.Errorf("%s 的 detail 应是正文开头至多 2000 字符（实际 %d 字符）", where, n)
			}
			if b, _, ok := c.part("body"); !ok || b != 204800 || c.State != "full" || c.Bytes < 204800 {
				t.Errorf("%s 缺 content 清单: %+v", where, c)
			}
		}
		checkPreview("来源事件列表", big.Detail, big.Content)
		raw := k.APIRaw("GET", "/v1/sources/src_acc_raw/events?limit=500", "", k.OBS).Body
		var rawList struct {
			Events []map[string]json.RawMessage `json:"events"`
		}
		if err := json.Unmarshal([]byte(raw), &rawList); err != nil {
			t.Fatal(err)
		}
		for _, e := range rawList.Events {
			if _, has := e["messages"]; has {
				t.Errorf("列表不应返回聚合消息的内容: %s", e["id"])
			}
		}
		if len(raw) > 1<<20 {
			t.Errorf("事件列表 %d 字节：带了全文", len(raw))
		}

		// 事件未判定（无模型端点）：本人转为建议、接受成事项，再来一封同关联键的回信，使其挂靠。
		k.web("POST", "/v1/source-events/"+bigID+"/propose", `{}`, 200)
		for _, e := range k.events("src_acc_raw") {
			if e.ID == bigID {
				itemID = e.Item
			}
		}
		if itemID == "" {
			t.Fatal("转为建议后事件没有事项")
		}
		k.web("POST", "/v1/items/"+itemID+"/accept", `{}`, 200)
		k.setCase("src_acc_raw", "big-body-reply")
		reply := k.eventByStubID("src_acc_raw", "big-big-body-reply")
		k.until("回信挂靠到已接受的事项", func() bool {
			for _, e := range k.events("src_acc_raw") {
				if e.ID == reply.ID {
					reply = e
				}
			}
			return reply.Disposition == "attached" && reply.Item == itemID
		})
		var msgs struct {
			Messages []listed `json:"messages"`
		}
		r := k.APIRaw("GET", "/v1/items/"+itemID+"/messages", "", k.OBS)
		if r.Code != 200 || json.Unmarshal([]byte(r.Body), &msgs) != nil {
			t.Fatalf("GET /items/%s/messages: %d %.200s", itemID, r.Code, r.Body)
		}
		found := false
		for _, m := range msgs.Messages {
			if m.ID == reply.ID {
				found = true
				if n := utf8.RuneCountInString(m.Detail); n == 0 || n > 2000 {
					t.Errorf("挂靠消息 detail %d 字符，应至多 2000", n)
				}
				if b, _, ok := m.Content.part("body"); !ok || b != 204800 {
					t.Errorf("挂靠消息缺 content 清单: %+v", m.Content)
				}
			}
		}
		if !found {
			t.Fatalf("挂靠消息列表里没有回信 %s: %.300s", reply.ID, r.Body)
		}
		if len(r.Body) > 1<<20 {
			t.Errorf("挂靠消息列表 %d 字节：带了全文", len(r.Body))
		}
		full := k.readEvent("", reply.ID, "body", 200)
		if full.Body == nil || len(*full.Body) != 204800 {
			t.Errorf("单条读取应能拿到回信全文，得到 %d 字节", lenOf(full.Body))
		}
	})

	k.step("ACT-RAW-05 CLI 读单条来源事件", func(t *testing.T) {
		r := k.apa(k.OBS, 0, "source", "event", bigID)
		var out read
		if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
			t.Fatalf("apa source event 输出不是 JSON: %v\n%.300s", err, r.Stdout)
		}
		eq(t, "CLI 写明的范围", out.Scope, "full")
		if out.Event.ID != bigID || out.Body == nil || *out.Body != bigBody || out.Content.State != "full" {
			t.Errorf("CLI 输出缺事件、清单或正文: scope=%s state=%s", out.Scope, out.Content.State)
		}
		if out.Trimmed != nil {
			t.Errorf("full 范围的裁剪量应为空: %+v", out.Trimmed)
		}
		mailID := k.eventByStubID("src_acc_raw", "mail-like-1").ID
		var hdr read
		r = k.apa(k.OBS, 0, "source", "event", mailID, "--part", "headers")
		if err := json.Unmarshal([]byte(r.Stdout), &hdr); err != nil {
			t.Fatalf("--part headers 输出不是 JSON: %v", err)
		}
		if len(hdr.Headers) == 0 || hdr.Body != nil || hdr.Original != nil || len(hdr.Attachments) != 0 || len(hdr.Messages) != 0 {
			t.Errorf("--part headers 只应输出 headers: %.300s", r.Stdout)
		}
	})

	k.step("ACT-RAW-06 任何入口都不能改写来源原文", func(t *testing.T) {
		const edited = "改过的正文"
		k.apa(k.DEL, 0, "item", "update", itemID, "--body", edited)
		var it struct {
			Item struct {
				Body string `json:"body"`
			} `json:"item"`
		}
		if err := json.Unmarshal([]byte(k.APIRaw("GET", "/v1/items/"+itemID, "", k.OBS).Body), &it); err != nil || it.Item.Body != edited {
			t.Fatalf("事项正文应已改成 %q: %+v %v", edited, it, err)
		}
		again := k.readEvent("", bigID, "body", 200)
		if again.Body == nil || sha([]byte(*again.Body)) != sha([]byte(bigBody)) {
			t.Error("改事项正文影响了来源事件的原文")
		}
		// 没有写来源原文的路由：除读取外，/source-events 下只有转为建议与忽略。
		for _, m := range []string{"PUT", "PATCH", "DELETE"} {
			if r := k.Web(m, "/v1/source-events/"+bigID, `{"detail":"x"}`); r.Code < 400 {
				t.Errorf("%s /v1/source-events/{id} 返回 %d，不应存在", m, r.Code)
			}
		}
		for _, p := range []string{"body", "original", "content"} {
			if r := k.Web("PUT", "/v1/source-events/"+bigID+"?part="+p, `{"text":"x"}`); r.Code < 400 {
				t.Errorf("PUT ?part=%s 返回 %d", p, r.Code)
			}
		}
		doc, err := os.ReadFile("../../../docs/appendix-b-api-cli.md")
		if err != nil {
			t.Fatal(err)
		}
		// 附录 B：/source-events 下除读取外只有处置类 POST（转为建议、忽略、重判、放回、取回附件等），没有 PUT、PATCH、DELETE，
		// 也没有以原文各部分命名的写入口。
		route := regexp.MustCompile("`(GET|POST|PUT|PATCH|DELETE) (/source-events[^`]*)`")
		seen := 0
		for _, m := range route.FindAllStringSubmatch(string(doc), -1) {
			seen++
			if m[1] == "PUT" || m[1] == "PATCH" || m[1] == "DELETE" {
				t.Errorf("附录 B 里有写来源事件的路由: %s %s", m[1], m[2])
			}
			if m[1] == "POST" && regexp.MustCompile(`/(body|original|headers|attachments$|messages|content)\b`).MatchString(m[2]) {
				t.Errorf("附录 B 里有写来源原文的 POST 路由: %s", m[2])
			}
		}
		if seen == 0 {
			t.Error("附录 B 里没有扫到 /source-events 路由，检查口径失效")
		}
		again = k.readEvent("", bigID, "body", 200)
		if *again.Body != bigBody {
			t.Error("探测写入口之后原文被改变")
		}
	})
}

func lenOf(s *string) int {
	if s == nil {
		return -1
	}
	return len(*s)
}
