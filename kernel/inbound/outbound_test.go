package inbound

import (
	"context"
	"encoding/json"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const obdSrc = "src_obd"

// obdEvent 是验收脚本 OBD_SRC 输出的事件：正文 2000 字符，开头 HEAD-MARK，第 600 字符处 MID-MARK，末尾 TAIL-MARK。
func obdEvent(id, subject string) map[string]any {
	body := []rune(strings.Repeat("字", 2000))
	copy(body, []rune("HEAD-MARK"))
	copy(body[600:], []rune("MID-MARK"))
	copy(body[2000-9:], []rune("TAIL-MARK"))
	return ev(id, "doc.updated", subject, map[string]any{"detail": string(body), "summary": "SUM-MARK",
		"sender": "alice@example.org", "link": "https://docs.example.org/path/secret-123"})
}

func (e *env) locality(loc string) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "locality", func(p *policy.Policy) error { p.GenerateEndpoint.Locality = loc; return nil })
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

// registerObd 登记 OBD_SRC 并设外发范围；external 为空串表示未选择。
func (e *env) registerObd(local, external string) {
	e.t.Helper()
	e.must(registry.CmdRegisterExec, map[string]any{"id": obdSrc, "type": "script", "source_type": "inbox",
		"command": filepath.Join(e.dir, obdSrc+".sh"), "schedule": "1m", "timeout": 30}) // emit 写这个脚本
	e.sources++
	e.outbound(obdSrc, local, external)
}

func (e *env) outbound(id, local, external string) {
	e.t.Helper()
	in := map[string]any{"id": id, "local": local, "external": nil}
	if external != "" {
		in["external"] = external
	}
	e.must(registry.CmdSetOutbound, in)
}

// sent 返回自 from 起模型端点收到的全部请求体。
func (e *env) sent(from int) string {
	var b strings.Builder
	for _, r := range e.stub.Requests()[from:] {
		b.WriteString(r.BodyText)
	}
	return b.String()
}

// 验收 ACT-OBD-03/04/05/07：外部端点只收到所选范围内的内容；本机端点按本机范围，不被外部范围裁剪。
func TestEndpointReceivesOnlyTheChosenScope(t *testing.T) {
	cases := []struct {
		name            string
		locality        string
		local, external string
		has, missing    []string
	}{
		{"external full", "external", "full", "full", []string{"HEAD-MARK", "MID-MARK", "TAIL-MARK", "SUM-MARK", "/path/secret-123"}, nil},
		{"external excerpt", "external", "full", "excerpt", []string{"SUM-MARK", "HEAD-MARK", "docs.example.org"}, []string{"MID-MARK", "TAIL-MARK", "/path/secret-123"}},
		{"external meta", "external", "full", "meta", []string{"doc.updated", "OBD 外发测试", "alice@example.org", "docs.example.org"},
			[]string{"/path/secret-123", "SUM-MARK", "HEAD-MARK", "MID-MARK", "TAIL-MARK"}},
		{"local endpoint takes the local scope", "local", "full", "meta", []string{"HEAD-MARK", "MID-MARK", "TAIL-MARK", "SUM-MARK"}, nil},
		{"local endpoint with local=meta", "local", "meta", "full", []string{"docs.example.org"}, []string{"HEAD-MARK", "MID-MARK", "SUM-MARK", "/path/secret-123"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, true)
			e.locality(c.locality)
			e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
			e.registerObd(c.local, c.external)
			e.emit(obdSrc, "inbox", obdEvent("o1", "OBD 外发测试"))
			if n := len(e.stub.Requests()); n == 0 {
				t.Fatal("the endpoint was never called")
			}
			got := e.sent(0)
			for _, w := range c.has {
				if !strings.Contains(got, w) {
					t.Errorf("%q missing from the request", w)
				}
			}
			for _, w := range c.missing {
				if strings.Contains(got, w) {
					t.Errorf("%q leaked to the endpoint", w)
				}
			}
			if evs := e.events(obdSrc); evs[0].Disposition != domain.DispositionProposed {
				t.Fatalf("event %+v", evs[0])
			}
		})
	}
}

