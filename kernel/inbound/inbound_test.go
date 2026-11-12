package inbound

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/metrics"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

const (
	packYAML = `name: inbound-triage
version: 1.0.0
description: inbound triage fixture
source_rules:
  - id: ticket-opened
    match: {source_type: ticket, event_type: [ticket.opened, ticket.labeled]}
    kind: task
    project_by: repo_remote
    then: proposed
  - id: noise
    match: {source_type: ticket, event_type: ticket.noise}
    then: ignore
  - id: mail-in
    match: {source_type: inbox, event_type: mail.received}
    project_by: "fixed:p_mail"
    then: judge
  - id: auto-pr
    match: {source_type: review, event_type: pr.opened}
    kind: task
    then: proposed
    accept: auto
    mode: observe
  - id: x-prop
    match: {source_type: exits, event_type: x.prop}
    then: proposed
  - id: x-dig
    match: {source_type: exits, event_type: x.news}
    then: digest
  - id: x-eq
    match: {source_type: exits, event_type: x.who}
    where: [{field: sender, op: eq, value: alice}]
  - id: x-in
    match: {source_type: exits, event_type: x.who-in}
    where: [{field: sender, op: in, value: [bob, carol]}]
  - id: x-size
    match: {source_type: exits, event_type: x.size}
    where: [{field: metrics.size, op: gte, value: 10}, {field: metrics.size, op: lt, value: 20}]
  - id: x-fixed
    match: {source_type: exits, event_type: x.fixed}
    project_by: "fixed:p_mail"
  - id: x-none
    match: {source_type: exits, event_type: x.none}
    project_by: none
  - id: x-tpl
    match: {source_type: exits, event_type: x.tpl}
    then: screen
    screen: acc-screen
  - id: x-chat
    match: {source_type: exits, event_type: x.chat}
    then: extract
  - id: x-alert
    match: {source_type: exits, event_type: x.metric}
    where: [{field: metrics.value, op: crosses_above, value: 90}]
    then: alert
    mode: observe
  - id: x-quiet
    match: {source_type: exits, event_type: x.metric}
    then: ignore
  - id: x-level
    match: {source_type: exits, event_type: x.level}
    where: [{field: metrics.level, op: changed}]
  - id: x-temp
    match: {source_type: exits, event_type: x.temp}
    where: [{field: metrics.temp, op: crosses_below, value: 5}]
`
	autoKey = "inbound-triage/source_rules/auto-pr"
)

type env struct {
	t         *testing.T
	st        *store.Store
	x         *command.Executor
	clk       *clock.Clock
	r         *runner.Runner
	svc       *Service
	stub      *modelstub.Stub    // 对话端点（生成端点）替身
	cls       *modelstub.ClsStub // 判断端点（分类端点）替身
	dir       string
	now       time.Time
	done      chan proc.Result
	errs      chan error
	loadr     *packs.Loader
	web, mail string
	sources   int
	queue     *delivery.Queue
	att       *attention.Attention
	// wantErr 为真时，拉取期间的流水线错误留在 errs 里，不让 waitRuns 失败。
	wantErr bool
}

func (e *env) Now() time.Time { return e.now }

