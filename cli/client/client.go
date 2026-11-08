// Package client 是 apa CLI 的薄客户端骨干（设计 §4.2、§17.3、§17.4、§11.5）：
// 命令分发、凭据解析、HTTP 调用、JSON 输出与稳定退出码。不含业务逻辑，不访问存储。
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

// 环境变量（§17.4）。
const (
	EnvURL            = "APA_URL"
	EnvToken          = "APA_TOKEN"
	EnvProject        = "APA_PROJECT"
	EnvSession        = "APA_SESSION"
	EnvHarness        = "APA_HARNESS"
	EnvRun            = "APA_RUN"
	EnvRunCredentials = "APA_RUN_CREDENTIAL_FILE"

	apiPrefix = "/v1"
)

// Env 注入进程环境，测试可替换。
type Env struct {
	Getenv   func(string) string
	UserHome string
	Stdin    io.Reader
	Stdout   io.Writer
	Stderr   io.Writer
	HTTP     *http.Client
}

// OSEnv 返回真实进程环境。
func OSEnv() Env {
	home, _ := os.UserHomeDir()
	return Env{Getenv: os.Getenv, UserHome: home, Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		HTTP: &http.Client{Timeout: 60 * time.Second}}
}

// Command 是一条子命令；args 不含子命令名自身。
type Command func(c *Ctx, args []string) error

var registry = map[string]Command{}

// Register 登记顶层子命令。各命令组在自己的文件 init() 里调用，入口不改。重名是编程错误。
func Register(name string, cmd Command) {
	if _, dup := registry[name]; dup {
		panic("client: duplicate command " + name)
	}
	registry[name] = cmd
}

// Ctx 是命令执行上下文。
type Ctx struct {
	Env    Env
	Pretty bool
	// Evidence 是全局 --evidence：delegate 代理操作的本人原话，随每个请求以 X-APA-Evidence 头发出（§14.3）。
	Evidence string
	creds    *Credentials // 懒解析
}

// Project 返回 APA_PROJECT。
func (c *Ctx) Project() string { return c.Env.Getenv(EnvProject) }

// Print 把值输出为 JSON（默认紧凑，--pretty 缩进）。
func (c *Ctx) Print(v any) error { return c.emit(c.Env.Stdout, v) }

func (c *Ctx) emit(w io.Writer, v any) error {
	var b []byte
	var err error
	if c.Pretty {
		b, err = json.MarshalIndent(v, "", "  ")
	} else {
		b, err = json.Marshal(v)
	}
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "%s\n", b)
	return err
}

// usage 构造用法错误（退出码 2）。
func usage(why, next string) error {
	return domain.NewError(domain.CodeInvalidRequest, why, next)
}

// errUnreachable 是 CLI 侧独有的"内核不可达"（退出码 7），没有 HTTP 响应。
const codeUnreachable domain.Code = "unreachable"

func exitOf(err error) int {
	var de *domain.Error
	if errors.As(err, &de) {
		if de.Code == codeUnreachable {
			return domain.ExitUnreachable
		}
		return de.ExitCode()
	}
	return domain.ExitUnclassified
}

// ExitStatus 让命令在成功输出之后仍以非零退出码结束（如 agents check 的 10、11）；它不是错误体，Main 不向 stderr 写任何东西。
type ExitStatus int

func (e ExitStatus) Error() string { return fmt.Sprintf("exit status %d", int(e)) }

// Main 运行 CLI 并返回退出码。成功输出到 stdout，错误体（code/why/next）输出到 stderr。
func Main(args []string, env Env) int {
	c := &Ctx{Env: env}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch a := args[i]; {
		case a == "--pretty":
			c.Pretty = true
		case a == "--evidence" && i+1 < len(args):
			i++
			c.Evidence = args[i]
		case strings.HasPrefix(a, "--evidence="):
			c.Evidence = strings.TrimPrefix(a, "--evidence=")
		default:
			rest = append(rest, a)
		}
	}
	err := c.dispatch(rest)
	if err == nil {
		return domain.ExitOK
	}
	var xs ExitStatus
	if errors.As(err, &xs) {
		return int(xs)
	}
	var de *domain.Error
	if !errors.As(err, &de) {
		// 本地错误（文件系统等）不带账本内容，但细节可能含路径；只给机制性说明。
		de = domain.NewError(domain.CodeInternal, "the command failed locally", "check the APA_HOME directory permissions and retry")
	}
	_ = c.emit(env.Stderr, de)
	return exitOf(de)
}