// 验收 ACT-OBD-06：对所有可用端点的范围都是 none 时不外发，事件改走摘要出口（digest），不进建议流，也不忽略。
func TestAllEndpointsNoneRoutesToDigest(t *testing.T) {
	e := newEnv(t, true)
	e.locality("external")
	e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
	e.registerObd("full", "none")
	e.emit(obdSrc, "inbox", obdEvent("o4", "OBD-4 外发测试"))
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("endpoint received %d requests: %s", n, e.sent(0))
	}
	evs := e.events(obdSrc)
	if evs[0].Disposition != domain.DispositionDigest || evs[0].Item != "" || len(e.allItems()) != 0 {
		t.Fatalf("event %+v items %d", evs[0], len(e.allItems()))
	}
}

// 验收 ACT-OBD-12/14：外部范围未选择且端点在外部时记 outbound_unset、不外发；选定后一轮后台维护自动重判（system 操作），按所选范围裁剪。
func TestUnsetScopeHoldsBackThenRejudgesWhenChosen(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.locality("external")
	e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
	e.registerObd("full", "")
	e.emit(obdSrc, "inbox", obdEvent("o1", "OBD-RSS-1"))
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("content was sent while the scope was not chosen: %s", e.sent(0))
	}
	got := e.events(obdSrc)[0]
	if got.Disposition != domain.DispositionUnjudged || got.UnjudgedReason != domain.UnjudgedOutboundUnset || len(e.allItems()) != 0 {
		t.Fatalf("event %+v", got)
	}
	// 范围仍未选：维护一轮什么也不发
	if err := e.svc.Rejudge(bg); err != nil || len(e.stub.Requests()) != 0 {
		t.Fatalf("err=%v requests=%d", err, len(e.stub.Requests()))
	}
	if g := e.events(obdSrc)[0]; g.Rejudges != 0 || g.UnjudgedReason != domain.UnjudgedOutboundUnset {
		t.Fatalf("event requeued without a chosen scope: %+v", g)
	}

	e.outbound(obdSrc, "full", "excerpt")
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	got = e.events(obdSrc)[0]
	if got.Disposition != domain.DispositionProposed || got.Item == "" || got.Rejudges != 1 {
		t.Fatalf("event not rejudged into a proposal: %+v", got)
	}
	if body := e.sent(0); !strings.Contains(body, "HEAD-MARK") || strings.Contains(body, "MID-MARK") || strings.Contains(body, "TAIL-MARK") {
		t.Fatalf("rejudge was not trimmed to the excerpt: %s", body)
	}
	var requeued []domain.Event
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Query(tx, eventlog.Filter{Type: EvRequeued}, 0, 10)
		for _, en := range es {
			requeued = append(requeued, en.Event)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(requeued) != 1 || requeued[0].Actor.Type != domain.ActorSystem || requeued[0].Object != got.ID {
		t.Fatalf("requeue events: %+v", requeued)
	}
}

// 端点位置改成本机也会放开：未选择的外部范围只拦外部端点。
func TestUnsetExternalScopeDoesNotBlockLocalEndpoint(t *testing.T) {
	e := newEnv(t, true)
	e.locality("local")
	e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
	e.registerObd("full", "")
	e.emit(obdSrc, "inbox", obdEvent("o1", "OBD 外发测试"))
	if evs := e.events(obdSrc); evs[0].Disposition != domain.DispositionProposed {
		t.Fatalf("event %+v", evs[0])
	}
	if !strings.Contains(e.sent(0), "TAIL-MARK") {
		t.Fatal("local endpoint should get the full text")
	}
}

// 规则直接定案的事件不调模型，也就不受外发范围影响（未选择时规则照常工作）。
func TestRuleDecidedEventsAreNotHeldBack(t *testing.T) {
	e := newEnv(t, true)
	e.locality("external")
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "登录页 500", map[string]any{"link": "https://git.example/org/web/issues/7"}))
	evs := e.events("src_t")
	if len(evs) != 1 || evs[0].Disposition != domain.DispositionProposed || len(e.stub.Requests()) != 0 {
		t.Fatalf("event %+v requests %d", evs, len(e.stub.Requests()))
	}
}

func (e *env) setCaptureOutbound(project string, v any) {
	e.t.Helper()
	e.must("project.capture_outbound.set", map[string]any{"id": project, "value": v})
}

