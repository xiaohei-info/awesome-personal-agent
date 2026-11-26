package capture

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/direct"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/inbound"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
	_ "modernc.org/sqlite"
)

var (
	bg    = context.Background()
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}
)

const (
	packYAML = "name: inbound-triage\nversion: 1.0.0\ndescription: capture fixture\n"
)

type env struct {
	t    *testing.T
	st   *store.Store
	auth *auth.Auth
	x    *command.Executor
	srv  *core.Server
	stub *modelstub.Stub
	proj string
}

// setup 装配真实账本、入站流水线（含捕获命令）和 HTTP 服务；只有模型端点是替身。项目已开启捕获。
func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module,
		proc.Module, delivery.Module, registry.Module, runner.Module, rules.Module, direct.Module, runidentity.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	stub := modelstub.New()
	t.Cleanup(stub.Close)
	stub.Judge(modelstub.ChatJudge{Answer: "yes"}) // 没有判断端点：预筛由对话端点答"是"
	stub.SetDefault(modelstub.Reply{Content: `{"candidates":[{"title":"补发布脚本","kind":"task"}]}`})
	x := command.New(st, time.Now)
	kinds := items.NewKinds()
	for _, err := range []error{projects.Register(x), items.Register(x, kinds)} {
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "model", func(p *policy.Policy) error {
			p.GenerateEndpoint = policy.GenerateEndpoint{URL: stub.URL(), Model: "m1", APIKey: "k", Locality: policy.LocalityLocal}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	fs := fstest.MapFS{"inbound-triage/pack.yaml": {Data: []byte(packYAML)}}
	for _, name := range []string{"triage-screen"} { // 用内置包随发行的统一判断模板
		b, err := os.ReadFile("../../packs/inbound-triage/prompts/" + name + ".md")
		if err != nil {
			t.Fatal(err)
		}
		fs["inbound-triage/prompts/"+name+".md"] = &fstest.MapFile{Data: b}
	}
	ld := packs.New(packs.Options{Kinds: kinds, Builtin: fs})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load: %v %+v", err, rej)
	}
	judge, err := direct.New(direct.Options{Store: st, Packs: ld, Model: &model.Client{}, Attempts: 1})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := inbound.New(inbound.Options{Store: st, Exec: x, Packs: ld, Judge: judge, Owner: "owner", OnError: func(err error) { t.Errorf("inbound: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Register(x); err != nil {
		t.Fatal(err)
	}
	a := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: a, Groups: []core.RouteGroup{Routes(x)}})
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, st: st, auth: a, x: x, srv: srv, stub: stub}
	r, err := x.Do(bg, command.Request{Identity: owner, Command: "project.create", Input: []byte(`{"name":"web"}`), Owner: "owner"})
	if err != nil {
		t.Fatal(err)
	}
	var p struct{ ID string }
	_ = json.Unmarshal(r.Value, &p)
	e.proj = p.ID
	e.setCapture(true)
	return e
}

func (e *env) setCapture(on bool) {
	e.t.Helper()
	b, _ := json.Marshal(map[string]any{"id": e.proj, "capture": on})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "project.update", Input: b, Owner: "owner"}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) token(kind auth.Kind, scope string) string {
	e.t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, s, err := e.auth.Issue(tx, auth.Spec{Kind: kind, Scope: scope})
		secret = s
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return secret
}

// runSecret 为一个事项入队后台运行并签发运行凭据（与内核拉起运行同一条路径）。
func (e *env) runSecret() string {
	e.t.Helper()
	r0, err := e.x.Do(bg, command.Request{Identity: owner, Command: "item.create", Owner: "owner",
		Input: []byte(`{"kind":"task","title":"target","project":"` + e.proj + `"}`)})
	if err != nil {
		e.t.Fatal(err)
	}
	var it struct{ ID string }
	_ = json.Unmarshal(r0.Value, &it)
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		r, err := proc.Enqueue(tx, proc.Request{Run: domain.Run{Category: domain.RunBackground, Target: it.ID, TimeoutSec: 60},
			Spec: proc.Spec{Command: []string{"true"}}}, time.Now())
		if err != nil {
			return err
		}
		secret, err = runidentity.Issue(tx, r.ID, time.Now())
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return secret
}

