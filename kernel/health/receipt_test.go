package health

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// receiptEnv 用真实命令管线产生回执与捕获的拒绝告警（退出码 2、4、23），再看健康视图怎么归类与清除。
type receiptEnv struct {
	*env
	x   *command.Executor
	now time.Time
}

func newReceiptEnv(t *testing.T) *receiptEnv {
	e := &receiptEnv{env: setup(t), now: t0}
	e.x = command.New(e.st, func() time.Time { return e.now })
	for _, name := range []string{"delegate.report", "inbound.capture"} {
		err := e.x.Register(command.Spec{Name: name, Action: authz.ActionUpdateOwn, Tier: authz.TierWork, Receipt: true,
			Object: func(*store.Tx, json.RawMessage) (authz.Object, error) { return authz.Object{}, nil },
			Apply: func(c *command.Ctx) (any, error) {
				var in struct{ Exit int }
				_ = json.Unmarshal(c.Input, &in)
				switch in.Exit {
				case domain.ExitUsage:
					return nil, domain.NewError(domain.CodeInvalidRequest, "bad receipt", "fix")
				case domain.ExitPermissionDenied:
					return nil, domain.NewError(domain.CodePermissionDenied, "not yours", "fix")
				case domain.ExitBasisRequired:
					return nil, domain.NewError(domain.CodeBasisRequired, "need basis", "fix")
				case 1:
					return nil, errors.New("internal")
				}
				return nil, nil
			}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *receiptEnv) call(principal, cmd string, exit int, key string) {
	e.t.Helper()
	id := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: principal, Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	in, _ := json.Marshal(map[string]int{"Exit": exit})
	_, _ = e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: in, IdemKey: key})
	e.now = e.now.Add(time.Second)
}

// 2、4、23 三种拒绝都计入"回执与捕获失败"，其余 health.alert 的归类不变。
func TestReceiptRejectionsAreTheirOwnSignal(t *testing.T) {
	e := newReceiptEnv(t)
	e.call("dlg", "delegate.report", domain.ExitUsage, "k1")
	e.call("dlg", "delegate.report", domain.ExitPermissionDenied, "k2")
	e.call("dlg", "inbound.capture", domain.ExitBasisRequired, "k3")
	e.call("dlg", "inbound.capture", 1, "k4") // 其他失败不记告警
	e.event(domain.EvHealthAlert, "item_4", "rule action failed: boom", map[string]string{"action": "create_timer"})
	v := e.collect(nil)
	if len(v.ReceiptFailures) != 3 || v.Healthy() {
		t.Fatalf("receipt failures %+v", v.ReceiptFailures)
	}
	if r := v.ReceiptFailures[2]; r.Command != "inbound.capture" || r.Exit != domain.ExitBasisRequired || r.Credential != "dlg" {
		t.Fatalf("detail %+v", r)
	}
	if len(v.OtherAlerts) != 1 || v.OtherAlerts[0].Object != "item_4" || len(v.ModelUnavailable) != 0 {
		t.Fatalf("other alerts changed: %+v %+v", v.OtherAlerts, v.ModelUnavailable)
	}
}

// 同一凭据的同一命令之后成功一次即清除；另一凭据或另一命令的成功不清除；成功之后的新拒绝重新出现。
func TestReceiptClearsOnlyOnSameCredentialAndCommandSuccess(t *testing.T) {
	e := newReceiptEnv(t)
	e.call("dlg", "delegate.report", domain.ExitUsage, "k1")
	e.call("dlg", "inbound.capture", domain.ExitUsage, "k2")
	e.call("other", "delegate.report", 0, "k3")
	e.call("dlg", "inbound.capture", 0, "k4") // 另一命令
	if v := e.collect(nil); len(v.ReceiptFailures) != 1 || v.ReceiptFailures[0].Command != "delegate.report" {
		// k2 被同凭据同命令的 k4 清除，k1 仍在
		t.Fatalf("after unrelated successes: %+v", v.ReceiptFailures)
	}
	e.call("dlg", "delegate.report", 0, "") // 不带幂等键的成功同样清除
	if v := e.collect(nil); len(v.ReceiptFailures) != 0 || !v.Healthy() {
		t.Fatalf("not cleared: %+v", v.ReceiptFailures)
	}
	e.call("dlg", "delegate.report", domain.ExitBasisRequired, "k6")
	if v := e.collect(nil); len(v.ReceiptFailures) != 1 {
		t.Fatalf("new rejection after success must show: %+v", v.ReceiptFailures)
	}
}
