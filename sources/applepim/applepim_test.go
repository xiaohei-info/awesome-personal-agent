package applepim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var fixedNow = func() time.Time { return time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC) }

// readOnly 是适配器允许调用的子命令；任何别的子命令都说明可能写数据。
var readOnly = map[string]bool{"messages": true, "get": true, "events": true, "items": true}

// replay 回放录制输出（测试边界替身，不连真实账号），并断言只读。
func replay(t *testing.T, calls *[][]string) func(Config) Runner {
	t.Helper()
	return func(Config) Runner {
		return func(_ context.Context, cli string, args ...string) ([]byte, []byte, error) {
			*calls = append(*calls, append([]string{cli}, args...))
			if !readOnly[args[0]] {
				t.Errorf("非只读子命令: %s %v", cli, args)
			}
			name := map[string]string{"messages": "messages.json", "events": "events.json"}[args[0]]
			switch args[0] {
			case "get":
				for _, a := range args {
					if id, ok := strings.CutPrefix(a, "--id="); ok {
						name = "get-" + strings.SplitN(id, "@", 2)[0] + ".json"
					}
				}
			case "items":
				name = "reminders-" + strings.TrimPrefix(argVal(args, "--filter="), "") + ".json"
			}
			b, err := os.ReadFile(filepath.Join("testdata", name))
			if err != nil {
				t.Fatalf("缺样本 %s: %v", name, err)
			}
			return b, nil, nil
		}
	}
}

func argVal(args []string, prefix string) string {
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v
		}
	}
	return ""
}

func exec1(t *testing.T, mk func(Config) Runner, in string) (int, []map[string]any, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), strings.NewReader(in), &out, &errb, mk, fixedNow)
	var lines []map[string]any
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if l == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("输出不是 JSONL: %q", l)
		}
		lines = append(lines, m)
	}
	return code, lines, errb.String()
}

func TestMail(t *testing.T) {
	var calls [][]string
	code, lines, _ := exec1(t, replay(t, &calls), `{"config":{"kind":"mail","account":"Acct"}}`)
	if code != ExitOK || len(lines) != 3 {
		t.Fatalf("code=%d lines=%v", code, lines)
	}
	// 最旧优先；无主题补占位；正文来自 get；末行游标 = 最新一封时间。
	if lines[0]["id"] != "m1@example.test" || lines[0]["subject"] != "(无主题)" || lines[0]["detail"] != "正文一" || lines[0]["sender"] != "a@example.test" {
		t.Errorf("第一封: %v", lines[0])
	}
	if lines[2]["cursor"] != "2026-01-03T10:00:00Z" {
		t.Errorf("游标: %v", lines[2])
	}
	if got := argVal(calls[0], "--since="); got != "2025-12-28T00:00:00Z" {
		t.Errorf("首次回看 7 天，since=%s", got)
	}
}

func TestMailCursorAndFutureDate(t *testing.T) {
	var calls [][]string
	_, lines, _ := exec1(t, replay(t, &calls), `{"cursor":"2026-01-01T00:00:00Z","config":{"kind":"mail","account":"Acct"}}`)
	if argVal(calls[0], "--since=") != "2026-01-01T00:00:00Z" {
		t.Errorf("应从游标继续: %v", calls[0])
	}
	// now(2026-01-04) 之后的邮件日期不能把游标带到未来。
	m := replay(t, &calls)
	_, lines, _ = exec1(t, func(c Config) Runner {
		r := m(c)
		return func(ctx context.Context, cli string, a ...string) ([]byte, []byte, error) {
			o, e, err := r(ctx, cli, a...)
			return bytes.ReplaceAll(o, []byte("2026-01-03T10:00:00.000Z"), []byte("2099-01-03T10:00:00.000Z")), e, err
		}
	}, `{"config":{"kind":"mail","account":"Acct"}}`)
	if lines[len(lines)-1]["cursor"] != "2026-01-04T00:00:00Z" {
		t.Errorf("游标越过当前时间: %v", lines[len(lines)-1])
	}
}

