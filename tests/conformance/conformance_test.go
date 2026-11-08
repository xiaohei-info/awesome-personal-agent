// Package conformance 脚本化对比设计文档与实现：任何漂移都让测试失败。
//
// 没有白名单或缺口台账：文档和实现不一致时，先改设计（docs/），再改代码（AGENTS.md 权威顺序）。
// 覆盖：`apa --help` 与附录 B.1、HTTP 路由与 B.2、事件类型与 B.4、退出码与 §17.3/B.3、内置包与 C.14。
package conformance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	rootpacks "github.com/xiaohei-info/awesome-personal-agent/packs"
)

// ---- 文档读取 ----

const root = "../.."

func doc(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "docs", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// between 取 from 标题之后、to 标题之前的文本；任一标题缺失即失败（文档结构变了，检查必须跟着改）。
func between(t *testing.T, s, from, to string) string {
	t.Helper()
	_, rest, ok := strings.Cut(s, from)
	if !ok {
		t.Fatalf("heading %q not found", from)
	}
	if to == "" {
		return rest
	}
	sec, _, ok := strings.Cut(rest, to)
	if !ok {
		t.Fatalf("heading %q not found", to)
	}
	return sec
}

// tableRows 返回 Markdown 表格各行的单元格（去掉首尾空格），跳过表头分隔行。
// 单元格内转义的竖线 `\|` 不是列分隔，还原为 `|`。
func tableRows(section string) [][]string {
	var out [][]string
	for _, line := range strings.Split(section, "\n") {
		if !strings.HasPrefix(line, "|") {
			continue
		}
		const esc = "\x00"
		cells := strings.Split(strings.Replace(strings.Trim(line, "|"), `\|`, esc, -1), "|")
		for i := range cells {
			cells[i] = strings.TrimSpace(strings.ReplaceAll(cells[i], esc, "|"))
		}
		if strings.Trim(strings.Join(cells, ""), "-: ") == "" {
			continue // 分隔行
		}
		out = append(out, cells)
	}
	return out
}

var span = regexp.MustCompile("`([^`]+)`")

func diff(t *testing.T, what string, docSet, implSet []string) {
	t.Helper()
	d, i := toSet(docSet), toSet(implSet)
	var onlyDoc, onlyImpl []string
	for k := range d {
		if !i[k] {
			onlyDoc = append(onlyDoc, k)
		}
	}
	for k := range i {
		if !d[k] {
			onlyImpl = append(onlyImpl, k)
		}
	}
	sort.Strings(onlyDoc)
	sort.Strings(onlyImpl)
	if len(onlyDoc) > 0 {
		t.Errorf("%s: documented but not implemented: %v", what, onlyDoc)
	}
	if len(onlyImpl) > 0 {
		t.Errorf("%s: implemented but not documented: %v", what, onlyImpl)
	}
}

func toSet(s []string) map[string]bool {
	m := make(map[string]bool, len(s))
	for _, v := range s {
		m[v] = true
	}
	return m
}

// ---- 真实二进制 ----

var (
	buildOnce sync.Once
	apaBin    string
	buildErr  error
)

func buildAPA(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		dir, err := os.MkdirTemp("", "apa-conformance-")
		if err != nil {
			buildErr = err
			return
		}
		apaBin = filepath.Join(dir, "apa")
		cmd := exec.Command("go", "build", "-o", apaBin, "./cli/apa")
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			buildErr = fmt.Errorf("go build ./cli/apa: %v\n%s", err, out)
		}
	})
	if buildErr != nil {
		t.Fatal(buildErr)
	}
	return apaBin
}

type run struct {
	code           int
	stdout, stderr string
}

func apa(t *testing.T, home, stdin string, args ...string) run {
	t.Helper()
	cmd := exec.Command(buildAPA(t), args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "APA_HOME=" + filepath.Join(home, "apa")}
	cmd.Stdin = strings.NewReader(stdin)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	r := run{stdout: o.String(), stderr: e.String()}
	var ee *exec.ExitError
	switch {
	case errors.As(err, &ee):
		r.code = ee.ExitCode()
	case err != nil:
		t.Fatal(err)
	}
	return r
}

