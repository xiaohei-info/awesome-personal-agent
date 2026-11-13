//go:build e2e

package hostplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

type fx struct {
	*baseline.Env
	t *testing.T
}

// step 把流程里的一个 action 作为子测试运行；前一步未通过，后续不得开始。
func (f *fx) step(name string, fn func(t *testing.T)) {
	f.t.Helper()
	parent := f.t
	ok := parent.Run(name, func(t *testing.T) {
		f.t, f.Env.T = t, t
		defer func() { f.t, f.Env.T = parent, parent }()
		fn(t)
	})
	if !ok {
		parent.Fatalf("%s 未通过，后续流程不得开始", name)
	}
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	return f.apaEnv(token, want, nil, args...)
}

// apaEnv 以 token 运行 apa，并追加环境变量（如 APA_SESSION、APA_RUN）。
func (f *fx) apaEnv(token string, want int, env map[string]string, args ...string) baseline.Result {
	f.t.Helper()
	r := f.runApa(token, env, args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func idOf(t *testing.T, stdout string) string {
	t.Helper()
	var m struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal([]byte(stdout), &m); err != nil || m.ID == "" {
		t.Fatalf("输出里没有事项 id: %q %v", stdout, err)
	}
	return m.ID
}

// add 用 DEL 在项目里建一个事项。
func (f *fx) add(project, title, phase string, extra ...string) string {
	f.t.Helper()
	args := append([]string{"item", "add", "--kind", "task", "--title", title, "--phase", phase, "--assignee", "work-host.cli",
		"--project", f.Projects[project], "--evidence", "验收建事项"}, extra...)
	return idOf(f.t, f.apa(f.DEL, 0, args...).Stdout)
}

// ask 用 WRK 给事项写入带两个选项的决策请求。
func (f *fx) ask(id string, extra ...string) {
	f.t.Helper()
	f.apa(f.WRK, 0, append([]string{"ask", id, "--question", "选哪个方案", "--option", "A=方案A", "--option", "B=方案B", "--recommend", "A"}, extra...)...)
}

// decision 建一条普通待决事项并返回事项 id。
func (f *fx) decision(project, title string) string {
	f.t.Helper()
	id := f.add(project, title, "open")
	f.ask(id)
	return id
}

func repoRoot() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "..")
}

// py 在仓库根目录运行 python3，返回合并输出与退出码（插件契约测试与自检命令）。
func py(t *testing.T, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("python3", args...)
	cmd.Dir = repoRoot()
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); ok {
		return string(out), ee.ExitCode()
	} else if err != nil {
		t.Fatalf("python3 %v: %v", args, err)
	}
	return string(out), 0
}

// pytest 运行一个插件契约测试文件，要求退出码 0 且输出里包含 want 的每个用例名（-v 逐条列出）。
func pytest(t *testing.T, file string, want ...string) string {
	t.Helper()
	out, code := py(t, "-m", "pytest", "-v", file)
	if code != 0 {
		t.Fatalf("pytest %s 退出码 %d\n%s", file, code, out)
	}
	for _, w := range want {
		if !regexp.MustCompile(`::` + regexp.QuoteMeta(w) + `(\[[^\]]*\])? PASSED`).MatchString(out) {
			t.Errorf("pytest %s 的结果里没有通过的用例 %s", file, w)
		}
	}
	return out
}

func qpath(s string) string { return url.PathEscape(s) }

// runApa 以完全由调用方给定的环境运行 apa：只带 APA_HOME、APA_URL 和 env（不像 baseline.Apa 那样替身会话变量），
// 这样测试精确控制宿主插件会设置的 APA_HARNESS、APA_SESSION、APA_SESSION_KIND、APA_RUN。
func (f *fx) runApa(token string, env map[string]string, args ...string) baseline.Result {
	f.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, f.Bin["apa"], args...)
	cmd.Env = append(os.Environ(), "APA_HOME="+f.Home, "APA_URL="+f.URL)
	for _, kv := range []string{"APA_TOKEN", "APA_SESSION", "APA_SESSION_KIND", "APA_HARNESS", "APA_RUN"} {
		cmd.Env = append(cmd.Env, kv+"=") // 清掉外层环境里可能存在的同名变量
	}
	if token != "" {
		cmd.Env = append(cmd.Env, "APA_TOKEN="+token)
	}
	if slices.Contains(args, "--evidence") { // 带本人原话的命令按私聊上报，与 baseline.Apa 一致；测试要验证别的会话时在 env 里显式给出
		env = maps.Clone(env)
		if env == nil {
			env = map[string]string{}
		}
		for k, v := range map[string]string{"APA_SESSION": "e2e-session", "APA_SESSION_KIND": "direct"} {
			if _, ok := env[k]; !ok {
				env[k] = v
			}
		}
	}
	for k, v := range env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := baseline.Result{Stdout: out.String(), Stderr: errb.String()}
	if ee, ok := err.(*exec.ExitError); ok {
		r.Code = ee.ExitCode()
	} else if err != nil {
		f.t.Fatalf("运行 apa %v: %v", args, err)
	}
	return r
}