func (c *Ctx) dispatch(args []string) error {
	if len(args) == 0 {
		return usage("no command given", "run `apa guide`; available commands: "+strings.Join(commandNames(), ", "))
	}
	cmd, ok := registry[args[0]]
	if !ok {
		return usage("unknown command", "run `apa guide`; available commands: "+strings.Join(commandNames(), ", "))
	}
	return cmd(c, args[1:])
}

func commandNames() []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// Raw 作为 Do 的 body 时原样发送（不做 JSON 编码），用于附件上传。
type Raw []byte

// Do 向内核发一次 JSON 请求（path 省略 /v1 前缀）。body 为 nil 则无请求体；
// out 为 nil 时丢弃响应；为 *[]byte 时原样返回响应体而不按 JSON 解码。非 2xx 按附录 B.3 还原为 *domain.Error。
func (c *Ctx) Do(ctx context.Context, method, path string, body, out any) error {
	return c.DoHeaders(ctx, nil, method, path, body, out)
}

// DoHeaders 同 Do，并附加请求头（如 If-Match、Idempotency-Key、X-APA-Evidence）；hdr 不能覆盖认证头。
func (c *Ctx) DoHeaders(ctx context.Context, hdr map[string]string, method, path string, body, out any) error {
	cr, err := c.Credentials()
	if err != nil {
		return err
	}
	return c.doWith(ctx, cr, hdr, method, path, body, out)
}

func (c *Ctx) doWith(ctx context.Context, cr Credentials, hdr map[string]string, method, path string, body, out any) error {
	var rdr io.Reader
	raw, isRaw := body.(Raw)
	if isRaw {
		rdr = bytes.NewReader(raw)
	} else if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return usage("request body is not encodable as JSON", "check the command arguments")
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(cr.URL, "/")+apiPrefix+path, rdr)
	if err != nil {
		return usage("APA_URL is not a valid URL", "set APA_URL (or run `apa login --url`) to the kernel address, e.g. http://127.0.0.1:7777")
	}
	if c.Evidence != "" {
		req.Header.Set("X-APA-Evidence", c.Evidence)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	req.Header.Set("Authorization", "Bearer "+cr.Token)
	req.Header.Set("Accept", "application/json")
	if isRaw {
		req.Header.Set("Content-Type", "application/octet-stream")
	} else if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if s := c.Env.Getenv(EnvSession); s != "" {
		req.Header.Set("X-APA-Session", s)
	}
	if h := c.Env.Getenv(EnvHarness); h != "" {
		req.Header.Set("X-APA-Harness", h)
	}
	resp, err := c.Env.HTTP.Do(req)
	if err != nil {
		return domain.NewError(codeUnreachable, "the APA kernel is not reachable", "check that `apa serve` is running and APA_URL is correct, then retry later")
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return domain.NewError(codeUnreachable, "the connection to the APA kernel broke while reading the response", "retry later")
	}
	if resp.StatusCode/100 != 2 {
		return decodeError(resp.StatusCode, data)
	}
	if raw, ok := out.(*[]byte); ok { // 纯文本响应（如 format=inject）原样交回
		*raw = data
		return nil
	}
	if out == nil || len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return domain.NewError(domain.CodeInternal, "the kernel returned a response that is not valid JSON", "check that APA_URL points at an APA kernel")
	}
	return nil
}

// decodeError 还原错误体。只信任 code/why/next/allowed/web_url 这些机制字段；
// 体不是合法错误体时（如代理页面）不回显原文，按 HTTP 状态归类。
func decodeError(status int, data []byte) error {
	var e domain.Error
	if json.Unmarshal(data, &e) == nil && e.Code != "" && e.Why != "" {
		if _, known := knownCodes[e.Code]; known {
			return &e
		}
		e.Code = domain.CodeFromHTTP(status) // 未登记的码按状态归类，退出码不被服务端任意指定
		return &e
	}
	code := domain.CodeFromHTTP(status)
	return domain.NewError(code, fmt.Sprintf("the kernel answered HTTP %d without a standard error body", status),
		"check that APA_URL points at an APA kernel")
}

var knownCodes = map[domain.Code]struct{}{
	domain.CodeInvalidRequest: {}, domain.CodeUnauthenticated: {}, domain.CodePermissionDenied: {},
	domain.CodeWebOnly: {}, domain.CodeExecutorNotAllowed: {}, domain.CodeRiskScreeningPending: {}, domain.CodeBasisRequired: {},
	domain.CodeNotFound: {}, domain.CodeConflict: {}, domain.CodeInternal: {},
}
