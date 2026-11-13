package inbound

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

const releaseDraft = `{"title":"起草标题","summary":"起草摘要","kind":"task","recommend":"accept","reason":"值得看"}`

// newReleaseEnv 造一批合成积压：high 条 [FX-H]（分类端点 0.90）、mid 条 [FX-M]（0.40，带内，对话端点答否）、low 条 [FX-L]（0.05），
// 落盘顺序里三种交错；处置在 unjudged、expired 与旧原因之间轮换。端点在事件落盘之后才配（否则入站会先去问替身）。
func newReleaseEnv(t *testing.T, perDay, high, mid, low int) *scoreEnv {
	t.Helper()
	e := newScoreEnv(t)
	e.svc.o.Now = e.Now
	e.svc.o.BacklogReleasePerDay = perDay
	var evs []map[string]any
	kinds := map[string]int{"H": high, "M": mid, "L": low}
	n := 0
	for len(evs) < high+mid+low {
		for _, k := range []string{"H", "M", "L"} {
			if kinds[k] > 0 {
				kinds[k]--
				evs = append(evs, ev(fmt.Sprintf("e%03d", n), "ticket.other", fmt.Sprintf("[FX-%s] 积压 %d", k, n), nil))
				n++
			}
		}
	}
	e.emit("src_t", "plain", evs...)
	for i := range evs {
		disp, why := domain.DispositionUnjudged, domain.UnjudgedModelUnavailable
		switch i % 3 {
		case 1:
			disp, why = domain.DispositionExpired, ""
		case 2:
			why = domain.UnjudgedUndecided
		}
		e.backlogged("src_t", "ticket", fmt.Sprintf("e%03d", i), disp, why)
	}
	e.configure(true)
	yes := true
	e.cls.PYes(modelstub.Key{Contains: "[FX-H]"}, 0.90)
	e.cls.PYes(modelstub.Key{Contains: "[FX-M]"}, 0.40)
	e.cls.PYes(modelstub.Key{Contains: "[FX-L]"}, 0.05)
	e.stub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "[FX-M]"}, Answer: "no", Sufficient: &yes})
	e.draft(releaseDraft)
	return e
}

func (e *scoreEnv) rejudge(source string, limit int) (ReleaseResult, error) {
	return e.svc.BacklogRejudge(bg, command.Request{Identity: owner, Evidence: "本人：放开", Owner: "owner"}, source, limit)
}

func (e *scoreEnv) count(d domain.Disposition) (n int) {
	for _, x := range e.events("src_t") {
		if x.Disposition == d {
			n++
		}
	}
	return
}

// proposedSubjects 是已进入建议流的来源事件的标题。
func (e *scoreEnv) proposedSubjects() (out []string) {
	for _, x := range e.events("src_t") {
		if x.Disposition == domain.DispositionProposed {
			out = append(out, x.Subject)
		}
	}
	return
}

func (e *scoreEnv) mustRejudge(limit int) ReleaseResult {
	e.t.Helper()
	res, err := e.rejudge("", limit)
	if err != nil {
		e.t.Fatal(err)
	}
	return res
}

// requeuedBy 返回某个来源事件的 inbound.requeued 的操作者。
func (e *scoreEnv) requeuedBy(event string) (who []string) {
	e.view(func(tx *store.Tx) {
		es, err := eventlog.ReadObject(tx, event)
		if err != nil {
			e.t.Fatal(err)
		}
		for _, y := range es {
			if y.Event.Type == EvRequeued {
				who = append(who, y.Event.Actor.Principal)
			}
		}
	})
	return
}