// newEnv 装配真实的来源调度、规则、判断型与入站流水线；只有模型端点是替身。
func newEnv(t *testing.T, configured bool) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, proc.Module, delivery.Module, attention.Module, registry.Module, runner.Module, rules.Module, direct.Module, delegation.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, dir: t.TempDir(), now: t0, stub: modelstub.New(), cls: modelstub.NewCls(), done: make(chan proc.Result, 16), errs: make(chan error, 16)}
	t.Cleanup(e.stub.Close)
	t.Cleanup(e.cls.Close)
	e.x = command.New(st, e.Now)
	lay := config.LayoutOf(filepath.Join(e.dir, "home"))
	if err := lay.Ensure(); err != nil {
		t.Fatal(err)
	}
	kinds := items.NewKinds()
	for _, err := range []error{projects.Register(e.x), items.Register(e.x, kinds), rules.Register(e.x),
		registry.Register(e.x, registry.Options{Layout: lay})} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if configured {
		e.configureModel()
	}
	var web, mail struct{ ID string }
	_ = json.Unmarshal(e.must("project.create", map[string]any{"name": "web"}), &web)
	e.must("project.repo.add", map[string]any{"id": web.ID, "remote": "https://git.example/org/web"})
	_ = json.Unmarshal(e.must("project.create", map[string]any{"name": "mail"}), &mail)
	e.web, e.mail = web.ID, mail.ID
	fs := fstest.MapFS{"inbound-triage/pack.yaml": {Data: []byte(strings.ReplaceAll(packYAML, "p_mail", mail.ID))}}
	for _, name := range []string{"triage-screen", "triage-draft", "triage-extract", "capture-prescreen"} { // 用内置包里随发行的模板，不另写一份
		b, err := os.ReadFile("../../packs/inbound-triage/prompts/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		fs["inbound-triage/prompts/"+name+".md"] = &fstest.MapFile{Data: b}
		if name == "triage-screen" {
			fs["inbound-triage/prompts/acc-screen.md"] = &fstest.MapFile{Data: b}
		}
	}
	e.loadr = packs.New(packs.Options{Builtin: fs, Kinds: kinds})
	if rej, err := e.loadr.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %+v", err, rej)
	}
	judge, err := direct.New(direct.Options{Store: st, Packs: e.loadr, Model: &model.Client{}, Now: e.Now,
		Sleep: func(context.Context, time.Duration) error { return nil }, Attempts: 2})
	if err != nil {
		t.Fatal(err)
	}
	eng, err := rules.New(rules.Options{Store: st, Exec: e.x, Packs: e.loadr, Owner: "owner", Now: e.Now,
		OnError: func(err error) { t.Errorf("rules: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	e.queue = delivery.New(delivery.Options{Now: e.Now})
	e.att = attention.New(attention.Options{Queue: e.queue, Now: e.Now})
	e.svc, err = New(Options{Store: st, Exec: e.x, Packs: e.loadr, Judge: judge, Owner: "owner", OnError: func(err error) { e.errs <- err },
		Series: metrics.Series{}, Alert: &metrics.Alerter{Attention: e.att}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(e.svc.captures.Wait) // 晚于 st.Close 注册，所以先执行
	if err := e.svc.Register(e.x); err != nil {
		t.Fatal(err)
	}
	e.r, err = runner.New(runner.Options{Store: st, Exec: e.x, Layout: lay, Now: e.Now,
		OnError: func(err error) { e.errs <- err }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.r.Register(e.x); err != nil {
		t.Fatal(err)
	}
	disp := proc.New(st, config.Config{MaxSubprocesses: 2, MaxShortSubprocesses: 2}, proc.Options{Now: e.Now, OnResult: func(res proc.Result) {
		if err := e.r.Handle(bg, res); err != nil {
			e.errs <- err
		}
		e.done <- res
	}})
	t.Cleanup(disp.Close)
	e.r.Attach(disp)
	e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error { return e.r.HandleTimer(tx, c) }, e.Now)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) configureModel() {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error {
			p.GenerateEndpoint, p.ScreeningTimeoutMs = policy.GenerateEndpoint{URL: e.stub.URL(), Model: "m1", APIKey: "k", Locality: policy.LocalityLocal}, 1000
			p.JudgeEndpoint = &policy.JudgeEndpoint{URL: e.cls.URL(), Protocol: policy.ProtocolProbability, Locality: policy.LocalityLocal, TimeoutMs: 1000}
			return nil
		})
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) must(cmd string, in any) json.RawMessage {
	e.t.Helper()
	b, _ := json.Marshal(in)
	r, err := e.x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

// emit 登记一个来源（若尚未登记）并让它的脚本输出给定事件，跑一次拉取，等结果入账并被入站流水线处理完。
func (e *env) emit(source, kind string, events ...map[string]any) {
	e.t.Helper()
	e.emitRaw(source, kind, events...)
	select {
	case err := <-e.errs:
		e.t.Fatalf("pipeline: %v", err)
	default:
	}
}

// emitRaw 同 emit，但不对流水线错误失败：错误留在 e.errs 里由调用方检查。
func (e *env) emitRaw(source, kind string, events ...map[string]any) {
	e.t.Helper()
	var body strings.Builder
	for _, ev := range events {
		b, _ := json.Marshal(ev)
		body.WriteString("echo '" + string(b) + "'\n")
	}
	body.WriteString("echo '{\"cursor\":\"c\"}'\n")
	script := filepath.Join(e.dir, source+".sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\n"+body.String()), 0o755); err != nil {
		e.t.Fatal(err)
	}
	if _, err := e.source(source); err != nil {
		e.must(registry.CmdRegisterExec, map[string]any{"id": source, "type": "script", "source_type": kind, "command": script,
			"schedule": "1m", "timeout": 30})
		e.sources++
	}
	e.now = e.now.Add(2 * time.Minute)
	if err := e.r.Tick(bg, e.clk); err != nil {
		e.t.Fatal(err)
	}
	e.waitRuns()
}

// waitRuns 等所有已登记来源本轮到期的 Run 结束（含结果入账与入站处理）。
func (e *env) waitRuns() {
	e.t.Helper()
	for range e.sources {
		select {
		case <-e.done:
		case err := <-e.errs:
			if !e.wantErr {
				e.t.Fatalf("runner: %v", err)
			}
			e.errs <- err
		case <-time.After(20 * time.Second):
			e.t.Fatal("timed out waiting for the source run")
		}
	}
}

func (e *env) waitRunsNoErr() {
	e.t.Helper()
	for range e.sources {
		select {
		case <-e.done:
		case <-time.After(20 * time.Second):
			e.t.Fatal("timed out waiting for the source run")
		}
	}
}

func (e *env) source(id string) (s domain.Source, err error) {
	err = e.st.View(bg, func(tx *store.Tx) (err error) { s, err = registry.Get(tx, id); return })
	return
}

func ev(id, typ, subject string, extra map[string]any) map[string]any {
	m := map[string]any{"id": id, "type": typ, "subject": subject, "occurred_at": "2026-03-01T00:00:00Z", "detail": "外部正文：" + subject}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (e *env) events(source string) (out []domain.SourceEvent) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = runner.Events(tx, source); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) item(id string) domain.Item {
	e.t.Helper()
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return it
}

func (e *env) allItems() (out []domain.Item) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = items.List(tx, items.Filter{}); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

// screen 让分类端点对所有初筛题回答 p_yes = p；draft 让生成端点对起草与抽取请求返回 content。
func (e *env) screen(p float64) { e.cls.SetDefaultPYes(p) }
func (e *env) draft(content string) {
	e.stub.SetDefault(modelstub.Reply{Content: content})
}

// judge 沿用 v0.1 测试里"判断结论"的写法：含 ignore 的结论让分类端点答"否"，其余答"是"。
func (e *env) judge(content string) {
	if strings.Contains(content, `"ignore"`) {
		e.screen(0.05)
		return
	}
	e.screen(0.95)
}

// chatOnly 去掉判断端点，只留对话端点：初筛由对话端点答"是"，其余请求（起草、抽取）得到 reply。
// 外发范围的测试用它，因为所有内容都经同一个端点，容易数请求、查泄漏。
func (e *env) chatOnly(reply string) {
	e.t.Helper()
	err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "chat only", func(p *policy.Policy) error { p.JudgeEndpoint = nil; return nil })
		return err
	})
	if err != nil {
		e.t.Fatal(err)
	}
	e.stub.Judge(modelstub.ChatJudge{Answer: "yes"})
	e.draft(reply)
}

// requests 返回生成端点收到的、模板为 tmpl 的请求数（system 段里带模板的问法，这里按输出 schema 的特征字段识别）。
func (e *env) drafts() int {
	n := 0
	for _, r := range e.stub.Requests() {
		if strings.Contains(r.BodyText, "起草一条建议") {
			n++
		}
	}
	return n
}

// ACT-TRI-03：没有模型端点时，命中 then: proposed 规则的事件照常进建议流，kind 和项目正确，且不外呼。
func TestRuleProposesWithoutModel(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "登录页 500", map[string]any{
		"summary": "点击登录后 500", "link": "https://git.example/org/web/issues/7", "sender": "alice", "sender_verified": true}))

	evs := e.events("src_t")
	if len(evs) != 1 || evs[0].Disposition != domain.DispositionProposed || evs[0].Item == "" {
		t.Fatalf("event %+v", evs)
	}
	it := e.item(evs[0].Item)
	if it.Phase != domain.PhaseProposed || it.Kind != "task" || it.Project != e.web || it.Title != "登录页 500" || it.Summary != "点击登录后 500" {
		t.Fatalf("item %+v", it)
	}
	if it.Origin == nil || it.Origin.Source != "src_t" || it.Origin.Actor != "alice" || it.Origin.Ref != "https://git.example/org/web/issues/7" {
		t.Fatalf("origin %+v", it.Origin)
	}
	if !strings.Contains(it.Body, "外部正文") {
		t.Fatalf("body %q", it.Body)
	}
	if n := len(e.stub.Requests()); n != 0 {
		t.Fatalf("model was called %d times", n)
	}
}