func (e *env) post(secret, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/capture", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+secret)
	req.Header.Set("X-APA-Session", "s1")
	rec := httptest.NewRecorder()
	e.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (e *env) events() (out []domain.SourceEvent) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { out, err = runner.Events(tx, "capture:"+e.proj); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) body(rec *httptest.ResponseRecorder, wantStatus int) map[string]any {
	e.t.Helper()
	var m map[string]any
	if rec.Code != wantStatus || json.Unmarshal(rec.Body.Bytes(), &m) != nil {
		e.t.Fatalf("status %d (want %d) body %s", rec.Code, wantStatus, rec.Body)
	}
	return m
}

// ACT-CAP-03：worker 凭据在开启捕获的项目里提交增量，立即得到 captured；事件经统一 judge 后进入来源处理队列。
func TestCaptureEndpointProposesCandidate(t *testing.T) {
	e := setup(t)
	m := e.body(e.post(e.token(auth.Worker, ""), `{"project":"`+e.proj+`","text":"用户：别忘了把发布脚本补上"}`), 200)
	if m["captured"] != true || m["project"] != e.proj {
		t.Fatalf("response %v", m)
	}
	var ev domain.SourceEvent
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if evs := e.events(); len(evs) == 1 && evs[0].Queue == runner.QueueQueued {
			ev = evs[0]
			break
		}
	}
	if ev.Queue != runner.QueueQueued || ev.SourceID != "capture:"+e.proj {
		t.Fatalf("event not queued: %+v", e.events())
	}
}

// 默认关闭与运行身份下不生效：都是 200 + captured:false，账本里没有任何来源事件，也不外呼模型。
func TestCaptureEndpointNoEffectWhenDisabledOrRunIdentity(t *testing.T) {
	e := setup(t)
	body := `{"project":"` + e.proj + `","text":"随便聊聊"}`
	if m := e.body(e.post(e.runSecret(), body), 200); m["captured"] != false || m["reason"] != inbound.ReasonRunIdentity {
		t.Fatalf("run identity: %v", m)
	}
	e.setCapture(false)
	if m := e.body(e.post(e.token(auth.Worker, ""), body), 200); m["captured"] != false || m["reason"] != inbound.ReasonCaptureDisabled {
		t.Fatalf("disabled: %v", m)
	}
	if len(e.events()) != 0 || len(e.stub.Requests()) != 0 {
		t.Fatal("no-op requests must leave no trace")
	}
}

// 授权与输入错误经统一错误体返回：observer 403、ext 密钥 403、未认证 401、空文本与未知字段 400、未知项目 404。
func TestCaptureEndpointErrors(t *testing.T) {
	e := setup(t)
	ok := `{"project":"` + e.proj + `","text":"x"}`
	worker := e.token(auth.Worker, "")
	for _, c := range []struct {
		name, secret, body string
		status             int
		code               domain.Code
	}{
		{"observer", e.token(auth.Observer, ""), ok, http.StatusForbidden, domain.CodePermissionDenied},
		{"ext key", e.token(auth.Ext, "telegram"), ok, http.StatusForbidden, domain.CodePermissionDenied},
		{"no credential", "apa_wrk_unknown", ok, http.StatusUnauthorized, domain.CodeUnauthenticated},
		{"empty text", worker, `{"project":"` + e.proj + `","text":" "}`, http.StatusBadRequest, domain.CodeInvalidRequest},
		{"unknown field", worker, `{"project":"` + e.proj + `","text":"x","chain_depth":0}`, http.StatusBadRequest, domain.CodeInvalidRequest},
		{"unknown project", worker, `{"project":"p_nope","text":"x"}`, http.StatusNotFound, domain.CodeNotFound},
		{"not an object", worker, `null`, http.StatusBadRequest, domain.CodeInvalidRequest},
	} {
		rec := e.post(c.secret, c.body)
		var de domain.Error
		if rec.Code != c.status || json.Unmarshal(rec.Body.Bytes(), &de) != nil || de.Code != c.code {
			t.Errorf("%s: status %d body %s", c.name, rec.Code, rec.Body)
		}
	}
	if len(e.events()) != 0 {
		t.Fatal("rejected requests must not capture")
	}
}