// ---- B.1 CLI ----

var word = regexp.MustCompile(`^[a-z][a-z-]*([/|][a-z][a-z-]*)*$`)

// leaves 取一段命令文本开头的“词”序列（遇到参数、选项即停），把 a/b 或 a|b 展开成多条叶子路径。
// `item show/list [..]` → [item show, item list]；`project repos add|remove <id>` → [project repos add, project repos remove]。
func leaves(prefix []string, words []string) []string {
	var path []string
	path = append(path, prefix...)
	var alts []string
	for i, w := range words {
		if !word.MatchString(w) {
			break
		}
		parts := strings.FieldsFunc(w, func(r rune) bool { return r == '/' || r == '|' })
		if len(parts) > 1 && (i+1 >= len(words) || !word.MatchString(words[i+1])) {
			alts = parts
			break
		}
		path = append(path, parts[0])
	}
	if alts == nil {
		return []string{strings.Join(path, " ")}
	}
	var out []string
	for _, a := range alts {
		out = append(out, strings.Join(append(slices.Clone(path), a), " "))
	}
	return out
}

func docCommands(t *testing.T) []string {
	t.Helper()
	sec := between(t, doc(t, "appendix-b-api-cli.md"), "## B.1", "## B.2")
	var out []string
	for _, cells := range tableRows(sec) {
		if len(cells) < 4 {
			continue
		}
		m := span.FindStringSubmatch(cells[0])
		if m == nil {
			continue
		}
		f := strings.Fields(m[1])
		if f[0] != "apa" {
			t.Fatalf("B.1 command %q does not start with apa", m[1])
		}
		out = append(out, leaves(nil, f[1:])...)
	}
	if len(out) < 60 {
		t.Fatalf("parsed only %d commands from B.1; the table format changed", len(out))
	}
	return out
}

// implCommands 由真实二进制得出命令集：`apa --help` 给出顶层命令，
// 文档里带子命令的命令组再各跑一次无子命令调用，读它的 usage 行（只写机制的错误体 `next`）。
func implCommands(t *testing.T, docCmds []string) (top []string, all []string) {
	t.Helper()
	home := t.TempDir()
	r := apa(t, home, "", "--help")
	var body struct{ Next string }
	if err := json.Unmarshal([]byte(r.stderr), &body); err != nil {
		t.Fatalf("`apa --help` must print an error body listing commands: %q (%v)", r.stderr, err)
	}
	_, list, ok := strings.Cut(body.Next, "available commands: ")
	if !ok {
		t.Fatalf("`apa --help` does not list available commands: %q", body.Next)
	}
	top = strings.Split(list, ", ")
	all = slices.Clone(top)

	grouped := map[string]bool{}
	for _, c := range docCmds {
		if g, _, ok := strings.Cut(c, " "); ok {
			grouped[g] = true
		}
	}
	for _, g := range top {
		if !grouped[g] {
			continue
		}
		u := apa(t, home, "", g)
		var eb struct{ Next string }
		if err := json.Unmarshal([]byte(u.stderr), &eb); err != nil {
			t.Fatalf("`apa %s` without a subcommand must print a usage error body: %q", g, u.stderr)
		}
		usage, ok := strings.CutPrefix(eb.Next, "usage: apa "+g+" ")
		if !ok {
			t.Fatalf("`apa %s` usage does not start with `usage: apa %s `: %q", g, g, eb.Next)
		}
		// 子命令组用法里的 `|` 既分隔备选子命令，也出现在 `check|sync` 里；只按两侧带空格的 ` | ` 切备选。
		for _, alt := range strings.Split(usage, " | ") {
			all = append(all, leaves([]string{g}, strings.Fields(alt))...)
		}
		all = slices.DeleteFunc(all, func(c string) bool { return c == g })
	}
	return top, all
}

