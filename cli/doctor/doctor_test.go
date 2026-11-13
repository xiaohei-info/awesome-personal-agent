package doctor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/backup"
	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	kdoctor "github.com/xiaohei-info/awesome-personal-agent/kernel/doctor"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/eventlog"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/executors"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/outcall/model"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/registry"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/sources/runner"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
	"github.com/xiaohei-info/awesome-personal-agent/tests/doubles/modelstub"
)

var bg = context.Background()
var now = time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC)
var owner = domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}

func fullReg(t *testing.T) *store.Registry {
	t.Helper()
	r := store.NewRegistry()
	for _, m := range []store.Module{eventlog.Module, auth.Module, policy.Module, command.Module, executors.Module,
		delivery.Module, registry.Module, runner.Module, kdoctor.Module} {
		if err := r.Register(m); err != nil {
			t.Fatal(err)
		}
	}
	return r
}

// env 是一个数据目录加它的注册表；h 是 home。
type env struct {
	t    *testing.T
	home string
	reg  *store.Registry
	st   *store.Store
}

func newEnv(t *testing.T) *env {
	t.Helper()
	home := filepath.Join(t.TempDir(), "apa")
	reg := fullReg(t)
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t, home, reg, st}
	for _, d := range []string{"sources", "webhook-keys", "run-credentials"} {
		must(t, os.MkdirAll(filepath.Join(home, d), 0o700))
	}
	must(t, os.Chmod(home, 0o700))
	e.writeFile("config.yaml", "listen: 127.0.0.1:7777\n")
	return e
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (e *env) writeFile(rel, content string) {
	e.t.Helper()
	p := filepath.Join(e.home, rel)
	must(e.t, os.MkdirAll(filepath.Dir(p), 0o755))
	must(e.t, os.WriteFile(p, []byte(content), 0o644))
}

func (e *env) update(f func(*store.Tx) error) {
	e.t.Helper()
	must(e.t, e.st.Update(bg, f))
}

func (e *env) setPolicy(f func(*policy.Policy)) {
	e.update(func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner, "t", func(p *policy.Policy) error { f(p); return nil })
		return err
	})
}

func (e *env) addSource(id, typ, kind, cmd, external string) {
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO sources_registry_source (id, type, source_type, command, schedule, timeout, credentials_path,
			dedupe_domain, config, cursor, enabled, stats, default_project, outbound_local, outbound_external)
			VALUES (?, ?, ?, ?, '1m', 30, '', '', '', '', 1, '{"runs":1,"last_success_at":"2026-11-12T09:00:00Z"}', '', 'full', ?)`, id, typ, kind, cmd, external)
		return err
	})
}

func (e *env) declare(d executors.Declaration) {
	b, _ := json.Marshal(d)
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO executors_declaration (harness, rev, doc, state) VALUES (?, 1, ?, 'active')`, d.Harness, string(b))
		return err
	})
}

// script 写一个可执行脚本，返回路径。
func (e *env) script(name, body string) string {
	p := filepath.Join(e.t.TempDir(), name)
	must(e.t, os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755))
	return p
}

// seal 关闭账本（模拟服务停止），并返回数据目录的文件清单与哈希（不含 logs/）。
func (e *env) seal() { must(e.t, e.st.Close()) }

func tree(t *testing.T, home string) string {
	t.Helper()
	var lines []string
	must(t, filepath.WalkDir(home, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(home, p)
		if rel == "logs" || strings.HasPrefix(rel, "logs/") {
			return nil
		}
		if d.IsDir() {
			lines = append(lines, "d "+rel)
			return nil
		}
		b, _ := os.ReadFile(p)
		lines = append(lines, fmt.Sprintf("f %s %x", rel, sha256.Sum256(b)))
		return nil
	}))
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func (e *env) input() Input {
	return Input{Home: e.home, Reg: e.reg, Model: &model.Client{}, Now: now, Env: os.Environ()}
}

func find(t *testing.T, r kdoctor.Report, name string) kdoctor.Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %s in %+v", name, r)
	return kdoctor.Check{}
}

