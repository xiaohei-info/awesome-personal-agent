package deliveries

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apidecisions "github.com/xiaohei-info/awesome-personal-agent/api/decisions"
	apiitems "github.com/xiaohei-info/awesome-personal-agent/api/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	kdec "github.com/xiaohei-info/awesome-personal-agent/kernel/decisions"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	kitems "github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

const ownerPW = "correct horse battery"

var bg = context.Background()

type env struct {
	t      *testing.T
	st     *store.Store
	auth   *auth.Auth
	q      *delivery.Queue
	srv    *core.Server
	cookie *http.Cookie
	csrf   string

	mu  sync.Mutex
	now time.Time // 队列的时钟，测试里手动推进租约与退避
}

func (e *env) clock() time.Time { e.mu.Lock(); defer e.mu.Unlock(); return e.now }
func (e *env) advance(d time.Duration) {
	e.mu.Lock()
	e.now = e.now.Add(d)
	e.mu.Unlock()
}

// setup 装配真实账本、命令执行器、事项与决策命令、投递队列和 HTTP 服务，并以本人口令登录一个 Web 会话。
func setup(t *testing.T, o delivery.Options) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, kitems.Module, delivery.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, auth: auth.New(nil), now: time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)}
	o.Now = e.clock
	e.q = delivery.New(o)
	x := command.New(st, time.Now)
	if err := kitems.Register(x, kitems.NewKinds()); err != nil {
		t.Fatal(err)
	}
	if err := kdec.Register(x); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(bg, func(tx *store.Tx) error { return core.SetOwnerPassword(tx, ownerPW) }); err != nil {
		t.Fatal(err)
	}
	e.srv, err = core.New(core.Deps{Store: st, Auth: e.auth,
		Groups: []core.RouteGroup{apiitems.Routes(x, st), apidecisions.Routes(x, "http://apa.example"), Routes(st, e.q, 5*time.Millisecond)}})
	if err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/v1/auth/login", `{"password":"`+ownerPW+`"}`, nil)
	if rec.Code != 200 {
		t.Fatalf("login: %d %s", rec.Code, rec.Body)
	}
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	for _, c := range rec.Result().Cookies() {
		if c.Name == core.SessionCookie {
			e.cookie = c
		}
	}
	e.csrf = out.CSRF
	return e
}

func (e *env) do(method, path, body string, mod func(*http.Request)) *httptest.ResponseRecorder {
	e.t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:5000"
	if mod != nil {
		mod(req)
	}
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) web(r *http.Request) {
	r.AddCookie(e.cookie)
	r.Header.Set(core.CSRFHeader, e.csrf)
}

// key 签发凭据并返回它的 Bearer 修饰函数与凭据 id。
func (e *env) key(spec auth.Spec) (func(*http.Request), string) {
	e.t.Helper()
	var secret, id string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		c, s, err := e.auth.Issue(tx, spec)
		secret, id = s, c.ID
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+secret) }, id
}

func (e *env) chan_(name string) func(*http.Request) {
	m, _ := e.key(auth.Spec{Kind: auth.Ext, Scope: ChannelScopePrefix + name})
	return m
}

func (e *env) enqueue(id, route string, version int64) {
	e.t.Helper()
	d := domain.Delivery{ID: id, Item: "item-" + id, Class: domain.DeliveryDecision, Route: route, Version: version,
		Content: domain.DeliveryContent{Title: "ship it?", Summary: "s", Options: []string{"yes", "no"}, WebURL: "http://apa/items/x"}}
	if err := e.st.Update(bg, func(tx *store.Tx) error { _, _, err := e.q.Create(tx, d); return err }); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) state(id string) domain.Delivery {
	e.t.Helper()
	var d domain.Delivery
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { d, err = e.q.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return d
}

func codeOf(t *testing.T, rec *httptest.ResponseRecorder) domain.Code {
	t.Helper()
	var de domain.Error
	if err := json.Unmarshal(rec.Body.Bytes(), &de); err != nil || de.Code == "" {
		t.Fatalf("not an error body: %d %s", rec.Code, rec.Body)
	}
	return de.Code
}

func decode(t *testing.T, rec *httptest.ResponseRecorder) domain.Delivery {
	t.Helper()
	var d domain.Delivery
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &d) != nil {
		t.Fatalf("expected a delivery: %d %s", rec.Code, rec.Body)
	}
	return d
}