func TestCLIMatchesAppendixB1(t *testing.T) {
	docCmds := docCommands(t)
	_, impl := implCommands(t, docCmds)
	diff(t, "apa commands (B.1 vs `apa --help` and group usage)", docCmds, impl)
}

// ---- B.2 路由 ----

type route struct {
	methods []string
	path    string
}

// docRoutes 解析 B.2 表格第二列：`GET/POST /items`、接在前一条后面的 `/accept`、只有路径的设置组。
func docRoutes(t *testing.T) []route {
	t.Helper()
	sec := between(t, doc(t, "appendix-b-api-cli.md"), "## B.2", "## B.3")
	var out []route
	for _, cells := range tableRows(sec) {
		if len(cells) < 4 {
			continue
		}
		var prev *route
		for _, m := range span.FindAllStringSubmatch(cells[1], -1) {
			r := route{}
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
		t.Fatalf("parsed only %d routes from B.2; the table format changed", len(out))
	}
	return out
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

// startServe 在新目录上 `apa init` 再 `apa serve`（真实子进程），返回基地址；测试结束时停止进程。
func startServe(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if r := apa(t, home, "correct-horse\n", "init", "--password-stdin"); r.code != 0 {
		t.Fatalf("apa init: %+v", r)
	}
	addr := "127.0.0.1:" + strconv.Itoa(freePort(t))
	if err := os.WriteFile(filepath.Join(home, "apa", "config.yaml"), []byte("listen: "+addr+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(buildAPA(t), "serve")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "APA_HOME=" + filepath.Join(home, "apa")}
	var log bytes.Buffer
	cmd.Stdout, cmd.Stderr = &log, &log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	t.Cleanup(func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
	})
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			c.Close()
			return "http://" + addr
		}
		select {
		case <-done:
			t.Fatalf("apa serve exited early: %s", log.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatalf("apa serve not ready: %s", log.String())
	return ""
}

// mounted 用未登录的真实请求探测：骨干对未挂载的路由统一回 404 "no such route"；
// 已挂载的要么 401，要么进了处理器（处理器自己的 404 也算挂载）。
func mounted(t *testing.T, base, method, p string) bool {
	t.Helper()
	url := base + "/v1" + p
	if strings.HasPrefix(p, "/ingest/") || strings.HasPrefix(p, "/forms/") { // 外部系统与人点开的链接不带版本前缀
		url = base + p
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

// 只能证明“文档里的每条路由都已挂载”；反方向（实现多出的路由）HTTP 探测枚举不出，由 cli/serve 的装配测试守住。
func TestHTTPRoutesMatchAppendixB2(t *testing.T) {
	base := startServe(t)
	ph := regexp.MustCompile(`\{[^}]*\}`)
	for _, r := range docRoutes(t) {
		probe := ph.ReplaceAllString(r.path, "x")
		if len(r.methods) == 0 { // 表里只写了路径（设置组）：任一方法挂载都算
			got := false
			for _, m := range []string{"GET", "POST", "PUT", "PATCH", "DELETE"} {
				got = got || mounted(t, base, m, probe)
			}
			if !got {
				t.Errorf("%s is in appendix B.2 but not mounted by apa serve", r.path)
			}
			continue
		}
		for _, m := range r.methods {
			if !mounted(t, base, m, probe) {
				t.Errorf("%s %s is in appendix B.2 but not mounted by apa serve", m, r.path)
			}
		}
	}
}

// ---- B.4 事件类型 ----

func TestEventTypesMatchAppendixB4(t *testing.T) {
	sec := between(t, doc(t, "appendix-b-api-cli.md"), "## B.4", "")
	var docTypes []string
	for _, m := range span.FindAllStringSubmatch(sec, -1) {
		if strings.Contains(m[1], ".") && !strings.ContainsAny(m[1], " /") {
			docTypes = append(docTypes, m[1])
		}
	}
	if len(docTypes) < 20 {
		t.Fatalf("parsed only %d event types from B.4", len(docTypes))
	}
	diff(t, "event types (B.4 vs domain.RegisteredEventTypes)", docTypes, domain.RegisteredEventTypes)
	if !slices.Equal(docTypes, domain.RegisteredEventTypes) {
		t.Errorf("event type order differs from B.4:\n doc  %v\n impl %v", docTypes, domain.RegisteredEventTypes)
	}
}

// ---- §17.3 退出码 ----

func TestExitCodesMatchSection173(t *testing.T) {
	sec := between(t, doc(t, "solution-design.md"), "### 17.3 退出码", "Source 程序的退出码")
	docCodes := map[int]bool{}
	for _, cells := range tableRows(sec) {
		n, err := strconv.Atoi(cells[0])
		if err != nil {
			continue // 表头
		}
		docCodes[n] = true
	}
	// 实现侧：domain.Exit* 常量全集（从源码读，新增常量不会被漏掉）。
	src, err := os.ReadFile(filepath.Join(root, "kernel", "domain", "errors.go"))
	if err != nil {
		t.Fatal(err)
	}
	implCodes := map[int]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*Exit\w+\s*=\s*(\d+)`).FindAllStringSubmatch(string(src), -1) {
		n, _ := strconv.Atoi(m[1])
		implCodes[n] = true
	}
	var d, i []string
	for n := range docCodes {
		d = append(d, strconv.Itoa(n))
	}
	for n := range implCodes {
		i = append(i, strconv.Itoa(n))
	}
	diff(t, "exit codes (§17.3 vs domain.Exit*)", d, i)

	// B.3：HTTP 状态到退出码的对应，以及三个 403 子码。
	want := map[domain.Code][2]int{ // 码 → {HTTP, 退出码}
		domain.CodeInvalidRequest: {400, 2}, domain.CodeUnauthenticated: {401, 3},
		domain.CodePermissionDenied: {403, 4}, domain.CodeWebOnly: {403, 20},
		domain.CodeExecutorNotAllowed: {403, 21}, domain.CodeRiskScreeningPending: {403, 22},
		domain.CodeNotFound: {404, 5}, domain.CodeConflict: {409, 6}, domain.CodeInternal: {500, 1},
	}
	b3 := between(t, doc(t, "appendix-b-api-cli.md"), "## B.3", "## B.4")
	for _, frag := range []string{"400 → 2", "401 → 3", "404 → 5", "409 → 6", "不可达时 CLI 返回 7", "403 → 4、20、21、22"} {
		if !strings.Contains(b3, frag) {
			t.Errorf("B.3 no longer says %q; update this check together with the doc", frag)
		}
	}
	for c, w := range want {
		if c.HTTPStatus() != w[0] || c.ExitCode() != w[1] {
			t.Errorf("code %s: implementation maps to HTTP %d / exit %d, B.3 says %d / %d", c, c.HTTPStatus(), c.ExitCode(), w[0], w[1])
		}
	}
	if domain.ExitUnreachable != 7 {
		t.Errorf("unreachable must exit 7")
	}
}

// 退出码 2、3、7 能在真实二进制上观察到：用法错误、未认证、内核不可达。
func TestBinaryExitsWithDocumentedCodes(t *testing.T) {
	home := t.TempDir()
	if r := apa(t, home, "", "--help"); r.code != 2 {
		t.Errorf("unknown command must exit 2, got %d", r.code)
	}
	if r := apa(t, home, "", "health"); r.code != 3 {
		t.Errorf("no credential must exit 3, got %d: %s", r.code, r.stderr)
	}
	cmd := exec.Command(buildAPA(t), "health")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + home, "APA_HOME=" + filepath.Join(home, "apa"),
		"APA_URL=http://127.0.0.1:" + strconv.Itoa(freePort(t)), "APA_TOKEN=x"}
	err := cmd.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != 7 {
		t.Errorf("unreachable kernel must exit 7, got %v", err)
	}
}

// ---- C.14 内置包 ----

// 每个“声明的内容”短语对应包里一个可检查的事实。
type fact struct {
	phrase string // 多个说法用 | 分隔，C.14 出现任一即算声明
	has    func(p packFacts) bool
}

type packFacts struct {
	manifest map[string]any
	dirs     map[string]bool // prompts、views、templates 是否有文件
}

func nonEmpty(key string) func(packFacts) bool {
	return func(p packFacts) bool {
		v, ok := p.manifest[key]
		if !ok {
			return false
		}
		switch x := v.(type) {
		case []any:
			return len(x) > 0
		case map[string]any:
			return len(x) > 0
		}
		return v != nil
	}
}

func dir(name string) func(packFacts) bool { return func(p packFacts) bool { return p.dirs[name] } }

var facts = []fact{
	{"实例参数", nonEmpty("instance_params")},
	{"时钟规则", nonEmpty("clock_rules")},
	{"事件规则|规则样例", nonEmpty("event_rules")},
	{"来源规则", nonEmpty("source_rules")},
	{"webhook 校验方案", nonEmpty("webhook_schemes")},
	{"字段映射", nonEmpty("webhook_mappings")},
	{"提示词", dir("prompts")},
	{"视图", dir("views")},
	{"模板", dir("templates")},
}

func TestBuiltinPacksMatchC14(t *testing.T) {
	sec := between(t, doc(t, "appendix-c-capability-pack.md"), "## C.14", "")
	docPacks := map[string]string{}
	var names []string
	for _, cells := range tableRows(sec) {
		m := span.FindStringSubmatch(cells[0])
		if m == nil || len(cells) < 2 {
			continue
		}
		docPacks[m[1]] = cells[1]
		names = append(names, m[1])
	}
	entries, err := fs.ReadDir(rootpacks.FS, ".")
	if err != nil {
		t.Fatal(err)
	}
	var impl []string
	for _, e := range entries {
		if e.IsDir() {
			impl = append(impl, e.Name())
		}
	}
	diff(t, "builtin packs (C.14 vs packs/)", names, impl)

	for _, name := range names {
		raw, err := fs.ReadFile(rootpacks.FS, name+"/pack.yaml")
		if err != nil {
			t.Errorf("pack %s: %v", name, err)
			continue
		}
		var man map[string]any
		if err := yaml.Unmarshal(raw, &man); err != nil {
			t.Errorf("pack %s: pack.yaml: %v", name, err)
			continue
		}
		if man["name"] != name {
			t.Errorf("pack %s: pack.yaml name is %v", name, man["name"])
		}
		pf := packFacts{manifest: man, dirs: map[string]bool{}}
		for _, d := range []string{"prompts", "views", "templates"} {
			if es, err := fs.ReadDir(rootpacks.FS, name+"/"+d); err == nil && len(es) > 0 {
				pf.dirs[d] = true
			}
		}
		text := docPacks[name]
		for _, f := range facts {
			says, has := slices.ContainsFunc(strings.Split(f.phrase, "|"), func(p string) bool { return strings.Contains(text, p) }), f.has(pf)
			switch {
			case says && !has:
				t.Errorf("pack %s: C.14 declares %q but the pack has none", name, f.phrase)
			case !says && has:
				t.Errorf("pack %s: pack has %q but C.14 does not list it", name, f.phrase)
			}
		}
		// kind `x`：文档点名的 kind 必须在包里声明。
		for _, m := range regexp.MustCompile("kind `([^`]+)`").FindAllStringSubmatch(text, -1) {
			if !slices.Contains(kindNames(man), m[1]) {
				t.Errorf("pack %s: C.14 declares kind %q, pack declares %v", name, m[1], kindNames(man))
			}
		}
	}
}

func kindNames(man map[string]any) []string {
	var out []string
	ks, _ := man["kinds"].([]any)
	for _, k := range ks {
		if m, ok := k.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				out = append(out, n)
			}
		}
	}
	return out
}
