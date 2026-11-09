package client

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/kernel/config"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const credentialsFile = "credentials.json"

// Credentials 是 CLI 调内核所需的地址与 token。
type Credentials struct {
	URL   string `json:"url"`
	Token string `json:"token"`
}

// withRunContext 让运行上下文文件（APA_RUN_CONTEXT，§7.7）决定运行身份，盖过环境里的 APA_RUN、APA_URL、APA_TOKEN、
// APA_PROJECT：工具若在 harness 的常驻进程里执行，这些变量是拉起那个进程的运行留下的，不是这次运行的。
// 文件读不到（运行已结束）时身份留空但仍是运行模式：请求一律未认证，绝不回退到本机凭据。
// PATH 带运行标记却没有 APA_RUN_CONTEXT，说明 harness 没放行它，同样按运行模式拒绝，不用环境里的其他凭据。
func withRunContext(env Env) Env {
	path := env.Getenv(EnvRunContext)
	if path == "" && !runMarked(env.Getenv("PATH")) {
		return env
	}
	var rc struct {
		Run     string `json:"run"`
		URL     string `json:"url"`
		Token   string `json:"token"`
		Project string `json:"project"`
	}
	b, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(b, &rc)
	}
	if err != nil || rc.Run == "" {
		rc.Run, rc.Token = "unknown", ""
	}
	base := env.Getenv
	env.Getenv = func(k string) string {
		switch k {
		case EnvRun:
			return rc.Run
		case EnvURL:
			return rc.URL
		case EnvToken:
			return rc.Token
		case EnvProject:
			return rc.Project
		case EnvRunCredentials:
			return ""
		}
		return base(k)
	}
	return env
}

func runMarked(path string) bool {
	for _, p := range filepath.SplitList(path) {
		if filepath.Base(p) == runMark {
			return true
		}
	}
	return false
}

// RunMode 报告是否处于运行身份（设置了 APA_RUN，§11.5）。
func (c *Ctx) RunMode() bool { return c.Env.Getenv(EnvRun) != "" }

func (c *Ctx) credentialsPath() (string, error) {
	home, err := config.ResolveHome(c.Env.Getenv, c.Env.UserHome)
	if err != nil {
		return "", usage("cannot locate the APA home directory", "set APA_HOME to an absolute path")
	}
	return filepath.Join(home, credentialsFile), nil
}

// Credentials 解析凭据（§11.5、§17.4）：
// 设置了 APA_RUN 时只用运行凭据（APA_TOKEN，否则 APA_RUN_CREDENTIAL_FILE 指向的文件），绝不读本机凭据文件；
// 否则 APA_URL / APA_TOKEN 优先，缺的部分由 `apa login` 保存的文件补齐。
func (c *Ctx) Credentials() (Credentials, error) {
	if c.creds != nil {
		return *c.creds, nil
	}
	cr, err := c.resolveCredentials()
	if err != nil {
		return Credentials{}, err
	}
	c.creds = &cr
	return cr, nil
}

func (c *Ctx) resolveCredentials() (Credentials, error) {
	cr := Credentials{URL: c.Env.Getenv(EnvURL), Token: c.Env.Getenv(EnvToken)}
	if c.RunMode() {
		if cr.Token == "" {
			if f := c.Env.Getenv(EnvRunCredentials); f != "" {
				b, err := os.ReadFile(f)
				if err != nil {
					return cr, domain.NewError(domain.CodeUnauthenticated, "the run credential file cannot be read", "check APA_RUN_CREDENTIAL_FILE; a run never falls back to the local credential file")
				}
				cr.Token = strings.TrimSpace(string(b))
			}
		}
		if cr.Token == "" && c.Env.Getenv(EnvRunContext) == "" && runMarked(c.Env.Getenv("PATH")) {
			return cr, domain.NewError(domain.CodeUnauthenticated, "this is an APA run but APA_RUN_CONTEXT did not reach this process", "let the harness pass APA_RUN_CONTEXT through to its tools (see the harness plugin's README), then start a new run. A run never uses another credential")
		}
		if cr.Token == "" && c.Env.Getenv(EnvRunContext) != "" {
			return cr, domain.NewError(domain.CodeUnauthenticated, "the run context file cannot be read", "the run has ended or never started; do not retry. A run never falls back to the local credential file")
		}
		if cr.Token == "" {
			return cr, domain.NewError(domain.CodeUnauthenticated, "APA_RUN is set but no run credential was provided", "provide APA_TOKEN or APA_RUN_CREDENTIAL_FILE; a run never falls back to the local credential file")
		}
		if cr.URL == "" {
			return cr, usage("APA_RUN is set but APA_URL is not", "set APA_URL to the kernel address")
		}
		return cr, nil
	}
	if cr.URL == "" || cr.Token == "" {
		p, err := c.credentialsPath()
		if err != nil {
			return cr, err
		}
		b, err := os.ReadFile(p)
		switch {
		case err == nil:
			var saved Credentials
			if json.Unmarshal(b, &saved) != nil {
				return cr, domain.NewError(domain.CodeUnauthenticated, "the saved credential file is corrupt", "run `apa login --url <url> --token <token>` again")
			}
			if cr.URL == "" {
				cr.URL = saved.URL
			}
			if cr.Token == "" {
				cr.Token = saved.Token
			}
		case !errors.Is(err, os.ErrNotExist):
			return cr, domain.NewError(domain.CodeUnauthenticated, "the saved credential file cannot be read", "check permissions of the APA home directory, or run `apa login` again")
		}
	}
	if cr.URL == "" || cr.Token == "" {
		return cr, domain.NewError(domain.CodeUnauthenticated, "no APA credential is available", "run `apa login --url <url> --token <token>`, or set APA_URL and APA_TOKEN")
	}
	return cr, nil
}

// saveCredentials 原子写入凭据文件，权限 0600。
func (c *Ctx) saveCredentials(cr Credentials) error {
	p, err := c.credentialsPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	b, _ := json.Marshal(cr)
	tmp, err := os.CreateTemp(filepath.Dir(p), ".credentials-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil { // CreateTemp 已是 0600
		return err
	}
	return os.Rename(tmp.Name(), p)
}
