//go:build e2e

// Package mapping 按验收文档 flow-project-mapping（ACT-MAP-01..04）用真实 apa 二进制验证
// "当前目录按明确规则映射到项目，不猜"。环境来自 flow-env-baseline 的共用脚本。
package mapping

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// scope 是 `apa context` 输出里与映射相关的部分。
type scope struct {
	Project *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"project"`
	Items []struct {
		ID string `json:"id"`
	} `json:"items"`
}

// apaIn 在目录 dir 内以 token 运行 apa，并追加 extra 环境变量（真实进程的当前目录与环境决定映射）。
func apaIn(t *testing.T, e *baseline.Env, dir, token string, extra []string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, e.Bin["apa"], args...)
	cmd.Dir = dir
	// 先剔除外部环境里可能存在的 APA_*，免得验收结果受执行者 shell 影响。
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "APA_") {
			cmd.Env = append(cmd.Env, kv)
		}
	}
	cmd.Env = append(cmd.Env, "APA_HOME="+e.Home, "APA_URL="+e.URL, "APA_TOKEN="+token)
	cmd.Env = append(cmd.Env, extra...)
	var o, er bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &er
	if err := cmd.Run(); err != nil {
		ee, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("运行 apa %v: %v", args, err)
		}
		code = ee.ExitCode()
	}
	return o.String(), er.String(), code
}

// ctxIn 运行 `apa context` 并解析作用域；退出码必须为 0。
func ctxIn(t *testing.T, e *baseline.Env, dir string, extra []string, args ...string) scope {
	t.Helper()
	out, errOut, code := apaIn(t, e, dir, e.OBS, extra, append([]string{"context"}, args...)...)
	if code != 0 {
		t.Fatalf("apa context %v 在 %s 退出码 %d: %s", args, dir, code, errOut)
	}
	var s scope
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("apa context 输出不是 JSON: %v\n%s", err, out)
	}
	return s
}

func (s scope) has(id string) bool {
	for _, it := range s.Items {
		if it.ID == id {
			return true
		}
	}
	return false
}

func mkdir(t *testing.T, root string, rel string) string {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	// 进程的当前目录是真实路径；登记的检出也必须是真实路径。
	if p, err := filepath.EvalSymlinks(p); err == nil {
		return p
	}
	return p
}

func TestFlowProjectMapping(t *testing.T) {
	e := baseline.New(t)
	e.StartKernel()
	e.IssueCredentials()
	e.CreateProjects() // acc-work、acc-open、acc-empty、acc-home

	host, err := os.Hostname()
	if err != nil || host == "" {
		t.Fatalf("取不到本机主机名: %v", err)
	}
	work, open, home := e.Projects["acc-work"], e.Projects["acc-open"], e.Projects["acc-home"]

	// 每个项目一条事项：作用域里该出现谁的事项，一眼可辨；另有一条无项目事项。
	items := map[string]string{}
	for name, proj := range map[string]string{"acc-work": work, "acc-open": open, "acc-home": home, "free": ""} {
		args := []string{"item", "add", "--kind", "task", "--title", "mapping-" + name, "--reason", "mapping e2e", "--evidence", "mapping e2e"}
		if proj != "" {
			args = append(args, "--project", proj)
		}
		out, errOut, code := apaIn(t, e, e.Root, e.DEL, nil, args...)
		var it struct {
			ID string `json:"id"`
		}
		if code != 0 || json.Unmarshal([]byte(out), &it) != nil || it.ID == "" {
			t.Fatalf("建事项 %s: %d %s %s", name, code, out, errOut)
		}
		items[name] = it.ID
	}

	acc := mkdir(t, e.Root, "acc")
	repo := mkdir(t, acc, "repo")
	sub := mkdir(t, acc, "repo/sub")
	subX := mkdir(t, acc, "repo/sub/x")
	other := mkdir(t, acc, "other")
	outside := mkdir(t, e.Root, "outside") // 不在任何检出内

	// 检出必须挂在项目已登记的远程地址下（内核：remote is not registered in this project），
	// 远程地址由 delegate 登记；acc-work 的远程地址已由环境基线登记。
	remotes := map[string]string{work: e.TestRepo}
	for name, proj := range map[string]string{"acc-open": open, "acc-home": home} {
		remotes[proj] = "https://example.invalid/acc/" + name + ".git"
		if _, errOut, code := apaIn(t, e, e.Root, e.DEL, []string{"APA_SESSION=e2e-session"}, "project", "repos", "add", proj, "--remote", remotes[proj], "--evidence", "mapping e2e"); code != 0 {
			t.Fatalf("登记 %s 远程地址退出码 %d: %s", name, code, errOut)
		}
	}
	addCheckout := func(proj, path, h string) {
		t.Helper()
		args := []string{"project", "repos", "add", proj, "--remote", remotes[proj], "--path", path}
		if h != "" {
			args = append(args, "--host", h)
		}
		if _, errOut, code := apaIn(t, e, e.Root, e.WRK, nil, args...); code != 0 {
			t.Fatalf("apa %v 退出码 %d: %s", args, code, errOut)
		}
	}
	step := func(name string, f func(t *testing.T)) {
		t.Helper()
		if !t.Run(name, f) {
			t.Fatalf("%s 未通过，后续流程不得开始", name)
		}
	}

	step("ACT-MAP-01 显式指定优先", func(t *testing.T) {
		addCheckout(work, repo, "") // acc-work 的检出目录
		for name, s := range map[string]scope{
			"--project":   ctxIn(t, e, repo, nil, "--project", open),
			"APA_PROJECT": ctxIn(t, e, repo, []string{"APA_PROJECT=" + open}, "--scope", "cwd"),
		} {
			if s.Project == nil || s.Project.ID != open || s.Project.Name != "acc-open" {
				t.Errorf("%s：作用域应是 acc-open，实际 %+v", name, s.Project)
			}
			if !s.has(items["acc-open"]) || s.has(items["acc-work"]) {
				t.Errorf("%s：事项被当前目录覆盖: %+v", name, s.Items)
			}
		}
		// 对照：不显式指定时，同一目录确实属于 acc-work，说明上面是显式指定在起作用。
		if s := ctxIn(t, e, repo, nil, "--scope", "cwd"); s.Project == nil || s.Project.ID != work {
			t.Errorf("对照失败，repo 目录应属于 acc-work: %+v", s.Project)
		}
	})

	step("ACT-MAP-02 按本机检出取最长前缀", func(t *testing.T) {
		addCheckout(open, sub, "")
		s := ctxIn(t, e, subX, nil, "--scope", "cwd")
		if s.Project == nil || s.Project.ID != open || !s.has(items["acc-open"]) || s.has(items["acc-work"]) {
			t.Fatalf("最长前缀应选 acc-open: %+v", s)
		}
		// 内核映射接口与 CLI 结论一致。
		r := e.APIRaw("GET", "/v1/projects/resolve?path="+subX+"&host="+host, "", e.OBS)
		var res struct {
			Project *struct {
				ID string `json:"id"`
			} `json:"project"`
		}
		if r.Code != 200 || json.Unmarshal([]byte(r.Body), &res) != nil || res.Project == nil || res.Project.ID != open {
			t.Errorf("GET /v1/projects/resolve 与 CLI 不一致: %d %s", r.Code, r.Body)
		}
		// 回到较短前缀的目录仍属于 acc-work，证明选的是最长而非最后登记的。
		if s := ctxIn(t, e, repo, nil, "--scope", "cwd"); s.Project == nil || s.Project.ID != work {
			t.Errorf("repo 目录应仍属于 acc-work: %+v", s.Project)
		}
	})

	step("ACT-MAP-03 其他主机的检出不参与匹配", func(t *testing.T) {
		addCheckout(home, other, "other-host")
		s := ctxIn(t, e, other, nil, "--scope", "cwd")
		if s.Project != nil || s.has(items["acc-home"]) {
			t.Errorf("other-host 的检出不应匹配本机目录: %+v", s)
		}
		// 对照：同一路径换成该主机名时才会匹配，说明登记确实生效。
		if s := ctxIn(t, e, other, nil, "--scope", "cwd", "--host", "other-host"); s.Project == nil || s.Project.ID != home {
			t.Errorf("对照失败，--host other-host 应匹配 acc-home: %+v", s.Project)
		}
		r := e.APIRaw("GET", "/v1/projects/resolve?path="+other+"&host="+host, "", e.OBS)
		if r.Code != 200 || strings.Contains(r.Body, home) {
			t.Errorf("resolve 不应返回 acc-home: %d %s", r.Code, r.Body)
		}
	})

	step("ACT-MAP-04 未命中时不猜", func(t *testing.T) {
		s := ctxIn(t, e, outside, nil, "--scope", "cwd")
		if s.Project != nil {
			t.Fatalf("不应猜测项目: %+v", s.Project)
		}
		for name, id := range items {
			if name != "free" && s.has(id) {
				t.Errorf("无项目作用域返回了项目 %s 的事项", name)
			}
		}
		if !s.has(items["free"]) {
			t.Errorf("无项目作用域应仍可见无项目事项: %+v", s.Items)
		}
	})
}
