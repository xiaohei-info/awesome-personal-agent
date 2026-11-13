// Package stats 是判断计数与四项指标（设计 §8.6、§8.10）：从判断记录按用途、模板、来源、后端计数，
// 给出升级率、未定率、误丢率、接受率。只读：不写账本，不换算费用，不设额度（APA 只计数）。
//
//   - rows：每个（用途、模板、来源、后端）的调用数 calls、定案 decided、不确定 uncertain、报错 errors、复用 reused。
//     复用的结果不计调用次数；跳过的级不计。
//   - 升级率 escalation：分类端点答过的题（定案 + 不确定）里落入不确定带的比例。
//   - 未定率 undecided：各级都没定案、取了保守默认的记录占比。影子记录只记分不定案，不进两项比率。
//   - 误丢率 false_drop：已抽检记录里被本人判为"该看"的比例；reviewed 是已抽检条数。
//   - 接受率 acceptance：进建议流后已由本人处置的建议里，被接受或补到已有事项的比例（丢弃为分母不为分子）。
//     数据来自已有的建议处置 Event，只计本人的决定（Web 操作与带证据的代答）；还在等待的建议不进分母。
//   - histogram：分类端点概率的分布，10 个等宽区间。
//
// 旧版 run.finished（category: judge）不是判断记录，不计入（附录 B.5）。
package stats

