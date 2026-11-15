package why

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/followmsg"
	kinbound "github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/spotcheck"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// 账本形状照真实部署里三条典型消息（规则忽略、模型忽略后抽查、成了建议后被驳回）造，经过只读不写。

var (
	t0      = time.Date(2026, 11, 14, 23, 24, 0, 0, time.UTC)
	inbound = domain.Actor{Type: domain.ActorSystem, Principal: "kernel:inbound", Via: domain.ViaSource}
	follow  = domain.Actor{Type: domain.ActorSystem, Principal: "kernel:followmsg", Via: domain.ViaSource}
)

type tenv struct {
	t *testing.T
	s *store.Store
}

func traceEnv(t *testing.T) tenv {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, command.Module, clock.Module, proc.Module, items.Module, registry.Module, runner.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return tenv{t, s}
}

func (e tenv) do(f func(tx *store.Tx) error) {
	e.t.Helper()
	if err := e.s.Update(ctx, f); err != nil {
		e.t.Fatal(err)
	}
}

// msg 落一条来源消息（与 runner 的写入同列）。
func (e tenv) msg(id, typ, subject, sender string, verified bool) {
	e.do(func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
			occurred_at, correlation_keys, sender, sender_verified, metrics, merge_key, messages, prev_metrics)
			VALUES ('src_gh', ?, ?, 'src_gh', 'run_1', ?, ?, '', '', '', ?, '[]', ?, ?, '', '', '', '')`,
			id, id, typ, subject, t0.Add(-20*time.Minute).UnixNano(), sender, verified)
		return err
	})
}

func (e tenv) ev(at time.Duration, typ, object string, actor domain.Actor, reason string, payload any) {
	e.do(func(tx *store.Tx) error {
		b, _ := json.Marshal(payload)
		ev := domain.Event{Type: typ, Object: object, Actor: actor, Reason: reason, Payload: b, At: t0.Add(at)}
		if actor.Via == domain.ViaRule {
			ev.Basis = &domain.Basis{RuleID: strings.TrimPrefix(actor.Principal, "rule:")}
		}
		_, err := eventlog.Append(tx, ev)
		return err
	})
}

func (e tenv) judgment(id, subject string, p float64, answer string, review *record.Review) {
	e.do(func(tx *store.Tx) error {
		return record.Put(tx, record.Record{ID: id, Use: "inbound_screen", Template: "inbound-triage/triage-screen@x", Subject: subject, Fingerprint: "f",
			Band: record.Band{Low: 0.15, High: 0.6}, DecidedBy: record.DecidedClassifier, Answer: answer, Review: review, CreatedAt: t0,
			Levels: []record.Level{{Backend: record.BackendClassifier, Model: "jev-1", PYes: &p, Outcome: record.OutcomeDecided}}})
	})
}

// item 建一个事项并记 item.created，返回 id 与快照。
func (e tenv) item(title string, phase domain.Phase, parent string, at time.Duration, actor domain.Actor, reason string) string {
	var id string
	e.do(func(tx *store.Tx) error {
		it, snap, err := items.Create(tx, domain.Item{Kind: "task", Title: title, Phase: phase}, parent, t0.Add(at))
		if err != nil {
			return err
		}
		id = it.ID
		typ := domain.EvItemCreated
		if parent != "" {
			typ = domain.EvDelegationCreated
		}
		_, err = eventlog.Append(tx, domain.Event{Type: typ, Object: id, Actor: actor, Reason: reason, Payload: snap, At: t0.Add(at)})
		return err
	})
	return id
}

func (e tenv) revise(id, op, typ string, at time.Duration, actor domain.Actor, reason string, f func(*domain.Item)) {
	e.do(func(tx *store.Tx) error {
		_, _, snap, err := items.Revise(tx, id, op, t0.Add(at), func(it *domain.Item) error { f(it); return nil })
		if err != nil {
			return err
		}
		_, err = eventlog.Append(tx, domain.Event{Type: typ, Object: id, Actor: actor, Reason: reason, Payload: snap, At: t0.Add(at)})
		return err
	})
}

func (e tenv) trace(id string, level authz.Level) Trace {
	e.t.Helper()
	var tr Trace
	err := e.s.View(ctx, func(tx *store.Tx) (err error) {
		r := Request{Actor: owner, Level: level, ID: id}
		if strings.HasPrefix(id, "itm_") {
			tr, err = ItemTrace(tx, policy.Policy{}, r)
		} else {
			tr, err = MessageTrace(tx, policy.Policy{}, r)
		}
		return
	})
	if err != nil {
		e.t.Fatal(err)
	}
	return tr
}

func kinds(es []Entry) string {
	var k []string
	for _, x := range es {
		k = append(k, x.Kind)
	}
	return strings.Join(k, ",")
}

// A：被规则忽略。一行规则，用"包/规则 id"称呼；没有判断行；到达时间是第一次处理它的时刻。
func TestTraceRuleIgnored(t *testing.T) {
	e := traceEnv(t)
	e.msg("sev_a", "pr.merged", "feat(web): 改为选填", "alice", false)
	e.ev(0, evDisposed, "sev_a", inbound, "rule inbound-triage/source_rules/pr-activity ignores this event",
		map[string]any{"disposition": "ignored", "rule": "inbound-triage/source_rules/pr-activity"})

	tr := e.trace("sev_a", authz.LevelWeb)
	if got := kinds(tr.Entries); got != "arrived,rule" {
		t.Fatalf("kinds %s", got)
	}
	if r := tr.Entries[1]; r.Who != (Who{Kind: WhoRule, Name: "inbound-triage/pr-activity"}) || r.Args["then"] != "ignore" || len(r.Steps) != 1 {
		t.Fatalf("rule entry %+v", r)
	}
	if h := tr.Message; h.ArrivedAt == nil || !h.ArrivedAt.Equal(t0) || h.Sender != "alice" || h.SenderVerified || h.Source != "src_gh" {
		t.Fatalf("head %+v", h)
	}
}

// B：规则交判断模型，模型把握高判为不值得看，之后本人抽查"确实不用"。新数据读 inbound.spot_reviewed；
// 旧数据只有判断记录上的结论，也能显示；判断记录过期后只剩处置载荷里的结论。
func TestTraceModelIgnoredThenSpotChecked(t *testing.T) {
	for name, withEvent := range map[string]bool{"event": true, "record only": false} {
		t.Run(name, func(t *testing.T) {
			e := traceEnv(t)
			e.msg("sev_b", "mail.received", "领取奖励", "x@example.invalid", false)
			e.judgment("jdg_b", "sev_b", 0.11, "no", &record.Review{Result: "correct", At: t0.Add(time.Hour)})
			e.ev(time.Second, evDisposed, "sev_b", inbound, "screened out: not worth a suggestion",
				map[string]any{"disposition": "ignored", "rule": "inbound-triage/source_rules/mail-received", "judgment": "jdg_b", "answer": "no", "decided_by": "classifier"})
			if withEvent {
				e.ev(time.Hour, evSpotReviewed, "sev_b", owner, "spot-check: correct", map[string]any{"verdict": "correct", "judgment": "jdg_b"})
			}
			tr := e.trace("sev_b", authz.LevelWeb)
			if got := kinds(tr.Entries); got != "arrived,rule,judged,spot" {
				t.Fatalf("kinds %s", got)
			}
			if r := tr.Entries[1]; r.Args["then"] != "judge" {
				t.Fatalf("rule %+v", r)
			}
			j := tr.Entries[2]
			lv := j.Args["levels"].([]map[string]any)[0]
			if j.Who != (Who{Kind: WhoModel, Name: "jev-1", Role: RoleJudge}) || lv["seg"] != "below" || lv["p"] != 0.11 || j.Args["low"] != 0.15 {
				t.Fatalf("judged %+v", j)
			}
			if s := tr.Entries[3]; s.Who.Kind != WhoOwner || s.Args["verdict"] != "correct" || (len(s.Steps) == 1) != withEvent {
				t.Fatalf("spot %+v", s)
			}
		})
	}
	t.Run("record expired", func(t *testing.T) {
		e := traceEnv(t)
		e.msg("sev_x", "mail.received", "x", "", false)
		e.ev(0, evDisposed, "sev_x", inbound, "screened out: not worth a suggestion",
			map[string]any{"disposition": "ignored", "judgment": "jdg_gone", "answer": "no", "decided_by": "classifier"})
		tr := e.trace("sev_x", authz.LevelWeb)
		if j := tr.Entries[1]; j.Kind != KJudged || j.Args["expired"] != true || j.Args["answer"] != "no" {
			t.Fatalf("expired judgment %+v", tr.Entries)
		}
	})
}

// C：判断模型提成建议，起草失败（旧数据只在理由里），同一 PR 的后续消息挂上，本人驳回。
// 新建事项的几条机械记录并进"提成建议"一行；后续消息的同步与挂靠合成一行；起草成败如实写。
func TestTraceProposedThenDismissed(t *testing.T) {
	e := traceEnv(t)
	e.msg("sev_c", "pr.opened", "test(e2e): 等摘要服务", "alice", true)
	e.msg("sev_c2", "pr.merged", "test(e2e): 等摘要服务", "", false)
	e.judgment("jdg_c", "sev_c", 0.68, "yes", nil)
	reason := "screened in; draft failed: model: output violates schema: content is not a single JSON value"
	it := e.item("test(e2e): 等摘要服务", domain.PhaseProposed, "", 7*time.Second, inbound, reason)
	e.revise(it, "update", domain.EvItemUpdated, 7*time.Second, inbound, "merged source event sev_c", func(*domain.Item) {})
	e.ev(7*time.Second, evDisposed, "sev_c", inbound, reason, map[string]any{"disposition": "proposed", "item": it, "judgment": "jdg_c"})
	e.revise(it, "update", domain.EvItemUpdated, 8*time.Second, follow, "source object updated by pr.merged sev_c2", func(x *domain.Item) { x.Body = "{\"action\":\"closed\"}" })
	e.ev(8*time.Second, evFollowAttach, it, follow, "message recorded on the item", map[string]any{"message": "sev_c2"})
	e.revise(it, "proposal.dismiss", domain.EvItemPhaseChanged, 5*time.Minute, owner, "proposal dismissed", func(x *domain.Item) { x.Phase = domain.PhaseDropped })

	tr := e.trace("sev_c", authz.LevelWeb)
	if got := kinds(tr.Entries); got != "arrived,judged,proposed,drafted,follow,dismissed" {
		t.Fatalf("kinds %s", got)
	}
	if j := tr.Entries[1]; j.Args["levels"].([]map[string]any)[0]["seg"] != "above" {
		t.Fatalf("judged %+v", j)
	}
	if p := tr.Entries[2]; p.Item == nil || p.Item.ID != it || p.Item.Title == "" || len(p.Steps) != 1 {
		t.Fatalf("proposed %+v", p)
	}
	if d := tr.Entries[3]; d.Args["outcome"] != "failed" || d.Args["code"] != "schema" {
		t.Fatalf("drafted %+v", d)
	}
	f := tr.Entries[4]
	if f.Who != (Who{Kind: WhoSource, Name: "src_gh"}) || f.Args["subject"] != "test(e2e): 等摘要服务" || len(f.Steps) != 2 || !f.Mech {
		t.Fatalf("follow %+v", f)
	}
	if fs, _ := f.Args["fields"].([]string); len(fs) != 1 || fs[0] != "body" {
		t.Fatalf("follow fields %+v", f.Args)
	}
	if d := tr.Entries[5]; d.Who.Kind != WhoOwner || len(d.Steps) != 1 {
		t.Fatalf("dismissed %+v", d)
	}

	// 后续消息自己的经过：挂到了哪个事项，以及（旧数据）当时改写了哪些字段。
	f2 := e.trace("sev_c2", authz.LevelWeb)
	if got := kinds(f2.Entries); got != "arrived,attached" || f2.Entries[1].Item.ID != it || f2.Entries[1].Args["via"] != "follow" {
		t.Fatalf("follow message trace %s %+v", got, f2.Entries)
	}

	// 事项经过：开头接上来源消息，"新建"不再单列。
	itr := e.trace(it, authz.LevelWeb)
	if len(itr.Origins) != 1 || itr.Origins[0].Message.ID != "sev_c" || kinds(itr.Origins[0].Entries) != "arrived,judged,proposed,drafted" {
		t.Fatalf("origins %+v", itr.Origins)
	}
	if got := kinds(itr.Entries); got != "follow,dismissed" {
		t.Fatalf("item kinds %s", got)
	}
}

// 起草：新数据读载荷的 draft；旧数据没有前缀时写"不清楚"，不猜成功。积压与放回如实写。
func TestTraceDraftAndBacklog(t *testing.T) {
	e := traceEnv(t)
	e.msg("sev_d", "issue.opened", "新 issue", "", false)
	e.ev(0, evDisposed, "sev_d", inbound, "model endpoint unavailable", map[string]any{"disposition": "unjudged", "unjudged_reason": "model_unavailable"})
	e.ev(time.Hour, evRequeued, "sev_d", inbound, "rejudging", map[string]any{})
	e.judgment("jdg_d", "sev_d", 0.9, "yes", nil)
	it := e.item("整理过的标题", domain.PhaseProposed, "", time.Hour, inbound, "screened in")
	e.ev(time.Hour, evDisposed, "sev_d", inbound, "screened in", map[string]any{"disposition": "proposed", "item": it, "judgment": "jdg_d", "draft": map[string]any{"outcome": "ok"}})
	tr := e.trace("sev_d", authz.LevelWeb)
	if got := kinds(tr.Entries); got != "arrived,backlog,judged,requeued,proposed,drafted" && got != "arrived,backlog,requeued,judged,proposed,drafted" {
		t.Fatalf("kinds %s", got)
	}
	last := tr.Entries[len(tr.Entries)-1]
	if last.Args["outcome"] != "ok" || last.Who.Role != RoleGenerate {
		t.Fatalf("drafted %+v", last)
	}
	if b := tr.Entries[1]; b.Args["reason"] != "model_unavailable" {
		t.Fatalf("backlog %+v", b)
	}

	e.msg("sev_old", "issue.opened", "旧数据", "", false)
	e.judgment("jdg_old", "sev_old", 0.9, "yes", nil)
	old := e.item("旧数据", domain.PhaseProposed, "", 0, inbound, "screened in")
	e.ev(0, evDisposed, "sev_old", inbound, "screened in", map[string]any{"disposition": "proposed", "item": old, "judgment": "jdg_old"})
	if d := e.trace("sev_old", authz.LevelWeb).Entries; d[len(d)-1].Args["outcome"] != "unknown" {
		t.Fatalf("old draft %+v", d[len(d)-1])
	}
}

// 事项经过：改了什么按快照比出来；派出去的子事项（派出、开工、交回）并进父事项；原始记录按凭据级别裁剪。
func TestTraceItemWithDelegation(t *testing.T) {
	e := traceEnv(t)
	it := e.item("修登录", domain.PhaseProposed, "", 0, owner, "created")
	e.revise(it, "update", domain.EvItemUpdated, time.Minute, owner, "", func(x *domain.Item) { x.Title = "修登录页"; x.Tags = []string{"agent-ok"} })
	e.revise(it, "transition", domain.EvItemPhaseChanged, 2*time.Minute, owner, "proposal accepted", func(x *domain.Item) { x.Phase = domain.PhaseOpen })
	child := e.item("修登录页", domain.PhaseOpen, it, 3*time.Minute, agent, "delegated to coder")
	e.revise(it, "delegate.create", domain.EvItemPhaseChanged, 3*time.Minute, agent, "waiting on coder", func(x *domain.Item) {
		x.Phase, x.WaitingOn = domain.PhaseWaiting, &domain.WaitingOn{Kind: domain.WaitingExecutor, Ref: "coder"}
	})
	e.ev(4*time.Minute, evDispatched, child, agent, "dispatched to coder", map[string]any{"dispatch": map[string]any{"executor": "coder", "harness": "h", "state": "running"}})
	e.ev(5*time.Minute, evDispatchUpd, child, agent, "dispatch ended", map[string]any{"dispatch": map[string]any{"executor": "coder", "state": "ended"}})

	tr := e.trace(it, authz.LevelWeb)
	if got := kinds(tr.Entries); got != "created,edited,accepted,delegated,phase,dispatched,dispatch_updated" {
		t.Fatalf("kinds %s", got)
	}
	ed := tr.Entries[1].Args
	if fs := ed["fields"].([]string); len(fs) != 2 || fs[0] != "title" || fs[1] != "tags" || ed["tags_added"].([]string)[0] != "agent-ok" {
		t.Fatalf("edited %+v", ed)
	}
	if d := tr.Entries[3]; d.Item == nil || d.Item.ID != child || d.Who.Kind != WhoAgent || d.Who.Name != "bot" {
		t.Fatalf("delegated %+v", d)
	}
	if p := tr.Entries[4].Args; p["to"] != "waiting" || p["waiting_ref"] != "coder" {
		t.Fatalf("phase %+v", p)
	}
	if u := tr.Entries[6]; !u.Mech || u.Args["state"] != "ended" {
		t.Fatalf("dispatch_updated %+v", u)
	}
	// observer 只见信封：原始记录不带 reason 与 payload。
	for _, en := range e.trace(it, authz.LevelObserver).Entries {
		for _, s := range en.Steps {
			if s.Reason != "" || len(s.Payload) != 0 {
				t.Fatalf("observer sees content: %+v", s)
			}
		}
	}
}

// 本包不依赖入站、抽检、跟进消息与委派，事件类型的字面值在这里对齐。
func TestTraceEventTypesMatchProducers(t *testing.T) {
	for got, want := range map[string]string{
		evDisposed: kinbound.EvDisposed, evRequeued: kinbound.EvRequeued, evCaptured: kinbound.EvCaptured, evSpotReviewed: spotcheck.EvReviewed,
		evFollowAttach: followmsg.EvAttached, evFollowDone: followmsg.EvProcessed, evFollowBack: followmsg.EvHandedBack, evFollowRelease: followmsg.EvReleased,
		evObserved: rules.EvObserved, evDispatched: delegation.EvDispatched, evDispatchUpd: delegation.EvDispatchUpdated,
	} {
		if got != want {
			t.Errorf("%q != %q", got, want)
		}
	}
}

// 写作模型替事项写一句话摘要失败：是写作模型的一次生成与一条告警，不是规则，也不是 Agent 后台运行。
func TestTraceGenerateCallIsNotARule(t *testing.T) {
	e := traceEnv(t)
	it := e.item("修登录", domain.PhaseOpen, "", 0, owner, "created")
	clockActor := domain.Actor{Type: domain.ActorSystem, Principal: "kernel:summaries", Via: domain.ViaClock}
	e.do(func(tx *store.Tx) error {
		for _, ev := range []domain.Event{
			{Type: domain.EvRunFinished, Object: it, Actor: clockActor, Reason: "generate failed", Payload: json.RawMessage(`{"category":"generate","state":"failed","template":"one-line-summary"}`),
				Basis: &domain.Basis{RuleID: "decision-card/one-line-summary"}, At: t0.Add(time.Minute)},
			{Type: domain.EvHealthAlert, Object: it, Actor: clockActor, Reason: "one-line-summary draft failed", Payload: json.RawMessage(`{}`),
				Basis: &domain.Basis{RuleID: "decision-card/one-line-summary"}, At: t0.Add(time.Minute)},
		} {
			if _, err := eventlog.Append(tx, ev); err != nil {
				return err
			}
		}
		return nil
	})
	tr := e.trace(it, authz.LevelWeb)
	if got := kinds(tr.Entries); got != "created,generated,alert" {
		t.Fatalf("kinds %s", got)
	}
	if g := tr.Entries[1]; g.Who != (Who{Kind: WhoModel, Role: RoleGenerate}) || g.Args["state"] != "failed" || !g.Mech {
		t.Fatalf("generated %+v", g)
	}
	if a := tr.Entries[2]; a.Who.Kind != WhoSystem {
		t.Fatalf("alert %+v", a)
	}
}
