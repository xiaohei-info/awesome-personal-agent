package serve

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/delivery"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

type result struct {
	code        int
	out, errOut string
}

func envOf(home, userHome, stdin string, out, errb *bytes.Buffer) client.Env {
	vars := map[string]string{"APA_HOME": home}
	return client.Env{Getenv: func(k string) string { return vars[k] }, UserHome: userHome,
		Stdin: strings.NewReader(stdin), Stdout: out, Stderr: errb, HTTP: http.DefaultClient}
}

func apa(t *testing.T, home, stdin string, args ...string) result {
	t.Helper()
	var out, errb bytes.Buffer
	code := client.Main(args, envOf(home, t.TempDir(), stdin, &out, &errb))
	return result{code, out.String(), errb.String()}
}

const pw = "correct-horse"

// ACT-BOOT-01：空目录上 apa init 建立完整数据目录并设置 Web 登录，数据不落在 APA_HOME 之外。
func TestInitBuildsCompleteDataDirectoryInsideAPAHome(t *testing.T) {
	root := t.TempDir()
	home := filepath.Join(root, "apa-home")
	userHome := t.TempDir()
	var out, errb bytes.Buffer
	code := client.Main([]string{"init"}, envOf(home, userHome, pw+"\n"+pw+"\n", &out, &errb))
	if code != 0 {
		t.Fatalf("init exit %d: %s", code, errb.String())
	}
	for _, name := range []string{"config.yaml", "ledger.db"} {
		if st, err := os.Stat(filepath.Join(home, name)); err != nil || !st.Mode().IsRegular() {
			t.Fatalf("%s missing: %v", name, err)
		}
	}
	for _, name := range []string{"blobs", "packs", "sources", "runs", "logs"} {
		if st, err := os.Stat(filepath.Join(home, name)); err != nil || !st.IsDir() {
			t.Fatalf("%s/ missing: %v", name, err)
		}
	}
	if st, _ := os.Stat(filepath.Join(home, "sources")); st.Mode().Perm() != 0o700 {
		t.Fatalf("sources/ must be 0700, got %v", st.Mode().Perm())
	}
	if ents, _ := os.ReadDir(root); len(ents) != 1 {
		t.Fatalf("init wrote outside APA_HOME: %v", ents)
	}
	if ents, _ := os.ReadDir(userHome); len(ents) != 0 {
		t.Fatalf("init must not touch the user home when APA_HOME is set: %v", ents)
	}
	if strings.Contains(out.String()+errb.String(), pw+pw) || strings.Contains(out.String(), pw) {
		t.Fatalf("password leaked to stdout: %q", out.String())
	}
}

func TestInitDefaultsToDotApaUnderUserHome(t *testing.T) {
	userHome := t.TempDir()
	var out, errb bytes.Buffer
	e := envOf("", userHome, pw+"\n", &out, &errb)
	if code := client.Main([]string{"init", "--password-stdin"}, e); code != 0 {
		t.Fatalf("init exit %d: %s", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(userHome, ".apa", "ledger.db")); err != nil {
		t.Fatal(err)
	}
}

func TestInitRejectsBadPasswordAndCreatesNoLedger(t *testing.T) {
	for name, stdin := range map[string]string{"short": "short\nshort\n", "mismatch": pw + "\nother-pass-1\n", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			home := filepath.Join(t.TempDir(), "h")
			r := apa(t, home, stdin, "init")
			if r.code != 2 {
				t.Fatalf("want exit 2, got %+v", r)
			}
			// 被拒绝的 init 即使留下了空账本，serve 也必须拒绝启动，而不是放行一个无法登录的内核。
			if s := apa(t, home, "", "serve"); s.code != 2 {
				t.Fatalf("serve must refuse a home without a login password: %+v", s)
			}
		})
	}
}

