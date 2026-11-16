package runner

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 来源原文的部分与完整程度（§6.10）。原文按部分存在 sources_runner_event_part，与事件行同一事务落盘；
// 入账路径上没有任何按长度裁剪原文的代码（§6.14 第 18 条）：事件行的 detail 列只是正文的前 PreviewChars 个字符，供预览。
const (
	PartBody        = "body"
	PartOriginal    = "original"
	PartHeaders     = "headers"
	PartAttachments = "attachments"
	PartMessages    = "messages"

	StateFull    = "full"
	StatePartial = "partial"
	StateLegacy  = "legacy"
	StatePurged  = "purged"

	// PreviewChars 是事件行为正文留的预览字数。
	PreviewChars = 2000

	// DefaultMaxEvent 是 max_source_event_mb 的默认值（字节）。
	DefaultMaxEvent = 16 << 20

	mediaText = "text/plain; charset=utf-8"
	mediaJSON = "application/json"
)

var partNames = []string{PartBody, PartOriginal, PartHeaders, PartAttachments, PartMessages}

// Parts 是原文各部分的名字，按固定顺序。
var Parts = partNames

// Original 是未经转换的原始正文与它的媒体类型（HTML 邮件的 HTML、接口返回的那一条 JSON、webhook 的请求体）。内核只存不解析。
type Original struct {
	MediaType string `json:"media_type"`
	Text      string `json:"text"`
}

// AttachmentRef 是附件清单的一项：只有元数据，没有内容；Ref 是来源程序自己认得的取回句柄。
type AttachmentRef struct {
	Name      string `json:"name"`
	MediaType string `json:"media_type,omitempty"`
	Size      int64  `json:"size"`
	Inline    bool   `json:"inline,omitempty"`
	Ref       string `json:"ref,omitempty"`
}

// Missing 是来源程序自报没能取到的一部分与原因。
type Missing struct {
	Part   string `json:"part"`
	Reason string `json:"reason,omitempty"`
}

// Declared 是标准事件的 content 字段：来源程序自报完整程度。不给按 full 记。
type Declared struct {
	State   string    `json:"state,omitempty"`
	Missing []Missing `json:"missing,omitempty"`
}

// validateRaw 校验原文字段的形状；只拒绝形状不对的行，不看长度（长度由 tooLarge 管）。
func validateRaw(ev parsedEvent) error {
	if o := ev.Original; o != nil && strings.TrimSpace(o.MediaType) == "" {
		return fmt.Errorf("bad field: original.media_type is empty")
	}
	for name, v := range ev.Headers {
		switch x := v.(type) {
		case string:
		case []any:
			for _, e := range x {
				if _, ok := e.(string); !ok {
					return fmt.Errorf("bad field: headers.%s must be a string or an array of strings", name)
				}
			}
		default:
			return fmt.Errorf("bad field: headers.%s must be a string or an array of strings", name)
		}
	}
	for i, a := range ev.Attachments {
		if strings.TrimSpace(a.Name) == "" {
			return fmt.Errorf("bad field: attachments[%d].name is empty", i)
		}
	}
	c := ev.Content
	if c == nil {
		return nil
	}
	switch c.State {
	case "", StateFull:
	case StatePartial:
		if len(c.Missing) == 0 {
			return fmt.Errorf("bad field: content.missing must say which part is missing when state is partial")
		}
	default:
		return fmt.Errorf("bad field: content.state must be full or partial, got %q", c.State)
	}
	for i, m := range c.Missing {
		if !slices.Contains(partNames, m.Part) {
			return fmt.Errorf("bad field: content.missing[%d].part %q is not one of %s", i, m.Part, strings.Join(partNames, ", "))
		}
	}
	return nil
}

type partRow struct {
	part, media string
	content     []byte
}

