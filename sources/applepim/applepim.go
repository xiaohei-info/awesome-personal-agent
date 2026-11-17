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
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/sources/internal/content"
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
	// undatedFilter：MEAS-REM-UNDATED 未实测，没有专门筛选时取列表全部提醒，由适配器筛未完成。
	undatedFilter = "all"
)

// outputMB 与内核 max_source_output_mb 的默认值一致；来源程序拿不到运行配置里的实际值（shortcut: 配小时改由内核传入）。
var outputMB = 32

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
	// UndatedLists：列在其中的列表，未完成的提醒不论有无到期日都产出事件（无到期日的是 reminder.open）。
	UndatedLists []string `json:"undated_lists,omitempty"`

	TimeoutSec int `json:"timeout_sec,omitempty"`
}

type event struct {
	ID              string         `json:"id"`
	Type            string         `json:"type"`
	Subject         string         `json:"subject"`
	Summary         string         `json:"summary,omitempty"`
	Detail          string         `json:"detail,omitempty"`
	Link            string         `json:"link,omitempty"`
	OccurredAt      string         `json:"occurred_at"`
	CorrelationKeys []string       `json:"correlation_keys,omitempty"`
	Sender          string         `json:"sender,omitempty"`
	Original        *original      `json:"original,omitempty"`
	Headers         map[string]any `json:"headers,omitempty"`
	Attachments     []attachment   `json:"attachments,omitempty"`
	Content         *declared      `json:"content,omitempty"`
	// 不设 sender_verified：邮件应用不给可核实的结论（§9.7）。
}

type original struct {
	MediaType string `json:"media_type"`
	Text      string `json:"text"`
}

type attachment struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size"`
	Inline    bool   `json:"inline,omitempty"`
}

type missing struct {
	Part   string `json:"part"`
	Reason string `json:"reason"`
}

// declared 是事件的 content 字段：没取全的部分自报为 partial（设计 §9.2）。
type declared struct {
	State   string    `json:"state"`
	Missing []missing `json:"missing"`
}

