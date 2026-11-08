package registry

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

var bg = context.Background()

var owner = command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb}

type env struct {
	st  *store.Store
	x   *command.Executor
	au  *auth.Auth
	dir string
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
	e := &env{st: st, au: auth.New(nil), x: command.New(st, nil), dir: t.TempDir()}
	lay := config.LayoutOf(e.dir)
	if err := Register(e.x, Options{Bundled: []string{"pull-feed", "pull-http"}, Layout: lay}); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) bearer(t *testing.T, k auth.Kind) command.Bearer {
	t.Helper()
	var secret string
	if err := e.st.Update(bg, func(tx *store.Tx) (err error) {
		_, secret, err = e.au.Issue(tx, auth.Spec{Kind: k, Label: string(k)})
		return
	}); err != nil {
		t.Fatal(err)
	}
	return command.Bearer{Auth: e.au, Secret: secret, Origin: auth.Origin{Session: "s1"}, OnBehalfOf: "owner"}
}

func js(v any) json.RawMessage { b, _ := json.Marshal(v); return b }

type m = map[string]any

func (e *env) do(id command.Identity, cmd string, in any) (command.Result, error) {
	return e.x.Do(bg, command.Request{Identity: id, Command: cmd, Input: js(in), Evidence: "owner said so"})
}

func (e *env) get(t *testing.T, id string) domain.Source {
	t.Helper()
	var s domain.Source
	if err := e.st.View(bg, func(tx *store.Tx) (err error) { s, err = Get(tx, id); return }); err != nil {
		t.Fatal(err)
	}
	return s
}

func (e *env) types(t *testing.T) []string {
	t.Helper()
	var out []string
	if err := e.st.View(bg, func(tx *store.Tx) error {
		es, err := eventlog.Read(tx, 0, 1000)
		for _, x := range es {
			out = append(out, x.Event.Type)
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return out
}

func exit(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		return de.ExitCode()
	}
	return -1
}

func pull(id string) m {
	return m{"id": id, "type": "pull", "source_type": "feed", "command": "pull-feed", "schedule": "15m", "timeout": 60}
}

func script(id string) m {
	return m{"id": id, "type": "script", "source_type": "inspect", "command": "/opt/check.sh --all", "schedule": "1h", "timeout": 300,
		"dedupe_domain": "ops", "config": m{"k": "v"}}
}

func TestOwnerRegistersAllThreeTypesAndTheyPersist(t *testing.T) {
	e := setup(t)
	cred := filepath.Join(e.dir, "sources", "src_mail", "token")
	in := script("src_mail")
	in["credentials_path"] = cred
	for _, c := range []struct {
		cmd string
		in  m
	}{
		{CmdRegister, pull("src_feed")},
		{CmdRegisterExec, in},
		{CmdRegister, m{"id": "src_hook", "type": "webhook", "source_type": "alerts"}},
	} {
		if _, err := e.do(owner, c.cmd, c.in); err != nil {
			t.Fatalf("%s: %v", c.cmd, err)
		}
	}
	s := e.get(t, "src_mail")
	if s.Type != domain.SourceScript || s.Command != "/opt/check.sh --all" || s.Schedule != "1h" || s.Timeout != 300 ||
		s.SourceKind != "inspect" || s.DedupeDomain != "ops" || s.CredentialsPath != cred || !s.Enabled || string(s.Config) != `{"k":"v"}` {
		t.Fatalf("script not persisted: %+v", s)
	}
	if got := e.get(t, "src_feed"); got.Type != domain.SourcePull || got.Command != "pull-feed" || got.Timeout != 60 {
		t.Fatalf("pull: %+v", got)
	}
	// 去重域：登记了用域，未登记用 source id（§6.10）。
	if k := s.DedupeKeyFor("e1"); k.Domain != "ops" {
		t.Fatalf("dedupe %+v", k)
	}
	if k := e.get(t, "src_hook").DedupeKeyFor("e1"); k.Domain != "src_hook" {
		t.Fatalf("dedupe %+v", k)
	}
	var all []domain.Source
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { all, err = List(tx); return })
	if len(all) != 3 || all[0].ID != "src_feed" || all[1].ID != "src_hook" || all[2].ID != "src_mail" {
		t.Fatalf("list %+v", all)
	}
	if got := e.types(t); len(got) != 3 || got[0] != EvRegistered {
		t.Fatalf("events %v", got)
	}
}

func TestUpdateChangesFieldsButNotTypeCursorOrStats(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegister, pull("src_feed")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdUpdate, m{"id": "src_feed", "schedule": "5m", "timeout": 90, "dedupe_domain": "shared", "command": "pull-http"}); err != nil {
		t.Fatal(err)
	}
	s := e.get(t, "src_feed")
	if s.Schedule != "5m" || s.Timeout != 90 || s.DedupeDomain != "shared" || s.Command != "pull-http" || s.SourceKind != "feed" {
		t.Fatalf("update: %+v", s)
	}
	for _, f := range []m{{"type": "script"}, {"cursor": "x"}, {"stats": m{}}, {"enabled": false}} {
		f["id"] = "src_feed"
		if _, err := e.do(owner, CmdUpdate, f); exit(err) != domain.ExitUsage {
			t.Fatalf("%v should be rejected: %v", f, err)
		}
	}
	if _, err := e.do(owner, CmdUpdate, m{"id": "src_none", "schedule": "5m"}); exit(err) != domain.ExitNotFound {
		t.Fatalf("missing: %v", err)
	}
	if e.get(t, "src_feed").Schedule != "5m" {
		t.Fatal("rejected updates must not change anything")
	}
}

