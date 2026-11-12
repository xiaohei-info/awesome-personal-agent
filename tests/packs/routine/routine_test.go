package routine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/agents/task"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/clock"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/instances"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/materials"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/proc"
	kp "github.com/xiaohei-info/awesome-personal-agent/kernel/packs"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/projects"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/rules"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/runidentity"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

var (
	bg    = context.Background()
	t0    = time.Date(2026, 1, 1, 8, 0, 0, 0, time.UTC)
	owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}
)

// secret 是本人写在实例里的任务说明里的标记：它只能出现在运行目录的文件里，不能出现在提示词里（C.7、§8.4）。
const secret = "ACC-INSTR-7"

// 无头运行替身脚本：参数 1 是提示词，参数 2 是运行目录。
var scripts = map[string]string{
	// 记下提示词与运行凭据，等测试以运行凭据写回结果后再退出。
	"receipt": `printf '%s' "$1" > "$2/prompt.seen"
printf '%s' "$APA_TOKEN" > "$2/token"
while [ ! -f "$2/go" ]; do sleep 0.05; done
echo '{"session_id":"s","outcome":"done"}'
`,
	"exit1":  "exit 1\n",
	"silent": "true\n", // 不写回直接退出 0
}

type env struct {
	t    *testing.T
	st   *store.Store
	x    *command.Executor
	clk  *clock.Clock
	eng  *rules.Engine
	ld   *kp.Loader
	disp *proc.Dispatcher
	runs string
	dir  string
	now  time.Time
	done chan proc.Result
}

// newEnv 用真实的存储、命令管线、实例、时钟、规则引擎、任务型与无头运行调度，加载内置 routine 包（经 embed）。
// 没有模型端点：生成型没有装配，任何模型调用都会失败。
func newEnv(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, projects.Module, clock.Module, items.Module, instances.Module,
		rules.Module, task.Module, executors.Module, proc.Module, runidentity.Module, delegation.Module, materials.Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{t: t, st: st, now: t0, runs: filepath.Join(t.TempDir(), "runs"), dir: t.TempDir(), done: make(chan proc.Result, 16)}
	e.x = command.New(st, func() time.Time { return e.now })
	kinds := items.NewKinds()
	e.ld = kp.New(kp.Options{Builtin: rootpacks.FS, Kinds: kinds})
	rej, err := e.ld.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rej {
		if r.Pack == "routine" {
			t.Fatalf("routine rejected by loader: %s", r.Reason)
		}
	}
	for _, f := range []func() error{
		func() error { return projects.Register(e.x) },
		func() error { return items.Register(e.x, kinds) },
		func() error { return instances.Register(e.x, e.ld) },
		func() error { return executors.Register(e.x, executors.Options{Owner: "owner"}) },
		func() error {
			return delegation.Register(e.x, delegation.Options{OnError: func(command.Commit, error) {}})
		},
		func() error { return materials.Register(e.x) },
	} {
		if err := f(); err != nil {
			t.Fatal(err)
		}
	}
	var svc *task.Service
	e.disp = proc.New(st, config.Config{MaxSubprocesses: 2, MaxShortSubprocesses: 1}, proc.Options{
		Env:      func(r domain.Run) map[string]string { return svc.Env(r) },
		OnResult: func(r proc.Result) { svc.Settle(bg, r); e.done <- r },
	})
	t.Cleanup(e.disp.Close)
	svc, err = task.New(task.Options{Store: st, Exec: e.x, Packs: e.ld, Queue: e.disp, RunsDir: e.runs, APIURL: "http://apa.local", Host: "this-host",
		Now: func() time.Time { return e.now }, OnError: func(run string, err error) { t.Errorf("settle %s: %v", run, err) }})
	if err != nil {
		t.Fatal(err)
	}
	e.eng, err = rules.New(rules.Options{Store: st, Exec: e.x, Packs: e.ld, Owner: "owner", Now: func() time.Time { return e.now },
		Handlers: map[string]rules.Handler{"call_agent": svc.CallAgent(task.AgentOptions{})},
		OnError:  func(err error) { t.Errorf("rules: %v", err) }})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.eng.Reconcile(bg); err != nil {
		t.Fatal(err)
	}
	if e.clk, err = clock.New(st, func(tx *store.Tx, c clock.Command) error {
		if instances.Owns(c) {
			return instances.HandleTimer(tx, c)
		}
		return e.eng.HandleTimer(tx, c)
	}, func() time.Time { return e.now }); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner", Evidence: "owner said so", WebURL: "http://web"})
}

