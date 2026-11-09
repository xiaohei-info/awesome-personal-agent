//go:build live

package guideline

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

const (
	harness     = "hermes"
	defaultRepo = "https://github.com/xiaohei-info/apa-live-test.git"
)

var tokenRE = regexp.MustCompile(`apa_[A-Za-z0-9_]{12,}`)

type fx struct {
	*baseline.Env
	t        *testing.T
	hermesBN string
	hhome    string
	wrapper  string
	evidence map[string]any
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func repoRoot() string {
	wd, _ := os.Getwd()
	return filepath.Clean(filepath.Join(wd, "..", "..", ".."))
}

// setup 建环境：init、serve（配置本机起草受托的执行者）、凭据与项目、模型端点（MODEL_STUB）、真实 Hermes 的声明与默认 harness。
func setup(t *testing.T) *fx {
	t.Helper()
	t.Setenv("APA_E2E_TEST_REPO", envOr("APA_E2E_TEST_REPO", defaultRepo))
	e := baseline.New(t)
	e.Init()
	// 缺规范检测周期改为 1 分钟：数据目录下的同名包整体覆盖内置包（附录 C.1）。
	pk := filepath.Join(e.Home, "packs", "guideline-draft")
	must(t, os.MkdirAll(filepath.Join(pk, "prompts"), 0o755))
	yml := readFile(t, filepath.Join(repoRoot(), "packs", "guideline-draft", "pack.yaml"))
	if !strings.Contains(yml, "schedule: 24h") {
		t.Fatal("guideline-draft 内置包的检测周期不是 24h，覆盖包需要同步")
	}
	must(t, os.WriteFile(filepath.Join(pk, "pack.yaml"), []byte(strings.Replace(yml, "schedule: 24h", "schedule: 1m", 1)), 0o644))
	must(t, os.WriteFile(filepath.Join(pk, "prompts", "draft-guideline.md"), []byte(readFile(t, filepath.Join(repoRoot(), "packs", "guideline-draft", "prompts", "draft-guideline.md"))), 0o644))
	e.ServeWith("guideline_draft_executor: work-host.cli\n")
	e.IssueCredentials()
	e.CreateProjects()
	e.StartModelStub()
	f := &fx{Env: e, t: t, evidence: map[string]any{}}
	f.prepareHermes()
	f.declareHermes()
	f.webOK("PUT", "/settings/policy", `{"default_harness":"`+harness+`"}`, 200)
	// 起草是任务型，read 档（模板声明）；上限放到 edit 以便主会话与起草都不被压低。
	f.webOK("PUT", "/settings/access-limits", `{"permission_cap":{"default":"edit","by_harness":{}}}`, 200)
	f.webOK("PUT", "/settings/policy", `{"web_only":{"categories":["external_commitment","funds","irreversible","permission_change","spec_activation"],"rules":[{"tag":"acc-finance","category":"funds"}]}}`, 200)
	return f
}

func (f *fx) prepareHermes() {
	f.hermesBN = realHermes(f.t)
	f.hhome = f.newHermesHome("headless")
	f.wrapper = filepath.Join(f.Root, "hermes-run.sh")
	script := fmt.Sprintf(`#!/bin/sh
export HERMES_HOME=%q
export PATH=%q:"$PATH"
export APA_BIN=%q
printf '%%s\n' "$@" > args.txt
pwd -P > cwd.txt
# 留下真实 Hermes 的 stdout/stderr 供排查，原样转交给内核。
%q "$@" >stdout.txt 2>stderr.txt
rc=$?
cat stdout.txt
cat stderr.txt >&2
exit $rc
`, f.hhome, filepath.Dir(f.Bin["apa"]), f.Bin["apa"], f.hermesBN)
	must(f.t, os.WriteFile(f.wrapper, []byte(script), 0o755))
}

// newHermesHome 建全新的隔离 HERMES_HOME：只复制种子的 config.yaml 与 .env，挂上 plugins/hermes 的四个插件并启用。
func (f *fx) newHermesHome(name string) string {
	t := f.t
	seed := envOr("APA_LIVE_HERMES_SEED", "/opt/apa-live/hermes-home")
	// 目录名要短：Hermes 在家目录下建 unix socket，路径超过 100 字节会以 invalid_params 失败。
	home := filepath.Join(f.Root, "h-"+name)
	must(t, os.MkdirAll(filepath.Join(home, "plugins"), 0o700))
	for _, n := range []string{"config.yaml", ".env"} {
		b, err := os.ReadFile(filepath.Join(seed, n))
		if err != nil {
			t.Fatalf("读取 Hermes 种子 %s：%v", n, err)
		}
		if n == "config.yaml" {
			if strings.Contains("\n"+string(b), "\nplugins:") {
				t.Fatal("种子 config.yaml 已有 plugins 段，请改为无插件的全新种子")
			}
			enabled := "\nplugins:\n  enabled:\n    - apa-core\n    - apa-tools\n    - apa-headless\n"
			if strings.HasPrefix(name, "main-") { // 主会话才有会话开始的 check 注入；无头运行不挂 inject
				enabled += "    - apa-inject\n"
			}
			b = append(b, []byte(enabled)...)
		}
		must(t, os.WriteFile(filepath.Join(home, n), b, 0o600))
	}
	for link, dir := range map[string]string{"apa-core": "core", "apa-tools": "tools", "apa-headless": "headless", "apa-inject": "inject"} {
		if dir == "inject" && !strings.HasPrefix(name, "main-") {
			continue
		}
		must(t, os.Symlink(filepath.Join(repoRoot(), "plugins", "hermes", dir), filepath.Join(home, "plugins", link)))
	}
	return home
}

func (f *fx) declareHermes() {
	t := f.t
	raw, err := os.ReadFile(filepath.Join(repoRoot(), "plugins", "hermes", "headless", "declaration.json"))
	must(t, err)
	var decl struct {
		Harness  string         `json:"harness"`
		Headless map[string]any `json:"headless"`
	}
	must(t, json.Unmarshal(raw, &decl))
	cmd := decl.Headless["command"].([]any)
	decl.Headless["command"] = append([]any{f.wrapper}, cmd[1:]...)
	b, _ := json.Marshal(map[string]any{"harness": harness, "headless": decl.Headless})
	if r := f.Apa(f.DEL, string(b), "declare", "-", "--evidence", "真实 Hermes 的无头运行声明"); r.Code != 0 {
		t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/"+harness+"/1/confirm", "", 200)
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

// apaIn 在目录 dir 里运行 apa（agents 命令按当前检出的 origin 找仓库），返回退出码与输出。
func (f *fx) apaIn(dir, token string, args ...string) baseline.Result {
	f.t.Helper()
	cmd := exec.Command(f.Bin["apa"], args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "APA_HOME="+f.Home, "APA_URL="+f.URL, "APA_TOKEN="+token)
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	r := baseline.Result{}
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		f.t.Fatalf("运行 apa %v: %v", args, err)
	}
	r.Stdout, r.Stderr = o.String(), e.String()
	return r
}

func (f *fx) wantIn(dir, token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.apaIn(dir, token, args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func jsonOf[T any](t testing.TB, s string) (v T) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%.400s", err, s)
	}
	return v
}

func idOf(t *testing.T, stdout string) string {
	t.Helper()
	m := jsonOf[struct {
		ID string `json:"id"`
	}](t, stdout)
	if m.ID == "" {
		t.Fatalf("没有返回 id: %q", stdout)
	}
	return m.ID
}

type event struct {
	Seq     int64          `json:"seq"`
	Type    string         `json:"type"`
	Object  string         `json:"object"`
	Reason  string         `json:"reason"`
	Payload map[string]any `json:"payload"`
}

func (f *fx) events(object, typ string) []event {
	f.t.Helper()
	path := "/audit/events?limit=500"
	if object != "" {
		path += "&object=" + url.QueryEscape(object)
	}
	if typ != "" {
		path += "&type=" + url.QueryEscape(typ)
	}
	return jsonOf[struct {
		Events []event `json:"events"`
	}](f.t, f.webOK("GET", path, "", 200)).Events
}

func (f *fx) item(id string) map[string]any {
	f.t.Helper()
	return jsonOf[struct {
		Item map[string]any `json:"item"`
	}](f.t, f.apa(f.OBS, 0, "item", "show", id).Stdout).Item
}

func str(m map[string]any, k string) string { s, _ := m[k].(string); return s }

type material struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Project    string `json:"project"`
	CurrentRev string `json:"current_rev"`
}

type revision struct {
	ID      string `json:"id"`
	State   string `json:"state"`
	Content string `json:"content"`
}

func (f *fx) guidelines(project string) []material {
	f.t.Helper()
	var out []material
	for _, m := range jsonOf[struct {
		Materials []material `json:"materials"`
	}](f.t, f.webOK("GET", "/materials?project="+project, "", 200)).Materials {
		if m.Kind == "guideline" {
			out = append(out, m)
		}
	}
	return out
}

func (f *fx) revisions(material string) []revision {
	f.t.Helper()
	return jsonOf[struct {
		Revisions []revision `json:"revisions"`
	}](f.t, f.webOK("GET", "/materials/"+material+"/revisions", "", 200)).Revisions
}

// proposedRev 返回材料上唯一的 proposed 修订 id。
func (f *fx) proposedRev(material string) string {
	f.t.Helper()
	var id string
	for _, r := range f.revisions(material) {
		if r.State == "proposed" {
			if id != "" {
				f.t.Fatalf("材料 %s 有多于一条 proposed 修订", material)
			}
			id = r.ID
		}
	}
	if id == "" {
		f.t.Fatalf("材料 %s 没有 proposed 修订", material)
	}
	return id
}

func (f *fx) waitFor(what string, d time.Duration, cond func() bool) {
	f.t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			f.t.Fatalf("等待超时（%v）：%s", d, what)
		}
		time.Sleep(time.Second)
	}
}

