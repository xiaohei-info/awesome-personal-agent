package mail

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

var fixedNow = func() time.Time { return time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC) }

// replay 按版本样本目录回放录制输出（测试边界替身，不连真实账号），并断言只读、argv 形态正确。
func replay(t *testing.T, dir string, calls *[][]string) Runner {
	t.Helper()
	return func(_ context.Context, args []string) ([]byte, []byte, error) {
		*calls = append(*calls, args)
		joined := " " + strings.Join(args, " ") + " "
		for _, bad := range []string{" send ", " flag ", " move ", " delete ", " copy ", " add ", " remove ", " save ", " write ", " reply ", " forward "} {
			if strings.Contains(joined, bad) {
				t.Errorf("出现写操作参数: %v", args)
			}
		}
		read := func(name string) ([]byte, []byte, error) {
			b, err := os.ReadFile(filepath.Join("testdata", dir, name))
			if err != nil {
				t.Fatalf("缺样本 %s: %v", name, err)
			}
			return b, nil, nil
		}
		switch {
		case len(args) == 1 && args[0] == "--version":
			return read("version.txt")
		case args[0] == "envelope" && args[1] == "list":
			return read("list.json")
		case args[0] == "message" && args[1] == "read":
			if !strings.Contains(joined, " --preview ") {
				t.Errorf("message read 缺 --preview，会改已读标记: %v", args)
			}
			return read("read-" + args[len(args)-len(jsonFlagFor(dir))-1] + ".json")
		}
		t.Fatalf("意外调用 %v", args)
		return nil, nil, nil
	}
}

func jsonFlagFor(dir string) []string {
	for _, vr := range Supported {
		if vr.Sample == dir {
			return vr.JSONFlag
		}
	}
	return nil
}

func exec1(t *testing.T, r Runner, input string) (int, string, string) {
	t.Helper()
	var o, e bytes.Buffer
	code := run(context.Background(), strings.NewReader(input), &o, &e, r, fixedNow)
	return code, o.String(), e.String()
}

const in0 = `{"source_id":"src_m","run_id":"r","config":{"account":"work"},"cursor":"%s"}`

func input(cursor string) string { return strings.Replace(in0, "%s", cursor, 1) }

// ACT-MAIL-02、ACT-MAIL-03、ACT-MAIL-08：每个声明版本的录制样本 → 标准事件；id 取 Message-ID；
// 不设 sender_verified；只读；JSON 标志随版本变化。
func TestEveryDeclaredVersionConvertsRecordedSample(t *testing.T) {
	for _, vr := range Supported {
		t.Run(vr.Sample, func(t *testing.T) {
			var calls [][]string
			code, out, errs := exec1(t, replay(t, vr.Sample, &calls), input(""))
			if code != ExitOK {
				t.Fatalf("exit=%d %s", code, errs)
			}
			lines := strings.Split(strings.TrimSpace(out), "\n")
			if n := len(lines); n < 3 {
				t.Fatalf("lines=%v", lines)
			}
			var first map[string]any
			_ = json.Unmarshal([]byte(lines[0]), &first)
			if first["id"] != "<old-1@example.com>" || first["type"] != "mail.received" || first["sender"] != "bob@example.com" || first["detail"] != "周报正文" {
				t.Fatalf("first=%v", first)
			}
			if _, ok := first["sender_verified"]; ok {
				t.Fatal("不应设置 sender_verified")
			}
			var inv map[string]any
			_ = json.Unmarshal([]byte(lines[1]), &inv)
			if inv["id"] != "<inv-42@example.com>" || inv["occurred_at"] != "2026-01-01T17:30:00Z" && inv["occurred_at"] != "2026-01-02T01:30:00Z" {
				t.Fatalf("inv=%v", inv)
			}
			var cur map[string]string
			if err := json.Unmarshal([]byte(lines[len(lines)-1]), &cur); err != nil || cur["cursor"] == "" {
				t.Fatalf("末行不是游标: %q", lines[len(lines)-1])
			}
			flag := strings.Join(vr.JSONFlag, " ")
			for _, c := range calls[1:] {
				if !strings.HasSuffix(strings.Join(c, " "), flag) {
					t.Errorf("调用缺少该版本的 JSON 标志 %q: %v", flag, c)
				}
			}
			if strings.Join(calls[1], " ") != "envelope list -a work -f INBOX --page-size 50 "+flag {
				t.Errorf("list argv=%v", calls[1])
			}
		})
	}
}