func (e *env) must(cmd string, in any) json.RawMessage {
	e.t.Helper()
	r, err := e.do(owner, cmd, in)
	if err != nil {
		e.t.Fatalf("%s: %v", cmd, err)
	}
	return r.Value
}

func (e *env) project(name string) string {
	e.t.Helper()
	var o struct{ ID string }
	_ = json.Unmarshal(e.must("project.create", map[string]any{"name": name}), &o)
	return o.ID
}

// instance 建一个 routine 实例，返回实例 id。
func (e *env) instance(project string, params map[string]any) string {
	e.t.Helper()
	in := map[string]any{"pack": "routine", "params": params}
	if project != "" {
		in["project"] = project
	}
	var o struct{ ID string }
	_ = json.Unmarshal(e.must("instance.create", in), &o)
	return o.ID
}

// harness 登记 acc-stub 并设为默认 harness，声明 read、edit 两档；Web 的权限上限保持默认（read）。
func (e *env) harness(script string) {
	e.t.Helper()
	path := filepath.Join(e.dir, script+".sh")
	if err := os.WriteFile(path, []byte(scripts[script]), 0o700); err != nil {
		e.t.Fatal(err)
	}
	e.must("executor.register", map[string]any{"harness": "acc-stub", "headless": map[string]any{
		"command": []string{"sh", path, "{prompt}", "{run_dir}"}, "session": map[string]any{"extract": "$.session_id"},
		"access": map[string]any{"read": []string{}, "edit": []string{}}}})
	if err := e.st.Update(bg, func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner.Actor, "harness", func(p *policy.Policy) error { p.DefaultHarness = "acc-stub"; return nil })
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) tick(at time.Time) {
	e.t.Helper()
	e.now = at
	if _, err := e.clk.Tick(bg); err != nil {
		e.t.Fatal(err)
	}
	if err := e.eng.Drain(bg); err != nil {
		e.t.Fatal(err)
	}
}

func (e *env) routineItems() (out []domain.Item) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) error {
		all, err := items.List(tx, items.Filter{})
		for _, it := range all {
			if it.Kind == "routine" {
				out = append(out, it)
			}
		}
		return err
	}); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) item(id string) (it domain.Item) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { it, err = items.Get(tx, id); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) runList() (rs []proc.Record) {
	e.t.Helper()
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { rs, err = proc.List(tx); return }); err != nil {
		e.t.Fatal(err)
	}
	return
}

func (e *env) eventCount(typ string) (n int) {
	e.t.Helper()
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 100000)
		for _, x := range es {
			if x.Event.Type == typ {
				n++
			}
		}
		return err
	})
	return
}

func (e *env) waitResult() proc.Result {
	e.t.Helper()
	select {
	case r := <-e.done:
		return r
	case <-time.After(15 * time.Second):
		e.t.Fatal("run did not finish")
	}
	return proc.Result{}
}

