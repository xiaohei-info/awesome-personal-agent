package review

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

// env 用真实的存储、命令管线、事项中心、实例、时钟和内置 review 与 brief 包（经 embed 加载）。
type env struct {
	t   *testing.T
	st  *store.Store
	x   *command.Executor
	clk *clock.Clock
	ld  *kp.Loader
	now time.Time
}

func newEnv(t *testing.T, dataDir string) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, instances.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: t0}
	kinds := items.NewKinds()
	e.ld = kp.New(kp.Options{Builtin: rootpacks.FS, DataDir: dataDir, Kinds: kinds})
	rej, err := e.ld.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "review" {
			t.Fatalf("review rejected by loader: %s", r.Reason)
		}
	}
	e.x = command.New(st, func() time.Time { return e.now })
	for _, f := range []func() error{
		func() error { return projects.Register(e.x) },
		func() error { return items.Register(e.x, kinds) },
		func() error { return instances.Register(e.x, e.ld) },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	if e.clk, err = clock.New(st, instances.HandleTimer, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: owner, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so"})
}

func (e *env) tick(at time.Time) int {
	e.t.Helper()
	e.now = at
	n, err := e.clk.Tick(bg)
	if err != nil {
		e.t.Fatal(err)
	}
	return n
}

func (e *env) fired() (out []domain.Event) {
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			if x.Event.Type == instances.EvFired {
				out = append(out, x.Event)
			}
		}
		return err
	})
	return
}

// C.14：review 包声明实例参数、周期时钟规则、任务型提示词，写回 brief kind；
// 不注册新 kind，不含 mode: auto、命令、策略字段与 harness 名。
func TestReviewPackDeclaresWhatC14Lists(t *testing.T) {
	e := newEnv(t, "")
	p, ok := e.ld.Get("review")
	if !ok {
		t.Fatal("review is not an active builtin pack")
	}
	if len(p.Kinds) != 0 {
		t.Fatalf("review must not register kinds: %+v", p.Kinds)
	}
	props, _ := p.InstanceParams["properties"].(map[string]any)
	if props["schedule"] == nil || props["projects"] == nil {
		t.Fatalf("instance_params must declare schedule and projects: %+v", p.InstanceParams)
	}
	if len(p.ClockRules) != 1 {
		t.Fatalf("clock rules: %+v", p.ClockRules)
	}
	cr := p.ClockRules[0]
	if cr.Type != "recur" || cr.Schedule != "{instance.schedule}" || cr.Mode != "" {
		t.Fatalf("clock rule must be an instance-driven recur rule without mode: %+v", cr)
	}
	var acts []string
	for _, a := range cr.Do {
		s, _ := a["action"].(string)
		k, _ := a["kind"].(string)
		pr, _ := a["prompt"].(string)
		acts = append(acts, s+":"+k+pr)
	}
	if strings.Join(acts, ",") != "create_item:brief,call_agent:write-review" {
		t.Fatalf("do[]: %v", acts)
	}
	w, ok := p.Prompts["write-review"]
	if !ok || w.Meta.Agent != "task" || w.Meta.Access != "read" {
		t.Fatalf("write-review must be a read-only task prompt: %+v", w.Meta)
	}
	// spec.period 取到期时刻才分得开周期；分节键名与简报同一 schema；写回即回执，提示词不能再让运行者报 done（附不出证据，§8.4）。
	for _, s := range []string{"{item_id}", "{materials_path}", "spec.period", "item.title", "spec.sections[].{project, body}", "按项目分节", "写回成功就是回执"} {
		if !strings.Contains(w.Body, s) {
			t.Errorf("write-review body lacks %q: %s", s, w.Body)
		}
	}
	for name, pr := range p.Prompts {
		for _, h := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), h) {
				t.Errorf("prompt %s mentions %q", name, h)
			}
		}
	}
}

// ACT-BRF-06：review 实例按周期触发（每周期一次）；写回走 brief kind，
// 按其 schema 校验（period 非字符串被拒），不同周期靠 spec.period 区分。
func TestReviewWritesBackBriefKindDistinguishedByPeriod(t *testing.T) {
	e := newEnv(t, "")
	for _, bad := range []string{`{}`, `{"schedule":8}`, `{"schedule":"168h","projects":[1]}`} {
		if _, err := e.do("instance.create", map[string]any{"pack": "review", "params": json.RawMessage(bad)}); err == nil {
			t.Errorf("params %s must be rejected", bad)
		}
	}
	if _, err := e.do("instance.create", map[string]any{"pack": "review", "params": json.RawMessage(`{"schedule":"168h","projects":["p1"]}`)}); err != nil {
		t.Fatalf("valid instance rejected: %v", err)
	}
	w1 := t0.Add(168 * time.Hour)
	if e.tick(w1) != 1 || e.tick(w1.Add(time.Hour)) != 0 {
		t.Fatal("same period must fire once")
	}
	w2 := w1.Add(168 * time.Hour)
	if e.tick(w2) != 1 {
		t.Fatal("next period must fire again")
	}
	if f := e.fired(); len(f) != 2 || !f[1].At.Equal(w2) {
		t.Fatalf("fired: %+v", f)
	}
	// 回写内容按 brief schema 校验，spec.period 区分周期。
	for _, period := range []string{"2026-W01", "2026-W02"} {
		spec := `{"period":"` + period + `","sections":[{"project":"p1","body":"done"}]}`
		if _, err := e.do("item.create", map[string]any{"kind": "brief", "title": "review " + period, "spec": json.RawMessage(spec)}); err != nil {
			t.Fatalf("review write-back %s rejected: %v", period, err)
		}
	}
	if _, err := e.do("item.create", map[string]any{"kind": "brief", "title": "bad", "spec": json.RawMessage(`{"period": 7}`)}); err == nil {
		t.Fatal("non-string period must be rejected")
	}
}
