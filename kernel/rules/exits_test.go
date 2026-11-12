package rules

import (
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
)

type prev map[string]float64

func (p prev) Previous(_ domain.SourceEvent, m string) (float64, bool, error) {
	v, ok := p[m]
	return v, ok, nil
}

func TestExitNames(t *testing.T) {
	for then, want := range map[string]string{"": ExitProposed, "judge": ExitScreen, "screen": ExitScreen, "digest": ExitDigest, "alert": ExitAlert} {
		if got := Exit(packs.SourceRule{Then: then}); got != want {
			t.Errorf("then %q → %q, want %q", then, got, want)
		}
	}
}

func TestWhereOperators(t *testing.T) {
	ev := domain.SourceEvent{Sender: "alice", Metrics: map[string]float64{"size": 10, "level": 2}}
	c := func(f, op string, v any) []packs.Condition { return []packs.Condition{{Field: f, Op: op, Value: v}} }
	cases := []struct {
		name   string
		conds  []packs.Condition
		series Series
		want   bool
	}{
		{"empty", nil, nil, true},
		{"eq", c("sender", "eq", "alice"), nil, true},
		{"ne", c("sender", "ne", "alice"), nil, false},
		{"in", c("sender", "in", []any{"bob", "alice"}), nil, true},
		{"gt boundary", c("metrics.size", "gt", 10), nil, false},
		{"gte boundary", c("metrics.size", "gte", 10), nil, true},
		{"lt boundary", c("metrics.size", "lt", 10), nil, false},
		{"lte boundary", c("metrics.size", "lte", 10), nil, true},
		{"eq numeric", c("metrics.size", "eq", 10.0), nil, true},
		{"missing metric", c("metrics.none", "gte", 0), nil, false},
		{"all must hold", append(c("metrics.size", "gte", 10), c("metrics.size", "lt", 10)...), nil, false},
		{"changed without series", c("metrics.level", "changed", nil), nil, false},
		{"first appearance", c("metrics.level", "changed", nil), prev{}, false},
		{"unchanged", c("metrics.level", "changed", nil), prev{"level": 2}, false},
		{"changed", c("metrics.level", "changed", nil), prev{"level": 1}, true},
		{"crosses_above", c("metrics.size", "crosses_above", 5), prev{"size": 3}, true},
		{"already above", c("metrics.size", "crosses_above", 5), prev{"size": 7}, false},
		{"crosses_below", c("metrics.size", "crosses_below", 15), prev{"size": 20}, true},
		{"not below", c("metrics.size", "crosses_below", 5), prev{"size": 20}, false},
	}
	for _, tc := range cases {
		got, err := Where(tc.conds, domain.Source{}, ev, tc.series)
		if err != nil || got != tc.want {
			t.Errorf("%s: got %v err %v, want %v", tc.name, got, err, tc.want)
		}
	}
	if _, err := Where(c("colour", "eq", "red"), domain.Source{}, ev, nil); err == nil {
		t.Error("unknown field accepted")
	}
}

func TestProjectPriority(t *testing.T) {
	cases := []struct{ by, repo, def, want string }{
		{"fixed:a", "", "d", "a"},
		{"repo_remote", "r", "d", "r"},
		{"repo_remote", "", "d", "d"},
		{"none", "", "d", "d"},
		{"draft", "", "d", "d"},
		{"", "", "", ""},
	}
	for _, c := range cases {
		if got := Project(c.by, c.repo, c.def); got != c.want {
			t.Errorf("%+v → %q", c, got)
		}
	}
}