// ACT-DLV-01：渠道 ext 领到的投递带标题、摘要、选项、Web 链接和版本号。
func TestNextReturnsDeliveryWithContract(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("d1", delivery.ChannelRoute("acc-chan"), 3)
	rec := e.do("GET", "/v1/deliveries/next?wait=1", "", e.chan_("acc-chan"))
	d := decode(t, rec)
	if d.ID != "d1" || d.Class != domain.DeliveryDecision || d.Version != 3 || d.State != domain.DeliveryClaimed ||
		d.Content.Title != "ship it?" || d.Content.Summary != "s" || len(d.Content.Options) != 2 || d.Content.WebURL == "" {
		t.Fatalf("delivery = %+v", d)
	}
}

// 没有可领投递：不带 wait 立即 204；带 wait 等到期后 204；等待期间入队的投递被领到。
func TestNextLongPoll(t *testing.T) {
	e := setup(t, delivery.Options{})
	ch := e.chan_("acc-chan")
	if rec := e.do("GET", "/v1/deliveries/next", "", ch); rec.Code != http.StatusNoContent {
		t.Fatalf("empty queue: %d %s", rec.Code, rec.Body)
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		e.enqueue("late", delivery.ChannelRoute("acc-chan"), 1)
	}()
	start := time.Now()
	if d := decode(t, e.do("GET", "/v1/deliveries/next?wait=5", "", ch)); d.ID != "late" {
		t.Fatalf("got %s", d.ID)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("long poll did not return as soon as a delivery arrived")
	}
	for _, bad := range []string{"abc", "-1"} {
		if rec := e.do("GET", "/v1/deliveries/next?wait="+bad, "", ch); rec.Code != 400 {
			t.Fatalf("wait=%s: %d", bad, rec.Code)
		}
	}
}

// ACT-CHP-01：租约内其他消费者领不到，到期后同一条可再次领取。
func TestLeaseBlocksOthersThenExpires(t *testing.T) {
	e := setup(t, delivery.Options{Lease: time.Minute})
	e.enqueue("d1", delivery.ChannelRoute("c"), 1)
	a, b := e.chan_("c"), e.chan_("c")
	decode(t, e.do("GET", "/v1/deliveries/next", "", a))
	if rec := e.do("GET", "/v1/deliveries/next", "", b); rec.Code != http.StatusNoContent {
		t.Fatalf("second consumer inside the lease: %d", rec.Code)
	}
	e.advance(2 * time.Minute)
	if d := decode(t, e.do("GET", "/v1/deliveries/next", "", b)); d.ID != "d1" {
		t.Fatalf("after lease: %s", d.ID)
	}
}

// ACT-CHP-02：渠道由密钥确定，别的渠道的密钥领不到，也不能回报别人的投递。
func TestChannelIsDecidedByKey(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("d1", delivery.ChannelRoute("acc-chan"), 1)
	other := e.chan_("acc-chan2")
	if rec := e.do("GET", "/v1/deliveries/next", "", other); rec.Code != http.StatusNoContent {
		t.Fatalf("cross-channel claim: %d %s", rec.Code, rec.Body)
	}
	rec := e.do("POST", "/v1/deliveries/d1/ack", `{"version":1,"external_ref":"m1"}`, other)
	if rec.Code != 404 || e.state("d1").State != domain.DeliveryPending {
		t.Fatalf("cross-channel ack: %d %s state=%s", rec.Code, rec.Body, e.state("d1").State)
	}
	if rec := e.do("POST", "/v1/deliveries/d1/fail", `{"version":1,"reason":"x"}`, other); rec.Code != 404 {
		t.Fatalf("cross-channel fail: %d", rec.Code)
	}
}

// 网关插件 token（worker 及以上）只领唤醒类投递；observer 不行。
func TestGatewayTokenClaimsOnlyWake(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("ch", delivery.ChannelRoute("c"), 1)
	e.enqueue("wk", delivery.RouteWake, 1)
	for _, k := range []auth.Spec{{Kind: auth.Worker}, {Kind: auth.Delegate}} {
		m, _ := e.key(k)
		e.enqueue("wk-"+string(k.Kind), delivery.RouteWake, 1)
		if d := decode(t, e.do("GET", "/v1/deliveries/next", "", m)); d.Route != delivery.RouteWake {
			t.Fatalf("%s claimed route %s", k.Kind, d.Route)
		}
	}
	m, _ := e.key(auth.Spec{Kind: auth.Worker})
	if rec := e.do("POST", "/v1/deliveries/ch/ack", `{"version":1,"external_ref":"m"}`, m); rec.Code != 404 {
		t.Fatalf("token acked a channel delivery: %d", rec.Code)
	}
	obs, _ := e.key(auth.Spec{Kind: auth.Observer})
	if rec := e.do("GET", "/v1/deliveries/next", "", obs); rec.Code != 403 {
		t.Fatalf("observer: %d", rec.Code)
	}
}