// 链接不属于任何项目的仓库时不归项目，但仍进建议流。
func TestRepoRemoteWithoutProjectStillProposes(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "别处的问题", map[string]any{"link": "https://git.example/org/web2/issues/1"}))
	evs := e.events("src_t")
	if evs[0].Disposition != domain.DispositionProposed || e.item(evs[0].Item).Project != "" {
		t.Fatalf("event %+v item %+v", evs[0], e.item(evs[0].Item))
	}
}

// then: ignore 只记 ignored，不建事项。
func TestIgnoreRuleRecordsIgnored(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_t", "ticket", ev("n1", "ticket.noise", "机器人评论", nil))
	if evs := e.events("src_t"); evs[0].Disposition != domain.DispositionIgnored || evs[0].Item != "" || len(e.allItems()) != 0 {
		t.Fatalf("event %+v items %d", evs[0], len(e.allItems()))
	}
}

// ACT-TRI-01（模型未配置）：规则判不了的事件标记 unjudged，留在来源页，不进建议流，也不当作失败。
func TestUnconfiguredModelLeavesEventUnjudged(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_m", "inbox", ev("m1", "mail.received", "合同请签字", nil))
	evs := e.events("src_m")
	if evs[0].Disposition != domain.DispositionUnjudged || evs[0].Item != "" || len(e.allItems()) != 0 {
		t.Fatalf("event %+v items %d", evs[0], len(e.allItems()))
	}
	// 规则照常：同一时刻模型不可用不影响确定性规则。
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "仍然进建议流", nil))
	if got := e.events("src_t")[0].Disposition; got != domain.DispositionProposed {
		t.Fatalf("rule path blocked by missing model: %s", got)
	}
}

// ACT-TRI-05：两个端点都配置却都不可用：重试用尽后记 unjudged（model_unavailable）；事件不丢，不进建议流，也不当作"把握低"。
func TestModelFailureLeavesEventUnjudgedAfterRetries(t *testing.T) {
	e := newEnv(t, true)
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	e.stub.SetDefault(modelstub.Reply{Status: 500})
	e.emit("src_m", "inbox", ev("m1", "mail.received", "合同请签字", nil))
	evs := e.events("src_m")
	if len(evs) != 1 || evs[0].Disposition != domain.DispositionUnjudged || evs[0].UnjudgedReason != domain.UnjudgedModelUnavailable || len(e.allItems()) != 0 {
		t.Fatalf("event %+v items %d", evs, len(e.allItems()))
	}
	if n := len(e.stub.Requests()); n != 2 {
		t.Fatalf("chat attempts = %d, want 2 (retried with backoff)", n)
	}
}

