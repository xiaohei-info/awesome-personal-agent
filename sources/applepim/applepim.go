// Package applepim 是 macOS 自带应用（邮件、日历、提醒事项）的来源薄适配（设计 §9.7）：按 Source
// 执行契约（§9.2）读 stdin，只调用 apple-pim 的 mail-cli、calendar-cli、reminder-cli 的读命令，
// 转成标准事件 JSONL，末行是新游标。一个二进制，由 config.kind 选择 mail、calendar、reminders。
// 不访问应用的数据文件，不写账本；系统授权归工具和（可选的）辅助应用，本程序不读不存任何口令。
package applepim

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
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 退出码沿用 §9.2。
const (
	ExitOK        = 0
	ExitTemporary = 1 // 超时、工具偶发失败：退避重试
	ExitConfig    = 2 // 未安装、系统授权缺失、输出无法解析、配置不合法：生成 needs_input
)

const (
	defaultTimeout = 60 * time.Second
	defaultMax     = 50
	defaultDays    = 7
	defaultHorizon = 30
	listLimit      = 500
	maxDetail      = 16 << 10
)

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
	Kind string `json:"kind"` // mail | calendar | reminders，必填
	// BinDir 是 mail-cli 等所在目录，缺省按 PATH 查找。
	BinDir string `json:"bin_dir,omitempty"`
	// HelperApp 是辅助应用（.app）路径。工具读日历、提醒事项需要系统授权，授权归"启动它的应用"；
	// 来源由后台服务拉起时没有这个应用，填上后经 `open -W -n -a` 让辅助应用成为授权主体。缺省直接拉起工具。
	HelperApp string `json:"helper_app,omitempty"`

	Account      string `json:"account,omitempty"` // mail：邮件应用里的账户名，必填
	Mailbox      string `json:"mailbox,omitempty"` // mail：缺省 INBOX
	MaxMessages  int    `json:"max_messages,omitempty"`
	LookbackDays int    `json:"lookback_days,omitempty"` // mail：首次拉取只看最近 N 天，缺省 7

	Calendars   []string `json:"calendars,omitempty"`    // calendar：日历名或 ID；缺省全部
	HorizonDays int      `json:"horizon_days,omitempty"` // calendar：向前看多少天，缺省 30
	Lists       []string `json:"lists,omitempty"`        // reminders：列表名或 ID；缺省全部

	TimeoutSec int `json:"timeout_sec,omitempty"`
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
	// 不设 sender_verified：邮件应用不给可核实的结论（§9.7）。
}

// Runner 运行一个 apple-pim 工具（cli 如 "mail-cli"），参数一律 argv，不经 shell。
// 测试边界用它隔离真实工具。
type Runner func(ctx context.Context, cli string, args ...string) (stdout, stderr []byte, err error)

type runErr struct {
	exit int
	msg  string
}

func (e *runErr) Error() string { return e.msg }

func fail(exit int, f string, a ...any) error { return &runErr{exit, fmt.Sprintf(f, a...)} }

// Run 执行一次拉取并返回退出码；失败时 stdout 无内容（不输出新游标）。
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	return run(ctx, stdin, stdout, stderr, execRunner, time.Now)
}

