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
		events    []event
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
			ev, upd, ok, err := issueEvent(repo, raw, since)
			if err != nil {
				return nil, "", fmt.Errorf("repo %s: %w", repo, err)
			}
			if !in.Window.To.IsZero() && upd.After(in.Window.To) {
				continue
			}
			note(upd)
			last = upd
			if ok {
				events = append(events, ev)
			}
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
			events = append(events, ev)
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
	return events, next.UTC().Format(time.RFC3339), nil
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

// issueEvent 把 issues 列表项（含 PR）映射成事件。ok 为假表示该项不产生事件但时间仍算已读。
// 事件 id 稳定：opened/closed/merged 每个对象只有一条；updated 带更新时间。
func issueEvent(repo string, raw json.RawMessage, since time.Time) (event, time.Time, bool, error) {
	var it issueRaw
	if err := json.Unmarshal(raw, &it); err != nil || it.Number == 0 || it.UpdatedAt.IsZero() {
		return event{}, time.Time{}, false, fmt.Errorf("bad issue item")
	}
	kind := "issue"
	if it.PullRequest != nil {
		kind = "pr"
	}
	key := fmt.Sprintf("%s#%d", repo, it.Number)
	typ, occurred, suffix := "updated", it.UpdatedAt, "updated-"+fmt.Sprint(it.UpdatedAt.Unix())
	switch {
	case !it.CreatedAt.Before(since):
		typ, occurred, suffix = "opened", it.CreatedAt, "opened"
	case it.PullRequest != nil && !it.PullRequest.MergedAt.IsZero() && !it.PullRequest.MergedAt.Before(since):
		typ, occurred, suffix = "merged", it.PullRequest.MergedAt, "merged"
	case !it.ClosedAt.IsZero() && !it.ClosedAt.Before(since):
		typ, occurred, suffix = "closed", it.ClosedAt, "closed"
	}
	return event{
		ID:              fmt.Sprintf("%s-%s-%s", kind, strings.ReplaceAll(key, "/", "-"), suffix),
		Type:            kind + "." + typ,
		Subject:         it.Title,
		Summary:         clip(it.Body, sumLimit),
		Detail:          clip(it.Body, bodyLimit),
		Link:            it.HTMLURL,
		OccurredAt:      occurred.UTC().Format(time.RFC3339),
		CorrelationKeys: []string{key},
		Sender:          it.User.Login,
		// 作者身份由 GitHub 认证过的 API 返回，但是否可信到能进无人值守白名单由白名单决定；此处如实标已验证。
		SenderVerified: it.User.Login != "",
	}, it.UpdatedAt, true, nil
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
