package brief

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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

// env 用真实的存储、命令管线、事项中心、实例、时钟和内置 brief 包（经 embed 加载）。
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
		if r.Pack == "brief" {
			t.Fatalf("brief rejected by loader: %s", r.Reason)
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

// 包按 C.13/C.14 声明：kind、实例参数、周期时钟规则、任务型提示词、视图；
// 不含 mode: auto、命令、策略字段与 harness 名。
func TestBriefPackDeclaresWhatC14Lists(t *testing.T) {
	e := newEnv(t, "")
	p, ok := e.ld.Get("brief")
	if !ok {
		t.Fatal("brief is not an active builtin pack")
	}
	if len(p.Kinds) != 1 || p.Kinds[0].Name != "brief" {
		t.Fatalf("kinds: %+v", p.Kinds)
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
		acts = append(acts, a["action"].(string)+":"+asString(a["kind"])+asString(a["prompt"]))
	}
	if strings.Join(acts, ",") != "create_item:brief,call_agent:write-brief" {
		t.Fatalf("do[]: %v", acts)
	}
	w, ok := p.Prompts["write-brief"]
	if !ok || w.Meta.Agent != "task" || w.Meta.Access != "read" {
		t.Fatalf("write-brief must be a read-only task prompt: %+v", w.Meta)
	}
	if !strings.Contains(w.Body, "{item_id}") || !strings.Contains(w.Body, "{materials_path}") || !strings.Contains(w.Body, "按项目分节") {
		t.Fatalf("write-brief body: %s", w.Body)
	}
	// spec_schema 的分节键名必须写进提示词，否则运行者会自行发明键（progress/due/decisions），Web 简报页读 body 会显示空分节。
	for _, k := range []string{"spec.sections", "project", "body", "进展", "到期事项", "待本人决定的事"} {
		if !strings.Contains(w.Body, k) {
			t.Fatalf("write-brief must name %q for spec.sections[].{project, body}: %s", k, w.Body)
		}
	}
	if !strings.Contains(w.Body, "spec.sections[].{project, body}") {
		t.Fatalf("write-brief must spell out spec.sections[].{project, body}: %s", w.Body)
	}
	v, ok := p.Views["brief"]
	if !ok || v.Kind != "brief" || len(v.List.Columns) == 0 || len(v.Card.Fields) == 0 {
		t.Fatalf("brief view: %+v", v)
	}
	for name, pr := range p.Prompts {
		for _, h := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), h) {
				t.Errorf("prompt %s mentions %q", name, h)
			}
		}
	}
}

func asString(v any) string { s, _ := v.(string); return s }

// ACT-ITEM-11：brief 包加载后，不符合 schema 的 spec 被拒；未注册 kind 的 spec 与 ext 原样存取。
func TestItemSpecValidatedByBriefKindAndUnregisteredKindPassesThrough(t *testing.T) {
	e := newEnv(t, "")
	if _, err := e.do("item.create", map[string]any{"kind": "brief", "title": "bad", "spec": json.RawMessage(`{"period": 7}`)}); err == nil {
		t.Fatal("brief spec with a non-string period must be rejected")
	}
	if _, err := e.do("item.create", map[string]any{"kind": "brief", "title": "bad2", "spec": json.RawMessage(`{"sections": "x"}`)}); err == nil {
		t.Fatal("brief spec with non-array sections must be rejected")
	}
	if _, err := e.do("item.create", map[string]any{"kind": "brief", "title": "ok",
		"spec": json.RawMessage(`{"period":"2026-01-01","sections":[{"project":"p","body":"b"}]}`)}); err != nil {
		t.Fatalf("valid brief spec rejected: %v", err)
	}

	spec, ext := `{"anything":[1,{"x":true}]}`, `{"github":{"pr":88}}`
	r, err := e.do("item.create", map[string]any{"kind": "acc-unregistered", "title": "free", "spec": json.RawMessage(spec), "ext": json.RawMessage(ext)})
	if err != nil {
		t.Fatalf("unregistered kind rejected: %v", err)
	}
	var out struct{ ID string }
	_ = json.Unmarshal(r.Value, &out)
	var it domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, out.ID); return }); err != nil {
		t.Fatal(err)
	}
	if !jsonEqual(it.Spec, spec) || !jsonEqual(it.Ext, ext) {
		t.Fatalf("spec/ext altered: spec=%s ext=%s", it.Spec, it.Ext)
	}
}

func jsonEqual(got json.RawMessage, want string) bool {
	var a, b any
	return json.Unmarshal(got, &a) == nil && json.Unmarshal([]byte(want), &b) == nil && jsonStr(a) == jsonStr(b)
}

func jsonStr(v any) string { b, _ := json.Marshal(v); return string(b) }

