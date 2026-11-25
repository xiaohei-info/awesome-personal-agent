package inbound

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

type captureFailure struct{ Class, Project, At string }

// failures 读账本里全部 inbound.capture_failed 的载荷，同时断言载荷里只有类别、项目与时间（不含内容与凭据）。
func (e *env) failures() (out []captureFailure) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: domain.EvCaptureFailed}, 0, 100)
		for _, en := range es {
			var m map[string]string
			if err := json.Unmarshal(en.Event.Payload, &m); err != nil {
				return err
			}
			for k := range m {
				if k != "class" && k != "project" && k != "at" {
					e.t.Fatalf("unexpected payload key %q", k)
				}
			}
			out = append(out, captureFailure{m["class"], m["project"], m["at"]})
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

// 默认开启：新建的项目不用任何设置就收下增量（不调 setCapture）。
func TestCaptureOnByDefaultForNewProject(t *testing.T) {
	e := newEnv(t, true)
	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "默认就收"})
	if err != nil || !out.Captured || out.Project != e.web {
		t.Fatalf("out %+v err %v", out, err)
	}
	if got := e.failures(); len(got) != 0 {
		t.Fatalf("a captured delta is not a failure: %+v", got)
	}
}

// 会话没有对应项目时兜底进入内置 Inbox；显式指定不存在的项目则直接报错 404，不静默兜底。
func TestCaptureWithoutProjectFallsBackToInbox(t *testing.T) {
	e := newEnv(t, true)
	inboxID, err := projects.EnsureInbox(bg, e.x, e.st, "owner")
	if err != nil {
		t.Fatal(err)
	}
	e.screen(0.03)
	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"text": "主会话杂项讨论", "path": "/nowhere/else", "host": "h"})
	if err != nil || !out.Captured || out.Project != inboxID {
		t.Fatalf("out %+v err %v", out, err)
	}
	e.waitDisposed("capture:" + inboxID)
	if got := e.failures(); len(got) != 0 {
		t.Fatalf("fallback to inbox is not a failure: %+v", got)
	}
	// 显式指定了不存在的项目：必须报 not_found，不能兜底到 Inbox。
	if _, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": "prj_missing", "text": "x"}); err == nil {
		t.Fatal("explicit unknown project must fail, not fall back to inbox")
	}
}

// 项目捕获被显式关闭：不写来源事件，记一条 capture_disabled（内容不进账本）。
func TestCaptureDisabledIsRecordedNotSilent(t *testing.T) {
	e := newEnv(t, true)
	e.setCapture(e.web, false)
	const secret = "不应出现在失败记录里的正文"
	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": secret})
	if err != nil || out.Captured || out.Reason != ReasonCaptureDisabled || out.Project != e.web {
		t.Fatalf("out %+v err %v", out, err)
	}
	if got := e.failures(); len(got) != 1 || got[0].Class != FailDisabled || got[0].Project != e.web {
		t.Fatalf("failures %+v", got)
	}
	if len(e.events("capture:"+e.web)) != 0 || len(e.stub.Requests()) != 0 || len(e.allItems()) != 0 {
		t.Fatal("disabled capture: no source event, model call or item")
	}
	var raw string
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, _ := eventlog.Query(tx, eventlog.Filter{}, 0, 1000)
		for _, en := range es {
			raw += string(en.Event.Payload) + en.Event.Reason
		}
		return nil
	})
	if strings.Contains(raw, secret) {
		t.Fatal("conversation text leaked into the ledger")
	}
}

// 运行身份下不生效且不记失败（那是设计，不是失败）。
func TestCaptureRunIdentityLeavesNoFailure(t *testing.T) {
	e := newEnv(t, true)
	if out, err := e.capture(caller(authz.LevelRun, "s1"), map[string]any{"project": e.web, "text": "x", "failures": []map[string]any{{"class": "kernel_unreachable"}}}); err != nil || out.Reason != ReasonRunIdentity {
		t.Fatalf("out %+v err %v", out, err)
	}
	if got := e.failures(); len(got) != 0 {
		t.Fatalf("%+v", got)
	}
}

// 插件补报自己没送达的失败：类别限定在固定集合（其余归 plugin_error），发生时间保留，晚于现在的按现在算；一次至多 20 条。
func TestCapturePluginReportedFailures(t *testing.T) {
	e := newEnv(t, true)
	ago := t0.Add(-2 * time.Hour).UTC() // 内核时钟是测试时钟 t0
	fs := []map[string]any{{"at": ago, "class": "kernel_unreachable"}, {"at": ago, "class": "made-up"}, {"at": t0.Add(48 * time.Hour), "class": "no_credentials"}}
	for i := 0; i < maxReportedFailures; i++ {
		fs = append(fs, map[string]any{"at": ago, "class": "kernel_refused"})
	}
	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "补报", "failures": fs})
	if err != nil || !out.Captured {
		t.Fatalf("out %+v err %v", out, err)
	}
	got := e.failures()
	if len(got) != maxReportedFailures {
		t.Fatalf("want %d failures, got %d", maxReportedFailures, len(got))
	}
	if got[0] != (captureFailure{FailUnreachable, "", ago.Format(time.RFC3339)}) || got[1] != (captureFailure{FailPlugin, "", ago.Format(time.RFC3339)}) {
		t.Fatalf("%+v", got[:2])
	}
	if got[2].Class != FailNoCredentials || got[2].At != t0.UTC().Format(time.RFC3339) {
		t.Fatalf("a future time must be clamped to now: %+v", got[2])
	}
}
