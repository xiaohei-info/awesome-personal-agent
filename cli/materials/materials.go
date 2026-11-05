// Package materials 实现 `apa material add/commit/list/show/search` 与 `apa blob put/get`（附录 B.1「材料与规范」、设计 §12.3–12.4）。
// 只是内核 /v1/materials、/v1/blobs 的薄客户端：托管型 commit 返回 6、observer 提交返回 4、回退与确认只在 Web，都由内核判定。
// show 不带托管型正文（正文只有 Web 能读），只回位置。
package materials

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"io"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
)

const materialUsage = "usage: apa material add --repo R --path P [--project ID] [--kind K] [--title T] [--tag T]... [--commit SHA] [--reason S] | " +
	"commit <id> --from FILE|- --reason S [--based-on REV] | commit --from FILE|- --reason S --kind K --title T [--project ID] [--tag T]... | " +
	"list [--project ID] | show <id> [--rev REV] | search <query> [--project ID] [--kind K] [--tag T]... [--limit N]"

const blobUsage = "usage: apa blob put <file> | get <sha256> [--out FILE]"

func init() {
	client.Register("material", material)
	client.Register("blob", blob)
}

func usage(why, next string) error { return domain.NewError(domain.CodeInvalidRequest, why, next) }

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(v string) error { *m = append(*m, v); return nil }

func newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}

// splitID 摘出可选的首个位置参数（flag 包在首个非 flag 参数处停止）。
func splitID(args []string) (string, []string) {
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		return args[0], args[1:]
	}
	return "", args
}

func call(c *client.Ctx, method, path string, body any) error {
	var out any
	if err := c.Do(context.Background(), method, path, body, &out); err != nil {
		return err
	}
	return c.Print(out)
}

func material(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no material subcommand given", materialUsage)
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "add":
		return add(c, rest)
	case "commit":
		return commit(c, rest)
	case "list":
		fs := newFlags("list")
		project := fs.String("project", "", "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 {
			return usage("invalid arguments for material list", materialUsage)
		}
		return call(c, "GET", "/materials"+query(url.Values{"project": {*project}}), nil)
	case "show":
		id, rest := splitID(rest)
		fs := newFlags("show")
		rev := fs.String("rev", "", "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || id == "" {
			return usage("show takes a material id and optional --rev", materialUsage)
		}
		return call(c, "GET", "/materials/"+url.PathEscape(id)+query(url.Values{"rev": {*rev}}), nil)
	case "search":
		return search(c, rest)
	}
	return usage("unknown material subcommand "+sub, materialUsage)
}

// query 只带非空参数。
func query(v url.Values) string {
	for k, vs := range v {
		if len(vs) == 1 && vs[0] == "" {
			delete(v, k)
		}
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

func add(c *client.Ctx, args []string) error {
	fs := newFlags("add")
	var tags multi
	repo, path := fs.String("repo", "", ""), fs.String("path", "", "")
	project, kind, title := fs.String("project", "", ""), fs.String("kind", "", ""), fs.String("title", "", "")
	commitSHA, reason := fs.String("commit", "", ""), fs.String("reason", "", "")
	fs.Var(&tags, "tag", "")
	if err := fs.Parse(args); err != nil || fs.NArg() != 0 || *repo == "" || *path == "" {
		return usage("material add needs --repo and --path", materialUsage)
	}
	body := map[string]any{"repo": *repo, "path": *path}
	for k, v := range map[string]string{"project": *project, "kind": *kind, "title": *title, "commit": *commitSHA, "reason": *reason} {
		if v != "" {
			body[k] = v
		}
	}
	if len(tags) > 0 {
		body["tags"] = []string(tags)
	}
	return call(c, "POST", "/materials", body)
}

// commit 提交自有型修订：有 <id> 时是对已有材料的新修订，没有时（带 --kind/--title）新建材料的首条修订。
// 对托管型 id 提交由内核返回 6。
func commit(c *client.Ctx, args []string) error {
	id, rest := splitID(args)
	fs := newFlags("commit")
	var tags multi
	from, reason, basedOn := fs.String("from", "", ""), fs.String("reason", "", ""), fs.String("based-on", "", "")
	project, kind, title := fs.String("project", "", ""), fs.String("kind", "", ""), fs.String("title", "", "")
	fs.Var(&tags, "tag", "")
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || *from == "" || *reason == "" {
		return usage("material commit needs --from and --reason", materialUsage)
	}
	if id == "" && (*kind == "" || *title == "") {
		return usage("committing a new material needs --kind and --title", materialUsage)
	}
	content, err := readFrom(c, *from)
	if err != nil {
		return err
	}
	body := map[string]any{"content": string(content), "reason": *reason}
	if *basedOn != "" {
		body["based_on"] = *basedOn
	}
	if id != "" {
		return call(c, "POST", "/materials/"+url.PathEscape(id)+"/revisions", body)
	}
	body["kind"], body["title"] = *kind, *title
	if *project != "" {
		body["project"] = *project
	}
	if len(tags) > 0 {
		body["tags"] = []string(tags)
	}
	return call(c, "POST", "/materials", body)
}

func readFrom(c *client.Ctx, from string) ([]byte, error) {
	var b []byte
	var err error
	if from == "-" {
		b, err = io.ReadAll(c.Env.Stdin)
	} else {
		b, err = os.ReadFile(from)
	}
	if err != nil {
		return nil, usage("cannot read "+from, "give a readable file, or - for standard input")
	}
	return b, nil
}

func search(c *client.Ctx, args []string) error {
	q, rest := splitID(args)
	fs := newFlags("search")
	var tags multi
	project, kind, limit := fs.String("project", "", ""), fs.String("kind", "", ""), fs.Int("limit", 0, "")
	fs.Var(&tags, "tag", "")
	if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || q == "" || *limit < 0 {
		return usage("search takes a query and optional filters", materialUsage)
	}
	v := url.Values{"q": {q}, "project": {*project}, "kind": {*kind}}
	if *limit > 0 {
		v.Set("limit", strconv.Itoa(*limit))
	}
	for _, t := range tags {
		v.Add("tag", t)
	}
	return call(c, "GET", "/materials/search"+query(v), nil)
}

func blob(c *client.Ctx, args []string) error {
	if len(args) == 0 {
		return usage("no blob subcommand given", blobUsage)
	}
	switch args[0] {
	case "put":
		if len(args) != 2 {
			return usage("blob put takes exactly one file", blobUsage)
		}
		data, err := readFrom(c, args[1])
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		return call(c, "PUT", "/blobs/"+hex.EncodeToString(sum[:]), client.Raw(data))
	case "get":
		hash, rest := splitID(args[1:])
		fs := newFlags("get")
		out := fs.String("out", "", "")
		if err := fs.Parse(rest); err != nil || fs.NArg() != 0 || hash == "" {
			return usage("blob get takes a sha256 and optional --out", blobUsage)
		}
		var data []byte
		if err := c.Do(context.Background(), "GET", "/blobs/"+url.PathEscape(hash), nil, &data); err != nil {
			return err
		}
		if *out != "" {
			return os.WriteFile(*out, data, 0o600)
		}
		_, err := c.Env.Stdout.Write(data)
		return err
	}
	return usage("unknown blob subcommand "+args[0], blobUsage)
}
