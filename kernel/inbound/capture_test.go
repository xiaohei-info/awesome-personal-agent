package inbound

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

func caller(level authz.Level, session string) command.Trusted {
	return command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "agent:x", Via: domain.ViaCLI, Session: session}, Level: level}
}

func (e *env) capture(id command.Identity, in map[string]any) (CaptureOut, error) {
	e.t.Helper()
	var out CaptureOut
	b, _ := jsonMarshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: id, Command: CmdCapture, Input: b})
	if err != nil {
		return out, err
	}
	if err := jsonUnmarshal(r.Value, &out); err != nil {
		e.t.Fatal(err)
	}
	return out, nil
}

func (e *env) setCapture(project string, on bool) {
	e.t.Helper()
	e.must("project.update", map[string]any{"id": project, "capture": on})
}

// waitDisposed 等捕获事件被入站流水线（异步）处置完。
func (e *env) waitDisposed(source string) domain.SourceEvent {
	e.t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if evs := e.events(source); len(evs) == 1 && evs[0].Disposition != domain.DispositionPending {
			return evs[0]
		}
	}
	e.t.Fatal("capture event was not processed")
	return domain.SourceEvent{}
}

// submit 提交一段增量并等内部来源事件处置完。
func (e *env) submit(text string) domain.SourceEvent {
	e.t.Helper()
	if out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": text}); err != nil || !out.Captured {
		e.t.Fatalf("out %+v err %v", out, err)
	}
	return e.waitDisposed("capture:" + e.web)
}

func (e *env) extractReqs() (n int) {
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, "抽取可以成为建议的候选事项") {
			n++
		}
	}
	return
}

// ACT-CAP-03：一段含两件事的对话先预筛（分类端点）再抽取（生成端点），得到该项目里两条 proposed 事项，来源事件记下两个事项 id。
func TestCaptureExtractsTwoCandidatesAfterPrescreen(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.90)
	e.draft(`{"candidates":[{"title":"把合同发给小王","kind":"task"},{"title":"周五订会议室","kind":"task"}]}`)
	ev := e.submit("cap-two 下周三前把合同发给小王；另外周五要订会议室")
	if ev.Disposition != domain.DispositionProposed || len(ev.Items) != 2 || ev.Item != ev.Items[0] {
		t.Fatalf("event %+v", ev)
	}
	for i, want := range []string{"把合同发给小王", "周五订会议室"} {
		if it := e.item(ev.Items[i]); it.Title != want || it.Phase != domain.PhaseProposed || it.Project != e.web {
			t.Fatalf("item %d %+v", i, it)
		}
	}
	if len(e.cls.Batches()) == 0 || e.extractReqs() != 1 || len(e.stub.Requests()) != 1 {
		t.Fatalf("prescreen batches %d, generate requests %d (extract %d)", len(e.cls.Batches()), len(e.stub.Requests()), e.extractReqs())
	}
	if !strings.Contains(e.sent(0), "把合同发给小王") {
		t.Fatal("conversation delta was not passed to the extractor as material")
	}
}

// ACT-CAP-04：预筛为否（低于下限 0.10）时生成端点零调用，事件忽略，不建建议。
func TestCapturePrescreenNoMeansNoGenerateCall(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.03)
	e.draft(`{"candidates":[{"title":"不该出现","kind":"task"}]}`)
	ev := e.submit("cap-chat 好的谢谢，今天天气不错")
	if ev.Disposition != domain.DispositionIgnored || len(e.cls.Batches()) == 0 || len(e.stub.Requests()) != 0 || len(e.allItems()) != 0 {
		t.Fatalf("event %+v generate %d items %d", ev, len(e.stub.Requests()), len(e.allItems()))
	}
}

// ACT-CAP-05：预筛落在下限之上（0.30）即抽取；生成端点只收到一次抽取，没有对预筛题的重答。
func TestCapturePrescreenAboveFloorExtracts(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.30)
	e.draft(`{"candidates":[{"title":"cap-maybe 回电话给房东","kind":"task"}]}`)
	ev := e.submit("cap-maybe 回头记得给房东回电话")
	if ev.Disposition != domain.DispositionProposed || len(ev.Items) != 1 || e.item(ev.Item).Title != "cap-maybe 回电话给房东" {
		t.Fatalf("event %+v", ev)
	}
	if len(e.stub.Requests()) != 1 || e.extractReqs() != 1 {
		t.Fatalf("generate requests %d extract %d", len(e.stub.Requests()), e.extractReqs())
	}
}

// ACT-CAP-06：抽取出零条候选：事件忽略，不建（空标题）建议，不停在 pending。
func TestCaptureZeroCandidatesCreatesNothing(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.90)
	e.draft(`{"candidates":[]}`)
	ev := e.submit("cap-zero 我们讨论了一下架构")
	if ev.Disposition != domain.DispositionIgnored || e.extractReqs() != 1 || len(e.allItems()) != 0 {
		t.Fatalf("event %+v items %d", ev, len(e.allItems()))
	}
}

