// Package github 是随发行附带的 GitHub 拉取程序（设计 §9.1、§9.2）：
// 按 Source 执行契约（stdin JSON → 标准事件 JSONL + 末行游标 + 退出码）拉取仓库的 issue、PR 与通知，并在仓库默认分支根目录 AGENTS.md 变化时发 repo.agents_md_changed。
//
// 复用 gh CLI（§5.7）：本包只做游标与事件映射，经 API 接口取数据。
// 换掉 gh（改用官方 API 库或别的工具）只需另写一个实现 API 的类型并在 cmd/github-source 里替换，
// 事件映射、游标与退出码语义不变。
//
// 凭据目录是 Source 登记的 credentials_path，经 stdin 交来，内核不读。
// `github-source auth <credentials_path>` 把 token 明文写在这个目录（gh 的 --insecure-storage），
// 让凭据跟着来源目录走；备份时按敏感数据对待（§16.2）。
//
// 事件由上游状态决定，与游标落在哪里无关：
//   - opened：created_at，id 不带时间（只发生一次）；
//   - merged：merged_at，id 不带时间（只发生一次）；未合并而关闭的是 closed：closed_at，id 带时间，
//     所以 close → reopen → close 是两条事件；
//   - updated：updated_at 晚于上面所有时间时才有，id 带时间。评论、改标签都只改 updated_at，
//     相邻两次拉取之间的多次改动合并成一条，所以每个对象每次拉取至多一条 updated；reopen 也表现为 updated。
//
// sender 是对象的作者。只有 opened 的动作者确定是作者才标 sender_verified；
// issue 列表不给 closed、merged、updated 的动作者，这些事件不标。
//
// 游标是每个流（一个仓库或通知）各自的续传位置，见 stream。
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// 退出码沿用 §9.2 的 Source 契约。
const (
	ExitOK        = 0
	ExitTemporary = 1 // 网络、限流、5xx：退避重试
	ExitAuth      = 2 // 认证或配置失败：内核生成 needs_input
	ExitPermanent = 3 // 永久失败：仓库已不存在（410）
)

const (
	perPage   = 100
	bodyLimit = 4000
	sumLimit  = 200
	// maxItems 是一次运行处理的新对象数（所有仓库合计，按页检查，可能多出不到一页）。积压更多时下次续传。
	// 它控制单次运行的 API 调用数和耗时；按单对象最坏约 38KB 算，输出低于内核默认的 32MB 上限。
	maxItems = 500
	// maxNotifPages 是一个通知时间窗一次能读的页数；超过就缩小时间窗（通知只能倒序读，见 sweepNotifications）。
	maxNotifPages = 20
	notifStream   = "notifications"
)

// API 取一个 GitHub REST 路径（含查询串）的 JSON 响应。失败必须返回 *Error 以便映射退出码；
// 其他错误按临时失败处理。
type API interface {
	Get(ctx context.Context, path string) ([]byte, error)
}

// Error 是带退出码的取数失败。
type Error struct {
	Exit     int
	Msg      string
	NotFound bool // 上游回 404：对象不存在（用于区分"仓库没有 AGENTS.md"与认证失败）
}

func (e *Error) Error() string { return e.Msg }

// Input 是 stdin 的 Source 契约输入。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Config   Config `json:"config"`
	Cursor   string `json:"cursor"`
	Window   struct {
		From string `json:"from"` // 空串：没有下界（首次运行，内核传 ""）
		To   string `json:"to"`
	} `json:"window"`
	CredentialsPath string `json:"credentials_path"`
}

// Config 是登记里的 config：要拉的仓库（owner/name）与是否拉通知。
type Config struct {
	Repos         []string `json:"repos"`
	Notifications bool     `json:"notifications"`
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
	SenderVerified  bool     `json:"sender_verified,omitempty"`
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// Run 执行一次拉取。成功时 stdout 先出全部事件再出末行游标；失败时 stdout 不出任何内容、诊断写 stderr，
// 这样"事件落盘后才推进游标"在失败路径上不会留下半截输出。
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, api API, now func() time.Time) int {
	events, cursor, err := pull(ctx, stdin, api, now())
	if err != nil {
		fmt.Fprintln(stderr, "github source:", err)
		var e *Error
		if errors.As(err, &e) {
			return e.Exit
		}
		return ExitTemporary
	}
	enc := json.NewEncoder(stdout)
	for _, ev := range events {
		_ = enc.Encode(ev)
	}
	_ = enc.Encode(map[string]string{"cursor": cursor})
	return ExitOK
}

