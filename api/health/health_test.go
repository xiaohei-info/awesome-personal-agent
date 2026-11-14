package health

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/chain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	khealth "github.com/xiaohei-info/awesome-personal-agent/kernel/health"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
	// 十类信号的固定顺序（设计 §15.2 表）。
	signalIDs = []string{"source_failures", "outcall", "delivery_backlog", "chain_trips", "agentsmd_drift", "pack_rejections", "model_unavailable", "unjudged_backlog", "rule_fallbacks", "receipt_failures", "other_alerts", "stalled_delegations", "clock_lag"}
)

type fakePacks []packs.Rejection

func (f fakePacks) Health() []packs.Rejection { return f }

type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	auth *auth.Auth
	srv  *core.Server
}

func setup(t *testing.T, ph khealth.PackHealth) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, clock.Module, core.Module,
		proc.Module, delivery.Module, registry.Module, runner.Module, agentsmd.Module, direct.Module, items.Module, executors.Module, kdoctor.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	x := command.New(st, time.Now)
	if err := registry.Register(x, registry.Options{Bundled: []string{"pull-feed"}}); err != nil {
		t.Fatal(err)
	}
	a := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(st, delivery.New(delivery.Options{}), ph, 0), DoctorRoutes(st, kdoctor.Env{})}})
	if err != nil {
		t.Fatal(err)
	}
	return &env{t: t, st: st, x: x, auth: a, srv: srv}
}

func (e *env) get(kind auth.Kind) *httptest.ResponseRecorder {
	e.t.Helper()
	return e.serve(e.request(kind))
}

func (e *env) request(kind auth.Kind) *http.Request {
	e.t.Helper()
	scope := ""
	if kind == auth.Ext || kind == auth.Webhook {
		scope = "x"
	}
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Authorization", "Bearer "+secret)
	return req
}

func (e *env) serve(req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) report(kind auth.Kind) Report {
	e.t.Helper()
	rec := e.get(kind)
	if rec.Code != http.StatusOK {
		e.t.Fatalf("GET /health: %d %s", rec.Code, rec.Body)
	}
	var r Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		e.t.Fatal(err)
	}
	return r
}

func (r Report) byID(id string) Signal {
	for _, s := range r.Signals {
		if s.ID == id {
			return s
		}
	}
	return Signal{}
}

func (e *env) alert(object, reason string, payload any) {
	e.t.Helper()
	p, _ := json.Marshal(payload)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: domain.EvHealthAlert, Object: object, Reason: reason, Payload: p, At: time.Now(),
			Actor: domain.Actor{Type: domain.ActorSystem, Principal: "system", Via: domain.ViaRule}, Basis: &domain.Basis{PolicyVersion: 1, RuleID: "r"}})
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

// flow-health：全新账本返回固定顺序的十类信号，全部正常，且每类都带修复入口。
func TestEmptyLedgerReportsThirteenHealthySignals(t *testing.T) {
	r := setup(t, nil).report(auth.Observer)
	if !r.Healthy || len(r.Signals) != len(signalIDs) {
		t.Fatalf("%+v", r)
	}
	for i, s := range r.Signals {
		if s.ID != signalIDs[i] || !s.OK || s.Count != 0 || s.Why != "" || s.Next == "" {
			t.Fatalf("signal %d = %+v", i, s)
		}
	}
}