func waitFile(t *testing.T, path string) string {
	t.Helper()
	for end := time.Now().Add(10 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	t.Fatalf("%s never appeared", path)
	return ""
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 包按 C.14 声明：kind、七个实例参数里属于包的六个（项目是实例自己的字段，C.10）、实例驱动的时钟规则（建事项再调任务型）、
// 任务型提示词 run-routine（固定模板、instruction_path、result_fields）；不含 mode: auto、命令、策略字段与 harness 名。
func TestRoutinePackDeclaresWhatC14Lists(t *testing.T) {
	e := newEnv(t)
	p, ok := e.ld.Get("routine")
	if !ok {
		t.Fatal("routine is not an active builtin pack")
	}
	if len(p.Kinds) != 1 || p.Kinds[0].Name != "routine" {
		t.Fatalf("kinds: %+v", p.Kinds)
	}
	props, _ := p.InstanceParams["properties"].(map[string]any)
	for _, k := range []string{"schedule", "mode", "title", "instruction", "access", "output"} {
		if props[k] == nil {
			t.Errorf("instance_params lacks %s: %+v", k, p.InstanceParams)
		}
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
	if strings.Join(acts, ",") != "create_item:routine,call_agent:run-routine" {
		t.Fatalf("do[]: %v", acts)
	}
	r, ok := p.Prompts["run-routine"]
	if !ok || r.Meta.Agent != "task" || r.Meta.Access != "read" {
		t.Fatalf("run-routine must be a read-only task prompt: %+v", r.Meta)
	}
	for _, s := range []string{"{item_id}", "{materials_path}", "{instruction_path}", "apa material commit --kind minutes"} {
		if !strings.Contains(r.Body, s) {
			t.Errorf("run-routine body lacks %q: %s", s, r.Body)
		}
	}
	if got := strings.Join(r.Meta.ResultFields, ","); got != "result" {
		t.Fatalf("run-routine result_fields: %q", got)
	}
	for name, pr := range p.Prompts {
		for _, h := range []string{"claude", "codex", "hermes", "openclaw", "himalaya"} {
			if strings.Contains(strings.ToLower(pr.Body), h) {
				t.Errorf("prompt %s mentions %q", name, h)
			}
		}
	}
}

// ACT-RTN-06：mode 不在 run、create_item 之内、缺 schedule、缺 mode、档位或输出方式不在枚举内、带包没有声明的参数，
// 都按 routine 包的 instance_params 拒绝，账本里没有新实例。
func TestInvalidInstanceParamsAreRejected(t *testing.T) {
	e := newEnv(t)
	for name, bad := range map[string]string{
		"mode outside the enum": `{"schedule":"* * * * *","mode":"sometimes","title":"x"}`,
		"no schedule":           `{"mode":"create_item","title":"x"}`,
		"no mode":               `{"schedule":"* * * * *","title":"x"}`,
		"bad access":            `{"schedule":"* * * * *","mode":"run","access":"root"}`,
		"bad output":            `{"schedule":"* * * * *","mode":"run","output":"somewhere"}`,
		"schedule not a string": `{"schedule":8,"mode":"create_item"}`,
		"undeclared param":      `{"schedule":"* * * * *","mode":"create_item","command":"rm"}`,
		"unparseable schedule":  `{"schedule":"whenever","mode":"create_item"}`,
	} {
		_, err := e.do(owner, "instance.create", map[string]any{"pack": "routine", "params": json.RawMessage(bad)})
		if err == nil {
			t.Errorf("%s: %s must be rejected", name, bad)
		} else if strings.Contains(name, "enum") && !strings.Contains(err.Error(), "mode") {
			t.Errorf("%s: the error must point at the bad parameter: %v", name, err)
		}
	}
	var n int
	_ = e.st.View(bg, func(tx *store.Tx) error {
		l, err := instances.List(tx)
		n = len(l)
		return err
	})
	if n != 0 {
		t.Fatalf("%d instances exist after only rejected creations", n)
	}
	e.instance("", map[string]any{"schedule": "* * * * *", "mode": "create_item", "title": "ok"}) // 合法的建得出
}

var dateInTitle = regexp.MustCompile(`^acc 例行提醒 \d{4}-\d{2}-\d{2}$`)

// ACT-RTN-02 / 03：create_item 模式到点只建一条 open 事项（kind=routine、项目取实例的项目、标题带日期），不建 Run、
// 不调模型；没有任何 harness 时照常建，健康视图没有告警。
func TestCreateItemModeBuildsOneOpenItemWithoutHostOrModel(t *testing.T) {
	e := newEnv(t) // 没有 harness、没有默认 harness、没有模型端点
	home := e.project("acc-home")
	e.instance(home, map[string]any{"schedule": "24h", "mode": "create_item", "title": "acc 例行提醒 {date}"})
	e.tick(t0.Add(24 * time.Hour))
	got := e.routineItems()
	if len(got) != 1 {
		t.Fatalf("routine items: %+v", got)
	}
	it := got[0]
	if it.Phase != domain.PhaseOpen || it.Project != home || !dateInTitle.MatchString(it.Title) {
		t.Fatalf("item: phase=%s project=%s title=%q", it.Phase, it.Project, it.Title)
	}
	if want := t0.Add(24 * time.Hour).In(time.Local).Format("2006-01-02"); !strings.HasSuffix(it.Title, want) {
		t.Fatalf("title %q must carry the due date %s", it.Title, want)
	}
	if n := len(e.runList()); n != 0 {
		t.Fatalf("create_item mode started %d runs", n)
	}
	for _, typ := range []string{domain.EvHealthAlert, domain.EvRunStarted, domain.EvRunFinished} {
		if n := e.eventCount(typ); n != 0 {
			t.Fatalf("create_item mode left %d %s events", n, typ)
		}
	}
}

// ACT-RTN-04：同一周期至多一条；停机错过多个周期，醒来只补最近一次。
func TestOneItemPerPeriodAndOnlyLatestAfterDowntime(t *testing.T) {
	e := newEnv(t)
	e.instance("", map[string]any{"schedule": "24h", "mode": "create_item", "title": "r {date}"})
	d1 := t0.Add(24 * time.Hour)
	e.tick(d1)
	e.tick(d1.Add(time.Hour))
	if n := len(e.routineItems()); n != 1 {
		t.Fatalf("same period gave %d items", n)
	}
	e.tick(d1.Add(4*24*time.Hour + time.Hour)) // 停机 4 个周期后醒来
	if n := len(e.routineItems()); n != 2 {
		t.Fatalf("catch-up after downtime must add exactly one item, have %d", n)
	}
}

// ACT-RTN-05：停用实例后不再建事项，重新启用可恢复。
func TestDisabledInstanceBuildsNothing(t *testing.T) {
	e := newEnv(t)
	id := e.instance("", map[string]any{"schedule": "24h", "mode": "create_item"})
	e.tick(t0.Add(24 * time.Hour))
	e.must("instance.toggle", map[string]any{"id": id, "enabled": false})
	e.tick(t0.Add(10 * 24 * time.Hour))
	if n := len(e.routineItems()); n != 1 {
		t.Fatalf("a disabled instance kept building items: %d", n)
	}
}

// 没写 title 的实例仍可建事项：标题取包名与到期时刻。
func TestUntitledInstanceStillBuildsItem(t *testing.T) {
	e := newEnv(t)
	e.instance("", map[string]any{"schedule": "24h", "mode": "create_item"})
	e.tick(t0.Add(24 * time.Hour))
	if got := e.routineItems(); len(got) != 1 || got[0].Title == "" || strings.Contains(got[0].Title, "{") {
		t.Fatalf("items: %+v", got)
	}
}

// ACT-RTN-07：run 模式先建好 open 的 routine 事项，再在它上面起任务型 Run；提示词是固定模板、含 instruction_path 指向的文件，
// 但不含任务说明本身；文件在这次运行的运行目录里，内容就是本人写的任务说明；资料文件带实例参数与账本快照。
func TestRunModeGivesInstructionThroughFileInRunDir(t *testing.T) {
	e := newEnv(t)
	e.harness("receipt")
	home := e.project("acc-home")
	instr := "按本周完成的事项和进展写一份周报草稿 " + secret
	e.instance(home, map[string]any{"schedule": "24h", "mode": "run", "title": "acc 周报 {date}", "instruction": instr, "access": "read", "output": "item_and_material"})
	e.tick(t0.Add(24 * time.Hour))

	items := e.routineItems()
	rs := e.runList()
	if len(items) != 1 || len(rs) != 1 {
		t.Fatalf("items %d runs %d", len(items), len(rs))
	}
	it, run := items[0], rs[0]
	if run.Run.Target != it.ID || run.Run.Harness != "acc-stub" || run.Run.Access != "read" || it.Project != home {
		t.Fatalf("run %+v on item %+v", run.Run, it)
	}
	runDir := filepath.Join(e.runs, run.Run.ID)
	prompt := read(t, filepath.Join(runDir, "prompt.md"))
	path := regexp.MustCompile(`\S*instruction\.txt`).FindString(prompt)
	if path == "" {
		t.Fatalf("prompt does not point at an instruction file: %s", prompt)
	}
	if strings.Contains(prompt, secret) || strings.Contains(strings.Join(run.Spec.Command, " "), secret) {
		t.Fatalf("the instruction leaked into the prompt: %s", prompt)
	}
	if filepath.Dir(path) != runDir {
		t.Fatalf("instruction file %s is not inside the run dir %s", path, runDir)
	}
	if got := read(t, path); got != instr {
		t.Fatalf("instruction file holds %q, want %q", got, instr)
	}
	mats := read(t, filepath.Join(runDir, "materials.json"))
	if !strings.Contains(mats, "ledger_snapshot") || !strings.Contains(mats, "instance_params") || !strings.Contains(mats, "item_and_material") {
		t.Fatalf("materials lack the ledger snapshot or instance params: %s", mats)
	}

	// ACT-RTN-08：运行者以运行凭据写全 result，正常退出后内核代为转 done；证据记写回的对象。
	id := runidentity.Identity{Secret: waitFile(t, filepath.Join(runDir, "token"))}
	if _, err := e.do(id, "item.update", map[string]any{"id": it.ID, "spec": json.RawMessage(`{"result":"周报草稿"}`), "reason": "write"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "go"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	e.waitResult()
	if done := e.item(it.ID); done.Phase != domain.PhaseDone || !strings.Contains(string(done.Spec), "周报草稿") {
		t.Fatalf("item after write-back: phase=%s spec=%s ask=%+v", done.Phase, done.Spec, done.Ask)
	}
}

// ACT-RTN-09：access 高于 Web 设置的上限时不以高档位运行：没有 Run，事项转 needs_input 并指出超出上限。
func TestRunModeNeverRunsAboveThePermissionCap(t *testing.T) {
	e := newEnv(t)
	e.harness("receipt") // acc-stub 声明了 read、edit，上限仍是 read
	e.instance("", map[string]any{"schedule": "24h", "mode": "run", "instruction": "x", "access": "edit"})
	e.tick(t0.Add(24 * time.Hour))
	items := e.routineItems()
	if len(items) != 1 || len(e.runList()) != 0 {
		t.Fatalf("items %d runs %d", len(items), len(e.runList()))
	}
	if it := e.item(items[0].ID); it.Phase != domain.PhaseNeedsInput || it.Ask == nil || !strings.Contains(it.Ask.Question, "上限") {
		t.Fatalf("item: %s %+v", it.Phase, it.Ask)
	}
}

// ACT-RTN-10：运行失败转 needs_input 并写明原因；这条事项只有一个 Run，之后的重新处理不重试。
func TestRunModeFailureGoesToOwnerWithoutRetry(t *testing.T) {
	e := newEnv(t)
	e.harness("exit1")
	e.instance("", map[string]any{"schedule": "24h", "mode": "run", "instruction": "x"})
	e.tick(t0.Add(24 * time.Hour))
	e.waitResult()
	items := e.routineItems()
	if len(items) != 1 {
		t.Fatalf("items: %+v", items)
	}
	it := e.item(items[0].ID)
	if it.Phase != domain.PhaseNeedsInput || it.Ask == nil || it.Ask.Question == "" {
		t.Fatalf("item: %s %+v", it.Phase, it.Ask)
	}
	e.tick(t0.Add(24*time.Hour + time.Minute))
	if n := len(e.runList()); n != 1 {
		t.Fatalf("a failed run was retried: %d runs", n)
	}
}

// ACT-RTN-11：运行正常退出但没有写回结果，事项转 needs_input，原因写明声明的结果字段没写全，不转 done。
func TestRunModeWithoutResultNeedsInput(t *testing.T) {
	e := newEnv(t)
	e.harness("silent")
	e.instance("", map[string]any{"schedule": "24h", "mode": "run", "instruction": "x"})
	e.tick(t0.Add(24 * time.Hour))
	e.waitResult()
	items := e.routineItems()
	if len(items) != 1 {
		t.Fatalf("items: %+v", items)
	}
	it := e.item(items[0].ID)
	if it.Phase != domain.PhaseNeedsInput || it.Ask == nil || !strings.Contains(it.Ask.Question, "result") {
		t.Fatalf("item: %s %+v", it.Phase, it.Ask)
	}
}

// run 模式的实例没有任务说明：不拉起运行，事项转 needs_input 并说明缺什么，不用空文件冒充任务说明。
func TestRunModeWithoutInstructionNeedsInput(t *testing.T) {
	e := newEnv(t)
	e.harness("silent")
	e.instance("", map[string]any{"schedule": "24h", "mode": "run"})
	e.tick(t0.Add(24 * time.Hour))
	items := e.routineItems()
	if len(items) != 1 || len(e.runList()) != 0 {
		t.Fatalf("items %d runs %d", len(items), len(e.runList()))
	}
	if it := e.item(items[0].ID); it.Phase != domain.PhaseNeedsInput || it.Ask == nil || !strings.Contains(it.Ask.Question, "任务说明") {
		t.Fatalf("item: %s %+v", it.Phase, it.Ask)
	}
}