// ACT-INP-06：初筛为是的事件被完整起草后进入 proposed：标题、摘要、类别、项目、日期与推荐处置都来自起草；
// 判断记录 id 记在来源事件上；起草请求用 triage-draft 模板且不含判断题。
func TestScreenedInEventIsFullyDrafted(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"title":"审阅季度报告","summary":"对方请你周五前审阅","kind":"task","project":"` + e.web + `","deadline":"2026-03-13","planned_for":"2026-03-12","recommend":"accept","reason":"对方明确提出请求"}`)
	e.emit("src_x", "other", ev("h1", "thing.happened", "请在周五前审阅季度报告", nil))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionProposed || d.Judgment == "" {
		t.Fatalf("event %+v", d)
	}
	it := e.item(d.Item)
	if it.Phase != domain.PhaseProposed || it.Title != "审阅季度报告" || it.Summary != "对方请你周五前审阅" || it.Kind != "task" ||
		it.Project != e.web || it.PlannedFor != "2026-03-12" || it.Deadline == nil || it.Deadline.Format("2006-01-02") != "2026-03-13" {
		t.Fatalf("item %+v", it)
	}
	p := it.Proposal
	if p == nil || p.Recommend != "accept" || p.Reason != "对方明确提出请求" || p.LowConfidence || len(p.Sources) != 1 || p.Sources[0] != d.ID {
		t.Fatalf("proposal %+v", p)
	}
	if n := e.drafts(); n != 1 || len(e.cls.Batches()) != 1 || strings.Contains(e.sent(0), "answer") {
		t.Fatalf("drafts %d batches %d", n, len(e.cls.Batches()))
	}
}

// ACT-INP-05：初筛低于下限：忽略并留痕（有判断记录），不重答、不起草。
func TestScreenedOutIsIgnoredWithRecord(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.05)
	e.emit("src_x", "other", ev("x1", "thing.happened", "促销邮件", nil))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionIgnored || d.Item != "" || d.Judgment == "" || len(e.allItems()) != 0 {
		t.Fatalf("event %+v items %d", d, len(e.allItems()))
	}
	if len(e.stub.Requests()) != 0 || len(e.cls.Batches()) != 1 {
		t.Fatalf("chat %d cls %d", len(e.stub.Requests()), len(e.cls.Batches()))
	}
}

// ACT-INP-07：分类端点落入不确定带、对话端点也未定：留为建议并标"把握低"，仍被起草。
func TestUndecidedStaysAsLowConfidenceSuggestion(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.35)
	no := false
	e.stub.Judge(modelstub.ChatJudge{Key: modelstub.Key{Contains: "unsure"}, Answer: "no", Sufficient: &no})
	e.draft(`{"title":"unsure 起草标题","kind":"task","recommend":"dismiss"}`)
	e.emit("src_x", "other", ev("u1", "thing.happened", "unsure 也许相关", nil))
	d := e.events("src_x")[0]
	it := e.item(d.Item)
	if d.Disposition != domain.DispositionProposed || it.Title != "unsure 起草标题" || it.Proposal == nil || !it.Proposal.LowConfidence {
		t.Fatalf("event %+v item %+v", d, it)
	}
}

// ACT-INP-08、09：起草失败（端点 500）或输出不合 schema 都不挡路：标题取来源事件的标题，摘要留空，kind 取缺省。
func TestDraftFailureDegradesToEventTitle(t *testing.T) {
	for name, reply := range map[string]modelstub.Reply{
		"http 500":   {Status: 500},
		"bad schema": {Content: `{"kind":"foo"}`},
		"bad kind":   {Content: `{"title":"x","kind":"foo"}`},
	} {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, true)
			e.screen(0.9)
			e.stub.SetDefault(reply)
			e.emit("src_x", "other", ev("f1", "thing.happened", "原始标题", map[string]any{"summary": "原始摘要"}))
			d := e.events("src_x")[0]
			it := e.item(d.Item)
			if d.Disposition != domain.DispositionProposed || it.Title != "原始标题" || it.Summary != "" || it.Kind != "task" || it.Project != "" {
				t.Fatalf("event %+v item %+v", d, it)
			}
		})
	}
}

// ACT-INP-10、11：起草只能从内核给的候选清单里推荐"补到已有事项"；编造的事项 id 不被采纳，事件也不丢。
func TestAttachRecommendationComesOnlyFromCandidates(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	var t2 struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "供应商合同", "project": e.web, "phase": "open", "reason": "r"}), &t2)
	e.draft(`{"title":"供应商合同第三版","kind":"task","recommend":"attach","attach_to":"` + t2.ID + `","reason":"同一件事"}`)
	e.emit("src_x", "other", ev("c1", "thing.happened", "供应商合同第三版", nil))
	it := e.item(e.events("src_x")[0].Item)
	if it.Proposal == nil || it.Proposal.Recommend != "attach" || it.Proposal.AttachTo != t2.ID {
		t.Fatalf("proposal %+v", it.Proposal)
	}
	if !strings.Contains(e.sent(0), t2.ID) { // 候选清单在起草请求的结构化输入里
		t.Fatal("draft request carries no candidate list")
	}
	if got := e.item(t2.ID); got.Phase != domain.PhaseOpen {
		t.Fatalf("recommendation changed the target: %+v", got)
	}

	e.draft(`{"title":"某个通知","kind":"task","recommend":"attach","attach_to":"itm-does-not-exist"}`)
	e.emit("src_y", "other", ev("c2", "thing.happened", "某个通知", nil))
	d := e.events("src_y")[0]
	if p := e.item(d.Item).Proposal; d.Disposition != domain.DispositionProposed || p == nil || p.AttachTo != "" || p.Recommend == "attach" {
		t.Fatalf("invented attach target adopted: %+v", p)
	}
}

// ACT-IRL-18：project_by 缺省时起草从活动项目清单里选；清单外的项目不被写入。
func TestDraftProjectComesFromActiveList(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"title":"a","kind":"task","project":"` + e.web + `"}`)
	e.emit("src_x", "other", ev("r1", "thing.happened", "r1", nil))
	e.draft(`{"title":"b","kind":"task","project":"no-such-project"}`)
	e.emit("src_y", "other", ev("r2", "thing.happened", "r2", nil))
	if got := e.item(e.events("src_x")[0].Item).Project; got != e.web {
		t.Fatalf("r1 project %q", got)
	}
	if got := e.item(e.events("src_y")[0].Item).Project; got != "" {
		t.Fatalf("r2 project %q", got)
	}
	if !strings.Contains(e.sent(0), e.mail) || !strings.Contains(e.sent(0), `\"projects\"`) {
		t.Fatal("draft request carries no active project list")
	}
}

