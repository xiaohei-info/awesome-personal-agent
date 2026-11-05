// Package mail 是邮件来源的薄适配（设计 §9.7）：按 Source 执行契约（§9.2）调用 himalaya
// 只读取信，转成标准事件。事件 id 用 Message-ID；不实现 IMAP/SMTP/MIME/OAuth，不发信，
// 不改已读标记，不写账本。凭据归 himalaya 自己的配置，本程序不读不存。
package mail

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"time"
)

// 退出码沿用 §9.2。
const (
	ExitOK        = 0
	ExitTemporary = 1 // 网络等临时错误、超时：退避重试
	ExitConfig    = 2 // 未安装、版本不符、认证失败、输出无法解析、配置不合法：生成 needs_input
)

const (
	tool           = "himalaya"
	defaultTimeout = 60 * time.Second
	defaultPage    = 50
	maxDetail      = 16 << 10
)

// Supported 声明支持的工具版本范围 [Min, Max) 及该范围内的 JSON 标志（参数在不同版本不同，§9.7）。
// Sample 是 testdata/ 下对应录制样本目录；放宽范围前必须先加样本（契约测试会检查）。
var Supported = []VersionRange{
	{Min: ver{1, 0, 0}, Max: ver{2, 0, 0}, JSONFlag: []string{"--output", "json"}, Sample: "1.1.0"},
	{Min: ver{2, 0, 0}, Max: ver{3, 0, 0}, JSONFlag: []string{"--json"}, Sample: "2.0.0"},
}

type ver [3]int

func (v ver) less(o ver) bool {
	for i := range v {
		if v[i] != o[i] {
			return v[i] < o[i]
		}
	}
	return false
}

// VersionRange 是一段受支持的工具版本。
type VersionRange struct {
	Min, Max ver
	JSONFlag []string
	Sample   string
}

// Input 是 stdin 的 Source 契约输入。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Config   Config `json:"config"`
	Cursor   string `json:"cursor"`
	Window   struct {
		To string `json:"to"`
	} `json:"window"`
}

// Config 是适配器配置，全部可选。
type Config struct {
	Account    string `json:"account,omitempty"`
	Folder     string `json:"folder,omitempty"` // 默认 INBOX
	PageSize   int    `json:"page_size,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
}

type event struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Subject    string `json:"subject"`
	Detail     string `json:"detail,omitempty"`
	OccurredAt string `json:"occurred_at"`
	Sender     string `json:"sender,omitempty"`
	// 不设 sender_verified：himalaya 不给出可核实的结论（§9.7）。
}

// Runner 执行外部工具；参数一律 argv，不经 shell。测试边界用它隔离真实工具。
type Runner func(ctx context.Context, args []string) (stdout, stderr []byte, err error)

type runErr struct {
	exit int
	msg  string
}

func (e *runErr) Error() string { return e.msg }

func fail(exit int, f string, a ...any) error { return &runErr{exit, fmt.Sprintf(f, a...)} }

// Run 执行一次取信并返回退出码；失败时 stdout 无内容（不输出新游标）。
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(ctx, stdin, stdout, stderr, execRunner, time.Now)
}

func run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, r Runner, now func() time.Time) int {
	out, err := poll(ctx, stdin, r, now)
	if err != nil {
		fmt.Fprintln(stderr, err)
		var re *runErr
		if errors.As(err, &re) {
			return re.exit
		}
		return ExitTemporary
	}
	_, _ = stdout.Write(out)
	return ExitOK
}

var envKeep = regexp.MustCompile(`^(PATH|HOME|XDG_[A-Z_]+)=`)

// execRunner 以白名单环境（PATH、HOME、XDG_*，§9.7）拉起 himalaya。
func execRunner(ctx context.Context, args []string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	for _, kv := range os.Environ() {
		if envKeep.MatchString(kv) {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	return o.Bytes(), e.Bytes(), err
}

func poll(ctx context.Context, stdin io.Reader, r Runner, now func() time.Time) ([]byte, error) {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, fail(ExitConfig, "stdin 不是合法的 JSON: %v", err)
	}
	cfg := in.Config
	if cfg.Folder == "" {
		cfg.Folder = "INBOX"
	}
	if cfg.PageSize <= 0 {
		cfg.PageSize = defaultPage
	}
	timeout := defaultTimeout
	if cfg.TimeoutSec > 0 {
		timeout = time.Duration(cfg.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var floor time.Time
	if in.Cursor != "" {
		var err error
		if floor, err = time.Parse(time.RFC3339Nano, in.Cursor); err != nil {
			return nil, fail(ExitConfig, "游标不是 RFC3339 时间: %q", in.Cursor)
		}
	}

	vr, err := checkVersion(ctx, r)
	if err != nil {
		return nil, err
	}
	base := func(sub ...string) []string {
		a := append([]string{}, sub...)
		if cfg.Account != "" {
			a = append(a, "-a", cfg.Account)
		}
		return append(a, "-f", cfg.Folder)
	}
	listArgs := append(base("envelope", "list"), "--page-size", strconv.Itoa(cfg.PageSize))
	raw, err := call(ctx, r, append(listArgs, vr.JSONFlag...))
	if err != nil {
		return nil, err
	}
	var envs []envelope
	if err := json.Unmarshal(raw, &envs); err != nil {
		return nil, fail(ExitConfig, "envelope 输出不是 JSON 数组: %v", err)
	}

	type item struct {
		ev  event
		at  time.Time
		ref string // himalaya 的信件编号，仅用于 message read
	}
	var items []item
	for i, e := range envs {
		ev, at, err := e.toEvent()
		if err != nil {
			return nil, fail(ExitConfig, "第 %d 封无法解析: %v", i, err)
		}
		if at.Before(floor) { // 等于游标的保留，由内核按 id 去重
			continue
		}
		items = append(items, item{ev, at, e.ID.String()})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })

	newest := floor
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, it := range items {
		// --preview：不把邮件标为已读（§9.7 只读）。
		body, err := call(ctx, r, append(append(base("message", "read"), "--preview", it.ref), vr.JSONFlag...))
		if err != nil {
			return nil, err
		}
		var text string
		if err := json.Unmarshal(body, &text); err != nil {
			return nil, fail(ExitConfig, "message read 输出不是 JSON 字符串: %v", err)
		}
		it.ev.Detail = truncate(text)
		if it.at.After(newest) {
			newest = it.at
		}
		_ = enc.Encode(it.ev)
	}
	cursor := in.Cursor
	switch {
	case !newest.IsZero():
		cursor = newest.UTC().Format(time.RFC3339Nano)
	case in.Window.To != "":
		cursor = in.Window.To
	default:
		cursor = now().UTC().Format(time.RFC3339Nano)
	}
	_ = enc.Encode(map[string]string{"cursor": cursor})
	return buf.Bytes(), nil
}
