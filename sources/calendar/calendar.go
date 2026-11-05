// Package calendar 是日历来源的薄适配器（设计 §9.7）：按 Source 执行契约（§9.2）读 stdin，
// 只读调用 gog calendar 取日程，转成标准事件 JSONL，末行是新游标。
// 不实现 CalDAV 和 OAuth（鉴权归工具自己），不写账本、不访问内核存储。
package calendar

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// 退出码沿用 §9.2。
const (
	ExitOK        = 0
	ExitTemporary = 1 // 网络等临时错误、超时
	ExitAuth      = 2 // 工具未安装、版本不在声明范围、认证失败、输出无法解析、配置不合法
)

const (
	tool           = "gog"
	defaultTimeout = 60 * time.Second
	defaultHorizon = 30 // 天
	maxOutput      = 16 << 20
)

// 声明支持的工具版本范围 [minVersion, maxVersion)；放宽前先在 testdata 加该版本的录制样本。
var minVersion, maxVersion = [3]int{0, 9, 0}, [3]int{0, 10, 0}

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

// Config 是登记时填的配置。
type Config struct {
	CalendarID  string `json:"calendar_id,omitempty"`  // 缺省 primary
	HorizonDays int    `json:"horizon_days,omitempty"` // 向前看多少天，缺省 30
	TimeoutSec  int    `json:"timeout_sec,omitempty"`
}

type event struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Subject         string   `json:"subject"`
	Summary         string   `json:"summary,omitempty"`
	Detail          string   `json:"detail,omitempty"`
	Link            string   `json:"link,omitempty"`
	OccurredAt      string   `json:"occurred_at"`
	CorrelationKeys []string `json:"correlation_keys,omitempty"`
	Sender          string   `json:"sender,omitempty"`
}

// gogEvent 是 gog 输出的单个日程（Google Calendar 事件字段）。
type gogEvent struct {
	ID                string   `json:"id"`
	ICalUID           string   `json:"iCalUID"`
	Status            string   `json:"status"`
	Summary           string   `json:"summary"`
	Description       string   `json:"description"`
	Location          string   `json:"location"`
	HTMLLink          string   `json:"htmlLink"`
	Created           string   `json:"created"`
	Updated           string   `json:"updated"`
	Start             gogTime  `json:"start"`
	OriginalStartTime *gogTime `json:"originalStartTime"`
	Organizer         struct {
		Email string `json:"email"`
	} `json:"organizer"`
}

type gogTime struct {
	DateTime string `json:"dateTime"`
	Date     string `json:"date"`
}

func (g gogTime) String() string {
	if g.DateTime != "" {
		return g.DateTime
	}
	return g.Date
}

type runErr struct {
	exit int
	msg  string
}

func (e *runErr) Error() string { return e.msg }

func fail(exit int, f string, a ...any) error { return &runErr{exit, fmt.Sprintf(f, a...)} }

// execFn 运行一次工具并返回 stdout、stderr；非零退出以 *exec.ExitError 返回。
type execFn func(ctx context.Context, args ...string) (stdout, stderr []byte, err error)

// Run 执行一次拉取并返回退出码；失败时 stdout 无内容。
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(ctx, stdin, stdout, stderr, execTool, time.Now)
}

func run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, ex execFn, now func() time.Time) int {
	out, err := poll(ctx, stdin, ex, now)
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

func poll(ctx context.Context, stdin io.Reader, ex execFn, now func() time.Time) ([]byte, error) {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, fail(ExitAuth, "stdin 不是合法的 JSON: %v", err)
	}
	cfg := in.Config
	if cfg.CalendarID == "" {
		cfg.CalendarID = "primary"
	}
	if cfg.HorizonDays <= 0 {
		cfg.HorizonDays = defaultHorizon
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
			return nil, fail(ExitAuth, "游标不是 RFC3339 时间: %q", in.Cursor)
		}
	}
	if err := checkVersion(ctx, ex); err != nil {
		return nil, err
	}

	// 日历关心将来的日程：窗口从本次拉取时刻往后看；更新靠事件 id 里的更新标识区分。
	from := now().UTC()
	if t, err := time.Parse(time.RFC3339, in.Window.To); err == nil {
		from = t.UTC()
	}
	to := from.AddDate(0, 0, cfg.HorizonDays)
	stdout, stderr, err := ex(ctx, "--readonly", "--no-input", "--json", "calendar", "events", cfg.CalendarID,
		"--from", from.Format(time.RFC3339), "--to", to.Format(time.RFC3339))
	if err != nil {
		return nil, toolErr(ctx, err, stderr)
	}
	items, err := parseEvents(stdout)
	if err != nil {
		return nil, err
	}

	newest := floor
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for i, it := range items {
		ev, updated, err := convert(it)
		if err != nil {
			return nil, fail(ExitAuth, "第 %d 个日程无法解析: %v", i, err)
		}
		if !updated.IsZero() {
			if updated.Before(floor) { // 等于游标的保留，由内核按事件 id 去重
				continue
			}
			if updated.After(newest) {
				newest = updated
			}
		}
		_ = enc.Encode(ev)
	}
	cursor := in.Cursor
	if !newest.IsZero() {
		cursor = newest.UTC().Format(time.RFC3339Nano)
	} else if cursor == "" {
		cursor = from.Format(time.RFC3339Nano)
	}
	_ = enc.Encode(map[string]string{"cursor": cursor})
	return buf.Bytes(), nil
}

var versionRe = regexp.MustCompile(`(\d+)\.(\d+)\.(\d+)`)