// ACT-BKL-09、10、11：按分数从高到低，每天进入建议流的数量不超过上限；当天用完只推迟；次日接着放，--limit 生效。
func TestRejudgeFollowsScoreAndDailyCap(t *testing.T) {
	e := newReleaseEnv(t, 5, 12, 6, 6)
	if _, err := e.score(owner, "本人：打分", ""); err != nil {
		t.Fatal(err)
	}
	scoreBatches := len(e.cls.Batches())

	res := e.mustRejudge(0)
	if res != (ReleaseResult{Requeued: 5, Deferred: 19}) {
		t.Fatalf("first release %+v", res)
	}
	for _, b := range e.cls.Batches()[scoreBatches:] {
		if b.Questions > 32 {
			t.Fatalf("batch of %d questions", b.Questions)
		}
	}
	subjects := e.proposedSubjects()
	if len(subjects) != 5 || len(e.allItems()) != 5 {
		t.Fatalf("proposed %v, items %d", subjects, len(e.allItems()))
	}
	for _, s := range subjects {
		if !strings.Contains(s, "[FX-H]") {
			t.Fatalf("a lower score went first: %v", subjects)
		}
	}
	for _, it := range e.allItems() {
		if it.Title != "起草标题" || it.Proposal == nil {
			t.Fatalf("item not drafted: %+v", it)
		}
	}
	for _, x := range e.events("src_t") {
		if x.Disposition == domain.DispositionProposed {
			if who := e.requeuedBy(x.ID); len(who) != 1 || who[0] != "owner" {
				t.Fatalf("%s requeued by %v", x.ID, who)
			}
		}
	}

	backlog := func() int { return e.count(domain.DispositionUnjudged) + e.count(domain.DispositionExpired) }
	before := backlog()
	for _, limit := range []int{0, 3} { // 当天额度用完：只推迟，不放行
		res = e.mustRejudge(limit)
		if res != (ReleaseResult{Deferred: before}) || backlog() != before || len(e.allItems()) != 5 {
			t.Fatalf("same day, limit %d: %+v backlog %d items %d", limit, res, backlog(), len(e.allItems()))
		}
	}

	e.now = e.now.Add(24 * time.Hour) // 次日
	if res = e.mustRejudge(3); res.Requeued != 3 || len(e.allItems()) != 8 {
		t.Fatalf("next day limit 3: %+v items %d", res, len(e.allItems()))
	}
	if res = e.mustRejudge(0); res.Requeued != 2 || len(e.allItems()) != 10 {
		t.Fatalf("next day rest of the cap: %+v items %d", res, len(e.allItems()))
	}
	for _, s := range e.proposedSubjects() {
		if !strings.Contains(s, "[FX-H]") {
			t.Fatalf("10 highest scores are all [FX-H], got %s", s)
		}
	}

	// 逐日放完：积压清零，含 expired 与旧原因；每个事件被放回的次数等于它的重判次数。
	for day := 0; day < 10 && backlog() > 0; day++ {
		e.now = e.now.Add(24 * time.Hour)
		e.mustRejudge(0)
	}
	if backlog() != 0 {
		t.Fatalf("backlog left: %d", backlog())
	}
	if e.count(domain.DispositionProposed) != 12 || e.count(domain.DispositionIgnored) != 12 {
		t.Fatalf("proposed %d ignored %d", e.count(domain.DispositionProposed), e.count(domain.DispositionIgnored))
	}
	for _, x := range e.events("src_t") {
		if who := e.requeuedBy(x.ID); len(who) != x.Rejudges || x.Rejudges != 1 {
			t.Fatalf("%s: requeued %v, rejudges %d", x.ID, who, x.Rejudges)
		}
	}
}

// ACT-BKL-08、13：被忽略的不占额度；[FX-L] 被分类端点直接按否定案，[FX-M] 在带内由对话端点定案为否，各有一条记录。
func TestRejudgeIgnoredEventsDoNotUseTheCap(t *testing.T) {
	e := newReleaseEnv(t, 2, 0, 6, 4)
	if _, err := e.score(owner, "本人：打分", ""); err != nil {
		t.Fatal(err)
	}
	res := e.mustRejudge(0)
	if res != (ReleaseResult{Requeued: 10}) || e.count(domain.DispositionIgnored) != 10 || len(e.allItems()) != 0 {
		t.Fatalf("%+v ignored %d items %d", res, e.count(domain.DispositionIgnored), len(e.allItems()))
	}
	for _, x := range e.events("src_t") {
		var final *record.Record
		for _, r := range e.records(x.ID) {
			if !r.Shadow {
				r := r
				final = &r
			}
		}
		if final == nil || final.Answer != "no" {
			t.Fatalf("%s: no screening record: %+v", x.ID, e.records(x.ID))
		}
		switch {
		case strings.Contains(x.Subject, "[FX-M]"):
			if final.DecidedBy != record.DecidedChat || len(final.Levels) != 2 {
				t.Fatalf("%s: %+v", x.ID, final)
			}
		default:
			if final.DecidedBy != record.DecidedClassifier || len(final.Levels) != 1 {
				t.Fatalf("%s: %+v", x.ID, final)
			}
		}
	}
}

