package background

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/attention"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delegation"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/items"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

// startScript 记下收到的参数，在 stdout 写一个带会话 id 与回报结构的 JSON 对象（执行者不经运行凭据回执）。
const startScript = `printf '%s\n' "$@" > "$2/argv"
printf '{"agentId":"agt-9","outcome":"done","summary":"did it","pr":"https://git.example/o/r/pull/%s"}\n' "$$"
`

// declareStartable 登记一份声明：两个执行者共用它（一个在本机、一个在别的机器），profile 是执行者参数，模型可按次覆盖。
func (e *env) declareStartable() {
	e.t.Helper()
	path := filepath.Join(e.dir, "px.sh")
	if err := os.WriteFile(path, []byte(startScript), 0o700); err != nil {
		e.t.Fatal(err)
	}
	e.must(owner, "executor.register", map[string]any{"harness": "px",
		"executor_args": map[string]any{"profile": []string{"--profile", "{profile}"}},
		"overrides": map[string]any{"model": map[string]any{"label": map[string]string{"zh": "模型", "en": "Model"},
			"args": []string{"--model", "{model}"}, "choices": []string{"m1", "m2"}, "role": "model"}},
		"headless": map[string]any{"command": []string{"sh", path, "{prompt}", "{run_dir}", "--ctx={run_context}", "--label=apa_run={run_id}"},
			"session": map[string]any{"extract": "$.agentId"}, "resume": []string{"--resume", "{session_id}"},
			"access": map[string]any{"read": []string{}, "edit": []string{}}, "remote_args": []string{"--schema", "{report_schema}"},
			"cancel": map[string]any{"command": []string{"true", "{run_id}"}}},
		"executors": []map[string]any{
			{"id": "px.quick", "kind": "external-cli", "handle": map[string]string{"profile": "quick"}},
			{"id": "px.far", "kind": "remote-agent", "host": "far", "handle": map[string]string{"profile": "deep"}}}})
}