func TestEnableDisableIsIdempotentAndRecorded(t *testing.T) {
	e := setup(t)
	in := pull("src_feed")
	in["enabled"] = false
	if _, err := e.do(owner, CmdRegister, in); err != nil {
		t.Fatal(err)
	}
	if e.get(t, "src_feed").Enabled {
		t.Fatal("registered disabled")
	}
	for i := 0; i < 2; i++ {
		if _, err := e.do(owner, CmdSetEnabled, m{"id": "src_feed", "enabled": true}); err != nil {
			t.Fatal(err)
		}
	}
	if !e.get(t, "src_feed").Enabled {
		t.Fatal("not enabled")
	}
	if _, err := e.do(owner, CmdSetEnabled, m{"id": "src_feed", "enabled": false}); err != nil {
		t.Fatal(err)
	}
	if got := e.types(t); len(got) != 3 || got[1] != EvEnabled || got[2] != EvDisabled {
		t.Fatalf("events %v", got)
	}
	if _, err := e.do(owner, CmdSetEnabled, m{"id": "src_none", "enabled": true}); exit(err) != domain.ExitNotFound {
		t.Fatalf("missing: %v", err)
	}
}

func TestDelegateMayOnlyRegisterBundledPrograms(t *testing.T) {
	e := setup(t)
	dlg := e.bearer(t, auth.Delegate)
	if _, err := e.do(dlg, CmdRegister, pull("src_feed")); err != nil {
		t.Fatalf("bundled pull: %v", err)
	}
	if _, err := e.do(dlg, CmdRegister, m{"id": "src_hook", "type": "webhook"}); err != nil {
		t.Fatalf("webhook: %v", err)
	}
	if _, err := e.do(dlg, CmdUpdate, m{"id": "src_feed", "schedule": "30m"}); err != nil {
		t.Fatalf("update bundled: %v", err)
	}
	if _, err := e.do(dlg, CmdSetEnabled, m{"id": "src_feed", "enabled": false}); err != nil {
		t.Fatalf("disable: %v", err)
	}

	bad := pull("src_evil")
	bad["command"] = "/bin/sh -c evil"
	if _, err := e.do(dlg, CmdRegister, bad); exit(err) != domain.ExitUsage {
		t.Fatalf("non-bundled pull: %v", err)
	}
	if _, err := e.do(dlg, CmdUpdate, m{"id": "src_feed", "command": "/bin/sh"}); exit(err) != domain.ExitUsage {
		t.Fatalf("retarget to arbitrary command: %v", err)
	}
	// 自定义命令：专用命令对 delegate 是 web_only（退出码 20，留痕）。
	if _, err := e.do(dlg, CmdRegisterExec, script("src_sh")); exit(err) != domain.ExitWebOnly {
		t.Fatalf("delegate register_script: %v", err)
	}
	// 不能借通用命令绕过。
	if _, err := e.do(dlg, CmdRegister, script("src_sh")); exit(err) != domain.ExitUsage {
		t.Fatalf("delegate register as script: %v", err)
	}
	rejected := 0
	for _, ty := range e.types(t) {
		if ty == domain.EvCommandRejected {
			rejected++
		}
	}
	if rejected != 1 {
		t.Fatalf("web_only rejection must be recorded once, got %d", rejected)
	}
}