// partsOf 按固定顺序列出事件带来的各部分；没有内容的部分不存。
func partsOf(ev parsedEvent) []partRow {
	var out []partRow
	add := func(part, media string, content []byte) {
		if len(content) > 0 {
			out = append(out, partRow{part, media, content})
		}
	}
	add(PartBody, mediaText, []byte(ev.Detail))
	if ev.Original != nil {
		add(PartOriginal, ev.Original.MediaType, []byte(ev.Original.Text))
	}
	asJSON := func(part string, v any, empty bool) {
		if empty {
			return
		}
		b, err := json.Marshal(v)
		if err != nil {
			panic(err) // 来自刚解码的 JSON，不会失败
		}
		add(part, mediaJSON, b)
	}
	asJSON(PartHeaders, ev.Headers, len(ev.Headers) == 0)
	asJSON(PartAttachments, ev.Attachments, len(ev.Attachments) == 0)
	asJSON(PartMessages, ev.Items, len(ev.Items) == 0)
	return out
}

func partsBytes(ps []partRow) (n int64) {
	for _, p := range ps {
		n += int64(len(p.content))
	}
	return
}

// tooLarge 在原文总量超过 max（字节）时给出整行拒收的原因（event_too_large，带实际字节数）；max ≤ 0 表示不设限。
func tooLarge(ev parsedEvent, max int) string {
	if n := partsBytes(partsOf(ev)); max > 0 && n > int64(max) {
		return fmt.Sprintf("event_too_large: %d bytes exceeds the limit of %d bytes (max_source_event_mb)", n, max)
	}
	return ""
}

// previewOf 取正文的前 PreviewChars 个字符（按字符不按字节，不劈开多字节字符）。
func previewOf(s string) string {
	n := 0
	for i := range s {
		if n == PreviewChars {
			return s[:i]
		}
		n++
	}
	return s
}

func shaHex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// storeParts 在调用方事务里写事件的各部分，并返回事件行要记的原文总字节数、完整程度与自报缺失（JSON，无则空串）。
func storeParts(tx *store.Tx, eventID string, ev parsedEvent) (bytes int64, state, missing string, err error) {
	ps := partsOf(ev)
	for _, p := range ps {
		if _, err = tx.Exec(`INSERT INTO sources_runner_event_part (event_id, part, media_type, bytes, sha256, content) VALUES (?,?,?,?,?,?)`,
			eventID, p.part, p.media, int64(len(p.content)), shaHex(p.content), p.content); err != nil {
			return
		}
	}
	state = StateFull
	if c := ev.Content; c != nil && c.State == StatePartial {
		state = StatePartial
		b, merr := json.Marshal(c.Missing)
		if merr != nil {
			return 0, "", "", merr
		}
		missing = string(b)
	}
	return partsBytes(ps), state, missing, nil
}

// ---- 读取（raw-read-api 等经这两个函数取原文；先看原文表，没有再回退旧列） ----

