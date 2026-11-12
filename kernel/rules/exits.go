package rules

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
)

// 来源规则的出口（附录 C.5、设计 §9.3）。
const (
	ExitProposed = "proposed"
	ExitIgnore   = "ignore"
	ExitScreen   = "screen"
	ExitExtract  = "extract"
	ExitDigest   = "digest"
	ExitAlert    = "alert"
)

// Exit 返回规则的出口：省略取 proposed，旧名 judge 等同 screen。包校验已保证取值在枚举内。
func Exit(r packs.SourceRule) string {
	switch r.Then {
	case "":
		return ExitProposed
	case "judge":
		return ExitScreen
	}
	return r.Then
}

// Series 取同一序列（来源，事件的第一个关联键，指标名）上一次的取值（§9.2）。序列第一次出现时 ok 为假。
// changed、crosses_above、crosses_below 靠它判断；没有注入时这三种条件恒不成立（没有上一次取值就不成立）。
type Series interface {
	Previous(ev domain.SourceEvent, metric string) (v float64, ok bool, err error)
}

// Where 判断 where[] 是否逐条都成立；空列表恒成立。
func Where(conds []packs.Condition, src domain.Source, ev domain.SourceEvent, series Series) (bool, error) {
	for _, c := range conds {
		ok, err := holds(c, src, ev, series)
		if err != nil || !ok {
			return false, err
		}
	}
	return true, nil
}

func holds(c packs.Condition, src domain.Source, ev domain.SourceEvent, series Series) (bool, error) {
	if name, isMetric := packs.MetricName(c.Field); isMetric {
		cur, present := ev.Metrics[name]
		if !present {
			return false, nil // 事件没带这项指标：不成立，不算错
		}
		if packs.IsChangeOp(c.Op) {
			return changed(c, cur, ev, name, series)
		}
		if c.Op == "eq" || c.Op == "ne" || c.Op == "in" {
			return compareScalar(c, fmt.Sprint(cur))
		}
		limit, _ := number(c.Value)
		switch c.Op {
		case "gt":
			return cur > limit, nil
		case "gte":
			return cur >= limit, nil
		case "lt":
			return cur < limit, nil
		case "lte":
			return cur <= limit, nil
		}
		return false, fmt.Errorf("unsupported op %q", c.Op)
	}
	var got string
	switch c.Field {
	case "type":
		got = ev.Type
	case "subject":
		got = ev.Subject
	case "summary":
		got = ev.Summary
	case "detail":
		got = ev.Detail
	case "link":
		got = ev.Link
	case "sender":
		got = ev.Sender
	case "sender_verified":
		got = fmt.Sprint(ev.SenderVerified)
	case "merge_key":
		got = ev.MergeKey
	case "source":
		got = ev.SourceID
	default:
		return false, fmt.Errorf("field %q is not a standard event field", c.Field)
	}
	return compareScalar(c, got)
}

// compareScalar 做 eq、ne、in：按文本比较，数值写成 10 或 10.0 都与指标 10 相等。
func compareScalar(c packs.Condition, got string) (bool, error) {
	same := func(want any) bool {
		if a, ok := number(want); ok {
			if b, ok := number(got); ok {
				return a == b
			}
		}
		return fmt.Sprint(want) == got
	}
	switch c.Op {
	case "eq":
		return same(c.Value), nil
	case "ne":
		return !same(c.Value), nil
	case "in":
		l, _ := c.Value.([]any)
		for _, w := range l {
			if same(w) {
				return true, nil
			}
		}
		return false, nil
	}
	return false, fmt.Errorf("op %q cannot be applied to field %q", c.Op, c.Field)
}

func changed(c packs.Condition, cur float64, ev domain.SourceEvent, metric string, series Series) (bool, error) {
	if series == nil {
		return false, nil
	}
	prev, ok, err := series.Previous(ev, metric)
	if err != nil || !ok {
		return false, err
	}
	limit, _ := number(c.Value)
	switch c.Op {
	case "changed":
		return cur != prev, nil
	case "crosses_above":
		return prev <= limit && cur > limit, nil
	case "crosses_below":
		return prev >= limit && cur < limit, nil
	}
	return false, fmt.Errorf("unsupported op %q", c.Op)
}

func number(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// Project 按优先级取事件归属的项目（§9.3）：规则的 project_by（fixed:<项目>，或 repo_remote 已解析出的项目），
// 其次来源登记的默认项目；都没有则不归项目。none 与 draft 是"规则不指定"，draft 的最终选择由起草那一步做。
// repo 是调用方按仓库远程地址解析出的项目（没有解析到为空）。
func Project(by, repo, sourceDefault string) string {
	switch {
	case strings.HasPrefix(by, "fixed:"):
		return strings.TrimPrefix(by, "fixed:")
	case by == "repo_remote" && repo != "":
		return repo
	}
	return sourceDefault
}
