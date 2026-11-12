package packs_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/xiaohei-info/awesome-personal-agent/api/core"
	apipacks "github.com/xiaohei-info/awesome-personal-agent/api/packs"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	_ "github.com/xiaohei-info/awesome-personal-agent/cli/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

const briefPack = `name: brief
version: 1.0.0
description: 周期简报
instance_params:
  type: object
  required: [schedule]
  additionalProperties: false
  properties:
    schedule: {type: string}
    limit: {type: integer}
event_rules:
  - id: tag-hit
    mode: observe
    when: {event: item.phase_changed, to_phase: open}
    do:
      - action: add_tag
        tag: hit
`

// kernel 是真实内核（存储、命令管线、授权、包加载器与路由）经 httptest 暴露。
type kernel struct {
	t   *testing.T
	st  *store.Store
	au  *auth.Auth
	url string
}

func newKernel(t *testing.T) *kernel {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module,
		items.Module, instances.Module, rules.Module, core.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ld := kp.New(kp.Options{Kinds: items.NewKinds(), Builtin: fstest.MapFS{"brief/pack.yaml": {Data: []byte(briefPack)}}})
	if rej, err := ld.Load(); err != nil || len(rej) != 0 {
		t.Fatalf("load packs: %v %v", err, rej)
	}
	x := command.New(st, nil)
	for _, f := range []func(*command.Executor) error{
		projects.Register, rules.Register, func(x *command.Executor) error { return instances.Register(x, ld) },
	} {
		if err := f(x); err != nil {
			t.Fatal(err)
		}
	}
	au := auth.New(nil)
	srv, err := core.New(core.Deps{Store: st, Auth: au, Groups: []core.RouteGroup{apipacks.Routes(st, x, ld)}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return &kernel{t, st, au, ts.URL}
}

func (k *kernel) token(kind auth.Kind) string {
	k.t.Helper()
	var secret string
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		_, s, err := k.au.Issue(tx, auth.Spec{Kind: kind})
		secret = s
		return err
	}); err != nil {
		k.t.Fatal(err)
	}
	return secret
}

type result struct {
	code        int
	out, errOut string
}

func run(url, token string, args ...string) result {
	vars := map[string]string{"APA_URL": url, "APA_TOKEN": token, "APA_SESSION": "s1"}
	var out, errb bytes.Buffer
	env := client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: "/nonexistent",
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient}
	return result{client.Main(append([]string{"--evidence", "owner: go ahead"}, args...), env), out.String(), errb.String()}
}

func (k *kernel) apa(token string, args ...string) result { return run(k.url, token, args...) }

func (r result) instance(t *testing.T) instances.Instance {
	t.Helper()
	var in instances.Instance
	if err := json.Unmarshal([]byte(r.out), &in); err != nil || in.ID == "" {
		t.Fatalf("not an instance: %+v", r)
	}
	return in
}

// ACT-PACK-01：list 返回已加载能力包。
func TestPackListShowsLoadedPacks(t *testing.T) {
	k := newKernel(t)
	r := k.apa(k.token(auth.Observer), "pack", "list")
	var out struct {
		Packs []struct{ Name, Version string }
	}
	if r.code != 0 || json.Unmarshal([]byte(r.out), &out) != nil || len(out.Packs) != 1 || out.Packs[0].Name != "brief" || out.Packs[0].Version != "1.0.0" {
		t.Fatalf("list: %+v", r)
	}
}

// ACT-PACK-02/06：add 把 --param 交给内核按 instance_params 校验；list/enable/disable 驱动实例状态。
func TestInstanceLifecycle(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)

	r := k.apa(dlg, "instance", "add", "brief", "--param", "schedule=24h", "--param", "limit=3")
	in := r.instance(t)
	var params map[string]any
	_ = json.Unmarshal(in.Params, &params)
	if r.code != 0 || in.Pack != "brief" || !in.Enabled || params["schedule"] != "24h" || params["limit"] != float64(3) {
		t.Fatalf("add: %+v", r)
	}
	if d := k.apa(dlg, "instance", "disable", in.ID); d.code != 0 || d.instance(t).Enabled {
		t.Fatalf("disable: %+v", d)
	}
	l := k.apa(obs, "instance", "list")
	var lr struct{ Instances []instances.Instance }
	if l.code != 0 || json.Unmarshal([]byte(l.out), &lr) != nil || len(lr.Instances) != 1 || lr.Instances[0].Enabled {
		t.Fatalf("list after disable: %+v", l)
	}
	if e := k.apa(dlg, "instance", "enable", in.ID); e.code != 0 || !e.instance(t).Enabled {
		t.Fatalf("enable: %+v", e)
	}
	if off := k.apa(dlg, "instance", "add", "brief", "--param", "schedule=48h", "--disabled"); off.code != 0 || off.instance(t).Enabled {
		t.Fatalf("add --disabled: %+v", off)
	}
}