// flow-health：账本里各类问题都反映到对应信号，why/next 只写机制，来源报错与原因正文不外泄。
func TestProblemsSurfaceWithRepairEntryAndNoLedgerContent(t *testing.T) {
	e := setup(t, fakePacks{{Pack: "work", Source: "user", Reason: "SECRET-PACK-REASON"}})
	in, _ := json.Marshal(map[string]any{"id": "src_flaky", "type": "pull", "source_type": "feed", "command": "pull-feed", "schedule": "15m", "timeout": 60, "enabled": true})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: registry.CmdRegister, Input: in}); err != nil {
		t.Fatal(err)
	}
	stats, _ := json.Marshal(runner.Stats{ConsecutiveFailures: 3, LastError: "SECRET-SOURCE-ERROR"})
	if err := e.st.Update(bg, func(tx *store.Tx) error { return registry.SetRunState(tx, "src_flaky", "", stats) }); err != nil {
		t.Fatal(err)
	}
	e.alert("item_1", chain.ExceededReason, map[string]any{"chain_depth": 4, "threshold": 3})
	e.alert("item_2", "SECRET-SCREEN-REASON", map[string]any{"kind": "screening"})
	e.alert("rule:inbound/x", khealth.RuleFallbackReason, map[string]string{"rule": "inbound/x"})
	e.alert("item_3", "SECRET-FOLLOWUP-REASON", map[string]any{"kind": khealth.FollowupKind})
	e.alert("item_5", "SECRET-RECEIPT-REASON", map[string]any{"kind": command.ReceiptKind, "command": "delegate.report", "exit": 23, "why": "SECRET-RECEIPT-WHY"})
	e.alert("item_4", "SECRET-RULE-ACTION-REASON", map[string]any{"rule": "r/x", "action": "create_timer"})

	rec := e.get(auth.Observer)
	var r Report
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s %v", rec.Code, rec.Body, err)
	}
	if r.Healthy {
		t.Fatal("must be unhealthy")
	}
	for id, want := range map[string]string{"source_failures": "src_flaky", "chain_trips": "item_1", "pack_rejections": "work",
		"model_unavailable": "item_2", "rule_fallbacks": "inbound/x", "receipt_failures": "item_5", "other_alerts": "item_4"} {
		s := r.byID(id)
		raw, _ := json.Marshal(s.Detail)
		if id == "model_unavailable" { // 初筛 1 + 跟进判断 1
			if s.Count != 2 || !strings.Contains(string(raw), "item_3") {
				t.Fatalf("%s = %+v (%s)", id, s, raw)
			}
			continue
		}
		if s.OK || s.Count != 1 || s.Why == "" || s.Next == "" || !strings.Contains(string(raw), want) {
			t.Fatalf("%s = %+v (%s)", id, s, raw)
		}
	}
	for _, id := range []string{"outcall", "delivery_backlog", "agentsmd_drift"} {
		if s := r.byID(id); !s.OK {
			t.Fatalf("%s should be ok: %+v", id, s)
		}
	}
	for _, secret := range []string{"SECRET-SOURCE-ERROR", "SECRET-SCREEN-REASON", "SECRET-FOLLOWUP-REASON", "SECRET-RULE-ACTION-REASON", "SECRET-RECEIPT-REASON", "SECRET-RECEIPT-WHY"} {
		if strings.Contains(rec.Body.String(), secret) {
			t.Fatalf("response leaks %s: %s", secret, rec.Body)
		}
	}
}

// 能力包的拒绝原因是包文件自己的字段与新写法提示，不是账本内容：detail 逐个列出（C.11），本人据此就能修。
func TestPackRejectionDetailListsReason(t *testing.T) {
	e := setup(t, fakePacks{{Pack: "work", Source: "user", Reason: "prompts/j.md: confidence_threshold is removed"}})
	rec := e.get(auth.Observer)
	if !strings.Contains(rec.Body.String(), `"rejections":[{"pack":"work","source":"user","reason":"prompts/j.md: confidence_threshold is removed"}]`) {
		t.Fatalf("%s", rec.Body)
	}
}

// 失败语义：账本读失败原样报错，不返回残缺的"健康"。
func TestStorageFailureIsNotReportedHealthy(t *testing.T) {
	e := setup(t, nil)
	req := e.request(auth.Observer)
	e.st.Close()
	if rec := e.serve(req); rec.Code < 500 {
		t.Fatalf("closed ledger must be a server error: %s", rec.Body)
	}
}

// 授权：ext 凭据不能读健康视图；未认证被拒。
func TestExtCredentialAndAnonymousAreRejected(t *testing.T) {
	e := setup(t, nil)
	if rec := e.get(auth.Ext); rec.Code != http.StatusForbidden {
		t.Fatalf("ext: %d %s", rec.Code, rec.Body)
	}
	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous: %d %s", rec.Code, rec.Body)
	}
}