func checkVersion(ctx context.Context, ex execFn) error {
	out, stderr, err := ex(ctx, "--version")
	if err != nil {
		return toolErr(ctx, err, stderr)
	}
	m := versionRe.FindStringSubmatch(string(out))
	if m == nil {
		return fail(ExitAuth, "无法从 %s --version 识别版本: %q", tool, strings.TrimSpace(string(out)))
	}
	var v [3]int
	for i := range v {
		v[i], _ = strconv.Atoi(m[i+1])
	}
	if cmp(v, minVersion) < 0 || cmp(v, maxVersion) >= 0 {
		return fail(ExitAuth, "%s 版本 %s 不在声明范围 [%s, %s)", tool, m[0], join(minVersion), join(maxVersion))
	}
	return nil
}

func cmp(a, b [3]int) int {
	for i := range a {
		if a[i] != b[i] {
			if a[i] < b[i] {
				return -1
			}
			return 1
		}
	}
	return 0
}

func join(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }

var authRe = regexp.MustCompile(`(?i)\b(auth|authenticat\w*|unauthori[sz]ed|credentials?|token|login|invalid_grant|401|403|forbidden)\b`)

// toolErr 把工具失败映射成退出码：未安装、认证失败为 2；超时、网络等其余失败为 1。
func toolErr(ctx context.Context, err error, stderr []byte) error {
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return fail(ExitAuth, "未安装 %s", tool)
	case ctx.Err() != nil:
		return fail(ExitTemporary, "%s 超时", tool)
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		msg := strings.TrimSpace(string(stderr))
		if authRe.MatchString(msg) {
			return fail(ExitAuth, "%s 认证失败: %s", tool, msg)
		}
		return fail(ExitTemporary, "%s 失败（退出码 %d）: %s", tool, ee.ExitCode(), msg)
	}
	return fail(ExitAuth, "无法运行 %s: %v", tool, err) // 如权限不足，需本人修正
}

// parseEvents 接受 {"events":[…]} 或直接数组两种形态；其他形态视为输出无法解析。
func parseEvents(b []byte) ([]gogEvent, error) {
	var wrapped struct {
		Events *[]gogEvent `json:"events"`
	}
	if json.Unmarshal(b, &wrapped) == nil && wrapped.Events != nil {
		return *wrapped.Events, nil
	}
	var arr []gogEvent
	if err := json.Unmarshal(b, &arr); err != nil {
		return nil, fail(ExitAuth, "%s 输出无法解析", tool)
	}
	return arr, nil
}

// convert 把一个日程转成标准事件，并返回其更新时间（缺失为零值）。
// 事件 id = UID | RECURRENCE-ID | 更新标识：同一日程的新版本得到新 id，换工具沿用同一规则。
func convert(g gogEvent) (event, time.Time, error) {
	uid := g.ICalUID
	if uid == "" {
		uid = g.ID
	}
	start := g.Start.String()
	if uid == "" || start == "" {
		return event{}, time.Time{}, errors.New("缺少 UID 或开始时间")
	}
	at, err := parseStart(start)
	if err != nil {
		return event{}, time.Time{}, err
	}
	var updated time.Time
	marker := g.Updated
	if g.Updated != "" {
		if updated, err = time.Parse(time.RFC3339Nano, g.Updated); err != nil {
			return event{}, time.Time{}, fmt.Errorf("updated 不是 RFC3339: %q", g.Updated)
		}
	} else {
		h := sha256.Sum256([]byte(strings.Join([]string{g.Status, g.Summary, g.Description, g.Location, start}, "\x00")))
		marker = "sha256:" + hex.EncodeToString(h[:6])
	}
	recurrence := ""
	if g.OriginalStartTime != nil {
		recurrence = g.OriginalStartTime.String()
	}
	typ := "event.created"
	switch {
	case g.Status == "cancelled":
		typ = "event.cancelled"
	case g.Updated != "" && g.Created != "" && g.Updated != g.Created:
		typ = "event.updated"
	}
	subject := strings.TrimSpace(g.Summary)
	if subject == "" {
		subject = "(无标题)"
	}
	summary := start
	if g.Location != "" {
		summary += " @ " + g.Location
	}
	ev := event{
		ID: uid + "|" + recurrence + "|" + marker, Type: typ, Subject: subject, Summary: summary,
		Detail: g.Description, Link: g.HTMLLink, OccurredAt: at.UTC().Format(time.RFC3339Nano),
		CorrelationKeys: []string{"cal:" + uid}, Sender: g.Organizer.Email,
	}
	return ev, updated, nil // 工具不给可核实的结论，不设 sender_verified
}

func parseStart(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	t, err := time.Parse("2006-01-02", s) // 全天日程
	if err != nil {
		return t, fmt.Errorf("开始时间不是 RFC3339 或日期: %q", s)
	}
	return t, nil
}

// execTool 以 argv 方式运行 gog，子进程环境只放行白名单（§9.7）。
func execTool(ctx context.Context, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, tool, args...)
	cmd.Env = whitelistEnv(os.Environ())
	var out, errb bytes.Buffer
	cmd.Stdout = &limitWriter{w: &out, n: maxOutput}
	cmd.Stderr = &errb
	err := cmd.Run()
	return out.Bytes(), errb.Bytes(), err
}

func whitelistEnv(env []string) []string {
	var keep []string
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "PATH" || k == "HOME" || strings.HasPrefix(k, "XDG_") {
			keep = append(keep, kv)
		}
	}
	return keep
}

type limitWriter struct {
	w io.Writer
	n int
}

func (l *limitWriter) Write(p []byte) (int, error) {
	if len(p) > l.n {
		return 0, errors.New("工具输出过大")
	}
	l.n -= len(p)
	return l.w.Write(p)
}