func TestInitKeepsEditedConfigOnRerun(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	cfg := filepath.Join(home, "config.yaml")
	if err := os.WriteFile(cfg, []byte("listen: 127.0.0.1:9\nmax_subprocesses: 7\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(cfg); !strings.Contains(string(b), "max_subprocesses: 7") {
		t.Fatalf("rerun overwrote config.yaml: %q", b)
	}
}

func TestInitAndServeRejectUnknownArguments(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	for _, args := range [][]string{{"init", "extra"}, {"init", "--bogus"}, {"serve", "extra"}} {
		if r := apa(t, home, pw+"\n", args...); r.code != 2 {
			t.Fatalf("%v: want exit 2, got %+v", args, r)
		}
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("rejected arguments must not create the data directory")
	}
}

func TestServeRefusesUninitializedHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	r := apa(t, home, "", "serve")
	if r.code != 2 || !strings.Contains(r.errOut, "apa init") {
		t.Fatalf("want exit 2 pointing at apa init: %+v", r)
	}
	if _, err := os.Stat(home); err == nil {
		t.Fatal("serve must not create the data directory")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// start 在已 init 的目录上启动 serve，返回监听地址与停止函数（停止后断言干净退出）。
func start(t *testing.T, home string) (net.Addr, func()) {
	t.Helper()
	var out, errb bytes.Buffer
	c := &client.Ctx{Env: envOf(home, t.TempDir(), "", &out, &errb)}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan net.Addr, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, c, func(a net.Addr) { ready <- a }) }()
	select {
	case a := <-ready:
		return a, func() {
			cancel()
			if err := <-done; err != nil {
				t.Fatalf("serve must stop cleanly: %v", err)
			}
		}
	case err := <-done:
		t.Fatalf("serve exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not become ready")
	}
	return nil, nil
}