func TestCalendar(t *testing.T) {
	var calls [][]string
	code, lines, _ := exec1(t, replay(t, &calls), `{"config":{"kind":"calendar","calendars":["Cal"],"horizon_days":10}}`)
	if code != ExitOK || len(lines) != 4 {
		t.Fatalf("code=%d lines=%v", code, lines)
	}
	if lines[0]["type"] != "event.created" || lines[0]["summary"] != "2026-01-05 10:00 AM @ 会议室" || lines[0]["detail"] != "带材料" {
		t.Errorf("周会: %v", lines[0])
	}
	if !strings.HasPrefix(lines[0]["id"].(string), "SRC:uid-1|2026-01-05T02:00:00Z|sha256:") ||
		lines[0]["correlation_keys"].([]any)[0] != "SRC:uid-1|2026-01-05T02:00:00Z" {
		t.Errorf("id/关联键: %v", lines[0])
	}
	if lines[1]["type"] != "event.cancelled" || lines[2]["subject"] != "(无标题)" {
		t.Errorf("取消/无标题: %v %v", lines[1], lines[2])
	}
	if argVal(calls[0], "--calendar=") != "Cal" || argVal(calls[0], "--to=") != "2026-01-14T00:00:00Z" {
		t.Errorf("参数: %v", calls[0])
	}
}

func TestReminders(t *testing.T) {
	var calls [][]string
	code, lines, _ := exec1(t, replay(t, &calls), `{"config":{"kind":"reminders"}}`)
	if code != ExitOK || len(lines) != 5 { // R1、R2、R3、R0 + 游标；R2 在两个过滤里只出一次
		t.Fatalf("code=%d lines=%v", code, lines)
	}
	if lines[0]["type"] != "reminder.due" || lines[0]["occurred_at"] != "2026-01-10T00:00:00Z" || lines[0]["detail"] != "备注" {
		t.Errorf("R1: %v", lines[0])
	}
}

func TestFailures(t *testing.T) {
	fake := func(out, errOut string, err error) func(Config) Runner {
		return func(Config) Runner {
			return func(context.Context, string, ...string) ([]byte, []byte, error) {
				return []byte(out), []byte(errOut), err
			}
		}
	}
	cases := []struct {
		name string
		in   string
		mk   func(Config) Runner
		want int
	}{
		{"缺 kind", `{"config":{}}`, fake("", "", nil), ExitConfig},
		{"邮件缺 account", `{"config":{"kind":"mail"}}`, fake("", "", nil), ExitConfig},
		{"stdin 非 JSON", `x`, fake("", "", nil), ExitConfig},
		{"工具未安装", `{"config":{"kind":"reminders"}}`, fake("", "", &exec.Error{Name: "reminder-cli", Err: exec.ErrNotFound}), ExitConfig},
		{"系统授权被拒", `{"config":{"kind":"calendar"}}`, fake("", "Error: Calendar access denied", errors.New("exit 1")), ExitConfig},
		{"辅助应用路径授权被拒（stdout 空）", `{"config":{"kind":"calendar"}}`, fake("", "Calendar access denied", nil), ExitConfig},
		{"success=false", `{"config":{"kind":"reminders"}}`, fake(`{"success":false,"error":"Reminders authorization denied"}`, "", nil), ExitConfig},
		{"输出不是预期形态", `{"config":{"kind":"reminders"}}`, fake(`[1]`, "", nil), ExitConfig},
		{"其他失败为临时错误", `{"config":{"kind":"reminders"}}`, fake("", "boom", errors.New("exit 1")), ExitTemporary},
	}
	for _, c := range cases {
		code, lines, _ := exec1(t, c.mk, c.in)
		if code != c.want || len(lines) != 0 {
			t.Errorf("%s: code=%d lines=%v", c.name, code, lines)
		}
	}
}

