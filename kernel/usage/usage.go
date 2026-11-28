// Package usage 记录并汇总 token 用量：内核直接发出的模型请求，与 APA 拉起的 Agent 运行。
// 数字只取端点或运行自己回报的值，没回报的只计次数、不估算；记录不含内容与事项 id。
// 本包是 usage_record 的唯一写入方；不判断、不限额、不计费。
package usage

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// Module 是本模块在存储层登记的迁移。run_id 只用来让同一次运行最多记一条。
var Module = store.Module{Name: "usage", Migrations: []store.Migration{{
	Version: 1, Ledger: 12, Name: "ledger-v12-usage",
	SQL: `CREATE TABLE usage_record (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	at INTEGER NOT NULL,
	part TEXT NOT NULL,
	purpose TEXT NOT NULL,
	agent TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL DEFAULT '',
	run_id TEXT NOT NULL DEFAULT '',
	requests INTEGER NOT NULL,
	reported INTEGER NOT NULL,
	input_tokens INTEGER NOT NULL,
	output_tokens INTEGER NOT NULL,
	cache_read_tokens INTEGER NOT NULL,
	cache_write_tokens INTEGER NOT NULL
);
CREATE INDEX usage_record_at ON usage_record(at);
CREATE UNIQUE INDEX usage_record_run ON usage_record(run_id) WHERE run_id <> '';`,
}, {
	// 账本结构版本 16：Agent 运行的执行者声明里没有 headless.usage 时记 1（"未提供"，与声明了但没读到的"未回报"分开）。
	// 已有记录取 0，仍按未回报显示：当时是否声明没有留下。
	Version: 2, Ledger: 16, Name: "ledger-v16-usage-undeclared",
	SQL: `ALTER TABLE usage_record ADD COLUMN undeclared INTEGER NOT NULL DEFAULT 0;`,
}}}

// 三个部分。
const (
	PartJudge = "judge" // 判断请求（各级）
	PartModel = "model" // 其余直接发出的模型请求
	PartAgent = "agent" // Agent 后台运行
)

// Counts 是一组计数。Requests 是请求数（Agent 部分是运行次数），Reported 是其中回报了用量的次数，
// Undeclared 是执行者声明里没有 headless.usage、所以不会有用量的运行次数（"未提供"；其余没回报的是"未回报"）；
// InputTokens 含缓存读写的部分。
type Counts struct {
	Requests         int64 `json:"requests"`
	Reported         int64 `json:"reported"`
	Undeclared       int64 `json:"undeclared"`
	InputTokens      int64 `json:"input_tokens"`
	OutputTokens     int64 `json:"output_tokens"`
	CacheReadTokens  int64 `json:"cache_read_tokens"`
	CacheWriteTokens int64 `json:"cache_write_tokens"`
}

func (c *Counts) add(o Counts) {
	c.Requests += o.Requests
	c.Reported += o.Reported
	c.Undeclared += o.Undeclared
	c.InputTokens += o.InputTokens
	c.OutputTokens += o.OutputTokens
	c.CacheReadTokens += o.CacheReadTokens
	c.CacheWriteTokens += o.CacheWriteTokens
}

// Key 是分组维度。Agent 是显示名，只有 Agent 部分填。
type Key struct {
	Part    string `json:"part"`
	Purpose string `json:"purpose"`
	Agent   string `json:"agent,omitempty"`
	Model   string `json:"model,omitempty"`
}

// Record 是一条记录。
type Record struct {
	Key
	Counts
	Run string
}

