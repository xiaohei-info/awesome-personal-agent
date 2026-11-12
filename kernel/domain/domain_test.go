package domain

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestItemLifecycleEntryConstraints(t *testing.T) {
	ask := &Ask{AskInput: AskInput{Question: "合并吗？"}}
	wait := &WaitingOn{Kind: WaitingExecutor, Ref: "ex1"}
	cases := []struct {
		name     string
		item     Item
		to       Phase
		reason   string
		evidence bool
		wantErr  bool
	}{
		{"open 无约束", Item{Kind: KindTask}, PhaseOpen, "r", false, false},
		{"proposed 无约束", Item{Kind: KindTask}, PhaseProposed, "r", false, false},
		{"waiting 缺 waiting_on", Item{Kind: KindTask}, PhaseWaiting, "r", false, true},
		{"waiting 有 waiting_on", Item{Kind: KindTask, WaitingOn: wait}, PhaseWaiting, "r", false, false},
		{"needs_input 缺 ask", Item{Kind: KindTask}, PhaseNeedsInput, "r", false, true},
		{"needs_input 有 ask", Item{Kind: KindDecision, Ask: ask}, PhaseNeedsInput, "r", false, false},
		{"delegation done 无证据", Item{Kind: KindDelegation}, PhaseDone, "r", false, true},
		{"delegation done 有证据", Item{Kind: KindDelegation}, PhaseDone, "r", true, false},
		{"task done 无需证据", Item{Kind: KindTask}, PhaseDone, "r", false, false},
		{"自定义 kind done 无需证据", Item{Kind: "x.custom"}, PhaseDone, "r", false, false},
		{"重开 done→open", Item{Kind: KindTask, Phase: PhaseDone}, PhaseOpen, "reopen", false, false},
		{"缺 reason", Item{Kind: KindTask}, PhaseOpen, "", false, true},
		{"未知阶段", Item{Kind: KindTask}, Phase("busy"), "r", false, true},
	}
	for _, c := range cases {
		err := CheckEntry(c.item, c.to, c.reason, c.evidence)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err=%v wantErr=%v", c.name, err, c.wantErr)
		}
		if err != nil && (err.Code != CodeInvalidRequest || err.ExitCode() != ExitUsage) {
			t.Errorf("%s: 错误码映射 %v", c.name, err)
		}
	}
	if len(Phases) != 6 {
		t.Fatalf("阶段必须固定为六个，got %d", len(Phases))
	}
	if !PhaseDone.Terminal() || !PhaseDropped.Terminal() || PhaseOpen.Terminal() {
		t.Fatal("终态应只有 done、dropped")
	}
	if (AskOption{}).NextPhase() != PhaseOpen || (AskOption{Next: PhaseDone}).NextPhase() != PhaseDone {
		t.Fatal("选项下一阶段默认 open")
	}
}

// 内核维护字段不在调用方输入类型里：输入带 chain_depth 等被严格解码拒绝，输出类型仍序列化它们。
func TestLedgerKernelMaintainedFieldsNotWritable(t *testing.T) {
	for _, body := range []string{
		`{"kind":"task","title":"t","chain_depth":9}`,
		`{"kind":"task","title":"t","unattended":true}`,
		`{"kind":"task","title":"t","phase":"done"}`,
		`{"kind":"task","title":"t","version":3}`,
		`{"kind":"decision","title":"t","ask":{"question":"q","risk_class":"web_only"}}`,
		`{"kind":"decision","title":"t","ask":{"question":"q","screening_pending":true}}`,
	} {
		dec := json.NewDecoder(strings.NewReader(body))
		dec.DisallowUnknownFields()
		var in ItemInput
		if err := dec.Decode(&in); err == nil {
			t.Errorf("调用方输入不应接受内核维护字段: %s", body)
		}
	}
	it := Item{ID: "i1", Phase: PhaseNeedsInput, ItemKernelState: ItemKernelState{ChainDepth: 2, Unattended: true},
		Ask: &Ask{AskInput: AskInput{Question: "q"}, AskKernelState: AskKernelState{RiskClass: "web_only", ScreeningPending: true}}}
	b, _ := json.Marshal(it)
	for _, want := range []string{`"chain_depth":2`, `"unattended":true`, `"risk_class":"web_only"`, `"screening_pending":true`} {
		if !bytes.Contains(b, []byte(want)) {
			t.Errorf("Item 输出缺 %s: %s", want, b)
		}
	}
	var back Item
	if err := json.Unmarshal(b, &back); err != nil || back.Ask.RiskClass != "web_only" || back.ChainDepth != 2 {
		t.Fatalf("往返失败: %v %+v", err, back)
	}
}

