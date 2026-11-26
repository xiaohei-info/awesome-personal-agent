package inbound

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func (e *env) byID(source string) map[string]domain.SourceEvent {
	out := map[string]domain.SourceEvent{}
	for _, x := range e.events(source) {
		out[x.DedupeKey.EventID] = x
	}
	return out
}

// 模型不可用 → 积压 → 配上端点但仍失败（只探一条，退避期内不再探）→ 恢复 → 自动重判进建议流；再跑一轮不重复。
func TestRejudgeAfterModelRecovers(t *testing.T) {
	e := newEnv(t, false)
	e.svc.o.Now = e.Now
	e.emit("src_m", "inbox", ev("m1", "mail.received", "合同请签字", nil), ev("m2", "mail.received", "发票", nil))
	for id, x := range e.byID("src_m") {
		if x.Disposition != domain.DispositionUnjudged || x.UnjudgedReason != domain.UnjudgedModelUnavailable {
			t.Fatalf("%s: %+v", id, x)
		}
	}
	if err := e.svc.Rejudge(bg); err != nil || len(e.stub.Requests()) != 0 {
		t.Fatalf("no endpoint: err=%v requests=%d", err, len(e.stub.Requests()))
	}

	e.configureModel()
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	e.stub.SetDefault(modelstub.Reply{Status: 500})
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	got := e.byID("src_m")
	if n := len(e.stub.Requests()); n != 2 || got["m1"].Rejudges != 1 || got["m1"].Disposition != domain.DispositionUnjudged || got["m2"].Rejudges != 0 {
		t.Fatalf("probe should stop after the first failure: requests=%d %+v", n, got)
	}
	if err := e.svc.Rejudge(bg); err != nil || len(e.stub.Requests()) != 2 {
		t.Fatalf("backoff not honoured: requests=%d", len(e.stub.Requests()))
	}

	e.cls.Reset()
	e.screen(0.9)
	e.now = e.now.Add(DefaultRejudge.Backoff + time.Minute)
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	e.drainBatch()
	for id, x := range e.byID("src_m") {
		if x.Disposition != domain.DispositionProposed || x.Item == "" || e.item(x.Item).Phase != domain.PhaseProposed {
			t.Fatalf("%s not rejudged into a proposal: %+v", id, x)
		}
	}
	if err := e.svc.Rejudge(bg); err != nil || len(e.allItems()) != 2 {
		t.Fatalf("second round must be a no-op: err=%v items=%d", err, len(e.allItems()))
	}
}

// 超出时限的转 expired（不再重判、不计入积压）；本人按来源批量忽略 unjudged 与 expired，每条记一个操作者为本人的处置事件。
func TestStaleExpiresAndOwnerIgnoresBacklog(t *testing.T) {
	e := newEnv(t, false)
	e.svc.o.Now = e.Now
	old := t0.Add(-DefaultRejudge.Window - time.Hour).Format(time.RFC3339)
	e.emit("src_m", "inbox", ev("m1", "mail.received", "旧邮件", map[string]any{"occurred_at": old}), ev("m2", "mail.received", "新邮件", nil))
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	got := e.byID("src_m")
	if got["m1"].Disposition != domain.DispositionExpired || got["m2"].Disposition != domain.DispositionUnjudged {
		t.Fatalf("expire: %+v", got)
	}
	var backlog []domain.SourceEvent
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { backlog, err = runner.Unjudged(tx); return })
	if len(backlog) != 1 {
		t.Fatalf("expired must leave the backlog: %+v", backlog)
	}

	in, _ := json.Marshal(map[string]any{"source": "src_m"})
	res, err := e.x.Do(bg, command.Request{Identity: owner, Command: CmdIgnoreUnjudged, Input: in, Owner: "owner"})
	if err != nil || string(res.Value) != `{"ignored":2}` {
		t.Fatalf("batch ignore: %v %s", err, res.Value)
	}
	for id, x := range e.byID("src_m") {
		if x.Disposition != domain.DispositionIgnored {
			t.Fatalf("%s: %+v", id, x)
		}
		var byOwner bool
		_ = e.st.View(bg, func(tx *store.Tx) error {
			es, err := eventlog.ReadObject(tx, x.ID)
			for _, y := range es {
				byOwner = byOwner || (y.Event.Type == EvDisposed && y.Event.Actor.Principal == "owner" && y.Event.Actor.Type == domain.ActorHuman)
			}
			return err
		})
		if !byOwner {
			t.Fatalf("%s: no disposal event by the owner", id)
		}
	}
	single, _ := json.Marshal(map[string]any{"event": got["m2"].ID})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: CmdIgnoreUnjudged, Input: single, Owner: "owner"}); code(err) != domain.CodeConflict {
		t.Fatalf("ignoring an ignored event: %v", err)
	}
}

