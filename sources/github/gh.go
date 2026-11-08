package github

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"regexp"
	"strings"
)

// GH 是用 gh CLI 实现的 API。凭据只来自 ConfigDir（来源登记的 credentials_path），不读环境里的 token 和主机设置。
type GH struct {
	Bin       string // 默认 "gh"
	ConfigDir string
}

func (g GH) env() []string {
	env := os.Environ()[:0:0]
	for _, kv := range os.Environ() {
		switch k, _, _ := strings.Cut(kv, "="); k {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN", "GH_CONFIG_DIR", "GH_HOST":
		default:
			env = append(env, kv)
		}
	}
	return append(env, "GH_CONFIG_DIR="+g.ConfigDir, "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1")
}

func (g GH) bin() string {
	if g.Bin != "" {
		return g.Bin
	}
	return "gh"
}

// Get 执行 `gh api <path>`，并把失败按 §9.2 归类。
func (g GH) Get(ctx context.Context, path string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, g.bin(), "api", "-H", "Accept: application/vnd.github+json", path)
	cmd.Env = g.env()
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
		return nil, &Error{Exit: ExitAuth, Msg: "gh not found in PATH"}
	}
	return nil, classify(errb.String() + out.String())
}

var (
	rateRe = regexp.MustCompile(`(?i)rate limit|abuse|secondary|HTTP 429`)
	httpRe = regexp.MustCompile(`HTTP (\d{3})`)
)

// classify 按 gh 的输出判定退出码：认证/权限/仓库不可见 → 2，仓库已迁移删除（410）→ 3，其余（限流、5xx、网络）→ 1。
func classify(msg string) *Error {
	msg = strings.TrimSpace(msg)
	if rateRe.MatchString(msg) {
		return &Error{Exit: ExitTemporary, Msg: msg}
	}
	if strings.Contains(msg, "gh auth login") || strings.Contains(strings.ToLower(msg), "not logged in") {
		return &Error{Exit: ExitAuth, Msg: msg}
	}
	if m := httpRe.FindStringSubmatch(msg); m != nil {
		switch m[1] {
		case "401", "403":
			return &Error{Exit: ExitAuth, Msg: msg}
		case "404":
			return &Error{Exit: ExitAuth, Msg: msg, NotFound: true}
		case "410":
			return &Error{Exit: ExitPermanent, Msg: msg}
		}
	}
	return &Error{Exit: ExitTemporary, Msg: msg}
}

// Auth 启动 gh 的交互认证，凭据写入该来源自己的目录（`apa source auth <id>` 的落点）。
// --insecure-storage 让 token 落在配置目录而非系统钥匙串，这样凭据跟着来源目录走。
func Auth(ctx context.Context, g GH, stdin io.Reader, stdout, stderr io.Writer) int {
	if err := os.MkdirAll(g.ConfigDir, 0o700); err != nil {
		fmt.Fprintln(stderr, "github source:", err)
		return ExitAuth
	}
	cmd := exec.CommandContext(ctx, g.bin(), "auth", "login", "--insecure-storage")
	cmd.Env = g.env()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	if err := cmd.Run(); err != nil {
		fmt.Fprintln(stderr, "github source: auth failed:", err)
		return ExitAuth
	}
	return ExitOK
}
