package policy

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"
)

func openGroups(c Card) []string {
	return slices.Sorted(maps.Keys(c.Groups))
}

// §16.5 映射表：三种旧档位与"没设过"各换算一次；第 6–10 组一律不开，开放不带期限，工具档位按旧口径搬入。
func TestCardsFromLegacyFollowsTheMappingTable(t *testing.T) {
	tier := json.RawMessage(`{"by_agent":{"a_org":{"tier":"organize"},"a_dec":{"tier":"decide","projects":["p1"]},"legacy:hermes":{"tier":"rules"}}}`)
	caps := json.RawMessage(`{"by_agent":{"a_org":"edit"},"default":"edit","by_harness":{"hermes":"full"}}`)
	got, err := CardsFromLegacy(tier, caps, []string{"a_unset", "legacy:other"})
	if err != nil {
		t.Fatal(err)
	}
	base := []string{GroupOrganize, GroupSourceEvents, GroupView}
	want := map[string]struct {
		groups   []string
		projects []string
		tool     string
	}{
		"a_unset":       {base, nil, PermRead}, // 没设过档位、也没设过工具：只整理、只读
		"a_org":         {base, nil, PermEdit},
		"a_dec":         {[]string{GroupDecide, GroupOrganize, GroupSourceEvents, GroupView}, []string{"p1"}, PermRead},
		"legacy:hermes": {[]string{GroupDecide, GroupOrganize, GroupRules, GroupSourceEvents, GroupView}, nil, PermFull},
		"legacy:other":  {base, nil, PermEdit}, // 兼容卡没有 by_harness 时取 default
	}
	if len(got) != len(want) {
		t.Fatalf("cards for %v", slices.Sorted(maps.Keys(got)))
	}
	for id, w := range want {
		c := got[id]
		if !slices.Equal(openGroups(c), w.groups) || !slices.Equal(c.DecideProjects, w.projects) || c.Tool != w.tool || c.RunsAllMessages {
			t.Errorf("%s: %+v", id, c)
		}
		for g, gr := range c.Groups {
			if gr.Until != nil {
				t.Errorf("%s: group %s got an expiry", id, g)
			}
		}
	}
	if _, err := CardsFromLegacy(json.RawMessage(`{"by_agent":{"x":{"tier":"power"}}}`), nil, nil); !errors.Is(err, ErrInvalid) {
		t.Fatalf("unknown tier: %v", err)
	}
}

// 有效期由判定时比较时间得出：到期即回到不允许。
func TestCardGrantExpires(t *testing.T) {
	now := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	until := now.Add(time.Hour)
	c := DefaultCard()
	c.Groups[GroupPurge] = GroupGrant{Until: &until}
	if !c.Allows(GroupPurge, now) || c.Allows(GroupPurge, until) || c.Allows(GroupPurge, until.Add(time.Minute)) {
		t.Fatal("purge must be open before until and closed from until on")
	}
	if !c.Allows(GroupView, until.Add(24*time.Hour)) || c.Allows(GroupDecide, now) {
		t.Fatal("groups without expiry stay as they are")
	}
	if p := Default(); !slices.Equal(openGroups(p.Card("agent_new")), []string{GroupOrganize, GroupSourceEvents, GroupView}) || p.Card("").Tool != PermRead {
		t.Fatalf("a new Agent gets the default card: %+v", p.Card("agent_new"))
	}
}

// 校验：未知组、物理删除不带期限、未知工具档位、上限越界都是 400，不生效。
func TestNormalizeRejectsBadCards(t *testing.T) {
	until := time.Now().Add(time.Hour)
	for name, mut := range map[string]func(*Policy){
		"unknown group":  func(p *Policy) { p.AgentCards["a"] = Card{Groups: map[string]GroupGrant{"root": {}}} },
		"purge forever":  func(p *Policy) { p.AgentCards["a"] = Card{Groups: map[string]GroupGrant{GroupPurge: {}}} },
		"bad tool":       func(p *Policy) { p.AgentCards["a"] = Card{Tool: "root"} },
		"empty agent":    func(p *Policy) { p.AgentCards[""] = DefaultCard() },
		"chain too deep": func(p *Policy) { p.Limits.ChainDepth = MaxChainDepthLimit + 1 },
	} {
		p := Default()
		mut(&p)
		if _, err := normalize(p); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := Default()
	p.AgentCards["a"] = Card{Groups: map[string]GroupGrant{GroupPurge: {Until: &until}}}
	got, err := normalize(p)
	if err != nil || got.AgentCards["a"].Tool != PermRead {
		t.Fatalf("purge with expiry is valid and tool defaults to read: %+v %v", got.AgentCards["a"], err)
	}
}
