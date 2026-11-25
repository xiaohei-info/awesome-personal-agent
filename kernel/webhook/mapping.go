package webhook

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
)

// exprCtx 是模板取值的资料：请求头（键小写）与解析后的 JSON 体。
type exprCtx struct {
	header map[string]string
	body   any // 体不是 JSON 时为 nil
}

// lookup 取一个表达式的值：header.<名> 或 $.a.b[0].c。取不到返回 ok=false。
func (c exprCtx) lookup(expr string) (string, bool) {
	expr = strings.TrimSpace(expr)
	if name, ok := strings.CutPrefix(expr, "header."); ok {
		v, ok := c.header[strings.ToLower(name)]
		return v, ok
	}
	if !strings.HasPrefix(expr, "$") || c.body == nil {
		return "", false
	}
	cur := c.body
	for _, seg := range splitPath(expr[1:]) {
		switch n := cur.(type) {
		case map[string]any:
			v, ok := n[seg]
			if !ok {
				return "", false
			}
			cur = v
		case []any:
			i, err := strconv.Atoi(seg)
			if err != nil || i < 0 || i >= len(n) {
				return "", false
			}
			cur = n[i]
		default:
			return "", false
		}
	}
	switch v := cur.(type) {
	case nil:
		return "", false
	case string:
		return v, true
	case json.Number:
		return v.String(), true
	case bool:
		return strconv.FormatBool(v), true
	}
	b, _ := json.Marshal(cur) // 对象或数组：原样 JSON
	return string(b), true
}

// splitPath 把 ".a.b[0].c" 拆成 a b 0 c。
func splitPath(p string) []string {
	p = strings.NewReplacer("[", ".", "]", "").Replace(p)
	var out []string
	for _, s := range strings.Split(p, ".") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// render 展开模板里的 {表达式}；取不到的表达式展开为空串。
func (c exprCtx) render(tpl string) string {
	var b strings.Builder
	for {
		i := strings.IndexByte(tpl, '{')
		j := strings.IndexByte(tpl, '}')
		if i < 0 || j < i {
			b.WriteString(tpl)
			return b.String()
		}
		b.WriteString(tpl[:i])
		v, _ := c.lookup(tpl[i+1 : j])
		b.WriteString(v)
		tpl = tpl[j+1:]
	}
}

func newExprCtx(header map[string]string, body []byte) exprCtx {
	c := exprCtx{header: header}
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var v any
	if dec.Decode(&v) == nil {
		c.body = v
	}
	return c
}

// matches 判断映射的 match 是否命中：header 是请求头相等，json_path 加 equals 是体字段相等。
func (c exprCtx) matches(m map[string]any) (bool, error) {
	for k, v := range m {
		switch k {
		case "header":
			hs, ok := v.(map[string]any)
			if !ok {
				return false, fmt.Errorf("match.header must be a map")
			}
			for name, want := range hs {
				if c.header[strings.ToLower(name)] != fmt.Sprint(want) {
					return false, nil
				}
			}
		case "json_path":
			got, _ := c.lookup(fmt.Sprint(v))
			if got != fmt.Sprint(m["equals"]) {
				return false, nil
			}
		case "equals":
		default:
			return false, fmt.Errorf("match condition %q is not supported (header, json_path, equals)", k)
		}
	}
	return true, nil
}

// mapEvent 按字段映射把 payload 转成标准事件（§9.2）。
// 缺省：id 取投递键、subject 取 type、occurred_at 取接收时间；没写 detail 就不带 detail（原始 payload 只留在接收记录里，不充当正文）；sender_verified 只在声明 on_signature 且签名方案通过时为真。
func mapEvent(m packs.WebhookMapping, c exprCtx, body []byte, deliveryKey string, received time.Time, signed bool) (runner.IngestEvent, error) {
	f := func(name string) string {
		s, _ := m.Fields[name].(string)
		return c.render(s)
	}
	ev := runner.IngestEvent{ID: f("id"), Type: f("type"), Subject: f("subject"), Summary: f("summary"), Detail: f("detail"),
		Link: f("link"), Sender: f("sender"), OccurredAt: received,
		// 统一时间字段（§9.2）：字段声明里有这几个键就能带上，不用写代码；格式与一致性由 ValidEvent 校验。
		DueAt: f("due_at"), StartsAt: f("starts_at"), EndsAt: f("ends_at")}
	switch strings.ToLower(f("all_day")) {
	case "true", "1", "yes":
		ev.AllDay = true
	}
	if ev.ID == "" {
		ev.ID = deliveryKey
	}
	if ev.Subject == "" {
		ev.Subject = ev.Type
	}
	if at := f("occurred_at"); at != "" {
		t, err := time.Parse(time.RFC3339, at)
		if err != nil {
			return ev, fmt.Errorf("occurred_at %q is not RFC3339", at)
		}
		ev.OccurredAt = t.UTC()
	}
	switch keys := m.Fields["correlation_keys"].(type) {
	case []any:
		for _, k := range keys {
			if s, ok := k.(string); ok {
				if v := c.render(s); v != "" {
					ev.CorrelationKeys = append(ev.CorrelationKeys, v)
				}
			}
		}
	case nil:
	default:
		return ev, fmt.Errorf("correlation_keys must be a list of templates")
	}
	// 请求体永久保存为这条事件原文的 original 部分，媒体类型取请求的 Content-Type（§9.5）；它不充当正文。
	// shortcut: 事件经 JSON 流转，非 UTF-8 的请求体会带替换字符；webhook 载荷实际是 JSON 或表单文本，出现二进制体时再给 original 加编码字段。
	ct := c.header["content-type"]
	if ct == "" {
		ct = "application/octet-stream"
	}
	ev.Original = &runner.Original{MediaType: ct, Text: string(body)}
	ev.SenderVerified = m.SenderVerified == "on_signature" && signed && ev.Sender != ""
	return ev, runner.ValidEvent(ev)
}
