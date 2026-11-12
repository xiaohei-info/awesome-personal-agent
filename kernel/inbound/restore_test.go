package inbound

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var (
	winFrom = time.Date(2026, 2, 10, 0, 0, 0, 0, time.UTC)
	winTo   = time.Date(2026, 2, 20, 0, 0, 0, 0, time.UTC)
)

// restoreEnv 造一批 v0.1 升级后的账本：窗口内旧级联忽略（src_a 3 条、src_b 2 条）、对照组（规则忽略、本人忽略、窗口外、新级联忽略），
// 以及一条有判断记录的旧忽略（src_c，真实打过分）。来源事件都经真实流水线入账，再把处置改成升级前的样子。
type restoreEnv struct {
	*scoreEnv
	old                      []string // 窗口内应命中的事件 id（来源事件 id）
	rule, user, outside, new string
}

func newRestoreEnv(t *testing.T) *restoreEnv {
	t.Helper()
	e := &restoreEnv{scoreEnv: newScoreEnv(t)}
	day := func(d int) string { return winFrom.AddDate(0, 0, d).Format(time.RFC3339) }
	mk := func(source string, n int, prefix string, at string) {
		var evs []map[string]any
		for i := range n {
			evs = append(evs, ev(fmt.Sprintf("%s%d", prefix, i), "ticket.other", fmt.Sprintf("%s %d", prefix, i), map[string]any{"occurred_at": at}))
		}
		e.emit(source, "plain", evs...)
	}
	mk("src_a", 3, "olda", day(2))
	mk("src_b", 2, "oldb", day(4))
	mk("src_a", 2, "rule", day(3))
	mk("src_a", 1, "user", day(3))
	mk("src_a", 1, "out", winTo.AddDate(0, 0, 5).Format(time.RFC3339)) // 窗口后
	mk("src_a", 1, "new", day(5))
	mk("src_c", 1, "scored", day(6))
	e.configure(true)
	e.cls.SetDefaultPYes(0.40)
	if _, err := e.score(owner, "", "src_c"); err != nil { // 真实的判断记录
		t.Fatal(err)
	}
	old := domain.Actor{Type: domain.ActorSystem, Principal: systemPrinc, Via: domain.ViaSource}
	ignore := func(source, id string, actor domain.Actor, rule, judgment string) string {
		x := e.byID(source)[id]
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			if _, err := tx.Exec(`UPDATE sources_runner_event SET disposition = 'ignored', unjudged_reason = '', judgment = ? WHERE id = ?`, judgment, x.ID); err != nil {
				return err
			}
			p, _ := json.Marshal(map[string]any{"event": x.ID, "disposition": "ignored", "item": "", "rule": rule, "unjudged_reason": ""})
			ev := domain.Event{Type: EvDisposed, Object: x.ID, Actor: actor, Reason: "judge: ignore", Payload: p, At: e.now}
			if rule != "" {
				ev.Basis = &domain.Basis{PolicyVersion: 1, RuleID: rule}
			}
			_, err := eventlog.Append(tx, ev)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return x.ID
	}
	for _, id := range []string{"olda0", "olda1", "olda2"} {
		e.old = append(e.old, ignore("src_a", id, old, "", ""))
	}
	for _, id := range []string{"oldb0", "oldb1"} {
		e.old = append(e.old, ignore("src_b", id, old, "", ""))
	}
	e.rule = ignore("src_a", "rule0", old, "inbound-triage/x", "") // 内核身份写入，但载荷带规则：规则的 ignore 出口
	ignore("src_a", "rule1", domain.Actor{Type: domain.ActorSystem, Principal: "rule:inbound-triage/x", Via: domain.ViaRule}, "inbound-triage/x", "")
	e.user = ignore("src_a", "user0", domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, "", "")
	e.outside = ignore("src_a", "out0", old, "", "")
	e.new = ignore("src_a", "new0", old, "", "jdg_new") // 新级联：带判断记录 id
	ignore("src_c", "scored0", old, "", "")             // 旧忽略，但真有判断记录（影子分数）
	return e
}

func (e *restoreEnv) preview(source string) RestorePreview {
	e.t.Helper()
	var out RestorePreview
	e.view(func(tx *store.Tx) {
		if err := RestoreWindow(tx, winFrom, winTo, source); err != nil {
			e.t.Fatal(err)
		}
		var err error
		if out, err = PreviewRestore(tx, winFrom, winTo, source); err != nil {
			e.t.Fatal(err)
		}
	})
	return out
}

