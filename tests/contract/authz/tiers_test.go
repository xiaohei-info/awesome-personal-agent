package authz_test

import (
	"os"
	"regexp"
	"strings"
	"testing"

	apisources "github.com/xiaohei-info/awesome-personal-agent/api/sources"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/channel"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/spotcheck"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/retention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
)

// 附录 B.1「三档与硬线」表逐条对照：文档里每条命令所在的档，就是它登记的 Spec.Tier（按状态升档的命令登记的是基础档）。
// 判定与文档同源（主文档 §14.3）：命令换档必须同时改文档，否则这里失败。
var tierRows = map[string]authz.Tier{
	"读": authz.TierRead, "一档 整理": authz.TierOrganize, "二档 拍板": authz.TierDecide,
	"3a 改日常": authz.TierRules, "3b 造新权力": authz.TierPower, "系统内部": authz.TierInternal,
}

func docTiers(t *testing.T) map[string]authz.Tier {
	t.Helper()
	b, err := os.ReadFile("../../../docs/appendix-b-api-cli.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(string(b), "**三档与硬线**")
	sec, _, _ := strings.Cut(rest, "## B.2")
	name := regexp.MustCompile("`([a-z_]+(?:\\.[a-z_]+)+)`")
	out := map[string]authz.Tier{}
	for _, line := range strings.Split(sec, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		tier, ok := tierRows[strings.TrimSpace(cells[1])]
		if !ok {
			continue
		}
		for _, m := range name.FindAllStringSubmatch(cells[2], -1) {
			if prev, dup := out[m[1]]; dup && prev != tier {
				t.Errorf("%s is listed in two tiers (%d and %d)", m[1], prev, tier)
			}
			out[m[1]] = tier
		}
	}
	if len(out) < 90 {
		t.Fatalf("parsed only %d commands from the B.1 tier table; the table format changed", len(out))
	}
	return out
}

func TestRegisteredTiersMatchAppendixB1(t *testing.T) {
	e := setup(t)
	x := e.x // 已登记事项、项目、材料、附件、物理删除、执行者、委派与凭据
	reg := map[string]error{
		"decisions": decisions.Register(x),
		"hosted":    hosted.Register(x),
		"instances": instances.Register(x, nil),
		"registry":  registry.Register(x, registry.Options{Declared: func(s domain.Source) string { return s.ID }}),
		"attach":    inbound.RegisterAttach(x),
		"rules":     rules.Register(x),
		"spotcheck": spotcheck.Register(x),
		"channel":   channel.Register(x, channel.Options{}),
		"retention": retention.Register(x, t.TempDir(), retention.Defaults(0, 0, 0)),
	}
	for name, err := range reg {
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if _, err := apisources.Mount(e.st, x, auth.New(nil), nil, nil, apisources.Declared{}); err != nil {
		t.Fatal(err)
	}
	// 文档 → 实现：本装配里登记了的命令，档位必须与文档一致。其余命令要整套服务才能登记（入站、挂靠消息、连接、
	// AGENTS.md、跟进、来源执行），由各自模块的测试覆盖；这里要求本装配至少覆盖文档的大半。
	checked := 0
	for name, want := range docTiers(t) {
		got, ok := x.Tier(name)
		if !ok {
			continue
		}
		checked++
		if got != want {
			t.Errorf("%s: registered tier %d, appendix B.1 says %d", name, got, want)
		}
	}
	if checked < 70 {
		t.Errorf("only %d documented commands are registered in this assembly; register more modules", checked)
	}
	// 已取消的命令不再登记（附录 B.6 末行）。
	for _, name := range []string{"channel.form_answer", "grant.issue", "grant.list", "grant.replace", "grant.revoke", "proposal.batch"} {
		if _, ok := x.Tier(name); ok {
			t.Errorf("%s was cancelled but is still registered", name)
		}
	}
}
