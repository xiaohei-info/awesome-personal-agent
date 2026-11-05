package events

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apievents "github.com/xiaohei-info/awesome-personal-agent/api/events"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventfeed"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	_ "modernc.org/sqlite"
)

var bg = context.Background()

type handlerTransport struct{ h http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.RemoteAddr = "127.0.0.1:5000"
	h.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

type rig struct {
	t    *testing.T
	st   *store.Store
	auth *auth.Auth
	srv  *core.Server
}

// rig 装配真实账本、鉴权与真实 /v1/events 路由；CLI 经 http.Client 直接打进该 handler。
func newRig(t *testing.T) *rig {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	a := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apievents.Routes(st, 10*time.Millisecond)}})
	if err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, st: st, auth: a, srv: srv}
}

func (r *rig) token(kind auth.Kind, scope string) string {
	var secret string
	if err := r.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := r.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		r.t.Fatal(err)
	}
	return secret
}

var human = domain.Actor{Type: domain.ActorHuman, Principal: "me", Via: domain.ViaWeb}

func (r *rig) emit(typ, obj, title string) {
	r.t.Helper()
	err := r.st.Update(bg, func(tx *store.Tx) error {
		_, err := eventlog.Append(tx, domain.Event{Type: typ, Object: obj, Actor: human, Reason: "SECRET-REASON",
			Evidence: "SECRET-EVIDENCE", Payload: json.RawMessage(`{"title":"` + title + `"}`)})
		return err
	})
	if err != nil {
		r.t.Fatal(err)
	}
}

type result struct {
	code        int
	out, errOut string
}

func (r *rig) apaWith(rt http.RoundTripper, token string, args ...string) result {
	r.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": token, "APA_HOME": r.t.TempDir()}
	env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: r.t.TempDir(), Stdin: strings.NewReader(""),
		Stdout: &out, Stderr: &errb, HTTP: &http.Client{Transport: rt}}
	return result{client.Main(args, env), out.String(), errb.String()}
}

func (r *rig) apa(token string, args ...string) result {
	return r.apaWith(handlerTransport{r.srv.Handler()}, token, args...)
}

func envelopes(t *testing.T, res result) []eventfeed.Envelope {
	t.Helper()
	if res.code != 0 {
		t.Fatalf("exit %d: %s", res.code, res.errOut)
	}
	var out []eventfeed.Envelope
	for _, ln := range strings.Split(strings.TrimSpace(res.out), "\n") {
		if ln == "" {
			continue
		}
		var e eventfeed.Envelope
		if err := json.Unmarshal([]byte(ln), &e); err != nil {
			t.Fatalf("not one JSON envelope per line: %q", ln)
		}
		out = append(out, e)
	}
	return out
}

func objects(es []eventfeed.Envelope) string {
	var s []string
	for _, e := range es {
		s = append(s, e.Object)
	}
	return strings.Join(s, ",")
}

// ACT-EVT-02：从游标续读。调用方用上次最后一条信封的 seq 作 --cursor，只拿到之后的事件；
// 未登记类型不出现，信封不含 reason/证据。
func TestEventsResumeFromCursor(t *testing.T) {
	r := newRig(t)
	r.emit("internal.secret", "x_0", "hidden")
	r.emit("item.created", "item_1", "one")
	r.emit("decision.requested", "dec_1", "two")
	r.emit("item.phase_changed", "item_1", "three")
	obs := r.token(auth.Observer, "")

	all := envelopes(t, r.apa(obs, "events"))
	if objects(all) != "item_1,dec_1,item_1" || all[0].Title != "one" || all[0].Type != "item.created" {
		t.Fatalf("all = %+v", all)
	}
	res := r.apa(obs, "events", "--cursor", "0")
	if strings.Contains(res.out, "SECRET") || strings.Contains(res.out, "hidden") {
		t.Fatalf("leak: %s", res.out)
	}

	rest := envelopes(t, r.apa(obs, "events", "--cursor", jsonInt(all[1].Seq)))
	if len(rest) != 1 || rest[0].Seq != all[2].Seq {
		t.Fatalf("resume = %+v", rest)
	}
	if none := envelopes(t, r.apa(obs, "events", "--cursor", jsonInt(all[2].Seq))); len(none) != 0 {
		t.Fatalf("past the end = %+v", none)
	}
	if lim := envelopes(t, r.apa(obs, "events", "--limit", "2")); objects(lim) != "item_1,dec_1" {
		t.Fatalf("limit = %+v", lim)
	}

	pretty := r.apa(obs, "events", "--pretty")
	if pretty.code != 0 || !strings.Contains(pretty.out, "item.created") || !strings.Contains(pretty.out, "one") {
		t.Fatalf("pretty: %+v", pretty)
	}
}