// ACT-CAP-01 / §14.5 第一档：delegate 凭据提交候选捕获是 agent 自己的工作，不带本人原话也放行，事件入队待批处理。
func TestCaptureByDelegateNeedsNoWords(t *testing.T) {
	e := setup(t)
	m := e.body(e.post(e.token(auth.Delegate, ""), `{"project":"`+e.proj+`","text":"用户：周五前把发布脚本补上"}`), 200)
	if m["captured"] != true {
		t.Fatalf("response %v", m)
	}
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if evs := e.events(); len(evs) == 1 && evs[0].Queue == runner.QueueQueued {
			return
		}
	}
	t.Fatalf("no event queued: %+v", e.events())
}

// 来源标注（附录 B.2、B.6）：外部人发起的轮次照常捕获；不是的过滤为 200 captured:false（不是错误），
// 不合法或未知的标注是 400。标注字段原样透传到内核。
func TestCaptureEndpointFiltersNonExternalOrigins(t *testing.T) {
	e := setup(t)
	worker := e.token(auth.Worker, "")
	m := e.body(e.post(worker, `{"project":"`+e.proj+`","text":"用户：记一下周五之前把发布脚本补上","capture_schema":1,
		"speaker_role":"human","origin_type":"external_chat"}`), 200)
	if m["captured"] != true || m["project"] != e.proj {
		t.Fatalf("external human turn: %v", m)
	}
	for _, c := range []struct{ body, reason string }{
		{`{"project":"` + e.proj + `","text":"[Cronjob] 输出","capture_schema":1,"speaker_role":"system","origin_type":"cron_echo"}`, inbound.ReasonNonExternalOrigin},
		{`{"project":"` + e.proj + `","text":"后台回执","capture_schema":1,"speaker_role":"system","origin_type":"system_notice"}`, inbound.ReasonNonExternalOrigin},
		{`{"project":"` + e.proj + `","text":"已经答复完了","capture_schema":1,"speaker_role":"human","origin_type":"external_chat","status_hint":"resolved"}`, inbound.ReasonAlreadyResolved},
	} {
		m := e.body(e.post(worker, c.body), 200)
		if m["captured"] != false || m["reason"] != c.reason || m["project"] != e.proj {
			t.Fatalf("filtered turn: %v", m)
		}
	}
	for _, body := range []string{
		`{"project":"` + e.proj + `","text":"x","capture_schema":3}`,
		`{"project":"` + e.proj + `","text":"x","capture_schema":1,"speaker_role":"human"}`,
		`{"project":"` + e.proj + `","text":"x","speaker_role":"robot","origin_type":"external_chat"}`,
	} {
		rec := e.post(worker, body)
		var de domain.Error
		if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &de) != nil || de.Code != domain.CodeInvalidRequest {
			t.Errorf("status %d body %s", rec.Code, rec.Body)
		}
	}
	// 放行的这一条异步走完流水线再结束，免得测试关账本时它还在写。
	for end := time.Now().Add(20 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if evs := e.events(); len(evs) == 1 && evs[0].Queue == runner.QueueQueued {
			return
		}
	}
	t.Fatalf("the external human turn must become exactly one source event: %+v", e.events())
}