func (e *env) argvOf(run string) []string {
	e.t.Helper()
	b, err := os.ReadFile(filepath.Join(e.runs, run, "argv"))
	if err != nil {
		e.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// 路径 A（§7.5、§13.6）：委派即开工按执行者的声明拉起，执行者参数取自句柄，覆盖值按 单次 > 执行者默认 取声明允许的值；
// 拉起时写委派记录（run 非空），运行结束补上会话句柄；stdout 的回报结构兜底回执，回报同时成为父事项的进展。
func TestDelegateStartRunsTheExecutorFromItsDeclaration(t *testing.T) {
	e := setup(t, 0)
	e.svc.Attach(e.x)
	e.declareStartable()
	e.must(owner, "executor.override", map[string]any{"id": "px.quick", "values": map[string]string{"model": "m1"}})
	parent := e.newItem("ship it", nil, nil)
	e.open(parent)

	if _, err := e.do(owner, "delegate.create", map[string]any{"parent": parent, "assignee": "px.quick", "start": true,
		"overrides": map[string]string{"model": "gpt-free-text"}}, command.Chain{}); err == nil {
		t.Fatal("an override outside the declared choices must be refused")
	}
	if p := e.item(parent); p.Phase != domain.PhaseOpen {
		t.Fatalf("a refused start writes nothing, parent is %s", p.Phase)
	}

	var out struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "delegate.create", map[string]any{"parent": parent, "assignee": "px.quick", "title": "README", "start": true}).Value, &out)
	res := e.waitResult()
	argv := e.argvOf(res.Run.ID)
	if !slices.Contains(argv, "--profile") || argv[slices.Index(argv, "--profile")+1] != "quick" || argv[slices.Index(argv, "--model")+1] != "m1" {
		t.Fatalf("executor args and the executor's default model: %q", argv)
	}
	if slices.Contains(argv, "--schema") || !slices.Contains(argv, "--label=apa_run="+res.Run.ID) {
		t.Fatalf("a local executor gets no remote args, and its run id: %q", argv)
	}
	if res.Run.AgentSnapshot == nil || res.Run.AgentSnapshot.ConfiguredModel != "m1" || res.Run.Harness != "px" {
		t.Fatalf("the run records the model it ran with: %+v", res.Run.AgentSnapshot)
	}
	var d delegation.Dispatch
	var prog []domain.Progress
	if err := e.st.View(bg, func(tx *store.Tx) (err error) {
		if d, _, err = delegation.ByRun(tx, res.Run.ID); err != nil {
			return err
		}
		prog, err = items.Progress(tx, parent)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if d.Item != out.ID || d.Run != res.Run.ID || d.Session != "agt-9" || !d.Resumable || d.State != delegation.DispatchEnded || d.Harness != "px" {
		t.Fatalf("dispatch record of the started run: %+v", d)
	}
	if c := e.item(out.ID); c.Phase != domain.PhaseDone {
		t.Fatalf("the stdout report closes the child: %s", c.Phase)
	}
	if p := e.item(parent); p.Phase != domain.PhaseOpen || len(prog) != 1 || !strings.Contains(prog[0].Text, "did it") {
		t.Fatalf("parent back to open with the report as progress: %s %+v", p.Phase, prog)
	}

	// 单次覆盖先于执行者默认；执行者不在 APA 主机上：不发运行上下文，追加 remote_args，正文随提示词带过去。
	e.must(owner, "delegate.create", map[string]any{"parent": parent, "assignee": "px.far", "body": "SPEC-TEXT", "start": true,
		"overrides": map[string]string{"model": "m2"}})
	res = e.waitResult()
	argv = e.argvOf(res.Run.ID)
	if argv[slices.Index(argv, "--model")+1] != "m2" || argv[slices.Index(argv, "--profile")+1] != "deep" || !slices.Contains(argv, "--ctx=") ||
		argv[slices.Index(argv, "--schema")+1] != filepath.Join(e.runs, res.Run.ID, executors.ReportSchemaFile) || !strings.Contains(strings.Join(argv, "\n"), "SPEC-TEXT") {
		t.Fatalf("remote executor argv: %q", argv)
	}
}

// 委派结果通知（A8，§13.6）：APA 拉起的委派有了结果，通知发起方——本人在 Web 发起的推本人层（没有默认渠道时 Web 未读）；
// 一次回执一条，换一个触达实例（相当于重启后重新追赶事件）也不再入队；外部 harness 自己派出的（没有运行 id）不推。
func TestStartedDelegationResultNotifiesWhoStartedIt(t *testing.T) {
	e := setup(t, 0)
	e.svc.Attach(e.x)
	e.declareStartable()
	parent := e.newItem("ship it", nil, nil)
	e.open(parent)
	var started, plain struct{ ID string }
	_ = json.Unmarshal(e.must(owner, "delegate.create", map[string]any{"parent": parent, "assignee": "px.quick", "title": "README", "start": true}).Value, &started)
	e.waitResult()
	_ = json.Unmarshal(e.must(owner, "delegate.create", map[string]any{"parent": parent, "assignee": "work-host.cli", "title": "by hand"}).Value, &plain)
	e.must(owner, "delegate.report", map[string]any{"id": plain.ID, "outcome": "done", "summary": "done by hand", "evidence": map[string]string{"link": "https://x.example/1"}})

	results := func() (ds []domain.Delivery) {
		a := attention.New(attention.Options{Queue: delivery.New(delivery.Options{Now: e.clock}), Now: e.clock})
		if err := e.st.Update(bg, a.Sync); err != nil {
			t.Fatal(err)
		}
		if err := e.st.View(bg, func(tx *store.Tx) error {
			all, err := delivery.Since(tx, t0.Add(-time.Hour))
			for _, s := range all {
				if err == nil && strings.HasPrefix(s.ID, delivery.ResultPrefix) {
					var d domain.Delivery
					d, err = delivery.New(delivery.Options{}).Get(tx, s.ID)
					ds = append(ds, d)
				}
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return ds
	}
	ds := results()
	if len(ds) != 1 || ds[0].Item != started.ID || ds[0].Route != delivery.RouteWeb || !strings.Contains(ds[0].Content.Title, "README") || ds[0].Content.Summary != "did it" {
		t.Fatalf("one result notice for the started delegation, to the owner: %+v", ds)
	}
	if again := results(); len(again) != 1 || again[0].ID != ds[0].ID {
		t.Fatalf("catching up again (after a restart) must not queue it twice: %+v", again)
	}
}

// 一次卡住只通知一次（§7.4 delegation_result）：执行者报"卡住"、或运行结束没交回结果（内核代记"卡住"），发起方只收到一条委派结果通知；
// 不另转"等你决定"问本人，也不再按运行失败推送。
func TestStartedDelegationStuckNotifiesOnce(t *testing.T) {
	for name, out := range map[string]string{
		"executor reports blocked": `printf '{"outcome":"blocked","summary":"need creds"}\n'`,
		"run returns nothing":      `exit 0`,
	} {
		t.Run(name, func(t *testing.T) {
			e := setup(t, 0)
			e.svc.Attach(e.x)
			e.declareStartable()
			if err := os.WriteFile(filepath.Join(e.dir, "px.sh"), []byte(out+"\n"), 0o700); err != nil {
				t.Fatal(err)
			}
			parent := e.newItem("ship it", nil, nil)
			e.open(parent)
			var child struct{ ID string }
			_ = json.Unmarshal(e.must(owner, "delegate.create", map[string]any{"parent": parent, "assignee": "px.quick", "title": "README", "start": true}).Value, &child)
			e.waitResult()
			if c := e.item(child.ID); c.Phase != domain.PhaseOpen || c.Ask != nil || !strings.HasPrefix(c.StatusLabel, "blocked: ") {
				t.Fatalf("the child stays open with a blocked receipt, no question: %s %+v %q", c.Phase, c.Ask, c.StatusLabel)
			}
			a := attention.New(attention.Options{Queue: delivery.New(delivery.Options{Now: e.clock}), Now: e.clock})
			if err := e.st.Update(bg, a.Sync); err != nil {
				t.Fatal(err)
			}
			var ids []string
			if err := e.st.View(bg, func(tx *store.Tx) error {
				all, err := delivery.Since(tx, t0.Add(-time.Hour))
				for _, s := range all {
					if s.Item == child.ID {
						ids = append(ids, s.ID)
					}
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
			if len(ids) != 1 || !strings.HasPrefix(ids[0], delivery.ResultPrefix) {
				t.Fatalf("exactly one notice for the stuck child, the delegation result: %v", ids)
			}
			// 子事项放弃后，还没读的"卡住了"不再留在 Web 未读。
			e.must(owner, "item.transition", map[string]any{"id": child.ID, "to": "dropped", "reason": "not needed"})
			if err := e.st.Update(bg, a.Sync); err != nil {
				t.Fatal(err)
			}
			if err := e.st.View(bg, func(tx *store.Tx) error {
				d, err := delivery.New(delivery.Options{}).Get(tx, ids[0])
				if err == nil && d.State != domain.DeliverySuperseded {
					t.Errorf("the unread stuck notice of a dropped child is withdrawn, state = %s", d.State)
				}
				return err
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}
