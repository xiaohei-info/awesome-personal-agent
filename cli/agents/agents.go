// Package agents 是 apa 的 agents 命令组（附录 B.1，设计 §12.5）：agents check、agents sync。
//
// 本包只做本机的事：读工作区 AGENTS.md、把内容报给内核判定、按内核的结论写文件。渲染与三态判定都在内核，
// 本包不含业务规则，不直接访问存储，不提交也不推送。
package agents

import (
	"context"
	"errors"
	"flag"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

func init() { client.Register("agents", cmd) }

const (
	usageHint  = "usage: apa agents check|sync [--repo <remote url>] [--file <path>] [--host <name>]"
	fileName   = "AGENTS.md"
	timeout    = 30 * time.Second
	stateStale = "stale"
	stateDrift = "drift"
	// decisionOverride 是本人对漂移事项选了"覆盖"（kernel/agentsmd.ChoiceOverride）。
	decisionOverride = "override"
)

func usage(why string) error { return domain.NewError(domain.CodeInvalidRequest, why, usageHint) }

// verdict 是 POST /agents-md/check 的响应（附录 B.2）。
type verdict struct {
	State      string `json:"state"`
	Exit       int    `json:"exit"`
	FileHash   string `json:"file_hash,omitempty"`
	RenderHash string `json:"render_hash"`
	Prompt     bool   `json:"prompt,omitempty"`
	Item       string `json:"item,omitempty"`
	Decision   string `json:"decision,omitempty"`
}

type syncOut struct {
	verdict
	Written bool   `json:"written"`
	File    string `json:"file"`
}

func cmd(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no agents subcommand given")
	}
	var sync bool
	switch args[0] {
	case "check":
	case "sync":
		sync = true
	default:
		return usage("unknown agents subcommand " + args[0])
	}
	fs := flag.NewFlagSet("agents", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", "", "")
	file := fs.String("file", fileName, "")
	host := fs.String("host", "", "")
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage("invalid arguments for agents " + args[0])
	}
	if *repo == "" {
		r, err := originOf(filepath.Dir(*file))
		if err != nil {
			return usage("cannot find the repository: pass --repo <remote url> or run inside a checkout with an origin remote")
		}
		*repo = r
	}
	if *host == "" {
		h, err := os.Hostname()
		if err != nil {
			return usage("cannot determine this machine's host name; pass --host")
		}
		*host = h
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	vd, err := report(ctx, c, *repo, *host, *file)
	if err != nil {
		return err
	}
	if !sync {
		if err := c.Print(vd); err != nil {
			return err
		}
		if vd.Exit != 0 {
			return client.ExitStatus(vd.Exit)
		}
		return nil
	}
	return doSync(ctx, c, *repo, *file, vd)
}

// report 读工作区文件并上报内核；缺失文件按"缺失"上报，其余读失败直接暴露。
func report(ctx context.Context, c *client.Ctx, repo, host, file string) (verdict, error) {
	body := map[string]any{"repo": repo, "host": host}
	switch b, err := os.ReadFile(file); {
	case errors.Is(err, os.ErrNotExist):
		body["missing"] = true
	case err != nil:
		return verdict{}, domain.NewError(domain.CodeInvalidRequest, "the workspace file cannot be read", "check the --file path and its permissions")
	default:
		body["content"] = string(b)
	}
	var vd verdict
	return vd, c.Do(ctx, "POST", "/agents-md/check", body, &vd)
}

// doSync：过期（含文件缺失）直接写；漂移只有本人选了"覆盖"才写，否则不动文件并返回 6。
func doSync(ctx context.Context, c *client.Ctx, repo, file string, vd verdict) error {
	out := syncOut{verdict: vd, File: file}
	if vd.State == stateDrift && vd.Decision != decisionOverride {
		if err := c.Print(out); err != nil {
			return err
		}
		return domain.NewError(domain.CodeConflict, "the workspace AGENTS.md was edited by hand and is not overwritten",
			"wait for the user to decide on the needs_input item "+vd.Item+" (merge back, override or ignore), then run `apa agents sync` again")
	}
	if vd.State == stateStale || vd.State == stateDrift {
		var v struct {
			Content string `json:"content"`
			Hash    string `json:"hash"`
		}
		if err := c.Do(ctx, "GET", "/agents-md?repo="+url.QueryEscape(repo), nil, &v); err != nil {
			return err
		}
		if v.Hash != vd.RenderHash { // 判定与读取之间渲染又变了：不写旧结论
			return domain.NewError(domain.CodeConflict, "the rendering changed while syncing", "run `apa agents sync` again")
		}
		if err := writeAtomic(file, v.Content); err != nil {
			return err
		}
		out.Written = true
	}
	return c.Print(out)
}

// writeAtomic 先写同目录临时文件再改名，写到一半失败不会留下半截 AGENTS.md。
func writeAtomic(file, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(file), ".agents-md-*")
	if err != nil {
		return domain.NewError(domain.CodeInvalidRequest, "the workspace directory is not writable", "check the --file path and its permissions")
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), file)
}

// originOf 取 dir 所在检出的 origin 远程地址。
func originOf(dir string) (string, error) {
	b, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}
