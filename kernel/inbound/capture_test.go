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
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
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

// waitDisposed 等捕获事件被入站流水线（异步）判断/入队或处置完。
func (e *env) waitDisposed(source string) domain.SourceEvent {
	e.t.Helper()
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if evs := e.events(source); len(evs) == 1 && (evs[0].Disposition != domain.DispositionPending || evs[0].Queue != "") {
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
		if strings.Contains(r.BodyText, "确需本人后续处理或拍板的候选事项") {
			n++
		}
	}
	return
}

// ACT-CAP-03：对话捕获按普通来源走统一 judge（两题）并进队列，不调用生成端点抽取。
func TestCaptureJudgesAndEnqueuesLikeNormalSource(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.90)
	ev := e.submit("cap-two 下周三前把合同发给小王；另外周五要订会议室")
	if ev.Disposition != domain.DispositionPending || ev.Queue != runner.QueueQueued || ev.Judgment == "" {
		t.Fatalf("event %+v", ev)
	}
	if len(e.cls.Batches()) == 0 || len(e.stub.Requests()) != 0 {
		t.Fatalf("judge batches %d, generate requests %d", len(e.cls.Batches()), len(e.stub.Requests()))
	}
	e.drainBatch()
	ev = e.events("capture:" + e.web)[0]
	if ev.Disposition != domain.DispositionProposed || ev.Item == "" || e.item(ev.Item).Project != e.web {
		t.Fatalf("drained event %+v", ev)
	}
}

// ACT-CAP-04：judge 判不值得时生成端点零调用，事件忽略，不入队、不建建议。
func TestCapturePrescreenNoMeansNoGenerateCall(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.03)
	ev := e.submit("cap-chat 好的谢谢，今天天气不错")
	if ev.Disposition != domain.DispositionIgnored || ev.Queue != "" || len(e.cls.Batches()) == 0 || len(e.stub.Requests()) != 0 || len(e.allItems()) != 0 {
		t.Fatalf("event %+v generate %d items %d", ev, len(e.stub.Requests()), len(e.allItems()))
	}
}

// ACT-CAP-05：judge 不确定（0.30）按保守策略进普通队列（不漏办、不升级成紧急）。
func TestCaptureUncertainJudgeEnqueuesNormal(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.30)
	ev := e.submit("cap-maybe 回头记得给房东回电话")
	if ev.Disposition != domain.DispositionPending || ev.Queue != runner.QueueQueued || ev.QueueLevel != runner.LevelNormal {
		t.Fatalf("event %+v", ev)
	}
}