// healthy 备好一个除"最近一次备份"外一切正常的实例：两个端点接替身、已确认的宿主声明、已选范围的来源。
func healthy(t *testing.T) (*env, *modelstub.ClsStub, *modelstub.Stub) {
	e := newEnv(t)
	cls, gen := modelstub.NewCls(), modelstub.New()
	t.Cleanup(cls.Close)
	t.Cleanup(gen.Close)
	gen.SetDefault(modelstub.Reply{Content: `{"answer":"yes","sufficient":true}`})
	e.setPolicy(func(p *policy.Policy) {
		p.JudgeEndpoint = &policy.JudgeEndpoint{URL: cls.URL(), Protocol: policy.ProtocolProbability, Locality: "local"}
		p.GenerateEndpoint = policy.GenerateEndpoint{URL: gen.URL(), Model: "m", Locality: "local"}
	})
	ok := e.script("good-source", "exit 0\n")
	e.addSource("doc-ok", "script", "", ok, "excerpt")
	sc := e.script("selfcheck", `case "$DOCTOR_CASE" in
no-session-kind) echo '{"plugin_version":"0.1.0","hooks":[],"reports_session_kind":false,"overlaps":[]}';;
overlaps) echo '{"plugin_version":"0.1.0","hooks":[],"reports_session_kind":true,"overlaps":[{"kind":"injection","name":"other-todo-hook","detail":"d"},{"kind":"schedule","name":"daily-standup","detail":"d"}]}';;
*) echo '{"plugin_version":"0.1.0","hooks":[{"name":"inject","installed":true}],"reports_session_kind":true,"overlaps":[]}';;
esac
`)
	e.declare(executors.Declaration{Harness: "acc-stub", Headless: &executors.Headless{Command: []string{ok}, Session: executors.Session{Extract: "$.s"},
		Access: map[string][]string{"read": nil}}, Selfcheck: &executors.Selfcheck{Command: []string{sc}}})
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO doctor_seen (subject, at) VALUES ('harness:acc-stub', ?)`, now.Add(-time.Minute).UnixNano())
		return err
	})
	e.writeFile("backups/b1/manifest.json", `{"format":1,"created_at":"2026-11-11T10:00:00Z"}`)
	return e, cls, gen
}

// ACT-DOC-01 与 ACT-DOC-02：健康实例九类检查都有结论、没有错误、退出码 0，JSON 字段齐全；只读且不含账本里的用户内容。
func TestDoctorHealthyInstanceIsReadOnlyAndFreeOfLedgerContent(t *testing.T) {
	e, _, _ := healthy(t)
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec(`INSERT INTO eventlog_event (type, object, actor, reason, payload, chain_depth, basis, resolved_via, evidence, at)
			VALUES ('item.created', 'o1', 'u', '', '{"title":"ACC-DOCTOR-PRIVATE-TITLE"}', 0, '', '', '', 1)`)
		return err
	})
	e.seal()
	before := tree(t, e.home)
	UseRegistry(func() (*store.Registry, error) { return e.reg, nil })
	var out, errb bytes.Buffer
	cenv := client.Env{Getenv: func(k string) string {
		return map[string]string{"APA_HOME": e.home}[k]
	}, UserHome: t.TempDir(), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient}
	if code := client.Main([]string{"doctor", "--json"}, cenv); code != 0 {
		t.Fatalf("exit %d: %s %s", code, out.String(), errb.String())
	}
	var rep kdoctor.Report
	must(t, json.Unmarshal(out.Bytes(), &rep))
	if !rep.OK || len(rep.Checks) != 9 {
		t.Fatalf("report: %s", out.String())
	}
	for i, n := range kdoctor.Order {
		c := rep.Checks[i]
		if c.Name != n || c.Status == kdoctor.Error || c.Why == "" || c.Next == "" {
			t.Errorf("check %d: %+v", i, c)
		}
	}
	if strings.Contains(out.String(), "ACC-DOCTOR-PRIVATE-TITLE") {
		t.Fatal("ledger content leaked into the report")
	}
	if after := tree(t, e.home); after != before {
		t.Fatalf("the data directory changed:\n%s\n---\n%s", before, after)
	}
	dd := find(t, rep, kdoctor.DataDir)
	if dd.Status != kdoctor.OK || dd.Data["last_backup_at"] != "2026-11-11T10:00:00Z" || dd.Data["schema_ledger"] != dd.Data["schema_binary"] {
		t.Fatalf("data dir: %+v", dd)
	}
	// 默认（人读）输出也可用。
	out.Reset()
	if code := client.Main([]string{"doctor"}, cenv); code != 0 || !strings.Contains(out.String(), "host_plugin") {
		t.Fatalf("human output: %d %s", code, out.String())
	}
	if code := client.Main([]string{"doctor", "--bogus"}, cenv); code != domain.ExitUsage {
		t.Fatalf("bad flag exit %d", code)
	}
}

// ACT-DOC-03：目录权限被放开时指出，并给下一步；有错误时退出码 1。
func TestDoctorFlagsOpenDataDirectoryPermissions(t *testing.T) {
	e, _, _ := healthy(t)
	e.seal()
	must(t, os.Chmod(e.home, 0o777))
	c := find(t, Run(bg, e.input()), kdoctor.DataDir)
	if c.Status != kdoctor.Error || !strings.Contains(c.Why, "writable by other users") || !strings.Contains(c.Next, "chmod") {
		t.Fatalf("%+v", c)
	}
	must(t, os.Chmod(e.home, 0o700))
	must(t, os.Chmod(filepath.Join(e.home, "sources"), 0o755))
	if c := find(t, Run(bg, e.input()), kdoctor.DataDir); c.Status != kdoctor.Error || !strings.Contains(c.Why, "sources") {
		t.Fatalf("credential directory: %+v", c)
	}
}

// 数据目录与版本的其余反例：没有账本、账本损坏、账本比二进制新、账本比二进制旧、没有备份。
func TestDoctorDataDirVersionCases(t *testing.T) {
	e := newEnv(t)
	must(t, os.Remove(filepath.Join(e.home, "config.yaml")))
	e.seal()
	if c := find(t, Run(bg, e.input()), kdoctor.DataDir); c.Status != kdoctor.Warning || !strings.Contains(c.Why, "no backup") {
		t.Fatalf("no backup: %+v", c)
	}
	// 账本比二进制新：二进制少一个模块。
	older := store.NewRegistry()
	must(t, older.Register(eventlog.Module))
	in := e.input()
	in.Reg = older
	r := Run(bg, in)
	if c := find(t, r, kdoctor.DataDir); c.Status != kdoctor.Error || !strings.Contains(c.Why, "newer version") || r.OK {
		t.Fatalf("newer: %+v", c)
	}
	if c := find(t, r, kdoctor.Tier); c.Status != kdoctor.Warning || !strings.Contains(c.Why, "skipped") {
		t.Fatalf("dependent checks must be skipped: %+v", c)
	}
	// 损坏的账本。
	must(t, os.WriteFile(filepath.Join(e.home, "ledger.db"), []byte("not a database at all, just text"), 0o644))
	for _, f := range []string{"ledger.db-wal", "ledger.db-shm"} {
		os.Remove(filepath.Join(e.home, f))
	}
	if c := find(t, Run(bg, e.input()), kdoctor.DataDir); c.Status != kdoctor.Error {
		t.Fatalf("damaged: %+v", c)
	}
	// 没有账本。
	must(t, os.Remove(filepath.Join(e.home, "ledger.db")))
	if c := find(t, Run(bg, e.input()), kdoctor.DataDir); c.Status != kdoctor.Error || !strings.Contains(c.Why, "no ledger.db") {
		t.Fatalf("missing: %+v", c)
	}
	must(t, os.RemoveAll(e.home))
	if c := find(t, Run(bg, e.input()), kdoctor.DataDir); c.Status != kdoctor.Error || !strings.Contains(c.Why, "does not exist") {
		t.Fatalf("no dir: %+v", c)
	}
}

// 账本结构比二进制旧：提醒（启动时会先快照再迁移），其余依赖账本的检查跳过。
func TestDoctorOlderLedgerWarnsAndSkips(t *testing.T) {
	home := filepath.Join(t.TempDir(), "apa")
	old := store.NewRegistry()
	must(t, old.Register(eventlog.Module))
	st, err := store.Open(home, old)
	must(t, err)
	must(t, st.Close())
	must(t, os.Chmod(home, 0o700))
	r := Run(bg, Input{Home: home, Reg: fullRegFor(t), Model: &model.Client{}, Now: now})
	c := find(t, r, kdoctor.DataDir)
	if c.Status != kdoctor.Warning || !strings.Contains(c.Why, "migrates when apa serve starts") {
		t.Fatalf("%+v", c)
	}
	if find(t, r, kdoctor.Sources).Status != kdoctor.Warning || !r.OK {
		t.Fatalf("%+v", r)
	}
}

func fullRegFor(t *testing.T) *store.Registry { return fullReg(t) }

// ACT-DOC-04 与 ACT-DPL-10：监听非本机地址而没有 https 的 public_url 是错误，退出码 1；无法解析的 config.yaml 指明位置。
func TestDoctorRuntimeConfigErrors(t *testing.T) {
	e, _, _ := healthy(t)
	e.seal()
	UseRegistry(func() (*store.Registry, error) { return e.reg, nil })
	run := func() (int, kdoctor.Report) {
		var out, errb bytes.Buffer
		code := client.Main([]string{"doctor", "--json"}, client.Env{Getenv: func(k string) string { return map[string]string{"APA_HOME": e.home}[k] },
			UserHome: t.TempDir(), Stdout: &out, Stderr: &errb})
		var rep kdoctor.Report
		must(t, json.Unmarshal(out.Bytes(), &rep))
		return code, rep
	}
	e.writeFile("config.yaml", "listen: 0.0.0.0:7777\n")
	code, rep := run()
	if c := find(t, rep, kdoctor.Config); code != 1 || c.Status != kdoctor.Error || !strings.Contains(c.Why, "public_url") || !strings.Contains(c.Why, "not a local address") {
		t.Fatalf("exposure: %d %+v", code, c)
	}
	e.writeFile("config.yaml", "listen: 127.0.0.1:7777\nthis is not yaml\n")
	code, rep = run()
	if c := find(t, rep, kdoctor.Config); code != 1 || c.Status != kdoctor.Error || !strings.Contains(c.Why, "config.yaml:2") {
		t.Fatalf("unparsable: %d %+v", code, c)
	}
	e.writeFile("config.yaml", "listen: 0.0.0.0:7777\npublic_url: https://apa.example.com\n")
	if code, rep = run(); code != 0 || find(t, rep, kdoctor.Config).Status != kdoctor.OK {
		t.Fatalf("with public_url: %d", code)
	}
	// 主机配置文件无法解析也算运行配置的错误。
	e.writeFile("sources.json", `{"sources": 5}`)
	if c := find(t, Run(bg, e.input()), kdoctor.Config); c.Status != kdoctor.Error || !strings.Contains(c.Why, "sources.json") {
		t.Fatalf("sources.json: %+v", c)
	}
	e.writeFile("sources.json", `{"sources": []}`)
	e.writeFile("declarations/bad.json", `{"harness":""}`)
	if c := find(t, Run(bg, e.input()), kdoctor.Config); c.Status != kdoctor.Error || !strings.Contains(c.Why, "declarations/bad.json") {
		t.Fatalf("declaration: %+v", c)
	}
}

// ACT-DOC-05：形态按手里有什么判断，清空后回到第一档，未配置不是错误。
func TestDoctorTierFollowsConfiguration(t *testing.T) {
	e, _, _ := healthy(t)
	e.seal()
	if c := find(t, Run(bg, e.input()), kdoctor.Tier); c.Data["tier"] != float64(3) && c.Data["tier"] != 3 {
		t.Fatalf("tier 3 expected: %+v", c)
	}
	e2 := newEnv(t)
	e2.seal()
	r := Run(bg, e2.input())
	c := find(t, r, kdoctor.Tier)
	if c.Data["tier"] != 1 || c.Status != kdoctor.OK || !strings.Contains(c.Next, "generate endpoint") || find(t, r, kdoctor.ModelEndpoint).Status != kdoctor.OK {
		t.Fatalf("tier 1: %+v", c)
	}
}

// ACT-DOC-06：示例题是内置固定文本，不含账本数据；判断端点自报概率或协议不符时报错。
func TestDoctorProbesEndpointsWithBuiltInSamples(t *testing.T) {
	e, cls, gen := healthy(t)
	e.seal()
	r := Run(bg, e.input())
	me := find(t, r, kdoctor.ModelEndpoint)
	if me.Status != kdoctor.OK || len(me.Details) != 2 || me.Details[0].Data["elapsed_ms"] == nil {
		t.Fatalf("%+v", me)
	}
	if len(cls.Batches()) != 1 || len(gen.Requests()) != 1 {
		t.Fatalf("requests: %d %d", len(cls.Batches()), len(gen.Requests()))
	}
	cls.SetFault(modelstub.ClsFault{Source: "self_reported"})
	if r := Run(bg, e.input()); r.OK || find(t, r, kdoctor.ModelEndpoint).Status != kdoctor.Error {
		t.Fatalf("self_reported must fail")
	}
	cls.SetFault(modelstub.ClsFault{Protocol: "other/9"})
	if r := Run(bg, e.input()); r.OK {
		t.Fatalf("protocol mismatch must fail")
	}
}

// ACT-DOC-07：命令缺失、工具版本不符、外发范围未选、外部全文外发各被指出；doc-ok 在本地端点上正常。
func TestDoctorSourcesMissingCommandToolVersionUnsetAndFullText(t *testing.T) {
	e, _, gen := healthy(t)
	_ = gen
	e.setPolicy(func(p *policy.Policy) { p.GenerateEndpoint.Locality = "external" })
	e.update(func(tx *store.Tx) error {
		_, err := tx.Exec(`UPDATE sources_registry_source SET outbound_external = 'full' WHERE id = 'doc-ok'`)
		return err
	})
	tool := e.script("mail-adapter", `echo "himalaya 版本 9.9.9 不在声明范围内（[1.0.0, 3.0.0)）" >&2; exit 2`)
	e.addSource("doc-missing", "script", "", "/no/such/command", "meta")
	e.addSource("doc-mail", "script", "mail", tool, "meta")
	e.addSource("doc-unset", "script", "", e.script("s", "exit 0"), "")
	e.seal()
	c := find(t, Run(bg, e.input()), kdoctor.Sources)
	want := map[string]string{
		"doc-missing": "command does not exist", "doc-mail": "version is not in the range",
		"doc-unset": "outbound scope is not chosen", "doc-ok": "full text",
	}
	for id, sub := range want {
		d, ok := c.Find(id)
		if !ok || d.Status == kdoctor.OK || !strings.Contains(d.Why, sub) || d.Data["last_run"] == nil {
			t.Errorf("%s: %+v", id, d)
		}
	}
	// 工具未安装、认证问题、超时之外的退出：分类各不同。
	for stderr, sub := range map[string]string{"未找到 himalaya，请先安装": "not installed", "himalaya 认证失败": "authentication"} {
		tool := e.script("t2", fmt.Sprintf("echo %q >&2; exit 2", stderr))
		problem, _ := probeOne(bg, e.input(), domain.Source{ID: "x", Command: tool})
		if !strings.Contains(problem, sub) {
			t.Errorf("%s: %q", stderr, problem)
		}
	}
	if p, _ := probeOne(bg, e.input(), domain.Source{ID: "x", Command: e.script("t3", "exit 1")}); !strings.Contains(p, "temporarily") {
		t.Errorf("temporary: %q", p)
	}
	if p, _ := probeOne(bg, e.input(), domain.Source{ID: "x", Command: e.script("t4", "exit 0")}); p != "" {
		t.Errorf("ok adapter: %q", p)
	}
}

// ACT-DOC-08：覆盖内置包的用户包用了已移除字段时，逐字段指出并说明内置包生效。
func TestDoctorPackFieldReasons(t *testing.T) {
	e, _, _ := healthy(t)
	e.seal()
	src, dst := "inbound-triage", filepath.Join(e.home, "packs", "inbound-triage")
	must(t, fs.WalkDir(rootpacks.FS, src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		out := filepath.Join(dst, strings.TrimPrefix(p, src))
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		b, _ := fs.ReadFile(rootpacks.FS, p)
		if strings.HasSuffix(p, "triage-screen.md") {
			b = bytes.Replace(b, []byte("on_undecided: yes\n"), []byte("confidence_threshold: 0.7\nescalate_to: x\n"), 1)
		}
		return os.WriteFile(out, b, 0o644)
	}))
	r := Run(bg, e.input())
	c := find(t, r, kdoctor.Packs)
	d, ok := c.Find("inbound-triage")
	if !ok || r.OK || d.Status != kdoctor.Error {
		t.Fatalf("%+v", c)
	}
	for _, w := range []string{"confidence_threshold", "escalate_to", "on_undecided", "builtin pack of the same name is in effect"} {
		if !strings.Contains(d.Why, w) {
			t.Errorf("reason lacks %q: %s", w, d.Why)
		}
	}
}

// ACT-DOC-09、ACT-DOC-10、ACT-SKD-14：插件不上报会话类型、无头命令不可执行、重复提醒只盘点，宿主一侧文件不动。
func TestDoctorHostPluginAndOverlaps(t *testing.T) {
	e, _, _ := healthy(t)
	e.seal()
	// 正常：版本与最近调用时间都在。
	r := Run(bg, e.input())
	hp := find(t, r, kdoctor.HostPlugin)
	d := hp.Details[0]
	if hp.Status != kdoctor.OK || d.Data["plugin_version"] != "0.1.0" || d.Data["last_call_at"] != "2026-11-12T09:59:00Z" {
		t.Fatalf("healthy: %+v", hp)
	}
	if ov := find(t, r, kdoctor.Overlaps); ov.Status != kdoctor.OK || len(ov.Details) != 0 {
		t.Fatalf("overlaps: %+v", ov)
	}
	// 不上报会话类型。
	in := e.input()
	in.Env = append(os.Environ(), "DOCTOR_CASE=no-session-kind")
	hp = find(t, Run(bg, in), kdoctor.HostPlugin)
	if hp.Status != kdoctor.Warning || !strings.Contains(hp.Details[0].Why, "does not report the session type") ||
		!strings.Contains(hp.Details[0].Why, "exit 23") || !strings.Contains(hp.Details[0].Next, "upgrade the plugin") || hp.Details[0].Data["plugin_version"] != "0.1.0" {
		t.Fatalf("no session kind: %+v", hp)
	}
	// 重复提醒：列出两项；宿主一侧的脚本文件不变。
	dir := filepath.Dir(e.home)
	before := tree(t, dir)
	in.Env = append(os.Environ(), "DOCTOR_CASE=overlaps")
	ov := find(t, Run(bg, in), kdoctor.Overlaps)
	if ov.Status != kdoctor.Warning || len(ov.Details) != 2 || !strings.Contains(ov.Details[0].Subject+ov.Details[1].Subject, "other-todo-hook") ||
		!strings.Contains(ov.Details[0].Subject+ov.Details[1].Subject, "daily-standup") {
		t.Fatalf("overlaps: %+v", ov)
	}
	if tree(t, dir) != before {
		t.Fatal("the host side changed")
	}
	// 无头命令不可执行 + 自检命令失败。
	e2 := newEnv(t)
	e2.declare(executors.Declaration{Harness: "gone", Headless: &executors.Headless{Command: []string{"/no/such/harness"}, Session: executors.Session{Extract: "$.s"},
		Access: map[string][]string{"read": nil}}, Selfcheck: &executors.Selfcheck{Command: []string{"/no/such/selfcheck"}}})
	e2.declare(executors.Declaration{Harness: "slow", Selfcheck: &executors.Selfcheck{Command: []string{e2.script("junk", "echo not-json")}}, Executors: nil})
	e2.seal()
	hp = find(t, Run(bg, e2.input()), kdoctor.HostPlugin)
	if hp.Status != kdoctor.Error || !strings.Contains(hp.Details[0].Why, "not executable") || !strings.Contains(hp.Details[0].Why, "selfcheck failed") {
		t.Fatalf("gone: %+v", hp)
	}
	if !strings.Contains(hp.Details[1].Why, "not the JSON") {
		t.Fatalf("junk: %+v", hp.Details[1])
	}
	// 没有声明自检命令：这一项被跳过，不是错误。
	e3 := newEnv(t)
	e3.declare(executors.Declaration{Harness: "plain", Headless: &executors.Headless{Command: []string{"sh"}, Session: executors.Session{Extract: "$.s"},
		Access: map[string][]string{"read": nil}}})
	e3.seal()
	r3 := Run(bg, e3.input())
	if ov := find(t, r3, kdoctor.Overlaps); ov.Status != kdoctor.OK || !strings.Contains(ov.Why, "skipped") || !r3.OK {
		t.Fatalf("skipped: %+v", ov)
	}
	if hp := find(t, Run(bg, newEnv(t).sealed()), kdoctor.HostPlugin); hp.Status != kdoctor.OK {
		t.Fatalf("no host: %+v", hp)
	}
}

func (e *env) sealed() Input { e.seal(); return e.input() }

// 自检命令超过 5 秒、输出过大的情形：报"自检失败"，不阻塞。
func TestSelfcheckTimeoutAndOutputLimit(t *testing.T) {
	e := newEnv(t)
	if _, err := runSelfcheck(bg, []string{e.script("big", `head -c 2000000 /dev/zero | tr '\0' x`)}, os.Environ()); err == nil {
		t.Fatal("oversized output must fail")
	}
	ctx, cancel := context.WithTimeout(bg, time.Millisecond)
	defer cancel()
	if _, err := runSelfcheck(ctx, []string{e.script("slow", "sleep 3")}, os.Environ()); err == nil || !strings.Contains(err.Error(), "within") {
		t.Fatalf("timeout: %v", err)
	}
}

// ACT-DOC-11：渠道 ext 最近领取时间与积压；ext 停止时有积压，领完后积压清零、时间更新。
func TestDoctorChannelsBacklogBeforeAndAfterClaim(t *testing.T) {
	e := newEnv(t)
	e.update(func(tx *store.Tx) error {
		_, _, err := auth.New(nil).Issue(tx, auth.Spec{Kind: auth.Ext, Scope: "channel.tg"})
		return err
	})
	q := delivery.New(delivery.Options{})
	e.update(func(tx *store.Tx) error {
		for _, id := range []string{"d1", "d2", "d3"} {
			if _, _, err := q.Create(tx, domain.Delivery{ID: id, Item: "it", Route: delivery.ChannelRoute("tg"), Version: 1,
				Class: domain.DeliveryReminder, Content: domain.DeliveryContent{Title: "SECRET-TITLE"}}); err != nil {
				return err
			}
		}
		return nil
	})
	seen := kdoctor.NewSeen(time.Second)
	must(t, seen.Note(bg, e.st, kdoctor.SubjectExt+"channel.tg", now.Add(-time.Hour)))
	in := e.input()
	in.Home = e.home
	snapshot := func() kdoctor.Detail {
		l, err := openLedger(bg, in)
		must(t, err)
		defer l.cleanup()
		f, err := kdoctor.ReadFacts(bg, l.st)
		must(t, err)
		c := kdoctor.Kernel(bg, f, kdoctor.Env{})[4]
		d, _ := c.Find("channel.tg")
		return *d
	}
	d := snapshot()
	if d.Status != kdoctor.Warning || d.Data["backlog"] != 3 || d.Data["last_seen_at"] != "2026-11-12T09:00:00Z" {
		t.Fatalf("before: %+v", d)
	}
	e.update(func(tx *store.Tx) error {
		for i := 0; i < 3; i++ {
			c, err := q.Claim(tx, delivery.ChannelRoute("tg"))
			if err != nil {
				return err
			}
			if _, _, err := q.Ack(tx, c.ID, c.Version, "r"); err != nil {
				return err
			}
		}
		return nil
	})
	must(t, seen.Note(bg, e.st, kdoctor.SubjectExt+"channel.tg", now))
	d = snapshot()
	if d.Status != kdoctor.OK || d.Data["backlog"] != 0 || d.Data["last_seen_at"] != "2026-11-12T10:00:00Z" {
		t.Fatalf("after: %+v", d)
	}
	r := Run(bg, e.input())
	if b, _ := json.Marshal(r); strings.Contains(string(b), "SECRET-TITLE") {
		t.Fatal("delivery content leaked")
	}
}

// ACT-BAK-09：--no-secrets 恢复出的目录里，缺失的来源凭据与 webhook 接收密钥在来源检查里给警告，补齐后消失。
func TestDoctorFlagsSecretsMissingAfterNoSecretsRestore(t *testing.T) {
	e, _, _ := healthy(t)
	e.writeFile(backup.NoticeFile, `{"no_secrets":true,"withheld":["sources/doc-ok","webhook-keys/hook1"]}`)
	e.seal()
	UseRegistry(func() (*store.Registry, error) { return e.reg, nil })
	run := func() kdoctor.Check {
		var out, errb bytes.Buffer
		cenv := client.Env{Getenv: func(k string) string { return map[string]string{"APA_HOME": e.home}[k] },
			UserHome: t.TempDir(), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient}
		client.Main([]string{"doctor", "--json"}, cenv)
		var rep kdoctor.Report
		must(t, json.Unmarshal(out.Bytes(), &rep))
		return find(t, rep, kdoctor.Sources)
	}
	c := run()
	cred, ok1 := c.Find("doc-ok")
	key, ok2 := c.Find("hook1")
	if c.Status != kdoctor.Warning || !ok1 || !ok2 || !strings.Contains(cred.Why, "credentials are missing") || !strings.Contains(key.Next, "rotate") {
		t.Fatalf("sources check: %+v", c)
	}
	e.writeFile("sources/doc-ok/cred", "x")
	e.writeFile("webhook-keys/hook1", "k")
	if c := run(); c.Status != kdoctor.OK {
		t.Fatalf("after re-auth and rotation: %+v", c)
	}
}