// ack：送达，同 ref 重复回报幂等；ref 缺失 400；不同 ref 409。
func TestAckDeliversAndIsIdempotent(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("d1", delivery.ChannelRoute("c"), 2)
	ch := e.chan_("c")
	decode(t, e.do("GET", "/v1/deliveries/next", "", ch))
	if rec := e.do("POST", "/v1/deliveries/d1/ack", `{"version":2}`, ch); rec.Code != 400 {
		t.Fatalf("missing ref: %d", rec.Code)
	}
	if rec := e.do("POST", "/v1/deliveries/d1/ack", `{"external_ref":"m"}`, ch); rec.Code != 400 {
		t.Fatalf("missing version: %d", rec.Code)
	}
	for i := 0; i < 2; i++ {
		d := decode(t, e.do("POST", "/v1/deliveries/d1/ack", `{"version":2,"external_ref":"m1"}`, ch))
		if d.State != domain.DeliveryDelivered || d.ExternalRef != "m1" {
			t.Fatalf("ack #%d: %+v", i, d)
		}
	}
	if rec := e.do("POST", "/v1/deliveries/d1/ack", `{"version":2,"external_ref":"other"}`, ch); rec.Code != 409 {
		t.Fatalf("different ref: %d", rec.Code)
	}
	if rec := e.do("POST", "/v1/deliveries/nope/ack", `{"version":1,"external_ref":"m"}`, ch); rec.Code != 404 {
		t.Fatalf("unknown delivery: %d", rec.Code)
	}
}

// ACT-CHP-03：事项变化后旧版本 ack 返回 409，投递是 superseded，不会被送达。
func TestAckWithStaleVersionConflicts(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("d1", delivery.ChannelRoute("c"), 1)
	ch := e.chan_("c")
	decode(t, e.do("GET", "/v1/deliveries/next", "", ch))
	if err := e.st.Update(bg, func(tx *store.Tx) error { _, err := e.q.Supersede(tx, "item-d1", 2); return err }); err != nil {
		t.Fatal(err)
	}
	rec := e.do("POST", "/v1/deliveries/d1/ack", `{"version":1,"external_ref":"m1"}`, ch)
	if rec.Code != 409 || codeOf(t, rec) != domain.CodeConflict {
		t.Fatalf("stale ack: %d %s", rec.Code, rec.Body)
	}
	if got := e.state("d1"); got.State != domain.DeliverySuperseded || got.ExternalRef != "" {
		t.Fatalf("after stale ack: %+v", got)
	}
	if rec := e.do("POST", "/v1/deliveries/d1/fail", `{"version":1,"reason":"x"}`, ch); rec.Code != 409 {
		t.Fatalf("stale fail: %d", rec.Code)
	}
}

// ACT-CHP-04：失败回报按 1、3、7、15、30 分钟退避，第 6 次转 fallback_web 并出现在 Web 未读。
func TestFailBacksOffThenFallsBackToWeb(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("d1", delivery.ChannelRoute("c"), 1)
	ch := e.chan_("c")
	for i, wait := range []time.Duration{1, 3, 7, 15, 30} {
		wait *= time.Minute
		decode(t, e.do("GET", "/v1/deliveries/next", "", ch))
		d := decode(t, e.do("POST", "/v1/deliveries/d1/fail", `{"version":1,"reason":"offline"}`, ch))
		if d.State != domain.DeliveryPending || d.Attempts != i+1 {
			t.Fatalf("fail #%d: %+v", i+1, d)
		}
		e.advance(wait - time.Second)
		if rec := e.do("GET", "/v1/deliveries/next", "", ch); rec.Code != http.StatusNoContent {
			t.Fatalf("claimable %v early: %d", wait, rec.Code)
		}
		e.advance(time.Second)
	}
	decode(t, e.do("GET", "/v1/deliveries/next", "", ch))
	if d := decode(t, e.do("POST", "/v1/deliveries/d1/fail", `{"version":1,"reason":"offline"}`, ch)); d.State != domain.DeliveryFallbackWeb {
		t.Fatalf("after the 5 retries: %+v", d)
	}
	var out struct{ Deliveries []domain.Delivery }
	rec := e.do("GET", "/v1/deliveries/unread", "", e.web)
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Deliveries) != 1 || out.Deliveries[0].ID != "d1" {
		t.Fatalf("unread: %d %s", rec.Code, rec.Body)
	}
}

