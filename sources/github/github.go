// Package github 是随发行附带的 GitHub 拉取程序（设计 §9.1、§9.2）：
// 按 Source 执行契约（stdin JSON → 标准事件 JSONL + 末行游标 + 退出码）拉取仓库的 issue、PR 与通知。
//
// 复用 gh CLI（§5.7）：本包只做游标与事件映射，经 API 接口取数据。
// 换掉 gh（改用官方 API 库或别的工具）只需另写一个实现 API 的类型并在 cmd/github-source 里替换 NewGH，
// 事件映射、游标与退出码语义不变。
// 凭据由 `github-source auth <source_id>` 写入 <APA_HOME>/sources/<source_id>/，内核不读。
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
	maxPages  = 20
	bodyLimit = 4000
	sumLimit  = 200
)

// API 取一个 GitHub REST 路径（含查询串）的 JSON 响应。失败必须返回 *Error 以便映射退出码；
// 其他错误按临时失败处理。
type API interface {
	Get(ctx context.Context, path string) ([]byte, error)
}

// Error 是带退出码的取数失败。
type Error struct {
	Exit int
	Msg  string
}

func (e *Error) Error() string { return e.Msg }

// Input 是 stdin 的 Source 契约输入。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Config   Config `json:"config"`
	Cursor   string `json:"cursor"`
	Window   struct {
		From time.Time `json:"from"`
		To   time.Time `json:"to"`
	} `json:"window"`
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

// RunStdin 先从 stdin 取出 source_id 以确定该来源的凭据目录（newAPI），再按 Run 执行。
func RunStdin(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer, newAPI func(sourceID string) (API, error), now func() time.Time) int {
	raw, err := io.ReadAll(stdin)
	if err != nil {
		fmt.Fprintln(stderr, "github source:", err)
		return ExitTemporary
	}
	var id struct {
		SourceID string `json:"source_id"`
	}
	if err := json.Unmarshal(raw, &id); err != nil {
		fmt.Fprintln(stderr, "github source: bad input:", err)
		return ExitAuth
	}
	api, err := newAPI(id.SourceID)
	if err != nil {
		fmt.Fprintln(stderr, "github source:", err)
		return ExitAuth
	}
	return Run(ctx, bytes.NewReader(raw), stdout, stderr, api, now)
}

func pull(ctx context.Context, stdin io.Reader, api API, now time.Time) ([]event, string, error) {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, "", &Error{ExitAuth, "bad input: " + err.Error()}
	}
	if len(in.Config.Repos) == 0 && !in.Config.Notifications {
		return nil, "", &Error{ExitAuth, "config needs repos or notifications"}
	}
	for _, r := range in.Config.Repos {
		if !repoRe.MatchString(r) || strings.Contains(r, "..") {
			return nil, "", &Error{ExitAuth, fmt.Sprintf("bad repo %q, want owner/name", r)}
		}
	}
	since, err := sinceOf(in, now)
	if err != nil {
		return nil, "", err
	}

	var (
		entries   []entry
		maxSeen   time.Time
		truncated []time.Time // 被页数上限截断的流，其已读到的最后时间
	)
	note := func(t time.Time) {
		if t.After(maxSeen) {
			maxSeen = t
		}
	}
	for _, repo := range in.Config.Repos {
		items, err := fetchAll(ctx, api, fmt.Sprintf("/repos/%s/issues?state=all&sort=updated&direction=asc&per_page=%d&since=%s",
			repo, perPage, since.Format(time.RFC3339)))
		var trunc *truncatedErr
		if err != nil && !errors.As(err, &trunc) {
			return nil, "", fmt.Errorf("repo %s: %w", repo, err)
		}
		var last time.Time
		for _, raw := range items {
			evs, upd, err := issueEvents(repo, raw, since)
			if err != nil {
				return nil, "", fmt.Errorf("repo %s: %w", repo, err)
			}
			if !in.Window.To.IsZero() && upd.After(in.Window.To) {
				continue
			}
			note(upd)
			last = upd
			entries = append(entries, entry{upd, evs})
		}
		if trunc != nil {
			truncated = append(truncated, last)
		}
	}
	if in.Config.Notifications {
		// 通知按更新时间倒序，截断会丢最旧的一段，无法安全推进游标：整次按临时失败。
		items, err := fetchAll(ctx, api, fmt.Sprintf("/notifications?all=true&per_page=%d&since=%s", perPage, since.Format(time.RFC3339)))
		if err != nil {
			return nil, "", fmt.Errorf("notifications: %w", err)
		}
		for _, raw := range items {
			ev, upd, err := notificationEvent(raw)
			if err != nil {
				return nil, "", fmt.Errorf("notifications: %w", err)
			}
			if !in.Window.To.IsZero() && upd.After(in.Window.To) {
				continue
			}
			note(upd)
			entries = append(entries, entry{upd, []event{ev}})
		}
	}

	next := since // 没读到更新时游标不动；游标含端点，重复的事件由内核按 (去重域, id) 去重
	if !maxSeen.IsZero() {
		next = maxSeen
	}
	if len(truncated) > 0 {
		sort.Slice(truncated, func(i, j int) bool { return truncated[i].Before(truncated[j]) })
		if !truncated[0].IsZero() {
			next = truncated[0]
		}
	}
	events, cut := bound(entries)
	if cut != nil && cut.Before(next) {
		next = *cut
	}
	return events, next.UTC().Format(time.RFC3339), nil
}