// ACT-CAP-07：判断端点不可用时直接交生成型抽取，抽取照常发生。
func TestCaptureJudgeDownExtractsDirectly(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	e.draft(`{"candidates":[{"title":"cap-nocls 交报销单","kind":"task"}]}`)
	ev := e.submit("cap-nocls 明天交报销单")
	if ev.Disposition != domain.DispositionProposed || len(ev.Items) != 1 || e.item(ev.Item).Title != "cap-nocls 交报销单" || e.extractReqs() != 1 {
		t.Fatalf("event %+v", ev)
	}
}

// ACT-CAP-08：两个端点都不可用：增量记未判定（model_unavailable），不丢，也不进建议流。
func TestCaptureBothEndpointsDownIsUnjudged(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	e.stub.SetDefault(modelstub.Reply{Status: 500})
	ev := e.submit("cap-down 下周一体检")
	if ev.Disposition != domain.DispositionUnjudged || ev.UnjudgedReason != domain.UnjudgedModelUnavailable || len(e.allItems()) != 0 {
		t.Fatalf("event %+v", ev)
	}
}

// ACT-CAP-01：捕获默认开启；被直接调 API 关掉的项目不入账、不调模型，原因只写机制，并记一条失败类别（不含内容）。
func TestCaptureDisabledProjectDoesNothing(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, false)
	out, err := e.capture(caller(authz.LevelWorker, ""), map[string]any{"project": e.web, "text": "随便聊聊"})
	if err != nil || out.Captured || out.Reason != ReasonCaptureDisabled {
		t.Fatalf("out %+v err %v", out, err)
	}
	if len(e.events("capture:"+e.web)) != 0 || len(e.stub.Requests()) != 0 || len(e.allItems()) != 0 {
		t.Fatal("a disabled project must leave no source event, model call or item")
	}
	if got := e.failures(); len(got) != 1 || got[0].Class != FailDisabled || got[0].Project != e.web {
		t.Fatalf("failures %+v", got)
	}
}

// ACT-CAP-10：运行身份下不生效，即使项目已开启捕获（防回环）。
func TestCaptureNoEffectUnderRunIdentity(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	out, err := e.capture(caller(authz.LevelRun, "s1"), map[string]any{"project": e.web, "text": "运行者的输出"})
	if err != nil || out.Captured || out.Reason != ReasonRunIdentity {
		t.Fatalf("out %+v err %v", out, err)
	}
	if len(e.events("capture:"+e.web)) != 0 || len(e.stub.Requests()) != 0 {
		t.Fatal("run identity must not capture")
	}
}

// observer 只读，不能提交增量；空白正文被拒。
func TestCaptureRejectsObserverAndBlankText(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	if _, err := e.capture(caller(authz.LevelObserver, ""), map[string]any{"project": e.web, "text": "x"}); err == nil {
		t.Fatal("observer must be denied")
	}
	if _, err := e.capture(caller(authz.LevelWorker, ""), map[string]any{"project": e.web, "text": "  "}); err == nil {
		t.Fatal("empty text must be rejected")
	}
}

// 决策 1：主会话兜底进 Inbox 的对话允许 triage-extract 识别具体业务项目，识别不出则落入 Inbox；
// 送给模型的 projects 清单不含 Inbox 自身；显式具体项目的捕获仍强锁定原项目。
func TestCaptureInboxExtractClassifiesConcreteProjectOrFallsBack(t *testing.T) {
	e := newEnv(t, true)
	inboxID, err := projects.EnsureInbox(bg, e.x, e.st, "owner")
	if err != nil {
		t.Fatal(err)
	}
	e.screen(0.95)
	e.draft(`{"candidates":[{"title":"修 web 登录超时","kind":"task","project":"` + e.web + `"},{"title":"周末买咖啡豆","kind":"task","project":""}]}`)
	out, err := e.capture(caller(authz.LevelWorker, "s-main"), map[string]any{"text": "帮我记两件事：修 web 登录超时，另外周末买咖啡豆", "path": "/Users/demo", "host": "h"})
	if err != nil || !out.Captured || out.Project != inboxID {
		t.Fatalf("out %+v err %v", out, err)
	}
	ev := e.waitDisposed("capture:" + inboxID)
	if ev.Disposition != domain.DispositionProposed || len(ev.Items) != 2 {
		t.Fatalf("event %+v", ev)
	}
	if got := e.item(ev.Items[0]).Project; got != e.web {
		t.Fatalf("first candidate should be classified to web (%s), got %q", e.web, got)
	}
	if got := e.item(ev.Items[1]).Project; got != inboxID {
		t.Fatalf("second candidate should fall back to inbox (%s), got %q", inboxID, got)
	}
	if strings.Contains(e.sent(0), `"id":"`+inboxID+`"`) {
		t.Fatalf("projects list sent to extractor should exclude Inbox (%s): %s", inboxID, e.sent(0))
	}
}

