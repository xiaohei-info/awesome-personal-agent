// Package rss 是 RSS/Atom 拉取程序（设计 §9.1、§9.2）：
// stdin JSON → stdout 标准事件 JSONL（末行新游标）+ 退出码。
// 只经 stdin/stdout 契约与内核交互，不写账本、不访问内核存储。
// 解析用标准库 encoding/xml 做结构映射，不自写 XML 解析器。
//
// 游标是已输出条目中最大发布时间（RFC3339，UTC）。拉取失败（不可达、HTTP 错误、非法 XML）
// 时 stdout 不输出任何内容，内核因此不推进游标。
package rss

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 退出码沿用 §9.2 的 Source 契约。
const (
	ExitOK        = 0
	ExitTransient = 1 // 不可达、超时、5xx/429、非法 XML：退避重试
	ExitAuth      = 2 // 配置不合法或 401/403：找本人
	ExitBadConfig = ExitAuth
	ExitPermanent = 3 // 404/410：停用并产出健康类投递
)

const (
	EventType      = "feed.entry"
	defaultItems   = 20 // stdout 被调用器只保留末 64KB，条目数与正文长度都要有界
	maxSummary     = 400
	maxDetail      = 1500
	maxBody        = 10 << 20
	fetchTimeout   = 30 * time.Second
	epoch          = "1970-01-01T00:00:00Z"
	timeLayoutOut  = time.RFC3339
	maxConfigItems = 200
)

// Input 是 stdin 的 Source 契约输入；window 本程序不用，游标已足够。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Cursor   string `json:"cursor"`
	Config   struct {
		URL      string `json:"url"`
		MaxItems int    `json:"max_items"`
	} `json:"config"`
}

type event struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Subject    string `json:"subject"`
	Summary    string `json:"summary,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Link       string `json:"link,omitempty"`
	OccurredAt string `json:"occurred_at"`
	Sender     string `json:"sender,omitempty"`
}

type entry struct {
	event
	at    time.Time
	dated bool
}

type runErr struct {
	exit int
	msg  string
}

func (e *runErr) Error() string { return e.msg }

func fail(exit int, f string, a ...any) error { return &runErr{exit, fmt.Sprintf(f, a...)} }

// Run 执行一次拉取并返回退出码；诊断写 stderr，成功才写 stdout。
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	lines, err := pull(ctx, stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		var re *runErr
		if errors.As(err, &re) {
			return re.exit
		}
		return ExitTransient
	}
	enc := json.NewEncoder(stdout)
	for _, l := range lines {
		_ = enc.Encode(l)
	}
	return ExitOK
}

func pull(ctx context.Context, stdin io.Reader) ([]any, error) {
	var in Input
	if err := json.NewDecoder(stdin).Decode(&in); err != nil {
		return nil, fail(ExitBadConfig, "stdin 不是合法的 JSON: %v", err)
	}
	u, err := url.Parse(in.Config.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fail(ExitBadConfig, "config.url 必须是 http(s) 地址")
	}
	max := in.Config.MaxItems
	switch {
	case max == 0:
		max = defaultItems
	case max < 0 || max > maxConfigItems:
		return nil, fail(ExitBadConfig, "config.max_items 必须在 1..%d", maxConfigItems)
	}
	body, err := fetch(ctx, in.Config.URL)
	if err != nil {
		return nil, err
	}
	entries, err := parseFeed(body)
	if err != nil {
		return nil, fail(ExitTransient, "订阅内容不是合法的 RSS/Atom: %v", err)
	}

	cursor := epoch
	var since time.Time
	if t, err := time.Parse(time.RFC3339, in.Cursor); err == nil { // 坏游标当作没有：重发由内核去重兜底
		since, cursor = t.UTC(), t.UTC().Format(timeLayoutOut)
	}
	picked := selectNew(entries, since, max)
	out := make([]any, 0, len(picked)+1)
	for _, e := range picked {
		out = append(out, e.event)
		if e.dated && e.at.UTC().Format(timeLayoutOut) > cursor {
			cursor = e.at.UTC().Format(timeLayoutOut)
		}
	}
	return append(out, map[string]string{"cursor": cursor}), nil
}

// selectNew 取游标之后的条目，按时间从旧到新，至多 max 个；
// 同一时刻的条目整组发出，否则严格大于游标的下一轮会漏掉被截掉的同刻条目。
// 无日期的条目每次都发（稳定 id 交给内核去重），不参与游标。
func selectNew(all []entry, since time.Time, max int) []entry {
	var undated, dated []entry
	for _, e := range all {
		switch {
		case !e.dated:
			undated = append(undated, e)
		case e.at.After(since):
			dated = append(dated, e)
		}
	}
	sort.SliceStable(dated, func(i, j int) bool { return dated[i].at.Before(dated[j].at) })
	if len(dated) > max {
		cut := max
		for cut < len(dated) && dated[cut].at.Equal(dated[cut-1].at) {
			cut++
		}
		dated = dated[:cut]
	}
	if len(undated) > max {
		undated = undated[:max]
	}
	return append(undated, dated...)
}

func fetch(ctx context.Context, rawURL string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fail(ExitBadConfig, "config.url 不合法: %v", err)
	}
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/xml, text/xml;q=0.9, */*;q=0.5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fail(ExitTransient, "订阅不可达: %v", err)
	}
	defer resp.Body.Close()
	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return nil, fail(ExitAuth, "订阅拒绝访问: HTTP %d", s)
	case s == http.StatusNotFound || s == http.StatusGone:
		return nil, fail(ExitPermanent, "订阅已不存在: HTTP %d", s)
	case s != http.StatusOK:
		return nil, fail(ExitTransient, "订阅返回 HTTP %d", s)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, fail(ExitTransient, "读取订阅失败: %v", err)
	}
	if len(b) > maxBody {
		return nil, fail(ExitTransient, "订阅超过 %d 字节", maxBody)
	}
	return b, nil
}

// ---- XML 结构 ----

type rssDoc struct {
	Items []struct {
		Title       string `xml:"title"`
		Link        string `xml:"link"`
		GUID        string `xml:"guid"`
		PubDate     string `xml:"pubDate"`
		Description string `xml:"description"`
		Content     string `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
		Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
		Author      string `xml:"author"`
	} `xml:"channel>item"`
}