// declareSelfcheck 重新提交 harness 的无头运行声明并带上自检命令，在 Web 确认（新修订号）。
func (f *fx) declareSelfcheck(harness string, selfcheck []string) {
	f.t.Helper()
	bin := f.Bin["stub-harness"]
	decl, _ := json.Marshal(map[string]any{"harness": harness,
		"headless": map[string]any{"command": []string{bin, "{prompt}"}, "session": map[string]any{"extract": "$.session_id"},
			"resume": []string{bin, "--resume", "{prompt}"},
			"access": map[string]any{"read": []string{"--access=read"}, "edit": []string{"--access=edit"}, "full": []string{"--access=full"}}},
		"selfcheck": map[string]any{"command": selfcheck}})
	r := f.Apa(f.DEL, string(decl), "declare", "-", "--evidence", "提交带自检命令的声明")
	var out struct {
		Rev int `json:"rev"`
	}
	if r.Code != 0 || json.Unmarshal([]byte(r.Stdout), &out) != nil || out.Rev < 1 {
		f.t.Fatalf("apa declare: %d %s%s", r.Code, r.Stdout, r.Stderr)
	}
	f.webOK("POST", "/settings/declarations/"+harness+"/"+strconv.Itoa(out.Rev)+"/confirm", "", 200)
}

// hostPluginDetail 是 `apa doctor --json` 里"宿主插件"项对 harness 的明细。
func (f *fx) hostPluginDetail(harness string) (status, why string) {
	f.t.Helper()
	r := f.runApa(f.OBS, nil, "doctor", "--json") // 其他项的 warning 不影响本项，退出码不作判断
	var out struct {
		Checks []struct {
			Name    string `json:"name"`
			Details []struct {
				Subject string `json:"subject"`
				Status  string `json:"status"`
				Why     string `json:"why"`
				Next    string `json:"next"`
			} `json:"details"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(r.Stdout), &out); err != nil {
		f.t.Fatalf("apa doctor --json 输出不是 JSON: %v\n%s%s", err, r.Stdout, r.Stderr)
	}
	for _, c := range out.Checks {
		if c.Name != "host_plugin" {
			continue
		}
		for _, d := range c.Details {
			if d.Subject == harness {
				return d.Status, d.Why + " | " + d.Next
			}
		}
	}
	f.t.Fatalf("doctor 没有 %s 的宿主插件明细: %s", harness, r.Stdout)
	return
}

func (f *fx) hostPluginStatus(harness string) string { s, _ := f.hostPluginDetail(harness); return s }
func (f *fx) hostPluginWhy(harness string) string    { _, w := f.hostPluginDetail(harness); return w }

// wrapperResult 是插件包 CLI 包装返回给模型的结果。
type wrapperResult struct {
	ExitCode int             `json:"exit_code"`
	OK       bool            `json:"ok"`
	Stdout   json.RawMessage `json:"stdout"`
	Stderr   json.RawMessage `json:"stderr"`
	Error    string          `json:"error"`
}

// pluginWrapper 在"终端会话"宿主上下文替身里，经插件包（pluginRoot 下的 tools）的 CLI 包装运行一次 apa。
// APA_BIN 是一个记录调用环境（只记 APA_HARNESS、APA_SESSION、APA_SESSION_KIND，不记凭据）后转给真 apa 的脚本。
func (f *fx) pluginWrapper(pluginRoot, session, token string, args ...string) wrapperResult {
	f.t.Helper()
	logf := f.wrapperLogPath()
	shim := filepath.Join(f.Root, "apa-recorder.sh")
	body := fmt.Sprintf("#!/bin/sh\nenv | grep -E '^APA_(HARNESS|SESSION|SESSION_KIND)=' | sort > %q\nexec %q \"$@\"\n", logf, f.Bin["apa"])
	if err := os.WriteFile(shim, []byte(body), 0o755); err != nil {
		f.t.Fatal(err)
	}
	cmd := exec.Command("python3", append([]string{"-I", filepath.Join(repoRoot(), "tests", "e2e", "hostplugin", "testdata", "wrapper_driver.py"), pluginRoot, session}, args...)...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + f.Root, "APA_HOME=" + f.Home, "APA_URL=" + f.URL, "APA_TOKEN=" + token, "APA_BIN=" + shim}
	out, err := cmd.Output()
	if err != nil {
		f.t.Fatalf("wrapper_driver: %v\n%s", err, out)
	}
	var r wrapperResult
	if err := json.Unmarshal(out, &r); err != nil {
		f.t.Fatalf("包装结果不是 JSON: %v\n%s", err, out)
	}
	if r.Error != "" {
		f.t.Fatalf("包装工具报错: %s", r.Error)
	}
	return r
}

func (f *fx) wrapperLogPath() string { return filepath.Join(f.Root, "apa-recorder.env") }

// wrapperEnvLog 是最近一次经包装工具调用 apa 时的 APA_* 环境记录。
func (f *fx) wrapperEnvLog() string {
	f.t.Helper()
	b, err := os.ReadFile(f.wrapperLogPath())
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

// extractV01Plugin 解出 v0.1 发布物里的宿主插件包，返回其中 hermes 目录（含 tools）。
func (f *fx) extractV01Plugin() string {
	f.t.Helper()
	dir := filepath.Join(f.Root, "v01-plugin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		f.t.Fatal(err)
	}
	if out, err := exec.Command("tar", "-xzf", f.V01Plugin(), "-C", dir).CombinedOutput(); err != nil {
		f.t.Fatalf("解开 v0.1 插件包: %v\n%s", err, out)
	}
	return filepath.Join(dir, "hermes")
}
