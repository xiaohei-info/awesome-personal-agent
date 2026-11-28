package why

// 讨论区（§6.3、§15.1）：事项的讨论流就是它的处理经过——发言（进展）带 id 与回复关系，决定、派活与回报、后续消息
// 按时间排在同一条流里，不另存一份。读取有条数与字数上限：从最新一条往前取，装不下的计入 More，用 Before 读更早一页；
// 精简读法不带原始记录（Steps）。最新一条单独就超过字数上限时只截它的正文并标 truncated。裁剪只发生在这次输出，不回写账本（§6.14）。

import (
	"encoding/json"
	"strconv"
	"unicode/utf8"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Thread 是讨论流的一页，按时间先后。More 是比这一页更早、没给出的条数；有更早的时 Before 是读下一页要传的游标。
type Thread struct {
	Entries []ThreadEntry `json:"entries"`
	More    int           `json:"more,omitempty"`
	Before  int64         `json:"before,omitempty"`
}

// ThreadEntry 是讨论流的一行：与经过的一行相同，原始记录只在 Full 时给出。
type ThreadEntry struct {
	Entry
	Steps []Step `json:"steps,omitempty"`
}

// Page 是一次读取的范围。Before 为 0 从最新一条读起；Limit 为 0 取 ThreadLimit；Budget 是整页 JSON 的字数上限（0 不限）；
// Full 带每行的原始记录。
type Page struct {
	Before int64
	Limit  int
	Budget int
	Full   bool
}

const (
	// ThreadLimit 是一页的缺省条数，MaxThreadLimit 是上限。
	ThreadLimit    = 20
	MaxThreadLimit = 100
	// threadEnvelope 是给页外的字段（more、before 与括号）留的字数。
	threadEnvelope = 100
)

// ItemThread 返回事项讨论流的一页。授权与按凭据级别的裁剪同 ItemTrace。
func ItemThread(tx *store.Tx, p policy.Policy, r Request, pg Page) (Thread, error) {
	if pg.Limit == 0 {
		pg.Limit = ThreadLimit
	}
	if pg.Limit < 0 || pg.Limit > MaxThreadLimit {
		return Thread{}, domain.NewError(domain.CodeInvalidRequest, "limit must be between 1 and "+strconv.Itoa(MaxThreadLimit), "pass a smaller limit and page with before")
	}
	t, err := ItemTrace(tx, p, r)
	if err != nil {
		return Thread{}, err
	}
	es := t.Entries
	if pg.Before != 0 {
		i := 0
		for i < len(es) && seqOf(es[i]) != pg.Before {
			i++
		}
		if i == len(es) {
			return Thread{}, domain.NewError(domain.CodeInvalidRequest, "before is not a position in this thread", "pass the before value from the previous page")
		}
		es = es[:i]
	}
	left := pg.Budget - threadEnvelope
	out := Thread{Entries: []ThreadEntry{}}
	n := len(es)
	for n > 0 && len(out.Entries) < pg.Limit {
		e := ThreadEntry{Entry: es[n-1]}
		if pg.Full {
			e.Steps = e.Entry.Steps
		}
		size := sizeOf(e) + 1
		if pg.Budget > 0 && size > left {
			if len(out.Entries) > 0 {
				break
			}
			e, size = cut(e, left)
		}
		left -= size
		out.Entries = append(out.Entries, e)
		n--
	}
	if n > 0 {
		out.More, out.Before = n, seqOf(es[n])
	}
	for i, j := 0, len(out.Entries)-1; i < j; i, j = i+1, j-1 {
		out.Entries[i], out.Entries[j] = out.Entries[j], out.Entries[i]
	}
	return out, nil
}

// seqOf 是一行在账本里的位置：合成它的第一条原始记录的落账序号。事项经过的每一行都来自至少一条记录。
func seqOf(e Entry) int64 {
	if len(e.Steps) == 0 {
		return 0
	}
	return e.Steps[0].Seq
}

func sizeOf(e ThreadEntry) int {
	b, _ := json.Marshal(e)
	return utf8.RuneCount(b)
}

// cut 让单独一行装进 budget：去掉原始记录，正文从尾部截短并标 truncated（全文用 apa item show 读）。
func cut(e ThreadEntry, budget int) (ThreadEntry, int) {
	e.Steps = nil
	text, _ := e.Args["text"].(string)
	args := make(map[string]any, len(e.Args)+1)
	for k, v := range e.Args {
		args[k] = v
	}
	args["truncated"] = true
	e.Args = args
	r := []rune(text)
	for keep := len(r); ; { // 转义让 JSON 比字数长，按超出的量逐次收
		e.Args["text"] = string(r[:keep])
		size := sizeOf(e) + 1
		if size <= budget || keep == 0 {
			return e, size
		}
		keep = max(0, keep-(size-budget))
	}
}