// RunStdin 先从 stdin 取出内核交来的 credentials_path 以确定凭据目录（newAPI），再按 Run 执行。
// 来源没有登记 credentials_path 属于配置失败。
func RunStdin(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, newAPI func(credentialsPath string) API, now func() time.Time) int {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "github source:", err)
		return ExitTemporary
	}
	var in Input
	if err := json.Unmarshal(raw, &in); err != nil {
		fmt.Fprintln(stderr, "github source: bad input:", err)
		return ExitAuth
	}
	if in.CredentialsPath == "" {
		fmt.Fprintln(stderr, "github source: the source has no credentials_path, register one")
		return ExitAuth
	}
	return Run(ctx, bytes.NewReader(raw), stdout, stderr, newAPI(in.CredentialsPath), now)
}

// parseWindow 解析 window 的一端：空串返回零值（表示未给），否则必须是 RFC3339。
func parseWindow(name, v string) (time.Time, error) {
	if v == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return time.Time{}, &Error{Exit: ExitAuth, Msg: fmt.Sprintf("bad window.%s %q, want RFC3339: %v", name, v, err)}
	}
	return t, nil
}

// stream 是一个流的续传位置。游标是流名（owner/name 或 notifications）到 stream 的 JSON。
type stream struct {
	// At 之前的对象都已处理；updated_at 正好等于 At 的对象可能只处理了一部分（同秒对象、拉取之后才出现的同秒更新）。
	At time.Time `json:"at"`
	// Seen 是 updated_at 等于 At 的对象已发出的事件 id，用来消除同秒歧义：
	// 重读 At 这一秒时已发的不再发，没发的（同一秒后到的对象或状态迁移）照发，游标重放不产生新输出。
	Seen []string `json:"seen,omitempty"`
	// Floor 是状态迁移的下限：早于它的迁移在之前的扫描里已发出。只有一轮扫描读到流尾才上移到 At：
	// 积压分多次运行扫描时，排在后面的对象的 opened、closed 可能早于已推进的 At，用 At 判断就丢了。
	// 这种情况下对象再次更新时可能重发同 id 的事件，由内核去重。
	Floor time.Time `json:"floor,omitzero"`
	// AgentsSHA 是仓库默认分支根目录 AGENTS.md 上次看到的 blob sha（空串：当时没有该文件；nil：从未看过）。
	// 只用于仓库流，见 agentsEvent。
	AgentsSHA *string `json:"agents_sha,omitempty"`
}

// admit 把 updated_at 为 at 的对象的事件并入续传位置，返回其中没发过的。
func (s *stream) admit(at time.Time, evs []event) []event {
	if at.Before(s.At) {
		return nil
	}
	if at.After(s.At) {
		s.At, s.Seen = at, nil
	}
	var fresh []event
	for _, ev := range evs {
		if !slices.Contains(s.Seen, ev.ID) {
			s.Seen = append(s.Seen, ev.ID)
			fresh = append(fresh, ev)
		}
	}
	return fresh
}