func (e *restoreEnv) restore(who command.Identity, evidence, source, idem string) (command.Result, error) {
	in := map[string]any{"from": winFrom, "to": winTo}
	if source != "" {
		in["source"] = source
	}
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: who, Command: CmdBacklogRestore, Input: b, Owner: "owner", Evidence: evidence, IdemKey: idem})
}

func restored(r command.Result) (n int) {
	var v RestoreResult
	_ = json.Unmarshal(r.Value, &v)
	return v.Restored
}

func (e *restoreEnv) head() (n int64) {
	e.view(func(tx *store.Tx) {
		var err error
		if n, err = eventlog.Head(tx); err != nil {
			e.t.Fatal(err)
		}
	})
	return
}

// ACT-RST-03、04、08：预览只读，只命中窗口内无判断记录、由内核依旧级联写入的 ignored，按来源分布；对照组不命中。
func TestRestorePreviewMatchesOnlyOldCascadeIgnored(t *testing.T) {
	e := newRestoreEnv(t)
	before := e.head()
	p := e.preview("")
	if p.Matched != 5 || len(p.Sources) != 2 || p.Sources[0] != (SourceCount{"src_a", 3}) || p.Sources[1] != (SourceCount{"src_b", 2}) {
		t.Fatalf("preview %+v", p)
	}
	if p := e.preview("src_b"); p.Matched != 2 || len(p.Sources) != 1 || p.Sources[0].Source != "src_b" {
		t.Fatalf("src_b preview %+v", p)
	}
	if p := e.preview("src_c"); p.Matched != 0 || p.Sources == nil {
		t.Fatalf("a record-bearing ignore must not match: %+v", p)
	}
	if e.head() != before || len(e.stub.Requests()) != 0 || len(e.cls.Batches()) != 1 { // 唯一一批是 setup 里 src_c 的打分
		t.Fatal("preview must not write or call a model")
	}
	e.view(func(tx *store.Tx) {
		for name, args := range map[string][3]any{
			"equal":   {winFrom, winFrom, ""},
			"reverse": {winTo, winFrom, ""},
			"missing": {time.Time{}, winTo, ""},
		} {
			if err := RestoreWindow(tx, args[0].(time.Time), args[1].(time.Time), args[2].(string)); code(err) != domain.CodeInvalidRequest {
				t.Fatalf("%s: %v", name, err)
			}
		}
		if err := RestoreWindow(tx, winFrom, winTo, "no-such-source"); code(err) != domain.CodeNotFound {
			t.Fatalf("unknown source: %v", err)
		}
	})
}

// ACT-RST-05：缺本人原话、非 direct 会话、observer 都不能放回；被拒后预览条数不变。
func TestRestoreAuthorizationBoundary(t *testing.T) {
	e := newRestoreEnv(t)
	deleg := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
	group := deleg
	group.Actor.SessionKind = "group"
	obs := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "obs", Via: domain.ViaCLI}, Level: authz.LevelObserver}
	for name, c := range map[string]struct {
		who      command.Identity
		evidence string
		want     domain.Code
	}{
		"no words":    {deleg, "", domain.CodeBasisRequired},
		"group":       {group, "本人：放回", domain.CodeSessionNotDirect},
		"observer":    {obs, "本人：放回", domain.CodePermissionDenied},
		"bad session": {deleg, "", domain.CodeBasisRequired},
	} {
		if _, err := e.restore(c.who, c.evidence, "", ""); code(err) != c.want {
			t.Fatalf("%s: %v", name, err)
		}
	}
	if p := e.preview(""); p.Matched != 5 {
		t.Fatalf("a refused restore changed the ledger: %+v", p)
	}
}