func TestLedgerKindIsOpenString(t *testing.T) {
	var in ItemInput
	if err := json.Unmarshal([]byte(`{"kind":"insurance.renewal","title":"续保","spec":{"a":1},"ext":{"x":{"y":2}}}`), &in); err != nil {
		t.Fatal(err)
	}
	if in.Kind != "insurance.renewal" || string(in.Spec) != `{"a":1}` || string(in.Ext) != `{"x":{"y":2}}` {
		t.Fatalf("开放 kind 与 spec/ext 必须原样存取: %+v", in)
	}
}

func TestLedgerErrorContractMapping(t *testing.T) {
	cases := []struct {
		code Code
		http int
		exit int
	}{
		{CodeInvalidRequest, 400, 2},
		{CodeUnauthenticated, 401, 3},
		{CodePermissionDenied, 403, 4},
		{CodeNotFound, 404, 5},
		{CodeConflict, 409, 6},
		{CodeWebOnly, 403, 20},
		{CodeExecutorNotAllowed, 403, 21},
		{CodeRiskScreeningPending, 403, 22},
		{CodeBasisRequired, 403, 23},
		{CodeInternal, 500, 1},
		{Code("never_registered"), 500, 1},
	}
	for _, c := range cases {
		if c.code.HTTPStatus() != c.http || c.code.ExitCode() != c.exit {
			t.Errorf("%s: http=%d exit=%d", c.code, c.code.HTTPStatus(), c.code.ExitCode())
		}
	}
	if CodeFromHTTP(404) != CodeNotFound || CodeFromHTTP(403) != CodePermissionDenied || CodeFromHTTP(502) != CodeInternal {
		t.Fatal("CodeFromHTTP")
	}
	if ExitUnreachable != 7 || ExitAgentsStale != 10 || ExitAgentsDrift != 11 {
		t.Fatal("非错误响应的退出码常量")
	}
}

// 附录 B.3 的示例响应必须能解码并往返，web_url 为 null 时仍输出该字段。
func TestLedgerErrorResponseShape(t *testing.T) {
	raw := `{"code":"executor_not_allowed","why":"执行者不在项目允许范围内","next":"改选 allowed 中的执行者，或用 apa ask 交给本人","allowed":["work-host.codex"],"web_url":null}`
	var e Error
	if err := json.Unmarshal([]byte(raw), &e); err != nil {
		t.Fatal(err)
	}
	if e.ExitCode() != 21 || e.HTTPStatus() != 403 || len(e.Allowed) != 1 || e.WebURL != nil {
		t.Fatalf("%+v", e)
	}
	b, _ := json.Marshal(&e)
	if string(b) != raw {
		t.Fatalf("往返不一致:\n%s\n%s", b, raw)
	}
	var err error = &e
	if !strings.Contains(err.Error(), "executor_not_allowed") {
		t.Fatal("Error() 应含 code")
	}
}

func TestEventSubscribeRegisteredTypes(t *testing.T) {
	want := strings.Fields(`item.created item.updated item.phase_changed item.deadline_passed decision.requested decision.resolved
delegation.created delegation.reported command.rejected material.revised material.revision_proposed
guideline.activated project.updated project.allowed_executors_changed rule.mode_changed allowlist.changed
policy.changed grant.issued grant.revoked run.started run.finished source.failed health.alert item.progress_added proposal.attached`)
	if strings.Join(RegisteredEventTypes, " ") != strings.Join(want, " ") {
		t.Fatalf("登记事件类型与附录 B.4 不一致: %v", RegisteredEventTypes)
	}
	for _, ty := range want {
		if !IsRegisteredEventType(ty) {
			t.Errorf("%s 应已登记", ty)
		}
	}
	for _, ty := range []string{"", "item.deleted", "internal.rule_fired", "ITEM.CREATED"} {
		if IsRegisteredEventType(ty) {
			t.Errorf("%q 不应对外开放", ty)
		}
	}
}

