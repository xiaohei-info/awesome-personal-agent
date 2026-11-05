package client

import (
	"context"
	"flag"
	"io"
	"net/url"
	"strings"
)

func init() { Register("login", login) }

// login 校验地址与 token 后保存到 <APA_HOME>/credentials.json（附录 B.1：任意 token 即可，失败码 3）。
// token 取 --token，缺省读 APA_TOKEN；`--token -` 从 stdin 读，避免出现在进程列表里。
func login(c *Ctx, args []string) error {
	if c.RunMode() {
		return usage("login is not available under a run identity", "a run uses its run credential; unset APA_RUN to log in as yourself")
	}
	fs := flag.NewFlagSet("login", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	u := fs.String("url", c.Env.Getenv(EnvURL), "")
	tok := fs.String("token", c.Env.Getenv(EnvToken), "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for login", "usage: apa login --url <url> --token <token|->")
	}
	if *tok == "-" {
		b, err := io.ReadAll(io.LimitReader(c.Env.Stdin, 4096))
		if err != nil {
			return usage("cannot read the token from stdin", "pipe the token to `apa login --token -`")
		}
		*tok = strings.TrimSpace(string(b))
	}
	if pu, err := url.Parse(*u); err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
		return usage("--url must be an http(s) URL", "usage: apa login --url http://127.0.0.1:7777 --token <token>")
	}
	if *tok == "" {
		return usage("no token given", "usage: apa login --url <url> --token <token>")
	}
	cr := Credentials{URL: *u, Token: *tok}
	if err := c.doWith(context.Background(), cr, "GET", "/items", nil, nil); err != nil {
		return err
	}
	if err := c.saveCredentials(cr); err != nil {
		return err
	}
	return c.Print(map[string]any{"ok": true, "url": cr.URL})
}
