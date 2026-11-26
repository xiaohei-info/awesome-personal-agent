package inbound

import (
	"context"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// B4 验收：有输出时产生一条知会并按矩阵 source_result 行路由；空输出时用固定说明；过长时会截断。
func TestBatchResultNotificationAndDeliveryRouting(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	_, _ = registerBatchAgent(t, e, policy.SourcePipeline{BatchSize: 1})
	att := attention.New(attention.Options{
		Queue:  e.queue,
		WebURL: func(id string) string { return "http://localhost/items/" + id },
	})
	e.svc.o.Batch.Notify = func(ctx context.Context, run domain.Run, summary string) error {
		return e.st.Update(ctx, func(tx *store.Tx) error { return att.SourceResult(tx, run.ID, summary) })
	}

	// 1) 正常输出：产生一条投递，默认路由推给 Agent（主会话未配置时退到 next_chat 注入）且不推本人
	enqueueEvent(t, e, "ev_b1", "normal", "body", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	r1 := latestBatchRun(t, e)
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunSucceeded), r1.Run.ID)
		return err
	})
	r1.Run.State = domain.RunSucceeded
	e.svc.SettleBatch(bg, proc.Result{Run: r1.Run, Stdout: []byte("处理了 3 条，新建 1 条建议。")})

	var deliveries []domain.Delivery
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT id, route, content FROM delivery_delivery WHERE id LIKE 'batch-result:%' ORDER BY rowid`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var id, route, content string
			if err := r.Scan(&id, &route, &content); err != nil {
				return err
			}
			deliveries = append(deliveries, domain.Delivery{ID: id, Route: route})
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(deliveries) != 1 {
		t.Fatalf("expected 1 delivery, got %d: %+v", len(deliveries), deliveries)
	}
	if deliveries[0].ID != attention.BatchResultID(r1.Run.ID) {
		t.Fatalf("unexpected delivery id: %s", deliveries[0].ID)
	}
	// 主会话未配置 MainAgentID 时，MainRoute 返回 blocked，按 Else=next_chat 退到注入路由
	if deliveries[0].Route != delivery.RouteInject && deliveries[0].Route != delivery.RouteWake {
		t.Fatalf("expected delivery to agent route (inject or wake), got %q", deliveries[0].Route)
	}

	// 2) 空输出：SettleBatch 用固定说明替代
	var emptySummary string
	e.svc.o.Batch.Notify = func(_ context.Context, _ domain.Run, summary string) error {
		emptySummary = summary
		return nil
	}
	enqueueEvent(t, e, "ev_b2", "normal", "body", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	r2 := latestBatchRun(t, e)
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunSucceeded), r2.Run.ID)
		return err
	})
	r2.Run.State = domain.RunSucceeded
	e.svc.SettleBatch(bg, proc.Result{Run: r2.Run, Stdout: nil})
	if emptySummary != EmptyBatchOutputNotice {
		t.Fatalf("empty stdout should use fixed notice, got %q", emptySummary)
	}

	// 3) 超长摘要按 2000 字截断
	long := make([]rune, 2500)
	for i := range long {
		long[i] = '字'
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		return att.SourceResult(tx, "run_long", string(long))
	}); err != nil {
		t.Fatal(err)
	}
	var body string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT content FROM attention_held WHERE id = ?`, attention.BatchResultID("run_long"))
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			return r.Scan(&body)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len([]rune(body)) > 2050 {
		t.Fatalf("summary should be truncated around 2000 runes, got %d", len([]rune(body)))
	}
}