func TestEventShapeSupportsReplayAndActorBadge(t *testing.T) {
	ev := Event{Seq: 7, Type: EvItemPhaseChanged, Object: "i1",
		Actor:   Actor{Type: ActorSystem, Principal: "clock", Via: ViaClock},
		Payload: json.RawMessage(`{"from":"waiting","to":"open"}`), Basis: &Basis{PolicyVersion: 3, RuleID: "r1"}}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.Seq != 7 || back.Actor.Via != ViaClock || back.Basis.RuleID != "r1" || string(back.Payload) != `{"from":"waiting","to":"open"}` {
		t.Fatalf("%+v", back)
	}
}

func TestDeliveryStateMachine(t *testing.T) {
	ok := [][2]DeliveryState{
		{DeliveryPending, DeliveryClaimed}, {DeliveryClaimed, DeliveryDelivered}, {DeliveryDelivered, DeliveryAcknowledged},
		{DeliveryClaimed, DeliveryPending}, {DeliveryPending, DeliveryFallbackWeb}, {DeliveryDelivered, DeliverySuperseded},
	}
	for _, p := range ok {
		if !p[0].CanTransition(p[1]) {
			t.Errorf("%s→%s 应允许", p[0], p[1])
		}
	}
	bad := [][2]DeliveryState{
		{DeliveryPending, DeliveryAcknowledged}, // 不能跳过送达
		{DeliveryAcknowledged, DeliverySuperseded},
		{DeliverySuperseded, DeliveryPending},
		{DeliveryFallbackWeb, DeliveryClaimed},
		{DeliveryDelivered, DeliveryPending},
	}
	for _, p := range bad {
		if p[0].CanTransition(p[1]) {
			t.Errorf("%s→%s 不应允许", p[0], p[1])
		}
	}
}

func TestRunStateMachine(t *testing.T) {
	if !RunQueued.CanTransition(RunRunning) || !RunRunning.CanTransition(RunTimeout) || !RunQueued.CanTransition(RunFailed) {
		t.Fatal("合法推进被拒绝")
	}
	if RunQueued.CanTransition(RunSucceeded) || RunSucceeded.CanTransition(RunRunning) || RunTimeout.CanTransition(RunFailed) {
		t.Fatal("非法推进被放行")
	}
	if !RunFailed.Terminal() || RunRunning.Terminal() {
		t.Fatal("Terminal")
	}
}

func TestProjectAllowedExecutors(t *testing.T) {
	cases := []struct {
		name    string
		allowed []string
		exec    string
		want    bool
	}{
		{"未设置不限制", nil, "any", true},
		{"空列表只能本人", []string{}, "any", false},
		{"空列表本人可以", []string{}, "me", true},
		{"在名单内", []string{"a", "b"}, "b", true},
		{"名单外", []string{"a", "b"}, "c", false},
	}
	for _, c := range cases {
		if got := (Project{AllowedExecutors: c.allowed}).ExecutorAllowed(c.exec, "me"); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
	// nil 与空列表在 JSON 里必须可区分
	var p Project
	if err := json.Unmarshal([]byte(`{"allowed_executors":[]}`), &p); err != nil || p.AllowedExecutors == nil {
		t.Fatal("空列表不能退化成 nil")
	}
}

func TestSourceEventDedupeAndTrust(t *testing.T) {
	if k := (Source{ID: "s1"}).DedupeKeyFor("e1"); k != (DedupeKey{"s1", "e1"}) {
		t.Fatalf("未登记去重域应用 source id: %+v", k)
	}
	if k := (Source{ID: "s1", DedupeDomain: "mail"}).DedupeKeyFor("e1"); k.Domain != "mail" {
		t.Fatalf("%+v", k)
	}
	for _, e := range []SourceEvent{{}, {Sender: "a"}, {SenderVerified: true}} {
		if e.TrustedSender() {
			t.Errorf("缺失或未核实应不可信: %+v", e)
		}
	}
	if !(SourceEvent{Sender: "a", SenderVerified: true}).TrustedSender() {
		t.Fatal("已核实应可信")
	}
}

func TestSmallEnums(t *testing.T) {
	if !PriorityHigh.Valid() || Priority("x").Valid() || !WaitingTime.Valid() || WaitingKind("x").Valid() {
		t.Fatal("枚举校验")
	}
	_ = Timer{At: time.Now()}
}