func login(t *testing.T, addr net.Addr, password string) int {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": password})
	resp, err := http.Post("http://"+addr.String()+"/v1/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

// ACT-BOOT-02：默认配置下 serve 只监听回环；init 时设置的口令能登录，错误口令不能。
func TestServeListensOnLoopbackOnlyAndAcceptsInitPassword(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	cfg := filepath.Join(home, "config.yaml")
	b, _ := os.ReadFile(cfg)
	if !strings.Contains(string(b), "listen: 127.0.0.1:7777") {
		t.Fatalf("default config must bind loopback: %q", b)
	}
	// 只把端口换成空闲端口（避免与本机 7777 冲突），地址部分保持 init 写出的默认值。
	patched := strings.Replace(string(b), "127.0.0.1:7777", "127.0.0.1:"+strconv.Itoa(freePort(t)), 1)
	if err := os.WriteFile(cfg, []byte(patched), 0o644); err != nil {
		t.Fatal(err)
	}
	addr, stop := start(t, home)
	defer stop()
	if !addr.(*net.TCPAddr).IP.IsLoopback() {
		t.Fatalf("default listen must be loopback, got %s", addr)
	}
	if got := login(t, addr, pw); got != 200 {
		t.Fatalf("init password must log in, got %d", got)
	}
	if got := login(t, addr, "wrong-password"); got != 401 {
		t.Fatalf("wrong password must be 401, got %d", got)
	}
}

func TestServeReportsBusyPortAsConflict(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	port := l.Addr().(*net.TCPAddr).Port
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte("listen: 127.0.0.1:"+strconv.Itoa(port)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r := apa(t, home, "", "serve")
	if r.code != 6 || !strings.Contains(r.errOut, "cannot listen") {
		t.Fatalf("want conflict exit 6: %+v", r)
	}
}

func TestServeRejectsInvalidConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	_ = os.WriteFile(filepath.Join(home, "config.yaml"), []byte("bogus: 1\n"), 0o644)
	if r := apa(t, home, "", "serve"); r.code != 2 {
		t.Fatalf("invalid config must fail with exit 2: %+v", r)
	}
}

// 配置里的 delivery_unclaimed_after 经装配传给投递队列：无人领取超过它就转 fallback_web（ACT-DLV-06）。
func TestAssemblePassesDeliveryUnclaimedAfter(t *testing.T) {
	l := config.LayoutOf(filepath.Join(t.TempDir(), "h"))
	if err := l.Ensure(); err != nil {
		t.Fatal(err)
	}
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(l.Home, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Default()
	cfg.DeliveryUnclaimedAfter = 300 * time.Millisecond
	k, err := assemble(st, l, cfg, "http://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	state := func() domain.DeliveryState {
		var d domain.Delivery
		if err := st.Update(context.Background(), func(tx *store.Tx) (err error) {
			if err = k.att.Tick(tx); err != nil {
				return err
			}
			d, err = delivery.New(delivery.Options{}).Get(tx, "d1")
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return d.State
	}
	if err := st.Update(context.Background(), func(tx *store.Tx) error {
		_, _, err := delivery.New(delivery.Options{}).Create(tx, domain.Delivery{ID: "d1", Item: "it_1", Class: domain.DeliveryBrief,
			Route: "chan", Version: 1, Content: domain.DeliveryContent{Title: "t"}})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if got := state(); got != domain.DeliveryPending {
		t.Fatalf("must not fall back before the configured time, got %s", got)
	}
	time.Sleep(400 * time.Millisecond)
	if got := state(); got != domain.DeliveryFallbackWeb {
		t.Fatalf("configured unclaimed_after ignored, state %s (default would be 30m)", got)
	}
}

// 终端式 stdin（字符设备）取不到口令时给出用法错误；没有结尾换行的最后一行也算有效输入。
func TestInitPasswordInputEdges(t *testing.T) {
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	var out, errb bytes.Buffer
	e := envOf(filepath.Join(t.TempDir(), "h"), t.TempDir(), "", &out, &errb)
	e.Stdin = devnull
	if code := client.Main([]string{"init"}, e); code != 2 {
		t.Fatalf("empty terminal input must fail with exit 2, got %d", code)
	}
	home := filepath.Join(t.TempDir(), "h2")
	if r := apa(t, home, pw, "init", "--password-stdin"); r.code != 0 {
		t.Fatalf("last line without newline must be accepted: %+v", r)
	}
	if r := apa(t, home, pw+"\n"+pw, "init"); r.code != 0 { // 重复 init 更换口令，第二次输入无结尾换行
		t.Fatalf("%+v", r)
	}
}

// 终端输入时先关回显、读完（含失败）后恢复回显；用替身 stty 记录调用顺序。
func TestInitDisablesEchoWhileReadingFromTerminalAndRestoresIt(t *testing.T) {
	bin, logf := t.TempDir(), filepath.Join(t.TempDir(), "stty.log")
	script := "#!/bin/sh\necho \"$@\" >> " + logf + "\n"
	if err := os.WriteFile(filepath.Join(bin, "stty"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer devnull.Close()
	var out, errb bytes.Buffer
	e := envOf(filepath.Join(t.TempDir(), "h"), t.TempDir(), "", &out, &errb)
	e.Stdin = devnull
	client.Main([]string{"init"}, e)
	if got, _ := os.ReadFile(logf); string(got) != "-echo\necho\n" {
		t.Fatalf("stty calls = %q, want -echo then echo", got)
	}
}

// ---- 装配完整性 ----

// initOnFreePort 在新目录上 init，并把监听端口换成空闲端口。
func initOnFreePort(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "h")
	if r := apa(t, home, pw+"\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatal(r)
	}
	listen := "listen: 127.0.0.1:" + strconv.Itoa(freePort(t)) + "\n"
	if err := os.WriteFile(filepath.Join(home, "config.yaml"), []byte(listen), 0o644); err != nil {
		t.Fatal(err)
	}
	return home
}

// appendixRoute 是附录 B.2 的一条路由；methods 为空表示表里只写了路径（设置组），任一方法挂载都算。
type appendixRoute struct {
	methods []string
	path    string
}

// appendixRoutes 解析附录 B.2 表格第二列，它是路由清单的唯一来源。写法有三种：
// `GET/POST /items`、接在前一条后面的 `/accept`（换掉最后一段，沿用方法）、只有路径的 `/settings/policy`。
func appendixRoutes(t *testing.T) []appendixRoute {
	t.Helper()
	data, err := os.ReadFile("../../docs/appendix-b-api-cli.md")
	if err != nil {
		t.Fatal(err)
	}
	_, rest, _ := strings.Cut(string(data), "## B.2")
	section, _, _ := strings.Cut(rest, "## B.3")
	span := regexp.MustCompile("`([^`]+)`")
	var out []appendixRoute
	for _, line := range strings.Split(section, "\n") {
		cells := strings.Split(line, "|")
		if len(cells) < 4 {
			continue
		}
		var prev *appendixRoute
		for _, m := range span.FindAllStringSubmatch(cells[2], -1) {
			r := appendixRoute{}
			f := strings.Fields(m[1])
			r.path, _, _ = strings.Cut(f[len(f)-1], "?")
			if len(f) == 2 {
				r.methods = strings.Split(f[0], "/")
			}
			if len(f) == 1 && prev != nil && strings.Count(r.path, "/") == 1 {
				r.methods, r.path = prev.methods, path.Dir(prev.path)+r.path
			}
			out = append(out, r)
			prev = &r
		}
	}
	if len(out) < 40 {
		t.Fatalf("parsed only %d routes from appendix B.2; the table format changed", len(out))
	}
	return out
}

// notYetImplemented 是附录 B.2 列出、但 main 上还没有实现的路由（目标设计先于代码）。这不是路由清单，只是缺口台账，只减不增：
// 实现并挂载后本测试会要求删掉对应条目；实现了却忘了挂，由 TestEveryRouteGroupPackageIsMounted 拦下。
var notYetImplemented = map[string]bool{}

// mounted 用未登录的真实请求探测：骨干对未挂载的路由统一回 "no such route"；已挂载的要么 401，要么进了处理器（处理器自己的 404 也算挂载）。
func mounted(t *testing.T, addr net.Addr, method, p string) bool {
	t.Helper()
	url := "http://" + addr.String() + "/v1" + p
	if strings.HasPrefix(p, "/ingest/") || strings.HasPrefix(p, "/forms/") { // 外部系统与人点开的链接不带版本前缀（附录 B）
		url = "http://" + addr.String() + p
	}
	req, err := http.NewRequest(method, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return !(resp.StatusCode == http.StatusNotFound && strings.Contains(string(body), "no such route"))
}

// ACT-REL-03 的路由部分：真实 serve 上，附录 B.2 的每条路由都已挂载；Web 根路径可访问，且不抢 API 路径。
func TestServeMountsEveryAppendixRouteAndWeb(t *testing.T) {
	addr, stop := start(t, initOnFreePort(t))
	defer stop()
	placeholder := regexp.MustCompile(`\{[^}]*\}`)
	stale := map[string]bool{}
	for k := range notYetImplemented {
		stale[k] = true
	}
	for _, r := range appendixRoutes(t) {
		probe := placeholder.ReplaceAllString(r.path, "x")
		if len(r.methods) == 0 {
			got := false
			for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
				got = got || mounted(t, addr, m, probe)
			}
			check(t, r.path, got, stale)
			continue
		}
		for _, m := range r.methods {
			check(t, m+" "+r.path, mounted(t, addr, m, probe), stale)
		}
	}
	for k := range stale {
		t.Errorf("%s is listed in notYetImplemented but appendix B.2 no longer has it; delete the entry", k)
	}

	resp, err := http.Get("http://" + addr.String() + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	// CI 先构建前端，这里是 200；本地没构建时 Web handler 回 503 并说明原因。两者都证明 Web 已挂载。
	if built := resp.StatusCode == 200 && strings.Contains(string(body), "<html"); !built &&
		!(resp.StatusCode == 503 && strings.Contains(string(body), "not built")) {
		t.Fatalf("web root not served: %d %q", resp.StatusCode, body)
	}
	api, err := http.Get("http://" + addr.String() + "/v1/no-such-route")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Body.Close()
	if api.StatusCode != 404 {
		t.Fatalf("unknown API paths must stay 404, got %d", api.StatusCode)
	}
}

func check(t *testing.T, key string, got bool, stale map[string]bool) {
	t.Helper()
	delete(stale, key)
	switch {
	case !got && !notYetImplemented[key]:
		t.Errorf("%s is in appendix B.2 but not mounted by apa serve", key)
	case got && notYetImplemented[key]:
		t.Errorf("%s is mounted now; delete it from notYetImplemented", key)
	}
}

// api/ 下导出路由分组（返回 core.RouteGroup 的函数或 core.Register）的包都必须被 cli/serve 引入。
// 不靠路由表：新路由组哪怕还没写进附录 B，或路由还记在 notYetImplemented 里，忘了挂载也会失败。
func TestEveryRouteGroupPackageIsMounted(t *testing.T) {
	const module = "github.com/xiaohei-info/awesome-personal-agent/"
	imported := map[string]bool{}
	fset := token.NewFileSet()
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		pf, err := parser.ParseFile(fset, f, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, im := range pf.Imports {
			imported[strings.Trim(im.Path.Value, `"`)] = true
		}
	}
	groups := 0
	err := filepath.WalkDir("../../api", func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return err
		}
		rel, _ := filepath.Rel("../..", filepath.Dir(p))
		pkg := filepath.ToSlash(rel)
		if pkg == "api/core" { // 骨干本身
			return nil
		}
		pf, err := parser.ParseFile(fset, p, nil, 0)
		if err != nil {
			return err
		}
		if declaresRouteGroup(pf) {
			groups++
			if !imported[module+pkg] {
				t.Errorf("%s exports a route group but cli/serve does not mount it", pkg)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if groups == 0 {
		t.Fatal("found no route groups; the scan is broken")
	}
}

func declaresRouteGroup(f *ast.File) bool {
	found := false
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.FuncDecl:
			if v.Recv == nil && v.Name.IsExported() && v.Type.Results != nil {
				for _, r := range v.Type.Results.List {
					if s, ok := r.Type.(*ast.SelectorExpr); ok && s.Sel.Name == "RouteGroup" {
						found = true
					}
				}
			}
		case *ast.CallExpr:
			if s, ok := v.Fun.(*ast.SelectorExpr); ok && s.Sel.Name == "Register" {
				if x, ok := s.X.(*ast.Ident); ok && x.Name == "core" {
					found = true
				}
			}
		}
		return true
	})
	return found
}

// TestEveryStoreModuleIsRegistered 扫描 kernel/ 与 api/：声明了包级 `var Module = store.Module{...}` 的包，
// 必须在 registry() 里登记。漏登记的模块没有建表，第一次写它的表时才失败（如只能发文字的渠道上的表单令牌）。
func TestEveryStoreModuleIsRegistered(t *testing.T) {
	const module = "github.com/xiaohei-info/awesome-personal-agent/"
	fset := token.NewFileSet()
	pf, err := parser.ParseFile(fset, "serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	alias := map[string]string{} // 本文件里的包名 → 导入路径
	for _, im := range pf.Imports {
		p := strings.Trim(im.Path.Value, `"`)
		name := path.Base(p)
		if im.Name != nil {
			name = im.Name.Name
		}
		alias[name] = p
	}
	registered := map[string]bool{}
	for _, d := range pf.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == "registry" {
			ast.Inspect(fd, func(n ast.Node) bool {
				if s, ok := n.(*ast.SelectorExpr); ok && s.Sel.Name == "Module" {
					if x, ok := s.X.(*ast.Ident); ok {
						registered[alias[x.Name]] = true
					}
				}
				return true
			})
		}
	}
	declared := 0
	for _, root := range []string{"../../kernel", "../../api"} {
		err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return err
			}
			f, err := parser.ParseFile(fset, p, nil, 0)
			if err != nil {
				return err
			}
			if !declaresStoreModule(f) {
				return nil
			}
			declared++
			rel, _ := filepath.Rel("../..", filepath.Dir(p))
			if pkg := module + filepath.ToSlash(rel); !registered[pkg] {
				t.Errorf("%s declares a store module but cli/serve registry() does not register it", rel)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if declared == 0 {
		t.Fatal("found no store modules; the scan is broken")
	}
}

func declaresStoreModule(f *ast.File) bool {
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, sp := range g.Specs {
			vs := sp.(*ast.ValueSpec)
			for i, n := range vs.Names {
				if n.Name != "Module" || i >= len(vs.Values) {
					continue
				}
				if cl, ok := vs.Values[i].(*ast.CompositeLit); ok {
					if s, ok := cl.Type.(*ast.SelectorExpr); ok && s.Sel.Name == "Module" {
						return true
					}
				}
			}
		}
	}
	return false
}

// session 是 Web 登录后的客户端。
type session struct {
	t      *testing.T
	base   string
	cookie []*http.Cookie
	csrf   string
}

func webLogin(t *testing.T, addr net.Addr) *session {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"password": pw})
	base := "http://" + addr.String() + "/v1"
	resp, err := http.Post(base+"/auth/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		CSRF string `json:"csrf_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil || resp.StatusCode != 200 {
		t.Fatalf("login: %d %v", resp.StatusCode, err)
	}
	return &session{t, base, resp.Cookies(), out.CSRF}
}

func (s *session) do(method, p, body string) (int, string) {
	s.t.Helper()
	req, err := http.NewRequest(method, s.base+p, strings.NewReader(body))
	if err != nil {
		s.t.Fatal(err)
	}
	for _, c := range s.cookie {
		req.AddCookie(c)
	}
	req.Header.Set("X-CSRF-Token", s.csrf)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		s.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// ACT-CMD-04（主机配置文件一路）：主机文件登记的脚本来源被时钟调度、经调度器拉起、事件走到入站流水线；
// 声明文件直接生效；重启后重复登记不出错。同时跑一个简报实例：到期要走到任务型；任何 Timer 分派错模块都会记 error，这里一条都不许有。
func TestServeLoadsHostFilesAndRunsKernelServices(t *testing.T) {
	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelError})))
	defer slog.SetDefault(prev)

	home := initOnFreePort(t)
	script := filepath.Join(t.TempDir(), "emit.sh")
	emit := `#!/bin/sh
cat >/dev/null
echo '{"id":"e1","type":"note.created","subject":"from host file","occurred_at":"2026-01-01T00:00:00Z"}'
echo '{"cursor":"c1"}'
`
	if err := os.WriteFile(script, []byte(emit), 0o755); err != nil {
		t.Fatal(err)
	}
	src, _ := json.Marshal(map[string]any{"sources": []any{map[string]any{"id": "src_host", "type": "script", "source_type": "note",
		"command": script, "schedule": "1s", "timeout": 10, "dedupe_domain": "note"}}})
	if err := os.WriteFile(filepath.Join(home, "sources.json"), src, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, "declarations"), 0o755); err != nil {
		t.Fatal(err)
	}
	decl := `{"harness":"acme","executors":[{"id":"acme.cli","kind":"external-cli"}]}`
	if err := os.WriteFile(filepath.Join(home, "declarations", "acme.json"), []byte(decl), 0o644); err != nil {
		t.Fatal(err)
	}

	addr, stop := start(t, home)
	s := webLogin(t, addr)
	if code, body := s.do("POST", "/instances", `{"pack":"brief","params":{"schedule":"1s"}}`); code >= 300 {
		t.Fatalf("create instance: %d %s", code, body)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, body := s.do("GET", "/sources/src_host/events?disposition=unjudged", "")
		if code == 200 && strings.Contains(body, "from host file") { // 未配置模型：判断型记 unjudged，留在来源页
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("source event never reached the inbound pipeline: %d %s; logs: %s", code, body, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	if code, body := s.do("GET", "/settings/executors", ""); code != 200 || !strings.Contains(body, "acme.cli") {
		t.Fatalf("host declaration not active: %d %s", code, body)
	}
	if code, body := s.do("GET", "/packs", ""); code != 200 || !strings.Contains(body, "inbound-triage") {
		t.Fatalf("builtin packs not loaded: %d %s", code, body)
	}
	// 实例到期 → instance.fired → 规则执行建 brief 事项 → 任务型。没有设默认 harness，所以转 needs_input 交本人（§8.7）。
	for {
		code, body := s.do("GET", "/items?kind=brief", "")
		if code == 200 && strings.Contains(body, `"phase":"needs_input"`) && strings.Contains(body, "默认 harness") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("instance firing never reached the task agent: %d %s; logs: %s", code, body, logs.String())
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(2 * clockTicks) // 让实例与来源的 Timer 再各到期一次
	stop()

	addr, stop = start(t, home)
	s = webLogin(t, addr)
	if code, body := s.do("GET", "/sources/src_host", ""); code != 200 || !strings.Contains(body, script) {
		t.Fatalf("source lost after restart: %d %s", code, body)
	}
	stop()
	if logs.Len() != 0 {
		t.Fatalf("kernel services logged errors:\n%s", logs.String())
	}
}

const clockTicks = 1100 * time.Millisecond

func TestServeRefusesBadHostFile(t *testing.T) {
	home := initOnFreePort(t)
	if err := os.WriteFile(filepath.Join(home, "sources.json"), []byte(`{"sources":[{"id":"x","type":"bogus"}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := apa(t, home, "", "serve"); r.code != 2 || !strings.Contains(r.errOut, "sources.json") {
		t.Fatalf("a bad host file must refuse to start: %+v", r)
	}
}

// notBundled 是 sources/*/cmd/* 里有意不进 bundledPull 的程序及理由（§9.7、§14.12）；其余程序必须在 bundledPull 里。
var notBundled = map[string]string{
	"mail-himalaya": "邮件适配器，按 §9.7 只在 Web 或主机配置文件登记",
	"calendar":      "日历适配器，同上",
	"hosted-reader": "托管文档读取器，不是来源",
}

// 新增拉取程序漏登记会失败：清单取自 sources/*/cmd/* 目录，bundledPull 不能少、也不能多出目录里没有的名字。
func TestEveryBundledPullProgramIsRegistered(t *testing.T) {
	dirs, err := filepath.Glob("../../sources/*/cmd/*")
	if err != nil || len(dirs) == 0 {
		t.Fatalf("no programs found under sources/*/cmd: %v", err)
	}
	onDisk := map[string]bool{}
	for _, d := range dirs {
		name := filepath.Base(d)
		onDisk[name] = true
		_, exempt := notBundled[name]
		if in := slices.Contains(bundledPull, name); in == exempt {
			t.Errorf("%s: in bundledPull=%v, in notBundled=%v; a program belongs to exactly one of them", name, in, exempt)
		}
	}
	for _, name := range append(slices.Clone(bundledPull), slices.Collect(maps.Keys(notBundled))...) {
		if !onDisk[name] {
			t.Errorf("%s is listed but has no sources/*/cmd/%s", name, name)
		}
	}
}

// ACT-PACK-04、ACT-RULE-07：数据目录里的包内容变化后，热加载在下一轮生效；改坏整体拒绝、保留上一有效版本，
// 同一坏内容只产出一条健康投递；规则内容变了回到 observe 并产出一条健康投递；内容没变什么都不重做。
func TestReloadPacksFollowsDataDirectory(t *testing.T) {
	home := initOnFreePort(t)
	l := config.LayoutOf(home)
	pack := filepath.Join(l.Packs, "brief")
	if err := os.CopyFS(pack, os.DirFS("../../packs/brief")); err != nil {
		t.Fatal(err)
	}
	reg, err := registry()
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(home, reg)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg, err := config.Load(l.Config)
	if err != nil {
		t.Fatal(err)
	}
	k, err := assemble(st, l, cfg, "http://127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := k.start(ctx); err != nil {
		t.Fatal(err)
	}
	report := func(what string, err error) {
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	count := func(q string, args ...any) (n int64) {
		t.Helper()
		if err := st.View(ctx, func(tx *store.Tx) error {
			r, err := tx.Query(q, args...)
			if err != nil {
				return err
			}
			defer r.Close()
			if r.Next() {
				err = r.Scan(&n)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return n
	}
	healthDeliveries := func() int64 { return count(`SELECT COUNT(*) FROM delivery_delivery WHERE class = 'health'`) }
	ruleMode := func() (mode string) {
		t.Helper()
		if err := st.View(ctx, func(tx *store.Tx) error {
			r, err := tx.Query(`SELECT mode FROM rules_state WHERE pack = 'brief'`)
			if err != nil {
				return err
			}
			defer r.Close()
			if r.Next() {
				err = r.Scan(&mode)
			}
			return err
		}); err != nil {
			t.Fatal(err)
		}
		return mode
	}
	edit := func(old, repl string) {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(pack, "pack.yaml"))
		if err != nil || !strings.Contains(string(b), old) {
			t.Fatalf("pack.yaml: %v", err)
		}
		if err := os.WriteFile(filepath.Join(pack, "pack.yaml"), []byte(strings.Replace(string(b), old, repl, 1)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	version := func() string { p, _ := k.packs.Get("brief"); return p.Version }

	var key string
	if err := st.View(ctx, func(tx *store.Tx) error {
		r, err := tx.Query(`SELECT key FROM rules_state WHERE pack = 'brief'`)
		if err != nil {
			return err
		}
		defer r.Close()
		if r.Next() {
			err = r.Scan(&key)
		}
		return err
	}); err != nil || key == "" {
		t.Fatalf("brief rule not registered: %v", err)
	}
	in, _ := json.Marshal(map[string]string{"rule": key, "mode": "auto"})
	if _, err := k.x.Do(ctx, command.Request{Identity: hostOwner, Command: "rule.set_mode", Input: in, Owner: owner}); err != nil {
		t.Fatal(err)
	}
	base := healthDeliveries()

	k.reloadPacks(ctx, report) // 内容没变：不重装，规则不动，没有投递
	if ruleMode() != "auto" || healthDeliveries() != base || version() != "1.0.0" {
		t.Fatal("an unchanged data directory must change nothing")
	}

	edit("version: 1.0.0", "version: 1.0.1") // 不改规则的内容变化：下一轮生效，auto 保持
	k.reloadPacks(ctx, report)
	if version() != "1.0.1" || ruleMode() != "auto" || healthDeliveries() != base {
		t.Fatalf("edited pack not reloaded cleanly: %s %s", version(), ruleMode())
	}

	edit("version: 1.0.1", "version: [") // 改坏：整体拒绝，保留 1.0.1，同一坏内容重复轮询只报一次
	for range 3 {
		k.reloadPacks(ctx, report)
	}
	if version() != "1.0.1" || len(k.packs.Health()) != 1 || healthDeliveries() != base+1 {
		t.Fatalf("bad pack: version %s, health %v, deliveries %d", version(), k.packs.Health(), healthDeliveries())
	}
	edit("version: [", "version: [[") // 坏内容变了：再报一次
	k.reloadPacks(ctx, report)
	if healthDeliveries() != base+2 {
		t.Fatalf("a changed bad content must be reported again: %d", healthDeliveries())
	}

	edit("version: [[", "version: 1.0.2") // 修好：健康视图清空，新版本生效
	k.reloadPacks(ctx, report)
	if version() != "1.0.2" || len(k.packs.Health()) != 0 || healthDeliveries() != base+2 {
		t.Fatalf("fixed pack: version %s, health %v", version(), k.packs.Health())
	}

	edit("        kind: brief\n", "        kind: brief\n      - action: create_item\n        kind: brief\n") // 规则内容变化：回到 observe 并产出一条健康投递
	k.reloadPacks(ctx, report)
	if ruleMode() != "observe" || healthDeliveries() != base+3 {
		t.Fatalf("changed rule: mode %s, deliveries %d", ruleMode(), healthDeliveries())
	}
	k.reloadPacks(ctx, report)
	if healthDeliveries() != base+3 {
		t.Fatal("the rule fallback must be reported once")
	}
}

// ACT-FUP：跟进服务用的是已加载能力包里 follow_up 规则的 schedule 与当前模式。serve 运行中在数据目录覆盖内置包、
// 把间隔改成 1s，热加载后新进入等待的事项按 1s 跟进；规则保持包声明的 observe：到期只记 rule.observed，
// 不调模型（未配置模型时 auto 会记 health.alert）、不提醒。
func TestServeFollowUpUsesPackScheduleAndMode(t *testing.T) {
	home := initOnFreePort(t)
	dst := filepath.Join(home, "packs", "follow-up")
	sub, err := fs.Sub(rootpacks.FS, "follow-up")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.CopyFS(dst, sub); err != nil {
		t.Fatal(err)
	}
	y, err := os.ReadFile(filepath.Join(dst, "pack.yaml"))
	if err != nil || !strings.Contains(string(y), "schedule: 72h") || !strings.Contains(string(y), "mode: observe") {
		t.Fatalf("follow-up pack changed shape: %v\n%s", err, y)
	}
	addr, stop := start(t, home)
	defer stop()
	s := webLogin(t, addr)
	if err := os.WriteFile(filepath.Join(dst, "pack.yaml"), []byte(strings.Replace(string(y), "schedule: 72h", "schedule: 1s", 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	time.Sleep(packReloadInterval + clockTicks) // 等时钟循环热加载到新内容
	code, b := s.do("POST", "/items", `{"title":"等回复","kind":"task","phase":"open"}`)
	var it struct{ ID string }
	if json.Unmarshal([]byte(b), &it); code != 201 {
		t.Fatalf("create item: %d %s", code, b)
	}
	if code, b := s.do("POST", "/items/"+it.ID+"/transition", `{"to":"waiting","reason":"sent","waiting_on":{"kind":"human","ref":"someone"}}`); code >= 300 {
		t.Fatalf("transition: %d %s", code, b)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		code, b := s.do("GET", "/why/"+it.ID, "")
		if code == 200 && strings.Count(b, `"rule.observed"`) >= 2 { // 每 1s 一轮，observe 也照常再排下一轮
			if strings.Contains(b, "health.alert") || strings.Contains(b, "followup.reminded") {
				t.Fatalf("observe must not call the model or remind: %s", b)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("follow-up did not run on the pack's 1s interval: %d %s", code, b)
		}
		time.Sleep(200 * time.Millisecond)
	}
}