import (
	"encoding/json"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// DefaultWindow 是 since 缺省的窗口。
const DefaultWindow = 30 * 24 * time.Hour

// ParseSince 解析 since：空取最近 30 天；`Nd`（天数）、Go 时长（如 36h）或 RFC3339 时刻。
func ParseSince(v string, now time.Time) (time.Time, error) {
	bad := domain.NewError(domain.CodeInvalidRequest, "since must be a window like 7d or 36h, or an RFC3339 time", "send e.g. since=30d")
	if v == "" {
		return now.Add(-DefaultWindow), nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, nil
	}
	d, err := time.ParseDuration(v)
	if n, ok := strings.CutSuffix(v, "d"); ok {
		days, e := strconv.Atoi(n)
		d, err = time.Duration(days)*24*time.Hour, e
	}
	if err != nil || d <= 0 {
		return time.Time{}, bad
	}
	return now.Add(-d), nil
}

// Filter 筛选记录；空字段不筛。Since 非零时只统计 created_at 不早于它的记录。
type Filter struct {
	Use, Template, Source string
	Since                 time.Time
}

// Row 是一个分组（用途、模板、来源、后端）的计数。
type Row struct {
	Use       string `json:"use"`
	Template  string `json:"template"`
	Source    string `json:"source,omitempty"`
	Backend   string `json:"backend"`
	Calls     int    `json:"calls"`
	Decided   int    `json:"decided"`
	Uncertain int    `json:"uncertain"`
	Errors    int    `json:"errors"`
	Reused    int    `json:"reused"`
}

// Rate 是一个分组（用途、模板、来源）的四项指标。
type Rate struct {
	Use        string  `json:"use"`
	Template   string  `json:"template"`
	Source     string  `json:"source,omitempty"`
	Escalation float64 `json:"escalation"`
	Undecided  float64 `json:"undecided"`
	Acceptance float64 `json:"acceptance"`
	FalseDrop  float64 `json:"false_drop"`
	Reviewed   int     `json:"reviewed"`
}

// Bucket 是分类端点概率分布的一个区间 [Low, High)，最后一个区间含 1。
type Bucket struct {
	Use      string  `json:"use"`
	Template string  `json:"template"`
	Source   string  `json:"source,omitempty"`
	Low      float64 `json:"low"`
	High     float64 `json:"high"`
	Count    int     `json:"count"`
}

// Result 是 GET /judgments/stats 的内容。
type Result struct {
	Rows      []Row    `json:"rows"`
	Rates     []Rate   `json:"rates"`
	Histogram []Bucket `json:"histogram"`
}

type group struct{ use, template, source string }

type tally struct {
	records, undecided, classAnswered, classUncertain int
	reviewed, shouldSee                               int
	accepted, resolved                                int
	hist                                              [10]int
}

// Compute 统计 f 范围内的判断记录。
func Compute(tx *store.Tx, f Filter) (Result, error) {
	res := Result{Rows: []Row{}, Rates: []Rate{}, Histogram: []Bucket{}}
	rows := map[[4]string]*Row{}
	tallies := map[group]*tally{}
	counted := map[string]bool{} // 同一建议被多条记录指向时只计一次接受
	rf := record.Filter{Use: f.Use, Template: f.Template, Source: f.Source, Since: f.Since, Limit: record.MaxLimit}
	for {
		page, more, err := record.List(tx, rf)
		if err != nil {
			return res, err
		}
		for _, r := range page {
			g := group{r.Use, r.Template, r.Source}
			t := tallies[g]
			if t == nil {
				t = &tally{}
				tallies[g] = t
			}
			for _, l := range r.Levels {
				if l.Reused || l.Outcome == record.OutcomeSkipped {
					continue
				}
				k := [4]string{r.Use, r.Template, r.Source, l.Backend}
				row := rows[k]
				if row == nil {
					row = &Row{Use: r.Use, Template: r.Template, Source: r.Source, Backend: l.Backend}
					rows[k] = row
				}
				row.Calls++
				switch l.Outcome {
				case record.OutcomeDecided:
					row.Decided++
				case record.OutcomeUncertain:
					row.Uncertain++
				case record.OutcomeError:
					row.Errors++
				}
			}
			for _, l := range r.Levels {
				if l.Reused && l.Outcome != record.OutcomeSkipped {
					k := [4]string{r.Use, r.Template, r.Source, l.Backend}
					if rows[k] == nil {
						rows[k] = &Row{Use: r.Use, Template: r.Template, Source: r.Source, Backend: l.Backend}
					}
					rows[k].Reused++
				}
				if l.Backend == record.BackendClassifier && l.PYes != nil && !l.Reused {
					i := int(*l.PYes * 10)
					if i > 9 {
						i = 9
					}
					t.hist[i]++
				}
			}
			if r.Review != nil {
				t.reviewed++
				if r.Review.Result == "should_see" {
					t.shouldSee++
				}
			}
			if r.Shadow {
				continue
			}
			t.records++
			if r.DecidedBy == record.DecidedDefault {
				t.undecided++
			}
			for _, l := range r.Levels {
				if l.Backend != record.BackendClassifier || l.Reused {
					continue
				}
				switch l.Outcome {
				case record.OutcomeDecided:
					t.classAnswered++
				case record.OutcomeUncertain:
					t.classAnswered++
					t.classUncertain++
				}
			}
			if r.Review != nil && r.Review.Result == "should_see" {
				continue // 误丢的事件由本人转成建议，那次转换不是"进建议流后被接受"
			}
			item, outcome, err := proposalOutcome(tx, r.Subject)
			if err != nil {
				return res, err
			}
			ck := key(g.use, g.template, g.source, item)
			if outcome == "" || counted[ck] {
				continue
			}
			counted[ck] = true
			t.resolved++
			if outcome != outcomeDismissed {
				t.accepted++
			}
		}
		if !more {
			break
		}
		rf.After = page[len(page)-1].ID
	}
	for _, r := range rows {
		res.Rows = append(res.Rows, *r)
	}
	sort.Slice(res.Rows, func(i, j int) bool {
		a, b := res.Rows[i], res.Rows[j]
		return key(a.Use, a.Template, a.Source, a.Backend) < key(b.Use, b.Template, b.Source, b.Backend)
	})
	for g, t := range tallies {
		res.Rates = append(res.Rates, Rate{Use: g.use, Template: g.template, Source: g.source, Reviewed: t.reviewed,
			Escalation: ratio(t.classUncertain, t.classAnswered), Undecided: ratio(t.undecided, t.records),
			Acceptance: ratio(t.accepted, t.resolved), FalseDrop: ratio(t.shouldSee, t.reviewed)})
		for i, n := range t.hist {
			if n > 0 {
				res.Histogram = append(res.Histogram, Bucket{Use: g.use, Template: g.template, Source: g.source,
					Low: float64(i) / 10, High: float64(i+1) / 10, Count: n})
			}
		}
	}
	sort.Slice(res.Rates, func(i, j int) bool {
		a, b := res.Rates[i], res.Rates[j]
		return key(a.Use, a.Template, a.Source, "") < key(b.Use, b.Template, b.Source, "")
	})
	sort.Slice(res.Histogram, func(i, j int) bool {
		a, b := res.Histogram[i], res.Histogram[j]
		if ka, kb := key(a.Use, a.Template, a.Source, ""), key(b.Use, b.Template, b.Source, ""); ka != kb {
			return ka < kb
		}
		return a.Low < b.Low
	})
	return res, nil
}

func key(parts ...string) string {
	s := ""
	for _, p := range parts {
		s += p + "\x00"
	}
	return s
}

func ratio(n, d int) float64 {
	if d == 0 {
		return 0
	}
	return float64(n) / float64(d)
}

const (
	outcomeAccepted  = "accepted"
	outcomeAttached  = "attached"
	outcomeDismissed = "dismissed"
)

// proposalOutcome 找判断对象（来源事件）进建议流后被本人做的处置：没有建议、建议还在等待或不是本人处置时返回空。
// 来源事件不存在（已清理）按没有建议处理。
func proposalOutcome(tx *store.Tx, subject string) (item, outcome string, err error) {
	ev, err := runner.Event(tx, subject)
	if err != nil {
		if e, ok := err.(*domain.Error); ok && e.Code == domain.CodeNotFound {
			return "", "", nil
		}
		return "", "", err
	}
	if ev.Item == "" {
		return "", "", nil
	}
	es, err := eventlog.ReadObject(tx, ev.Item)
	if err != nil {
		return "", "", err
	}
	for _, en := range es {
		if en.Tombstone || (en.Event.Type != domain.EvItemPhaseChanged && en.Event.Type != domain.EvProposalAttached) {
			continue
		}
		if via := en.Event.ResolvedVia; via != decisions.ViaWeb && via != decisions.ViaChat {
			continue
		}
		if en.Event.Type == domain.EvProposalAttached {
			// 补到已有事项后来源事件改挂到目标事项，建议本身看不到了；目标上的 proposal.attached 载荷列着被并入的建议与消息。
			var a struct {
				Proposal string   `json:"proposal"`
				Messages []string `json:"messages"`
			}
			if json.Unmarshal(en.Event.Payload, &a) == nil && slices.Contains(a.Messages, ev.ID) {
				return a.Proposal, outcomeAttached, nil
			}
			continue
		}
		var p struct {
			Op     string `json:"op"`
			Before *struct {
				Phase domain.Phase `json:"phase"`
			} `json:"before"`
			Item struct {
				Phase domain.Phase `json:"phase"`
			} `json:"item"`
		}
		if json.Unmarshal(en.Event.Payload, &p) != nil || p.Before == nil || p.Before.Phase != domain.PhaseProposed {
			continue
		}
		switch {
		case p.Op == "proposal.attach":
			return ev.Item, outcomeAttached, nil
		case p.Item.Phase == domain.PhaseOpen:
			return ev.Item, outcomeAccepted, nil
		case p.Item.Phase == domain.PhaseDropped:
			return ev.Item, outcomeDismissed, nil
		}
	}
	return ev.Item, "", nil
}