// ACT-CAP-07：判断端点不可用时，由对话补全端点按概率判断退路作答并入队。
func TestCaptureJudgeDownFallsBackToChatJudge(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	e.stub.Judge(modelstub.ChatJudge{Answer: "yes"})
	ev := e.submit("cap-nocls 明天交报销单")
	if ev.Disposition != domain.DispositionPending || ev.Queue != runner.QueueQueued || len(e.stub.Requests()) == 0 {
		t.Fatalf("event %+v requests %d", ev, len(e.stub.Requests()))
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

// ACT-CAP-09：同一会话重复提交同一段增量只落一条来源事件、只入队一次。
func TestCaptureSameDeltaIsIdempotent(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.95)
	in := map[string]any{"project": e.web, "text": "用户：把发布流程写进文档"}
	for range 2 {
		if out, err := e.capture(caller(authz.LevelWorker, "s1"), in); err != nil || !out.Captured {
			t.Fatalf("out %+v err %v", out, err)
		}
	}
	ev := e.waitDisposed("capture:" + e.web)
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	if n := len(e.events("capture:" + e.web)); n != 1 || ev.Queue != runner.QueueQueued || len(e.cls.Batches()) != 1 {
		t.Fatalf("events %d queue %q batches %d", n, ev.Queue, len(e.cls.Batches()))
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

// 来源标注（§9.3）：外部人发起的轮次放行，等它走完流水线；审计载荷里带上这次的标注。
func TestCaptureAdmitsExternalHumanTurn(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.90)
	e.draft(`{"candidates":[{"title":"cap-human 交季度报表","kind":"task"}]}`)
	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "cap-human 下周一交季度报表",
		"capture_schema": 1, "speaker_role": "human", "origin_type": "external_chat"})
	if err != nil || !out.Captured || out.Event == "" {
		t.Fatalf("out %+v err %v", out, err)
	}
	var got map[string]any
	for _, en := range e.eventsOf(out.Event) {
		if en.Type == EvCaptured {
			if err := json.Unmarshal(en.Payload, &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got["capture_schema"] != float64(CaptureSchemaRoles) || got["speaker_role"] != SpeakerHuman || got["origin_type"] != OriginExternalChat || got["legacy"] != false {
		t.Fatalf("audit payload %+v", got)
	}
}

// 旧调用方没有标注：本版照旧放行，审计里记 legacy 便于观测。
func TestCaptureLegacyRequestIsAdmittedAndAuditedAsLegacy(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.screen(0.90)
	e.draft(`{"candidates":[{"title":"cap-legacy 续签护照","kind":"task"}]}`)
	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "cap-legacy 记得续签护照"})
	if err != nil || !out.Captured {
		t.Fatalf("out %+v err %v", out, err)
	}
	var got map[string]any
	for _, en := range e.eventsOf(out.Event) {
		if en.Type == EvCaptured {
			if err := json.Unmarshal(en.Payload, &got); err != nil {
				t.Fatal(err)
			}
		}
	}
	if got["legacy"] != true || got["speaker_role"] != "" || got["origin_type"] != "" {
		t.Fatalf("audit payload %+v", got)
	}
}

// 非外部人发起的轮次（助手输出、系统通知、定时任务回声、后台内部会话）一律不入账：不建来源事件、不记失败、不调模型。
func TestCaptureFiltersNonExternalOrigins(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	for _, c := range []struct {
		speaker, origin, status, reason string
	}{
		{SpeakerAssistant, OriginAssistantOutput, "", ReasonNonExternalOrigin},
		{SpeakerAssistant, OriginExternalChat, "", ReasonNonExternalOrigin},
		{SpeakerSystem, OriginSystemNotice, "", ReasonNonExternalOrigin},
		{SpeakerSystem, OriginCronEcho, "", ReasonNonExternalOrigin},
		{SpeakerSystem, OriginBackgroundInternal, "", ReasonNonExternalOrigin},
		{SpeakerSystem, OriginSystemNotice, StatusOpen, ReasonNonExternalOrigin},
		{SpeakerHuman, OriginExternalChat, StatusResolved, ReasonAlreadyResolved},
	} {
		in := map[string]any{"project": e.web, "text": "cap-filter " + c.origin, "capture_schema": 1,
			"speaker_role": c.speaker, "origin_type": c.origin}
		if c.status != "" {
			in["status_hint"] = c.status
		}
		out, err := e.capture(caller(authz.LevelWorker, "s1"), in)
		if err != nil || out.Captured || out.Reason != c.reason || out.Project != e.web {
			t.Fatalf("%s/%s/%s: out %+v err %v", c.speaker, c.origin, c.status, out, err)
		}
	}
	if n := len(e.events("capture:" + e.web)); n != 0 {
		t.Fatalf("filtered turns must leave no source event, got %d", n)
	}
	if n := len(e.failures()); n != 0 {
		t.Fatalf("filtered turns are expected, not failures: %+v", e.failures())
	}
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("filtered turns must not call the model, got %d", n)
	}
}

// 标注本身不合法（未知的契约版本、缺字段、不在枚举里的取值）是请求错误：不猜、不静默放行。
func TestCaptureRejectsMalformedAnnotations(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	ok := func(m map[string]any) map[string]any {
		m["project"], m["text"] = e.web, "cap-bad"
		return m
	}
	for _, in := range []map[string]any{
		ok(map[string]any{"capture_schema": 2, "speaker_role": SpeakerHuman, "origin_type": OriginExternalChat}),
		ok(map[string]any{"capture_schema": 1, "origin_type": OriginExternalChat}),
		ok(map[string]any{"capture_schema": 1, "speaker_role": SpeakerHuman}),
		ok(map[string]any{"capture_schema": 1, "speaker_role": "robot", "origin_type": OriginExternalChat}),
		ok(map[string]any{"capture_schema": 1, "speaker_role": SpeakerHuman, "origin_type": "webhook"}),
		ok(map[string]any{"capture_schema": 1, "speaker_role": SpeakerHuman, "origin_type": OriginExternalChat, "status_hint": "done"}),
		ok(map[string]any{"speaker_role": SpeakerHuman}), // 只带一个标注字段也按标注校验
	} {
		if _, err := e.capture(caller(authz.LevelWorker, "s1"), in); err == nil {
			t.Fatalf("%v must be rejected", in)
		}
	}
	if n := len(e.events("capture:" + e.web)); n != 0 {
		t.Fatalf("rejected requests must not capture, got %d events", n)
	}
}
