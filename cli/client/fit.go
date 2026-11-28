package client

import (
	"encoding/json"
	"unicode/utf8"
)

// Fit 从 out[key] 这个列表的末尾删行，直到整份输出（紧凑 JSON）不超过 budget 个字符；至少留一行。
// finish 在每次量之前按留下的行数补上续页游标与"下一步"，所以这些说明也计入上限，不会因补写而超出。
func Fit(out map[string]any, key string, budget int, finish func(kept int)) {
	rows, _ := out[key].([]any)
	for {
		out[key] = rows
		finish(len(rows))
		b, err := json.Marshal(out)
		if err != nil || utf8.RuneCount(b) <= budget || len(rows) <= 1 {
			return
		}
		rows = rows[:len(rows)-1]
	}
}

// Size 是 v 的紧凑 JSON 字符数。
func Size(v any) int {
	b, _ := json.Marshal(v)
	return utf8.RuneCount(b)
}

// Quote 把一个参数按 shell 单引号写法引起来，用于输出里的"下一步"命令。
func Quote(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, `'\''`...)
			continue
		}
		out = append(out, s[i])
	}
	return string(append(out, '\''))
}