// 验收 ACT-OBD-13：外发范围未选而未判定的事件计入 unjudged_backlog 的 outbound_unset，信号是提醒级（不影响 healthy），
// next 指向来源页去选范围；它们与模型端点没配置无关，不会让 model_unavailable 报警。
func TestUnjudgedBacklogCountsOutboundUnset(t *testing.T) {
	e := setup(t, fakePacks(nil))
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		for id, reason := range map[string]string{"ev1": "outbound_unset", "ev2": "outbound_unset", "ev3": "undecided"} {
			if _, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
				occurred_at, correlation_keys, sender, sender_verified, disposition, unjudged_reason) VALUES ('d', ?, ?, 'src_rss', 'run', 't', 's', '', '', '', 0, '[]', '', 0, 'unjudged', ?)`,
				id, id, reason); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	r := e.report(auth.Observer)
	var sig Signal
	for _, s := range r.Signals {
		switch s.ID {
		case "unjudged_backlog":
			sig = s
		case "model_unavailable":
			if !s.OK {
				t.Errorf("held-back events must not blame the model endpoint: %+v", s)
			}
		}
	}
	d, _ := sig.Detail.(map[string]any)
	if sig.OK || sig.Severity != SeverityWarning || !r.Healthy || sig.Count != 3 || d["outbound_unset"] != float64(2) || d["undecided"] != float64(1) || d["model_unavailable"] != float64(0) {
		t.Fatalf("signal %+v healthy=%v", sig, r.Healthy)
	}
	if !strings.Contains(sig.Next, "outbound scope") {
		t.Fatalf("next does not point to choosing the scope: %q", sig.Next)
	}
}

// ACT-HLT-03/05：时钟迟到是提醒级——severity=warning、why 说明机制，healthy 不变，信号仍恒为十三项。
func TestClockLagIsWarningAndKeepsHealthy(t *testing.T) {
	e := setup(t, nil)
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return khealth.RecordClockLag(tx, clock.Command{TimerID: "tm_1", Target: "item_hl2", Kind: domain.TimerDue, Late: 8 * time.Minute}, time.Now())
	}); err != nil {
		t.Fatal(err)
	}
	r := e.report(auth.Observer)
	s := r.byID("clock_lag")
	if len(r.Signals) != 13 || !r.Healthy || s.OK || s.Count != 1 || s.Severity != SeverityWarning || s.Why == "" || s.Next == "" {
		t.Fatalf("%+v", r)
	}
	if sd := r.byID("stalled_delegations"); !sd.OK || sd.Severity != "" {
		t.Fatalf("%+v", sd)
	}
}

// ACT-EPC-09：model_unavailable 的 detail 是 {judge, generate, endpoint}，各取空、not_configured 或 failing，endpoint 取较差者。
func TestModelUnavailableDetailSplitsJudgeAndGenerate(t *testing.T) {
	detail := func(e *env) map[string]any {
		t.Helper()
		s := e.report(auth.Observer).byID("model_unavailable")
		d, _ := s.Detail.(map[string]any)
		return d
	}
	want := func(d map[string]any, judge, generate, endpoint string) {
		t.Helper()
		if d["judge"] != judge || d["generate"] != generate || d["endpoint"] != endpoint {
			t.Fatalf("detail %v, want judge=%q generate=%q endpoint=%q", d, judge, generate, endpoint)
		}
	}
	e := setup(t, fakePacks(nil))
	want(detail(e), "", "", "")

	// 没配置任何端点而有事件在等：两个端点都 not_configured。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
			occurred_at, correlation_keys, sender, sender_verified, disposition, unjudged_reason) VALUES ('d', 'e1', 'e1', 'src_rss', 'run', 't', 's', '', '', '', 0, '[]', '', 0, 'unjudged', 'model_unavailable')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want(detail(e), "not_configured", "not_configured", "not_configured")

	// 生成端点持续失败：generate 为 failing，judge 仍只是没配置，endpoint 取较差的 failing。
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO direct_health (id, since, last_error) VALUES (1, 1, 'boom')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	want(detail(e), "not_configured", "failing", "failing")
}

// 暂停模型（§8.7、§15.2）：暂停期间未判定的事件记 models_paused，进积压提醒；模型信号是正常的"已暂停"，
// 端点此前的失败标记不报警，整体仍健康。
func TestPausedModelsAreHealthyAndCountedApart(t *testing.T) {
	e := setup(t, fakePacks(nil))
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := policy.Update(tx, owner.Actor, "pause", func(p *policy.Policy) error {
			p.GenerateEndpoint = policy.GenerateEndpoint{URL: "http://127.0.0.1:9/v1", Model: "m", APIKey: "k"}
			p.ModelsPaused = true
			return nil
		}); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO direct_health (id, since, last_error) VALUES (1, 1, 'boom')`); err != nil {
			return err
		}
		_, err := tx.Exec(`INSERT INTO sources_runner_event (dedupe_domain, event_id, id, source_id, run_id, type, subject, summary, detail, link,
			occurred_at, correlation_keys, sender, sender_verified, disposition, unjudged_reason) VALUES ('d', 'e1', 'e1', 'src_rss', 'run', 't', 's', '', '', '', 0, '[]', '', 0, 'unjudged', 'models_paused')`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	r := e.report(auth.Observer)
	m := r.byID("model_unavailable")
	md, _ := m.Detail.(map[string]any)
	if !m.OK || md["paused"] != true || md["judge"] != "paused" || md["generate"] != "paused" || md["endpoint"] != "" || !r.Healthy {
		t.Fatalf("model signal %+v healthy=%v", m, r.Healthy)
	}
	b := r.byID("unjudged_backlog")
	bd, _ := b.Detail.(map[string]any)
	if b.OK || b.Count != 1 || bd["models_paused"] != float64(1) || bd["model_unavailable"] != float64(0) {
		t.Fatalf("backlog signal %+v", b)
	}
}
