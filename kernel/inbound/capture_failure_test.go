package inbound

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
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

// 会话没有对应项目：不捕获，也不静默丢掉：不写来源事件，但记一条 no_project（内容不进账本）。
func TestCaptureWithoutProjectIsRecordedNotSilent(t *testing.T) {
	e := newEnv(t, true)
	const secret = "不应出现在失败记录里的正文"
	out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"text": secret, "path": "/nowhere/else", "host": "h"})
	if err != nil || out.Captured || out.Reason != ReasonNoProject {
		t.Fatalf("out %+v err %v", out, err)
	}
	if got := e.failures(); len(got) != 1 || got[0].Class != FailNoProject || got[0].Project != "" {
		t.Fatalf("failures %+v", got)
	}
	if len(e.events("capture:"+e.web)) != 0 || len(e.stub.Requests()) != 0 || len(e.allItems()) != 0 {
		t.Fatal("no project: no source event, model call or item")
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