func run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, mk func(Config) Runner, now func() time.Time) int {
	out, err := poll(ctx, stdin, mk, now)
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

func poll(ctx context.Context, stdin io.Reader, mk func(Config) Runner, now func() time.Time) ([]byte, error) {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, fail(ExitConfig, "stdin 不是合法的 JSON: %v", err)
	}
	cfg := in.Config
	timeout := defaultTimeout
	if cfg.TimeoutSec > 0 {
		timeout = time.Duration(cfg.TimeoutSec) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var events []event
	var cursor string
	var err error
	r := mk(cfg)
	switch cfg.Kind {
	case "mail":
		events, cursor, err = pollMail(ctx, r, cfg, in, now())
	case "calendar":
		events, cursor, err = pollCalendar(ctx, r, cfg, in, now())
	case "reminders":
		events, cursor, err = pollReminders(ctx, r, cfg, now())
	default:
		return nil, fail(ExitConfig, "config.kind 必须是 mail、calendar 或 reminders，现为 %q", cfg.Kind)
	}
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	for _, ev := range events {
		_ = enc.Encode(ev)
	}
	_ = enc.Encode(map[string]string{"cursor": cursor})
	return buf.Bytes(), nil
}

// ---- 邮件 ----

type mailMsg struct {
	MessageID    string `json:"messageId"`
	Subject      string `json:"subject"`
	DateReceived string `json:"dateReceived"`
	SenderAddr   string `json:"senderAddress"`
	Sender       string `json:"sender"`
	Content      string `json:"content"`
	// get 的 message 带线程头（MEAS-MAIL-THREAD）；缺失时只给自己的 Message-ID。
	References string `json:"references"`
	InReplyTo  string `json:"inReplyTo"`
}

func pollMail(ctx context.Context, r Runner, cfg Config, in Input, now time.Time) ([]event, string, error) {
	if cfg.Account == "" {
		return nil, "", fail(ExitConfig, "邮件来源配置缺少 account：填邮件应用里的账户名（mail-cli accounts）")
	}
	if cfg.Mailbox == "" {
		cfg.Mailbox = "INBOX"
	}
	if cfg.MaxMessages <= 0 {
		cfg.MaxMessages = defaultMax
	}
	if cfg.LookbackDays <= 0 {
		cfg.LookbackDays = defaultDays
	}
	floor := now.Add(-time.Duration(cfg.LookbackDays) * 24 * time.Hour)
	if t, err := time.Parse(time.RFC3339Nano, in.Window.From); err == nil {
		floor = t
	}
	if in.Cursor != "" {
		t, err := time.Parse(time.RFC3339Nano, in.Cursor)
		if err != nil {
			return nil, "", fail(ExitConfig, "游标不是 RFC3339 时间: %q", in.Cursor)
		}
		floor = t
	}
	// --since 隐含最旧优先，--limit 截断后游标只推进到已读部分，多次运行收敛；
	// sqlite 引擎只读邮件应用的索引与邮件文件，不经应用，不会改已读标记（§9.7）。
	var out struct {
		Messages []mailMsg `json:"messages"`
	}
	raw, err := call(ctx, r, "mail-cli", "messages", "--format=json", "--engine=sqlite", "--account="+cfg.Account,
		"--mailbox="+cfg.Mailbox, "--since="+floor.UTC().Format(time.RFC3339), "--limit="+strconv.Itoa(cfg.MaxMessages))
	if err != nil {
		return nil, "", err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fail(ExitConfig, "messages 输出无法解析: %v", err)
	}
	type item struct {
		m  mailMsg
		at time.Time
	}
	items := make([]item, 0, len(out.Messages))
	for i, m := range out.Messages {
		at, err := time.Parse(time.RFC3339Nano, m.DateReceived)
		if err != nil || m.MessageID == "" {
			return nil, "", fail(ExitConfig, "第 %d 封缺少 messageId 或 dateReceived 无法解析", i)
		}
		items = append(items, item{m, at})
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].at.Before(items[j].at) })

	newest := time.Time{}
	if in.Cursor != "" {
		newest = floor
	}
	var evs []event
	for n, it := range items {
		if n > 0 && ctx.Err() != nil {
			break // 超时：已读部分照常输出，游标只到已读部分
		}
		raw, err := call(ctx, r, "mail-cli", "get", "--format=json", "--engine=sqlite", "--account="+cfg.Account,
			"--mailbox="+cfg.Mailbox, "--id="+it.m.MessageID)
		if err != nil {
			if n > 0 && ctx.Err() != nil {
				break
			}
			return nil, "", err
		}
		var got struct {
			Message mailMsg `json:"message"`
		}
		if err := json.Unmarshal(raw, &got); err != nil {
			return nil, "", fail(ExitConfig, "get 输出无法解析: %v", err)
		}
		subj := strings.TrimSpace(it.m.Subject)
		if subj == "" {
			subj = "(无主题)"
		}
		sender := it.m.SenderAddr
		if sender == "" {
			sender = it.m.Sender
		}
		evs = append(evs, event{ID: it.m.MessageID, Type: "mail.received", Subject: subj, Detail: truncate(got.Message.Content),
			OccurredAt: it.at.UTC().Format(time.RFC3339Nano), Sender: sender,
			CorrelationKeys: []string{"thread:" + threadRoot(it.m.MessageID, got.Message)}})
		// 游标最多到当前时间：发件人控制的日期不可信，未来日期不能让后续邮件被永久越过。
		if at := minTime(it.at, now); at.After(newest) {
			newest = at
		}
	}
	if newest.IsZero() {
		newest = now
	}
	return evs, newest.UTC().Format(time.RFC3339Nano), nil
}

