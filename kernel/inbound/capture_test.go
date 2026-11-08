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

// ACT-CAP-03：开启捕获的项目里，对话增量成为内部来源事件，由判断型/生成型抽出候选，进该项目的 proposed。
func TestCaptureProposesCandidateInProject(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.stub.When("起标题", modelstub.Reply{Content: `{"title":"补登录页重试","summary":"用户说要给登录页加重试"}`})
	e.stub.SetDefault(modelstub.Reply{Content: `{"choice":"task","confidence":0.5}`})

	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "用户：登录页挂了，记得加重试"})
	if err != nil || !out.Captured || out.Project != e.web {
		t.Fatalf("out %+v err %v", out, err)
	}
	ev := e.waitDisposed("capture:" + e.web)
	if ev.Disposition != domain.DispositionProposed || ev.Type != CaptureEvent || ev.Item == "" {
		t.Fatalf("event %+v", ev)
	}
	it := e.item(ev.Item)
	if it.Phase != domain.PhaseProposed || it.Project != e.web || it.Title != "补登录页重试" || it.Kind != "task" {
		t.Fatalf("item %+v", it)
	}
	if rs := e.stub.Requests(); len(rs) == 0 || !strings.Contains(rs[0].BodyText, "登录页挂了") {
		t.Fatal("conversation delta was not passed to the model as material")
	}
}

// 默认关闭：项目没开捕获时不入账、不调模型；原因只写机制。
func TestCaptureDisabledProjectDoesNothing(t *testing.T) {
	e := newEnv(t, true)
	out, err := e.capture(caller(authz.LevelWorker, ""), map[string]any{"project": e.web, "text": "随便聊聊"})
	if err != nil || out.Captured || out.Reason != ReasonCaptureDisabled {
		t.Fatalf("out %+v err %v", out, err)
	}
	if len(e.events("capture:"+e.web)) != 0 || len(e.stub.Requests()) != 0 || len(e.allItems()) != 0 {
		t.Fatal("a disabled project must leave no trace")
	}
}

// 运行身份下不生效，即使项目已开启捕获（防回环）。
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

// observer 只读，不能提交增量；没有项目作用域时不入账。
func TestCaptureRejectsObserverAndNoProject(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	if _, err := e.capture(caller(authz.LevelObserver, ""), map[string]any{"project": e.web, "text": "x"}); err == nil {
		t.Fatal("observer must be denied")
	}
	out, err := e.capture(caller(authz.LevelWorker, ""), map[string]any{"text": "没有项目"})
	if err != nil || out.Captured || out.Reason != ReasonNoProject {
		t.Fatalf("out %+v err %v", out, err)
	}
	if _, err := e.capture(caller(authz.LevelWorker, ""), map[string]any{"project": e.web, "text": "  "}); err == nil {
		t.Fatal("empty text must be rejected")
	}
}

// 同一会话重复提交同一段增量只落一条来源事件、只建一个事项。
func TestCaptureSameDeltaIsIdempotent(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, true)
	e.judge(`{"choice":"task","confidence":0.95}`)
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
	if n := len(e.events("capture:" + e.web)); n != 1 || len(e.allItems()) != 1 {
		t.Fatalf("events %d items %d", n, len(e.allItems()))
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
