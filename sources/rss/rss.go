// Package rss 是 RSS/Atom 拉取程序（设计 §9.1、§9.2）：
// stdin JSON → stdout 标准事件 JSONL（末行新游标）+ 退出码。
// 只经 stdin/stdout 契约与内核交互，不写账本、不访问内核存储。
//
// 解析用标准库 encoding/xml 做结构映射（非严格模式，识别 HTML 实体），字符集转换用
// golang.org/x/net/html/charset。没有选 gofeed：它多带 goquery、json-iterator 等一串依赖，
// 而这里只需要条目的几个字段。换解析库只改 parseFeed。
//
// 事件 id 只由条目决定（guid/id，没有时用 link，再没有用内容哈希），跨次运行稳定，
// 重复由内核按 (去重域, 事件 id) 去掉。游标见 cursor。
// 拉取失败（不可达、HTTP 错误、非法订阅）时 stdout 不输出任何内容，内核因此不推进游标。
package rss

import (
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"golang.org/x/net/html/charset"
)

// 退出码沿用 §9.2 的 Source 契约。
const (
	ExitOK        = 0
	ExitTransient = 1 // 不可达、超时、5xx/429、非法订阅：退避重试
	ExitAuth      = 2 // 配置不合法或 401/403：找本人
	ExitBadConfig = ExitAuth
	ExitPermanent = 3 // 404/410：停用并产出健康类投递
)

const (
	EventType = "feed.entry"
	// defaultItems 是一次运行至多处理的新条目数，积压更多时用游标下次续读。
	defaultItems   = 500
	maxConfigItems = 5000
	// overlap 是重叠窗口：游标时刻之前这段时间内的条目每次都重发，同刻或迟到出现的条目
	// 因此不会漏，已发过的由内核去重。重发量受订阅本身大小约束。
	overlap      = 7 * 24 * time.Hour
	maxSummary   = 400
	maxDetail    = 4000
	maxShort     = 300 // subject、sender 的长度上限
	maxBody      = 10 << 20
	fetchTimeout = 30 * time.Second
)