var msgIDRe = regexp.MustCompile(`<([^<>\s]+)>`)

// threadRoot 返回线程根 Message-ID（§9.7：References 首项，没有取 In-Reply-To，都没有就是自己）。
// 写法与自己的 messageId 对齐：messageId 不带尖括号时，头里的 <id> 也去掉，根信与回信的键才相等。
func threadRoot(own string, m mailMsg) string {
	for _, h := range []string{m.References, m.InReplyTo} {
		if sm := msgIDRe.FindStringSubmatch(h); sm != nil {
			if strings.HasPrefix(own, "<") {
				return sm[0]
			}
			return sm[1]
		}
	}
	return own
}

// ---- 日历 ----

type calEvent struct {
	ID             string `json:"id"`
	Title          string `json:"title"`
	Status         string `json:"status"`
	StartDate      string `json:"startDate"`
	EndDate        string `json:"endDate"`
	OccurrenceDate string `json:"occurrenceDate"`
	LocalStart     string `json:"localStart"`
	Location       string `json:"location"`
	Notes          string `json:"notes"`
	URL            string `json:"url"`
}

func pollCalendar(ctx context.Context, r Runner, cfg Config, in Input, now time.Time) ([]event, string, error) {
	if cfg.HorizonDays <= 0 {
		cfg.HorizonDays = defaultHorizon
	}
	from := now.UTC()
	if t, err := time.Parse(time.RFC3339, in.Window.To); err == nil {
		from = t.UTC()
	}
	to := from.AddDate(0, 0, cfg.HorizonDays)
	// 与 Google 日历适配器相同：每次取前瞻窗口，不按游标过滤，重复由内核按事件 id 去重。
	scopes := cfg.Calendars
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	var evs []event
	for _, c := range scopes {
		args := []string{"events", "--format=json", "--from=" + from.Format(time.RFC3339), "--to=" + to.Format(time.RFC3339), "--limit=" + strconv.Itoa(listLimit)}
		if c != "" {
			args = append(args, "--calendar="+c)
		}
		raw, err := call(ctx, r, "calendar-cli", args...)
		if err != nil {
			return nil, "", err
		}
		var out struct {
			Events []calEvent `json:"events"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return nil, "", fail(ExitConfig, "events 输出无法解析: %v", err)
		}
		if len(out.Events) >= listLimit {
			return nil, "", fail(ExitConfig, "前瞻窗口内日程达到 %d 条上限，请缩小 horizon_days 或 calendars", listLimit)
		}
		for i, e := range out.Events {
			at, err := time.Parse(time.RFC3339Nano, e.StartDate)
			if err != nil || e.ID == "" {
				return nil, "", fail(ExitConfig, "第 %d 个日程缺少 id 或 startDate 无法解析", i)
			}
			occ := e.OccurrenceDate
			if occ == "" {
				occ = e.StartDate
			}
			typ := "event.created"
			if strings.EqualFold(e.Status, "canceled") || strings.EqualFold(e.Status, "cancelled") {
				typ = "event.cancelled"
			}
			subject := strings.TrimSpace(e.Title)
			if subject == "" {
				subject = "(无标题)"
			}
			summary := e.LocalStart
			if e.Location != "" {
				summary += " @ " + e.Location
			}
			// 工具不给更新时间：事件 id 带内容摘要，改动后得到新 id；关联键不含摘要，更新经 §9.4 写回原事项。
			evs = append(evs, event{
				ID: e.ID + "|" + occ + "|" + digest(e.Status, e.Title, e.Notes, e.Location, e.StartDate, e.EndDate), Type: typ,
				Subject: subject, Summary: strings.TrimSpace(summary), Detail: e.Notes, Link: e.URL,
				OccurredAt: at.UTC().Format(time.RFC3339Nano), CorrelationKeys: []string{e.ID + "|" + occ},
			})
		}
	}
	return evs, from.Format(time.RFC3339Nano), nil
}

// ---- 提醒事项 ----

type reminder struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	List    string `json:"list"`
	DueDate string `json:"dueDate"`
	Notes   string `json:"notes"`
}

func pollReminders(ctx context.Context, r Runner, cfg Config, now time.Time) ([]event, string, error) {
	scopes := cfg.Lists
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	// upcoming 只含未完成且有到期日的提醒；overdue 单列，两者合并去重。
	seen := map[string]bool{}
	var evs []event
	for _, l := range scopes {
		for _, f := range []string{"upcoming", "overdue"} {
			args := []string{"items", "--format=json", "--filter=" + f, "--limit=" + strconv.Itoa(listLimit)}
			if l != "" {
				args = append(args, "--list="+l)
			}
			raw, err := call(ctx, r, "reminder-cli", args...)
			if err != nil {
				return nil, "", err
			}
			var out struct {
				Reminders []reminder `json:"reminders"`
			}
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, "", fail(ExitConfig, "items 输出无法解析: %v", err)
			}
			if len(out.Reminders) >= listLimit {
				return nil, "", fail(ExitConfig, "提醒事项达到 %d 条上限，请用 lists 缩小范围", listLimit)
			}
			for i, x := range out.Reminders {
				if x.ID == "" {
					return nil, "", fail(ExitConfig, "第 %d 条提醒缺少 id", i)
				}
				if seen[x.ID] {
					continue
				}
				seen[x.ID] = true
				due, err := parseDue(x.DueDate)
				if x.DueDate != "" && err != nil {
					return nil, "", fail(ExitConfig, "提醒 dueDate 无法解析: %q", x.DueDate)
				}
				if x.DueDate == "" {
					due = now
				}
				subject := strings.TrimSpace(x.Title)
				if subject == "" {
					subject = "(无标题)"
				}
				summary := "到期 " + x.DueDate
				if x.List != "" {
					summary += " · " + x.List
				}
				evs = append(evs, event{
					ID: x.ID + "|" + digest(x.Title, x.DueDate, x.Notes, x.List), Type: "reminder.due", Subject: subject,
					Summary: summary, Detail: x.Notes, OccurredAt: due.UTC().Format(time.RFC3339Nano), CorrelationKeys: []string{x.ID},
				})
			}
		}
	}
	return evs, now.UTC().Format(time.RFC3339Nano), nil
}

func parseDue(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if t, err := time.ParseInLocation("2006-01-02 15:04", s, time.Local); err == nil {
		return t, nil // 工具对带时间的本地提醒输出不带时区的 "日期 时:分"
	}
	return time.Parse("2006-01-02", s) // 只有日期的提醒
}

// ---- 工具调用 ----

var authRe = regexp.MustCompile(`(?i)access denied|not authori[sz]ed|authori[sz]ation|permission|not allowed|notDetermined|automation`)

// call 运行工具并校验输出：必须是 JSON，且 success 不为 false。工具没有版本参数，
// 输出契约靠必填字段校验（缺字段返回 2），而不是版本比对。
func call(ctx context.Context, r Runner, cli string, args ...string) ([]byte, error) {
	out, errOut, err := r(ctx, cli, args...)
	msg := strings.TrimSpace(string(errOut))
	var nf *exec.Error
	switch {
	case errors.As(err, &nf) && errors.Is(nf.Err, exec.ErrNotFound):
		return nil, fail(ExitConfig, "未找到 %s，请安装 apple-pim 或设置 config.bin_dir", cli)
	case ctx.Err() != nil:
		return nil, fail(ExitTemporary, "%s 超时: %v", cli, ctx.Err())
	case err != nil || len(bytes.TrimSpace(out)) == 0:
		return nil, toolFailure(cli, msg, err)
	}
	var st struct {
		Success *bool  `json:"success"`
		Error   string `json:"error"`
	}
	if json.Unmarshal(out, &st) == nil && st.Success != nil && !*st.Success {
		return nil, toolFailure(cli, st.Error, nil)
	}
	return out, nil
}

func toolFailure(cli, msg string, err error) error {
	if authRe.MatchString(msg) {
		return fail(ExitConfig, "%s 没有系统授权或被拒绝: %s", cli, msg)
	}
	return fail(ExitTemporary, "%s 失败: %v: %s", cli, err, msg)
}

var envKeep = regexp.MustCompile(`^(PATH|HOME|XDG_[A-Z_]+)=`)

func baseEnv() []string {
	var env []string
	for _, kv := range os.Environ() {
		if envKeep.MatchString(kv) {
			env = append(env, kv)
		}
	}
	return env
}

// execRunner 以白名单环境（PATH、HOME、XDG_*，§9.7）拉起工具；配了辅助应用则经 open -W 拉起。
// open 不传回子进程退出码，所以辅助应用路径上"stdout 为空"即视为失败（call 处理）。
func execRunner(cfg Config) Runner {
	return func(ctx context.Context, cli string, args ...string) ([]byte, []byte, error) {
		path := cli
		if cfg.BinDir != "" {
			path = filepath.Join(cfg.BinDir, cli)
		}
		if cfg.HelperApp == "" {
			cmd := exec.CommandContext(ctx, path, args...)
			cmd.Env = baseEnv()
			var o, e bytes.Buffer
			cmd.Stdout, cmd.Stderr = &o, &e
			err := cmd.Run()
			return o.Bytes(), e.Bytes(), err
		}
		if _, err := exec.LookPath(path); err != nil {
			return nil, nil, err
		}
		dir, err := os.MkdirTemp("", "applepim-")
		if err != nil {
			return nil, nil, err
		}
		defer os.RemoveAll(dir)
		outF, errF := filepath.Join(dir, "out"), filepath.Join(dir, "err")
		// -n：多个来源同时拉取时，已在运行的辅助应用不会再收到 --args，必须各开一个实例。
		oa := []string{"-W", "-n", "-a", cfg.HelperApp}
		if cfg.BinDir != "" {
			oa = append(oa, "--env", "APPLE_PIM_BIN_DIR="+cfg.BinDir)
		}
		oa = append(oa, "--args", cli, outF, errF)
		cmd := exec.CommandContext(ctx, "/usr/bin/open", append(oa, args...)...)
		cmd.Env = baseEnv()
		if err := cmd.Run(); err != nil {
			return nil, nil, err
		}
		o, _ := os.ReadFile(outF)
		e, _ := os.ReadFile(errF)
		return o, e, nil
	}
}

func digest(parts ...string) string {
	h := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(h[:6])
}

func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

func truncate(s string) string {
	if len(s) <= maxDetail {
		return s
	}
	return strings.ToValidUTF8(s[:maxDetail], "")
}
