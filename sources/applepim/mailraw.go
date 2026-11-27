package applepim

import (
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net/mail"
	"strings"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/sources/internal/content"
)

type mailHeader struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type mailAtt struct {
	Name     string `json:"name"`
	MimeType string `json:"mimeType"`
	Size     int64  `json:"size"`
	Inline   bool   `json:"inline"`
}

// mailParts 是一封邮件在适配器里的原文各部分；nil 表示工具没给、也无从得知。
type mailParts struct {
	text, html  string
	hasHTML     bool // 已知有无 HTML（工具给了 htmlContent，或解析了原始邮件）
	headers     []content.Header
	attachments *[]attachment
	missing     []missing
}

// collectMail 合并 mail-cli get 的结构化字段与（若有）原始邮件 source：结构化字段优先，
// 原始邮件只补缺的部分，用标准库解析信头与 MIME 结构（D.8）。
func collectMail(it mailMsg, got mailMsg) mailParts {
	p := mailParts{text: got.Content}
	for _, h := range got.Headers {
		p.headers = append(p.headers, content.Header{Name: h.Name, Value: h.Value})
	}
	if got.HTMLContent != nil {
		p.html, p.hasHTML = *got.HTMLContent, true
	}
	if got.Attachments != nil {
		as := make([]attachment, 0, len(*got.Attachments))
		for _, a := range *got.Attachments {
			as = append(as, attachment{Name: a.Name, MediaType: a.MimeType, Size: a.Size, Inline: a.Inline})
		}
		p.attachments = &as
	}
	if got.Source != nil {
		rp, err := parseRaw(*got.Source)
		if err != nil {
			p.missing = append(p.missing, missing{"original", "原始邮件无法解析: " + err.Error()})
		} else {
			if len(p.headers) == 0 {
				p.headers = rp.headers
			}
			if !p.hasHTML {
				p.html, p.hasHTML = rp.html, true
			}
			if p.text == "" {
				p.text = rp.text
			}
			if p.attachments == nil {
				p.attachments = rp.attachments
			}
			for _, m := range rp.missing {
				if m.Part != "body" || got.Content == "" { // 工具已给正文时，原始邮件解不出的正文部分不算缺
					p.missing = append(p.missing, m)
				}
			}
		}
	}
	if len(p.headers) == 0 {
		// 工具没给信头：只用已知的几项（来自 messages 与 get 本身），并自报不全。
		add := func(n, v string) {
			if v != "" {
				p.headers = append(p.headers, content.Header{Name: n, Value: v})
			}
		}
		add("Message-ID", it.MessageID)
		add("Subject", it.Subject)
		add("From", it.Sender)
		add("Date", it.DateReceived)
		p.missing = append(p.missing, missing{"headers", "mail-cli get 没有给出信头，只有列表里的几项"})
	}
	if p.attachments == nil && !hasMissing(p.missing, "attachments") {
		p.missing = append(p.missing, missing{"attachments", "mail-cli get 没有给出附件清单"})
	}
	if !p.hasHTML {
		p.missing = append(p.missing, missing{"original", "mail-cli get 没有给出 HTML 原文，无法确认邮件有无 HTML 正文"})
	}
	return p
}

func hasMissing(ms []missing, part string) bool {
	for _, m := range ms {
		if m.Part == part {
			return true
		}
	}
	return false
}

// parseRaw 解析 RFC 5322 原始邮件：信头、text/plain 与 text/html 正文、附件清单。
// 只支持 UTF-8 与 ASCII 字符集（标准库没有别的解码器）；其他字符集的那一部分自报缺失。
func parseRaw(src string) (mailParts, error) {
	var p mailParts
	msg, err := mail.ReadMessage(strings.NewReader(src))
	if err != nil {
		return p, err
	}
	for name, vs := range msg.Header {
		for _, v := range vs {
			p.headers = append(p.headers, content.Header{Name: name, Value: v})
		}
	}
	atts := []attachment{}
	p.attachments = &atts
	p.hasHTML = true
	walkPart(&p, msg.Header.Get("Content-Type"), msg.Header.Get("Content-Transfer-Encoding"), msg.Header.Get("Content-Disposition"), msg.Header.Get("Content-Id"), msg.Body)
	return p, nil
}

func walkPart(p *mailParts, ctype, cte, disp, cid string, body io.Reader) {
	mt, params, err := mime.ParseMediaType(ctype)
	if err != nil {
		mt, params = "text/plain", map[string]string{}
	}
	if strings.HasPrefix(mt, "multipart/") {
		mr := multipart.NewReader(body, params["boundary"])
		for {
			part, err := mr.NextRawPart() // Raw：保留传输编码，由下面统一解码
			if err != nil {
				return
			}
			walkPart(p, part.Header.Get("Content-Type"), part.Header.Get("Content-Transfer-Encoding"), part.Header.Get("Content-Disposition"), part.Header.Get("Content-Id"), part)
		}
	}
	data, err := decodeTransfer(body, cte)
	if err != nil {
		p.missing = append(p.missing, missing{"body", fmt.Sprintf("%s 部分的传输编码无法解码: %v", mt, err)})
		return
	}
	dt, dparams, _ := mime.ParseMediaType(disp)
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	if name != "" || dt == "attachment" {
		if name == "" {
			name = "attachment"
		}
		if dec, err := new(mime.WordDecoder).DecodeHeader(name); err == nil {
			name = dec
		}
		*p.attachments = append(*p.attachments, attachment{Name: name, MediaType: mt, Size: int64(len(data)), Inline: dt == "inline" || (dt == "" && cid != "")})
		return
	}
	if mt != "text/plain" && mt != "text/html" {
		return
	}
	if cs := strings.ToLower(params["charset"]); cs != "" && cs != "utf-8" && cs != "us-ascii" || !utf8.Valid(data) {
		part := "body"
		if mt == "text/html" {
			part = "original"
		}
		p.missing = append(p.missing, missing{part, fmt.Sprintf("%s 部分的字符集 %q 无法用标准库解码", mt, params["charset"])})
		return
	}
	if mt == "text/html" {
		p.html += string(data)
	} else {
		p.text += string(data)
	}
}

func decodeTransfer(r io.Reader, cte string) ([]byte, error) {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		return io.ReadAll(base64.NewDecoder(base64.StdEncoding, &stripNL{r}))
	case "quoted-printable":
		return io.ReadAll(quotedprintable.NewReader(r))
	}
	return io.ReadAll(r)
}

// stripNL 去掉 base64 正文里的换行与空白，标准库的解码器不接受它们。
type stripNL struct{ r io.Reader }

func (s *stripNL) Read(b []byte) (int, error) {
	for {
		n, err := s.r.Read(b)
		w := 0
		for _, c := range b[:n] {
			if c != '\r' && c != '\n' && c != ' ' && c != '\t' {
				b[w] = c
				w++
			}
		}
		if w > 0 || err != nil {
			return w, err
		}
	}
}