func (f *fx) redact(s string) string {
	for _, tok := range []string{f.OBS, f.WRK, f.WRK2, f.DEL, f.CHAN} {
		if tok != "" {
			s = strings.ReplaceAll(s, tok, "<redacted>")
		}
	}
	return tokenRE.ReplaceAllString(s, "<redacted>")
}

func (f *fx) saveEvidence(name, content string) {
	dir := os.Getenv("APA_LIVE_EVIDENCE_DIR")
	if dir == "" {
		return
	}
	must(f.t, os.MkdirAll(dir, 0o755))
	must(f.t, os.WriteFile(filepath.Join(dir, name), []byte(f.redact(content)), 0o644))
}

func (f *fx) flushEvidence(name string) {
	b, _ := json.MarshalIndent(f.evidence, "", "  ")
	f.saveEvidence(name, string(b)+"\n")
}

func eq(t *testing.T, what string, got, want any) {
	t.Helper()
	if got != want {
		t.Errorf("%s = %v，期望 %v", what, got, want)
	}
}

func has(t *testing.T, what, s, sub string) {
	t.Helper()
	if !strings.Contains(s, sub) {
		t.Errorf("%s 应含 %q，实际：\n%.600s", what, sub, s)
	}
}

// ---- git ----

// gitEnv 让 git 用本机已登录的 gh 作凭据助手访问 GitHub 专用测试仓库（不改任何 git 配置文件）。
func gitEnv() []string {
	return append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_COUNT=1", "GIT_CONFIG_KEY_0=credential.helper",
		"GIT_CONFIG_VALUE_0=!gh auth git-credential", "GIT_AUTHOR_NAME=apa-live", "GIT_AUTHOR_EMAIL=apa-live@example.invalid",
		"GIT_COMMITTER_NAME=apa-live", "GIT_COMMITTER_EMAIL=apa-live@example.invalid")
}