// ACT-BKL-09：没有先打分时，初筛题按批提交（每批不超过 max_batch），每题只问一次；流水线逐条作答时复用批里的结果。
func TestRejudgeSubmitsScreeningInBatches(t *testing.T) {
	e := newReleaseEnv(t, 0, 40, 0, 0)
	res := e.mustRejudge(0)
	if res != (ReleaseResult{Requeued: 40}) || e.count(domain.DispositionProposed) != 40 {
		t.Fatalf("%+v proposed %d", res, e.count(domain.DispositionProposed))
	}
	total := 0
	for _, b := range e.cls.Batches() {
		if b.Questions > 32 {
			t.Fatalf("batch of %d questions", b.Questions)
		}
		total += b.Questions
	}
	if total != 40 || len(e.cls.Batches()) != 2 {
		t.Fatalf("classifier saw %d questions in %d batches, want 40 in 2", total, len(e.cls.Batches()))
	}
}

// ACT-BKL-17：没有判断端点时按落盘顺序，由对话端点一题一次地回答；答否的不占额度；不碰分类端点。
func TestRejudgeWithoutJudgeEndpointUsesChatInDiskOrder(t *testing.T) {
	e := newReleaseEnv(t, 3, 6, 0, 0)
	no := true
	e.stub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "积压 1"}, Answer: "no", Sufficient: &no})
	e.stub.Judge(modelstub.ChatJudge{Answer: "yes"})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error { p.JudgeEndpoint = nil; return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res := e.mustRejudge(0)
	if res != (ReleaseResult{Requeued: 4, Deferred: 2}) {
		t.Fatalf("%+v", res)
	}
	if len(e.cls.Batches()) != 0 {
		t.Fatal("classifier was called")
	}
	var subjects []string
	for _, x := range e.events("src_t") {
		if x.Disposition == domain.DispositionProposed {
			subjects = append(subjects, x.Subject)
		}
	}
	// 落盘顺序是 e000..e005；e001 答否被忽略，不占额度，所以 e000、e002、e003 进入建议流。
	want := []string{"[FX-H] 积压 0", "[FX-H] 积压 2", "[FX-H] 积压 3"}
	if strings.Join(subjects, "|") != strings.Join(want, "|") {
		t.Fatalf("proposed %v, want %v", subjects, want)
	}
	if got := e.byID("src_t")["e001"].Disposition; got != domain.DispositionIgnored {
		t.Fatalf("e001 %s", got)
	}
}

// ACT-BKL-12：放回后中途崩溃（事件停在 pending）：再次放开先把它们处理完并算进当天额度，不重复建议、不多记 inbound.requeued。
func TestRejudgeResumesAfterCrash(t *testing.T) {
	e := newReleaseEnv(t, 4, 6, 0, 0)
	// 模拟崩溃：两条已放回（记了 inbound.requeued），流水线还没处理。
	crashed := []string{"e000", "e001"}
	for _, id := range crashed {
		x := e.byID("src_t")[id]
		in, _ := json.Marshal(releaseIn{Event: x.ID, Attempt: x.Rejudges + 1})
		if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: CmdBacklogRejudge, Input: in, Evidence: "本人：放开", Owner: "owner",
			IdemKey: fmt.Sprintf("inbound:rejudge:%s:%d", x.ID, x.Rejudges+1)}); err != nil {
			t.Fatal(err)
		}
		if got := e.byID("src_t")[id].Disposition; got != domain.DispositionPending {
			t.Fatalf("%s should be pending, is %s", id, got)
		}
	}
	res := e.mustRejudge(0)
	if res.Requeued != 2 || res.Deferred != 2 {
		t.Fatalf("%+v: the 2 pending ones count against the cap of 4, leaving 2 more", res)
	}
	if e.count(domain.DispositionProposed) != 4 || e.count(domain.DispositionPending) != 0 || len(e.allItems()) != 4 {
		t.Fatalf("proposed %d pending %d items %d", e.count(domain.DispositionProposed), e.count(domain.DispositionPending), len(e.allItems()))
	}
	for _, x := range e.events("src_t") {
		if x.Disposition == domain.DispositionProposed {
			if who := e.requeuedBy(x.ID); len(who) != 1 {
				t.Fatalf("%s: %d requeued events", x.ID, len(who))
			}
		}
	}
	// 同一个放回的幂等键重放不会再记一条。
	x := e.byID("src_t")["e000"]
	in, _ := json.Marshal(releaseIn{Event: x.ID, Attempt: 1})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: CmdBacklogRejudge, Input: in, Evidence: "本人：放开", Owner: "owner",
		IdemKey: fmt.Sprintf("inbound:rejudge:%s:%d", x.ID, 1)}); err != nil || len(e.requeuedBy(x.ID)) != 1 {
		t.Fatalf("replay: %v %v", err, e.requeuedBy(x.ID))
	}
}