// ACT-MAIL-13、ACT-MAIL-08：桌面邮件应用适配器同样产出线程关联键。get 的 message 带
// references/inReplyTo；关联键写法与事件 id 一致（messageId 不带尖括号）。
func TestMailThreadCorrelationKeys(t *testing.T) {
	var calls [][]string
	base := replay(t, &calls)
	mk := func(c Config) Runner {
		r := base(c)
		return func(ctx context.Context, cli string, a ...string) ([]byte, []byte, error) {
			if a[0] == "messages" {
				b, err := os.ReadFile("testdata/messages-thread.json")
				if err != nil {
					t.Fatal(err)
				}
				return b, nil, nil
			}
			return r(ctx, cli, a...)
		}
	}
	code, lines, _ := exec1(t, mk, `{"config":{"kind":"mail","account":"Acct"}}`)
	if code != ExitOK || len(lines) != 4 {
		t.Fatalf("code=%d lines=%v", code, lines)
	}
	for i, id := range []string{"mid-p1@example.test", "mid-p2@example.test", "mid-p3@example.test"} {
		ks, _ := lines[i]["correlation_keys"].([]any)
		if lines[i]["id"] != id || len(ks) != 1 || ks[0] != "thread:mid-p1@example.test" {
			t.Errorf("%s: %v", id, lines[i])
		}
	}
}

// 离开未完成集合的提醒逐条按 id 核实：已完成的发 reminder.completed（时刻取完成时间）；仍未完成（移出拉取范围）
// 或找不到的发 reminder.out_of_view，类型本身不表示完成。其他读失败整轮失败、不出游标。
func TestRemindersLeavingTheOpenSetAreVerified(t *testing.T) {
	var calls [][]string
	base := replay(t, &calls)
	transient := false
	mk := func(c Config) Runner {
		r := base(c)
		return func(ctx context.Context, cli string, a ...string) ([]byte, []byte, error) {
			if a[0] != "get" {
				return r(ctx, cli, a...)
			}
			switch argVal(a, "--id=") {
			case "RX", "RY": // 录制样本（MEAS-REM-GET）
				return r(ctx, cli, a...)
			case "RZ":
				if transient {
					return nil, []byte("boom"), errors.New("exit 1")
				}
				stderr, err := os.ReadFile("testdata/get-notfound.stderr")
				if err != nil {
					t.Fatal(err)
				}
				return nil, stderr, errors.New("exit status 1")
			}
			t.Fatalf("unexpected get %v", a)
			return nil, nil, nil
		}
	}
	in := `{"config":{"kind":"reminders"},"cursor":"{\"scope\":\"[null,null]\",\"open\":{\"R1\":\"x\",\"RX\":\"旧提醒\",\"RY\":\"改了到期日\",\"RZ\":\"删了的\"}}"}`
	code, lines, _ := exec1(t, mk, in)
	got := map[string]map[string]any{}
	for _, l := range lines {
		if k, _ := l["correlation_keys"].([]any); len(k) == 1 && l["type"] != "reminder.due" && l["type"] != "reminder.open" {
			got[k[0].(string)] = l
		}
	}
	if code != ExitOK || len(got) != 3 || got["R1"] != nil {
		t.Fatalf("code=%d left-open events: %v", code, got)
	}
	if x := got["RX"]; x["type"] != "reminder.completed" || x["occurred_at"] != "2026-01-03T09:30:00Z" || x["subject"] != "旧提醒" {
		t.Errorf("completed: %v", x)
	}
	for _, id := range []string{"RY", "RZ"} {
		if x := got[id]; x["type"] != "reminder.out_of_view" || strings.Contains(x["type"].(string), "complet") || strings.Contains(x["type"].(string), "close") {
			t.Errorf("%s only left the view: %v", id, x)
		}
	}
	var cur struct{ Open map[string]string }
	if err := json.Unmarshal([]byte(lines[len(lines)-1]["cursor"].(string)), &cur); err != nil || cur.Open["R1"] == "" || cur.Open["RX"] != "" {
		t.Fatalf("cursor must list the open reminders: %v %v", err, lines[len(lines)-1])
	}
	transient = true
	if code, lines, _ := exec1(t, mk, in); code != ExitTemporary || len(lines) != 0 {
		t.Fatalf("a failed verification must fail the poll without a cursor: code=%d lines=%v", code, lines)
	}
}

