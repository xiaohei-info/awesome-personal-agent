package applepim

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// casePlayer 回放 testdata 下一个录制 case：mail 用 messages-<case>.json 与 get-<case>.json，
// 提醒事项用 rem/<case>/<filter>.json，--list 按列表名过滤（工具自己做的事，测试边界替身）。
func casePlayer(t *testing.T, mailCase, remCase string) func(Config) Runner {
	t.Helper()
	read := func(parts ...string) []byte {
		b, err := os.ReadFile(filepath.Join(append([]string{"testdata"}, parts...)...))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	return func(Config) Runner {
		return func(_ context.Context, cli string, args ...string) ([]byte, []byte, error) {
			switch args[0] {
			case "messages":
				return read("messages-" + mailCase + ".json"), nil, nil
			case "get":
				return read("get-" + mailCase + ".json"), nil, nil
			case "items":
				b := read("rem", remCase, strings.TrimPrefix(argVal(args, "--filter="), "")+".json")
				list := argVal(args, "--list=")
				if list == "" {
					return b, nil, nil
				}
				var o struct {
					Reminders []map[string]any `json:"reminders"`
				}
				_ = json.Unmarshal(b, &o)
				kept := []map[string]any{}
				for _, r := range o.Reminders {
					if r["list"] == list {
						kept = append(kept, r)
					}
				}
				out, _ := json.Marshal(map[string]any{"success": true, "reminders": kept})
				return out, nil, nil
			}
			t.Fatalf("意外的子命令 %v", args)
			return nil, nil, nil
		}
	}
}

func TestMailLongHTMLNotTruncated(t *testing.T) {
	code, lines, _ := exec1(t, casePlayer(t, "long-html", ""), `{"config":{"kind":"mail","account":"Acct"}}`)
	if code != ExitOK || len(lines) != 2 {
		t.Fatalf("code=%d lines=%d", code, len(lines))
	}
	ev := lines[0]
	if ev["id"] != "long-html@example.test" { // 事件 id 算法不变：Message-ID
		t.Errorf("id: %v", ev["id"])
	}
	detail := ev["detail"].(string)
	if len(detail) <= 16<<10 || !strings.Contains(detail, "[link 259](https://example.test/p/259)") || !strings.Contains(detail, "- alpha") {
		t.Errorf("detail 应是未截断且保留结构的转换文本，len=%d", len(detail))
	}
	if strings.Contains(detail, "alert(1)") {
		t.Error("detail 不应含脚本")
	}
	orig := ev["original"].(map[string]any)
	if orig["media_type"] != "text/html" || !strings.Contains(orig["text"].(string), "<script>alert(1)</script>") {
		t.Errorf("original 应是原样 HTML: %v", orig["media_type"])
	}
	h := ev["headers"].(map[string]any)
	if h["From"] != "Sender <sender@example.test>" || h["Subject"] != "任务报告" || h["Message-Id"] != "<long-html@example.test>" || h["Cc"] != "team@example.test" {
		t.Errorf("headers: %v", h)
	}
	if rc, ok := h["Received"].([]any); !ok || len(rc) != 2 {
		t.Errorf("同名多值应成数组: %v", h["Received"])
	}
	atts := ev["attachments"].([]any)
	if len(atts) != 2 || atts[0].(map[string]any)["name"] != "report.pdf" || atts[0].(map[string]any)["size"] != float64(123456) || atts[1].(map[string]any)["inline"] != true {
		t.Errorf("attachments: %v", atts)
	}
	if _, partial := ev["content"]; partial {
		t.Errorf("取全了不应自报 partial: %v", ev["content"])
	}
}

// 升级前同一封信：id 仍是 Message-ID，与升级前的算法一致，所以重新拉到时被内核去重。
func TestMailLegacyIDUnchanged(t *testing.T) {
	_, lines, _ := exec1(t, casePlayer(t, "long-legacy", ""), `{"config":{"kind":"mail","account":"Acct"}}`)
	ev := lines[0]
	if ev["id"] != "legacy-long@example.test" || len(ev["detail"].(string)) <= 16<<10 {
		t.Errorf("legacy: id=%v detail=%d", ev["id"], len(ev["detail"].(string)))
	}
	if _, has := ev["original"]; has {
		t.Error("纯文本邮件不给 original")
	}
}

func TestMailPartialWhenToolGivesLittle(t *testing.T) {
	var calls [][]string
	_, lines, _ := exec1(t, replay(t, &calls), `{"config":{"kind":"mail","account":"Acct"}}`) // get-m1：只有 content
	ev := lines[0]
	if ev["detail"] != "正文一" {
		t.Errorf("detail: %v", ev["detail"])
	}
	c := ev["content"].(map[string]any)
	var parts []string
	for _, m := range c["missing"].([]any) {
		parts = append(parts, m.(map[string]any)["part"].(string))
	}
	if c["state"] != "partial" || strings.Join(parts, ",") != "headers,attachments,original" {
		t.Errorf("content: %v", c)
	}
	if ev["headers"].(map[string]any)["Message-Id"] != "m1@example.test" {
		t.Errorf("至少带 Message-ID: %v", ev["headers"])
	}
}

const rawMail = "From: =?UTF-8?B?5byg5LiJ?= <zs@example.test>\r\nTo: me@example.test\r\nSubject: Hi\r\nMessage-ID: <raw1@example.test>\r\n" +
	"MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=B1\r\n\r\n" +
	"--B1\r\nContent-Type: multipart/alternative; boundary=B2\r\n\r\n" +
	"--B2\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\nplain =E4=BD=A0\r\n" +
	"--B2\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>html <b>body</b></p>\r\n--B2--\r\n" +
	"--B1\r\nContent-Type: application/pdf; name=\"a.pdf\"\r\nContent-Disposition: attachment; filename=\"a.pdf\"\r\nContent-Transfer-Encoding: base64\r\n\r\nSGVsbG8gd29y\r\nbGQ=\r\n" +
	"--B1\r\nContent-Type: text/plain; charset=gb2312\r\n\r\nxxx\r\n--B1--\r\n"

// 工具只给原始邮件时，用标准库解析信头、MIME 结构与附件；解不了的字符集自报缺失。
func TestMailFromRawSource(t *testing.T) {
	mk := func(Config) Runner {
		return func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
			if args[0] == "messages" {
				return []byte(`{"messages":[{"messageId":"raw1@example.test","subject":"Hi","dateReceived":"2026-01-03T10:00:00Z","senderAddress":"zs@example.test"}]}`), nil, nil
			}
			b, _ := json.Marshal(map[string]any{"success": true, "message": map[string]any{"messageId": "raw1@example.test", "source": rawMail}})
			return b, nil, nil
		}
	}
	code, lines, _ := exec1(t, mk, `{"config":{"kind":"mail","account":"Acct"}}`)
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	ev := lines[0]
	h := ev["headers"].(map[string]any)
	if h["From"] != "张三 <zs@example.test>" || h["Subject"] != "Hi" {
		t.Errorf("headers: %v", h)
	}
	if ev["detail"] != "html **body**" || ev["original"].(map[string]any)["text"] != "<p>html <b>body</b></p>" {
		t.Errorf("detail/original: %v %v", ev["detail"], ev["original"])
	}
	atts := ev["attachments"].([]any)
	if len(atts) != 1 || atts[0].(map[string]any)["name"] != "a.pdf" || atts[0].(map[string]any)["size"] != float64(11) {
		t.Errorf("attachments: %v", atts)
	}
	c := ev["content"].(map[string]any)
	if c["state"] != "partial" || !strings.Contains(c["missing"].([]any)[0].(map[string]any)["reason"].(string), "gb2312") {
		t.Errorf("gb2312 部分应自报缺失: %v", c)
	}
}

