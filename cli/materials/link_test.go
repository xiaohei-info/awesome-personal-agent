package materials_test

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/cli/client"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/auth"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/authz"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/command"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/domain"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/policy"
	"github.com/xiaohei-info/awesome-personal-agent/kernel/store"
)

func (k *kernel) apaEnv(kind auth.Kind, env map[string]string, args ...string) run {
	k.t.Helper()
	var out, errb bytes.Buffer
	vars := map[string]string{"APA_URL": k.url, "APA_TOKEN": k.tokens[kind], "APA_HOME": k.t.TempDir()}
	for n, v := range env {
		vars[n] = v
	}
	code := client.Main(args, client.Env{Getenv: func(s string) string { return vars[s] }, UserHome: k.t.TempDir(),
		Stdin: strings.NewReader(""), Stdout: &out, Stderr: &errb, HTTP: http.DefaultClient})
	return run{code, out.String(), errb.String()}
}

func (k *kernel) okEnv(kind auth.Kind, env map[string]string, args ...string) run {
	k.t.Helper()
	r := k.apaEnv(kind, env, args...)
	if r.code != 0 {
		k.t.Fatalf("apa %v: exit %d stderr=%s", args, r.code, r.errOut)
	}
	return r
}

// vaults 以本人身份登记一个笔记库（登记只在 Web）。
func (k *kernel) vaults(t *testing.T, name, openURL string) {
	t.Helper()
	owner := domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}
	if err := k.st.Update(context.Background(), func(tx *store.Tx) error {
		_, err := policy.Update(tx, owner, "vaults", func(p *policy.Policy) error {
			p.NoteVaults = []policy.NoteVault{{Name: name, OpenURL: openURL}}
			return nil
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
}

// flow-link-material / ACT-LNK-01、02、05：apa material add --link 登记链接型材料，重复登记返回已有的那条，show 回位置与打开地址。
func TestMaterialAddLink(t *testing.T) {
	k := newKernel(t)
	k.vaults(t, "notes", "obsidian://open?vault={vault}&file={path}")

	r := k.ok(auth.Worker, "material", "add", "--link", "vault:notes/设计/a.md", "--title", "方案", "--project", k.prj, "--tag", "d").json(t)
	id := r["material"].(string)
	if id == "" || r["created"] != true {
		t.Fatalf("add --link: %v", r)
	}
	again := k.ok(auth.Worker, "material", "add", "--link", "vault:notes/设计/a.md", "--title", "另一个", "--project", k.prj, "--kind", "minutes").json(t)
	if again["material"] != id || again["created"] != false {
		t.Fatalf("repeat: %v", again)
	}
	show := k.ok(auth.Observer, "material", "show", id).json(t)
	m := show["material"].(map[string]any)
	if m["form"] != "link" || m["link"] != "vault:notes/设计/a.md" || m["title"] != "方案" || m["kind"] != "reference" ||
		m["open_url"] != "obsidian://open?vault=notes&file=%E8%AE%BE%E8%AE%A1%2Fa.md" || show["revision"] != nil {
		t.Fatalf("show: %v", show)
	}
	if l := k.ok(auth.Observer, "material", "list", "--project", k.prj).json(t)["materials"].([]any); len(l) != 1 || l[0].(map[string]any)["form"] != "link" {
		t.Fatalf("list: %v", l)
	}
	// 项目缺省取 APA_PROJECT。
	d := k.okEnv(auth.Worker, map[string]string{"APA_PROJECT": k.prj}, "material", "add", "--link", "https://Example.com/Doc", "--title", "网页").json(t)
	if d["created"] != true {
		t.Fatalf("default project: %v", d)
	}
	if got := k.ok(auth.Observer, "material", "show", d["material"].(string)).json(t)["material"].(map[string]any); got["link"] != "https://example.com/Doc" {
		t.Fatalf("host not lower-cased: %v", got)
	}
}

// 项目缺省取当前目录到项目的映射（本机检出）。
func TestMaterialAddLinkProjectFromCheckout(t *testing.T) {
	k := newKernel(t)
	k.vaults(t, "notes", "")
	cwd, _ := os.Getwd()
	host, _ := os.Hostname()
	owner := command.Trusted{Actor: domain.Actor{Type: domain.ActorHuman, Principal: "owner", Via: domain.ViaWeb}, Level: authz.LevelWeb, Fresh: true}
	for _, c := range [][2]string{ // 有序：checkout 依赖先登记的 remote
		{"project.repo.add", `{"id":"` + k.prj + `","remote":"git@example.com:me/app.git"}`},
		{"project.checkout.set", `{"id":"` + k.prj + `","remote":"git@example.com:me/app.git","host":"` + host + `","path":"` + cwd + `"}`},
	} {
		if _, err := k.x.Do(context.Background(), command.Request{Identity: owner, Command: c[0], Input: []byte(c[1])}); err != nil {
			t.Fatalf("%s: %v", c[0], err)
		}
	}
	r := k.ok(auth.Worker, "material", "add", "--link", "vault:notes/a.md", "--title", "t").json(t)
	if r["created"] != true {
		t.Fatalf("add: %v", r)
	}
	if l := k.ok(auth.Observer, "material", "list", "--project", k.prj).json(t)["materials"].([]any); len(l) != 1 {
		t.Fatalf("the material belongs to the checked-out project: %v", l)
	}
}

func TestMaterialAddLinkFailures(t *testing.T) {
	k := newKernel(t)
	k.vaults(t, "notes", "")
	for name, args := range map[string][]string{
		"no title":         {"--link", "vault:notes/a.md", "--project", "P"},
		"mixed with repo":  {"--link", "vault:notes/a.md", "--title", "t", "--project", "P", "--repo", "git@h:r.git"},
		"absolute path":    {"--link", "/home/me/a.md", "--title", "t", "--project", "P"},
		"unregistered":     {"--link", "vault:nope/a.md", "--title", "t", "--project", "P"},
		"no project known": {"--link", "vault:notes/a.md", "--title", "t"},
	} {
		args = append([]string{"material", "add"}, args...)
		for i := range args {
			if args[i] == "P" {
				args[i] = k.prj
			}
		}
		if r := k.apa(auth.Worker, args...); r.code != domain.ExitUsage {
			t.Errorf("%s: exit %d, want %d: %s", name, r.code, domain.ExitUsage, r.errOut)
		}
	}
	if r := k.apa(auth.Observer, "material", "add", "--link", "vault:notes/a.md", "--title", "t", "--project", k.prj); r.code != domain.ExitPermissionDenied {
		t.Fatalf("observer: %d %s", r.code, r.errOut)
	}
	_ = context.Background()
}