// Input 是 stdin 的 Source 契约输入；window 本程序不用，游标已足够。
type Input struct {
	Cursor string `json:"cursor"`
	Config struct {
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
	out, err := pull(ctx, stdin)
	if err != nil {
		fmt.Fprintln(stderr, err)
		var re *runErr
		if errors.As(err, &re) {
			return re.exit
		}
		return ExitTransient
	}
	_, _ = stdout.Write(out)
	return ExitOK
}

func pull(ctx context.Context, stdin io.Reader) ([]byte, error) {
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
	var cur cursor
	if in.Cursor != "" {
		if err := json.Unmarshal([]byte(in.Cursor), &cur); err != nil {
			return nil, fail(ExitBadConfig, "游标不是本程序写出的格式: %v", err)
		}
	}
	body, ctype, err := fetch(ctx, in.Config.URL)
	if err != nil {
		return nil, err
	}
	entries, err := parseFeed(body, ctype)
	if err != nil {
		return nil, fail(ExitTransient, "订阅内容不是合法的 RSS/Atom: %v", err)
	}

	picked, next := selectEntries(entries, cur, max)
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	for _, e := range picked {
		if err := enc.Encode(e.event); err != nil {
			return nil, fail(ExitTransient, "编码事件失败: %v", err)
		}
	}
	c, err := json.Marshal(next)
	if err != nil {
		return nil, fail(ExitTransient, "编码游标失败: %v", err)
	}
	if err := enc.Encode(map[string]string{"cursor": string(c)}); err != nil {
		return nil, fail(ExitTransient, "编码游标失败: %v", err)
	}
	return b.Bytes(), nil
}

// cursor 是已处理到的位置：有日期的条目按 (发布时间, id) 排序，(T, ID) 是上次处理到的最后一条。
// More 表示上次因条目数上限停下，这次只往后续读、不重发重叠窗口，积压分批读完前不重复。
// 读完后每次重发 T 之前 overlap 内的条目，所以同刻或迟到出现的条目不会漏。
type cursor struct {
	T    time.Time `json:"t"`
	ID   string    `json:"id"`
	More bool      `json:"more,omitempty"`
}

func (c cursor) before(e entry) bool {
	return e.at.After(c.T) || e.at.Equal(c.T) && e.ID > c.ID
}

// selectEntries 返回本次要输出的条目和新游标。
// 游标之后的条目按 (发布时间, id) 从旧到新取，至多 max 条，其余下次续读；
// 未续读时再加上重叠窗口内已越过的条目。无日期的条目排不上时间线，每次都发、不计入上限，
// 由内核去重；它们的 occurred_at 是拉取时间，内核保留首次拉到的那条。
func selectEntries(all []entry, c cursor, max int) ([]entry, cursor) {
	var dated, out []entry
	for _, e := range all {
		if e.dated {
			dated = append(dated, e)
		} else {
			out = append(out, e)
		}
	}
	slices.SortFunc(dated, func(a, b entry) int {
		return cmp.Or(a.at.Compare(b.at), strings.Compare(a.ID, b.ID))
	})
	next, n := cursor{T: c.T, ID: c.ID}, 0
	for _, e := range dated {
		switch {
		case c.before(e):
			if n == max {
				next.More = true
				return out, next
			}
			out, n = append(out, e), n+1
			next.T, next.ID = e.at, e.ID
		case !c.More && !e.at.Before(c.T.Add(-overlap)):
			out = append(out, e)
		}
	}
	return out, next
}

func fetch(ctx context.Context, rawURL string) ([]byte, string, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", fail(ExitBadConfig, "config.url 不合法: %v", err)
	}
	req.Header.Set("Accept", "application/atom+xml, application/rss+xml, application/xml, text/xml;q=0.9, */*;q=0.5")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, "", fail(ExitTransient, "订阅不可达: %v", err)
	}
	defer resp.Body.Close()
	switch s := resp.StatusCode; {
	case s == http.StatusUnauthorized || s == http.StatusForbidden:
		return nil, "", fail(ExitAuth, "订阅拒绝访问: HTTP %d", s)
	case s == http.StatusNotFound || s == http.StatusGone:
		return nil, "", fail(ExitPermanent, "订阅已不存在: HTTP %d", s)
	case s != http.StatusOK:
		return nil, "", fail(ExitTransient, "订阅返回 HTTP %d", s)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, "", fail(ExitTransient, "读取订阅失败: %v", err)
	}
	if len(b) > maxBody {
		return nil, "", fail(ExitTransient, "订阅超过 %d 字节", maxBody)
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// ---- XML 结构 ----

// text 取元素的文本；元素里是未转义的 HTML 或 Atom 的 xhtml 时，直接文本为空，改取内部原文。
type text struct {
	Chars string `xml:",chardata"`
	Inner string `xml:",innerxml"`
}

func (t text) String() string { return first(t.Chars, t.Inner) }

// rssItem 兼容 RSS 2.0（channel>item）与 RSS 1.0（rdf:RDF 下的 item，id 在 rdf:about，日期在 dc:date）。
type rssItem struct {
	Title       text   `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	About       string `xml:"http://www.w3.org/1999/02/22-rdf-syntax-ns# about,attr"`
	PubDate     string `xml:"pubDate"`
	DCDate      string `xml:"http://purl.org/dc/elements/1.1/ date"`
	Description text   `xml:"description"`
	Content     text   `xml:"http://purl.org/rss/1.0/modules/content/ encoded"`
	Creator     string `xml:"http://purl.org/dc/elements/1.1/ creator"`
	Author      string `xml:"author"`
}

type rssDoc struct {
	Items    []rssItem `xml:"channel>item"`
	RDFItems []rssItem `xml:"item"`
}

type atomDoc struct {
	Entries []struct {
		Title   text   `xml:"title"`
		ID      string `xml:"id"`
		Updated string `xml:"updated"`
		Pub     string `xml:"published"`
		Summary text   `xml:"summary"`
		Content text   `xml:"content"`
		Links   []struct {
			Rel  string `xml:"rel,attr"`
			Href string `xml:"href,attr"`
		} `xml:"link"`
		Author struct {
			Name string `xml:"name"`
		} `xml:"author"`
	} `xml:"entry"`
}

// parseFeed 按根元素区分 RSS 2.0（rss）、RSS 1.0（RDF）与 Atom（feed）；其它根元素或语法错误都是非法订阅。
func parseFeed(b []byte, contentType string) ([]entry, error) {
	d, err := newDecoder(b, contentType)
	if err != nil {
		return nil, err
	}
	var root xml.StartElement
	for {
		tok, err := d.Token()
		if err != nil {
			return nil, err // 含空内容（EOF）
		}
		if se, ok := tok.(xml.StartElement); ok {
			root = se
			break
		}
	}
	var out []entry
	switch root.Name.Local {
	case "rss", "RDF":
		var doc rssDoc
		if err := d.DecodeElement(&doc, &root); err != nil {
			return nil, err
		}
		for _, it := range append(doc.Items, doc.RDFItems...) {
			link := strings.TrimSpace(it.Link)
			out = append(out, build(first(it.GUID, it.About, link), it.Title.String(), it.Description.String(), it.Content.String(),
				link, first(it.Creator, it.Author), first(it.PubDate, it.DCDate)))
		}
	case "feed":
		var doc atomDoc
		if err := d.DecodeElement(&doc, &root); err != nil {
			return nil, err
		}
		for _, it := range doc.Entries {
			link := ""
			for _, l := range it.Links {
				if l.Rel == "" || l.Rel == "alternate" {
					link = strings.TrimSpace(l.Href)
					break
				}
			}
			out = append(out, build(first(it.ID, link), it.Title.String(), it.Summary.String(), it.Content.String(),
				link, it.Author.Name, first(it.Updated, it.Pub)))
		}
	default:
		return nil, fmt.Errorf("根元素 %q 不是 rss、RDF 或 feed", root.Name.Local)
	}
	return compact(out), nil
}

// newDecoder 对真实订阅放宽：不严格（容忍裸 & 等）、识别 &nbsp; 等 HTML 实体。
// 字符集以 XML 声明的 encoding 为准，没有声明时用 HTTP Content-Type 的 charset，都没有按 UTF-8。
// 声明优先于 HTTP：不少服务器不看文件内容就默认补 charset=utf-8（如按内容嗅探的 Go 服务器），声明则是生成者写的。
func newDecoder(b []byte, contentType string) (*xml.Decoder, error) {
	var r io.Reader = bytes.NewReader(b)
	if _, params, _ := mime.ParseMediaType(contentType); params["charset"] != "" && !declaresEncoding(b) {
		var err error
		if r, err = charset.NewReaderLabel(params["charset"], r); err != nil {
			return nil, err
		}
	}
	d := xml.NewDecoder(r)
	d.Strict = false
	d.Entity = xml.HTMLEntity
	d.CharsetReader = charset.NewReaderLabel
	return d, nil
}

// declaresEncoding 判断文档开头的 XML 声明是否写了 encoding。
func declaresEncoding(b []byte) bool {
	b = bytes.TrimLeft(bytes.TrimPrefix(b, []byte("\xef\xbb\xbf")), " \t\r\n")
	end := bytes.Index(b, []byte("?>"))
	return bytes.HasPrefix(b, []byte("<?xml")) && end > 0 && bytes.Contains(b[:end], []byte("encoding"))
}

func build(id, title, summary, detail, link, sender, date string) entry {
	title, summary, detail = strings.TrimSpace(title), strings.TrimSpace(summary), strings.TrimSpace(detail)
	if id = strings.TrimSpace(id); id == "" {
		h := sha256.Sum256([]byte(title + "\x00" + summary + "\x00" + detail + "\x00" + strings.TrimSpace(date)))
		id = "sha256:" + hex.EncodeToString(h[:16])
	}
	e := entry{event: event{
		ID: id, Type: EventType,
		Subject: clip(first(title, link, id), maxShort),
		Summary: clip(summary, maxSummary),
		Detail:  clip(detail, maxDetail),
		Link:    link, Sender: clip(strings.TrimSpace(sender), maxShort),
	}}
	if t, ok := parseDate(date); ok {
		e.at, e.dated = t.UTC(), true
		e.OccurredAt = e.at.Format(time.RFC3339)
	} else {
		e.OccurredAt = time.Now().UTC().Format(time.RFC3339) // 契约要求必填
	}
	return e
}

// compact 在同一订阅内按 id 去重，保留第一条。
func compact(in []entry) []entry {
	seen := map[string]bool{}
	var out []entry
	for _, e := range in {
		if !seen[e.ID] {
			seen[e.ID] = true
			out = append(out, e)
		}
	}
	return out
}

var dateLayouts = []string{time.RFC1123Z, time.RFC1123, time.RFC3339Nano,
	"Mon, 2 Jan 2006 15:04:05 -0700", "Mon, 2 Jan 2006 15:04:05 MST", "2 Jan 2006 15:04:05 -0700", "2 Jan 2006 15:04:05 MST",
	"Mon, 2 Jan 2006 15:04 -0700", "2006-01-02 15:04:05 -0700", "2006-01-02T15:04:05", "2006-01-02 15:04:05", "2006-01-02"}

// parseDate 认 RFC 822/1123、RFC 3339 及常见的不严格写法；没有时区的按 UTC。
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
		if v = strings.TrimSpace(v); v != "" {
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
