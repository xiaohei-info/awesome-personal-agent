package inbound

import (
	"fmt"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/judgment/record"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

// scoreEnv 在 env 之上加影子打分用的辅助：env 已带分类端点替身（cls）与带分类端点的判断入口。
type scoreEnv struct{ *env }

// newScoreEnv 此时还没有端点：事件先落成积压，再由 configure 配端点（否则入站流水线会先去问替身）。
func newScoreEnv(t *testing.T) *scoreEnv {
	t.Helper()
	return &scoreEnv{env: newEnv(t, false)}
}

// configure 配上对话端点（影子打分不得碰它）；judgeEndpoint 为假时再撤掉分类端点。
func (e *scoreEnv) configure(judgeEndpoint bool) {
	e.t.Helper()
	e.configureModel()
	if judgeEndpoint {
		return
	}
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error { p.JudgeEndpoint = nil; return nil })
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *scoreEnv) score(id command.Identity, evidence, source string) (ScoreResult, error) {
	return e.svc.BacklogScore(bg, command.Request{Identity: id, Evidence: evidence, Owner: "owner"}, source)
}

func (e *scoreEnv) records(event string) (out []record.Record) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		out, _, err = record.List(tx, record.Filter{Subject: event})
		return
	}); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *scoreEnv) disposed(source string) map[string]domain.Disposition {
	out := map[string]domain.Disposition{}
	for _, x := range e.events(source) {
		out[x.DedupeKey.EventID] = x.Disposition
	}
	return out
}

// 第 2 步：每条积压（含 expired 与旧原因）得一条带 shadow 的记录，概率是分类端点的回答，处置、事项、对话端点都不动；
// 分类端点每批不超过 32 题；之后列表按分数排序，没有分数的排最后。
func TestBacklogScoreWritesShadowRecordsAndSortsByScore(t *testing.T) {
	e := newScoreEnv(t)
	var evs []map[string]any
	for i := 0; i < 40; i++ {
		evs = append(evs, ev(fmt.Sprintf("e%02d", i), "ticket.other", fmt.Sprintf("[FX-%d] 积压", i), nil))
	}
	e.emit("src_t", "plain", evs...)
	e.emit("src_u", "plain", ev("u1", "ticket.other", "[FX-U] 别的来源", nil))
	for i := 0; i < 40; i++ {
		disp, why := domain.DispositionUnjudged, domain.UnjudgedModelUnavailable
		switch i % 3 {
		case 1:
			disp, why = domain.DispositionExpired, ""
		case 2:
			why = domain.UnjudgedUndecided
		}
		e.backlogged("src_t", "ticket", fmt.Sprintf("e%02d", i), disp, why)
	}
	e.backlogged("src_u", "ticket", "u1", domain.DispositionUnjudged, "")
	e.configure(true)
	e.cls.PYes(modelstub.Key{Contains: "[FX-1]"}, 0.90)
	e.cls.PYes(modelstub.Key{Contains: "[FX-2]"}, 0.05)
	e.cls.SetDefaultPYes(0.40)
	before := e.disposed("src_t")

	res, err := e.score(owner, "", "src_t")
	if err != nil {
		t.Fatal(err)
	}
	if res != (ScoreResult{Scored: 40}) {
		t.Fatalf("result %+v", res)
	}
	if fmt.Sprint(e.disposed("src_t")) != fmt.Sprint(before) || e.disposed("src_u")["u1"] != domain.DispositionUnjudged || len(e.allItems()) != 0 {
		t.Fatal("scoring must not change any disposition or create items")
	}
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("chat endpoint called %d times", n)
	}
	total := 0
	for _, b := range e.cls.Batches() {
		if b.Questions > 32 {
			t.Fatalf("batch of %d questions", b.Questions)
		}
		total += b.Questions
	}
	if total != 40 {
		t.Fatalf("classifier saw %d questions, want 40 (other source must not be scored)", total)
	}
	var high, low string
	for _, x := range e.events("src_t") {
		rs := e.records(x.ID)
		if len(rs) != 1 || !rs[0].Shadow || len(rs[0].Levels) != 1 || rs[0].Levels[0].PYes == nil || rs[0].Subject != x.ID {
			t.Fatalf("%s: %+v", x.ID, rs)
		}
		switch x.Subject {
		case "[FX-1] 积压":
			high = x.ID
			if *rs[0].Levels[0].PYes != 0.90 {
				t.Fatalf("p_yes %v", *rs[0].Levels[0].PYes)
			}
		case "[FX-2] 积压":
			low = x.ID
		}
	}
	if u := e.events("src_u"); len(e.records(u[0].ID)) != 0 {
		t.Fatal("other source was scored")
	}

	var sorted []Scored
	all := append(e.events("src_t"), e.events("src_u")...)
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		sorted, err = SortByScore(tx, all)
		return
	}); err != nil {
		t.Fatal(err)
	}
	if len(sorted) != 41 || sorted[0].ID != high || *sorted[0].Score != 0.90 || sorted[39].ID != low || *sorted[39].Score != 0.05 ||
		sorted[40].Score != nil || sorted[40].SourceID != "src_u" {
		t.Fatalf("order: first %+v last %+v", sorted[0], sorted[40])
	}
	for i := 1; i < 39; i++ {
		if *sorted[i].Score != 0.40 {
			t.Fatalf("middle %d: %+v", i, sorted[i])
		}
	}
}