// FIX-3：stream-json 输出包含工具调用、工具返回、事件 id、原文正文与启动日志时，
// 推给主会话的投递内容只能包含执行器声明提取出的最终助手文本，绝不能泄漏任何工具记录、日志或事件 id。
func TestBatchResultFiltersStreamJSONToolsAndLogs(t *testing.T) {
	e := setupUnifiedJudgeEnv(t, nil, nil, "", "")
	_, _ = registerBatchAgent(t, e, policy.SourcePipeline{BatchSize: 1})
	// 覆盖声明为 stream-json + final_text，贴合实际执行器声明。
	e.must("executor.register", executors.Declaration{
		Harness: "stub-harness",
		Headless: &executors.Headless{
			Command:   []string{"/bin/echo", "{prompt}"},
			Output:    []string{"--format", "stream-json"},
			FinalText: "$.text",
			Session:   executors.Session{Extract: "$.session_id"},
			Access:    map[string][]string{"read": {}, "edit": {}},
		},
	})
	att := attention.New(attention.Options{
		Queue:  e.queue,
		WebURL: func(id string) string { return "http://localhost/items/" + id },
	})
	e.svc.o.Batch.Notify = func(ctx context.Context, run domain.Run, summary string) error {
		return e.st.Update(ctx, func(tx *store.Tx) error { return att.SourceResult(tx, run.ID, summary) })
	}

	streamFixture := strings.Join([]string{
		`Warning: startup diagnostic log should never reach main session`,
		`{"type":"system","subtype":"init","model":"test-model","session_id":"sess-01","timestamp":1000}`,
		`{"type":"tool_use","name":"apa_source_event","tool_call_id":"call_1","input":{"id":"sev_secret_999"},"timestamp":1001}`,
		`{"type":"tool_result","name":"apa_source_event","tool_call_id":"call_1","output":"{\"id\":\"sev_secret_999\",\"detail\":\"CONFIDENTIAL-RAW-BODY\"}","duration_ms":42,"is_error":false,"timestamp":1002}`,
		`{"type":"text","text":"本批共处理 1 条，","timestamp":1003}`,
		`{"type":"text","text":"已为法务合同评审邮件新建待确认建议。","timestamp":1004}`,
		`{"type":"result","session_id":"sess-01","exit_code":0,"text":"本批共处理 1 条，已为法务合同评审邮件新建待确认建议。","tokens":{"total":320},"duration_ms":980,"timestamp":1005}`,
	}, "\n") + "\n"

	enqueueEvent(t, e, "ev_sj1", "normal", "CONFIDENTIAL-RAW-BODY", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	r1 := latestBatchRun(t, e)
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunSucceeded), r1.Run.ID)
		return err
	})
	r1.Run.State = domain.RunSucceeded
	e.svc.SettleBatch(bg, proc.Result{Run: r1.Run, Stdout: []byte(streamFixture)})

	var content string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT content FROM delivery_delivery WHERE id = ?`, attention.BatchResultID(r1.Run.ID))
		if err != nil {
			return err
		}
		defer r.Close()
		if !r.Next() {
			t.Fatalf("missing delivery for %s", r1.Run.ID)
		}
		return r.Scan(&content)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "本批共处理 1 条，已为法务合同评审邮件新建待确认建议。") {
		t.Fatalf("delivery missing final assistant text: %s", content)
	}
	for _, forbidden := range []string{"tool_use", "tool_result", "apa_source_event", "sev_secret_999", "CONFIDENTIAL-RAW-BODY", "startup diagnostic log", "session_id"} {
		if strings.Contains(content, forbidden) {
			t.Fatalf("delivery leaked %q: %s", forbidden, content)
		}
	}

	// 只有工具事件和诊断日志、没有最终助手文本时，退回固定空输出说明，绝不把日志或工具记录送进主会话。
	toolOnly := strings.Join([]string{
		`Warning: startup diagnostic log`,
		`{"type":"tool_use","name":"apa_source_event","input":{"id":"sev_secret_999"}}`,
		`{"type":"result","session_id":"sess-02","exit_code":0,"text":""}`,
	}, "\n") + "\n"
	enqueueEvent(t, e, "ev_sj2", "normal", "CONFIDENTIAL-RAW-BODY", e.now)
	if err := e.svc.TickQueue(bg); err != nil {
		t.Fatal(err)
	}
	r2 := latestBatchRun(t, e)
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE outcall_run SET state = ? WHERE id = ?`, string(domain.RunSucceeded), r2.Run.ID)
		return err
	})
	r2.Run.State = domain.RunSucceeded
	e.svc.SettleBatch(bg, proc.Result{Run: r2.Run, Stdout: []byte(toolOnly)})
	var content2 string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT content FROM delivery_delivery WHERE id = ?`, attention.BatchResultID(r2.Run.ID))
		if err != nil {
			return err
		}
		defer r.Close()
		if !r.Next() {
			t.Fatalf("missing delivery for %s", r2.Run.ID)
		}
		return r.Scan(&content2)
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content2, EmptyBatchOutputNotice) || strings.Contains(content2, "sev_secret_999") || strings.Contains(content2, "diagnostic") {
		t.Fatalf("tool-only output should fall back to fixed notice without leaking: %s", content2)
	}
}