// outputBudget 是一次输出的事件字节上限。内核只保留来源 stdout 的末 64KB，达到它就丢开头并扣住游标（§9.2 实现），
// 积压多时会永远重复同一份超量输出；所以这里自己在 64KB 以下截断，游标随之只推进到已输出的位置。
const outputBudget = 48 << 10

type entry struct {
	at  time.Time // 该项的 updated_at，游标按它推进
	evs []event
}

// bound 按 updated_at 升序取到预算为止，只在"同一时刻的一组项"的边界上截断（第一组无论多大都整组取，保证有进展）。
// 被截断时返回最后一个整组输出的时间作为游标上限：下次从它开始，该组已输出的事件由稳定 id 去重，
// 被丢掉的组 updated_at 严格晚于游标，所以它们的 updated 事件（要求严格晚于游标）不会丢。
// 同一时刻的项总量超预算时整组一起输出而超过预算，GitHub 时间戳精度是秒，实际不会到 64KB；
// shortcut: 真遇到时需要改用带 id 的复合游标。
func bound(entries []entry) ([]event, *time.Time) {
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	var out []event
	size := 0
	for i := 0; i < len(entries); {
		j, n := i, 0
		for ; j < len(entries) && entries[j].at.Equal(entries[i].at); j++ {
			for _, ev := range entries[j].evs {
				b, _ := json.Marshal(ev)
				n += len(b) + 1
			}
		}
		if i > 0 && size+n > outputBudget {
			cut := entries[i-1].at
			return out, &cut
		}
		size += n
		for _, e := range entries[i:j] {
			out = append(out, e.evs...)
		}
		i = j
	}
	return out, nil
}

// sinceOf：有游标按游标；首次运行取窗口起点；都没有时从现在起算，避免首次把历史全灌进建议流。
func sinceOf(in Input, now time.Time) (time.Time, error) {
	if in.Cursor != "" {
		t, err := time.Parse(time.RFC3339, in.Cursor)
		if err != nil {
			return time.Time{}, &Error{ExitAuth, "bad cursor: " + err.Error()}
		}
		return t, nil
	}
	if !in.Window.From.IsZero() {
		return in.Window.From, nil
	}
	return now, nil
}

type truncatedErr struct{}

func (*truncatedErr) Error() string { return "page limit reached" }

// fetchAll 翻页取完。达到页数上限时返回已取到的部分和 *truncatedErr。
func fetchAll(ctx context.Context, api API, path string) ([]json.RawMessage, error) {
	var all []json.RawMessage
	for page := 1; page <= maxPages; page++ {
		body, err := api.Get(ctx, fmt.Sprintf("%s&page=%d", path, page))
		if err != nil {
			return nil, err
		}
		var items []json.RawMessage
		if err := json.Unmarshal(body, &items); err != nil {
			return nil, fmt.Errorf("unexpected response: %w", err)
		}
		all = append(all, items...)
		if len(items) < perPage {
			return all, nil
		}
	}
	return all, &truncatedErr{}
}