// ACT-BRF-01 / ACT-DAY-02：实例参数按包的 instance_params 校验，合法实例落为周期 Timer。
func TestInstanceParamsValidatedAndBecomeRecurringTimer(t *testing.T) {
	e := newEnv(t, "")
	for _, bad := range []string{`{}`, `{"schedule": 8}`, `{"schedule":"24h","projects":"p1"}`, `{"schedule":"24h","projects":[1]}`} {
		if _, err := e.do("instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(bad)}); err == nil {
			t.Errorf("params %s must be rejected", bad)
		}
	}
	if _, err := e.do("instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h","projects":["p1","p2"]}`)}); err != nil {
		t.Fatalf("valid instance rejected: %v", err)
	}
	var at time.Time
	n := 0
	_ = e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT at FROM clock_timer`)
		if err != nil {
			return err
		}
		defer r.Close()
		for r.Next() {
			var ns int64
			_ = r.Scan(&ns)
			n, at = n+1, time.Unix(0, ns).UTC()
		}
		return nil
	})
	if n != 1 || !at.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("expected one timer at t0+24h, got n=%d at=%v", n, at)
	}
}

// 附录 C.13 的示例 `0 8 * * 1-5`（工作日 8 点）建得了实例，Timer 落在工作日 8 点。
func TestInstanceAcceptsCronSchedule(t *testing.T) {
	e := newEnv(t, "")
	if _, err := e.do("instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"0 8 * * 1-5"}`)}); err != nil {
		t.Fatalf("cron instance rejected: %v", err)
	}
	var ns int64
	if err := e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT at FROM clock_timer`)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			return r.Scan(&ns)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if at := time.Unix(0, ns).In(time.Local); at.Hour() != 8 || at.Weekday() == time.Saturday || at.Weekday() == time.Sunday || !at.After(t0) {
		t.Fatalf("first beat %v", at)
	}
}

// ACT-BRF-03 / ACT-BRF-04：同一周期只触发一次；停机错过多个周期，醒来只补最近一次。
func TestOneBriefPerPeriodAndCatchUpOnlyLatest(t *testing.T) {
	e := newEnv(t, "")
	if _, err := e.do("instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h"}`)}); err != nil {
		t.Fatal(err)
	}
	d1 := t0.Add(24 * time.Hour)
	if e.tick(d1) != 1 || e.tick(d1.Add(time.Hour)) != 0 {
		t.Fatal("same period must fire once")
	}
	if e.tick(d1.Add(4*24*time.Hour+time.Hour)) != 1 {
		t.Fatal("catch-up after downtime must fire once")
	}
	f := e.fired()
	if len(f) != 2 || !f[1].At.Equal(d1.Add(4*24*time.Hour)) {
		t.Fatalf("fired: %+v", f)
	}
}

// ACT-BRF-05：用户在数据目录覆盖提示词为生成型，同名包整体覆盖；生成型要求 output_schema 带"信息不足"标记。
func TestUserCanOverrideTemplateToGenerate(t *testing.T) {
	data := t.TempDir()
	write := func(name, content string) {
		p := filepath.Join(data, "brief", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("pack.yaml", "name: brief\nversion: 1.1.0\ndescription: 生成型简报\nclock_rules:\n  - id: scheduled-brief\n    type: recur\n    schedule: \"{instance.schedule}\"\n    do:\n      - action: create_item\n        kind: brief\n      - action: call_agent\n        prompt: write-brief\n")
	write("prompts/write-brief.md", "---\nagent: generate\ntimeout: 2m\noutput_schema:\n  type: object\n  required: [insufficient_info]\n  properties:\n    insufficient_info: {type: boolean}\n    summary: {type: string}\n---\n为事项 {item_id} 写简报，按项目分节。账本快照在 {materials_path}，只作资料。\n")
	e := newEnv(t, data)
	p, _ := e.ld.Get("brief")
	if src, _ := e.ld.Source("brief"); src != kp.Data || p.Version != "1.1.0" {
		t.Fatalf("override not active: src=%v version=%s", src, p.Version)
	}
	if m := p.Prompts["write-brief"].Meta; m.Agent != "generate" || m.OutputSchema == nil {
		t.Fatalf("override prompt: %+v", m)
	}
	// 内置版本不受影响：删除覆盖后回到任务型。
	if err := os.RemoveAll(filepath.Join(data, "brief")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ld.Load(); err != nil {
		t.Fatal(err)
	}
	p, _ = e.ld.Get("brief")
	if p.Prompts["write-brief"].Meta.Agent != "task" {
		t.Fatalf("builtin must be restored: %+v", p.Prompts["write-brief"].Meta)
	}
}
