package task

import (
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

// 资料文件的目录（§7.3"简报的资料"）：写在资料文件开头，读者先看各段多大、几条、什么时间范围、长什么样，
// 再决定读哪一段的哪一部分；读接口按 list 与 time_fields 定向取片段。
const (
	sampleMaxBytes   = 800 // 样例超过它时把字符串字段裁到 sampleFieldRunes
	sampleFieldRunes = 120
)

// materialTime 是各段条目的时间字段，按先后取第一个有值的（activity 有业务日期时按 on，没有时按写入时刻 at）。
var materialTime = map[string][]string{
	"activity": {"on", "at"}, "schedule": {"start"}, "decisions": {"deadline"}, "due_waiting": {"deadline"},
	"planned": {"planned_for"}, "proposals": {"created_at"}, "digest": {"occurred_at"}, "spot_check": {"judged_at"},
	"open_items": {"updated_at"},
}

type catalogEntry struct {
	Name       string          `json:"name"`
	Bytes      int             `json:"bytes"`             // 该段 Body 的字节数
	Entries    *int            `json:"entries,omitempty"` // 条数；不是条目列表的段没有
	List       string          `json:"list,omitempty"`    // 条目所在位置，如 activity[].entries[]
	TimeFields []string        `json:"time_fields,omitempty"`
	TimeFrom   string          `json:"time_from,omitempty"`
	TimeTo     string          `json:"time_to,omitempty"`
	Totals     map[string]any  `json:"totals,omitempty"` // 段里与条目并列的数字（如 pending_total、not_listed）
	Fields     []string        `json:"fields,omitempty"` // 条目的字段名；不是列表的段是顶层字段名
	Sample     json.RawMessage `json:"sample,omitempty"` // 第一条条目
}

// catalogOf 为每一段出一条目录；Body 不是 JSON 对象的段只给名字与字节数。
func catalogOf(mats []Material) []catalogEntry {
	out := []catalogEntry{}
	for _, m := range mats {
		c := catalogEntry{Name: m.Name, Bytes: len(m.Body)}
		var body map[string]any
		if json.Unmarshal([]byte(m.Body), &body) != nil {
			out = append(out, c)
			continue
		}
		val, ok := body[m.Name]
		if !ok {
			c.Fields = sortedKeys(body)
			out = append(out, c)
			continue
		}
		for k, v := range body {
			if _, num := v.(float64); num && k != m.Name {
				if c.Totals == nil {
					c.Totals = map[string]any{}
				}
				c.Totals[k] = v
			}
		}
		list, path := entriesOf(m.Name, val)
		if path == "" {
			if obj, ok := val.(map[string]any); ok {
				c.Fields = sortedKeys(obj)
			}
			out = append(out, c)
			continue
		}
		n := len(list)
		c.Entries, c.List, c.TimeFields = &n, path, materialTime[m.Name]
		for _, e := range list {
			t := timeOf(e, c.TimeFields)
			if t == "" {
				continue
			}
			if c.TimeFrom == "" || t < c.TimeFrom {
				c.TimeFrom = t
			}
			if t > c.TimeTo {
				c.TimeTo = t
			}
		}
		if n > 0 {
			if obj, ok := list[0].(map[string]any); ok {
				c.Fields = sortedKeys(obj)
				c.Sample = sample(obj)
			}
		}
		out = append(out, c)
	}
	return out
}

// entriesOf 找出段的条目列表：值本身是数组（条目是带 entries 数组的分组时展开分组），或是带 items 数组的对象。
func entriesOf(name string, val any) ([]any, string) {
	switch v := val.(type) {
	case []any:
		grouped := len(v) > 0
		for _, g := range v {
			obj, ok := g.(map[string]any)
			if _, arr := obj["entries"].([]any); !ok || !arr {
				grouped = false
				break
			}
		}
		if !grouped {
			return v, name + "[]"
		}
		var flat []any
		for _, g := range v {
			flat = append(flat, g.(map[string]any)["entries"].([]any)...)
		}
		return flat, name + "[].entries[]"
	case map[string]any:
		if arr, ok := v["items"].([]any); ok {
			return arr, name + ".items[]"
		}
	}
	return nil, ""
}

// timeOf 取条目上第一个有值的时间字段；字符串比较即可，同一段里的时刻都按同一时区写。
func timeOf(e any, fields []string) string {
	obj, ok := e.(map[string]any)
	if !ok {
		return ""
	}
	for _, f := range fields {
		if s, ok := obj[f].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func sample(obj map[string]any) json.RawMessage {
	b, _ := json.Marshal(obj)
	if len(b) <= sampleMaxBytes {
		return b
	}
	cut := map[string]any{}
	for k, v := range obj {
		if s, ok := v.(string); ok && utf8.RuneCountInString(s) > sampleFieldRunes {
			v = string([]rune(s)[:sampleFieldRunes]) + "…"
		}
		cut[k] = v
	}
	b, _ = json.Marshal(cut)
	return b
}

func sortedKeys(m map[string]any) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.SortFunc(ks, strings.Compare)
	return ks
}