// PartInfo 是清单里的一项。
type PartInfo struct {
	Part      string `json:"part"`
	MediaType string `json:"media_type"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
}

// Manifest 是事件原文的清单：各部分的类型与字节数、总字节数、完整程度；partial 时 Missing 写明缺哪一部分与原因。
type Manifest struct {
	State   string     `json:"state"`
	Bytes   int64      `json:"bytes"`
	Parts   []PartInfo `json:"parts"`
	Missing []Missing  `json:"missing,omitempty"`
}

// PartData 是一个部分的全文。
type PartData struct {
	PartInfo
	Content []byte
}

type contentRow struct {
	state, missing, detail, messages string
}

func readContentRow(tx *store.Tx, id string) (c contentRow, err error) {
	rows, err := tx.Query(`SELECT content_state, content_missing, detail, messages FROM sources_runner_event WHERE id = ?`, id)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return c, err
		}
		return c, domain.NewError(domain.CodeNotFound, "source event "+id+" not found", "list the source's events")
	}
	err = rows.Scan(&c.state, &c.missing, &c.detail, &c.messages)
	return c, err
}

// legacyParts 是升级前落盘的事件在旧列里有的部分（正文与聚合消息）；其余部分当时没有存。
func (c contentRow) legacyParts() []PartData {
	var out []PartData
	for _, l := range []struct{ part, media, text string }{{PartBody, mediaText, c.detail}, {PartMessages, mediaJSON, c.messages}} {
		if l.text != "" {
			b := []byte(l.text)
			out = append(out, PartData{PartInfo{l.part, l.media, int64(len(b)), shaHex(b)}, b})
		}
	}
	return out
}

// ContentOf 读事件原文的清单。升级前落盘的事件（legacy）没有原文表的行，清单按旧列现算。
func ContentOf(tx *store.Tx, id string) (Manifest, error) {
	c, err := readContentRow(tx, id)
	if err != nil {
		return Manifest{}, err
	}
	mf := Manifest{State: c.state, Parts: []PartInfo{}}
	rows, err := tx.Query(`SELECT part, media_type, bytes, sha256 FROM sources_runner_event_part WHERE event_id = ?`, id)
	if err != nil {
		return mf, err
	}
	defer rows.Close()
	for rows.Next() {
		var p PartInfo
		if err := rows.Scan(&p.Part, &p.MediaType, &p.Bytes, &p.SHA256); err != nil {
			return mf, err
		}
		mf.Parts = append(mf.Parts, p)
	}
	if err := rows.Err(); err != nil {
		return mf, err
	}
	if len(mf.Parts) == 0 && c.state == StateLegacy {
		for _, p := range c.legacyParts() {
			mf.Parts = append(mf.Parts, p.PartInfo)
		}
	}
	slices.SortFunc(mf.Parts, func(a, b PartInfo) int { return slices.Index(partNames, a.Part) - slices.Index(partNames, b.Part) })
	for _, p := range mf.Parts {
		mf.Bytes += p.Bytes
	}
	if c.missing != "" {
		if err := json.Unmarshal([]byte(c.missing), &mf.Missing); err != nil {
			return mf, fmt.Errorf("runner: event %s content_missing: %w", id, err)
		}
	}
	return mf, nil
}

// ReadPart 读事件的一个部分的全文。没有这个部分（来源没带、升级前没存、已按保留期清除）是 not_found。
func ReadPart(tx *store.Tx, id, part string) (PartData, error) {
	if !slices.Contains(partNames, part) {
		return PartData{}, domain.NewError(domain.CodeInvalidRequest, "unknown part "+part, "use one of "+strings.Join(partNames, ", "))
	}
	c, err := readContentRow(tx, id)
	if err != nil {
		return PartData{}, err
	}
	rows, err := tx.Query(`SELECT media_type, bytes, sha256, content FROM sources_runner_event_part WHERE event_id = ? AND part = ?`, id, part)
	if err != nil {
		return PartData{}, err
	}
	defer rows.Close()
	if rows.Next() {
		p := PartData{PartInfo: PartInfo{Part: part}}
		err := rows.Scan(&p.MediaType, &p.Bytes, &p.SHA256, &p.Content)
		return p, err
	}
	if err := rows.Err(); err != nil {
		return PartData{}, err
	}
	if c.state == StateLegacy {
		for _, p := range c.legacyParts() {
			if p.Part == part {
				return p, nil
			}
		}
	}
	return PartData{}, domain.NewError(domain.CodeNotFound, fmt.Sprintf("source event %s has no %s part (content %s)", id, part, c.state), "read the content manifest for the parts it has")
}

// Preview 是列表里 detail 的预览：全文的前 PreviewChars 个字符。升级前落盘的事件，行里的 detail 是全文，读出后也经它截取。
func Preview(s string) string { return previewOf(s) }

// Usage 是一个来源的原文占用（GET /storage 与来源 stats）。
type Usage struct {
	Events       int64
	ContentBytes int64
	Oldest       time.Time // 最早一条的发生时间；没有事件为零值
}

// UsageBySource 按来源汇总事件数与原文字节数。升级前落盘的事件没有记字节数（NULL），按原列（正文与聚合消息）的长度现算，
// 与 ContentOf 对这类事件的清单同一口径。
func UsageBySource(tx *store.Tx) (map[string]Usage, error) {
	rows, err := tx.Query(`SELECT source_id, COUNT(*),
		COALESCE(SUM(COALESCE(content_bytes, LENGTH(CAST(detail AS BLOB)) + LENGTH(CAST(messages AS BLOB)))), 0), MIN(occurred_at)
		FROM sources_runner_event GROUP BY source_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Usage{}
	for rows.Next() {
		var (
			id     string
			u      Usage
			oldest int64
		)
		if err := rows.Scan(&id, &u.Events, &u.ContentBytes, &oldest); err != nil {
			return nil, err
		}
		u.Oldest = time.Unix(0, oldest).UTC()
		out[id] = u
	}
	return out, rows.Err()
}
