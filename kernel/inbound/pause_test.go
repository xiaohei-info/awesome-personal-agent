package inbound

import (
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (e *env) setPaused(on bool) {
	e.t.Helper()
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "pause", func(p *policy.Policy) error { p.ModelsPaused = on; return nil })
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// 暂停模型（§8.7）：两个端点都配着，规则判不了的事件记未判定，原因 models_paused（不是 model_unavailable），零模型请求；
// 规则照常；暂停期间不自动重判；恢复后新事件立即被判断。
func TestPausedModelsLeaveEventUnjudgedAsPaused(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.screen(0.9)
	e.draft(`{"title":"恢复后起草","kind":"task"}`)
	e.setPaused(true)

	e.emit("src_x", "other", ev("p1", "thing.happened", "暂停期间的事件", nil))
	if d := e.events("src_x")[0]; d.Disposition != domain.DispositionUnjudged || d.UnjudgedReason != domain.UnjudgedModelsPaused || d.Item != "" {
		t.Fatalf("paused event %+v", d)
	}
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "规则照常", nil))
	if got := e.events("src_t")[0].Disposition; got != domain.DispositionProposed {
		t.Fatalf("rules must work while paused: %s", got)
	}
	e.now = e.now.Add(15 * time.Minute)
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	if d := e.events("src_x")[0]; d.Disposition != domain.DispositionUnjudged || d.Rejudges != 0 {
		t.Fatalf("paused backlog must not be rejudged while paused: %+v", d)
	}
	if n, m := len(e.cls.Batches()), len(e.stub.Requests()); n != 0 || m != 0 {
		t.Fatalf("paused models were called: judge %d, generate %d", n, m)
	}

	e.setPaused(false)
	e.emit("src_x", "other", ev("p2", "thing.happened", "恢复后的事件", nil))
	var fresh domain.SourceEvent
	for _, d := range e.events("src_x") {
		if d.DedupeKey.EventID == "p2" {
			fresh = d
		}
	}
	if fresh.Disposition != domain.DispositionProposed || e.item(fresh.Item).Title != "恢复后起草" {
		t.Fatalf("event after resume %+v", fresh)
	}
}

// 本人触发的影子打分与放开重判需要模型：暂停时拒绝（models_paused），不碰积压、不发请求。
func TestBacklogScoreAndRejudgeRefusedWhilePaused(t *testing.T) {
	e := newReleaseEnv(t, 5, 2, 0, 0)
	e.setPaused(true)
	if _, err := e.score(owner, "本人：打分", ""); code(err) != domain.CodeModelsPaused {
		t.Fatalf("score while paused: %v", err)
	}
	if _, err := e.rejudge("", 0); code(err) != domain.CodeModelsPaused {
		t.Fatalf("rejudge while paused: %v", err)
	}
	if e.count(domain.DispositionUnjudged)+e.count(domain.DispositionExpired) != 2 || len(e.cls.Batches()) != 0 || len(e.stub.Requests()) != 0 {
		t.Fatal("a refused call changed the backlog or reached the model")
	}
	e.setPaused(false)
	if _, err := e.rejudge("", 0); err != nil {
		t.Fatalf("rejudge after resume: %v", err)
	}
}