func pull(ctx context.Context, stdin io.Reader, api API, now time.Time) ([]event, string, error) {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, "", &Error{Exit: ExitAuth, Msg: "bad input: " + err.Error()}
	}
	if len(in.Config.Repos) == 0 && !in.Config.Notifications {
		return nil, "", &Error{Exit: ExitAuth, Msg: "config needs repos or notifications"}
	}
	for _, r := range in.Config.Repos {
		if !repoRe.MatchString(r) || strings.Contains(r, "..") {
			return nil, "", &Error{Exit: ExitAuth, Msg: fmt.Sprintf("bad repo %q, want owner/name", r)}
		}
	}
	saved := map[string]*stream{}
	if in.Cursor != "" {
		if err := json.Unmarshal([]byte(in.Cursor), &saved); err != nil {
			return nil, "", &Error{Exit: ExitAuth, Msg: "bad cursor: " + err.Error()}
		}
	}
	// to 是内核安排本次运行的时间，早于所有读取。只处理不晚于它的对象：读取途中发生的更新留给下次，
	// 这样不会出现"先读到旧状态的对象、再读到更晚的对象、游标越过前者的新状态"。
	to, err := parseWindow("to", in.Window.To)
	if err != nil {
		return nil, "", err
	}
	if to.IsZero() {
		to = now
	}
	to = to.UTC().Truncate(time.Second) // 上游时间戳精度是秒
	// 新流从窗口起点开始；没有窗口时从现在起算，避免首次把历史全灌进建议流。
	from, err := parseWindow("from", in.Window.From)
	if err != nil {
		return nil, "", err
	}
	start := from.UTC().Truncate(time.Second)
	if from.IsZero() {
		start = to
	}
	streamOf := func(name string) *stream {
		if s := saved[name]; s != nil {
			return s
		}
		return &stream{At: start, Floor: start}
	}

	next := map[string]*stream{}
	var out []event
	budget := maxItems
	for _, repo := range in.Config.Repos {
		s := streamOf(repo)
		next[repo] = s
		if budget <= 0 {
			continue // 前面的仓库用完了本次额度：下次轮到它；积压有限，不会一直轮不到
		}
		evs, used, err := sweepIssues(ctx, api, repo, s, to, budget)
		if err != nil {
			return nil, "", fmt.Errorf("repo %s: %w", repo, err)
		}
		out = append(out, evs...)
		budget -= used
		ev, err := agentsEvent(ctx, api, repo, s, to)
		if err != nil {
			return nil, "", fmt.Errorf("repo %s: %w", repo, err)
		}
		out = append(out, ev...)
	}
	if in.Config.Notifications {
		s := streamOf(notifStream)
		next[notifStream] = s
		evs, err := sweepNotifications(ctx, api, s, to)
		if err != nil {
			return nil, "", fmt.Errorf("notifications: %w", err)
		}
		out = append(out, evs...)
	}
	cursor, err := json.Marshal(next)
	if err != nil {
		return nil, "", err
	}
	return out, string(cursor), nil
}

// sweepIssues 从 s.At 起按 updated_at 升序读一个仓库（issue 列表含 PR），处理至多约 budget 个新对象并推进 s。
// 每页读完后把 since 收到已读的最新时间再从第一页读：页码翻页时，读取途中被更新的对象移到队尾，
// 后面的对象前移一位，会有对象落在两页之间读不到。一整页都是同一秒时 since 无法前进，才翻页。
func sweepIssues(ctx context.Context, api API, repo string, s *stream, to time.Time, budget int) ([]event, int, error) {
	var out []event
	used := 0
	since, page := s.At, 1
	for {
		body, err := api.Get(ctx, fmt.Sprintf("/repos/%s/issues?state=all&sort=updated&direction=asc&per_page=%d&since=%s&page=%d",
			repo, perPage, since.UTC().Format(time.RFC3339), page))
		if err != nil {
			return nil, 0, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, 0, fmt.Errorf("unexpected response: %w", err)
		}
		for _, raw := range items {
			var it issueRaw
			if err := json.Unmarshal(raw, &it); err != nil || it.Number == 0 || it.UpdatedAt.IsZero() {
				return nil, 0, fmt.Errorf("bad issue item")
			}
			if it.UpdatedAt.After(to) { // 升序：其后都晚于窗口。这些对象的早期迁移还没读到，Floor 不动
				return out, used, nil
			}
			if evs := s.admit(it.UpdatedAt, issueEvents(repo, it, s.Floor)); len(evs) > 0 {
				out = append(out, evs...)
				used++
			}
		}
		if len(items) < perPage {
			s.Floor = s.At
			return out, used, nil
		}
		if used >= budget {
			return out, used, nil
		}
		if s.At.After(since) {
			since, page = s.At, 1
		} else {
			page++
		}
	}
}