// 没有判断端点时被拒（409），不写记录、不外呼；缺本人原话、observer 同样被拒。
func TestBacklogScoreRefusals(t *testing.T) {
	e := newScoreEnv(t)
	e.emit("src_t", "plain", ev("a", "ticket.other", "x", nil))
	e.backlogged("src_t", "ticket", "a", domain.DispositionUnjudged, "")
	e.configure(false)
	if _, err := e.score(owner, "", ""); err == nil || code(err) != domain.CodeConflict {
		t.Fatalf("no judge endpoint: %v", err)
	}
	if rs := e.records(e.events("src_t")[0].ID); len(rs) != 0 || len(e.stub.Requests()) != 0 || len(e.cls.Batches()) != 0 {
		t.Fatalf("records %v, requests %d", rs, len(e.stub.Requests()))
	}

	e = newScoreEnv(t)
	e.emit("src_t", "plain", ev("a", "ticket.other", "x", nil))
	e.backlogged("src_t", "ticket", "a", domain.DispositionUnjudged, "")
	e.configure(true)
	deleg := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent", Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	if _, err := e.score(deleg, "", ""); err == nil || code(err) != domain.CodeBasisRequired {
		t.Fatalf("without words: %v", err)
	}
	if len(e.cls.Batches()) != 0 {
		t.Fatal("refused call reached the classifier")
	}
}

// 分类端点整批故障：把真实错误报出来，已有的积压原样，不写没有分数的记录。
func TestBacklogScoreSurfacesClassifierFailure(t *testing.T) {
	e := newScoreEnv(t)
	e.emit("src_t", "plain", ev("a", "ticket.other", "x", nil))
	e.backlogged("src_t", "ticket", "a", domain.DispositionUnjudged, "")
	e.configure(true)
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	res, err := e.score(owner, "", "")
	if err == nil || res.Scored != 0 || res.Skipped != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if rs := e.records(e.events("src_t")[0].ID); len(rs) != 0 {
		t.Fatalf("records %+v", rs)
	}
}

// 外发范围照常生效：判断端点是外部端点时，没选外部范围的来源整条跳过（不外呼），选了 meta 的来源只送元数据。
func TestBacklogScoreHonoursOutboundScope(t *testing.T) {
	e := newScoreEnv(t)
	e.emit("src_t", "plain", ev("a", "ticket.other", "SUBJ-UNSET", map[string]any{"detail": "BODY-UNSET"}))
	e.emit("src_u", "plain", ev("u", "ticket.other", "SUBJ-META", map[string]any{"detail": "BODY-META"}))
	e.backlogged("src_t", "ticket", "a", domain.DispositionUnjudged, "")
	e.backlogged("src_u", "ticket", "u", domain.DispositionUnjudged, "")
	e.outbound("src_u", "full", "meta")
	e.configure(true)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error { p.JudgeEndpoint.Locality = policy.LocalityExternal; return nil })
		return err
	}); err != nil {
		t.Fatal(err)
	}
	res, err := e.score(owner, "", "")
	if err != nil || res != (ScoreResult{Scored: 1, Skipped: 1}) {
		t.Fatalf("%+v %v", res, err)
	}
	var sent string
	for _, b := range e.cls.Batches() {
		sent += b.BodyText
	}
	if !strings.Contains(sent, "SUBJ-META") || strings.Contains(sent, "BODY-META") || strings.Contains(sent, "SUBJ-UNSET") || strings.Contains(sent, "BODY-UNSET") {
		t.Fatalf("classifier received: %s", sent)
	}
	if len(e.records(e.events("src_t")[0].ID)) != 0 {
		t.Fatal("the source without an external scope was scored")
	}
}