// ACT-EPC-08 / ACT-TRI-07：只配判断端点时初筛照常定案，起草退化为来源标题、摘要留空，不向生成端点发请求。
func TestOnlyJudgeEndpointDraftsFromEventTitle(t *testing.T) {
	e := newEnv(t, true)
	e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "gen off", func(p *policy.Policy) error { p.GenerateEndpoint = policy.GenerateEndpoint{}; return nil })
		return err
	})
	e.cls.PYes(modelstub.Key{Contains: "epc-low"}, 0.05)
	e.cls.PYes(modelstub.Key{Contains: "epc-high"}, 0.9)
	e.emit("src_x", "other", ev("epc-low", "thing.happened", "epc-low", nil), ev("epc-high", "thing.happened", "epc-high", map[string]any{"summary": "原始摘要"}))
	if got := e.disposition("src_x", "epc-low").Disposition; got != domain.DispositionIgnored {
		t.Fatalf("low: %s", got)
	}
	d := e.disposition("src_x", "epc-high")
	if it := e.item(d.Item); d.Disposition != domain.DispositionProposed || it.Title != "epc-high" || it.Summary != "" {
		t.Fatalf("high: %+v %+v", d, it)
	}
	if len(e.stub.Requests()) != 0 {
		t.Fatal("generate endpoint was called")
	}
}

// ACT-EPC-09：两个端点都不配：规则判不了的事件记 unjudged（model_unavailable），不发任何请求。
func TestNoEndpointsOnlyRulesApply(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_x", "other", ev("none1", "thing.happened", "自由文本", nil))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionUnjudged || d.UnjudgedReason != domain.UnjudgedModelUnavailable || len(e.stub.Requests())+len(e.cls.Batches()) != 0 {
		t.Fatalf("event %+v", d)
	}
}

// ACT-INP-02：同一事件重复到达只处理一次：一条来源事件、一条建议，初筛与起草各一次。
func TestDuplicateEventIsProcessedOnce(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"title":"inp-dup 起草标题","kind":"task"}`)
	e.emit("src_x", "other", ev("inp-dup-1", "thing.happened", "inp-dup 请审阅文档", nil))
	e.emit("src_x", "other", ev("inp-dup-1", "thing.happened", "inp-dup 请审阅文档", nil))
	if n := len(e.events("src_x")); n != 1 {
		t.Fatalf("events = %d", n)
	}
	if n := len(e.allItems()); n != 1 || len(e.cls.Batches()) != 1 || e.drafts() != 1 {
		t.Fatalf("items %d batches %d drafts %d", n, len(e.cls.Batches()), e.drafts())
	}
}

// ACT-INP-14：已有的 proposed 事项（v0.1 留下的建议）不会被再起草：没有 pending 的来源事件，流水线什么也不做。
func TestExistingProposalIsNotRedrafted(t *testing.T) {
	e := newEnv(t, true)
	var p0 struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "v01-prop 旧建议", "body": "旧正文"}), &p0)
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	it := e.item(p0.ID)
	if it.Title != "v01-prop 旧建议" || it.Body != "旧正文" || it.Proposal != nil || len(e.stub.Requests())+len(e.cls.Batches()) != 0 {
		t.Fatalf("item %+v", it)
	}
}

// ACT-IRL-07：extract 出口先预筛、再由生成型从一条聚合事件抽出多条候选，各成 proposed 事项，来源事件记下全部事项。
func TestExtractExitProposesEveryCandidate(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[{"title":"周五前交方案","kind":"task"},{"title":"约李工下周对接","kind":"task","summary":"s"}]}`)
	e.emit("src_x", "exits", ev("x1", "x.chat", "群聊片段", map[string]any{"items": []map[string]any{
		{"sender": "bob", "text": "方案周五前给我", "at": "2026-03-01T00:00:00Z"}}}))
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionProposed || len(d.Items) != 2 || d.Item != d.Items[0] || d.Judgment == "" {
		t.Fatalf("event %+v", d)
	}
	for i, want := range []string{"周五前交方案", "约李工下周对接"} {
		if it := e.item(d.Items[i]); it.Title != want || it.Phase != domain.PhaseProposed || it.Proposal == nil || it.Proposal.Sources[0] != d.ID {
			t.Fatalf("item %d: %+v", i, it)
		}
	}
	if !strings.Contains(e.sent(0), "bob: 方案周五前给我") {
		t.Fatal("aggregated messages were not passed as material")
	}
}

