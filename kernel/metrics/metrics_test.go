package metrics

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
)

var over90 = packs.SourceRule{ID: "over", Then: "alert", Where: []packs.Condition{
	{Field: "metrics.value", Op: "crosses_above", Value: 90},
	{Field: "sender", Op: "eq", Value: "mon"},
	{Field: "metrics.absent", Op: "gt", Value: 1}}}

// 序列上一次取值取自事件入账时记下的 PrevMetrics：ACT-IRL-09/12 的判定在这里端到端走一遍（规则引擎 + Series）。
func TestSeriesDrivesChangeConditions(t *testing.T) {
	cross := []packs.Condition{{Field: "metrics.value", Op: "crosses_above", Value: 90}}
	cases := []struct {
		name string
		prev map[string]float64
		cur  float64
		want bool
	}{
		{"first appearance of the series", nil, 95, false},
		{"below the threshold", map[string]float64{"value": 70}, 80, false},
		{"crossing", map[string]float64{"value": 80}, 92, true},
		{"staying above", map[string]float64{"value": 96}, 97, false},
		{"another metric's previous value does not count", map[string]float64{"free": 1}, 99, false},
	}
	for _, c := range cases {
		ev := domain.SourceEvent{Metrics: map[string]float64{"value": c.cur}, PrevMetrics: c.prev}
		got, err := rules.Where(cross, domain.Source{}, ev, Series{})
		if err != nil || got != c.want {
			t.Errorf("%s: got %v err %v, want %v", c.name, got, err, c.want)
		}
	}
}

// 回放记录：只列作用于 metrics 且事件带了该指标的条件，写明条件文本、上一次与本次取值；第一次出现没有上一次取值。
func TestExplainListsMetricConditionsWithBothValues(t *testing.T) {
	ev := domain.SourceEvent{Metrics: map[string]float64{"value": 96}, PrevMetrics: map[string]float64{"value": 70}}
	got := Explain(over90, ev)
	if len(got) != 1 || got[0].Condition != "metrics.value crosses_above 90" || got[0].Previous == nil || *got[0].Previous != 70 || got[0].Current != 96 {
		t.Fatalf("explain %+v", got)
	}
	first := Explain(over90, domain.SourceEvent{Metrics: map[string]float64{"value": 96}})
	if len(first) != 1 || first[0].Previous != nil {
		t.Fatalf("first appearance %+v", first)
	}
}