// unjudged_rejudge_days: 0：模型端点已配置也不重判、不转 expired，积压原样保留。
func TestRejudgeOffLeavesBacklogUntouched(t *testing.T) {
	e := newEnv(t, false)
	e.svc.o.Now = e.Now
	e.svc.o.Rejudge.Window = 0
	old := t0.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	e.emit("src_m", "inbox", ev("m1", "mail.received", "旧邮件", map[string]any{"occurred_at": old}), ev("m2", "mail.received", "新邮件", nil))
	e.configureModel()
	e.screen(0.9)
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	for id, x := range e.byID("src_m") {
		if x.Disposition != domain.DispositionUnjudged || x.Rejudges != 0 {
			t.Fatalf("%s touched while rejudge is off: %+v", id, x)
		}
	}
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("model called %d times", n)
	}
}

// v0.1 留下的 undecided 与空原因按 model_unavailable 对待（§6.10、§16.5）：窗口内的随模型恢复重判进建议流，窗口外的到期。
func TestLegacyUndecidedRejudgedAndExpires(t *testing.T) {
	e := newEnv(t, false)
	e.svc.o.Now = e.Now
	old := t0.Add(-DefaultRejudge.Window - time.Hour).Format(time.RFC3339)
	e.emit("src_m", "inbox", ev("m1", "mail.received", "旧邮件", map[string]any{"occurred_at": old}),
		ev("m2", "mail.received", "合同请签字", nil), ev("m3", "mail.received", "发票", nil))
	got := e.byID("src_m")
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for id, why := range map[string]string{"m1": domain.UnjudgedUndecided, "m2": domain.UnjudgedUndecided, "m3": ""} {
			if _, err := tx.Exec(`UPDATE sources_runner_event SET unjudged_reason = ? WHERE id = ?`, why, got[id].ID); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	e.configureModel()
	e.screen(0.9)
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	e.drainBatch()
	got = e.byID("src_m")
	if got["m1"].Disposition != domain.DispositionExpired {
		t.Fatalf("old undecided must expire: %+v", got["m1"])
	}
	for _, id := range []string{"m2", "m3"} {
		if x := got[id]; x.Disposition != domain.DispositionProposed || x.Rejudges != 1 {
			t.Fatalf("%s not rejudged into a proposal: %+v", id, x)
		}
	}
}

// withdrawn 只认 ChoiceIgnore 与已知前缀；认不出的报错，事件留在 pending，不静默归成不会重判的状态。
func TestWithdrawnRejectsUnknownVerdict(t *testing.T) {
	x := domain.SourceEvent{ID: "sev_1"}
	for verdict, want := range map[string][2]string{
		ChoiceIgnore:              {string(domain.DispositionIgnored), ""},
		unavailablePrefix + "x":   {string(domain.DispositionUnjudged), domain.UnjudgedModelUnavailable},
		pausedPrefix + "x":        {string(domain.DispositionUnjudged), domain.UnjudgedModelsPaused},
		outboundUnsetPrefix + "x": {string(domain.DispositionUnjudged), domain.UnjudgedOutboundUnset},
		outboundNonePrefix + "x":  {string(domain.DispositionDigest), ""},
	} {
		d, err := withdrawn(x, proposal{}, verdict)
		if err != nil || string(d.Disposition) != want[0] || d.UnjudgedReason != want[1] {
			t.Fatalf("%q: %+v err=%v", verdict, d, err)
		}
	}
	if _, err := withdrawn(x, proposal{}, "something new"); err == nil {
		t.Fatal("unknown verdict must not be settled")
	}
}