func jsonInt(n int64) string { b, _ := json.Marshal(n); return string(b) }

// ACT-EVT-01：--follow 先给出已有事件，然后挂起等待，新事件一到就输出；输出够 limit 条后结束。
func TestEventsFollowStreamsLiveEvents(t *testing.T) {
	r := newRig(t)
	r.emit("item.created", "item_1", "one")
	obs := r.token(auth.Observer, "")
	first := envelopes(t, r.apa(obs, "events"))

	done := make(chan result, 1)
	go func() { done <- r.apa(obs, "events", "--follow", "--cursor", "0", "--limit", "3") }()
	select {
	case res := <-done:
		t.Fatalf("follow must wait for more events: %+v", res)
	case <-time.After(150 * time.Millisecond):
	}
	r.emit("internal.secret", "x", "hidden")
	r.emit("run.finished", "run_1", "two")
	time.Sleep(50 * time.Millisecond)
	r.emit("source.failed", "src_1", "three")

	select {
	case res := <-done:
		got := envelopes(t, res)
		if objects(got) != "item_1,run_1,src_1" || got[0].Seq != first[0].Seq {
			t.Fatalf("follow = %+v", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("follow did not deliver live events")
	}
}

// follow 的游标越过不可见事件：账本里先有大量未登记事件时仍能跟到后面的登记事件，不卡原地。
func TestEventsFollowSkipsUnregisteredHistory(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		r.emit("internal.secret", "x", "hidden")
	}
	r.emit("item.created", "item_9", "nine")
	obs := r.token(auth.Observer, "")
	got := envelopes(t, r.apa(obs, "events", "--follow", "--limit", "1"))
	if objects(got) != "item_9" {
		t.Fatalf("got %+v", got)
	}
}

func code(res result) string {
	var e domain.Error
	_ = json.Unmarshal([]byte(res.errOut), &e)
	return string(e.Code)
}

// 失败语义：参数错误退出码 2；运行凭据不能订阅（4）；无凭据（3）；内核不可达（7），都不输出事件。
func TestEventsFailures(t *testing.T) {
	r := newRig(t)
	r.emit("item.created", "item_1", "one")
	obs := r.token(auth.Observer, "")

	for _, args := range [][]string{{"events", "--cursor", "-1"}, {"events", "--cursor", "abc"}, {"events", "--limit", "-2"}, {"events", "extra"}, {"events", "--bogus"}} {
		if res := r.apa(obs, args...); res.code != domain.ExitUsage || res.out != "" {
			t.Fatalf("%v: %+v", args, res)
		}
	}
	if res := r.apa(r.token(auth.Ext, "channel"), "events"); res.code != domain.ExitPermissionDenied || res.out != "" {
		t.Fatalf("channel ext: %+v", res)
	}
	if res := r.apa(r.token(auth.Ext, "export"), "events"); res.code != 0 {
		t.Fatalf("export ext must read: %+v", res)
	}
	if res := r.apa("apa_obs_wrong", "events"); res.code != domain.ExitUnauthenticated {
		t.Fatalf("bad token: %+v", res)
	}
	down := failTransport{errors.New("connection refused")}
	if res := r.apaWith(down, obs, "events", "--follow"); res.code != domain.ExitUnreachable || res.out != "" || code(res) != "unreachable" {
		t.Fatalf("unreachable: %+v", res)
	}
}

type failTransport struct{ err error }

func (f failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, f.err }
