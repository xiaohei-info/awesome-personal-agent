package delivery

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var ctx = context.Background()

type env struct {
	t   *testing.T
	s   *store.Store
	q   *Queue
	now time.Time
}

func newEnv(t *testing.T, o Options) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	s, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	e := &env{t: t, s: s, now: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	o.Now = func() time.Time { return e.now }
	e.q = New(o)
	return e
}

func (e *env) update(fn func(*store.Tx) error) {
	e.t.Helper()
	if err := e.s.Update(ctx, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) view(fn func(*store.Tx) error) {
	e.t.Helper()
	if err := e.s.View(ctx, fn); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) get(id string) (d domain.Delivery) {
	e.view(func(tx *store.Tx) (err error) { d, err = e.q.Get(tx, id); return })
	return
}

func (e *env) create(id, item, route string, version int64) domain.Delivery {
	var d domain.Delivery
	e.update(func(tx *store.Tx) (err error) {
		d, _, err = e.q.Create(tx, domain.Delivery{ID: id, Item: item, Class: domain.DeliveryDecision, Route: route, Version: version,
			Content: domain.DeliveryContent{Title: "pick one", Summary: "s", Options: []string{"a", "b"}, WebURL: "http://x/i/" + item}})
		return
	})
	return d
}

func (e *env) claim(route string) (d *domain.Delivery) {
	e.update(func(tx *store.Tx) (err error) { d, err = e.q.Claim(tx, route); return })
	return
}

func (e *env) ack(id string, v int64, ref string) (d domain.Delivery, r Result, err error) {
	_ = e.s.Update(ctx, func(tx *store.Tx) error { d, r, err = e.q.Ack(tx, id, v, ref); return nil })
	return
}

func (e *env) fail(id string, v int64) (d domain.Delivery, r Result, err error) {
	_ = e.s.Update(ctx, func(tx *store.Tx) error { d, r, err = e.q.Fail(tx, id, v, "boom"); return nil })
	return
}

func wantCode(t *testing.T, err error, code domain.Code) {
	t.Helper()
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != code {
		t.Fatalf("want %s, got %v", code, err)
	}
}

const ch = "channel:acc-chan"

// flow-channel-protocol：租约内独占，到期可重领且 id 不变；渠道由路由隔离。
func TestClaimLeaseAndChannelIsolation(t *testing.T) {
	e := newEnv(t, Options{Lease: time.Minute})
	created := e.create("d1", "i1", ch, 1)
	if created.State != domain.DeliveryPending || created.Content.Title != "pick one" || created.Version != 1 {
		t.Fatalf("%+v", created)
	}
	if e.claim("channel:acc-chan2") != nil {
		t.Fatal("跨渠道领取")
	}
	got := e.claim(ch)
	if got == nil || got.ID != "d1" || got.State != domain.DeliveryClaimed {
		t.Fatalf("%+v", got)
	}
	e.now = e.now.Add(30 * time.Second)
	if e.claim(ch) != nil {
		t.Fatal("租约内被第二个消费者领到")
	}
	e.now = e.now.Add(31 * time.Second)
	again := e.claim(ch)
	if again == nil || again.ID != "d1" {
		t.Fatalf("租约到期后应可再次领取: %+v", again)
	}
	e.view(func(tx *store.Tx) error {
		if _, err := e.q.Claim(tx, RouteInject); err == nil {
			t.Fatal("注入路由不可领取")
		}
		return nil
	})
}

func TestCreateIdempotentAndValidation(t *testing.T) {
	e := newEnv(t, Options{})
	e.create("d1", "i1", ch, 1)
	e.update(func(tx *store.Tx) error {
		d, created, err := e.q.Create(tx, domain.Delivery{ID: "d1", Item: "other", Class: domain.DeliveryBrief, Route: ch, Version: 9,
			Content: domain.DeliveryContent{Title: "changed"}})
		if err != nil || created || d.Item != "i1" || d.Version != 1 || d.Content.Title != "pick one" {
			t.Fatalf("重复创建应返回首次结果: %+v created=%v err=%v", d, created, err)
		}
		return nil
	})
	bad := []domain.Delivery{
		{Item: "i", Class: domain.DeliveryBrief, Route: ch, Version: 1, Content: domain.DeliveryContent{Title: "t"}},
		{ID: "x", Item: "i", Class: domain.DeliveryBrief, Route: ch, Version: 0, Content: domain.DeliveryContent{Title: "t"}},
		{ID: "x", Item: "i", Class: "nope", Route: ch, Version: 1, Content: domain.DeliveryContent{Title: "t"}},
		{ID: "x", Item: "i", Class: domain.DeliveryBrief, Route: ch, Version: 1},
	}
	for _, b := range bad {
		e.view(func(tx *store.Tx) error {
			_, _, err := e.q.Create(tx, b)
			wantCode(t, err, domain.CodeInvalidRequest)
			return nil
		})
	}
	e.view(func(tx *store.Tx) error {
		_, err := e.q.Get(tx, "missing")
		wantCode(t, err, domain.CodeNotFound)
		return nil
	})
}

// flow-channel-protocol：送达回报幂等，旧版本冲突并保持作废。
func TestAckIdempotentAndStaleVersion(t *testing.T) {
	e := newEnv(t, Options{})
	e.create("d1", "i1", ch, 2)
	e.claim(ch)
	d, r, err := e.ack("d1", 2, "msg-1")
	if err != nil || r != Applied || d.State != domain.DeliveryDelivered || d.ExternalRef != "msg-1" {
		t.Fatalf("%+v %v %v", d, r, err)
	}
	if _, r, err = e.ack("d1", 2, "msg-1"); err != nil || r != Duplicate {
		t.Fatalf("重复 ack 应幂等: %v %v", r, err)
	}
	_, _, err = e.ack("d1", 2, "msg-2")
	wantCode(t, err, domain.CodeConflict)
	if _, r, _ = e.ack("d1", 1, "msg-1"); r != Stale {
		t.Fatal("版本不符应为 Stale")
	}
	if _, _, err = e.ack("d1", 2, ""); err == nil {
		t.Fatal("缺 external_ref 应拒绝")
	}
	if _, _, err = e.ack("nope", 1, "r"); err == nil {
		t.Fatal("未知投递")
	}
}

// flow-channel-protocol：领取后事项变化，旧版本 ack 得到冲突，投递为 superseded 且不可再领。
func TestSupersedeThenStaleAck(t *testing.T) {
	e := newEnv(t, Options{})
	e.create("d1", "i1", ch, 1)
	e.create("d2", "i2", ch, 1)
	e.claim(ch)
	e.update(func(tx *store.Tx) error {
		n, err := e.q.Supersede(tx, "i1", 2)
		if n != 1 || err != nil {
			t.Fatalf("n=%d err=%v", n, err)
		}
		return nil
	})
	d, r, err := e.ack("d1", 1, "m")
	if err != nil || r != Stale || d.State != domain.DeliverySuperseded {
		t.Fatalf("%+v %v %v", d, r, err)
	}
	if _, r, _ = e.fail("d1", 1); r != Stale {
		t.Fatal("作废后 fail 应为 Stale")
	}
	if _, r, _ = confirm(e, "d1", 1); r != Stale {
		t.Fatal("作废后 confirm 应为 Stale")
	}
	if got := e.claim(ch); got == nil || got.ID != "d2" {
		t.Fatalf("旧版本不应再被领到: %+v", got)
	}
	e.update(func(tx *store.Tx) error {
		if n, _ := e.q.Supersede(tx, "i2", AllVersions); n != 1 {
			t.Fatal("AllVersions 应作废全部")
		}
		if n, _ := e.q.Supersede(tx, "i2", AllVersions); n != 0 {
			t.Fatal("终态不应再被作废")
		}
		return nil
	})
}

func confirm(e *env, id string, v int64) (d domain.Delivery, r Result, err error) {
	_ = e.s.Update(ctx, func(tx *store.Tx) error { d, r, err = e.q.Confirm(tx, id, v); return nil })
	return
}

// flow-channel-protocol：默认 1、3、7、15、30 分钟共 5 次退避，第 6 次失败转 fallback_web 且 Web 未读可见。
func TestFailBackoffThenFallbackWeb(t *testing.T) {
	e := newEnv(t, Options{})
	e.create("d1", "i1", ch, 1)
	for i, want := range []time.Duration{1, 3, 7, 15, 30} {
		want *= time.Minute
		if e.claim(ch) == nil {
			t.Fatalf("第 %d 次应可领取", i+1)
		}
		failedAt := e.now
		d, r, err := e.fail("d1", 1)
		if err != nil || r != Applied || d.State != domain.DeliveryPending || d.Attempts != i+1 {
			t.Fatalf("%d: %+v %v %v", i, d, r, err)
		}
		if !d.NextAttemptAt.Equal(failedAt.Add(want)) {
			t.Fatalf("第 %d 次失败后应 %v 重试，实际 %v", i+1, want, d.NextAttemptAt.Sub(failedAt))
		}
		if _, r, _ := e.fail("d1", 1); r != Duplicate {
			t.Fatal("退避中的重复 fail 应幂等")
		}
		e.now = e.now.Add(want - time.Second)
		if e.claim(ch) != nil {
			t.Fatalf("退避未到不应可领")
		}
		e.now = e.now.Add(time.Second)
	}
	e.claim(ch)
	d, _, err := e.fail("d1", 1)
	if err != nil || d.State != domain.DeliveryFallbackWeb || d.Attempts != 6 {
		t.Fatalf("%+v %v", d, err)
	}
	if e.claim(ch) != nil {
		t.Fatal("fallback_web 不应再被领取")
	}
	var unread []domain.Delivery
	e.view(func(tx *store.Tx) (err error) { unread, err = e.q.Unread(tx); return })
	if len(unread) != 1 || unread[0].ID != "d1" {
		t.Fatalf("Web 未读: %+v", unread)
	}
	if _, r, _ := e.fail("d1", 1); r != Duplicate {
		t.Fatal("fallback 后重复 fail 幂等")
	}
	e.update(func(tx *store.Tx) error { // Web 点击 = Confirm：fallback_web → acknowledged，幂等
		if d, r, err := e.q.Confirm(tx, "d1", 1); err != nil || r != Applied || d.State != domain.DeliveryAcknowledged {
			t.Fatalf("%+v %v %v", d, r, err)
		}
		_, r, err := e.q.Confirm(tx, "d1", 1)
		if err != nil || r != Duplicate {
			t.Fatalf("重复确认应幂等: %v %v", r, err)
		}
		return nil
	})
	e.view(func(tx *store.Tx) (err error) { unread, err = e.q.Unread(tx); return })
	if len(unread) != 0 {
		t.Fatal("已读后不应在未读里")
	}
	if d, _, err := e.fail("d1", 1); err == nil || d.State != domain.DeliveryAcknowledged {
		t.Fatalf("确认后 fail 不应推进: %+v %v", d, err)
	}
}

// 回答导致的作废只针对低版本：被回答的最新决策投递（含 fallback_web、已注入的）记 acknowledged。
func TestConfirmAnsweredSparesAnsweredVersionAndSupersedesOlder(t *testing.T) {
	e := newEnv(t, Options{})
	mk := func(id string, v int64, class domain.DeliveryClass, route string) {
		e.update(func(tx *store.Tx) error {
			_, _, err := e.q.Create(tx, domain.Delivery{ID: id, Item: "i1", Class: class, Route: route, Version: v, Content: domain.DeliveryContent{Title: "t"}})
			return err
		})
	}
	mk("old", 1, domain.DeliveryDecision, ch)
	mk("cur", 2, domain.DeliveryDecision, RouteWeb) // fallback_web
	mk("rem", 3, domain.DeliveryReminder, ch)       // 非决策类不被当作被回答的那条
	e.update(func(tx *store.Tx) error {
		ok, err := e.q.ConfirmAnswered(tx, "i1")
		if err != nil || !ok {
			t.Fatalf("%v %v", ok, err)
		}
		_, err = e.q.Supersede(tx, "i1", AllVersions)
		return err
	})
	for id, want := range map[string]domain.DeliveryState{"old": domain.DeliverySuperseded, "cur": domain.DeliveryAcknowledged, "rem": domain.DeliverySuperseded} {
		if got := e.get(id); got.State != want {
			t.Errorf("%s = %s, want %s", id, got.State, want)
		}
	}
	if d := e.get("cur"); d.State == domain.DeliveryAcknowledged {
		e.view(func(tx *store.Tx) error { // 无可确认的投递是正常情况
			if ok, err := e.q.ConfirmAnswered(tx, "i1"); ok || err != nil {
				t.Fatalf("%v %v", ok, err)
			}
			return nil
		})
	}
}

func TestFailConfigurableBackoffAndWrongState(t *testing.T) {
	e := newEnv(t, Options{Backoff: []time.Duration{time.Second}})
	e.create("d1", "i1", ch, 1)
	e.claim(ch)
	e.fail("d1", 1)
	e.now = e.now.Add(time.Second)
	e.claim(ch)
	if d, _, _ := e.fail("d1", 1); d.State != domain.DeliveryFallbackWeb {
		t.Fatalf("调小退避表后第 2 次失败应超限: %+v", d)
	}
	e.create("d3", "i3", ch, 1)
	e.claim(ch)
	e.ack("d3", 1, "m")
	_, _, err := e.fail("d3", 1)
	wantCode(t, err, domain.CodeConflict)
}

// flow-delivery-control：无人领取、以及领取后消费者一直不回报，最终都退回 Web 未读，投递不丢失。
func TestSweepUnclaimedAndAbandonedFallBackToWeb(t *testing.T) {
	e := newEnv(t, Options{UnclaimedAfter: 10 * time.Minute, Lease: time.Minute})
	e.create("abandoned", "i1", ch, 1)
	e.create("never", "i2", "channel:dead", 1)
	e.create("delivered", "i3", ch, 1)
	e.create("failedonce", "i4", "channel:gone", 1) // 失败一次后 ext 再不回来
	e.claim("channel:gone")
	e.fail("failedonce", 1)
	e.claim(ch) // abandoned：之后反复领取不回报
	e.claim(ch) // delivered
	e.ack("delivered", 1, "m")
	for i := 0; i < 4; i++ { // 租约内的 claimed 不受影响
		e.now = e.now.Add(2 * time.Minute)
		e.update(func(tx *store.Tx) error { _, err := e.q.Sweep(tx); return err })
		if e.get("abandoned").State != domain.DeliveryClaimed {
			t.Fatalf("未满 UnclaimedAfter 不应转 fallback: %s", e.get("abandoned").State)
		}
		e.claim(ch)
	}
	e.now = e.now.Add(3 * time.Minute)
	e.update(func(tx *store.Tx) error {
		if n, err := e.q.Sweep(tx); n != 3 || err != nil {
			t.Fatalf("n=%d err=%v", n, err)
		}
		return nil
	})
	if e.get("abandoned").State != domain.DeliveryFallbackWeb || e.get("never").State != domain.DeliveryFallbackWeb || e.get("failedonce").State != domain.DeliveryFallbackWeb ||
		e.get("delivered").State != domain.DeliveryDelivered {
		t.Fatalf("%s %s %s", e.get("abandoned").State, e.get("never").State, e.get("delivered").State)
	}
	var unread []domain.Delivery
	e.view(func(tx *store.Tx) (err error) { unread, err = e.q.Unread(tx); return })
	if len(unread) != 3 {
		t.Fatalf("Web 未读: %+v", unread)
	}
}

func TestAckAfterConfirmRecordsRef(t *testing.T) {
	e := newEnv(t, Options{})
	e.create("d1", "i1", ch, 1)
	e.claim(ch)
	confirm(e, "d1", 1)
	d, r, err := e.ack("d1", 1, "m1")
	if err != nil || r != Duplicate || d.ExternalRef != "m1" || e.get("d1").ExternalRef != "m1" {
		t.Fatalf("%+v %v %v", d, r, err)
	}
}

// flow-delivery-control：注入只记 injections，不等于送达；本人确认才 acknowledged，多会话重复不重复确认。
func TestInjectDoesNotAcknowledge(t *testing.T) {
	e := newEnv(t, Options{})
	e.create("d1", "i1", RouteInject, 1)
	e.create("d2", "i2", ch, 1)
	var inj []domain.Delivery
	e.view(func(tx *store.Tx) (err error) { inj, err = e.q.Injectable(tx); return })
	if len(inj) != 1 || inj[0].ID != "d1" {
		t.Fatalf("%+v", inj)
	}
	for _, sess := range []string{"s1", "s2", "s1"} {
		e.update(func(tx *store.Tx) error { _, err := e.q.Inject(tx, "d1", sess); return err })
	}
	d := e.get("d1")
	if d.State != domain.DeliveryPending || len(d.Injections) != 2 || d.Injections[0].Session != "s1" {
		t.Fatalf("注入只应记 injections: %+v", d)
	}
	d, r, err := confirm(e, "d1", 1)
	if err != nil || r != Applied || d.State != domain.DeliveryAcknowledged {
		t.Fatalf("%+v %v %v", d, r, err)
	}
	if _, r, _ = confirm(e, "d1", 1); r != Duplicate {
		t.Fatal("并发会话重复确认应幂等")
	}
	e.view(func(tx *store.Tx) error {
		_, err := e.q.Inject(tx, "d1", "s3")
		wantCode(t, err, domain.CodeConflict)
		if _, err := e.q.Inject(tx, "d1", ""); err == nil {
			t.Fatal("空会话")
		}
		if _, err := e.q.Inject(tx, "nope", "s"); err == nil {
			t.Fatal("未知投递")
		}
		return nil
	})
	e.view(func(tx *store.Tx) (err error) { inj, err = e.q.Injectable(tx); return })
	if len(inj) != 0 {
		t.Fatal("已确认的不应再注入")
	}
	if _, r, _ = confirm(e, "d2", 1); r != Applied {
		t.Fatal("渠道投递可由本人直接确认")
	}
	e.create("d5", "i5", ch, 1)
	e.now = e.now.Add(time.Hour)
	e.update(func(tx *store.Tx) error { _, err := e.q.Sweep(tx); return err })
	if d, r, err := confirm(e, "d5", 1); err != nil || r != Applied || d.State != domain.DeliveryAcknowledged {
		t.Fatalf("fallback_web 也可由本人确认: %+v %v %v", d, r, err)
	}
}

// 投递写在调用方事务内：事务回滚则投递一并回滚。
func TestRollback(t *testing.T) {
	e := newEnv(t, Options{})
	boom := errors.New("boom")
	err := e.s.Update(ctx, func(tx *store.Tx) error {
		if _, _, err := e.q.Create(tx, domain.Delivery{ID: "d1", Item: "i", Class: domain.DeliveryBrief, Route: ch, Version: 1,
			Content: domain.DeliveryContent{Title: "t"}}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	e.view(func(tx *store.Tx) error {
		_, err := e.q.Get(tx, "d1")
		wantCode(t, err, domain.CodeNotFound)
		return nil
	})
}

func TestWebRouteIsUnreadImmediately(t *testing.T) {
	e := newEnv(t, Options{})
	d := e.create("w1", "it", RouteWeb, 1)
	if d.State != domain.DeliveryFallbackWeb {
		t.Fatalf("state = %s", d.State)
	}
	if got := e.claim(RouteWeb); got != nil {
		t.Fatalf("nobody claims the web route: %+v", got)
	}
	var unread []domain.Delivery
	e.view(func(tx *store.Tx) (err error) { unread, err = e.q.Unread(tx); return })
	if len(unread) != 1 || unread[0].ID != "w1" {
		t.Fatalf("unread = %+v", unread)
	}
}

// 唤醒类投递带运行 id（§13.6）；其他投递不带。
func TestWakeDeliveryCarriesRunID(t *testing.T) {
	e := newEnv(t, Options{})
	e.update(func(tx *store.Tx) error {
		_, _, err := e.q.Create(tx, domain.Delivery{ID: "w1", Item: "i", Run: "run_1", Class: domain.DeliveryWake, Route: RouteWake, Version: 1, Content: domain.DeliveryContent{Title: "t"}})
		return err
	})
	if d := e.claim(RouteWake); d == nil || d.Run != "run_1" {
		t.Fatalf("claimed %+v", d)
	}
	if d := e.create("c1", "i", ChannelRoute("x"), 1); d.Run != "" {
		t.Fatalf("channel delivery has run %q", d.Run)
	}
}
