package health

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apihealth "github.com/xiaohei-info/awesome-personal-agent/api/health"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agentsmd"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
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

var bg = context.Background()

type fakePacks []packs.Rejection

func (f fakePacks) Health() []packs.Rejection { return f }

type handlerTransport struct{ h http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	r.RemoteAddr = "127.0.0.1:5000"
	h.h.ServeHTTP(rec, r)
	return rec.Result(), nil
}

type rig struct {
	t     *testing.T
	srv   *core.Server
	token string
}

// newRig 装配真实账本与真实 /v1/health 路由；CLI 经 http.Client 直接打进该 handler。
func newRig(t *testing.T, ph khealth.PackHealth) *rig {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, clock.Module, core.Module,
		proc.Module, delivery.Module, registry.Module, runner.Module, agentsmd.Module, direct.Module, items.Module} {
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
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{apihealth.Routes(st, delivery.New(delivery.Options{Now: time.Now}), ph, 0)}})
	if err != nil {
		t.Fatal(err)
	}
	var secret string
	if err := st.Update(bg, func(tx *store.Tx) error {
		_, s, err := a.Issue(tx, auth.Spec{Kind: auth.Observer})
		secret = s
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return &rig{t: t, srv: srv, token: secret}
}

func (r *rig) apa(args ...string) (code int, out, errOut string) {
	var o, e bytes.Buffer
	vars := map[string]string{"APA_URL": "http://apa.test", "APA_TOKEN": r.token, "APA_HOME": r.t.TempDir()}
	env := client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: r.t.TempDir(), Stdin: strings.NewReader(""),
		Stdout: &o, Stderr: &e, HTTP: &http.Client{Transport: handlerTransport{r.srv.Handler()}}}
	code = client.Main(args, env)
	return code, o.String(), e.String()
}

func (r *rig) apiBody() string {
	req := httptest.NewRequest("GET", "/v1/health", nil)
	req.RemoteAddr = "127.0.0.1:5000"
	req.Header.Set("Authorization", "Bearer "+r.token)
	rec := httptest.NewRecorder()
	r.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		r.t.Fatalf("GET /health: %d %s", rec.Code, rec.Body)
	}
	return rec.Body.String()
}

func canon(t *testing.T, s string) string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("not JSON %q: %v", s, err)
	}
	b, _ := json.Marshal(v)
	return string(b)
}

// flow-health：`apa health` 与 GET /health 是同一份十类信号（含修复入口），observer 凭据即可读。
func TestApaHealthPrintsTheSameReportAsTheAPI(t *testing.T) {
	r := newRig(t, fakePacks{{Pack: "work", Source: "user", Reason: "bad schema"}})
	code, out, errOut := r.apa("health")
	if code != 0 {
		t.Fatalf("health: %d %s", code, errOut)
	}
	if canon(t, out) != canon(t, r.apiBody()) {
		t.Fatalf("CLI and API differ:\ncli: %s\napi: %s", out, r.apiBody())
	}
	var rep struct {
		Healthy bool
		Signals []struct{ ID, Next string }
	}
	_ = json.Unmarshal([]byte(out), &rep)
	if rep.Healthy || len(rep.Signals) != 13 {
		t.Fatalf("want 13 signals and unhealthy: %s", out)
	}
	for _, s := range rep.Signals {
		if s.Next == "" {
			t.Fatalf("signal %s has no repair entry", s.ID)
		}
	}
}

// --pretty：失败的信号带 why 与 next，正常的只标 ok；干净账本显示 healthy。
func TestApaHealthPretty(t *testing.T) {
	_, out, _ := newRig(t, nil).apa("health", "--pretty")
	if !strings.HasPrefix(out, "healthy\n") || strings.Contains(out, "FAIL") || strings.Count(out, "  ok    ") != 13 {
		t.Fatalf("clean: %q", out)
	}
	_, out, _ = newRig(t, fakePacks{{Pack: "work", Reason: "bad"}}).apa("health", "--pretty")
	if !strings.HasPrefix(out, "unhealthy\n") || !strings.Contains(out, "FAIL  pack_rejections (1)") ||
		!strings.Contains(out, "why:  a pack failed to load") || !strings.Contains(out, "next: fix the pack") {
		t.Fatalf("unhealthy: %q", out)
	}
}

// 只读且不接受参数；错误凭据原样失败（退出码非 0）。
func TestApaHealthRejectsArgsAndBadCredential(t *testing.T) {
	r := newRig(t, nil)
	if code, _, _ := r.apa("health", "extra"); code == 0 {
		t.Fatal("extra args must fail")
	}
	r.token = "apa_obs_wrong"
	if code, _, _ := r.apa("health"); code == 0 {
		t.Fatal("bad token must fail")
	}
}