// extract 出口预筛为否或没有候选：忽略，不建事项。
func TestExtractExitIgnoresWhenNothingToExtract(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.9)
	e.draft(`{"candidates":[]}`)
	e.emit("src_x", "exits", ev("x1", "x.chat", "闲聊", nil))
	if d := e.events("src_x")[0]; d.Disposition != domain.DispositionIgnored || len(e.allItems()) != 0 {
		t.Fatalf("event %+v", d)
	}
}

// ACT-TRI-06：端点恢复后，未判定事件自动重判并走完整流水线（初筛 → 起草），重判计数加一。
func TestRejudgeRunsFullPipelineAfterRecovery(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Now = e.Now
	e.cls.SetFault(modelstub.ClsFault{Status: 503})
	e.stub.SetDefault(modelstub.Reply{Status: 500})
	e.emit("src_x", "other", ev("tri-down-1", "thing.happened", "tri-down 自由文本", nil))
	if d := e.events("src_x")[0]; d.Disposition != domain.DispositionUnjudged || d.UnjudgedReason != domain.UnjudgedModelUnavailable {
		t.Fatalf("event %+v", d)
	}
	e.cls.Reset()
	e.screen(0.9)
	e.draft(`{"title":"tri-down 起草标题","kind":"task"}`)
	e.now = e.now.Add(15 * time.Minute)
	if err := e.svc.Rejudge(bg); err != nil {
		t.Fatal(err)
	}
	d := e.events("src_x")[0]
	if d.Disposition != domain.DispositionProposed || d.Rejudges != 1 || e.item(d.Item).Title != "tri-down 起草标题" {
		t.Fatalf("event %+v", d)
	}
}

// 初筛模板缺失是配置错误：真实上报，事件保持 pending，不伪造成功也不丢。
func TestMissingScreenTemplateKeepsEventPendingAndSurfacesError(t *testing.T) {
	e := newEnv(t, true)
	e.svc.o.Judge = failingJudge{passJudge: passJudge{e.svc.o.Judge}, err: direct.ErrBadTemplate}
	e.wantErr = true
	e.emitRaw("src_m", "inbox", ev("m1", "mail.received", "x", nil))
	select {
	case err := <-e.errs:
		if !errors.Is(err, direct.ErrBadTemplate) {
			t.Fatalf("error %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("pipeline error was swallowed")
	}
	if got := e.events("src_m")[0].Disposition; got != domain.DispositionPending || len(e.allItems()) != 0 {
		t.Fatalf("disposition %s items %d", got, len(e.allItems()))
	}
}

// passJudge 把调用原样交给真实的判断型；测试里的替身嵌入它，只改写关心的那一个方法。
type passJudge struct{ J Judge }

func (p passJudge) Judge(ctx context.Context, c direct.JudgeCall) (direct.Verdict, error) {
	return p.J.Judge(ctx, c)
}
func (p passJudge) Do(ctx context.Context, c direct.Call) (direct.Outcome, error) {
	return p.J.Do(ctx, c)
}

type failingJudge struct {
	passJudge
	err error
}

func (f failingJudge) Judge(context.Context, direct.JudgeCall) (direct.Verdict, error) {
	return direct.Verdict{}, f.err
}

func (e *env) script(name, body string) string {
	p := filepath.Join(e.dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		e.t.Fatal(err)
	}
	return p
}

// ACT-TRI-04：accept: auto 的规则在 observe 档事项仍以 proposed 进入，只记"本来会自动接受"；
// 本人在 Web 切到 auto 后再来的事件直接进 open，Event 记策略版本与规则 id。
func TestAutoAcceptRunsInObserveThenAuto(t *testing.T) {
	e := newEnv(t, true)
	e.emit("src_r", "review", ev("p1", "pr.opened", "修复登录", nil))
	first := e.events("src_r")[0]
	it := e.item(first.Item)
	if first.Disposition != domain.DispositionProposed || it.Phase != domain.PhaseProposed {
		t.Fatalf("observe auto-accepted: %+v / %+v", first, it)
	}
	var observed, accepted bool
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, it.ID)
		for _, x := range es {
			switch x.Event.Type {
			case rules.EvObserved:
				observed = x.Event.Basis != nil && x.Event.Basis.RuleID == autoKey && strings.Contains(string(x.Event.Payload), "accept")
			case domain.EvItemPhaseChanged:
				accepted = true
			}
		}
		return err
	})
	if !observed || accepted {
		t.Fatalf("observed=%v accepted=%v", observed, accepted)
	}

	e.must("rule.set_mode", map[string]string{"rule": autoKey, "mode": rules.ModeAuto})
	e.emit("src_r", "review", ev("p2", "pr.opened", "修复注册", nil))
	second := e.events("src_r")[1]
	it2 := e.item(second.Item)
	if second.Disposition != domain.DispositionProposed || it2.Phase != domain.PhaseOpen {
		t.Fatalf("auto did not accept: %+v / %+v", second, it2)
	}
	var basis *domain.Basis
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, it2.ID)
		for _, x := range es {
			if x.Event.Type == domain.EvItemPhaseChanged {
				basis = x.Event.Basis
			}
		}
		return err
	})
	var pv int
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { pv, _, err = policy.Current(tx); return })
	if basis == nil || basis.RuleID != autoKey || basis.PolicyVersion != pv || pv == 0 {
		t.Fatalf("basis %+v, policy version %d", basis, pv)
	}
}

