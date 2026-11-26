package inbound

import (
	"context"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
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
