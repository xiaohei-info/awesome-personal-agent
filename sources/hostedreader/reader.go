// Package hostedreader 是托管文档只读读取器（设计 §12.3）：
// 按 Source 执行契约（stdin JSON → 退出码 + stdout）读取仓库默认分支上的文件内容。
// 有本机检出时读检出的默认分支提交（不读工作区），否则读远程，都读不到时返回明确失败。
// 凭据由 git 自身配置持有，读取器与内核都不读取凭据。
package hostedreader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
)

// 退出码沿用 §9.2 的 Source 契约。
const (
	ExitOK          = 0
	ExitUnreadable  = 1 // 临时失败：检出与远程都读不到，调用方显示"暂不可读"
	ExitBadConfig   = 2 // 输入或配置不合法
	ExitPathMissing = 3 // 永久失败：默认分支上已没有该路径，调用方生成 needs_input 修正登记
)

// 失败码，随 stdout 的 error 对象返回。
const (
	CodeUnreadable  = "unreadable"
	CodeBadConfig   = "bad_config"
	CodePathMissing = "path_not_found"
)

// Input 是 stdin 的 Source 契约输入；config 里是读取参数。
type Input struct {
	SourceID string `json:"source_id"`
	RunID    string `json:"run_id"`
	Config   Config `json:"config"`
}

// Config 读取参数。Checkout 是 APA 主机上的本机检出路径，可空。
type Config struct {
	Repo     string `json:"repo"`
	Path     string `json:"path"`
	Checkout string `json:"checkout,omitempty"`
}

// Output 是成功时 stdout 的唯一一行 JSON。Origin 为 checkout 或 remote。
type Output struct {
	Commit  string `json:"commit"`
	Branch  string `json:"branch"`
	Path    string `json:"path"`
	Origin  string `json:"origin"`
	Content string `json:"content"`
}

type failure struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

type readErr struct {
	exit int
	code string
	msg  string
}

func (e *readErr) Error() string { return e.msg }

// Run 执行一次读取并返回退出码；结果（成功或失败对象）写 stdout，诊断写 stderr。
func Run(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) int {
	out, err := read(ctx, stdin)
	enc := json.NewEncoder(stdout)
	if err == nil {
		_ = enc.Encode(out)
		return ExitOK
	}
	var re *readErr
	if !errors.As(err, &re) {
		re = &readErr{ExitUnreadable, CodeUnreadable, err.Error()}
	}
	var f failure
	f.Error.Code, f.Error.Message = re.code, re.msg
	_ = enc.Encode(f)
	fmt.Fprintln(stderr, re.msg)
	return re.exit
}

func read(ctx context.Context, stdin io.Reader) (*Output, error) {
	var in Input
	dec := json.NewDecoder(stdin)
	if err := dec.Decode(&in); err != nil {
		return nil, badConfig("stdin 不是合法的 JSON: %v", err)
	}
	cfg := in.Config
	if err := validate(cfg); err != nil {
		return nil, err
	}
	cfg.Path = path.Clean(cfg.Path)

	var checkoutErr error
	if cfg.Checkout != "" {
		out, err := readCheckout(ctx, cfg)
		if err == nil || isPathMissing(err) {
			return out, err
		}
		checkoutErr = err // 检出不可用（不存在、不是同一仓库、无默认分支）时退到远程
	}
	out, err := readRemote(ctx, cfg)
	if err != nil && checkoutErr != nil && !isPathMissing(err) {
		return nil, unreadable("检出与远程都读不到: 检出: %v; 远程: %v", checkoutErr, err)
	}
	return out, err
}

func validate(c Config) error {
	switch {
	case c.Repo == "" || strings.HasPrefix(c.Repo, "-"):
		return badConfig("config.repo 为空或不合法")
	case c.Path == "" || path.IsAbs(c.Path) || strings.HasPrefix(path.Clean(c.Path), ".."):
		return badConfig("config.path 必须是仓库内相对路径")
	}
	return nil
}

