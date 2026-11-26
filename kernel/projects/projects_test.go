package projects

import (
	"context"
	"encoding/json"
	"errors"
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

var owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}
var delegate = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "dlg", Via: domain.ViaCLI, Session: "s1", SessionKind: "direct", OnBehalfOf: "owner"}, Level: authz.LevelDelegate}
var worker = command.Trusted{Actor: domain.Actor{Type: domain.ActorAgent, Principal: "wrk", Via: domain.ViaCLI}, Level: authz.LevelWorker}

type env struct {
	st *store.Store
	x  *command.Executor
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
	x := command.New(st, nil)
	if err := Register(x); err != nil {
		t.Fatal(err)
	}
	return &env{st: st, x: x}
}

func (e *env) do(t *testing.T, id command.Identity, cmd string, in any) (command.Result, error) {
	t.Helper()
	b, _ := json.Marshal(in)
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: b, Owner: "owner"})
}

func (e *env) must(t *testing.T, id command.Identity, cmd string, in any) command.Result {
	t.Helper()
	r, err := e.do(t, id, cmd, in)
	if err != nil {
		t.Fatalf("%s: %v", cmd, err)
	}
	return r
}

func (e *env) create(t *testing.T, name string) string {
	t.Helper()
	var out struct{ ID string }
	r := e.must(t, owner, "project.create", map[string]any{"name": name})
	if err := json.Unmarshal(r.Value, &out); err != nil || out.ID == "" {
		t.Fatalf("create result %s", r.Value)
	}
	return out.ID
}

func (e *env) get(t *testing.T, id string) domain.Project {
	t.Helper()
	var p domain.Project
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { p, err = Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return p
}

func (e *env) events(t *testing.T) []domain.Event {
	t.Helper()
	var out []domain.Event
	_ = e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			out = append(out, x.Event)
		}
		return err
	})
	return out
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func TestCreateAndUpdatePersist(t *testing.T) {
	e := setup(t)
	r := e.must(t, owner, "project.create", map[string]any{"name": "Work", "intro": "i", "tags": []string{"a", "b"}})
	var out struct{ ID string }
	_ = json.Unmarshal(r.Value, &out)
	p := e.get(t, out.ID)
	if p.Name != "Work" || p.Status != domain.ProjectActive || !p.Capture || len(p.Tags) != 2 || p.AllowedExecutors != nil {
		t.Fatalf("created project: %+v", p)
	}
	ev := e.events(t)
	if len(ev) != 1 || ev[0].Type != domain.EvProjectUpdated || ev[0].Object != out.ID {
		t.Fatalf("events: %+v", ev)
	}

	// 改项目信息是一档；对话捕获开关是 3a，档位只整理的 delegate 得 23。
	e.must(t, delegate, "project.update", map[string]any{"id": out.ID, "status": "paused", "name": "Work2"})
	if _, err := e.do(t, delegate, "project.update", map[string]any{"id": out.ID, "capture": true}); exit(err) != domain.ExitTierClosed {
		t.Fatalf("delegate capture switch: %v", err)
	}
	e.must(t, owner, "project.update", map[string]any{"id": out.ID, "capture": true})
	p = e.get(t, out.ID)
	if p.Status != domain.ProjectPaused || !p.Capture || p.Name != "Work2" || p.Intro != "i" || len(p.Tags) != 2 {
		t.Fatalf("patch must only change given fields: %+v", p)
	}

	for name, in := range map[string]map[string]any{
		"empty name":   {"id": out.ID, "name": " "},
		"bad status":   {"id": out.ID, "status": "gone"},
		"empty tag":    {"id": out.ID, "tags": []string{""}},
		"no change":    {"id": out.ID},
		"extra field":  {"id": out.ID, "allowed_executors": []string{"x"}},
		"repos in upd": {"id": out.ID, "repos": []any{}},
	} {
		if _, err := e.do(t, owner, "project.update", in); exit(err) != domain.ExitUsage {
			t.Errorf("%s: want usage error, got %v", name, err)
		}
	}
	if _, err := e.do(t, owner, "project.update", map[string]any{"id": "nope", "name": "x"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("missing project: %v", err)
	}
	if _, err := e.do(t, owner, "project.create", map[string]any{"name": ""}); exit(err) != domain.ExitUsage {
		t.Fatalf("empty name: %v", err)
	}
	if _, err := e.do(t, owner, "project.create", map[string]any{"name": "x", "status": "weird"}); exit(err) != domain.ExitUsage {
		t.Fatalf("bad status: %v", err)
	}
	if _, err := e.do(t, worker, "project.update", map[string]any{"id": out.ID, "name": "x"}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker must not edit project info: %v", err)
	}
}

func TestOptimisticVersionOnUpdate(t *testing.T) {
	e := setup(t)
	id := e.create(t, "P")
	one := int64(5)
	b, _ := json.Marshal(map[string]any{"id": id, "name": "Q"})
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: "project.update", Input: b, ExpectVersion: &one}); exit(err) != domain.ExitConflict {
		t.Fatalf("stale version: %v", err)
	}
}