// 验收 ACT-OBD-16/17：对话捕获按项目的 capture_outbound 裁剪；未选择且端点在外部时对话不送。
func TestCaptureFollowsProjectOutbound(t *testing.T) {
	text := []rune(strings.Repeat("话", 2000))
	copy(text, []rune("CAP2-HEAD"))
	copy(text[600:], []rune("CAP2-MID"))
	copy(text[2000-9:], []rune("CAP2-TAIL"))

	t.Run("not chosen: nothing is sent", func(t *testing.T) {
		e := newEnv(t, true)
		e.locality("external")
		e.setCapture(e.web, true)
		if out, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "CAP-MARK 下周把合同发给小王"}); err != nil || !out.Captured {
			t.Fatalf("out %+v err %v", out, err)
		}
		ev := e.waitDisposed("capture:" + e.web)
		if len(e.stub.Requests()) != 0 || ev.Disposition != domain.DispositionUnjudged || ev.UnjudgedReason != domain.UnjudgedOutboundUnset {
			t.Fatalf("requests %d event %+v", len(e.stub.Requests()), ev)
		}
	})
	t.Run("excerpt: trimmed", func(t *testing.T) {
		e := newEnv(t, true)
		e.locality("external")
		e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
		e.setCapture(e.web, true)
		e.setCaptureOutbound(e.web, "excerpt")
		if _, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": string(text)}); err != nil {
			t.Fatal(err)
		}
		e.waitDisposed("capture:" + e.web)
		body := e.sent(0)
		if !strings.Contains(body, "CAP2-HEAD") || strings.Contains(body, "CAP2-MID") || strings.Contains(body, "CAP2-TAIL") {
			t.Fatalf("conversation was not trimmed to the excerpt: %.200s", body)
		}
	})
	t.Run("local endpoint gets the whole delta", func(t *testing.T) {
		e := newEnv(t, true)
		e.locality("local")
		e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
		e.setCapture(e.web, true)
		if _, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": string(text)}); err != nil {
			t.Fatal(err)
		}
		e.waitDisposed("capture:" + e.web)
		if !strings.Contains(e.sent(0), "CAP2-TAIL") {
			t.Fatal("local endpoint should receive the full delta")
		}
	})
}

// 选定项目的捕获范围后，积压里的对话增量同样自动重判。
func TestCaptureRejudgedAfterProjectScopeChosen(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.locality("external")
	e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
	e.setCapture(e.web, true)
	if _, err := e.capture(caller(authz.LevelWorker, "s1"), map[string]any{"project": e.web, "text": "CAP-MARK 发合同"}); err != nil {
		t.Fatal(err)
	}
	e.waitDisposed("capture:" + e.web)
	e.setCaptureOutbound(e.web, "full")
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	if got := e.events("capture:" + e.web)[0]; got.Disposition == domain.DispositionUnjudged || got.Disposition == domain.DispositionPending {
		t.Fatalf("event %+v", got)
	}
	if !strings.Contains(e.sent(0), "CAP-MARK") {
		t.Fatal("rejudge did not reach the model")
	}
}

// hookJudge 在第一次调用模型时先执行 before（模拟本人在一批事件处理中途改设置），再交给真实的判断型。
type hookJudge struct {
	passJudge
	before func()
	calls  int
}

func (h *hookJudge) Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error) {
	h.calls++
	if h.calls == 1 {
		h.before()
	}
	return h.passJudge.Judge(ctx, c)
}

