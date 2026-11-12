package inbound

import (
	"context"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// settle 把来源改成 kind 类型、把事件压回积压（disposition/reason），模拟旧版本或模型不可用时留下的积压。
func (e *env) backlogged(source, kind, id string, disp domain.Disposition, reason string) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec(`UPDATE sources_registry_source SET source_type = ? WHERE id = ?`, kind, source); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE sources_runner_event SET disposition = ?, unjudged_reason = ? WHERE source_id = ? AND event_id = ?`,
			string(disp), reason, source, id)
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) rules(id command.Identity, evidence, source string) (RulesResult, error) {
	return e.svc.BacklogRules(bg, command.Request{Identity: id, Evidence: evidence, Owner: "owner"}, source)
}

func disposedBy(t *testing.T, e *env, event string) []string {
	t.Helper()
	var who []string
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, event)
		for _, y := range es {
			if y.Event.Type == EvDisposed {
				who = append(who, y.Event.Actor.Principal)
			}
		}
		return err
	})
	return who
}

// 只跑规则：明确出口的按出口处置（含已 expired 的），其余原样（原因不改写），零模型调用，操作者是调用者。
func TestBacklogRulesDisposesRuleExitsAndLeavesRest(t *testing.T) {
	e := newEnv(t, false) // 无模型端点：所有事件落成积压
	e.emit("src_t", "plain",
		ev("opened", "ticket.opened", "登录页 500", map[string]any{"link": "https://git.example/org/web/issues/7"}),
		ev("noise", "ticket.noise", "机器人评论", nil),
		ev("other", "ticket.other", "没有规则", nil))
	e.emit("src_x", "plain", ev("news", "x.news", "本周简讯", nil))
	e.emit("src_m", "plain", ev("m1", "mail.received", "要判断的邮件", nil))
	e.backlogged("src_t", "ticket", "opened", domain.DispositionUnjudged, domain.UnjudgedModelUnavailable)
	e.backlogged("src_t", "ticket", "noise", domain.DispositionExpired, "")
	e.backlogged("src_t", "ticket", "other", domain.DispositionUnjudged, domain.UnjudgedUndecided)
	e.backlogged("src_x", "exits", "news", domain.DispositionUnjudged, domain.UnjudgedRestored)
	e.backlogged("src_m", "inbox", "m1", domain.DispositionUnjudged, "")
	e.configureModel() // 端点就绪也不能被调用

	res, err := e.rules(owner, "", "")
	if err != nil {
		t.Fatal(err)
	}
	if res != (RulesResult{Disposed: 3, Remaining: 2}) {
		t.Fatalf("result %+v", res)
	}
	t1 := e.byID("src_t")
	if x := t1["opened"]; x.Disposition != domain.DispositionProposed || x.Item == "" || e.item(x.Item).Phase != domain.PhaseProposed || e.item(x.Item).Project != e.web {
		t.Fatalf("opened: %+v", x)
	}
	if x := t1["noise"]; x.Disposition != domain.DispositionIgnored {
		t.Fatalf("noise: %+v", x)
	}
	if x := t1["other"]; x.Disposition != domain.DispositionUnjudged || x.UnjudgedReason != domain.UnjudgedUndecided {
		t.Fatalf("other must stay as it was: %+v", x)
	}
	if x := e.byID("src_x")["news"]; x.Disposition != domain.DispositionDigest || x.Item != "" {
		t.Fatalf("news: %+v", x)
	}
	if x := e.byID("src_m")["m1"]; x.Disposition != domain.DispositionUnjudged || x.UnjudgedReason != "" {
		t.Fatalf("m1 must stay as it was: %+v", x)
	}
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("model called %d times", n)
	}
	if who := disposedBy(t, e, t1["noise"].ID); who[len(who)-1] != "owner" {
		t.Fatalf("operator: %v", who)
	}
	if n := len(e.allItems()); n != 1 {
		t.Fatalf("items %d", n)
	}
	// 再跑一次：没有新的变化。
	if res, err := e.rules(owner, "", ""); err != nil || res != (RulesResult{Remaining: 2}) || len(e.allItems()) != 1 {
		t.Fatalf("second run: %+v %v", res, err)
	}
}

// 按来源限定；能挂靠的挂靠（挂靠由跟进消息归属负责，这里用替身只看入站如何调用它）。
func TestBacklogRulesAttachesAndScopesBySource(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_t", "plain", ev("a1", "ticket.opened", "能挂靠", nil), ev("a2", "ticket.noise", "噪声", nil))
	e.emit("src_u", "plain", ev("u1", "ticket.noise", "别的来源", nil))
	e.backlogged("src_t", "ticket", "a1", domain.DispositionExpired, "")
	e.backlogged("src_t", "ticket", "a2", domain.DispositionUnjudged, "")
	e.backlogged("src_u", "ticket", "u1", domain.DispositionUnjudged, "")
	e.svc.o.Intercept = func(ctx context.Context, ev domain.SourceEvent, _ string) (bool, string, error) {
		if ev.DedupeKey.EventID != "a1" {
			return false, "", nil
		}
		return true, "", e.st.Update(ctx, func(tx *store.Tx) error { return runner.SetAttachState(tx, ev.ID, runner.AttachProcessed, "itm_x") })
	}
	res, err := e.rules(owner, "", "src_t")
	if err != nil || res != (RulesResult{Attached: 1, Disposed: 1}) {
		t.Fatalf("%+v %v", res, err)
	}
	if x := e.byID("src_t")["a1"]; x.Disposition != domain.DispositionAttached || x.Item != "itm_x" || x.UnjudgedReason != "" {
		t.Fatalf("a1: %+v", x)
	}
	if x := e.byID("src_u")["u1"]; x.Disposition != domain.DispositionUnjudged {
		t.Fatalf("other source touched: %+v", x)
	}
}

// 缺本人原话被拒，积压不变；不受重判开关限制。
func TestBacklogRulesRequiresOwnersWords(t *testing.T) {
	e := newEnv(t, false)
	e.svc.o.Rejudge.Window = 0 // unjudged_rejudge_days: 0
	e.emit("src_t", "plain", ev("n1", "ticket.noise", "噪声", nil))
	e.backlogged("src_t", "ticket", "n1", domain.DispositionUnjudged, "")
	deleg := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent", Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	if _, err := e.rules(deleg, "", ""); err == nil || code(err) != domain.CodeBasisRequired {
		t.Fatalf("without words: %v", err)
	}
	if x := e.byID("src_t")["n1"]; x.Disposition != domain.DispositionUnjudged {
		t.Fatalf("changed without words: %+v", x)
	}
	if _, err := e.rules(deleg, "本人：先只跑规则", ""); err != nil && code(err) != domain.CodeSessionNotDirect {
		t.Fatalf("with words: %v", err)
	}
}