func TestReposAndCheckouts(t *testing.T) {
	e := setup(t)
	id := e.create(t, "P")
	other := e.create(t, "Q")
	const remote = "https://git.example/x/app.git"

	// 远程仓库属"Agent 与执行者配置"：worker 在钥匙范围外（4），没开这一组的 delegate 是 23。
	for _, c := range []struct {
		id2  command.Identity
		want int
	}{{worker, domain.ExitPermissionDenied}, {delegate, domain.ExitTierClosed}} {
		if _, err := e.do(t, c.id2, "project.repo.add", map[string]any{"id": id, "remote": remote}); exit(err) != c.want {
			t.Fatalf("agent must not add remote: %v", err)
		}
	}
	e.must(t, owner, "project.repo.add", map[string]any{"id": id, "remote": remote})
	if _, err := e.do(t, owner, "project.repo.add", map[string]any{"id": id, "remote": remote}); exit(err) != domain.ExitConflict {
		t.Fatalf("duplicate remote in same project: %v", err)
	}
	if _, err := e.do(t, owner, "project.repo.add", map[string]any{"id": other, "remote": remote}); exit(err) != domain.ExitConflict {
		t.Fatalf("remote must belong to one project: %v", err)
	}
	if _, err := e.do(t, owner, "project.repo.add", map[string]any{"id": id, "remote": "has space"}); exit(err) != domain.ExitUsage {
		t.Fatalf("bad remote: %v", err)
	}

	// worker 可以增改本机检出条目。
	co := map[string]any{"id": id, "remote": remote, "host": "box", "path": "/w/app"}
	e.must(t, worker, "project.checkout.set", co)
	e.must(t, worker, "project.checkout.set", co) // 重复写入幂等
	p := e.get(t, id)
	if len(p.Repos) != 1 || len(p.Repos[0].Checkouts) != 1 || p.Repos[0].Checkouts[0] != (domain.Checkout{Host: "box", Path: "/w/app"}) {
		t.Fatalf("checkouts: %+v", p.Repos)
	}
	for name, in := range map[string]map[string]any{
		"relative path": {"id": id, "remote": remote, "host": "box", "path": "w/app"},
		"unclean path":  {"id": id, "remote": remote, "host": "box", "path": "/w/../app"},
		"empty host":    {"id": id, "remote": remote, "host": "", "path": "/w/app"},
	} {
		if _, err := e.do(t, worker, "project.checkout.set", in); exit(err) != domain.ExitUsage {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := e.do(t, worker, "project.checkout.set", map[string]any{"id": id, "remote": "https://other/x.git", "host": "box", "path": "/w/o"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("checkout of unknown remote: %v", err)
	}
	if _, err := e.do(t, worker, "project.checkout.remove", map[string]any{"id": id, "remote": remote, "host": "box", "path": "/nope"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("remove unknown checkout: %v", err)
	}
	e.must(t, worker, "project.checkout.remove", co)
	if p = e.get(t, id); len(p.Repos[0].Checkouts) != 0 {
		t.Fatalf("checkout not removed: %+v", p.Repos)
	}

	// 删除远程地址同时带走其检出，之后别的项目可以登记它。
	e.must(t, worker, "project.checkout.set", co)
	if _, err := e.do(t, worker, "project.repo.remove", map[string]any{"id": id, "remote": remote}); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker must not remove remote: %v", err)
	}
	e.must(t, owner, "project.repo.remove", map[string]any{"id": id, "remote": remote})
	if p = e.get(t, id); len(p.Repos) != 0 {
		t.Fatalf("repo not removed: %+v", p.Repos)
	}
	if _, err := e.do(t, owner, "project.repo.remove", map[string]any{"id": id, "remote": remote}); exit(err) != domain.ExitNotFound {
		t.Fatalf("remove twice: %v", err)
	}
	e.must(t, owner, "project.repo.add", map[string]any{"id": other, "remote": remote})

	// 来源事件按远程地址归入项目。
	var got string
	_ = e.st.View(bg, func(tx *store.Tx) error {
		pr, ok, err := ByRemote(tx, remote)
		if ok {
			got = pr.ID
		}
		return err
	})
	if got != other {
		t.Fatalf("ByRemote = %q, want %q", got, other)
	}
}

func TestAllowedExecutorsThreeStates(t *testing.T) {
	e := setup(t)
	id := e.create(t, "P")
	if p := e.get(t, id); p.AllowedExecutors != nil {
		t.Fatal("new project must be unrestricted (nil)")
	}
	set := func(v any) error {
		_, err := e.do(t, owner, "project.allowed_executors.set", map[string]any{"id": id, "executors": v})
		return err
	}

	if err := set([]string{}); err != nil {
		t.Fatal(err)
	}
	p := e.get(t, id)
	if p.AllowedExecutors == nil || len(p.AllowedExecutors) != 0 {
		t.Fatalf("empty list must persist as empty, not nil: %#v", p.AllowedExecutors)
	}
	if p.ExecutorAllowed(domain.ExecutorRef{ID: "agent-a"}, "me") || !p.ExecutorAllowed(domain.ExecutorRef{ID: "me"}, "me") {
		t.Fatal("empty list means self only")
	}

	if err := set([]string{"agent-a", "agent-a", "agent-b"}); err != nil {
		t.Fatal(err)
	}
	p = e.get(t, id)
	if len(p.AllowedExecutors) != 2 || !p.ExecutorAllowed(domain.ExecutorRef{ID: "agent-b"}, "me") || p.ExecutorAllowed(domain.ExecutorRef{ID: "agent-c"}, "me") {
		t.Fatalf("list: %#v", p.AllowedExecutors)
	}

	if err := set(nil); err != nil {
		t.Fatal(err)
	}
	if p = e.get(t, id); p.AllowedExecutors != nil || !p.ExecutorAllowed(domain.ExecutorRef{ID: "anyone"}, "me") {
		t.Fatalf("null must restore unrestricted: %#v", p.AllowedExecutors)
	}

	if err := set([]string{""}); exit(err) != domain.ExitUsage {
		t.Fatalf("blank executor id: %v", err)
	}
	if _, err := e.do(t, owner, "project.allowed_executors.set", map[string]any{"id": id}); exit(err) != domain.ExitUsage {
		t.Fatalf("missing executors key must not silently clear: %v", err)
	}

	var changed int
	for _, ev := range e.events(t) {
		if ev.Type == domain.EvProjectAllowedExecutorsChanged {
			changed++
			if ev.Object != id || ev.Actor.Principal != "owner" {
				t.Fatalf("event: %+v", ev)
			}
		}
	}
	if changed != 3 {
		t.Fatalf("want 3 allowed_executors events, got %d", changed)
	}
}

func TestAllowedExecutorsAreWebOnly(t *testing.T) {
	e := setup(t)
	id := e.create(t, "P")
	in := map[string]any{"id": id, "executors": []string{"evil"}}

	_, err := e.do(t, delegate, "project.allowed_executors.set", in)
	if exit(err) != domain.ExitTierClosed {
		t.Fatalf("delegate/CLI must get exit 23, got %v", err)
	}
	if _, err := e.do(t, worker, "project.allowed_executors.set", in); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("worker: %v", err)
	}
	if p := e.get(t, id); p.AllowedExecutors != nil {
		t.Fatalf("rejected attempts must not change the list: %#v", p.AllowedExecutors)
	}
	var rejected int
	for _, ev := range e.events(t) {
		if ev.Type == domain.EvCommandRejected && ev.Object == id {
			rejected++
		}
	}
	if rejected != 2 {
		t.Fatalf("each exit-20 attempt must leave one command.rejected event, got %d", rejected)
	}
	// 其它项目编辑命令也写不了名单。
	if _, err := e.do(t, owner, "project.create", map[string]any{"name": "x", "allowed_executors": []string{"a"}}); exit(err) != domain.ExitUsage {
		t.Fatalf("create must not take allowed_executors: %v", err)
	}
}

func TestExecutorViolationUsesPersistedList(t *testing.T) {
	e := setup(t)
	id := e.create(t, "P")
	e.must(t, owner, "project.allowed_executors.set", map[string]any{"id": id, "executors": []string{"a"}})
	p := e.get(t, id)
	d := authz.Decide(policy.Policy{}, authz.Request{Actor: owner.Actor, Level: authz.LevelWeb, Action: authz.ActionDelegate, Group: authz.GroupOrganize,
		Owner: "me", Object: authz.Object{ProjectInfo: &p, Executor: "stranger"}})
	if d.Err == nil || d.ExitCode() != domain.ExitExecutorNotAllowed || len(d.Err.Allowed) != 1 || d.Err.Allowed[0] != "a" {
		t.Fatalf("decision %+v", d)
	}
}

func TestResolveProject(t *testing.T) {
	e := setup(t)
	a, b, c := e.create(t, "A"), e.create(t, "B"), e.create(t, "C")
	for id, co := range map[string][]domain.Checkout{
		a: {{Host: "box", Path: "/w/mono"}, {Host: "other", Path: "/w/mono/svc"}},
		b: {{Host: "box", Path: "/w/mono/svc"}},
		c: {{Host: "box", Path: "/w/dup"}},
	} {
		remote := "https://git.example/" + id + ".git"
		e.must(t, owner, "project.repo.add", map[string]any{"id": id, "remote": remote})
		for _, k := range co {
			e.must(t, worker, "project.checkout.set", map[string]any{"id": id, "remote": remote, "host": k.Host, "path": k.Path})
		}
	}
	d := e.create(t, "D")
	e.must(t, owner, "project.repo.add", map[string]any{"id": d, "remote": "https://git.example/d.git"})
	e.must(t, worker, "project.checkout.set", map[string]any{"id": d, "remote": "https://git.example/d.git", "host": "box", "path": "/w/dup"})

	res := func(s Scope) (string, error) {
		s.Host = "box"
		var got string
		err := e.st.View(bg, func(tx *store.Tx) (err error) { got, err = Resolve(tx, s); return })
		return got, err
	}
	cases := []struct {
		name string
		s    Scope
		want string
		exit int
	}{
		{"flag wins over env and cwd", Scope{Flag: a, Env: b, Cwd: "/w/mono/svc"}, a, 0},
		{"env wins over cwd", Scope{Env: b, Cwd: "/w/mono"}, b, 0},
		{"cwd inside checkout", Scope{Cwd: "/w/mono/docs/x"}, a, 0},
		{"longest prefix wins, other host's checkout ignored", Scope{Cwd: "/w/mono/svc/pkg"}, b, 0},
		{"exact path", Scope{Cwd: "/w/mono"}, a, 0},
		{"sibling with shared name prefix is not inside", Scope{Cwd: "/w/mono2"}, "", 0},
		{"no match means no scope", Scope{Cwd: "/elsewhere"}, "", 0},
		{"no cwd, no scope", Scope{}, "", 0},
		{"explicit unknown project fails instead of falling back", Scope{Flag: "nope", Cwd: "/w/mono"}, "", domain.ExitNotFound},
		{"env unknown project fails", Scope{Env: "nope"}, "", domain.ExitNotFound},
		{"same path in two projects is ambiguous, no guess", Scope{Cwd: "/w/dup/x"}, "", domain.ExitConflict},
		{"relative cwd rejected", Scope{Cwd: "w/mono"}, "", domain.ExitUsage},
	}
	for _, tc := range cases {
		got, err := res(tc.s)
		if tc.exit != 0 {
			if exit(err) != tc.exit {
				t.Errorf("%s: want exit %d, got %v (%q)", tc.name, tc.exit, err, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s: got %q, %v; want %q", tc.name, got, err, tc.want)
		}
	}
	// 同路径在别的机器上登记，不影响本机；本机名不同则不命中。
	var got string
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { got, err = Resolve(tx, Scope{Cwd: "/w/mono", Host: "other"}); return })
	if got != "" {
		t.Fatalf("host without matching checkout must get no scope, got %q", got)
	}
}

func TestCorruptRowSurfacesError(t *testing.T) {
	e := setup(t)
	id := e.create(t, "P")
	_ = e.st.Update(bg, func(tx *store.Tx) error {
		_, err := tx.Exec("UPDATE projects SET repos = 'not json' WHERE id = ?", id)
		return err
	})
	err := e.st.View(bg, func(tx *store.Tx) error { _, err := Get(tx, id); return err })
	if err == nil {
		t.Fatal("corrupt data must be an error, not an empty project")
	}
	if err := e.st.View(bg, func(tx *store.Tx) error { _, err := List(tx); return err }); err == nil {
		t.Fatal("List must surface corrupt rows")
	}
}

func TestListOrdersByName(t *testing.T) {
	e := setup(t)
	e.create(t, "b")
	e.create(t, "a")
	var ps []domain.Project
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { ps, err = List(tx); return })
	if len(ps) != 2 || ps[0].Name != "a" || ps[1].Name != "b" {
		t.Fatalf("list: %+v", ps)
	}
}

func TestRepoKeyAndSameRepo(t *testing.T) {
	const key = "example.com/acme/app"
	for _, s := range []string{
		"https://example.com/acme/app",
		"https://example.com/acme/app.git",
		"https://example.com/acme/app/",
		"HTTPS://Example.com/Acme/App.git",
		"https://user:secret@example.com/acme/app.git",
		"https://example.com:8443/acme/app.git",
		"http://example.com/acme/app",
		"ssh://git@example.com/acme/app.git",
		"ssh://git@example.com:2222/acme/app.git",
		"git@example.com:acme/app.git",
		"git@example.com:Acme/App",
		"example.com:acme/app",
		"example.com/acme/app",
	} {
		if got := RepoKey(s); got != key {
			t.Errorf("RepoKey(%q) = %q, want %q", s, got, key)
		}
	}
	// 简写没有 host，规范写法不虚构 host，但与任一站点的同路径仓库是同一身份。
	if got := RepoKey(" Acme/App.git/ "); got != "acme/app" {
		t.Errorf("short form key = %q", got)
	}
	// 本地路径与 file:// 没有站点，大小写有意义，原样保留。
	for _, s := range []string{"/tmp/Acme/app.git", "./acme/app", "../a/b", "~/a/b", "file:///tmp/Acme/app.git", "app"} {
		if got := RepoKey(s); got != s {
			t.Errorf("RepoKey(%q) = %q, want it unchanged", s, got)
		}
	}

	for _, c := range []struct {
		a, b string
		want bool
	}{
		{"https://example.com/acme/app.git", "git@example.com:ACME/app", true},
		{"acme/app", "https://example.com/acme/app.git", true},
		{"git@example.com:acme/app.git", "acme/app", true},
		{"acme/app", "ACME/APP.git", true},
		{"https://example.com/group/sub/app.git", "git@example.com:group/sub/app", true},
		{"/tmp/a/b.git", "/tmp/a/b.git", true},

		{"https://example.com/acme/app", "https://example.com/other/app", false}, // 不同 owner
		{"acme/app", "other/app", false},                                         // 不同 owner，简写
		{"acme/app", "acme/app2", false},                                         // 不同仓库
		{"https://example.com/acme/app", "https://example.org/acme/app", false},  // 不同站点
		{"acme/app", "https://example.com/group/acme/app", false},                // 路径段数不同
		{"/tmp/a/b", "/tmp/A/b", false},                                          // 本地路径区分大小写
		{"/tmp/a/b", "a/b", false},                                               // 本地路径不是简写
		{"", "", false},
		{"", "acme/app", false},
	} {
		if got := SameRepo(c.a, c.b); got != c.want {
			t.Errorf("SameRepo(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestRepoHasLinkMatchesWholePathSegments(t *testing.T) {
	const link = "https://h/org/web/issues/1"
	for _, c := range []struct {
		remote, link string
		want         bool
	}{
		{"https://h/org/web.git", "https://h/org/web", true},
		{"https://h/org/web.git", link, true},
		{"git@h:org/web.git", link, true},
		{"org/web", link, true},
		{"https://h/org/web.git", "https://h/org/web2/x", false},
		{"https://h/org/web.git", "https://other/org/web/x", false},
		{"https://h/org/web.git", "", false},
		{"org/web", "org/web#1", false},
	} {
		if got := RepoHasLink(c.remote, c.link); got != c.want {
			t.Errorf("RepoHasLink(%q, %q) = %v, want %v", c.remote, c.link, got, c.want)
		}
	}
}

func TestFindDoesNotGuessBetweenHosts(t *testing.T) {
	ps := []domain.Project{
		{ID: "a", Repos: []domain.Repo{{Remote: "https://a.example/acme/app.git"}}},
		{ID: "b", Repos: []domain.Repo{{Remote: "git@b.example:acme/app.git"}}},
		{ID: "c", Repos: []domain.Repo{{Remote: "https://a.example/acme/solo.git"}}},
	}
	for remote, want := range map[string]string{
		"acme/solo":                  "c",
		"ACME/solo.git":              "c",
		"git@b.example:acme/app":     "b",
		"https://a.example/acme/app": "a",
		"acme/app":                   "", // 两个站点都有，不猜
		"other/solo":                 "",
		"":                           "",
	} {
		p, _, ok := Find(ps, remote)
		if ok != (want != "") || p.ID != want {
			t.Errorf("Find(%q) = %q %v, want %q", remote, p.ID, ok, want)
		}
	}
}

// 同一仓库的不同写法：不能重复登记，检出与移除也都认得；账本里保留登记时的写法。
func TestRepoCommandsAcceptAnySpelling(t *testing.T) {
	e := setup(t)
	id, other := e.create(t, "P"), e.create(t, "Q")
	const remote = "https://example.com/acme/app.git"
	e.must(t, owner, "project.repo.add", map[string]any{"id": id, "remote": remote})

	for _, spelling := range []string{"git@example.com:ACME/app.git", "acme/app", "https://example.com/acme/app"} {
		if _, err := e.do(t, owner, "project.repo.add", map[string]any{"id": other, "remote": spelling}); exit(err) != domain.ExitConflict {
			t.Fatalf("%s must conflict with %s: %v", spelling, remote, err)
		}
	}
	e.must(t, owner, "project.repo.add", map[string]any{"id": other, "remote": "https://example.com/other/app.git"})

	e.must(t, worker, "project.checkout.set", map[string]any{"id": id, "remote": "git@example.com:acme/app.git", "host": "box", "path": "/w/app"})
	if p := e.get(t, id); len(p.Repos) != 1 || p.Repos[0].Remote != remote || len(p.Repos[0].Checkouts) != 1 {
		t.Fatalf("%+v", p.Repos)
	}
	e.must(t, owner, "project.repo.remove", map[string]any{"id": id, "remote": "acme/app"})
	if p := e.get(t, id); len(p.Repos) != 0 {
		t.Fatalf("%+v", p.Repos)
	}
}

// 按维度匹配已取消（§14.7）：名单只认执行者 id，带 match 的设置被拒，名单不变。
func TestAllowedExecutorMatch(t *testing.T) {
	e := setup(t)
	id := e.create(t, "P")
	in := map[string]any{"id": id, "executors": nil, "match": []map[string]string{{"host": "mbp"}}}
	if _, err := e.do(t, owner, "project.allowed_executors.set", in); exit(err) != domain.ExitUsage {
		t.Fatalf("match must be rejected: %v", err)
	}
	if p := e.get(t, id); p.Restricted() {
		t.Fatalf("rejected match changed the project: %+v", p)
	}
}

// 对话捕获默认开启的一次性迁移：只补开还没开的项目，操作者是 kernel:defaults，重复执行不再写；已开的项目不动、不写事件。
func TestSeedCaptureEnablesOnceAndIsIdempotent(t *testing.T) {
	e := setup(t)
	mk := func(name string) string {
		var out struct{ ID string }
		_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": name}).Value, &out)
		return out.ID
	}
	off, on := mk("old"), mk("new")
	e.must(t, owner, "project.update", map[string]any{"id": off, "capture": false}) // 升级前的项目：capture 是 false
	before := len(e.events(t))
	seed := func() {
		t.Helper()
		if err := SeedCapture(bg, e.x, e.st, "owner"); err != nil {
			t.Fatal(err)
		}
	}
	seed()
	if !e.get(t, off).Capture || !e.get(t, on).Capture {
		t.Fatal("every project must have capture on after the migration")
	}
	ev := e.events(t)
	if len(ev) != before+1 || ev[before].Object != off || ev[before].Actor.Principal != "kernel:defaults" || ev[before].Type != domain.EvProjectUpdated {
		t.Fatalf("exactly one system event for the one project that was off: %+v", ev[before:])
	}
	seed()
	seed()
	if len(e.events(t)) != before+1 {
		t.Fatal("repeated runs must not write again")
	}
	// 之后被直接调 API 关掉的，迁移不再把它打开（一次性）。
	e.must(t, owner, "project.update", map[string]any{"id": off, "capture": false})
	seed()
	if e.get(t, off).Capture {
		t.Fatal("the migration is one-time")
	}
}

// 内置 Inbox：新库创建 prj_inbox，幂等且事件折叠一致；列表永远置顶。
func TestEnsureInboxCreatesFreshAndVerifiesFold(t *testing.T) {
	e := setup(t)
	var other struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": "AAA"}).Value, &other)

	id, err := EnsureInbox(bg, e.x, e.st, "owner")
	if err != nil || id != InboxDefaultID {
		t.Fatalf("EnsureInbox: id=%q err=%v", id, err)
	}
	p := e.get(t, id)
	if !p.IsInbox() || p.Name != InboxDefaultName || p.Status != domain.ProjectActive || !p.Capture {
		t.Fatalf("unexpected inbox: %+v", p)
	}
	before := len(e.events(t))
	id2, err := EnsureInbox(bg, e.x, e.st, "owner")
	if err != nil || id2 != id || len(e.events(t)) != before {
		t.Fatalf("EnsureInbox not idempotent: id2=%q err=%v events %d->%d", id2, err, before, len(e.events(t)))
	}
	if err := e.st.View(bg, Verify); err != nil {
		t.Fatalf("projects.Verify failed: %v", err)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		ps, err := List(tx)
		if err != nil {
			return err
		}
		if len(ps) < 2 || !ps[0].IsInbox() {
			t.Fatalf("Inbox must sort first in List: %+v", ps)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// 内置 Inbox：老库已有无仓库活跃「收件箱」时原地认领，保留原 ID、统一显示名为 Inbox，折叠一致。
func TestEnsureInboxClaimsExistingAndVerifiesFold(t *testing.T) {
	e := setup(t)
	var old struct{ ID string }
	_ = json.Unmarshal(e.must(t, owner, "project.create", map[string]any{"name": "收件箱"}).Value, &old)

	id, err := EnsureInbox(bg, e.x, e.st, "owner")
	if err != nil || id != old.ID {
		t.Fatalf("want claimed id %s, got %q err %v", old.ID, id, err)
	}
	p := e.get(t, old.ID)
	if !p.IsInbox() || p.Name != InboxDefaultName || !p.Capture {
		t.Fatalf("claimed inbox: %+v", p)
	}
	if err := e.st.View(bg, Verify); err != nil {
		t.Fatalf("projects.Verify failed after claim: %v", err)
	}
}

// 内置 Inbox 保护边界与 ResolveOrInbox：禁止暂停/归档、绑仓库/检出、外部写 system_role；显式不存在项目仍报 404。
func TestInboxProtectionsAndResolveOrInbox(t *testing.T) {
	e := setup(t)
	inboxID, err := EnsureInbox(bg, e.x, e.st, "owner")
	if err != nil {
		t.Fatal(err)
	}
	for _, st := range []string{"paused", "archived"} {
		if _, err := e.do(t, owner, "project.update", map[string]any{"id": inboxID, "status": st}); exit(err) != domain.ExitUsage {
			t.Fatalf("inbox status=%s must be rejected with exit 2, got %v", st, err)
		}
	}
	if _, err := e.do(t, owner, "project.repo.add", map[string]any{"id": inboxID, "remote": "https://git.example/a/b"}); exit(err) != domain.ExitUsage {
		t.Fatalf("inbox repo.add must be rejected with exit 2, got %v", err)
	}
	if _, err := e.do(t, owner, "project.checkout.set", map[string]any{"id": inboxID, "remote": "https://git.example/a/b", "host": "h", "path": "/tmp/a"}); exit(err) != domain.ExitUsage {
		t.Fatalf("inbox checkout.set must be rejected with exit 2, got %v", err)
	}
	if _, err := e.do(t, owner, "project.update", map[string]any{"id": inboxID, "system_role": ""}); exit(err) != domain.ExitUsage {
		t.Fatalf("writing system_role externally must be rejected with exit 2, got %v", err)
	}
	if err := e.st.View(bg, func(tx *store.Tx) error {
		// Resolve 保持纯粹，不匹配返回空串
		if got, err := Resolve(tx, Scope{Cwd: "/tmp/no-repo", Host: "h"}); err != nil || got != "" {
			t.Fatalf("Resolve should stay pure: got %q err %v", got, err)
		}
		// ResolveOrInbox 无匹配兜底 Inbox
		got, fb, err := ResolveOrInbox(tx, Scope{Cwd: "/tmp/no-repo", Host: "h"})
		if err != nil || !fb || got != inboxID {
			t.Fatalf("ResolveOrInbox fallback: got %q fb %v err %v", got, fb, err)
		}
		// 显式指定不存在的项目：仍报 not_found，绝不兜底
		if _, _, err := ResolveOrInbox(tx, Scope{Flag: "prj_nope"}); err == nil {
			t.Fatal("explicit unknown project must return error")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