// off 档不自动接受，也不留"本来会接受"的观察记录。
func TestAutoAcceptOffDoesNothingExtra(t *testing.T) {
	e := newEnv(t, false)
	e.must("rule.set_mode", map[string]string{"rule": autoKey, "mode": rules.ModeOff})
	e.emit("src_r", "review", ev("p1", "pr.opened", "修复登录", nil))
	it := e.item(e.events("src_r")[0].Item)
	if it.Phase != domain.PhaseProposed {
		t.Fatalf("phase %s", it.Phase)
	}
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, _ := eventlog.ReadObject(tx, it.ID)
		for _, x := range es {
			if x.Event.Type == rules.EvObserved {
				t.Fatal("off mode left an observation")
			}
		}
		return nil
	})
}

// 重复处理不产生重复事项：已处置的事件不再 pending，再跑一遍什么也不做。
func TestProcessIsIdempotent(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "一个问题", nil))
	for range 2 {
		if err := e.svc.Process(bg); err != nil {
			t.Fatal(err)
		}
	}
	if n := len(e.allItems()); n != 1 {
		t.Fatalf("items = %d", n)
	}
}

// 不支持的匹配条件是规则错误：暴露出来，事件不被吞掉。
func TestUnsupportedMatchConditionIsReported(t *testing.T) {
	src := domain.Source{ID: "s", SourceKind: "k"}
	if _, err := matchFlat(map[string]any{"colour": "red"}, src, domain.SourceEvent{}); err == nil {
		t.Fatal("unsupported condition accepted")
	}
	ok, err := matchFlat(map[string]any{"source_type": "k", "event_type": []any{"a", "b"}}, src, domain.SourceEvent{Type: "b"})
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if ok, _ := matchFlat(map[string]any{"event_type": "a"}, src, domain.SourceEvent{Type: "b"}); ok {
		t.Fatal("mismatch matched")
	}
}

func TestNewRequiresDependencies(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("New accepted empty options")
	}
}

// crashJudge 模拟"事项已建好、处置没记完"：判断之后、本轮建事项之前，替上一轮补上它已经建出的事项。
type crashJudge struct {
	passJudge
	e    *env
	once bool
}

func (c *crashJudge) Do(ctx context.Context, call direct.Call) (direct.Outcome, error) {
	out, err := c.passJudge.Do(ctx, call)
	if err == nil && !c.once {
		c.once = true
		var pend []domain.SourceEvent
		_ = c.e.st.View(bg, func(tx *store.Tx) (err error) { pend, err = runner.Pending(tx); return })
		in, _ := json.Marshal(map[string]any{"kind": "task", "title": "上一轮的标题", "ext": map[string]string{"source_event": pend[0].ID}})
		if _, err := c.e.x.Do(bg, command.Request{Identity: system, Command: "item.create", Input: in, IdemKey: "inbound:item:" + pend[0].ID}); err != nil {
			c.e.t.Error(err)
		}
	}
	return out, err
}