// 评审阻断项：外发范围与端点位置按事件重读，而不是整批快照。同一批两条事件，处理第一条时本人把范围改成 meta / none /
// 把本机端点改成外部：第二条只送新范围（或不送）。
func TestScopeChangeMidBatchAppliesToNextEvent(t *testing.T) {
	cases := []struct {
		name    string
		change  func(e *env)
		has     []string
		missing []string
		reqs    int // 第二条事件产生的请求数（初筛一次、起草一次）
		disp    domain.Disposition
	}{
		{"scope full→meta", func(e *env) { e.outbound(obdSrc, "full", "meta") }, []string{"docs.example.org"}, []string{"HEAD-MARK", "TAIL-MARK", "SUM-MARK"}, 2, domain.DispositionProposed},
		{"scope full→none", func(e *env) { e.outbound(obdSrc, "full", "none") }, nil, nil, 0, domain.DispositionDigest},
		{"locality local→external", func(e *env) { e.locality("external") }, []string{"docs.example.org"}, []string{"HEAD-MARK", "TAIL-MARK"}, 2, domain.DispositionProposed},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newEnv(t, true)
			e.chatOnly(`{"candidates":[{"title":"t","kind":"task"}]}`)
			e.locality("external")
			e.registerObd("full", "full")
			if c.name == "locality local→external" {
				e.outbound(obdSrc, "full", "meta") // 外部范围更窄；本机端点仍收全文
			}
			if c.name == "locality local→external" {
				e.locality("local")
			}
			e.svc.o.Judge = &hookJudge{passJudge: passJudge{e.svc.o.Judge}, before: func() { c.change(e) }}
			e.emit(obdSrc, "inbox", obdEvent("o1", "OBD-1 外发测试"), obdEvent("o2", "OBD-2 外发测试"))
			rs := e.stub.Requests()
			if len(rs) != 2+c.reqs {
				t.Fatalf("requests = %d, want %d", len(rs), 2+c.reqs)
			}
			// 位置在 Judge 调用时才变：Judge 拒绝按旧位置发送，第一条按新位置重新裁剪，同样不得带全文
			if strings.Contains(c.name, "locality") == strings.Contains(rs[0].BodyText, "TAIL-MARK") {
				t.Fatalf("first request has the wrong scope (locality case must be re-trimmed, others keep the starting scope): %.120s", rs[0].BodyText)
			}
			if c.reqs > 0 {
				second := rs[2].BodyText
				for _, w := range c.has {
					if !strings.Contains(second, w) {
						t.Errorf("%q missing from the second request", w)
					}
				}
				for _, w := range c.missing {
					if strings.Contains(second, w) {
						t.Errorf("%q leaked after the scope changed", w)
					}
				}
			}
			if got := e.byID(obdSrc)["o2"]; got.Disposition != c.disp {
				t.Fatalf("second event: %+v", got)
			}
		})
	}
}

// 外发预览与真实外发同源：每个范围的资料正文等于 material 按该范围给出的正文，元信息等于 eventInput 的结构化字段。
func TestOutboundPreviewMatchesWhatIsSent(t *testing.T) {
	ev := domain.SourceEvent{ID: "sev_p", SourceID: "mail", Type: "message", Subject: "renewal", Sender: "a@example.com",
		Summary: "please confirm", Detail: strings.Repeat("长", registry.ExcerptChars+20), Link: "https://docs.example.com/x?token=t",
		OccurredAt: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC), Messages: []domain.EventMessage{{Sender: "b@example.com", Text: "follow-up"}}}
	pv := OutboundPreview(ev)
	if len(pv) != 3 {
		t.Fatalf("preview has full, excerpt and meta only: %v", pv)
	}
	var in map[string]any
	if err := json.Unmarshal(eventInput(domain.Source{ID: "mail"}, ev, "", nil), &in); err != nil {
		t.Fatal(err)
	}
	for _, scope := range []string{registry.ScopeFull, registry.ScopeExcerpt, registry.ScopeMeta} {
		want, ok := material(ev, domain.Outbound{Local: scope})(policy.LocalityLocal)
		v := pv[scope]
		if !ok || v.Material != want {
			t.Fatalf("%s material = %q, want %q", scope, v.Material, want)
		}
		if v.EventType != in["event_type"] || v.Subject != in["subject"] || v.Sender != in["sender"] || v.OccurredAt != in["occurred_at"] || v.Link != in["link"] {
			t.Fatalf("%s fields %+v differ from the structured input %v", scope, v, in)
		}
	}
	if !strings.Contains(pv[registry.ScopeFull].Material, "follow-up") || !strings.Contains(pv[registry.ScopeFull].Material, "token=t") {
		t.Fatalf("full sends messages and the full link: %q", pv[registry.ScopeFull].Material)
	}
	if pv[registry.ScopeMeta].Material != "" || pv[registry.ScopeMeta].Link != "docs.example.com" {
		t.Fatalf("meta sends no body and only the link domain: %+v", pv[registry.ScopeMeta])
	}
}