// 决策 4：sweep 跨来源合并不再硬编码拦截 capture:*，本机模型允许进入 ask，外部模型遵循项目 capture_outbound。
func TestCaptureSweepFollowsOutboundAndLocality(t *testing.T) {
	e := newEnv(t, true)
	inboxID, err := projects.EnsureInbox(bg, e.x, e.st, "owner")
	if err != nil {
		t.Fatal(err)
	}
	e.screen(0.95)
	e.draft(`{"candidates":[{"title":"对话里的合同审阅","kind":"task"}]}`)
	out, err := e.capture(caller(authz.LevelWorker, "s-sweep"), map[string]any{"text": "合同审阅别忘了", "path": "/tmp/nowhere", "host": "h"})
	if err != nil || !out.Captured {
		t.Fatalf("capture: %+v %v", out, err)
	}
	ev := e.waitDisposed("capture:" + inboxID)
	capItem := e.item(ev.Item)
	// 放一条进行中的已有事项作候选，避免“批内仅 1 条且无可选事项”被规则直接跳过
	var openRef struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "phase": "open", "title": "合同审阅", "project": e.web}), &openRef)
	all := []domain.Item{capItem, e.item(openRef.ID)}

	var pol policy.Policy
	_ = e.st.View(bg, func(tx *store.Tx) error {
		_, pol, err = policy.Current(tx)
		return err
	})
	pol.GenerateEndpoint.Locality = policy.LocalityLocal
	plan, err := e.svc.planBatch(bg, pol, all, map[string]domain.Source{}, []domain.Item{capItem})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ask) != 1 || len(plan.settle) != 0 {
		t.Fatalf("local endpoint should include capture item in sweep ask: ask=%d settle=%d", len(plan.ask), len(plan.settle))
	}

	// 外部端点且 Inbox capture_outbound 未选：保守留着（settle）。
	pol.GenerateEndpoint.Locality = policy.LocalityExternal
	plan, err = e.svc.planBatch(bg, pol, all, map[string]domain.Source{}, []domain.Item{capItem})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ask) != 0 || len(plan.settle) != 1 {
		t.Fatalf("external endpoint with unset capture_outbound should settle capture item: ask=%d settle=%d", len(plan.ask), len(plan.settle))
	}

	// 外部端点且 Inbox 选了 capture_outbound=full：允许进 ask。
	e.must(projects.CmdSetCaptureOutbound, map[string]any{"id": inboxID, "value": "full"})
	plan, err = e.svc.planBatch(bg, pol, all, map[string]domain.Source{}, []domain.Item{capItem})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ask) != 1 || len(plan.settle) != 0 {
		t.Fatalf("external endpoint with full capture_outbound should include capture item: ask=%d settle=%d", len(plan.ask), len(plan.settle))
	}
}

// ACT-CAP-09：同一会话重复提交同一段增量只落一条来源事件、只建一个事项。
func TestCaptureSameDeltaIsIdempotent(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.95)
	e.draft(`{"candidates":[{"title":"t","kind":"task"}]}`)
	in := map[string]any{"project": e.web, "text": "用户：把发布流程写进文档"}
	for range 2 {
		if out, err := e.capture(caller(authz.LevelWorker, "s1"), in); err != nil || !out.Captured {
			t.Fatalf("out %+v err %v", out, err)
		}
	}
	e.waitDisposed("capture:" + e.web)
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events("capture:" + e.web)); n != 1 || len(e.allItems()) != 1 || e.extractReqs() != 1 {
		t.Fatalf("events %d items %d extract requests %d", n, len(e.allItems()), e.extractReqs())
	}
}

func jsonMarshal(v any) ([]byte, error)   { return json.Marshal(v) }
func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// §14.5 失败可见：捕获被内核拒绝（退出码 2、4）时记 health.alert，对象为命令名（捕获不对应事项）。
func TestRejectedCaptureRecordsHealthAlert(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	if _, err := e.capture(caller(authz.LevelObserver, "s1"), map[string]any{"project": e.web, "text": "x"}); err == nil {
		t.Fatal("observer capture must be rejected")
	}
	if _, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "  "}); err == nil {
		t.Fatal("blank capture must be rejected")
	}
	var alerts []domain.Event
	err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvHealthAlert}, 0, 100)
		for _, en := range es {
			alerts = append(alerts, en.Event)
		}
		return err
	})
	if err != nil || len(alerts) != 2 || alerts[0].Object != CmdCapture {
		t.Fatalf("alerts %+v err %v", alerts, err)
	}
	var p struct {
		Kind string
		Exit int
	}
	_ = json.Unmarshal(alerts[0].Payload, &p)
	if p.Kind != command.ReceiptKind || p.Exit != domain.ExitPermissionDenied {
		t.Fatalf("payload %+v", p)
	}
}