// 中途失败后重跑：判断输出变了也不会卡死，恰好一个事项并最终记下处置。
func TestRerunAfterPartialFailureResumesWithoutDuplicate(t *testing.T) {
	e := newEnv(t, true)
	e.screen(0.95)
	e.draft(`{"title":"第一轮的标题","kind":"task"}`)
	e.svc.o.Judge = &crashJudge{passJudge: passJudge{e.svc.o.Judge}, e: e}
	var b strings.Builder
	j, _ := json.Marshal(ev("m1", "mail.received", "合同请签字", nil))
	b.WriteString("echo '" + string(j) + "'\necho '{\"cursor\":\"c\"}'\n")
	e.must(registry.CmdRegisterExec, map[string]any{"id": "src_m", "type": "script", "source_type": "inbox", "command": e.script("m.sh", b.String()), "schedule": "1m", "timeout": 30})
	e.sources++
	e.now = e.now.Add(2 * time.Minute)
	if err := e.r.Tick(bg, e.clk); err != nil {
		t.Fatal(err)
	}
	e.waitRunsNoErr()
	select { // 第一轮：同一事件的幂等键遇到不同输入，真实报错
	case err := <-e.errs:
		if !strings.Contains(err.Error(), "idempotency") {
			t.Fatalf("unexpected error %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("no error on first pass; event %+v items %+v", e.events("src_m")[0], e.allItems())
	}
	if got := e.events("src_m")[0].Disposition; got != domain.DispositionPending {
		t.Fatalf("first pass: %s", got)
	}
	e.draft(`{"title":"重跑时的标题","kind":"note"}`) // 重跑时起草输出已不同
	if err := e.svc.Process(bg); err != nil {
		t.Fatal(err)
	}
	evs := e.events("src_m")
	if evs[0].Disposition != domain.DispositionProposed || len(e.allItems()) != 1 || e.allItems()[0].ID != evs[0].Item {
		t.Fatalf("event %+v items %+v", evs[0], e.allItems())
	}
}

// ACT-TRI-02：未判定事件手动转建议：进建议流，操作者是本人，处置改为 proposed 并关联事项，事项可从事件折叠重建。
func TestProposeUnjudgedManually(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_m", "inbox", ev("m1", "mail.received", "合同请签字", nil))
	id := e.events("src_m")[0].ID

	got, err := e.svc.ProposeUnjudged(bg, owner, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Disposition != domain.DispositionProposed || got.Item == "" {
		t.Fatalf("event %+v", got)
	}
	it := e.item(got.Item)
	if it.Phase != domain.PhaseProposed || it.Kind != defaultKind || it.Title != "合同请签字" || it.Project != e.mail || it.Origin.Source != "src_m" {
		t.Fatalf("item %+v", it) // kind 缺省 task，项目沿用命中规则的 fixed
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		for obj, typ := range map[string]string{id: EvDisposed, got.Item: domain.EvItemUpdated} {
			es, err := eventlog.ReadObject(tx, obj)
			if err != nil {
				return err
			}
			if last := es[len(es)-1].Event; last.Type != typ || last.Actor.Principal != "owner" || last.Actor.Type != domain.ActorHuman {
				t.Fatalf("%s: %+v", obj, last)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		if _, err := tx.Exec(`DELETE FROM items WHERE id = ?`, got.Item); err != nil {
			return err
		}
		if err := items.Rebuild(tx); err != nil {
			return err
		}
		return items.Verify(tx)
	}); err != nil {
		t.Fatal(err)
	}
	if e.item(got.Item).Title != "合同请签字" {
		t.Fatal("rebuilt item differs")
	}
}

func code(err error) domain.Code {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.Code
	}
	return ""
}

func TestProposeUnjudgedRejections(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_t", "ticket", ev("t1", "ticket.opened", "规则已建议", nil))
	e.emit("src_m", "inbox", ev("m1", "mail.received", "未判定", nil))
	proposed, unjudged := e.events("src_t")[0], e.events("src_m")[0]
	before := len(e.allItems())

	if _, err := e.svc.ProposeUnjudged(bg, owner, proposed.ID); code(err) != domain.CodeConflict {
		t.Fatalf("not unjudged: %v", err)
	}
	if _, err := e.svc.ProposeUnjudged(bg, owner, "sev_missing"); code(err) != domain.CodeNotFound {
		t.Fatalf("missing: %v", err)
	}
	// item.create 对任何凭据开放：没有来源管理权的凭据不能留下事项。
	worker := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "w", Via: domain.ViaCLI}, Level: authz.LevelWorker}
	if _, err := e.svc.ProposeUnjudged(bg, worker, unjudged.ID); code(err) != domain.CodePermissionDenied {
		t.Fatalf("worker: %v", err)
	}
	if n := len(e.allItems()); n != before {
		t.Fatalf("rejected calls created items: %d -> %d", before, n)
	}
}

// 上次建好事项却没记完处置：重试接回已建事项，不建第二个；换一个身份重试也一样。
func TestProposeUnjudgedResumesAfterPartialFailure(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_m", "inbox", ev("m1", "mail.received", "未判定", nil))
	id := e.events("src_m")[0].ID
	var made struct{ ID string }
	_ = json.Unmarshal(e.must("item.create", map[string]any{"kind": "task", "title": "t", "ext": map[string]string{"source_event": id}}), &made)

	dlg := command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "d", Via: domain.ViaCLI}, Level: authz.LevelDelegate}
	got, err := e.svc.ProposeUnjudged(bg, dlg, id)
	if err != nil {
		t.Fatal(err)
	}
	if got.Item != made.ID || len(e.allItems()) != 1 {
		t.Fatalf("event %+v, %d items", got, len(e.allItems()))
	}
}

// 抽检"该看"依赖的起草：给被忽略的事件建 proposed 事项，不改事件处置；重试沿用已建事项，不重复建。
func TestDraftProposalIsIdempotentAndLeavesDispositionAlone(t *testing.T) {
	e := newEnv(t, false)
	e.emit("src_m", "inbox", ev("m1", "mail.received", "合同请签字", nil))
	before := e.events("src_m")[0]
	id := before.ID
	first, err := e.svc.DraftProposal(bg, owner, id)
	if err != nil || first == "" {
		t.Fatalf("draft: %q %v", first, err)
	}
	if it := e.item(first); it.Phase != domain.PhaseProposed || it.Title != "合同请签字" {
		t.Fatalf("item %+v", it)
	}
	if got := e.events("src_m")[0]; got.Disposition != before.Disposition || got.Item != "" {
		t.Fatalf("drafting changed the disposition: %+v", got)
	}
	n := len(e.allItems())
	if again, err := e.svc.DraftProposal(bg, owner, id); err != nil || again != first || len(e.allItems()) != n {
		t.Fatalf("retry: %q %v items %d→%d", again, err, n, len(e.allItems()))
	}
}