func TestScriptSourcesAreWebOnlyAfterRegistration(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegisterExec, script("src_sh")); err != nil {
		t.Fatal(err)
	}
	dlg := e.bearer(t, auth.Delegate)
	if _, err := e.do(dlg, CmdUpdateExec, m{"id": "src_sh", "command": "/bin/rm -rf /"}); exit(err) != domain.ExitWebOnly {
		t.Fatalf("delegate update_script: %v", err)
	}
	if _, err := e.do(dlg, CmdUpdate, m{"id": "src_sh", "schedule": "2h"}); exit(err) != domain.ExitUsage {
		t.Fatalf("delegate update script via generic: %v", err)
	}
	if got := e.get(t, "src_sh"); got.Command != "/opt/check.sh --all" || got.Schedule != "1h" {
		t.Fatalf("script changed by delegate: %+v", got)
	}
	if _, err := e.do(owner, CmdUpdateExec, m{"id": "src_sh", "command": "/opt/new.sh", "timeout": 10}); err != nil {
		t.Fatal(err)
	}
	if got := e.get(t, "src_sh"); got.Command != "/opt/new.sh" || got.Timeout != 10 {
		t.Fatalf("owner update: %+v", got)
	}
	// owner 也不能用 script 命令改非 script 来源。
	if _, err := e.do(owner, CmdRegister, pull("src_feed")); err != nil {
		t.Fatal(err)
	}
	if _, err := e.do(owner, CmdUpdateExec, m{"id": "src_feed", "command": "/bin/sh"}); exit(err) != domain.ExitUsage {
		t.Fatalf("update_script on pull: %v", err)
	}
}

func TestWorkerObserverAndScopedCredentialsCannotRegister(t *testing.T) {
	e := setup(t)
	for _, k := range []auth.Kind{auth.Worker, auth.Observer} {
		b := e.bearer(t, k)
		if _, err := e.do(b, CmdRegister, pull("src_feed")); exit(err) != domain.ExitPermissionDenied {
			t.Fatalf("%s: %v", k, err)
		}
		if _, err := e.do(b, CmdRegisterExec, script("src_sh")); exit(err) != domain.ExitPermissionDenied {
			t.Fatalf("%s script: %v", k, err)
		}
	}
	var secret string
	_ = e.st.Update(bg, func(tx *store.Tx) (err error) {
		_, secret, err = e.au.Issue(tx, auth.Spec{Kind: auth.Webhook, Scope: "src_hook", Label: "w"})
		return
	})
	if _, err := e.do(command.Bearer{Auth: e.au, Secret: secret}, CmdRegister, pull("src_feed")); exit(err) != domain.ExitPermissionDenied {
		t.Fatalf("webhook credential: %v", err)
	}
}