func git(t testing.TB, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = gitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// bareRepo 建本机 bare 远程和一个检出（origin 指向 bare），返回 (远程地址, 检出目录)。
func bareRepo(t testing.TB, root, name string, files map[string]string) (string, string) {
	t.Helper()
	bare := filepath.Join(root, name+".git")
	co := filepath.Join(root, name+"-co")
	must(t, os.MkdirAll(bare, 0o755))
	git(t, bare, "init", "--bare", "-b", "main")
	git(t, root, "clone", bare, co)
	for p, c := range files {
		must(t, os.MkdirAll(filepath.Dir(filepath.Join(co, p)), 0o755))
		must(t, os.WriteFile(filepath.Join(co, p), []byte(c), 0o644))
	}
	git(t, co, "add", "-A")
	git(t, co, "commit", "-m", "init")
	git(t, co, "push", "origin", "HEAD:main")
	return bare, co
}

func readFile(t testing.TB, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s：%v", path, err)
	}
	return string(b)
}

func hostname() string { h, _ := os.Hostname(); return h }

// realHermes 返回真实 hermes 可执行文件。live.env 的 APA_LIVE_HERMES_BIN 可能指向 /opt/apa-live/bin/hermes：
// 那是把 HERMES_HOME 固定为共享目录的包装脚本，会覆盖本测试的隔离家目录（没有 apa 插件），所以改用同一安装下 venv 里的 hermes。
func realHermes(t testing.TB) string {
	bin := envOr("APA_LIVE_HERMES_BIN", "/opt/apa-live/venv/bin/hermes")
	if b, err := os.ReadFile(bin); err == nil && strings.HasPrefix(string(b), "#!") && strings.Contains(string(b), "HERMES_HOME=") {
		bin = filepath.Join(filepath.Dir(bin), "..", "venv", "bin", "hermes")
	}
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("真实 Hermes 不可用（%s）：%v", bin, err)
	}
	return filepath.Clean(bin)
}