// Add 写一条记录；同一运行已记过时什么都不做。
func Add(tx *store.Tx, at time.Time, r Record) error {
	_, err := tx.Exec(`INSERT OR IGNORE INTO usage_record (at, part, purpose, agent, model, run_id, requests, reported,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, undeclared) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		at.UnixNano(), r.Part, r.Purpose, r.Agent, r.Model, r.Run, r.Requests, r.Reported,
		r.InputTokens, r.OutputTokens, r.CacheReadTokens, r.CacheWriteTokens, r.Undeclared)
	return err
}

// maxUsageFile 是用量文件读取上限。
const maxUsageFile = 64 << 10

// readUsageFile 读运行目录里声明的用量文件；没有或不是普通文件时返回空。运行目录 Agent 可写，所以不跟随链接、限长。
func readUsageFile(path string) []byte {
	if fi, err := os.Lstat(path); err != nil || !fi.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxUsageFile))
	if err != nil {
		return nil
	}
	return b
}

// AddRun 在一次 Agent 运行结束后记账：任务、后台运行与来源批处理，各记一次。任何注册的执行者都经它生效声明的
// headless.usage 提供用量（从运行目录的文件或 stdout 取，内核不认识具体 harness）：没声明的记为未提供，声明了没取到的记为未回报，
// 都不计 0、不估算。进程不在本机的运行不记。
func AddRun(ctx context.Context, st *store.Store, runsDir string, res proc.Result, now time.Time) error {
	run := res.Run
	switch run.Category {
	case domain.RunTask, domain.RunBackground, domain.RunSourceBatch:
	default:
		return nil
	}
	if run.Launch == policy.LaunchWake {
		return nil
	}
	if run.FinishedAt != nil {
		now = *run.FinishedAt
	}
	return st.Update(ctx, func(tx *store.Tx) error {
		r := Record{Key: Key{Part: PartAgent, Purpose: string(run.Category)}, Counts: Counts{Requests: 1, Undeclared: 1}, Run: run.ID}
		if a := run.AgentSnapshot; a != nil {
			r.Agent, r.Model = a.AgentName, a.ConfiguredModel
		}
		recs, err := executors.Records(tx, executors.StateActive)
		if err != nil {
			return err
		}
		for _, rec := range recs {
			if rec.Harness != run.Harness {
				continue
			}
			// 旧设置迁移来的卡名字是 harness 标识，换成声明里的显示名。
			if n := rec.Declaration.Name; n != nil && (r.Agent == "" || r.Agent == run.Harness) {
				r.Agent = n.Zh
			}
			h := rec.Declaration.Headless
			if h == nil || h.Usage == nil {
				continue
			}
			r.Undeclared = 0
			data := res.Stdout
			if h.Usage.File != "" {
				data = readUsageFile(filepath.Join(runsDir, run.ID, h.Usage.File))
			}
			if u, ok := h.Usage.Parse(data); ok {
				r.Reported, r.InputTokens, r.OutputTokens = 1, u.Input, u.Output
				r.CacheReadTokens, r.CacheWriteTokens = u.CacheRead, u.CacheWrite
				r.Model = cmp.Or(u.Model, r.Model)
			}
		}
		return Add(tx, now, r)
	})
}

// Row 是一个分组在整个时间段里的合计。
type Row struct {
	Key
	Counts
}

// Point 是一个时间槽里一个部分的合计。
type Point struct {
	Slot string `json:"slot"`
	Part string `json:"part"`
	Counts
}

// Summary 是时间段内的汇总。Since 是最早一条记录的时刻（还没有记录时为空），早于它的时间没有统计。
// Slots 是时间段内的全部时间槽，旧的在前、没有记录的也在；Series 只列有记录的。
type Summary struct {
	Since  *time.Time `json:"since,omitempty"`
	Rows   []Row      `json:"rows"`
	Slots  []string   `json:"slots"`
	Series []Point    `json:"series"`
}

// hourSlot 是今天视图的槽名：本地钟点 "00"–"23"。夏令时重复的钟点归一槽。
const hourSlot = "15"

// Summarize 汇总含 now 当天在内的最近 days 天；日界与钟点按 now 的时区。days 为 1 时按小时分槽（24 个），否则按天。
func Summarize(tx *store.Tx, now time.Time, days int) (Summary, error) {
	s := Summary{Rows: []Row{}, Slots: []string{}, Series: []Point{}}
	y, m, d := now.Date()
	from := time.Date(y, m, d-days+1, 0, 0, 0, 0, now.Location())
	layout := time.DateOnly
	if days == 1 {
		layout = hourSlot
		for h := range 24 {
			s.Slots = append(s.Slots, fmt.Sprintf("%02d", h))
		}
	} else {
		for i := range days { // 取正午，避开夏令时在零点跳变的时区
			s.Slots = append(s.Slots, time.Date(y, m, d-days+1+i, 12, 0, 0, 0, now.Location()).Format(layout))
		}
	}
	first, err := tx.Query(`SELECT COALESCE(MIN(at), 0) FROM usage_record`)
	if err != nil {
		return s, err
	}
	var at int64
	if first.Next() {
		err = first.Scan(&at)
	}
	first.Close()
	if err != nil {
		return s, err
	}
	if at != 0 {
		t := time.Unix(0, at).UTC()
		s.Since = &t
	}
	rows, err := tx.Query(`SELECT at, part, purpose, agent, model, requests, reported, undeclared, input_tokens, output_tokens, cache_read_tokens, cache_write_tokens
		FROM usage_record WHERE at >= ?`, from.UnixNano())
	if err != nil {
		return s, err
	}
	defer rows.Close()
	// shortcut: 逐条读进内存分组；30 天的记录到几十万条、页面变慢时改为按天预聚合。
	groups, series := map[Key]Counts{}, map[[2]string]Counts{}
	for rows.Next() {
		var k Key
		var c Counts
		if err := rows.Scan(&at, &k.Part, &k.Purpose, &k.Agent, &k.Model,
			&c.Requests, &c.Reported, &c.Undeclared, &c.InputTokens, &c.OutputTokens, &c.CacheReadTokens, &c.CacheWriteTokens); err != nil {
			return s, err
		}
		g := groups[k]
		g.add(c)
		groups[k] = g
		sk := [2]string{time.Unix(0, at).In(now.Location()).Format(layout), k.Part}
		p := series[sk]
		p.add(c)
		series[sk] = p
	}
	if err := rows.Err(); err != nil {
		return s, err
	}
	for k, c := range groups {
		s.Rows = append(s.Rows, Row{k, c})
	}
	slices.SortFunc(s.Rows, func(a, b Row) int {
		return cmp.Or(cmp.Compare(a.Part, b.Part), cmp.Compare(b.InputTokens+b.OutputTokens, a.InputTokens+a.OutputTokens),
			cmp.Compare(a.Purpose, b.Purpose), cmp.Compare(a.Agent, b.Agent), cmp.Compare(a.Model, b.Model))
	})
	for k, c := range series {
		s.Series = append(s.Series, Point{k[0], k[1], c})
	}
	slices.SortFunc(s.Series, func(a, b Point) int { return cmp.Or(cmp.Compare(a.Slot, b.Slot), cmp.Compare(a.Part, b.Part)) })
	return s, nil
}
