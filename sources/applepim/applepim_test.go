package applepim

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