type atomDoc struct {
	Entries []struct {
		Title   string `xml:"title"`
		ID      string `xml:"id"`
		Updated string `xml:"updated"`
		Pub     string `xml:"published"`
		Summary string `xml:"summary"`
		Content string `xml:"content"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
		Author struct {
			Name string `xml:"name"`
		} `xml:"author"`
	} `xml:"entry"`
}

// parseFeed 按根元素区分 RSS（rss）与 Atom（feed）；其它根元素或语法错误都是非法订阅。
func parseFeed(b []byte) ([]entry, error) {
	root, err := rootName(b)
	if err != nil {
		return nil, err
	}
	switch root {
	case "rss":
		var d rssDoc
		if err := xml.Unmarshal(b, &d); err != nil {
			return nil, err
		}
		var out []entry
		for _, it := range d.Items {
			link := strings.TrimSpace(it.Link)
			author := first(it.Creator, it.Author)
			out = append(out, build(first(it.GUID, link, it.Title), it.Title, it.Description, it.Content, link, author, it.PubDate))
		}
		return compact(out), nil
	case "feed":
		var d atomDoc
		if err := xml.Unmarshal(b, &d); err != nil {
			return nil, err
		}
		var out []entry
		for _, it := range d.Entries {
			link := ""
			for _, l := range it.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = l.Href
					break
				}
			}
			out = append(out, build(first(it.ID, link, it.Title), it.Title, it.Summary, it.Content, link, it.Author.Name, first(it.Updated, it.Pub)))
		}
		return compact(out), nil
	}
	return nil, fmt.Errorf("根元素 %q 不是 rss 或 feed", root)
}

func rootName(b []byte) (string, error) {
	d := xml.NewDecoder(strings.NewReader(string(b)))
	for {
		tok, err := d.Token()
		if err != nil {
			return "", err // 含空内容（EOF）
		}
		if se, ok := tok.(xml.StartElement); ok {
			return se.Name.Local, nil
		}
	}
}

func build(id, title, summary, detail, link, sender, date string) entry {
	e := entry{event: event{
		ID: strings.TrimSpace(id), Type: EventType,
		Subject: first(strings.TrimSpace(title), strings.TrimSpace(link), strings.TrimSpace(id)),
		Summary: clip(strings.TrimSpace(summary), maxSummary),
		Detail:  clip(strings.TrimSpace(detail), maxDetail),
		Link:    strings.TrimSpace(link), Sender: strings.TrimSpace(sender),
	}}
	if t, ok := parseDate(date); ok {
		e.at, e.dated = t.UTC(), true
		e.OccurredAt = e.at.Format(timeLayoutOut)
	} else {
		e.OccurredAt = time.Now().UTC().Format(timeLayoutOut) // 契约要求必填；无日期条目靠稳定 id 去重
	}
	return e
}

// compact 丢掉没有 id 的条目并在同一订阅内按 id 去重。
func compact(in []entry) []entry {
	seen := map[string]bool{}
	var out []entry
	for _, e := range in {
		if e.ID == "" || seen[e.ID] {
			continue
		}
		seen[e.ID] = true
		out = append(out, e)
	}
	return out
}

var dateLayouts = []string{time.RFC1123Z, time.RFC1123, time.RFC3339Nano, time.RFC3339,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2 Jan 2006 15:04:05 -0700"}

func parseDate(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	for _, l := range dateLayouts {
		if t, err := time.Parse(l, s); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func first(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
