package inbound

import (
	"slices"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func gprop(id, title, source, key string, minute int) domain.Item {
	it := domain.Item{ID: id, Title: title, Kind: domain.KindTask, Phase: domain.PhaseProposed, Origin: &domain.Origin{Source: source}, Proposal: &domain.Proposal{MergeKey: key}}
	it.CreatedAt = t0.Add(time.Duration(minute) * time.Minute)
	return it
}

// 分组不重不漏：每条建议恰好在一组；合并键相同的必在同一组；标题类别受来源类别约束；需要拍板的置顶；组内按建立先后，组头的最早与最新取组内首尾。
func TestGroupsPartition(t *testing.T) {
	urgent := true
	mail, chat, pr := Origin{Type: "mail.received"}, Origin{Type: "chat.mention"}, Origin{Type: "pr.opened", Object: "/o/r/pull/9"}
	props := []domain.Item{
		gprop("ci2", "[o/app] Run failed: ci - main (abc123)", "src_m", "", 5),
		gprop("ci1", "修复 main 分支 CI build 失败", "src_m", "", 1),
		gprop("ci3", "awesome PR #460 与 ci run 失败", "src_h", "", 3),
		gprop("rv1", "审阅 PR #217：豁免公告", "src_m", "", 2),
		gprop("rv2", "fix: 验收反馈", "src_h", "", 4),
		gprop("sec", "【交易所】新设备或新 IP 地址登录提醒", "src_m", "", 6),
		gprop("fin1", "提币成功", "src_m", "title:提币成功", 7),
		gprop("fin2", "提币成功", "src_m", "title:提币成功", 9),
		gprop("al", "告警处理：磁盘剩余空间不足（P1故障）", "src_c", "", 8),
		gprop("sub", "Your early access to High Yield Earn", "src_m", "", 10),
		gprop("chatsub", "储蓄卡撞库营销今日上线确认", "src_c", "", 11),          // 聊天里的"营销"是工作，不归订阅通知
		gprop("prtitle", "fix(health): 模型告警按端点恢复", "src_h", "", 12), // PR 自己的标题里的"告警"不是告警
		gprop("remind", "股票到账", "src_r", "", 13),                    // 提醒事项不按标题归类
		gprop("obj1", "同一链接甲", "src_m", "link:https://x.test/a", 14),
		gprop("obj2", "同一链接乙", "src_h", "link:https://x.test/a", 15),
		gprop("hosted", "feat(web): 新页面", "src_h", "", 16),
		gprop("hist", "调研：某框架", "agent:vault-migrate", "", 17),
		gprop("pin", "回复对方：今天能否验证", "src_c", "", 18),
		gprop("pinmk", "PR 30 评审请求", "src_m", "link:https://x.test/pr/30", 19),
		gprop("pinmk2", "PR 30 再次提醒评审", "src_m", "link:https://x.test/pr/30", 20),
	}
	props[17].Proposal.Urgent = &urgent
	props[18].Priority = domain.PriorityHigh
	origins := map[string]Origin{"ci1": mail, "ci2": mail, "ci3": pr, "rv1": mail, "rv2": {Type: "notification.review_requested", Object: "/o/r/pull/8"},
		"sec": mail, "fin1": mail, "fin2": mail, "al": chat, "sub": mail, "chatsub": chat, "prtitle": pr, "remind": {Type: "reminder.due"},
		"obj1": mail, "obj2": pr, "hosted": pr, "pin": chat, "pinmk": mail, "pinmk2": mail}

	gs := Groups(props, origins)
	where := map[string]string{}
	for _, g := range gs {
		for _, id := range g.Items {
			if prev, dup := where[id]; dup {
				t.Fatalf("%s is in %s and %s", id, prev, g.Key)
			}
			where[id] = g.Key
		}
		if g.First.After(g.Last) || g.First != propOf(props, g.Items[0]).CreatedAt || g.Last != propOf(props, g.Items[len(g.Items)-1]).CreatedAt {
			t.Errorf("%s first/last: %v %v", g.Key, g.First, g.Last)
		}
		if !slices.IsSortedFunc(g.Items, func(a, b string) int { return propOf(props, a).CreatedAt.Compare(propOf(props, b).CreatedAt) }) {
			t.Errorf("%s not in time order: %v", g.Key, g.Items)
		}
	}
	if len(where) != len(props) {
		t.Fatalf("every proposal in exactly one group: %d of %d", len(where), len(props))
	}
	want := map[string]string{
		"ci1": "topic:ci_failure", "ci2": "topic:ci_failure", "ci3": "topic:ci_failure",
		"rv1": "topic:review_request", "rv2": "topic:review_request",
		"sec": "topic:account_security", "fin1": "topic:finance", "fin2": "topic:finance", "al": "topic:alert", "sub": "topic:subscription",
		"chatsub": "rest", "prtitle": "hosted", "remind": "rest", "obj1": "object:obj1", "obj2": "object:obj1",
		"hosted": "hosted", "hist": "history", "pin": "pinned", "pinmk": "pinned", "pinmk2": "pinned",
	}
	for id, g := range want {
		if where[id] != g {
			t.Errorf("%s: in %s, want %s", id, where[id], g)
		}
	}
	keys := make([]string, len(gs))
	for i, g := range gs {
		keys[i] = g.Key
	}
	wantOrder := []string{"pinned", "topic:ci_failure", "topic:review_request", "topic:alert", "topic:finance", "topic:account_security", "topic:subscription", "object:obj1", "hosted", "history", "rest"}
	if !slices.Equal(keys, wantOrder) {
		t.Errorf("group order: %v", keys)
	}
	for _, g := range gs {
		switch g.Key {
		case "topic:finance":
			if len(g.Bundles) != 1 || !slices.Equal(g.Bundles[0], []string{"fin1", "fin2"}) {
				t.Errorf("same merge key in a topic is one bundle: %+v", g.Bundles)
			}
		case "object:obj1":
			if len(g.Bundles) != 1 || len(g.Bundles[0]) != 2 {
				t.Errorf("object group bundle: %+v", g.Bundles)
			}
		case "pinned":
			if g.Bundles != nil {
				t.Errorf("pinned has no bulk plan: %+v", g.Bundles)
			}
		case "topic:ci_failure":
			if g.Bundles != nil {
				t.Errorf("different objects make no bundle: %+v", g.Bundles)
			}
		}
	}
}

func propOf(ps []domain.Item, id string) domain.Item {
	for _, p := range ps {
		if p.ID == id {
			return p
		}
	}
	return domain.Item{}
}