func readCheckout(ctx context.Context, c Config) (*Output, error) {
	g := git{dir: c.Checkout}
	if _, err := g.run(ctx, "rev-parse", "--git-dir"); err != nil {
		return nil, unreadable("本机检出不可用: %v", err)
	}
	if origin, err := g.run(ctx, "remote", "get-url", "origin"); err == nil && normalizeRepo(string(origin)) != normalizeRepo(c.Repo) {
		return nil, unreadable("本机检出的 origin 与登记的仓库不一致")
	}
	branch, ref, err := defaultRef(ctx, g)
	if err != nil {
		return nil, err
	}
	return showFile(ctx, g, ref, branch, c.Path, "checkout")
}

// defaultRef 优先取远程跟踪的默认分支，其次本地 main/master；绝不读工作区。
func defaultRef(ctx context.Context, g git) (branch, ref string, err error) {
	if b, e := g.run(ctx, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); e == nil {
		name := strings.TrimPrefix(strings.TrimSpace(string(b)), "origin/")
		return name, "refs/remotes/origin/" + name, nil
	}
	for _, name := range []string{"main", "master"} {
		if _, e := g.run(ctx, "rev-parse", "--verify", "--quiet", "refs/heads/"+name); e == nil {
			return name, "refs/heads/" + name, nil
		}
	}
	return "", "", unreadable("本机检出找不到默认分支")
}

func readRemote(ctx context.Context, c Config) (*Output, error) {
	tmp, err := os.MkdirTemp("", "hostedreader-")
	if err != nil {
		return nil, unreadable("创建临时目录失败: %v", err)
	}
	defer os.RemoveAll(tmp)
	// 只取默认分支最新一个提交的元数据，不检出文件；blob 按需拉取。
	if _, err := (git{}).run(ctx, "clone", "--quiet", "--depth=1", "--no-checkout", "--filter=blob:none", "--", cloneURL(c.Repo), tmp); err != nil {
		return nil, unreadable("远程不可读: %v", err)
	}
	g := git{dir: tmp}
	branch, err := g.run(ctx, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, unreadable("远程不可读: %v", err)
	}
	return showFile(ctx, g, "HEAD", strings.TrimSpace(string(branch)), c.Path, "remote")
}

func showFile(ctx context.Context, g git, ref, branch, p, origin string) (*Output, error) {
	commit, err := g.run(ctx, "rev-parse", "--verify", ref+"^{commit}")
	if err != nil {
		return nil, unreadable("读取提交失败: %v", err)
	}
	spec := ref + ":" + p
	if typ, err := g.run(ctx, "cat-file", "-t", spec); err != nil || strings.TrimSpace(string(typ)) != "blob" {
		return nil, &readErr{ExitPathMissing, CodePathMissing, fmt.Sprintf("默认分支 %s 上不存在文件 %s", branch, p)}
	}
	content, err := g.run(ctx, "cat-file", "blob", spec)
	if err != nil {
		return nil, unreadable("读取文件失败: %v", err)
	}
	return &Output{Commit: strings.TrimSpace(string(commit)), Branch: branch, Path: p, Origin: origin, Content: string(content)}, nil
}

func isPathMissing(err error) bool {
	var re *readErr
	return errors.As(err, &re) && re.code == CodePathMissing
}

// ownerRepo 是来源事件用的 owner/name 简写。
var ownerRepo = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.-]*/[A-Za-z0-9_-][A-Za-z0-9_.-]*$`)

// cloneURL 把简写展开成可克隆的地址，否则 git 会把它当本地相对路径。简写的默认站点只在这里认定，内核不认识具体站点。
func cloneURL(repo string) string {
	if ownerRepo.MatchString(repo) && !strings.Contains(repo, "..") {
		return "https://github.com/" + repo
	}
	return repo
}

func normalizeRepo(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.TrimSuffix(strings.TrimSuffix(s, "/"), ".git")
}

func badConfig(f string, a ...any) error {
	return &readErr{ExitBadConfig, CodeBadConfig, fmt.Sprintf(f, a...)}
}

func unreadable(f string, a ...any) error {
	return &readErr{ExitUnreadable, CodeUnreadable, fmt.Sprintf(f, a...)}
}

// git 以子进程调用 git CLI（Source 在内核之外，允许起进程）。
type git struct{ dir string }

func (g git) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = g.dir
	// 禁止交互提示；只放行常见传输协议，挡掉 ext:: 等可执行任意命令的传输。
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_ALLOW_PROTOCOL=file:git:http:https:ssh", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}