type issueRaw struct {
	Number      int                    `json:"number"`
	Title       string                 `json:"title"`
	Body        string                 `json:"body"`
	HTMLURL     string                 `json:"html_url"`
	State       string                 `json:"state"`
	CreatedAt   time.Time              `json:"created_at"`
	UpdatedAt   time.Time              `json:"updated_at"`
	ClosedAt    time.Time              `json:"closed_at"`
	User        struct{ Login string } `json:"user"`
	PullRequest *struct {
		MergedAt time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

// issueEvents 把 issues 列表项（含 PR）按各自独立的事实映射成事件，不依赖"本次游标落在哪"：
// opened（created_at>=since）、merged 或 closed（merged_at/closed_at>=since）各有一个稳定 id，同一状态在任何一次运行里 id 都一样；
// updated 只在没有上述事件解释这次变化、且更新时间严格晚于游标时才出（游标含端点，端点上的项已在上次处理过），
// 所以每次变化至多一条 updated，不会每次评论都刷一条以外的噪声。时间仍算已读，由调用方推进游标。
func issueEvents(repo string, raw json.RawMessage, since time.Time) ([]event, time.Time, error) {
	var it issueRaw
	if err := json.Unmarshal(raw, &it); err != nil || it.Number == 0 || it.UpdatedAt.IsZero() {
		return nil, time.Time{}, fmt.Errorf("bad issue item")
	}
	kind := "issue"
	if it.PullRequest != nil {
		kind = "pr"
	}
	key := fmt.Sprintf("%s#%d", repo, it.Number)
	mk := func(typ string, at time.Time, suffix string) event {
		ev := event{
			ID:              fmt.Sprintf("%s-%s-%s", kind, strings.ReplaceAll(key, "/", "-"), suffix),
			Type:            kind + "." + typ,
			Subject:         it.Title,
			Summary:         clip(it.Body, sumLimit),
			Link:            it.HTMLURL,
			OccurredAt:      at.UTC().Format(time.RFC3339),
			CorrelationKeys: []string{key},
			Sender:          it.User.Login,
			// 作者身份由 GitHub 认证过的 API 返回，如实标已验证；但 updated 的变更者未必是作者，那种事件不标。
			SenderVerified: it.User.Login != "" && typ != "updated",
		}
		if len([]rune(it.Body)) > sumLimit { // 摘要已含全文时不重复，省事件体积
			ev.Detail = clip(it.Body, bodyLimit)
		}
		return ev
	}
	var evs []event
	if !it.CreatedAt.Before(since) {
		evs = append(evs, mk("opened", it.CreatedAt, "opened"))
	}
	if it.PullRequest != nil && !it.PullRequest.MergedAt.IsZero() {
		if !it.PullRequest.MergedAt.Before(since) {
			evs = append(evs, mk("merged", it.PullRequest.MergedAt, "merged"))
		}
	} else if !it.ClosedAt.IsZero() && !it.ClosedAt.Before(since) {
		evs = append(evs, mk("closed", it.ClosedAt, "closed-"+fmt.Sprint(it.ClosedAt.Unix())))
	}
	if len(evs) == 0 && it.UpdatedAt.After(since) {
		evs = append(evs, mk("updated", it.UpdatedAt, "updated-"+fmt.Sprint(it.UpdatedAt.Unix())))
	}
	return evs, it.UpdatedAt, nil
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

func notificationEvent(raw json.RawMessage) (event, time.Time, error) {
	var n notifRaw
	if err := json.Unmarshal(raw, &n); err != nil || n.ID == "" || n.UpdatedAt.IsZero() {
		return event{}, time.Time{}, fmt.Errorf("bad notification item")
	}
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
	return ev, n.UpdatedAt, nil
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}