// 首次、旧格式游标、拉取范围（lists、undated_lists）变了：这一轮只建立基线，不把旧集合报成离开。
func TestRemindersBaselineOnlyWhenNotComparable(t *testing.T) {
	for name, cursor := range map[string]string{
		"首次":    ``,
		"旧时间游标": `2026-01-01T00:00:00Z`,
		"范围变了":  `{"scope":"[[\"Work\"],null]","open":{"RX":"别的列表里的"}}`,
	} {
		var calls [][]string
		b, _ := json.Marshal(map[string]any{"config": map[string]any{"kind": "reminders"}, "cursor": cursor})
		_, lines, _ := exec1(t, replay(t, &calls), string(b))
		for _, l := range lines {
			if typ, _ := l["type"].(string); typ != "" && typ != "reminder.due" && typ != "reminder.open" {
				t.Errorf("%s: only the baseline is set, got %v", name, l)
			}
		}
		for _, c := range calls {
			if c[1] == "get" {
				t.Errorf("%s: nothing to verify, got %v", name, c)
			}
		}
	}
}

// 每轮至多核实 maxVerify 条；没核实的仍留在游标里，下一轮再核，不会被当成已处理。
func TestRemindersVerifyAtMostMaxPerPoll(t *testing.T) {
	var calls [][]string
	base := replay(t, &calls)
	gets := 0
	mk := func(c Config) Runner {
		r := base(c)
		return func(ctx context.Context, cli string, a ...string) ([]byte, []byte, error) {
			if a[0] != "get" {
				return r(ctx, cli, a...)
			}
			gets++
			return []byte(`{"success":true,"reminder":{"id":"` + argVal(a, "--id=") + `","isCompleted":true}}`), nil, nil
		}
	}
	open := map[string]string{"R1": "x"}
	for i := range maxVerify + 2 {
		open[fmt.Sprintf("G%02d", i)] = "离开的"
	}
	cur, _ := json.Marshal(map[string]any{"scope": "[null,null]", "open": open})
	in, _ := json.Marshal(map[string]any{"config": map[string]any{"kind": "reminders"}, "cursor": string(cur)})
	code, lines, _ := exec1(t, mk, string(in))
	var c struct{ Open map[string]string }
	_ = json.Unmarshal([]byte(lines[len(lines)-1]["cursor"].(string)), &c)
	if code != ExitOK || gets != maxVerify || c.Open["G00"] != "" || c.Open[fmt.Sprintf("G%02d", maxVerify)] == "" || c.Open[fmt.Sprintf("G%02d", maxVerify+1)] == "" {
		t.Fatalf("code=%d gets=%d cursor=%v", code, gets, c.Open)
	}
}