// Web 未读与已读：只有 Web 登录；读过后不再出现；不是未读的投递 409。
func TestUnreadAndReadAreWebOnly(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("w1", delivery.RouteWeb, 1)
	e.enqueue("p1", delivery.ChannelRoute("c"), 1)
	for _, mod := range []func(*http.Request){e.chan_("c"), func(*http.Request) {}} {
		if rec := e.do("GET", "/v1/deliveries/unread", "", mod); rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("non-web unread: %d", rec.Code)
		}
	}
	if rec := e.do("POST", "/v1/deliveries/w1/read", "", e.chan_("c")); rec.Code != 403 {
		t.Fatalf("ext read: %d", rec.Code)
	}
	unread := func() []domain.Delivery {
		var out struct{ Deliveries []domain.Delivery }
		rec := e.do("GET", "/v1/deliveries/unread", "", e.web)
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != 200 {
			t.Fatalf("unread: %d %s", rec.Code, rec.Body)
		}
		return out.Deliveries
	}
	if u := unread(); len(u) != 1 || u[0].ID != "w1" {
		t.Fatalf("unread = %+v", u)
	}
	for i := 0; i < 2; i++ { // 幂等
		if rec := e.do("POST", "/v1/deliveries/w1/read", "", e.web); rec.Code != 200 {
			t.Fatalf("read #%d: %d %s", i, rec.Code, rec.Body)
		}
	}
	if u := unread(); len(u) != 0 {
		t.Fatalf("after read: %+v", u)
	}
	if rec := e.do("POST", "/v1/deliveries/p1/read", "", e.web); rec.Code != 200 { // 点击即确认，不论投递处于哪个未作废状态
		t.Fatalf("read of a pending delivery: %d", rec.Code)
	}
	if rec := e.do("POST", "/v1/deliveries/nope/read", "", e.web); rec.Code != 404 {
		t.Fatalf("read of unknown: %d", rec.Code)
	}
	// Web 登录自己不领取、不回报：投递协议只给消费方。
	if rec := e.do("GET", "/v1/deliveries/next", "", e.web); rec.Code != 403 {
		t.Fatalf("web claim: %d", rec.Code)
	}
}

// ACT-CHP-08：吊销立即生效，领取和回报都回 401；重新签发后恢复。
func TestRevokedKeyIsRejectedImmediately(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("d1", delivery.ChannelRoute("c"), 1)
	ch, id := e.key(auth.Spec{Kind: auth.Ext, Scope: ChannelScopePrefix + "c"})
	decode(t, e.do("GET", "/v1/deliveries/next", "", ch))
	if err := e.st.Update(bg, func(tx *store.Tx) error { return e.auth.Revoke(tx, id) }); err != nil {
		t.Fatal(err)
	}
	if rec := e.do("GET", "/v1/deliveries/next", "", ch); rec.Code != 401 {
		t.Fatalf("claim after revoke: %d", rec.Code)
	}
	if rec := e.do("POST", "/v1/deliveries/d1/ack", `{"version":1,"external_ref":"m"}`, ch); rec.Code != 401 {
		t.Fatalf("ack after revoke: %d", rec.Code)
	}
	if e.state("d1").State != domain.DeliveryClaimed {
		t.Fatal("a revoked key changed delivery state")
	}
	if rec := e.do("POST", "/v1/deliveries/d1/ack", `{"version":1,"external_ref":"m"}`, e.chan_("c")); rec.Code != 200 {
		t.Fatalf("re-issued key: %d %s", rec.Code, rec.Body)
	}
}