// ACT-BKL-19：来源没有外发范围而端点在外部：重判把事件记为 outbound_unset，不送出任何内容，没有新建议。
func TestRejudgeRecordsOutboundUnsetWithoutSending(t *testing.T) {
	e := newReleaseEnv(t, 5, 3, 0, 0)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error {
			p.JudgeEndpoint.Locality, p.GenerateEndpoint.Locality = policy.LocalityExternal, policy.LocalityExternal
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res := e.mustRejudge(0)
	if res.Requeued != 3 || len(e.allItems()) != 0 {
		t.Fatalf("%+v items %d", res, len(e.allItems()))
	}
	for _, x := range e.events("src_t") {
		if x.Disposition != domain.DispositionUnjudged || x.UnjudgedReason != domain.UnjudgedOutboundUnset {
			t.Fatalf("%s: %s/%s", x.ID, x.Disposition, x.UnjudgedReason)
		}
	}
	for _, b := range e.cls.Batches() {
		t.Fatalf("classifier received %s", b.BodyText)
	}
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, "积压") {
			t.Fatalf("chat endpoint received event content: %s", r.BodyText)
		}
	}
}

// 模型不可用：把真实错误报出来，不继续放，已放回的事件仍在积压里（原因 model_unavailable），不进建议流。
func TestRejudgeStopsWhenModelUnavailable(t *testing.T) {
	e := newReleaseEnv(t, 5, 4, 0, 0)
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	e.stub.SetDefault(modelstub.Reply{Status: 503})
	res, err := e.rejudge("", 0)
	if err == nil || code(err) != domain.CodeConflict {
		t.Fatalf("want a conflict error, got %v", err)
	}
	if res.Requeued == 0 || len(e.allItems()) != 0 {
		t.Fatalf("%+v items %d", res, len(e.allItems()))
	}
	if res.Requeued+res.Deferred != 4 {
		t.Fatalf("every event must be either requeued or deferred: %+v", res)
	}
}

// 授权：缺本人原话、负的 limit 被拒，什么也没发生。
func TestRejudgeRefusals(t *testing.T) {
	e := newReleaseEnv(t, 5, 2, 0, 0)
	if _, err := e.svc.BacklogRejudge(bg, command.Request{Identity: delegate, Owner: "owner"}, "", 0); code(err) != domain.CodeBasisRequired {
		t.Fatalf("without words: %v", err)
	}
	if _, err := e.rejudge("", -1); code(err) != domain.CodeInvalidRequest {
		t.Fatalf("negative limit: %v", err)
	}
	if e.count(domain.DispositionUnjudged)+e.count(domain.DispositionExpired) != 2 || len(e.cls.Batches()) != 0 {
		t.Fatal("a refused call changed the backlog or reached the model")
	}
}

// 限定来源：只放开该来源的积压。
func TestRejudgeScopedToSource(t *testing.T) {
	e := newReleaseEnv(t, 0, 2, 0, 0)
	e.emit("src_u", "plain", ev("u1", "ticket.other", "[FX-H] 别的来源", nil))
	e.backlogged("src_u", "ticket", "u1", domain.DispositionUnjudged, "")
	res, err := e.rejudge("src_t", 0)
	if err != nil || res.Requeued != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := e.events("src_u")[0].Disposition; got != domain.DispositionUnjudged {
		t.Fatalf("other source touched: %s", got)
	}
}