func TestInstanceFailureSemantics(t *testing.T) {
	k := newKernel(t)
	dlg, obs := k.token(auth.Delegate), k.token(auth.Observer)
	real := k.apa(dlg, "instance", "add", "brief", "--param", "schedule=24h").instance(t)
	cases := []struct {
		name  string
		token string
		args  []string
		code  int
	}{
		{"params violate schema", dlg, []string{"instance", "add", "brief", "--param", "schedule=1", "--param", "limit=x"}, 2},
		{"missing required param", dlg, []string{"instance", "add", "brief", "--param", "limit=1"}, 2},
		{"unknown pack", dlg, []string{"instance", "add", "nope", "--param", "schedule=24h"}, 2},
		{"observer cannot add", obs, []string{"instance", "add", "brief", "--param", "schedule=24h"}, 4},
		{"enable unknown instance", dlg, []string{"instance", "enable", "ghost"}, 5},
		{"observer cannot disable", obs, []string{"instance", "disable", real.ID}, 4},
		{"bad param syntax", dlg, []string{"instance", "add", "brief", "--param", "schedule"}, 2},
		{"no pack", dlg, []string{"instance", "add"}, 2},
		{"no subcommand", dlg, []string{"instance"}, 2},
		{"unknown subcommand", dlg, []string{"instance", "rm", "x"}, 2},
		{"list args", dlg, []string{"instance", "list", "x"}, 2},
		{"enable args", dlg, []string{"instance", "enable"}, 2},
		{"extra positional", dlg, []string{"instance", "add", "brief", "stray"}, 2},
		{"pack no subcommand", dlg, []string{"pack"}, 2},
		{"pack unknown", dlg, []string{"pack", "rm"}, 2},
		{"pack list args", dlg, []string{"pack", "list", "x"}, 2},
		{"validate no dir", dlg, []string{"pack", "validate"}, 2},
	}
	for _, c := range cases {
		if r := k.apa(c.token, c.args...); r.code != c.code {
			t.Errorf("%s: want exit %d, got %+v", c.name, c.code, r)
		}
	}
	// 失败不留下实例，被拒的 disable 不改状态。
	var lr struct{ Instances []instances.Instance }
	_ = json.Unmarshal([]byte(k.apa(obs, "instance", "list").out), &lr)
	if len(lr.Instances) != 1 || !lr.Instances[0].Enabled {
		t.Fatalf("failed adds left instances: %+v", lr.Instances)
	}
}

// 没有任何切 auto 的命令（非目标）。
func TestNoRuleModeCommand(t *testing.T) {
	k := newKernel(t)
	for _, args := range [][]string{{"pack", "mode"}, {"instance", "mode"}, {"rule", "mode", "x", "auto"}} {
		if r := k.apa(k.token(auth.Delegate), args...); r.code != 2 {
			t.Errorf("%v: %+v", args, r)
		}
	}
}

const head = "name: x\nversion: 1\ndescription: d\n"

func writePack(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for n, c := range files {
		p := filepath.Join(dir, n)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// ACT-PACK-02：validate 离线运行（无 APA_URL、无凭据、内核不可达也不影响），通过时给出摘要。
func TestValidateAcceptsGoodPackOffline(t *testing.T) {
	dir := writePack(t, map[string]string{"pack.yaml": briefPack})
	r := run("", "", "pack", "validate", dir)
	var out struct {
		OK            bool
		Name, Version string
		Hash          string
	}
	if r.code != 0 || json.Unmarshal([]byte(r.out), &out) != nil || !out.OK || out.Name != "brief" || out.Hash == "" {
		t.Fatalf("validate: %+v", r)
	}
}

// ACT-PACK-02：七类坏包（C.11）各自被拒绝，退出码 2，原因点名问题所在。
func TestValidateRejectsBadPacks(t *testing.T) {
	cases := []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"mode auto", map[string]string{"pack.yaml": head + "event_rules:\n  - {id: r, mode: auto, do: []}\n"}, "auto"},
		{"executable command", map[string]string{"pack.yaml": head + "event_rules:\n  - id: r\n    do:\n      - {action: add_tag, tag: t, command: rm -rf /}\n"}, "command"},
		{"judge without text answer", map[string]string{"pack.yaml": head, "prompts/j.md": "---\nagent: judge\non_undecided: yes\n---\nq\n"}, "question_type"},
		{"generate declares tools", map[string]string{"pack.yaml": head, "prompts/g.md": "---\nagent: generate\noutput_schema: {type: object}\ntools: [shell]\n---\nq\n"}, "tools"},
		{"main_session with escalate_to", map[string]string{"pack.yaml": head, "prompts/t.md": "---\nrun_as: main_session\nescalate_to: other\n---\nq\n"}, "escalate_to"},
		{"action outside enum", map[string]string{"pack.yaml": head + "event_rules:\n  - id: r\n    do: [{action: send_mail}]\n"}, "enumeration"},
		{"invalid JSON Schema", map[string]string{"pack.yaml": head + "instance_params: {type: object, pattern: x}\n"}, "JSON Schema"},
		{"unknown placeholder", map[string]string{"pack.yaml": head, "prompts/t.md": "---\nagent: task\n---\n读取 {item_title}\n"}, "item_title"},
		{"file outside layout", map[string]string{"pack.yaml": head, "run.sh": "echo hi"}, "outside the pack layout"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := run("", "", "pack", "validate", writePack(t, c.files))
			if r.code != 2 || !strings.Contains(r.errOut, `"invalid_request"`) || !strings.Contains(r.errOut, c.want) || r.out != "" {
				t.Fatalf("want exit 2 mentioning %q: %+v", c.want, r)
			}
		})
	}
	if r := run("", "", "pack", "validate", filepath.Join(t.TempDir(), "missing")); r.code != 2 {
		t.Fatalf("missing directory must exit 2: %+v", r)
	}
}