// agentsEvent 比对仓库默认分支根目录 AGENTS.md 的 blob sha 与游标里的上一次，变化（含新建、删除）就发一条
// repo.agents_md_changed，内核据此做与 apa agents check 同一检测（§12.5）。
// 首次看到文件也发：已有手写 AGENTS.md 的仓库首次检查本就落入漂移；首次没有文件则只记下，不发。
// 读取失败不推进 sha，下次重试；id 含新 sha 与本次 window.to：sha 回到历史值（aaa→bbb→aaa）也是新的一次变化，id 不会撞上内核已去重的旧 id；
// 同一游标、同一 window.to 重放，id 不变。
func agentsEvent(ctx context.Context, api API, repo string, s *stream, to time.Time) ([]event, error) {
	sha, link := "", ""
	body, err := api.Get(ctx, "/repos/"+repo+"/contents/AGENTS.md")
	var apiErr *Error
	switch {
	case errors.As(err, &apiErr) && apiErr.NotFound: // 仓库本身已能读（issue 扫描成功），404 就是没有该文件
	case err != nil:
		return nil, err
	default:
		var f struct {
			Type    string `json:"type"`
			SHA     string `json:"sha"`
			HTMLURL string `json:"html_url"`
		}
		if err := json.Unmarshal(body, &f); err != nil || f.Type != "file" || f.SHA == "" {
			return nil, fmt.Errorf("unexpected AGENTS.md response")
		}
		sha, link = f.SHA, f.HTMLURL
	}
	prev := s.AgentsSHA
	s.AgentsSHA = &sha
	if sha == "" && prev == nil || prev != nil && *prev == sha {
		return nil, nil
	}
	suffix, summary := fmt.Sprintf("%s-%d", sha, to.Unix()), "AGENTS.md changed on the default branch"
	if sha == "" {
		suffix, summary = fmt.Sprintf("removed-%d", to.Unix()), "AGENTS.md was removed from the default branch"
	}
	return []event{{
		ID:              fmt.Sprintf("repo-%s-agents-md-%s", strings.ReplaceAll(repo, "/", "-"), suffix),
		Type:            "repo.agents_md_changed",
		Subject:         repo + " AGENTS.md",
		Summary:         summary,
		Link:            link,
		OccurredAt:      to.UTC().Format(time.RFC3339),
		CorrelationKeys: []string{repo},
	}}, nil
}