func partial(m []missing) *declared {
	if len(m) == 0 {
		return nil
	}
	return &declared{State: "partial", Missing: m}
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
		events, cursor, err = pollReminders(ctx, r, cfg, in.Cursor, now())
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
	// get 的结构化原文字段（MEAS-RAW-MAIL 未实测，形态按设计合成，见 testdata/README.md）：
	// 缺失（nil）与"确实没有"（空）要区分，前者以 partial 自报。
	Headers     []mailHeader `json:"headers"`
	HTMLContent *string      `json:"htmlContent"`
	Attachments *[]mailAtt   `json:"attachments"`
	Source      *string      `json:"source"` // 工具只给原始邮件时的 RFC 5322 全文
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
	batch := content.NewBatcher(outputMB)
	for n, it := range items {
		if n > 0 && (ctx.Err() != nil || batch.Full()) {
			break // 超时或输出预算用尽：已读部分照常输出，游标只到已读部分
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
		ev := buildMail(it.m, got.Message, subj, sender, it.at)
		if !batch.Add(eventSize(ev)) {
			break
		}
		evs = append(evs, ev)
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

// buildMail 把一封邮件的原文各部分装进事件；事件 id 仍是 Message-ID（WB-36）。
func buildMail(list, got mailMsg, subj, sender string, at time.Time) event {
	p := collectMail(list, got)
	ev := event{ID: list.MessageID, Type: "mail.received", Subject: subj, Detail: p.text,
		OccurredAt: at.UTC().Format(time.RFC3339Nano), Sender: sender,
		CorrelationKeys: []string{"thread:" + threadRoot(list.MessageID, got)}}
	if p.html != "" {
		ev.Original = &original{MediaType: "text/html", Text: p.html}
		if txt, err := content.HTMLToText(p.html); err != nil {
			ev.Detail = ""
			p.missing = append(p.missing, missing{"body", err.Error()})
		} else {
			ev.Detail = txt
		}
	}
	ev.Headers = content.NormalizeHeaders(p.headers)
	if p.attachments != nil {
		ev.Attachments = *p.attachments
	}
	ev.Content = partial(p.missing)
	return ev
}

func eventSize(ev event) int {
	b, _ := json.Marshal(ev)
	return len(b) + 1
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
	// 组织者与参与者：字符串，或带 name/email 的对象（形态未实测，两种都收）。
	Organizer json.RawMessage   `json:"organizer"`
	Attendees []json.RawMessage `json:"attendees"`
}

// person 把组织者、参与者的字符串或对象形态统一成 "名字 <邮箱>"。
func person(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var o struct{ Name, Email string }
	if json.Unmarshal(raw, &o) != nil {
		return ""
	}
	switch {
	case o.Name != "" && o.Email != "":
		return o.Name + " <" + o.Email + ">"
	case o.Email != "":
		return o.Email
	}
	return o.Name
}

func calHeaders(e calEvent) map[string]any {
	hs := []content.Header{{Name: "Start", Value: e.StartDate}, {Name: "End", Value: e.EndDate}, {Name: "Location", Value: e.Location},
		{Name: "Organizer", Value: person(e.Organizer)}, {Name: "Status", Value: e.Status}}
	for _, a := range e.Attendees {
		hs = append(hs, content.Header{Name: "Attendee", Value: person(a)})
	}
	return content.NormalizeHeaders(hs)
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
				OccurredAt: at.UTC().Format(time.RFC3339Nano), CorrelationKeys: []string{e.ID + "|" + occ}, Headers: calHeaders(e),
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
	// 未完成、创建时间与优先级；创建时间工具不给时取本次拉取的时刻。
	IsCompleted  bool   `json:"isCompleted"`
	CreationDate string `json:"creationDate"`
	Priority     *int   `json:"priority"`
}

// reminderCursor 是提醒事项的游标：上一轮看到的未完成提醒（id 到标题）。这一轮不再出现的发一条 reminder.closed，
// 让建议与简报知道它已不在未完成列表里（工具的 completed 过滤返回全部历史，按它取会触发条数上限，所以按差集判断）。
// 旧游标（时间串）读不出时不发 closed，这一轮只建立基线。
type reminderCursor struct {
	Open map[string]string `json:"open"`
}

func pollReminders(ctx context.Context, r Runner, cfg Config, cursor string, now time.Time) ([]event, string, error) {
	scopes := cfg.Lists
	if len(scopes) == 0 {
		scopes = []string{""}
	}
	// upcoming 只含未完成且有到期日的提醒；overdue 单列，两者合并去重。
	seen := map[string]bool{}
	var evs []event
	collect := func(list, filter string, undated bool) error {
		args := []string{"items", "--format=json", "--filter=" + filter, "--limit=" + strconv.Itoa(listLimit)}
		if list != "" {
			args = append(args, "--list="+list)
		}
		raw, err := call(ctx, r, "reminder-cli", args...)
		if err != nil {
			return err
		}
		var out struct {
			Reminders []reminder `json:"reminders"`
		}
		if err := json.Unmarshal(raw, &out); err != nil {
			return fail(ExitConfig, "items 输出无法解析: %v", err)
		}
		if len(out.Reminders) >= listLimit {
			return fail(ExitConfig, "提醒事项达到 %d 条上限，请用 lists 缩小范围", listLimit)
		}
		for i, x := range out.Reminders {
			if x.ID == "" {
				return fail(ExitConfig, "第 %d 条提醒缺少 id", i)
			}
			if seen[x.ID] || x.IsCompleted {
				continue
			}
			ev, err := reminderEvent(x, now, undated)
			if err != nil {
				return err
			}
			seen[x.ID] = true
			evs = append(evs, ev)
		}
		return nil
	}
	for _, l := range scopes {
		for _, f := range []string{"upcoming", "overdue"} {
			if err := collect(l, f, false); err != nil {
				return nil, "", err
			}
		}
	}
	// 没有到期日的提醒不在 upcoming、overdue 里：undated_lists 里的列表另取全部，由 collect 筛掉已完成的。
	for _, l := range cfg.UndatedLists {
		if err := collect(l, undatedFilter, true); err != nil {
			return nil, "", err
		}
	}
	var prev reminderCursor
	_ = json.Unmarshal([]byte(cursor), &prev)
	next := reminderCursor{Open: map[string]string{}}
	for _, ev := range evs {
		next.Open[ev.CorrelationKeys[0]] = ev.Subject
	}
	gone := make([]string, 0)
	for id := range prev.Open {
		if _, ok := next.Open[id]; !ok {
			gone = append(gone, id)
		}
	}
	slices.Sort(gone)
	for _, id := range gone {
		evs = append(evs, event{ID: id + "|closed|" + now.UTC().Format(time.RFC3339), Type: "reminder.closed", Subject: prev.Open[id],
			Summary: "不再是未完成：已完成、已删除，或移出了拉取范围", OccurredAt: now.UTC().Format(time.RFC3339Nano), CorrelationKeys: []string{id}})
	}
	b, err := json.Marshal(next)
	if err != nil {
		return nil, "", err
	}
	return evs, string(b), nil
}

// reminderEvent：有到期日是 reminder.due；undated_lists 的取数里没有到期日的是 reminder.open。
// 两者 id 的构成与关联键相同，改标题仍是同一个关联键，补上到期日后按来源对象更新写回原事项。
func reminderEvent(x reminder, now time.Time, undatedList bool) (event, error) {
	subject := strings.TrimSpace(x.Title)
	if subject == "" {
		subject = "(无标题)"
	}
	ev := event{ID: x.ID + "|" + digest(x.Title, x.DueDate, x.Notes, x.List), Subject: subject, Detail: x.Notes, CorrelationKeys: []string{x.ID}}
	hs := []content.Header{{Name: "List", Value: x.List}, {Name: "Due", Value: x.DueDate}}
	if x.Priority != nil {
		hs = append(hs, content.Header{Name: "Priority", Value: strconv.Itoa(*x.Priority)})
	}
	ev.Headers = content.NormalizeHeaders(hs)
	if x.DueDate == "" && undatedList {
		at := now
		if x.CreationDate != "" {
			t, err := parseDue(x.CreationDate)
			if err != nil {
				return ev, fail(ExitConfig, "提醒 creationDate 无法解析: %q", x.CreationDate)
			}
			at = t
		}
		ev.Type, ev.Summary, ev.OccurredAt = "reminder.open", x.List, at.UTC().Format(time.RFC3339Nano)
		return ev, nil
	}
	due := now // 不在 undated_lists 里取到的无到期日提醒：沿用旧行为
	if x.DueDate != "" {
		var err error
		if due, err = parseDue(x.DueDate); err != nil {
			return ev, fail(ExitConfig, "提醒 dueDate 无法解析: %q", x.DueDate)
		}
	}
	ev.Type, ev.Summary, ev.OccurredAt = "reminder.due", "到期 "+x.DueDate, due.UTC().Format(time.RFC3339Nano)
	if x.List != "" {
		ev.Summary += " · " + x.List
	}
	return ev, nil
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
