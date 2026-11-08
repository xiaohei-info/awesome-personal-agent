package authz_test

import (
	"testing"

	apisources "github.com/xiaohei-info/awesome-personal-agent/api/sources"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/hosted"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
)

// 附录 B.1「代理写命令清单」逐条对照：每条命令登记的档位。
// 第一档：agent 自己的工作；第二档：替本人做决定（可用授权单）；words：只接受本人原话的管理命令。
// followmsg.handback、inbound.capture 在各自模块的测试里对照（它们需要整套服务才能登记）。
func TestRegisteredTiersMatchAppendixB1(t *testing.T) {
	e := setup(t)
	x := e.x // 已登记事项、项目、材料、附件、执行者、委派
	for name, err := range map[string]error{
		"decisions": decisions.Register(x),
		"hosted":    hosted.Register(x),
		"instances": instances.Register(x, nil),
		"registry":  registry.Register(x, registry.Options{}),
	} {
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
	}
	if _, err := apisources.Mount(e.st, x, auth.New(nil), nil, nil); err != nil {
		t.Fatal(err)
	}
	const (
		work   = authz.TierWork
		decide = authz.TierDecide
		words  = authz.TierWords
	)
	want := map[string]authz.Tier{
		// 事项：新建与改动默认第二档，对象绑定（proposed 新建、绑定的事项）时降到第一档（Bound）
		"item.create": decide, "item.update": decide, "item.transition": decide, "item.relate": decide,
		// 决策
		"decision.request": work, "decision.answer": decide, "proposal.accept": decide, "proposal.dismiss": decide,
		// 材料
		"material.commit": work, "material.add": work, "blob.put": work, "revision.confirm": decide, "revision.reject": decide,
		// 委派
		"delegate.create": work, "delegate.report": decide, "executor.declare": work,
		// 项目
		"project.checkout.set": work, "project.checkout.remove": work,
		"project.create": words, "project.update": words, "project.repo.add": words, "project.repo.remove": words,
		// 来源与能力包
		"source.register": words, "source.update": words, "source.set_enabled": words, "source.webhook_key": words,
		"source.webhook_revoke": words, "source.replay": words, "instance.create": words, "instance.update": words, "instance.toggle": words,
	}
	for name, tier := range want {
		got, ok := x.Tier(name)
		if !ok {
			t.Errorf("%s is not registered", name)
		} else if got != tier {
			t.Errorf("%s: tier %d, appendix B.1 says %d", name, got, tier)
		}
	}
	// 设置类（第三档）只在 Web：登记可执行命令不是代理写。
	for _, name := range []string{registry.CmdRegisterExec, registry.CmdUpdateExec} {
		if got, _ := x.Tier(name); got != authz.TierNone {
			t.Errorf("%s must not be a proxy write, tier %d", name, got)
		}
	}
}