func TestValidation(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegister, pull("src_feed")); err != nil {
		t.Fatal(err)
	}
	with := func(base m, k string, v any) m {
		c := m{}
		for a, b := range base {
			c[a] = b
		}
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return c
	}
	cases := []struct {
		name string
		cmd  string
		in   any
		exit int
	}{
		{"bad id", CmdRegister, with(pull("x"), "id", "Feed 1"), domain.ExitUsage},
		{"no id prefix", CmdRegister, with(pull("x"), "id", "feed"), domain.ExitUsage},
		{"duplicate", CmdRegister, pull("src_feed"), domain.ExitConflict},
		{"no type", CmdRegister, with(pull("src_a"), "type", nil), domain.ExitUsage},
		{"unknown type", CmdRegister, with(pull("src_a"), "type", "push"), domain.ExitUsage},
		{"bad schedule", CmdRegister, with(pull("src_a"), "schedule", "often"), domain.ExitUsage},
		{"tiny schedule", CmdRegister, with(pull("src_a"), "schedule", "10ms"), domain.ExitUsage},
		{"no schedule", CmdRegister, with(pull("src_a"), "schedule", nil), domain.ExitUsage},
		{"zero timeout", CmdRegister, with(pull("src_a"), "timeout", nil), domain.ExitUsage},
		{"huge timeout", CmdRegister, with(pull("src_a"), "timeout", 999999), domain.ExitUsage},
		{"bad source_type", CmdRegister, with(pull("src_a"), "source_type", "Has Space"), domain.ExitUsage},
		{"bad dedupe", CmdRegister, with(pull("src_a"), "dedupe_domain", "a b"), domain.ExitUsage},
		{"config not object", CmdRegister, with(pull("src_a"), "config", []int{1}), domain.ExitUsage},
		{"webhook with command", CmdRegister, m{"id": "src_a", "type": "webhook", "command": "pull-feed"}, domain.ExitUsage},
		{"webhook with schedule", CmdRegister, m{"id": "src_a", "type": "webhook", "schedule": "1m"}, domain.ExitUsage},
		{"script no command", CmdRegisterExec, with(script("src_a"), "command", nil), domain.ExitUsage},
		{"script wrong type", CmdRegisterExec, with(script("src_a"), "type", "pull"), domain.ExitUsage},
		{"cursor on register", CmdRegister, with(pull("src_a"), "cursor", "c"), domain.ExitUsage},
		{"creds outside dir", CmdRegisterExec, with(script("src_a"), "credentials_path", "/etc/passwd"), domain.ExitUsage},
		{"creds escape", CmdRegisterExec, with(script("src_a"), "credentials_path", filepath.Join(e.dir, "sources", "src_a", "..", "src_b", "t")), domain.ExitUsage},
	}
	for _, c := range cases {
		if _, err := e.do(owner, c.cmd, c.in); exit(err) != c.exit {
			t.Errorf("%s: want exit %d, got %v", c.name, c.exit, err)
		}
	}
	if _, err := e.x.Do(bg, command.Request{Identity: owner, Command: CmdRegister, Input: json.RawMessage(`{`)}); exit(err) != domain.ExitUsage {
		t.Errorf("malformed json: %v", err)
	}
	var all []domain.Source
	_ = e.st.View(bg, func(tx *store.Tx) (err error) { all, err = List(tx); return })
	if len(all) != 1 {
		t.Fatalf("invalid requests must not persist: %+v", all)
	}
}

func TestRegisterRejectsBadBundledNames(t *testing.T) {
	e := setup(t)
	if err := Register(command.New(e.st, nil), Options{Bundled: []string{"../x"}}); err == nil {
		t.Fatal("bundled names must be plain program names")
	}
}