func TestCalendarHeaders(t *testing.T) {
	mk := func(Config) Runner {
		return func(context.Context, string, ...string) ([]byte, []byte, error) {
			return []byte(`{"events":[{"id":"E1","title":"周会","status":"none","startDate":"2026-01-05T02:00:00Z","endDate":"2026-01-05T03:00:00Z","location":"会议室",
"organizer":{"name":"Boss","email":"boss@example.test"},"attendees":["a@example.test",{"name":"B","email":"b@example.test"}]}]}`), nil, nil
		}
	}
	_, lines, _ := exec1(t, mk, `{"config":{"kind":"calendar"}}`)
	h := lines[0]["headers"].(map[string]any)
	att := h["Attendee"].([]any)
	if h["Start"] != "2026-01-05T02:00:00Z" || h["End"] != "2026-01-05T03:00:00Z" || h["Location"] != "会议室" || h["Organizer"] != "Boss <boss@example.test>" ||
		h["Status"] != "none" || len(att) != 2 || att[1] != "B <b@example.test>" {
		t.Errorf("headers: %v", h)
	}
}

func remEvents(t *testing.T, remCase, cfg string) map[string]map[string]any {
	t.Helper()
	code, lines, errs := exec1(t, casePlayer(t, "", remCase), `{"config":`+cfg+`}`)
	if code != ExitOK {
		t.Fatalf("code=%d %s", code, errs)
	}
	out := map[string]map[string]any{}
	for _, l := range lines {
		if id, ok := l["id"].(string); ok {
			out[strings.SplitN(id, "|", 2)[0]] = l
		}
	}
	return out
}

