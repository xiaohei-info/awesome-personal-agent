//go:build e2e

package release

// flow-release 的全流程验收（验收文档 ACT-REL-00..12）：用发布脚本（tools/release）在当前提交上产出发布包，
// 然后在"干净主机"里只用发布包里的文件运行。干净主机 = 空 PATH（没有 go、node、npm、任何工具）+ 空的用户目录 + 新的 APA_HOME，
// 不依赖编译工具链；一次性容器与它等价，这里用空用户目录模拟（任务约定）。
// ACT-REL-03、ACT-REL-04、ACT-REL-05 是同一命令下的独立用例（TestDocsMatchImplementation、
// TestGenericCapabilitiesWithoutHarness、TestRedlineOnReleaseCommit）。

import (
	"bufio"
	"debug/buildinfo"
	"debug/elf"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const releaseAtom = `<?xml version="1.0"?><feed xmlns="http://www.w3.org/2005/Atom"><title>t</title><id>urn:feed</id><updated>2030-01-01T00:00:00Z</updated>` +
	`<entry><id>a</id><title>release-flow</title><link href="http://x/a"/><updated>2030-01-01T00:00:00Z</updated></entry></feed>`

// ACT-REL-03：文档和实现一致（对照测试：附录 B.1/B.2/B.4、退出码、内置包）。
func TestDocsMatchImplementation(t *testing.T) {
	root := repoRoot(t)
	out, code := run(t, root, os.Environ(), "go", "test", "-count=1", "./tests/conformance/", "./cli/serve/")
	if code != 0 {
		t.Fatalf("文档与实现的对照测试未通过（退出码 %d）:\n%s", code, tail(out, 4000))
	}
	for _, pkg := range []string{"tests/conformance", "cli/serve"} {
		if !strings.Contains(out, "ok  \tgithub.com/xiaohei-info/awesome-personal-agent/"+pkg) {
			t.Errorf("%s 没有通过的记录:\n%s", pkg, tail(out, 2000))
		}
	}
}

func lookGo(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestFlowRelease(t *testing.T) {
	outer := t // 干净主机的目录与环境的生命周期属于整个流程，而不是某一步
	root := repoRoot(t)
	goBin := lookGo(t)
	sha256sum, err := exec.LookPath("sha256sum")
	if err != nil {
		t.Fatal(err)
	}
	step := func(name string, fn func(t *testing.T)) {
		if !t.Run(name, fn) {
			t.FailNow()
		}
	}

	var pkg string
	var cleanPath, userHome string
	var e *baseline.Env
	var fx *baseline.Fixture
	var headCommit string

	step("ACT-REL-00 准备干净主机与发布包", func(t *testing.T) {
		out := outer.TempDir()
		cmd := exec.Command(goBin, "run", "./tools/release", "-pack", out, "-skip-web", "-no-verify")
		cmd.Dir = root
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("发布脚本失败: %v\n%s", err, b)
		}
		m, _ := filepath.Glob(filepath.Join(out, "apa-*"))
		for _, p := range m {
			if fi, err := os.Stat(p); err == nil && fi.IsDir() {
				pkg = p
			}
		}
		if pkg == "" {
			t.Fatalf("发布包目录不存在: %v", m)
		}
		cleanPath = outer.TempDir() // 空目录作 PATH
		userHome = outer.TempDir()  // 空用户目录
		b, _ := exec.Command("/bin/sh", "-c", "command -v go node npm").Output()
		cmd = exec.Command("/bin/sh", "-c", "PATH="+cleanPath+" command -v go node npm")
		cmd.Env = []string{"PATH=" + cleanPath, "HOME=" + userHome}
		b, _ = cmd.Output()
		if strings.TrimSpace(string(b)) != "" {
			t.Fatalf("干净主机上不应有 Go、Node 工具链: %s", b)
		}
		head, err := exec.Command("git", "-C", root, "rev-parse", "HEAD").Output()
		if err != nil {
			t.Fatal(err)
		}
		headCommit = strings.TrimSpace(string(head))
		e = baseline.New(outer) // 测试构建的二进制与夹具工具在此之前构建；之后的一切都在干净环境里
	})

	step("ACT-ENV-08 夹具副本", func(t *testing.T) { fx = e.LedgerFixture(2) }) // 夹具用 cp，要在干净环境之前取

	apa := func() string { return filepath.Join(pkg, "bin", "apa") }

	// 从这里起整个进程环境就是干净主机：新起的 apa 进程、来源程序、ext 都继承它。
	cleanHost := func(t *testing.T) {
		t.Setenv("PATH", cleanPath)
		t.Setenv("HOME", userHome)
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(userHome, ".config"))
		t.Setenv("GOFLAGS", "")
	}

	step("干净主机", func(t *testing.T) {
		cleanHost(t)
		e.Bin["apa"] = apa() // 其余替身二进制不动；apa 换成发布包里的

		t.Run("ACT-REL-01 干净主机只用发布包装全并启动", func(t *testing.T) {
			bi, err := buildinfo.ReadFile(apa())
			if err != nil {
				t.Fatal(err)
			}
			cgo := ""
			for _, s := range bi.Settings {
				if s.Key == "CGO_ENABLED" {
					cgo = s.Value
				}
			}
			f, err := elf.Open(apa())
			if err != nil {
				t.Fatal(err)
			}
			libs, _ := f.ImportedLibraries()
			interp := f.Section(".interp")
			f.Close()
			if cgo != "0" || len(libs) > 0 || interp != nil {
				t.Errorf("主程序应是单个静态二进制: CGO_ENABLED=%q 动态库=%v interp=%v", cgo, libs, interp != nil)
			}
			e.StartKernel() // apa init（口令 12 个字符以上）+ apa serve，均为发布包的 apa
			resp, err := http.Get(e.URL + "/")
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 || !strings.Contains(string(body), `id="root"`) {
				t.Errorf("嵌入的 Web 登录页打不开: HTTP %d %.200s", resp.StatusCode, body)
			}
			if w := e.Web("GET", "/v1/packs", ""); w.Code != 200 || !strings.Contains(w.Body, "inbound-triage") {
				t.Errorf("内置能力包没有随二进制加载: %d %.200s", w.Code, w.Body)
			}
		})

		t.Run("ACT-REL-06 发布包内容齐全，哈希清单可校验", func(t *testing.T) {
			must := []string{"bin/apa", "sources/rss-source", "sources/mail-himalaya", "sources/calendar", "sources/github-source", // 来源程序与工具适配器
				"ext/telegram/ext.yaml", "ext/export/ext.yaml", "ext/webhookforward/ext.yaml", // 渠道 ext、导出 ext、webhook 转发 ext
				"plugins/hermes/core/plugin.yaml", "plugins/hermes/core/README.md", // 首批宿主插件包与安装说明
				"deploy/systemd/apa.service", "deploy/nginx/apa.conf.example", "deploy/caddy/Caddyfile.example", // 系统服务单元与反向代理样例
				"SHA256SUMS", "docs/user/quickstart.md", "docs/user/run-tiers.md", "docs/user/troubleshooting.md", "docs/user/security.md",
				"docs/user/upgrade.md", "docs/user/uninstall.md", "docs/user/changelog.md"}
			for _, p := range must {
				if _, err := os.Stat(filepath.Join(pkg, p)); err != nil {
					t.Errorf("发布包缺 %s", p)
				}
			}
			// §16.6："反向代理与隧道的配置样例"：除反向代理外还要有隧道样例。
			if !hasTunnelSample(pkg) {
				t.Errorf("发布包 deploy/ 里没有隧道（tunnel）的配置样例（设计 §16.6 部署样例一行）")
			}
			verify := func() (string, int) {
				cmd := exec.Command(sha256sum, "-c", "SHA256SUMS")
				cmd.Dir = pkg
				b, err := cmd.CombinedOutput()
				if ee, ok := err.(*exec.ExitError); ok {
					return string(b), ee.ExitCode()
				}
				return string(b), 0
			}
			if out, code := verify(); code != 0 {
				t.Fatalf("未改动时哈希清单应全部通过（退出码 %d）:\n%s", code, out)
			}
			target := filepath.Join(pkg, "docs", "user", "quickstart.md")
			orig, err := os.ReadFile(target)
			if err != nil {
				t.Fatal(err)
			}
			defer os.WriteFile(target, orig, 0o644)
			flipped := append([]byte{}, orig...)
			flipped[0] ^= 0x01 // 改动一个字节
			if err := os.WriteFile(target, flipped, 0o644); err != nil {
				t.Fatal(err)
			}
			out, code := verify()
			if code == 0 || !strings.Contains(out, "docs/user/quickstart.md: FAILED") {
				t.Errorf("改动一个字节后该文件应校验失败（退出码 %d）:\n%s", code, out)
			}
			if err := os.WriteFile(target, orig, 0o644); err != nil {
				t.Fatal(err)
			}
			if out, code := verify(); code != 0 {
				t.Errorf("恢复后应重新通过:\n%s", out)
			}
		})

		t.Run("ACT-REL-07 预编译的来源程序与 ext 在干净主机上直接可用", func(t *testing.T) {
			feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/atom+xml")
				_, _ = io.WriteString(w, releaseAtom)
			}))
			defer feed.Close()
			body, _ := json.Marshal(map[string]any{"id": "src_rel_rss", "type": "script", "command": filepath.Join(pkg, "sources", "rss-source"),
				"schedule": "1s", "timeout": 20, "source_type": "rss", "config": map[string]string{"url": feed.URL}})
			if w := e.Web("POST", "/v1/sources", string(body)); w.Code != 201 {
				t.Fatalf("登记包内 RSS 来源: %d %s", w.Code, w.Body)
			}
			if !e.WaitFor(30*time.Second, func() bool {
				w := e.Web("GET", "/v1/sources/src_rel_rss/events", "")
				var out struct {
					Events []json.RawMessage `json:"events"`
				}
				return w.Code == 200 && json.Unmarshal([]byte(w.Body), &out) == nil && len(out.Events) > 0
			}) {
				t.Fatalf("来源页没有出现新事件\n%s", e.ServiceLog())
			}
			w := e.Web("POST", "/v1/settings/ext-keys", `{"label":"REL","scope":"export"}`)
			var key struct {
				Secret string `json:"secret"`
			}
			if w.Code != 201 || json.Unmarshal([]byte(w.Body), &key) != nil || key.Secret == "" {
				t.Fatalf("签发导出 ext 密钥: %d %s", w.Code, w.Body)
			}
			exportDir := t.TempDir()
			cmd := exec.Command(apa(), "ext", "run", "export")
			cmd.Dir = pkg
			var log strings.Builder
			cmd.Stdout, cmd.Stderr = &log, &log
			cmd.Env = append(os.Environ(), "APA_URL="+e.URL, "APA_EXT_KEY="+key.Secret, "APA_EXPORT_DIR="+exportDir)
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			defer func() {
				_ = cmd.Process.Signal(os.Interrupt)
				select {
				case <-done:
				case <-time.After(10 * time.Second):
					_ = cmd.Process.Kill()
					<-done
				}
			}()
			if !e.WaitFor(30*time.Second, func() bool {
				ents, _ := os.ReadDir(exportDir)
				return len(ents) > 0
			}) {
				t.Fatalf("导出 ext 没有写出文件: %s", log.String())
			}
		})

		var cliVersion []byte
		var apiVersion map[string]any

		t.Run("ACT-REL-08 apa version 与 GET /version 返回同一份版本信息", func(t *testing.T) {
			cmd := exec.Command(apa(), "version")
			cmd.Env = append(os.Environ(), "APA_HOME="+e.Home, "APA_URL="+e.URL)
			var err error
			if cliVersion, err = cmd.Output(); err != nil {
				t.Fatal(err)
			}
			resp, err := http.Get(e.URL + "/v1/version") // 不带任何凭据
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			if resp.StatusCode != 200 {
				t.Fatalf("GET /v1/version 不带凭据应 200，实际 %d", resp.StatusCode)
			}
			var cli map[string]any
			if json.Unmarshal(cliVersion, &cli) != nil || json.Unmarshal(raw, &apiVersion) != nil || !reflect.DeepEqual(cli, apiVersion) {
				t.Fatalf("两者内容应相同:\ncli=%s\napi=%s", cliVersion, raw)
			}
			if apiVersion["version"] != "0.2.0" || apiVersion["commit"] != headCommit {
				t.Errorf("version/commit 不符（发布提交 %s）: %v", headCommit, apiVersion)
			}
			proto, _ := apiVersion["protocols"].(map[string]any)
			schema, _ := apiVersion["schema"].(map[string]any)
			if schema["ledger"] == nil || proto["http_api"] == nil || proto["source_contract"] == nil || proto["channel_ext"] == nil ||
				proto["probability_judgment"] != "probability-judgment/1" {
				t.Errorf("版本信息缺字段: %v", apiVersion)
			}
		})

		t.Run("ACT-REL-09 schema.ledger 等于发布提交上最新的账本迁移版本", func(t *testing.T) {
			maxV := 0
			re := regexp.MustCompile(`\bLedger:\s*(\d+)`)
			_ = filepath.WalkDir(filepath.Join(root, "kernel"), func(p string, d os.DirEntry, err error) error {
				if err != nil || d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
					return nil
				}
				b, _ := os.ReadFile(p)
				for _, m := range re.FindAllStringSubmatch(string(b), -1) {
					if v, _ := strconv.Atoi(m[1]); v > maxV {
						maxV = v
					}
				}
				return nil
			})
			schema, _ := apiVersion["schema"].(map[string]any)
			cmd := exec.Command(apa(), "doctor", "--json")
			cmd.Env = append(os.Environ(), "APA_HOME="+e.Home, "APA_URL="+e.URL)
			out, _ := cmd.Output()
			var rep docReport
			if err := json.Unmarshal(out, &rep); err != nil {
				t.Fatalf("apa doctor --json: %v %s", err, out)
			}
			applied := rep.check(t, "data_dir").Data["schema_ledger"]
			if maxV < 3 || schema["ledger"] != float64(maxV) || applied != float64(maxV) {
				t.Errorf("三个值应相同且不小于 3: 迁移文件最大 %d，schema.ledger %v，新数据目录已应用 %v", maxV, schema["ledger"], applied)
			}
		})

		t.Run("ACT-REL-10 用户文档齐全", func(t *testing.T) {
			TestUserDocsComplete(t)
			TestRunTiersMatchDesign(t)
			TestUpgradeDocMatchesDesign(t)
			for _, f := range []string{"quickstart.md", "run-tiers.md", "troubleshooting.md", "security.md", "upgrade.md", "uninstall.md", "changelog.md"} {
				a, _ := os.ReadFile(filepath.Join(pkg, "docs", "user", f))
				b, _ := os.ReadFile(filepath.Join(root, "docs", "user", f))
				if len(a) == 0 || string(a) != string(b) {
					t.Errorf("发布包里的 docs/user/%s 与仓库不一致", f)
				}
			}
		})

		t.Run("ACT-REL-11 变更记录写明界面语言限制与需迁移的变更", func(t *testing.T) {
			TestChangelogLanguageAndMigrations(t)
		})

		t.Run("ACT-REL-12 发布物不含测试时钟与迁移失败注入入口", func(t *testing.T) {
			// REL_OBS：本人在 Web 设置页签发的 observer token（环境基线的 IssueCredentials 同一接口）。
			w := e.Web("POST", "/v1/settings/tokens", `{"kind":"observer","label":"REL_OBS"}`)
			var tok struct {
				Secret string `json:"secret"`
			}
			if w.Code != 201 || json.Unmarshal([]byte(w.Body), &tok) != nil || tok.Secret == "" {
				t.Fatalf("签发 observer token: %d %s", w.Code, w.Body)
			}
			if apiVersion["test_clock"] != false {
				t.Errorf("GET /v1/version 的 test_clock 应为 false: %v", apiVersion)
			}
			var cli map[string]any
			_ = json.Unmarshal(cliVersion, &cli)
			if cli["test_clock"] != false {
				t.Errorf("apa version 的 test_clock 应为 false: %v", cli)
			}
			today := func() string {
				r := e.Apa(tok.Secret, "", "today")
				if r.Code != 0 {
					t.Fatalf("apa today: %d %s", r.Code, r.Stderr)
				}
				return r.Stdout
			}
			before := today()
			if err := os.WriteFile(filepath.Join(e.Home, "test-clock-offset"), []byte("86400\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			time.Sleep(10 * time.Second)
			if after := today(); after != before {
				t.Errorf("写偏移量文件后 apa today 的日期不应变化:\nbefore=%s\nafter=%s", before, after)
			}
			logs, _ := filepath.Glob(filepath.Join(e.Home, "logs", "*"))
			for _, p := range logs {
				if b, _ := os.ReadFile(p); strings.Contains(string(b), "test-clock offset") {
					t.Errorf("%s 里出现 test-clock offset", p)
				}
			}
			if strings.Contains(e.ServiceLog(), "test-clock offset") {
				t.Errorf("服务日志里出现 test-clock offset")
			}

			// 夹具副本（ACT-ENV-08）：带 APA_TEST_MIGRATION_FAIL_AFTER=1 启动，迁移必须正常完成。
			t.Setenv("APA_TEST_MIGRATION_FAIL_AFTER", "1")
			e.ServeFixture(fx, nil)
			if log := e.ServiceLog(); strings.Contains(log, "migration fault injected") {
				t.Errorf("发布物不应有迁移失败注入入口:\n%s", tail(log, 2000))
			}
			if v := e.APIRaw("GET", "/v1/version", "", ""); v.Code != 200 || !strings.Contains(v.Body, `"ledger":3`) {
				t.Errorf("夹具迁移应正常完成并升到结构版本 3: %d %s", v.Code, v.Body)
			}
			e.StopKernel()

			// go version -m：构建信息的 -tags 里不含 apa_testclock。
			cmd := exec.Command(goBin, "version", "-m", apa())
			cmd.Env = append(os.Environ(), "PATH="+filepath.Dir(goBin)+string(os.PathListSeparator)+cleanPath)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("go version -m: %v %s", err, out)
			}
			if strings.Contains(string(out), "apa_testclock") || !strings.Contains(string(out), "build\t") {
				t.Errorf("构建信息不应含 apa_testclock:\n%s", out)
			}
		})
	})
}

// hasTunnelSample 判断 deploy/ 下是否有隧道（cloudflared、tailscale funnel/serve 等）的配置样例。
func hasTunnelSample(pkg string) bool {
	found := false
	re := regexp.MustCompile(`(?i)tunnel|cloudflared|tailscale|ngrok`)
	_ = filepath.WalkDir(filepath.Join(pkg, "deploy"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if re.MatchString(filepath.Base(filepath.Dir(p))) || re.MatchString(filepath.Base(p)) {
			found = true
			return nil
		}
		f, err := os.Open(p)
		if err != nil {
			return nil
		}
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if !strings.HasPrefix(strings.TrimSpace(sc.Text()), "#") && re.MatchString(sc.Text()) {
				found = true
			}
		}
		return nil
	})
	return found
}

var _ = fmt.Sprintf