type issueRaw struct {
	Number      int                    `json:"number"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body"`
	HTMLURL     string                 `json:"html_url"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	ClosedAt    time.Time              `json:"closed_at"`
	User        struct{ Login string } `json:"user"`
	PullRequest *struct {
		MergedAt time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

// issueEvents 把对象当前的状态映射成事件（规则见包注释），只发不早于 floor 的。
func issueEvents(repo string, it issueRaw, floor time.Time) []event {
	kind := "issue"
	if it.PullRequest != nil {
		kind = "pr"
	}
	key := fmt.Sprintf("%s#%d", repo, it.Number)
	var evs []event
	add := func(typ string, at time.Time, suffix string) {
		if at.Before(floor) {
			return
		}
		ev := event{
			ID:              fmt.Sprintf("%s-%s-%s", kind, strings.ReplaceAll(key, "/", "-"), suffix),
			Type:            kind + "." + typ,
			Subject:         it.Title,
			Summary:         clip(it.Body, sumLimit),
			Link:            it.HTMLURL,
			OccurredAt:      at.UTC().Format(time.RFC3339),
			CorrelationKeys: []string{key},
			Sender:          it.User.Login,
			SenderVerified:  it.User.Login != "" && typ == "opened",
		}
		if len([]rune(it.Body)) > sumLimit { // 摘要已含全文时不重复，省事件体积
			ev.Detail = clip(it.Body, bodyLimit)
		}
		evs = append(evs, ev)
	}
	latest := it.CreatedAt
	add("opened", it.CreatedAt, "opened")
	if it.PullRequest != nil && !it.PullRequest.MergedAt.IsZero() {
		add("merged", it.PullRequest.MergedAt, "merged")
		latest = later(latest, it.PullRequest.MergedAt)
	} else if !it.ClosedAt.IsZero() {
		add("closed", it.ClosedAt, fmt.Sprintf("closed-%d", it.ClosedAt.Unix()))
		latest = later(latest, it.ClosedAt)
	}
	if it.UpdatedAt.After(latest) {
		add("updated", it.UpdatedAt, fmt.Sprintf("updated-%d", it.UpdatedAt.Unix()))
	}
	return evs
}

func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// sweepNotifications 读 [s.At, to] 内的通知并推进 s。通知接口只能按 updated_at 倒序读，积压超过一次能读的量时，
// 先读到的是最新的一段，最旧的读不到，游标也无法越过中间的空洞：所以把时间窗对半缩小，直到窗内读得完，
// 只处理这段最旧的，游标推进到窗尾，下次接着读。缩到一秒还读不完时不设页数上限，保证一定有进展。
func sweepNotifications(ctx context.Context, api API, s *stream, to time.Time) ([]event, error) {
	upper := to.Add(time.Second) // before 不含端点
	shrunk := false
	var items []notifRaw
	for {
		var full bool
		var err error
		if items, full, err = readNotifications(ctx, api, s.At, upper); err != nil {
			return nil, err
		}
		if !full {
			break
		}
		upper, shrunk = s.At.Add(upper.Sub(s.At)/2).Truncate(time.Second), true
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].UpdatedAt.Before(items[j].UpdatedAt) })
	var out []event
	for _, n := range items {
		if !n.UpdatedAt.Before(upper) {
			continue
		}
		out = append(out, s.admit(n.UpdatedAt, []event{notificationEvent(n)})...)
	}
	if shrunk { // 窗内全部处理完，窗可能是空的，游标也要越过它
		s.At, s.Seen = upper, nil
	}
	return out, nil
}

// readNotifications 读 [since, before) 内的全部通知（可能有重复，由 admit 去掉）。翻页方式同 sweepIssues，方向相反。
// 超过 maxNotifPages 且时间窗大于一秒时 full 为真。
func readNotifications(ctx context.Context, api API, since, before time.Time) (items []notifRaw, full bool, err error) {
	capped := before.Sub(since) > time.Second
	upper, page := before, 1
	for pages := 1; ; pages++ {
		// 接口未写明 since 是否含端点，多取一秒，由 admit 过滤
		body, err := api.Get(ctx, fmt.Sprintf("/notifications?all=true&per_page=%d&since=%s&before=%s&page=%d",
			perPage, since.Add(-time.Second).UTC().Format(time.RFC3339), upper.UTC().Format(time.RFC3339), page))
		if err != nil {
			return nil, false, err
		}
		var got []notifRaw
		if err := json.Unmarshal(body, &got); err != nil {
			return nil, false, fmt.Errorf("unexpected response: %w", err)
		}
		for _, n := range got {
			if n.ID == "" || n.UpdatedAt.IsZero() {
				return nil, false, fmt.Errorf("bad notification item")
			}
		}
		items = append(items, got...)
		if len(got) < perPage {
			return items, false, nil
		}
		if capped && pages >= maxNotifPages {
			return nil, true, nil
		}
		// before 也可能不含端点，收到最旧时间的下一秒，同一秒的不会漏
		if oldest := got[len(got)-1].UpdatedAt.Add(time.Second); oldest.Before(upper) {
			upper, page = oldest, 1
		} else {
			page++
		}
	}
}

type notifRaw struct {
	ID         string    `json:"id"`
	Reason     string    `json:"reason"`
	UpdatedAt  time.Time `json:"updated_at"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Subject struct {
		Title string `json:"title"`
		URL   string `json:"url"`
		Type  string `json:"type"`
	} `json:"subject"`
}

var apiSubjectRe = regexp.MustCompile(`^/repos/([^/]+/[^/]+)/(issues|pulls)/(\d+)$`)

func notificationEvent(n notifRaw) event {
	ev := event{
		ID:         fmt.Sprintf("notif-%s-%d", n.ID, n.UpdatedAt.Unix()),
		Type:       "notification." + n.Reason,
		Subject:    n.Subject.Title,
		Summary:    n.Repository.FullName + " " + n.Subject.Type,
		OccurredAt: n.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if u, err := url.Parse(n.Subject.URL); err == nil {
		if m := apiSubjectRe.FindStringSubmatch(u.Path); m != nil {
			html := map[string]string{"issues": "issues", "pulls": "pull"}[m[2]]
			ev.Link = fmt.Sprintf("https://github.com/%s/%s/%s", m[1], html, m[3])
			ev.CorrelationKeys = []string{m[1] + "#" + m[3]}
		}
	}
	return ev
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