// 游标之前的邮件被跳过；等于游标的保留；游标推进到最新邮件时间。
func TestCursorFiltersAndAdvances(t *testing.T) {
	var calls [][]string
	code, out, _ := exec1(t, replay(t, "2.0.0", &calls), input("2026-01-02T01:30:00Z"))
	if code != ExitOK {
		t.Fatal(code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 || !strings.Contains(lines[0], "inv-42") || lines[1] != `{"cursor":"2026-01-02T01:30:00Z"}` {
		t.Fatalf("out=%s", out)
	}
}

// 无新邮件时游标保持不变。
func TestNoNewMailKeepsCursor(t *testing.T) {
	var calls [][]string
	code, out, _ := exec1(t, replay(t, "2.0.0", &calls), input("2026-06-01T00:00:00Z"))
	if code != ExitOK || strings.TrimSpace(out) != `{"cursor":"2026-06-01T00:00:00Z"}` {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

func failing(version string, stderr string, runErr error, failOn string) Runner {
	return func(_ context.Context, args []string) ([]byte, []byte, error) {
		if args[0] == "--version" {
			if failOn == "version" {
				return nil, []byte(stderr), runErr
			}
			return []byte(version), nil, nil
		}
		return nil, []byte(stderr), runErr
	}
}

// ACT-MAIL-04、05、06：未安装、版本越界、认证失败 → 退出码 2，stdout 为空（无新游标）。
func TestToolFailuresExit2WithoutCursor(t *testing.T) {
	notFound := &exec.Error{Name: "himalaya", Err: exec.ErrNotFound}
	cases := map[string]Runner{
		"未安装":    failing("", "", notFound, "version"),
		"版本过低":   failing("himalaya 0.9.0", "", nil, ""),
		"版本过高":   failing("himalaya 3.0.0", "", nil, ""),
		"版本无法识别": failing("garbage", "", nil, ""),
		"认证失败":   failing("himalaya 2.0.0", "Error: authentication failed: invalid credentials", errors.New("exit status 1"), ""),
	}
	for name, r := range cases {
		t.Run(name, func(t *testing.T) {
			code, out, errs := exec1(t, r, input("2026-01-01T00:00:00Z"))
			if code != ExitConfig || out != "" || errs == "" {
				t.Fatalf("code=%d out=%q err=%q", code, out, errs)
			}
		})
	}
	_, _, errs := exec1(t, failing("himalaya 0.9.0", "", nil, ""), input(""))
	if !strings.Contains(errs, "版本") {
		t.Fatalf("应说明版本不符: %s", errs)
	}
}

// 网络等临时错误 → 退出码 1。
func TestTemporaryFailureExit1(t *testing.T) {
	r := failing("himalaya 2.0.0", "connection reset by peer", errors.New("exit status 1"), "")
	if code, out, _ := exec1(t, r, input("")); code != ExitTemporary || out != "" {
		t.Fatalf("code=%d out=%q", code, out)
	}
}

// ACT-MAIL-07：缺 Message-ID、非 JSON、正文非字符串 → 退出码 2，不输出任何东西（含游标）。
func TestUnparsableOutputExit2WithoutCursor(t *testing.T) {
	outputs := map[string]string{
		"缺Message-ID": `[{"id":"1","subject":"x","from":{"addr":"a@b"},"date":"2026-01-02T00:00:00Z"}]`,
		"非JSON":       `not json`,
		"日期坏":         `[{"id":"1","message_id":"<a>","date":"yesterday"}]`,
		"缺编号":         `[{"message_id":"<a>","date":"2026-01-02T00:00:00Z"}]`,
	}
	for name, list := range outputs {
		t.Run(name, func(t *testing.T) {
			r := func(_ context.Context, args []string) ([]byte, []byte, error) {
				if args[0] == "--version" {
					return []byte("himalaya 2.0.0"), nil, nil
				}
				return []byte(list), nil, nil
			}
			if code, out, _ := exec1(t, r, input("")); code != ExitConfig || out != "" {
				t.Fatalf("code=%d out=%q", code, out)
			}
		})
	}
	r := func(_ context.Context, args []string) ([]byte, []byte, error) {
		switch args[0] {
		case "--version":
			return []byte("himalaya 2.0.0"), nil, nil
		case "envelope":
			b, err := os.ReadFile("testdata/2.0.0/list.json")
			if err != nil {
				t.Fatal(err)
			}
			return b, nil, nil
		}
		return []byte(`{"not":"string"}`), nil, nil
	}
	if code, out, _ := exec1(t, r, input("")); code != ExitConfig || out != "" {
		t.Fatalf("正文坏: code=%d out=%q", code, out)
	}
}

func TestBadInputExit2(t *testing.T) {
	for _, in := range []string{`nope`, input("not-a-time")} {
		if code, out, _ := exec1(t, nil, in); code != ExitConfig || out != "" {
			t.Fatalf("%q: code=%d out=%q", in, code, out)
		}
	}
}

// 真实 exec 路径（ACT-MAIL-04）：PATH 里没有 himalaya → 退出码 2；不连任何账号。
func TestRealExecWithoutToolExit2(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	var o, e bytes.Buffer
	if code := Run(context.Background(), strings.NewReader(input("2026-01-01T00:00:00Z")), &o, &e); code != ExitConfig || o.Len() != 0 {
		t.Fatalf("code=%d out=%q err=%q", code, o.String(), e.String())
	}
}

// 真实 exec 路径：同名替身脚本只收到白名单环境变量，输出被正确解析。
func TestRealExecEnvWhitelist(t *testing.T) {
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
--version) echo "himalaya 2.0.0";;
envelope) if env | grep -q '^SECRET='; then echo leaked >&2; exit 9; fi; echo '[]';;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "himalaya"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":/usr/bin:/bin")
	t.Setenv("SECRET", "x")
	var o, e bytes.Buffer
	code := Run(context.Background(), strings.NewReader(input("2026-01-01T00:00:00Z")), &o, &e)
	if code != ExitOK || strings.TrimSpace(o.String()) != `{"cursor":"2026-01-01T00:00:00Z"}` {
		t.Fatalf("code=%d out=%q err=%q", code, o.String(), e.String())
	}
}

// 空游标且无邮件：用 window.to 起步。
func TestFirstRunEmptyMailboxUsesWindowTo(t *testing.T) {
	r := func(_ context.Context, args []string) ([]byte, []byte, error) {
		if args[0] == "--version" {
			return []byte("himalaya 2.0.0"), nil, nil
		}
		return []byte(`[]`), nil, nil
	}
	code, out, _ := exec1(t, r, `{"config":{},"window":{"to":"2026-03-01T00:00:00Z"}}`)
	if code != ExitOK || strings.TrimSpace(out) != `{"cursor":"2026-03-01T00:00:00Z"}` {
		t.Fatalf("code=%d out=%q", code, out)
	}
	_, out, _ = exec1(t, r, `{"config":{}}`)
	if !strings.Contains(out, "2026-02-01T00:00:00Z") {
		t.Fatalf("out=%q", out)
	}
}

func TestDetailTruncated(t *testing.T) {
	if got := truncate(strings.Repeat("好", maxDetail)); len(got) > maxDetail {
		t.Fatal(len(got))
	}
}