// ACT-RST-06、09、10：按来源放回，零模型调用、没有事项；每条一条 inbound.disposed（操作者是 delegate，载荷含原处置与时间段）；同一时间段再放回 0 条。
func TestRestoreMovesEventsBackWithAuditAndIsIdempotent(t *testing.T) {
	e := newRestoreEnv(t)
	deleg := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
	batches := len(e.cls.Batches())

	r, err := e.restore(deleg, "本人：把旧级联忽略的放回来", "src_a", "")
	if err != nil || restored(r) != 3 {
		t.Fatalf("restore: %+v %v", r.Value, err)
	}
	for _, id := range []string{"olda0", "olda1", "olda2"} {
		x := e.byID("src_a")[id]
		if x.Disposition != domain.DispositionUnjudged || x.UnjudgedReason != domain.UnjudgedRestored {
			t.Fatalf("%s: %+v", id, x)
		}
		var disposed []domain.Event
		for _, y := range e.eventsOf(x.ID) {
			if y.Type == EvDisposed && y.Actor.Principal == "dlg" {
				disposed = append(disposed, y)
			}
		}
		var p map[string]any
		if len(disposed) != 1 || json.Unmarshal(disposed[0].Payload, &p) != nil || p["previous"] != "ignored" || p["unjudged_reason"] != "restored" || p["from"] == nil || p["to"] == nil {
			t.Fatalf("%s: audit %+v %v", id, disposed, p)
		}
	}
	// 对照组、窗口外、别的来源、有记录的都不动。
	for _, c := range []struct{ src, id string }{{"src_a", "rule0"}, {"src_a", "rule1"}, {"src_a", "user0"}, {"src_a", "out0"}, {"src_a", "new0"}, {"src_b", "oldb0"}, {"src_c", "scored0"}} {
		if x := e.byID(c.src)[c.id]; x.Disposition != domain.DispositionIgnored {
			t.Fatalf("%s/%s must stay ignored: %+v", c.src, c.id, x)
		}
	}
	if len(e.stub.Requests()) != 0 || len(e.cls.Batches()) != batches || len(e.allItems()) != 0 {
		t.Fatal("restoring must not call a model or create items")
	}

	// 其余来源；再来一次与带同一个幂等键的两次都是 0 条，没有新的 Event。
	if r, err = e.restore(deleg, "本人：放回", "", "k1"); err != nil || restored(r) != 2 {
		t.Fatalf("rest: %+v %v", r.Value, err)
	}
	head := e.head()
	for _, key := range []string{"", "k2", "k2"} {
		r, err = e.restore(deleg, "本人：再放一次", "", key)
		if err != nil || restored(r) != 0 {
			t.Fatalf("again (%q): %+v %v", key, r.Value, err)
		}
	}
	if e.head() != head {
		t.Fatal("a repeated restore wrote events")
	}
}

// ACT-RST-11、12、13：restored 的事件不被自动重判、不转 expired；只经放开重判，走新的级联后带判断记录，不再可放回。
func TestRestoredEventsOnlyRejudgedThroughBacklogRelease(t *testing.T) {
	e := newRestoreEnv(t)
	e.svc.o.Now = e.Now
	e.svc.o.BacklogReleasePerDay = 5
	if _, err := e.restore(owner, "本人：放回", "", ""); err != nil {
		t.Fatal(err)
	}
	// 另有一条普通的老积压，作对照：它到期，restored 的不到期。
	e.emit("src_d", "plain", ev("stale", "ticket.other", "旧积压", map[string]any{"occurred_at": winFrom.Format(time.RFC3339)}))
	e.backlogged("src_d", "ticket", "stale", domain.DispositionUnjudged, domain.UnjudgedModelUnavailable)
	e.svc.o.Rejudge = RejudgeLimits{Window: 7 * 24 * time.Hour, Batch: 20, Backoff: time.Minute}
	e.now = winTo.AddDate(0, 0, 18)
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	if x := e.byID("src_d")["stale"]; x.Disposition != domain.DispositionExpired {
		t.Fatalf("control did not expire: %+v", x)
	}
	for _, x := range append(e.events("src_a"), e.events("src_b")...) {
		if x.UnjudgedReason == domain.UnjudgedRestored && (x.Disposition != domain.DispositionUnjudged || x.Rejudges != 0) {
			t.Fatalf("restored event was rejudged or expired: %+v", x)
		}
	}

	e.cls.SetDefaultPYes(0.01) // 新级联：分类端点低分即忽略，并留下判断记录
	if _, err := e.rejudge("src_a", 0); err != nil {
		t.Fatal(err)
	}
	if _, err := e.rejudge("src_b", 0); err != nil {
		t.Fatal(err)
	}
	for _, id := range e.old {
		var x domain.SourceEvent
		e.view(func(tx *store.Tx) {
			var err error
			if x, err = runner.Event(tx, id); err != nil {
				t.Fatal(err)
			}
		})
		if x.Disposition != domain.DispositionIgnored || len(e.records(id)) == 0 {
			t.Fatalf("%s after rejudge: %+v records=%d", id, x, len(e.records(id)))
		}
	}
	if p := e.preview(""); p.Matched != 0 {
		t.Fatalf("an event that went through the new cascade must not be restorable: %+v", p)
	}
}
