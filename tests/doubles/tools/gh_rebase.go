package tools

import (
	"regexp"
	"strings"
	"time"
)

// ghBase 是 gh 样本里时间的零点：样本落在 2026-11-10 09:00–14:00Z，即零点之后 1–6 小时。
var ghBase = time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)

var rfc3339Z = regexp.MustCompile(`\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z`)

// ghRebase 把样本时间平移到"安装时刻"之后：<binDir>/gh-epoch 存在（RFC3339）时，样本时间 T 对外呈现为
// epoch + (T - ghBase)。来源程序按内核时钟只读 since 之后、window.to 之前的更新，而内核时钟不会早于真实时钟，
// 固定在 2026-11-10 的样本一旦真实日期越过它就永远读不到；平移后验收用测试时钟推进若干小时即可逐个放出。
// 文件不存在时（单元测试）不平移，样本保持录制时的时间。nil 表示不平移。
type ghRebase struct{ shift time.Duration }

func loadGhRebase(binDir string) *ghRebase {
	t, err := time.Parse(time.RFC3339, readTrim(binDir+"/gh-epoch"))
	if err != nil {
		return nil
	}
	return &ghRebase{shift: t.Sub(ghBase)}
}

func (r *ghRebase) conv(s string, sign time.Duration) string {
	return rfc3339Z.ReplaceAllStringFunc(s, func(m string) string {
		t, err := time.Parse(time.RFC3339, m)
		if err != nil {
			return m
		}
		return t.Add(sign * r.shift).UTC().Format("2006-01-02T15:04:05Z")
	})
}

// args 把请求里的 since 换回样本时间（对外时间 - 平移量）。
func (r *ghRebase) args(args []string) []string {
	if r == nil {
		return args
	}
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = a
		if strings.Contains(a, "since=") {
			head, tail, _ := strings.Cut(a, "since=")
			ts, rest, _ := strings.Cut(tail, "&")
			ts = r.conv(ts, -1)
			if rest != "" {
				ts += "&" + rest
			}
			out[i] = head + "since=" + ts
		}
	}
	return out
}

// output 把响应里的时间平移到安装时刻之后。
func (r *ghRebase) output(s string) string {
	if r == nil {
		return s
	}
	return r.conv(s, 1)
}