// ACT-CHP-09 / ACT-EXP-02：渠道密钥读不了、写不了账本；导出与转发 ext 密钥、webhook 密钥碰不到投递协议。
func TestExtKeysStayInTheirLane(t *testing.T) {
	e := setup(t, delivery.Options{})
	e.enqueue("d1", delivery.ChannelRoute("c"), 1)
	id := func() string {
		rec := e.do("POST", "/v1/items", `{"kind":"task","title":"t","reason":"r"}`, e.web)
		var w struct{ ID string }
		_ = json.Unmarshal(rec.Body.Bytes(), &w)
		return w.ID
	}()
	ch := e.chan_("c")
	for _, c := range []struct{ method, path, body string }{
		{"GET", "/v1/items", ""},
		{"POST", "/v1/items", `{"kind":"task","title":"x","reason":"r"}`},
		{"POST", "/v1/items/" + id + "/decide", `{"option":"A"}`},
	} {
		if rec := e.do(c.method, c.path, c.body, ch); rec.Code != 403 {
			t.Fatalf("channel key %s %s: %d %s", c.method, c.path, rec.Code, rec.Body)
		}
	}
	for name, spec := range map[string]auth.Spec{
		"export":  {Kind: auth.Ext, Scope: "export.notes"},
		"forward": {Kind: auth.Ext, Scope: "forward.hooks"},
		"bare":    {Kind: auth.Ext, Scope: "channel."},
		"webhook": {Kind: auth.Webhook, Scope: "src"},
	} {
		m, _ := e.key(spec)
		for _, c := range []struct{ method, path, body string }{
			{"GET", "/v1/deliveries/next", ""},
			{"POST", "/v1/deliveries/d1/ack", `{"version":1,"external_ref":"m"}`},
			{"POST", "/v1/deliveries/d1/fail", `{"version":1,"reason":"x"}`},
		} {
			if rec := e.do(c.method, c.path, c.body, m); rec.Code != 403 {
				t.Fatalf("%s key %s %s: %d", name, c.method, c.path, rec.Code)
			}
		}
		if rec := e.do("POST", "/v1/items", `{"kind":"task","title":"x","reason":"r"}`, m); rec.Code != 403 {
			t.Fatalf("%s key created an item: %d", name, rec.Code)
		}
	}
	if e.state("d1").State != domain.DeliveryPending {
		t.Fatal("a rejected call changed delivery state")
	}
	if rec := e.do("GET", "/v1/deliveries/next", "", nil); rec.Code != 401 {
		t.Fatalf("no credential: %d", rec.Code)
	}
}

// ACT-PROP-02：建议不产生投递——只有触达产出的投递才会被领到，没有投递时领取为空、Web 未读无新增。
func TestNoDeliveryMeansNothingToClaimOrRead(t *testing.T) {
	e := setup(t, delivery.Options{})
	rec := e.do("POST", "/v1/items", `{"kind":"task","title":"idea","reason":"r"}`, func(r *http.Request) { m, _ := e.key(auth.Spec{Kind: auth.Observer}); m(r) })
	if rec.Code != 201 {
		t.Fatalf("propose: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/deliveries/next?wait=1", "", e.chan_("acc-chan")); rec.Code != http.StatusNoContent {
		t.Fatalf("claim: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/v1/deliveries/unread", "", e.web); !strings.Contains(rec.Body.String(), `"deliveries":[]`) {
		t.Fatalf("unread: %s", rec.Body)
	}
}

// GET /deliveries/today：只有 Web 登录；pushed 与日上限同口径，cap 取当前策略，被静音压下的单独计数。
func TestTodayCountsAreWebOnly(t *testing.T) {
	e := setup(t, delivery.Options{})
	pol := policy.Default()
	pol.Disturb.Mutes = []policy.Mute{{Scope: policy.MuteItem, ID: "item-m"}}
	for _, id := range []string{"p", "m"} {
		d := domain.Delivery{ID: id, Item: "item-" + id, Class: domain.DeliveryReminder, Route: delivery.ChannelRoute("c"), Version: 1, Content: domain.DeliveryContent{Title: id}}
		if err := e.st.Update(bg, func(tx *store.Tx) error { _, _, err := e.q.CreateGated(tx, pol, d, delivery.Target{}); return err }); err != nil {
			t.Fatal(err)
		}
	}
	obs, _ := e.key(auth.Spec{Kind: auth.Observer})
	for _, mod := range []func(*http.Request){e.chan_("c"), obs, func(*http.Request) {}} {
		if rec := e.do("GET", "/v1/deliveries/today", "", mod); rec.Code != 401 && rec.Code != 403 {
			t.Fatalf("non-web today: %d", rec.Code)
		}
	}
	rec := e.do("GET", "/v1/deliveries/today", "", e.web)
	var c map[string]int
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &c) != nil {
		t.Fatalf("today: %d %s", rec.Code, rec.Body)
	}
	want := map[string]int{"pushed": 1, "cap": policy.DefaultDailyCap, "suppressed_daily_cap": 0, "suppressed_muted": 1}
	if len(c) != len(want) {
		t.Fatalf("today = %v, want %v", c, want)
	}
	for k, v := range want {
		if c[k] != v {
			t.Fatalf("today = %v, want %v", c, want)
		}
	}
}
