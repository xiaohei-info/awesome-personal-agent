package brief

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// gated 按 cli/serve 的装配接上带跳过检查的规则引擎；返回引擎与运行目录。
func (e *env) gated() (*rules.Engine, string) {
	e.t.Helper()
	if err := executors.Register(e.x, executors.Options{Owner: "owner", Project: projects.Get}); err != nil {
		e.t.Fatal(err)
	}
	disp := proc.New(e.st, config.Config{MaxSubprocesses: 1, MaxShortSubprocesses: 1}, proc.Options{})
	e.t.Cleanup(disp.Close)
	runs := e.t.TempDir()
	tk, err := task.New(task.Options{Store: e.st, Exec: e.x, Packs: e.ld, Queue: disp, RunsDir: runs, Now: func() time.Time { return e.now }})
	if err != nil {
		e.t.Fatal(err)
	}
	eng, err := rules.New(rules.Options{Store: e.st, Exec: e.x, Packs: e.ld, Owner: "owner", Now: func() time.Time { return e.now },
		Handlers: map[string]rules.Handler{"call_agent": tk.CallAgent(task.AgentOptions{})}, Gate: tk.Gate,
		OnError: func(err error) { e.t.Errorf("rules: %v", err) }})
	if err != nil {
		e.t.Fatal(err)
	}
	return eng, runs
}

func (e *env) briefs() []domain.Item {
	e.t.Helper()
	var its []domain.Item
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { its, err = items.List(tx, items.Filter{Kind: "brief"}); return }); err != nil {
		e.t.Fatal(err)
	}
	return its
}

func (e *env) events(object, typ string) (out []eventlog.Entry) {
	e.t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.ReadObject(tx, object)
		for _, x := range es {
			if x.Event.Type == typ {
				out = append(out, x)
			}
		}
		return err
	})
	return
}

// agentReady 登记一个已确认的无头运行声明并设为默认 Agent。
func (e *env) agentReady() {
	e.t.Helper()
	if _, err := e.do("executor.register", map[string]any{"harness": "hx", "headless": map[string]any{
		"command": []string{"true", "{prompt}"}, "session": map[string]any{"extract": "$.session_id"}, "access": map[string][]string{"read": {}}}}); err != nil {
		e.t.Fatal(err)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "test", func(p *policy.Policy) error { p.DefaultHarness = "hx"; return nil })
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) fire(eng *rules.Engine, at time.Time) {
	e.t.Helper()
	if n := e.tick(at); n != 1 {
		e.t.Fatalf("ticks at %v = %d", at, n)
	}
	if err := eng.Drain(bg); err != nil {
		e.t.Fatal(err)
	}
}

