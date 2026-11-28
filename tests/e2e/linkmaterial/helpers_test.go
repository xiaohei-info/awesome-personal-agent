//go:build e2e

// Package linkmaterial 是全流程验收 flow-link-material、flow-report-links 的唯一 owner：真实 apa 二进制 + 真实内核进程，
// 回执由 STUB_HARNESS 替身以运行者身份调真实 CLI（apa report）发出。Web 步骤以本人会话请求 Web 页面调用的同一组 /v1 接口。
package linkmaterial

import (
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/xiaohei-info/awesome-personal-agent/tests/e2e/baseline"
)

// vaultsBody 登记一个库 notes；other 故意不登记。
const vaultsBody = `{"vaults":[{"name":"notes","open_url":"notes://open?vault={vault}&file={path}"}]}`

type fx struct {
	*baseline.Env
	t *testing.T
}

func setup(t *testing.T) *fx {
	t.Helper()
	e := baseline.Setup(t)
	f := &fx{Env: e, t: t}
	f.webOK("PUT", "/settings/note-vaults", vaultsBody, 200)
	return f
}

func (f *fx) webOK(method, path, body string, want int) string {
	f.t.Helper()
	r := f.Web(method, "/v1"+path, body)
	if r.Code != want {
		f.t.Fatalf("Web %s %s: HTTP %d（期望 %d）%s", method, path, r.Code, want, r.Body)
	}
	return r.Body
}

func (f *fx) apa(token string, want int, args ...string) baseline.Result {
	f.t.Helper()
	r := f.Apa(token, "", args...)
	if r.Code != want {
		f.t.Fatalf("apa %s 退出码 %d，期望 %d\nstdout: %s\nstderr: %s", strings.Join(args, " "), r.Code, want, r.Stdout, r.Stderr)
	}
	return r
}

func decode[T any](t *testing.T, s string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatalf("不是预期的 JSON: %v\n%s", err, s)
	}
	return v
}

type added struct {
	Created  bool   `json:"created"`
	Material string `json:"material"`
}

// addLink 以 WRK 登记链接型材料（apa material add --link），要求退出码 want。
func (f *fx) addLink(want int, project, link, title string, extra ...string) added {
	f.t.Helper()
	args := append([]string{"material", "add", "--link", link, "--title", title, "--project", project}, extra...)
	r := f.apa(f.WRK, want, args...)
	if want != 0 {
		return added{}
	}
	return decode[added](f.t, r.Stdout)
}

type material struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Title string `json:"title"`
	Form  string `json:"form"`
	Link  string `json:"link"`
}

// materials 是项目下全部材料（Web 材料页的数据源）。
func (f *fx) materials(project string) []material {
	f.t.Helper()
	return decode[struct {
		Materials []material `json:"materials"`
	}](f.t, f.webOK("GET", "/materials?project="+project, "", 200)).Materials
}

func (f *fx) byLink(project, link string) (material, bool) {
	for _, m := range f.materials(project) {
		if m.Link == link {
			return m, true
		}
	}
	return material{}, false
}

// outputs 是事项上 output 关系指向的材料 id。
func (f *fx) outputs(item string) []string {
	f.t.Helper()
	var out []string
	for _, r := range decode[struct {
		Relations []struct {
			From, To, Type string
		} `json:"relations"`
	}](f.t, f.apa(f.OBS, 0, "item", "show", item, "--full").Stdout).Relations {
		if r.Type == "output" && r.From == item {
			out = append(out, r.To)
		}
	}
	return out
}

// delegated 建父事项并委派出一个子事项（归 work-host.cli，只有 WRK 能回执），返回父、子事项 id。
func (f *fx) delegated(title, project string) (parent, child string) {
	f.t.Helper()
	parent = decode[struct {
		ID string `json:"id"`
	}](f.t, f.apa(f.DEL, 0, "item", "add", "--kind", "task", "--title", title, "--phase", "open", "--project", f.Projects[project]).Stdout).ID
	child = decode[struct {
		ID string `json:"id"`
	}](f.t, f.apa(f.DEL, 0, "delegate", "--parent", parent, "--assignee", "work-host.cli", "--title", title+" 子事项", "--reason", "委派").Stdout).ID
	return
}

// harnessReport 让 STUB_HARNESS 替身以 WRK 的运行身份发一次回执：它执行 `apa report <child> --outcome … --link …`（真实 CLI）。
func (f *fx) harnessReport(child, outcome string, links ...string) {
	f.t.Helper()
	cmd := exec.Command(f.Bin["stub-harness"])
	cmd.Dir = f.t.TempDir()
	cmd.Env = append(os.Environ(), "APA_HOME="+f.Home, "APA_URL="+f.URL, "APA_TOKEN="+f.WRK,
		"STUB_HARNESS_MODE=receipt", "STUB_HARNESS_RUN_DIR="+cmd.Dir,
		"STUB_HARNESS_RECEIPT_CMD="+f.Bin["apa"]+" report "+child+" --outcome "+outcome+" --summary acc-receipt --reason acc-reason",
		"STUB_HARNESS_RECEIPT_LINKS="+strings.Join(links, ","))
	if out, err := cmd.CombinedOutput(); err != nil {
		f.t.Fatalf("替身 harness 回执 %s %v 失败: %v\n%s", outcome, links, err, out)
	}
}

func (f *fx) phase(item string) string {
	f.t.Helper()
	return decode[struct {
		Item struct {
			Phase string `json:"phase"`
		} `json:"item"`
	}](f.t, f.apa(f.OBS, 0, "item", "show", item, "--full").Stdout).Item.Phase
}
