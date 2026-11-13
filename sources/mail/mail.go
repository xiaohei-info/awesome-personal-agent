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
	defaultMax     = 100
	defaultDays    = 7
	maxPages       = 100
	maxDetail      = 16 << 10
)

// Supported 声明支持的工具版本范围 [Min, Max) 及该范围内的 JSON 标志（参数在不同版本不同，§9.7）。
// Sample 是 testdata/ 下对应录制样本目录；放宽范围前必须先加样本（契约测试会检查）。
// IDFromRead：envelope JSON 不带 message_id（1.1.0 实测），改从 `message read -H Message-ID` 取；
// 2.x 范围未实测，样本沿用 envelope 带 message_id 的假设，拿到真实输出后再核对。
var Supported = []VersionRange{
	{Min: ver{1, 0, 0}, Max: ver{2, 0, 0}, JSONFlag: []string{"--output", "json"}, Sample: "1.1.0", IDFromRead: true},
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
	// IDFromRead 为真时 Message-ID 经 message read 的头部取得，而非 envelope JSON。
	IDFromRead bool
}

// Input 是 stdin 的 Source 契约输入。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Config   Config `json:"config"`
	Cursor   string `json:"cursor"`
	Window   struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"window"`
}

// Config 是适配器配置，全部可选。
type Config struct {
	Account  string `json:"account,omitempty"`
	Folder   string `json:"folder,omitempty"` // 默认 INBOX
	PageSize int    `json:"page_size,omitempty"`
	// MaxMessages 是单次运行最多读取正文的邮件数（默认 100），保证单次运行有界、多次运行收敛。
	MaxMessages int `json:"max_messages,omitempty"`
	// LookbackDays 只用于首次拉取（游标为空且无 window.from）：只看最近 N 天（默认 7）。
	LookbackDays int `json:"lookback_days,omitempty"`
	TimeoutSec   int `json:"timeout_sec,omitempty"`
}

type event struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Subject    string `json:"subject"`
	Detail     string `json:"detail,omitempty"`
	OccurredAt string `json:"occurred_at"`
	// CorrelationKeys 含 thread:<线程根 Message-ID>，回信与原信共用（§9.4、§9.7）。
	CorrelationKeys []string `json:"correlation_keys,omitempty"`
	Sender          string   `json:"sender,omitempty"`
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
	if cfg.MaxMessages <= 0 {
		cfg.MaxMessages = defaultMax
	}
	if cfg.LookbackDays <= 0 {
		cfg.LookbackDays = defaultDays
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

	// floor 是只读取不早于它的邮件。首次拉取（无游标）用 window.from，否则回看 LookbackDays 天，
	// 且只取最新 MaxMessages 封，以 now 为游标起点；更早的邮件按设计不回填。
	first := in.Cursor == ""
	floor := now().Add(-time.Duration(cfg.LookbackDays) * 24 * time.Hour)
	if in.Window.From != "" {
		if w, err := time.Parse(time.RFC3339Nano, in.Window.From); err == nil {
			floor = w
		}
	}
	if !first {
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
	type item struct {
		ev  event
		at  time.Time
		ref string // himalaya 的信件编号，仅用于 message read
	}
	// 信件按新到旧分页；翻到出现早于游标的信件、或页不满为止，否则积压会被游标越过而丢失。
	var items []item
	for page := 1; ; page++ {
		if first && len(items) >= cfg.MaxMessages {
			break // 首次拉取只要最新的一批
		}
		if page > maxPages {
			// 不能推进游标：更旧的未取信件仍在游标之后；重试不会收敛，需要本人调大 page_size。
			return nil, fail(ExitConfig, "游标之后的新邮件超过 %d 页（每页 %d），请调大 config.page_size", maxPages, cfg.PageSize)
		}
		args := append(base("envelope", "list"), "--page-size", strconv.Itoa(cfg.PageSize), "--page", strconv.Itoa(page))
		raw, err := call(ctx, r, append(args, vr.JSONFlag...))
		if err != nil {
			return nil, err
		}
		var envs []envelope
		if err := json.Unmarshal(raw, &envs); err != nil {
			return nil, fail(ExitConfig, "envelope 输出不是 JSON 数组: %v", err)
		}
		reachedOld := false
		for i, e := range envs {
			ev, at, err := e.toEvent(!vr.IDFromRead)
			if err != nil {
				return nil, fail(ExitConfig, "第 %d 页第 %d 封无法解析: %v", page, i, err)
			}
			if at.Before(floor) { // 等于游标的保留，由内核按 id 去重
				reachedOld = true
				continue
			}
			items = append(items, item{ev, at, e.ID.String()})
		}
		if reachedOld || len(envs) < cfg.PageSize {
			break
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })
	if len(items) > cfg.MaxMessages {
		if first {
			items = items[len(items)-cfg.MaxMessages:] // 首次：保留最新的
		} else {
			items = items[:cfg.MaxMessages] // 之后：从最旧的接着读，游标只推进到已读部分，多次运行收敛
		}
	}

	var newest time.Time
	if !first {
		newest = floor
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for n, it := range items {
		if n > 0 && ctx.Err() != nil {
			break // 单次运行超时：已读部分照常输出，游标只到已读部分，下次接着读
		}
		// --preview：不把邮件标为已读（§9.7 只读）。
		read := append(base("message", "read"), "--preview")
		if vr.IDFromRead {
			// 与读正文同一次调用，不增加往返；References/In-Reply-To 给线程关联键（MEAS-MAIL-THREAD）。
			read = append(read, "-H", "Message-ID", "-H", "References", "-H", "In-Reply-To")
		}
		body, err := call(ctx, r, append(append(read, it.ref), vr.JSONFlag...))
		if err != nil {
			if n > 0 && ctx.Err() != nil {
				break
			}
			return nil, err
		}
		var text string
		if err := json.Unmarshal(body, &text); err != nil {
			return nil, fail(ExitConfig, "message read 输出不是 JSON 字符串: %v", err)
		}
		root := it.ev.ID
		if vr.IDFromRead {
			if it.ev.ID, root, text, err = parseRead(text); err != nil {
				return nil, fail(ExitConfig, "message read 输出无法解析: %v", err)
			}
		}
		it.ev.CorrelationKeys = []string{"thread:" + root}
		it.ev.Detail = truncate(text)
		// 游标最多到当前时间：发件人控制的 Date 不可信，未来日期不能让后续邮件被永久越过。
		if at := minTime(it.at, now()); at.After(newest) {
			newest = at
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

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}
