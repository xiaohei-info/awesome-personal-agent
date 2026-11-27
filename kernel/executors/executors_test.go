package executors

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

func TestSameHostCardsResolveIndependently(t *testing.T) {
	e := setup(t)
	declaration := decl("host")
	declaration.MainSession = &MainSession{Args: []string{"--main"}}
	declaration.InstanceOptions = &InstanceOptions{
		Fields: map[string]OptionField{"variant": {Type: "enum", Label: Label{"模型", "Model"}, Choices: []string{"small", "large"}, DisplayAs: "model"}},
		Args:   map[string][]string{"variant": {"--model", "{variant}"}}, Env: map[string]map[string]string{"variant": {"HOST_MODEL": "{variant}"}},
	}
	e.must(t, owner, "executor.register", declaration)
	create := func(model string) Card {
		var card Card
		result := e.must(t, owner, "agent.configure", Card{Name: model, Harness: "host", Enabled: true, Values: map[string]string{"variant": model}})
		if err := json.Unmarshal(result.Value, &card); err != nil {
			t.Fatal(err)
		}
		return card
	}
	daily, main := create("small"), create("large")
	pol := policy.Default()
	pol.DefaultHarness = "host"
	pol.DefaultAgentID, pol.MainAgentID = &daily.ID, &main.ID
	pol.RuleAgent = map[string]string{"handoff": daily.ID}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		for _, test := range []struct {
			sel        Selection
			id, source string
		}{
			{Selection{}, daily.ID, "default"}, {Selection{Runner: "main_session"}, main.ID, "main"},
			{Selection{Rule: "handoff", Runner: "main_session"}, daily.ID, "rule"}, {Selection{Rule: "handoff", Schedule: main.ID}, main.ID, "schedule"},
			{Selection{Schedule: main.ID, Item: daily.ID}, daily.ID, "item"}, {Selection{Harness: "host"}, "legacy:host", "rule"},
		} {
			selected, err := Resolve(tx, pol, test.sel)
			if err != nil {
				return err
			}
			if selected.Reason != "" || selected.Snapshot.AgentID != test.id || selected.Snapshot.SelectionSource != test.source {
				t.Fatalf("%+v: %+v", test, selected)
			}
			if selected.Card.Rev > 0 && (selected.AccessReason("edit") == "" || len(selected.Args) != 2 || selected.Args[1] != selected.Snapshot.ConfiguredModel || selected.Env["HOST_MODEL"] != selected.Card.Name) {
				t.Fatalf("wrong options or inherited privileges: %+v", selected)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 默认不带人格和记忆：拉起参数追加声明的 headless.no_persona；本人开了就不追加。
	declaration.Headless.NoPersona = []string{"--bare"}
	e.must(t, owner, "executor.register", declaration)
	if err := e.st.View(bg, func(tx *store.Tx) error {
		for persona, want := range map[bool]bool{false: true, true: false} {
			pol.BackgroundPersona = persona
			selected, err := Resolve(tx, pol, Selection{})
			if err != nil {
				return err
			}
			if slices.Contains(selected.Args, "--bare") != want {
				t.Fatalf("persona=%v args=%v", persona, selected.Args)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 思考强度：声明的 {effort} 换成设置里"后台 Agent 运行"一项，没设取默认 medium。
	declaration.Headless.Effort = []string{"--think", "{effort}"}
	e.must(t, owner, "executor.register", declaration)
	if err := e.st.View(bg, func(tx *store.Tx) error {
		for set, want := range map[string]string{"": "medium", "high": "high"} {
			pol.Thinking.Background = set
			selected, err := Resolve(tx, pol, Selection{})
			if err != nil {
				return err
			}
			if i := slices.Index(selected.Args, "--think"); i < 0 || selected.Args[i+1] != want {
				t.Fatalf("thinking=%q args=%v", set, selected.Args)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	daily.Enabled = false
	e.must(t, owner, "agent.configure", daily)
	if err := e.st.View(bg, func(tx *store.Tx) error {
		selected, err := Resolve(tx, pol, Selection{Item: daily.ID, Schedule: main.ID})
		if selected.Reason == "" || selected.Snapshot.AgentID != daily.ID {
			t.Fatalf("disabled explicit selection: %+v", selected)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

var owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}

type env struct {
	st *store.Store
	x  *command.Executor
	au *auth.Auth
}

func setup(t *testing.T) *env {
	t.Helper()
	reg := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, Module} {
		if err := reg.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	st, err := store.Open(t.TempDir(), reg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	e := &env{st: st, au: auth.New(nil), x: command.New(st, nil)}
	projects := map[string]domain.Project{
		"restricted": {ID: "restricted", AllowedExecutors: []string{"acme.build"}},
		"open":       {ID: "open"},
		"selfonly":   {ID: "selfonly", AllowedExecutors: []string{}},
	}
	err = Register(e.x, Options{Owner: "me", Project: func(_ *store.Tx, id string) (domain.Project, error) {
		p, ok := projects[id]
		if !ok {
			return domain.Project{}, domain.NewError(domain.CodeNotFound, "project not found", "list projects")
		}
		return p, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) bearer(t *testing.T, s auth.Spec) command.Bearer {
	t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) { _, secret, err = e.au.Issue(tx, s); return }); err != nil {
		t.Fatal(err)
	}
	return command.Bearer{Auth: e.au, Secret: secret, Origin: auth.Origin{Session: "s1"}}
}

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	b, _ := json.Marshal(in)
	if s, ok := in.(string); ok {
		b = []byte(s)
	}
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b})
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) command.Result {
	t.Helper()
	r, err := e.do(id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func (e *env) list(t *testing.T) []Entry {
	t.Helper()
	var es []Entry
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { es, err = List(tx); return }); err != nil {
		t.Fatal(err)
	}
	return es
}

func (e *env) headless(t *testing.T, harness string) (Headless, bool) {
	t.Helper()
	var h Headless
	var ok bool
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { h, ok, err = ActiveHeadless(tx, harness); return }); err != nil {
		t.Fatal(err)
	}
	return h, ok
}

func (e *env) eventTypes(t *testing.T) []string {
	t.Helper()
	var out []string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, en := range es {
			out = append(out, en.Event.Type)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func decl(harness string, execs ...Executor) Declaration {
	return Declaration{
		Harness: harness,
		Headless: &Headless{
			Command: []string{"acme", "run", "{prompt}", "--dir", "{run_dir}"},
			Session: Session{Extract: "$.session_id"},
			Resume:  []string{"--resume", "{session_id}"},
			Access:  map[string][]string{"read": {"--ro"}, "edit": {"--rw"}},
		},
		Executors: execs,
	}
}

var (
	build = Executor{ID: "acme.build", Kind: KindExternalCLI, Scenarios: []string{"code"}, Host: "work-host"}
	chat  = Executor{ID: "acme.chat", Kind: KindNativeSubagent}
)

// flow-command-registration: 插件提交的声明和执行者先进入待确认，确认前不生效；本人确认后才进入清单。
func TestDeclaredStaysPendingUntilOwnerConfirms(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker, Label: "plugin"})

	res := e.must(t, worker, "executor.declare", decl("acme", build, chat))
	if got := string(res.Value); got != `{"harness":"acme","rev":1}` {
		t.Fatalf("declare result %s", got)
	}
	if len(e.list(t)) != 0 {
		t.Fatalf("pending executors must not be in the list: %+v", e.list(t))
	}
	if _, ok := e.headless(t, "acme"); ok {
		t.Fatal("pending headless declaration must not take effect")
	}
	var pend struct{ Declarations []Record }
	_ = json.Unmarshal(e.must(t, worker, "executor.pending", "").Value, &pend)
	if len(pend.Declarations) != 1 || pend.Declarations[0].State != StatePending || len(pend.Declarations[0].Declaration.Executors) != 2 {
		t.Fatalf("pending view %+v", pend)
	}

	e.must(t, owner, "executor.confirm", ref{"acme", 1})
	got := e.list(t)
	if len(got) != 2 || got[0].ID != "acme.build" || got[0].Host != "work-host" || got[0].Scenarios[0] != "code" || got[1].ID != "acme.chat" {
		t.Fatalf("list after confirm %+v", got)
	}
	h, ok := e.headless(t, "acme")
	if !ok || h.Command[0] != "acme" || h.Access["edit"][0] != "--rw" {
		t.Fatalf("headless after confirm %+v %v", h, ok)
	}
	want := []string{EvDeclared, EvConfirmed}
	if types := e.eventTypes(t); len(types) != 2 || types[0] != want[0] || types[1] != want[1] {
		t.Fatalf("events %v", types)
	}
}

// 登记类命令只有本人在 Web 上能做（§14.12）；CLI 凭据、delegate、ext 都不行，且不留下任何生效数据。
func TestOnlyOwnerCanConfirmOrRegister(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	delegate := e.bearer(t, auth.Spec{Kind: auth.Delegate})
	observer := e.bearer(t, auth.Spec{Kind: auth.Observer})
	ext := e.bearer(t, auth.Spec{Kind: auth.Ext, Scope: "chan"})
	e.must(t, worker, "executor.declare", decl("acme", build))

	// "Agent 与执行者配置"组：没开这一组的 delegate 是 23，worker、observer 在钥匙范围外是 4。
	for name, id := range map[string]command.Identity{"worker": worker, "delegate": delegate, "observer": observer} {
		want := domain.ExitPermissionDenied
		if name == "delegate" {
			want = domain.ExitTierClosed
		}
		for cmd, in := range map[string]any{
			"executor.confirm": ref{"acme", 1}, "executor.register": decl("other", chat),
			"executor.put": chat, "executor.remove": idIn{"acme.build"},
		} {
			if _, err := e.do(id, cmd, in); exit(err) != want {
				t.Errorf("%s %s: exit %d (%v), want %d", name, cmd, exit(err), err, want)
			}
		}
	}
	if _, err := e.do(observer, "executor.declare", decl("x", chat)); exit(err) != domain.ExitPermissionDenied {
		t.Errorf("observer declare: %v", err)
	}
	if _, err := e.do(ext, "executor.declare", decl("x", chat)); exit(err) != domain.ExitPermissionDenied {
		t.Errorf("ext declare: %v", err)
	}
	if len(e.list(t)) != 0 {
		t.Fatalf("nothing may take effect: %+v", e.list(t))
	}
}

func TestRejectedDeclarationNeverTakesEffect(t *testing.T) {
	e := setup(t)
	delegate := e.bearer(t, auth.Spec{Kind: auth.Delegate})
	e.must(t, delegate, "executor.declare", decl("acme", build))
	e.must(t, owner, "executor.reject", ref{"acme", 1})
	if _, err := e.do(owner, "executor.confirm", ref{"acme", 1}); exit(err) != domain.ExitConflict {
		t.Fatalf("confirm after reject: %v", err)
	}
	if _, err := e.do(owner, "executor.confirm", ref{"acme", 9}); exit(err) != domain.ExitNotFound {
		t.Fatalf("confirm unknown rev: %v", err)
	}
	if len(e.list(t)) != 0 {
		t.Fatal("rejected declaration must not take effect")
	}
}

func TestNewDeclarationSupersedesAndMergesHarnessEntries(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	e.must(t, worker, "executor.declare", decl("acme", build, chat))
	e.must(t, worker, "executor.declare", decl("acme", build)) // rev 2 取代未确认的 rev 1
	if _, err := e.do(owner, "executor.confirm", ref{"acme", 1}); exit(err) != domain.ExitConflict {
		t.Fatalf("superseded rev must not be confirmable: %v", err)
	}
	e.must(t, owner, "executor.confirm", ref{"acme", 2})
	if got := e.list(t); len(got) != 1 || got[0].ID != "acme.build" {
		t.Fatalf("list %+v", got)
	}

	// 已生效后再声明：确认前不变；确认后并入名录，已登记的条目不删除（§6.17）。
	e.must(t, worker, "executor.declare", decl("acme", chat))
	if got := e.list(t); len(got) != 1 || got[0].ID != "acme.build" {
		t.Fatalf("active must stay until confirmed: %+v", got)
	}
	e.must(t, owner, "executor.confirm", ref{"acme", 3})
	if got := e.list(t); len(got) != 2 || got[0].ID != "acme.build" || got[1].ID != "acme.chat" {
		t.Fatalf("list after merge %+v", got)
	}
	var active []Record
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { active, err = Records(tx, StateActive); return })
	if len(active) != 1 || active[0].Rev != 3 {
		t.Fatalf("exactly one active declaration expected: %+v", active)
	}
}

func TestInvalidDeclarationsAreRejectedAndStoreNothing(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	mod := func(f func(*Declaration)) Declaration { d := decl("acme", build); f(&d); return d }
	cases := map[string]any{
		"no harness":          mod(func(d *Declaration) { d.Harness = "" }),
		"nothing declared":    Declaration{Harness: "acme"},
		"empty command":       mod(func(d *Declaration) { d.Headless.Command = nil }),
		"unknown placeholder": mod(func(d *Declaration) { d.Headless.Command = []string{"acme", "{token}"} }),
		"both session modes":  mod(func(d *Declaration) { d.Headless.Session.Assign = []string{"--sid"} }),
		"no session mode":     mod(func(d *Declaration) { d.Headless.Session = Session{} }),
		"no access tier":      mod(func(d *Declaration) { d.Headless.Access = nil }),
		"bad access tier":     mod(func(d *Declaration) { d.Headless.Access["root"] = nil }),
		"bad credential":      mod(func(d *Declaration) { d.Headless.Credential = "argv" }),
		"output without code": mod(func(d *Declaration) { d.Headless.ExitOutput = map[string][]string{"session_missing": {"gone"}} }),
		"empty output mark": mod(func(d *Declaration) {
			d.Headless.ExitCodes = map[string][]int{"session_missing": {1}}
			d.Headless.ExitOutput = map[string][]string{"session_missing": {""}}
		}),
		"bad launch": mod(func(d *Declaration) { d.MainSession = &MainSession{Launch: "daemon"} }),
		"missing while wake": mod(func(d *Declaration) {
			d.MainSession = &MainSession{Wake: true, WakeMissing: []WakeGap{{Label{"甲", "a"}, Label{"乙", "b"}}}}
		}),
		"half wake gap": mod(func(d *Declaration) {
			d.MainSession = &MainSession{WakeMissing: []WakeGap{{Label: Label{"甲", "a"}, How: Label{Zh: "乙"}}}}
		}),
		"empty selfcheck":     mod(func(d *Declaration) { d.Selfcheck = &Selfcheck{} }),
		"half name":           mod(func(d *Declaration) { d.Name = &Label{Zh: "甲"} }),
		"tools on undeclared": mod(func(d *Declaration) { d.Headless.Tools = map[string][]Label{"full": {{Zh: "终端", En: "Terminal"}}} }),
		"half tool":           mod(func(d *Declaration) { d.Headless.Tools = map[string][]Label{"edit": {{Zh: "终端"}}} }),
		"note on undeclared":  mod(func(d *Declaration) { d.Headless.ToolNotes = map[string]Label{"full": {Zh: "甲", En: "a"}} }),
		"empty selfcheck arg": mod(func(d *Declaration) { d.Selfcheck = &Selfcheck{Command: []string{"check", ""}} }),
		"bad kind":            mod(func(d *Declaration) { d.Executors[0].Kind = "robot" }),
		"empty id":            mod(func(d *Declaration) { d.Executors[0].ID = "" }),
		"duplicate id":        mod(func(d *Declaration) { d.Executors = append(d.Executors, build) }),
		"unknown field":       `{"harness":"acme","executors":[{"id":"a","kind":"human","invoke":"rm -rf /"}]}`,
		"not json":            `harness: acme`,
	}
	for name, in := range cases {
		if _, err := e.do(worker, "executor.declare", in); exit(err) != domain.ExitUsage {
			t.Errorf("%s: exit %d (%v), want %d", name, exit(err), err, domain.ExitUsage)
		}
	}
	var recs []Record
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { recs, err = Records(tx, ""); return })
	if len(recs) != 0 {
		t.Fatalf("invalid declarations stored: %+v", recs)
	}
}

func TestOwnerMaintainsList(t *testing.T) {
	e := setup(t)
	e.must(t, owner, "executor.put", Executor{ID: "me", Kind: KindHuman})
	e.must(t, owner, "executor.put", Executor{ID: "me", Kind: KindHuman, Host: "laptop", Scenarios: []string{"review"}})
	if got := e.list(t); len(got) != 1 || got[0].Host != "laptop" || got[0].Scenarios[0] != "review" {
		t.Fatalf("update %+v", got)
	}
	if _, err := e.do(owner, "executor.put", Executor{ID: "x", Kind: "robot"}); exit(err) != domain.ExitUsage {
		t.Fatalf("bad kind: %v", err)
	}

	// id 不能被插件声明悄悄占用，已声明的项也不能被 put 覆盖。
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	if _, err := e.do(worker, "executor.declare", decl("acme", Executor{ID: "me", Kind: KindExternalCLI})); exit(err) != domain.ExitConflict {
		t.Fatalf("declare over owner entry: %v", err)
	}
	e.must(t, worker, "executor.declare", decl("acme", build))
	e.must(t, owner, "executor.confirm", ref{"acme", 1})
	if _, err := e.do(owner, "executor.put", Executor{ID: "acme.build", Kind: KindHuman}); exit(err) != domain.ExitConflict {
		t.Fatalf("put over declared entry: %v", err)
	}

	e.must(t, owner, "executor.remove", idIn{"me"})
	if _, err := e.do(owner, "executor.remove", idIn{"me"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("remove twice: %v", err)
	}
	if got := e.list(t); len(got) != 1 || got[0].ID != "acme.build" {
		t.Fatalf("list %+v", got)
	}
}

// flow-env-baseline: 本人写在主机上的配置文件直接登记，立即生效；重复登记同一份文件不产生新版本。
func TestHostFileRegistration(t *testing.T) {
	e := setup(t)
	path := filepath.Join(t.TempDir(), "acme.json")
	doc, _ := json.Marshal(decl("acme", build))
	if err := os.WriteFile(path, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	r1, err := RegisterHostFile(bg, e.x, owner, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.list(t); len(got) != 1 || got[0].ID != "acme.build" {
		t.Fatalf("list %+v", got)
	}
	if _, ok := e.headless(t, "acme"); !ok {
		t.Fatal("host-registered headless declaration must be active")
	}
	r2, err := RegisterHostFile(bg, e.x, owner, path)
	if err != nil || string(r1.Value) != string(r2.Value) {
		t.Fatalf("re-register must be a no-op: %s vs %s (%v)", r1.Value, r2.Value, err)
	}
	if types := e.eventTypes(t); len(types) != 1 || types[0] != EvConfirmed {
		t.Fatalf("events %v", types)
	}

	// 改了文件内容就是新版本，旧版本退位。
	doc, _ = json.Marshal(decl("acme", chat))
	_ = os.WriteFile(path, doc, 0o600)
	if r, err := RegisterHostFile(bg, e.x, owner, path); err != nil || string(r.Value) != `{"harness":"acme","rev":2}` {
		t.Fatalf("changed file: %s %v", r.Value, err)
	}
	if got := e.list(t); len(got) != 2 || got[1].ID != "acme.chat" {
		t.Fatalf("list %+v", got)
	}

	delegate := e.bearer(t, auth.Spec{Kind: auth.Delegate})
	if _, err := RegisterHostFile(bg, e.x, delegate, path); exit(err) != domain.ExitTierClosed {
		t.Fatalf("non-owner host registration: %v", err)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte(`{"harness":"zzz"}`), 0o600)
	if _, err := RegisterHostFile(bg, e.x, owner, bad); exit(err) != domain.ExitUsage {
		t.Fatalf("bad file: %v", err)
	}
	if _, err := RegisterHostFile(bg, e.x, owner, filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file must surface the real error: %v", err)
	}
}

// flow-delegate-recover: 委派以清单为准——清单外的执行者查不到（退出码 5 由委派模块据 Get 返回），
// 带项目列出时标明项目名单是否允许，本人始终允许。
func TestListMarksProjectAllowedAndLookupFindsOnlyListed(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	e.must(t, owner, "executor.put", Executor{ID: "me", Kind: KindHuman})
	e.must(t, worker, "executor.declare", decl("acme", build, chat))
	e.must(t, owner, "executor.confirm", ref{"acme", 1})

	allowed := func(project string) map[string]*bool {
		var out struct{ Executors []listed }
		in := map[string]string{"project": project}
		if err := json.Unmarshal(e.must(t, worker, "executor.list", in).Value, &out); err != nil {
			t.Fatal(err)
		}
		m := map[string]*bool{}
		for _, x := range out.Executors {
			m[x.ID] = x.Allowed
		}
		return m
	}
	r := allowed("restricted")
	if *r["acme.build"] != true || *r["acme.chat"] != false || *r["me"] != true {
		t.Fatalf("restricted: build=%v chat=%v me=%v", *r["acme.build"], *r["acme.chat"], *r["me"])
	}
	if o := allowed("open"); *o["acme.chat"] != true {
		t.Fatal("unrestricted project allows everyone")
	}
	if s := allowed("selfonly"); *s["acme.build"] != false || *s["me"] != true {
		t.Fatal("empty allow-list means owner only")
	}
	var plain struct{ Executors []listed }
	_ = json.Unmarshal(e.must(t, worker, "executor.list", "").Value, &plain)
	if len(plain.Executors) != 3 || plain.Executors[0].Allowed != nil {
		t.Fatalf("unscoped list %+v", plain)
	}
	if _, err := e.do(worker, "executor.list", map[string]string{"project": "nope"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("unknown project: %v", err)
	}

	_ = e.st.View(bg, func(tx *store.Tx) error {
		if _, ok, _ := Get(tx, "ghost"); ok {
			t.Error("executor outside the list must not be found")
		}
		if x, ok, _ := Get(tx, "acme.build"); !ok || x.Host != "work-host" {
			t.Errorf("Get %+v %v", x, ok)
		}
		return nil
	})
}

func TestDeclareIsIdempotentWithKey(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	in, _ := json.Marshal(decl("acme", build))
	req := command.Request{Identity: worker, Command: "executor.declare", Input: in, IdemKey: "k1"}
	r1, err := e.x.Do(bg, req)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := e.x.Do(bg, req)
	if err != nil || !r2.Replayed || string(r1.Value) != string(r2.Value) {
		t.Fatalf("replay: %+v %v", r2, err)
	}
	var recs []Record
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { recs, err = Records(tx, ""); return })
	if len(recs) != 1 {
		t.Fatalf("replay must not add a revision: %+v", recs)
	}
}

// ACT-SKD-14 的前提：声明可以带 selfcheck.command，确认后随声明原样取回（内核只存数据，不执行）。
func TestDeclarationCarriesSelfcheckCommand(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	d := decl("acme", build)
	d.Selfcheck = &Selfcheck{Command: []string{"acme-selfcheck", "--json"}}
	if _, err := e.do(worker, "executor.declare", d); err != nil {
		t.Fatal(err)
	}
	var pending []Record
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { pending, err = Records(tx, StatePending); return })
	if len(pending) != 1 || pending[0].Declaration.Selfcheck == nil || pending[0].Declaration.Selfcheck.Command[0] != "acme-selfcheck" {
		t.Fatalf("selfcheck not stored with the declaration: %+v", pending)
	}
}

// 补充 B：插件提交新版本时，仍待确认的旧版本被取代；本人只看到、只确认最新一份。
func TestPendingRevisionIsSupersededByNewOne(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	e.must(t, worker, "executor.declare", decl("acme", build))
	d2 := decl("acme", build)
	d2.Name = &Label{Zh: "Acme", En: "Acme"}
	d2.Headless.Tools = map[string][]Label{"edit": {{Zh: "终端", En: "Terminal"}}}
	e.must(t, worker, "executor.declare", d2)

	var pend struct{ Declarations []Record }
	_ = json.Unmarshal(e.must(t, worker, "executor.pending", "").Value, &pend)
	if len(pend.Declarations) != 1 || pend.Declarations[0].Rev != 2 || pend.Declarations[0].Declaration.Name.Zh != "Acme" {
		t.Fatalf("only rev 2 may be pending: %+v", pend.Declarations)
	}
	e.must(t, owner, "executor.confirm", ref{"acme", 2})
	var all []Record
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { all, err = Records(tx, ""); return })
	if len(all) != 2 || all[0].State != StateSuperseded || all[1].State != StateActive {
		t.Fatalf("rev 1 superseded, rev 2 active: %+v", all)
	}
}

// 重复提交同样内容不产生新版本：待确认的不重复，已生效的不再要本人确认。
func TestSameDeclarationDoesNotMakeANewRevision(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	e.must(t, worker, "executor.declare", decl("acme", build))
	if got := string(e.must(t, worker, "executor.declare", decl("acme", build)).Value); got != `{"harness":"acme","rev":1}` {
		t.Fatalf("same pending content: %s", got)
	}
	e.must(t, owner, "executor.confirm", ref{"acme", 1})
	if got := string(e.must(t, worker, "executor.declare", decl("acme", build)).Value); got != `{"harness":"acme","rev":1}` {
		t.Fatalf("same active content: %s", got)
	}
	var pend struct{ Declarations []Record }
	_ = json.Unmarshal(e.must(t, worker, "executor.pending", "").Value, &pend)
	if len(pend.Declarations) != 0 {
		t.Fatalf("nothing should wait for the owner: %+v", pend.Declarations)
	}
	if n := len(e.eventTypes(t)); n != 2 {
		t.Fatalf("only declared + confirmed events expected, got %v", e.eventTypes(t))
	}
}

// 插件自动同步：开→关（待确认 wake:false）→又开回（等于已生效）时，过时的待确认版本作废，本人不必处理。
func TestDeclaringTheActiveContentSupersedesThePendingOne(t *testing.T) {
	e := setup(t)
	worker := e.bearer(t, auth.Spec{Kind: auth.Worker})
	on := decl("acme")
	on.MainSession = &MainSession{Wake: true}
	e.must(t, worker, "executor.declare", on)
	e.must(t, owner, "executor.confirm", ref{"acme", 1})
	off := decl("acme")
	off.MainSession = &MainSession{WakeMissing: []WakeGap{{Label{"还没打开唤醒", "Wake-up is off"}, Label{"打开它", "Turn it on"}}}}
	e.must(t, worker, "executor.declare", off)
	var pend struct{ Declarations []Record }
	_ = json.Unmarshal(e.must(t, worker, "executor.pending", "").Value, &pend)
	if len(pend.Declarations) != 1 || pend.Declarations[0].Declaration.MainSession.WakeMissing[0].How.Zh != "打开它" {
		t.Fatalf("wake_missing must be stored with the pending declaration: %+v", pend.Declarations)
	}
	if got := string(e.must(t, worker, "executor.declare", on).Value); got != `{"harness":"acme","rev":1}` {
		t.Fatalf("back to the active content: %s", got)
	}
	pend.Declarations = nil
	_ = json.Unmarshal(e.must(t, worker, "executor.pending", "").Value, &pend)
	if len(pend.Declarations) != 0 {
		t.Fatalf("the stale pending declaration must be superseded: %+v", pend.Declarations)
	}
}

// Web「接入其他 Agent」表单能生成的最小声明（不续接：默认 extract；单档、空参数）被内核接受（web/src/features/settings-agents/declaration-form.ts）。
func TestFormMinimalDeclarationIsAccepted(t *testing.T) {
	d, err := ParseDeclaration([]byte(`{"harness":"acme-cli","name":{"zh":"甲","en":"Acme CLI"},
		"headless":{"command":["acme","run","{prompt}"],"session":{"extract":"$.session_id"},"access":{"read":[]}},
		"main_session":{"wake":true}}`))
	if err != nil || !d.MainSession.CanWake() || len(d.Headless.Access) != 1 {
		t.Fatalf("form declaration: %+v %v", d, err)
	}
}

func TestHeadlessFinalTextValidationAndExtraction(t *testing.T) {
	if _, err := ParseDeclaration([]byte(`{"harness":"bad","headless":{"command":["x"],"output":["--format","stream-json"],"final_text":"text","session":{"extract":"$.s"},"access":{"read":[]}}}`)); err == nil {
		t.Fatal("invalid final_text path should be rejected")
	}
	h := Headless{Output: []string{"--format", "stream-json"}, FinalText: "$.payload.summary"}
	out := []byte("log line\n{\"type\":\"tool_use\",\"name\":\"read\",\"payload\":{\"summary\":\"LEAK\"}}\n{\"type\":\"result\",\"payload\":{\"summary\":\"最终结论\"}}\n")
	if got := h.ExtractFinalText(out); got != "最终结论" {
		t.Fatalf("ExtractFinalText = %q, want 最终结论", got)
	}
	toolOnly := []byte("log line\n{\"type\":\"tool_result\",\"output\":\"raw-secret\"}\n")
	if got := h.ExtractFinalText(toolOnly); got != "" {
		t.Fatalf("tool-only stream-json must not leak raw logs, got %q", got)
	}
}