// 代码托管平台的通知邮件带上它所说 PR 的规范链接（附录 D.2）：原信与评审回信同一个链接，别的邮件不带。
func TestMailNotificationLink(t *testing.T) {
	const gh = "notifications@github.com"
	list := `{"success":true,"messages":[
{"dateReceived":"2026-01-03T12:00:00.000Z","messageId":"org/app/pull/7/review/1@github.com","senderAddress":"` + gh + `","subject":"Re: [org/app] fix: 登录 (PR #7)"},
{"dateReceived":"2026-01-03T10:00:00.000Z","messageId":"org/app/pull/7@github.com","senderAddress":"` + gh + `","subject":"[org/app] fix: 登录 (PR #7)"},
{"dateReceived":"2026-01-02T10:00:00.000Z","messageId":"m1@example.test","senderAddress":"a@example.test","subject":"报价"}]}`
	mk := func(Config) Runner {
		return func(_ context.Context, _ string, a ...string) ([]byte, []byte, error) {
			if a[0] == "messages" {
				return []byte(list), nil, nil
			}
			id := strings.TrimPrefix(argVal(a, "--id="), "")
			refs := ""
			if strings.Contains(id, "/review/") {
				refs = "<org/app/pull/7@github.com>"
			}
			return []byte(`{"success":true,"message":{"messageId":"` + id + `","content":"正文","references":"` + refs + `"}}`), nil, nil
		}
	}
	code, lines, _ := exec1(t, mk, `{"config":{"kind":"mail","account":"Acct"}}`)
	if code != ExitOK || len(lines) != 4 {
		t.Fatalf("code=%d lines=%v", code, lines)
	}
	want := map[string]any{"m1@example.test": nil, "org/app/pull/7@github.com": "https://github.com/org/app/pull/7", "org/app/pull/7/review/1@github.com": "https://github.com/org/app/pull/7"}
	for _, l := range lines[:3] {
		if l["link"] != want[l["id"].(string)] {
			t.Errorf("%s: link %v", l["id"], l["link"])
		}
	}
}

// 统一时间字段（设计 §9.2）：提醒的到期只放 due_at（只到日的保留日期精度，带时刻的转 RFC3339）；
// occurred_at 仍是原来那个时间，不因新字段改变含义。
func TestRemindersCarryDueAt(t *testing.T) {
	var calls [][]string
	code, lines, _ := exec1(t, replay(t, &calls), `{"config":{"kind":"reminders"}}`)
	if code != ExitOK {
		t.Fatalf("code=%d", code)
	}
	want := map[string]string{"交材料": "2026-01-10", "打电话": "2026-01-11T01:00:00Z", "已过期": "2025-12-30"}
	seen := 0
	for _, l := range lines {
		subject, _ := l["subject"].(string)
		if w, ok := want[subject]; ok {
			if l["due_at"] != w {
				t.Errorf("%s: due_at %v, want %s", subject, l["due_at"], w)
			}
			seen++
		}
		if subject == "带时间" {
			if s, _ := l["due_at"].(string); !strings.HasPrefix(s, "2026-01-12T15:00:00") {
				t.Errorf("带时刻的到期应转 RFC3339: %v", l["due_at"])
			}
		}
	}
	if seen != len(want) {
		t.Fatalf("due_at checked for %d reminders, want %d", seen, len(want))
	}
}

// 日历起止只放 starts_at、ends_at；全天日程给当地日期并标 all_day，定时日程原样给 RFC3339。
func TestCalendarCarriesStartsAndEnds(t *testing.T) {
	var calls [][]string
	code, lines, _ := exec1(t, replay(t, &calls), `{"config":{"kind":"calendar","calendars":["Cal"],"horizon_days":10}}`)
	if code != ExitOK || len(lines) != 4 {
		t.Fatalf("code=%d lines=%v", code, lines)
	}
	if !strings.HasPrefix(lines[0]["starts_at"].(string), "2026-01-05T02:00:00") || !strings.HasPrefix(lines[0]["ends_at"].(string), "2026-01-05T03:00:00") {
		t.Errorf("定时日程的起止: %v %v", lines[0]["starts_at"], lines[0]["ends_at"])
	}
	if _, ok := lines[0]["all_day"]; ok {
		t.Errorf("定时日程不应带 all_day: %v", lines[0])
	}
	allDay := lines[2]
	start, _ := time.Parse(time.RFC3339Nano, "2026-01-07T16:00:00Z")
	if allDay["all_day"] != true || allDay["starts_at"] != start.In(time.Local).Format(time.DateOnly) {
		t.Errorf("全天日程: %v", allDay)
	}
	if _, ok := allDay["ends_at"].(string); !ok {
		t.Errorf("全天日程也应带 ends_at: %v", allDay)
	}
}