// 最近一次运行的资料：instruction.txt 与 prompt.md。
func lastRun(t *testing.T, runs string, n int) (instruction, prompt string) {
	t.Helper()
	dirs, _ := filepath.Glob(filepath.Join(runs, "run_*"))
	if len(dirs) != n {
		t.Fatalf("runs = %d, want %d", len(dirs), n)
	}
	newest := dirs[0]
	for _, d := range dirs {
		a, _ := os.Stat(d)
		b, _ := os.Stat(newest)
		if a.ModTime().After(b.ModTime()) {
			newest = d
		}
	}
	ins, err := os.ReadFile(filepath.Join(newest, "instruction.txt"))
	if err != nil {
		t.Fatal(err)
	}
	pr, err := os.ReadFile(filepath.Join(newest, "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(ins), string(pr)
}

// Agent 还不能后台运行时，周期简报到点跳过：不建事项、不转给本人，只记一条 rule.skipped；
// 可用之后从下一个时间点起照常建事项并拉起任务型，不补跑错过的（§7.3）。
func TestBriefSkipsUntilAgentCanRunInBackground(t *testing.T) {
	e := newEnv(t, "")
	eng, runs := e.gated()
	res, err := e.do("instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var ins struct{ ID string }
	_ = json.Unmarshal(res.Value, &ins)

	e.fire(eng, t0.Add(24*time.Hour))
	if its := e.briefs(); len(its) != 0 {
		t.Fatalf("brief created while agent is not ready: %+v", its)
	}
	sk := e.events(ins.ID, rules.EvSkipped)
	if len(sk) != 1 || sk[0].Event.Reason != task.SkipAgentNotReady || sk[0].Event.Actor.Via != domain.ViaRule {
		t.Fatalf("skip events = %+v", sk)
	}
	if err := eng.Drain(bg); err != nil || len(e.events(ins.ID, rules.EvSkipped)) != 1 {
		t.Fatalf("re-drain must not record the skip twice: %v", err)
	}

	e.agentReady()
	if err := eng.Drain(bg); err != nil {
		t.Fatal(err)
	}
	if its := e.briefs(); len(its) != 0 {
		t.Fatalf("missed period must not be made up: %+v", its)
	}
	e.fire(eng, t0.Add(48*time.Hour))
	its := e.briefs()
	if len(its) != 1 || its[0].Phase != domain.PhaseOpen {
		t.Fatalf("briefs after agent ready = %+v ask=%+v", its, its[0].Ask)
	}
	var ran bool
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { ran, err = proc.HasRun(tx, its[0].ID); return })
	if !ran {
		t.Fatal("task agent was not launched on the brief")
	}
	if n := len(e.events(ins.ID, rules.EvSkipped)); n != 1 {
		t.Fatalf("skips = %d after agent ready", n)
	}
	insText, prompt := lastRun(t, runs, 1)
	pk, _ := e.ld.Get("brief")
	if def := pk.Prompts["write-brief"].Meta.DefaultInstruction; insText != def || def == "" {
		t.Fatalf("instruction.txt = %q, want the template default %q", insText, def)
	}
	if strings.Contains(prompt, insText) || !strings.Contains(prompt, "写回成功就是回执") {
		t.Fatalf("prompt must be the fixed template body with the instruction only in its file: %s", prompt)
	}
}

// 例行任务不受跳过检查：到点照常建事项，没有 Agent 时转 needs_input 问本人（§13.5）。
func TestRoutineIsNotSkippedWithoutAgent(t *testing.T) {
	e := newEnv(t, "")
	eng, _ := e.gated()
	if _, err := e.do("instance.create", map[string]any{"pack": "routine", "params": json.RawMessage(`{"schedule":"24h","mode":"run","instruction":"写周报"}`)}); err != nil {
		t.Fatal(err)
	}
	e.fire(eng, t0.Add(24*time.Hour))
	var its []domain.Item
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { its, err = items.List(tx, items.Filter{Kind: "routine"}); return })
	if len(its) != 1 || its[0].Phase != domain.PhaseNeedsInput {
		t.Fatalf("routine = %+v", its)
	}
}

// 本人改的内容要求下次运行生效，清空即回到模板默认；超过上限拒绝保存；"立即跑一次"走同一条路。
func TestInstructionOverrideAndRunNow(t *testing.T) {
	e := newEnv(t, "")
	eng, runs := e.gated()
	e.agentReady()
	res, err := e.do("instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"24h","instruction":"只写风险"}`)})
	if err != nil {
		t.Fatal(err)
	}
	var ins struct{ ID string }
	_ = json.Unmarshal(res.Value, &ins)

	if _, err := e.do("instance.update", map[string]any{"id": ins.ID, "params": map[string]any{"schedule": "24h", "instruction": strings.Repeat("长", 2001)}}); err == nil {
		t.Fatal("instruction over the limit must be rejected")
	}
	e.fire(eng, t0.Add(24*time.Hour))
	if got, _ := lastRun(t, runs, 1); got != "只写风险" {
		t.Fatalf("instruction.txt = %q", got)
	}

	if _, err := e.do("instance.update", map[string]any{"id": ins.ID, "params": map[string]any{"schedule": "24h"}}); err != nil {
		t.Fatal(err)
	}
	e.now = t0.Add(25 * time.Hour)
	if _, err := e.do("instance.run", map[string]any{"id": ins.ID}); err != nil {
		t.Fatal(err)
	}
	if err := eng.Drain(bg); err != nil {
		t.Fatal(err)
	}
	if its := e.briefs(); len(its) != 2 {
		t.Fatalf("run now must create one more brief: %+v", its)
	}
	pk, _ := e.ld.Get("brief")
	if got, _ := lastRun(t, runs, 2); got != pk.Prompts["write-brief"].Meta.DefaultInstruction {
		t.Fatalf("cleared instruction must fall back to the default: %q", got)
	}
}

// 默认安排：第一次启动建简报与复盘各一条（操作者 system），只建一次；本人删掉后不补回；
// 已有部署升级后第一次启动照样补建；本人自己建过的包不再补。
func TestSeedDefaultsOnce(t *testing.T) {
	ds := []instances.Default{
		{Pack: "brief", Params: json.RawMessage(`{"schedule":"30 8 * * 1-5"}`)},
		{Pack: "review", Params: json.RawMessage(`{"schedule":"30 17 * * 5"}`)},
	}
	list := func(e *env) []instances.Instance {
		var l []instances.Instance
		_ = e.st.View(bg, func(tx *store.Tx) (err error) { l, err = instances.List(tx); return })
		return l
	}
	seed := func(e *env) {
		if err := instances.Seed(bg, e.x, e.st, e.ld, "owner", ds); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("fresh install", func(t *testing.T) {
		e := newEnv(t, "")
		seed(e)
		seed(e) // 第二次启动
		l := list(e)
		if len(l) != 2 || !l[0].Enabled || !l[1].Enabled {
			t.Fatalf("defaults = %+v", l)
		}
		for _, in := range l {
			cr := e.events(in.ID, instances.EvCreated)
			if len(cr) != 1 || cr[0].Event.Actor.Type != domain.ActorSystem {
				t.Fatalf("created events for %s = %+v", in.Pack, cr)
			}
		}
		if n, err := e.timers(); n != 2 {
			t.Fatalf("timers = %d %v", n, err)
		}
		// 本人删掉（这里直接删行，代表任何删除路径）后再启动，不补回。
		if err := e.st.Update(bg, func(tx *store.Tx) error {
			_, err := tx.Exec(`DELETE FROM instances_instance`)
			return err
		}); err != nil {
			t.Fatal(err)
		}
		seed(e)
		if l := list(e); len(l) != 0 {
			t.Fatalf("deleted defaults came back: %+v", l)
		}
	})

	t.Run("upgrade with an own brief", func(t *testing.T) {
		e := newEnv(t, "")
		if _, err := e.do("instance.create", map[string]any{"pack": "brief", "params": json.RawMessage(`{"schedule":"0 7 * * *"}`)}); err != nil {
			t.Fatal(err)
		}
		seed(e)
		l := list(e)
		if len(l) != 2 {
			t.Fatalf("instances = %+v", l)
		}
		packs := map[string]string{}
		for _, in := range l {
			var p struct{ Schedule string }
			_ = json.Unmarshal(in.Params, &p)
			packs[in.Pack] = p.Schedule
		}
		if packs["brief"] != "0 7 * * *" || packs["review"] != "30 17 * * 5" {
			t.Fatalf("own brief must stay alone and review is seeded: %v", packs)
		}
	})
}

func (e *env) timers() (n int64, err error) {
	err = e.st.View(bg, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT count(*) FROM clock_timer`)
		if err != nil {
			return err
		}
		defer r.Close()
		r.Next()
		return r.Scan(&n)
	})
	return
}