func TestHostConfigFileRegistersAndOverridesScripts(t *testing.T) {
	e := setup(t)
	path := filepath.Join(t.TempDir(), "sources.json")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"sources":[
		{"id":"src_sh","type":"script","command":"/opt/a.sh","schedule":"1h","timeout":30,"source_type":"inspect"},
		{"id":"src_feed","type":"pull","command":"pull-feed","schedule":"10m","timeout":20}]}`)
	if err := LoadFile(bg, e.x, owner, path); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_sh"); s.Command != "/opt/a.sh" || s.Type != domain.SourceScript {
		t.Fatalf("%+v", s)
	}
	// 再次加载：覆盖已有条目，并按 enabled 启停。
	write(`{"sources":[{"id":"src_sh","command":"/opt/b.sh","schedule":"2h","timeout":45,"enabled":false}]}`)
	if err := LoadFile(bg, e.x, owner, path); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_sh"); s.Command != "/opt/b.sh" || s.Schedule != "2h" || s.Timeout != 45 || s.Enabled {
		t.Fatalf("override: %+v", s)
	}
	// 配置文件不是后门：delegate 身份加载仍是 web_only；错误文件如实报错。
	if err := LoadFile(bg, e.x, e.bearer(t, auth.Delegate), path); exit(err) != domain.ExitWebOnly {
		t.Fatalf("delegate load: %v", err)
	}
	write(`{"sources":[{"id":"src_x","type":"script"}]}`)
	if err := LoadFile(bg, e.x, owner, path); exit(err) != domain.ExitUsage {
		t.Fatalf("invalid entry: %v", err)
	}
	write(`{"sources":[],"extra":1}`)
	if err := LoadFile(bg, e.x, owner, path); exit(err) != domain.ExitUsage {
		t.Fatalf("unknown top-level key: %v", err)
	}
	if err := LoadFile(bg, e.x, owner, filepath.Join(t.TempDir(), "missing.json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
}

func TestHostConfigFileReloadWithoutEnabledIsIdempotent(t *testing.T) {
	e := setup(t)
	path := filepath.Join(t.TempDir(), "sources.json")
	body := `{"sources":[
		{"id":"src_a","type":"script","command":"/opt/a.sh","schedule":"1h","timeout":30},
		{"id":"src_b","type":"script","command":"/opt/b.sh","schedule":"1h","timeout":30}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := LoadFile(bg, e.x, owner, path); err != nil {
			t.Fatalf("load %d: %v", i, err)
		}
	}
	// 第二次加载后，排在后面的条目也被更新。
	body = `{"sources":[
		{"id":"src_a","type":"script","command":"/opt/a.sh","schedule":"1h","timeout":30},
		{"id":"src_b","type":"script","command":"/opt/b2.sh","schedule":"1h","timeout":30},
		{"id":"src_c","type":"script","command":"/opt/c.sh","schedule":"1h","timeout":30}]}`
	_ = os.WriteFile(path, []byte(body), 0o600)
	if err := LoadFile(bg, e.x, owner, path); err != nil {
		t.Fatal(err)
	}
	if e.get(t, "src_b").Command != "/opt/b2.sh" || e.get(t, "src_c").Command != "/opt/c.sh" {
		t.Fatal("later entries must be applied")
	}
}

// 调度模块经 SetRunState 写游标与统计；来源不存在时返回 not_found，而不是静默成功。
func TestSetRunStateWritesCursorAndRejectsUnknownSource(t *testing.T) {
	e := setup(t)
	if _, err := e.do(owner, CmdRegisterExec, m{"id": "src_a", "command": "run.sh", "schedule": "1m", "timeout": 30}); err != nil {
		t.Fatal(err)
	}
	if err := e.st.Update(bg, func(tx *store.Tx) error { return SetRunState(tx, "src_a", "c1", json.RawMessage(`{"runs":1}`)) }); err != nil {
		t.Fatal(err)
	}
	if s := e.get(t, "src_a"); s.Cursor != "c1" || string(s.Stats) != `{"runs":1}` {
		t.Fatalf("%+v", s)
	}
	err := e.st.Update(bg, func(tx *store.Tx) error { return SetRunState(tx, "src_nope", "c", nil) })
	var de *domain.Error
	if !errors.As(err, &de) || de.Code != domain.CodeNotFound {
		t.Fatalf("want not_found, got %v", err)
	}
}