func TestRemindersUndatedLists(t *testing.T) {
	evs := remEvents(t, "rem-undated", `{"kind":"reminders","undated_lists":["随手记"]}`)
	u := evs["RU1"]
	if u["type"] != "reminder.open" || u["occurred_at"] != "2026-01-02T08:00:00Z" || u["summary"] != "随手记" || u["correlation_keys"].([]any)[0] != "RU1" {
		t.Errorf("RU1: %v", u)
	}
	if h := u["headers"].(map[string]any); h["List"] != "随手记" || h["Priority"] != "5" || h["Due"] != nil {
		t.Errorf("RU1 headers: %v", h)
	}
	if d := evs["RD1"]; d["type"] != "reminder.due" || d["headers"].(map[string]any)["Due"] != "2026-01-10" || d["headers"].(map[string]any)["Priority"] != "1" {
		t.Errorf("RD1: %v", d)
	}
	if _, ok := evs["RW1"]; ok {
		t.Error("其他列表的无到期日提醒不应有事件")
	}
	if _, ok := evs["RC1"]; ok {
		t.Error("已完成的提醒不应有事件")
	}
	if len(evs) != 2 {
		t.Errorf("events: %v", evs)
	}
}

func TestRemindersWithoutUndatedListsUnchanged(t *testing.T) {
	evs := remEvents(t, "rem-undated", `{"kind":"reminders"}`)
	if len(evs) != 1 || evs["RD1"]["type"] != "reminder.due" {
		t.Errorf("不配 undated_lists 只应有 reminder.due: %v", evs)
	}
}

// 改标题：事件 id 变，关联键不变；补上到期日：类型变 reminder.due，关联键仍不变。
func TestRemindersRenameAndDateKeepCorrelationKey(t *testing.T) {
	cfg := `{"kind":"reminders","undated_lists":["随手记"]}`
	a, b, c := remEvents(t, "rem-undated", cfg)["RU1"], remEvents(t, "rem-undated-renamed", cfg)["RU1"], remEvents(t, "rem-undated-dated", cfg)["RU1"]
	if b["type"] != "reminder.open" || b["subject"] != "买燕麦奶" || b["id"] == a["id"] {
		t.Errorf("renamed: %v", b)
	}
	if c["type"] != "reminder.due" || c["occurred_at"] != "2026-01-15T00:00:00Z" {
		t.Errorf("dated: %v", c)
	}
	for _, e := range []map[string]any{a, b, c} {
		if ks := e["correlation_keys"].([]any); len(ks) != 1 || ks[0] != "RU1" {
			t.Errorf("关联键: %v", e)
		}
	}
}

func TestMailBatchStopsAtBudget(t *testing.T) {
	old := outputMB
	outputMB = 1 // 预算 768 KiB
	defer func() { outputMB = old }()
	big := strings.Repeat("x", 100<<10)
	var msgs []map[string]any
	for i := 0; i < 20; i++ {
		msgs = append(msgs, map[string]any{"messageId": string(rune('a'+i)) + "@example.test", "subject": "s", "dateReceived": "2026-01-03T10:0" + string(rune('0'+i/10)) + ":0" + string(rune('0'+i%10)) + "Z"})
	}
	mk := func(Config) Runner {
		return func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
			if args[0] == "messages" {
				b, _ := json.Marshal(map[string]any{"messages": msgs})
				return b, nil, nil
			}
			b, _ := json.Marshal(map[string]any{"message": map[string]any{"content": big}})
			return b, nil, nil
		}
	}
	code, lines, _ := exec1(t, mk, `{"config":{"kind":"mail","account":"Acct"}}`)
	n := len(lines) - 1
	if code != ExitOK || n < 6 || n >= 20 {
		t.Fatalf("应只输出一部分: code=%d n=%d", code, n)
	}
	if cur := lines[n]["cursor"]; cur != lines[n-1]["occurred_at"] {
		t.Errorf("游标应停在已输出的最后一条: %v vs %v", cur, lines[n-1]["occurred_at"])
	}
}
